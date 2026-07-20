package scheduler

import (
	"context"
	"fmt"
	"time"

	"github.com/OmniMintX/overmind/internal/aoclient"
	"github.com/OmniMintX/overmind/internal/store"
)

// pollTask reads the task's AO session and advances Overmind state.
// derived is the task's current derived status (dispatched|running|needs_human).
func (r *runner) pollTask(ctx context.Context, t store.Task, derived string) error {
	if t.AOSessionID == nil {
		return r.failTaskKind(t, "internal", "internal: active task has no AO session id", "", nil)
	}
	sess, err := r.AO.GetSession(ctx, *t.AOSessionID)
	if err != nil {
		if isTransport(err) {
			return err
		}
		// API-level answer (e.g. 404 SESSION_NOT_FOUND): the session is gone
		// without an outcome.
		return r.failTaskKind(t, "session_lost", fmt.Sprintf("session %s lost: %v", *t.AOSessionID, err), "", nil)
	}
	now := r.Now()
	ck := r.clocks[t.ID]
	if ck == nil {
		// First observation this run (fresh dispatch or resume): restart
		// the in-memory timeout clocks (Phase 1: not derived from events).
		ck = &taskClock{lastStatus: sess.Status, lastChangeAt: now}
		r.clocks[t.ID] = ck
	} else if sess.Status != ck.lastStatus {
		ck.lastStatus, ck.lastChangeAt = sess.Status, now
	}
	if sess.Status == aoclient.StatusNoSignal {
		if ck.noSignalAt.IsZero() {
			ck.noSignalAt = now
		}
	} else {
		ck.noSignalAt = time.Time{} // streak broken
	}
	switch Classify(sess.Status) {
	case ClassNeedsHuman:
		if derived != store.TaskNeedsHuman {
			payload := jsonPayload(map[string]any{"ao_status": string(sess.Status), "session_id": sess.ID})
			if err := r.St.MarkTaskNeedsHuman(r.plan.ID, t.ID, r.runID, payload); err != nil {
				return err
			}
			r.logf("task %s: NEEDS HUMAN (%s) — waiting, timeout clock stopped", t.ID, sess.Status)
		}
		return nil
	case ClassRunning:
		if derived == store.TaskNeedsHuman {
			if err := r.St.ResumeTask(r.plan.ID, t.ID, r.runID); err != nil {
				return err
			}
			r.logf("task %s: resumed (%s)", t.ID, sess.Status)
		} else if derived == store.TaskDispatched && sess.Status != aoclient.StatusDraft {
			if err := r.St.StartTask(r.plan.ID, t.ID, r.runID); err != nil {
				return err
			}
		}
		// needs_human stops the clock; only RUNNING tasks can time out.
		if now.Sub(ck.lastChangeAt) > r.Cfg.TaskTimeout {
			return r.killAndFailKind(ctx, t, sess, "timeout",
				fmt.Sprintf("timeout: no AO status change for %s (last: %s)", r.Cfg.TaskTimeout, sess.Status), nil)
		}
		return nil
	default: // ClassTerminal: merged | idle | terminated | no_signal
		return r.resolveTerminal(ctx, t, sess, ck)
	}
}

// resolveTerminal decides the outcome of a terminal-class AO status using
// the 5-way marker verdict: ok -> local-merge branch + done; fail ->
// failed (no merge); empty/absent -> wait (bounded); malformed -> one poll
// of grace, then failed.
func (r *runner) resolveTerminal(ctx context.Context, t store.Task, sess aoclient.Session, ck *taskClock) error {
	if sess.Status == aoclient.StatusNoSignal {
		if r.Now().Sub(ck.noSignalAt) > r.Cfg.NoSignalTimeout {
			return r.killAndFailKind(ctx, t, sess, "no_signal",
				fmt.Sprintf("no_signal for over %s (agent hung)", r.Cfg.NoSignalTimeout), nil)
		}
		return nil
	}
	if sess.Status == aoclient.StatusMerged {
		// AO itself reports the session merged (PR path, future AO versions):
		// the work already reached the base branch, no local merge needed.
		return r.finishTask(ctx, t, sess, "")
	}
	// idle | terminated: read the per-task marker.
	markerPath := markerPathFor(r.plan.ID, t.ID)
	content, found, err := r.AO.PreviewFile(ctx, sess.ID, markerPath)
	if err != nil {
		if isTransport(err) {
			return err
		}
		return r.failTaskKind(t, "marker_check_failed", fmt.Sprintf("check %s: %v", markerPath, err), sess.ID, nil)
	}
	verdict, detail := markerAbsent, ""
	if found {
		verdict, detail = parseMarker(content)
	}
	switch verdict {
	case markerOK:
		ck.markerBad = false
		proceed, err := r.mergeTaskBranch(ctx, t, sess)
		if err != nil || !proceed {
			// err: transport/store; !proceed: blocked (retry next tick) or
			// the merge already failed the task (conflict / no branch).
			return err
		}
		return r.finishTask(ctx, t, sess, detail)
	case markerFail:
		return r.killAndFailKind(ctx, t, sess, "marker_fail",
			"agent reported failure: "+detail, map[string]any{"marker_content": truncate(content, maxMarkerPayload)})
	case markerMalformed:
		if ck.markerBad {
			return r.killAndFailKind(ctx, t, sess, "marker_malformed",
				"marker first line is neither ok nor fail", map[string]any{"marker_content": truncate(content, maxMarkerPayload)})
		}
		ck.markerBad = true // grace: agent may still be writing; recheck next poll
		return nil
	default: // markerAbsent | markerEmpty: not finished (yet)
		ck.markerBad = false
		if sess.Status == aoclient.StatusTerminated {
			return r.failTaskKind(t, "marker_missing",
				"session terminated without "+markerPath+" marker", sess.ID,
				map[string]any{"marker_path": markerPath})
		}
		// Idle (or AO-merged) without marker: the agent may just be thinking
		// between turns. Bounded by IdleNoMarkerTimeout (10m default).
		if r.Now().Sub(ck.lastChangeAt) > r.Cfg.IdleNoMarkerTimeout {
			return r.killAndFailKind(ctx, t, sess, "marker_missing",
				fmt.Sprintf("idle without %s marker for over %s", markerPath, r.Cfg.IdleNoMarkerTimeout),
				map[string]any{"marker_path": markerPath})
		}
		return nil
	}
}

// mergeTaskBranch merges the finished session's branch into the repo's
// default branch (AO workers never open PRs; Overmind merges locally).
// proceed=true means the branch is in the default branch and the caller
// may FinishTask. proceed=false with nil error means either the merge is
// transiently blocked (retry next tick; the task clock is refrozen so
// idle-timeout never kills a merge-blocked task) or the task was already
// failed here (conflict / missing branch).
func (r *runner) mergeTaskBranch(ctx context.Context, t store.Task, sess aoclient.Session) (proceed bool, err error) {
	branch := sess.Branch
	if branch == "" && t.Branch != nil {
		branch = *t.Branch
	}
	if branch == "" {
		return false, r.failTaskKind(t, "merge_failed", "session has no branch to merge", sess.ID, nil)
	}
	ck := r.clocks[t.ID]
	msg := fmt.Sprintf("om: merge task %s (%s) into %s", t.ID, branch, r.defaultBranch)
	res, err := r.Git.Merge(ctx, r.repo, branch, r.defaultBranch, msg)
	if err != nil {
		return false, r.failTaskKind(t, "merge_failed", fmt.Sprintf("merge %s: %v", branch, err), sess.ID, nil)
	}
	switch {
	case res.Conflict != "":
		return false, r.killAndFailKind(ctx, t, sess, "merge_conflict",
			fmt.Sprintf("merge %s into %s conflicts", branch, r.defaultBranch),
			map[string]any{"branch": branch, "detail": truncate(res.Conflict, maxMarkerPayload)})
	case res.Blocked != "":
		if ck != nil {
			ck.lastChangeAt = r.Now() // freeze idle clock while blocked
			if !ck.blockedNoted {
				ck.blockedNoted = true
				payload := jsonPayload(map[string]any{"branch": branch, "reason": res.Blocked})
				if err := r.St.RecordMergeBlocked(r.plan.ID, t.ID, r.runID, payload); err != nil {
					return false, err
				}
			}
		}
		r.logf("task %s: merge blocked (%s) — retrying next tick", t.ID, res.Blocked)
		return false, nil
	default:
		if ck != nil {
			ck.blockedNoted = false
		}
		if res.Merged {
			payload := jsonPayload(map[string]any{"branch": branch, "sha": res.SHA})
			if err := r.St.RecordTaskBranchMerged(r.plan.ID, t.ID, r.runID, payload); err != nil {
				return false, err
			}
			r.logf("task %s: merged %s into %s (%s)", t.ID, branch, r.defaultBranch, res.SHA[:12])
		}
		r.merged[t.ID] = true
		return true, nil
	}
}
