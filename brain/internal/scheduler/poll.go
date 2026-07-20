package scheduler

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/OmniMintX/overmind/internal/aoclient"
	"github.com/OmniMintX/overmind/internal/store"
)

// pollTask reads the task's AO session and advances Overmind state.
// derived is the task's current derived status (dispatched|running|needs_human).
func (r *runner) pollTask(ctx context.Context, t store.Task, derived string) error {
	if t.AOSessionID == nil {
		return r.failTask(t, "internal: active task has no AO session id", "")
	}
	sess, err := r.AO.GetSession(ctx, *t.AOSessionID)
	if err != nil {
		if isTransport(err) {
			return err
		}
		// API-level answer (e.g. 404 SESSION_NOT_FOUND): the session is gone
		// without an outcome.
		return r.failTask(t, fmt.Sprintf("session %s lost: %v", *t.AOSessionID, err), "")
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
			return r.killAndFail(ctx, t, sess,
				fmt.Sprintf("timeout: no AO status change for %s (last: %s)", r.Cfg.TaskTimeout, sess.Status))
		}
		return nil
	default: // ClassTerminal: merged | idle | terminated | no_signal
		return r.resolveTerminal(ctx, t, sess, ck)
	}
}

// resolveTerminal decides the outcome of a terminal-class AO status.
func (r *runner) resolveTerminal(ctx context.Context, t store.Task, sess aoclient.Session, ck *taskClock) error {
	switch sess.Status {
	case aoclient.StatusMerged:
		return r.finishTask(ctx, t, sess)
	case aoclient.StatusIdle, aoclient.StatusTerminated:
		done, err := r.hasDoneMarker(ctx, sess.ID)
		if err != nil {
			if isTransport(err) {
				return err
			}
			return r.failTask(t, fmt.Sprintf("check %s marker: %v", DoneMarker, err), sess.ID)
		}
		if done {
			// Idle + marker = the agent finished: merge its PRs and complete.
			if reason, err := r.mergeSessionPRs(ctx, sess); err != nil {
				return err
			} else if reason != "" {
				return r.killAndFail(ctx, t, sess, reason)
			}
			return r.finishTask(ctx, t, sess)
		}
		if sess.Status == aoclient.StatusTerminated {
			return r.failTask(t, "session terminated without "+DoneMarker+" marker", sess.ID)
		}
		// Idle WITHOUT marker: the agent may just be thinking between turns.
		// Give it TaskTimeout since the last observed change, then kill.
		if r.Now().Sub(ck.lastChangeAt) > r.Cfg.TaskTimeout {
			return r.killAndFail(ctx, t, sess,
				fmt.Sprintf("idle without %s marker for over %s", DoneMarker, r.Cfg.TaskTimeout))
		}
		return nil
	default: // StatusNoSignal
		if r.Now().Sub(ck.noSignalAt) > r.Cfg.NoSignalTimeout {
			return r.killAndFail(ctx, t, sess,
				fmt.Sprintf("no_signal for over %s (agent hung)", r.Cfg.NoSignalTimeout))
		}
		return nil
	}
}

// hasDoneMarker reports whether the session workspace contains .om-done.
// AO 0.10.x daemons have no workspace/files listing route (404
// ROUTE_NOT_FOUND); fall back to probing the marker directly through the
// per-file preview route, which those versions do serve.
func (r *runner) hasDoneMarker(ctx context.Context, sessionID string) (bool, error) {
	files, err := r.AO.ListWorkspaceFiles(ctx, sessionID)
	if err == nil {
		for _, f := range files.Files {
			if f.Path == DoneMarker && f.Status != "deleted" {
				return true, nil
			}
		}
		return false, nil
	}
	var apiErr *aoclient.APIError
	if errors.As(err, &apiErr) && apiErr.Code == "ROUTE_NOT_FOUND" {
		_, found, perr := r.AO.PreviewFile(ctx, sessionID, DoneMarker)
		return found, perr
	}
	return false, err
}

// mergeSessionPRs merges every unmerged PR of a finished session. A merge
// refusal returns a failReason (conflicts don't self-heal); transport
// errors are returned as err for backoff.
func (r *runner) mergeSessionPRs(ctx context.Context, sess aoclient.Session) (string, error) {
	for _, pr := range sess.PRs {
		if pr.State == "merged" {
			continue
		}
		if _, err := r.AO.MergePR(ctx, strconv.Itoa(pr.Number)); err != nil {
			if isTransport(err) {
				return "", err
			}
			return fmt.Sprintf("merge PR #%d: %v", pr.Number, err), nil
		}
		r.logf("session %s: merged PR #%d", sess.ID, pr.Number)
	}
	return "", nil
}
