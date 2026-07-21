package scheduler

import (
	"context"

	"github.com/OmniMintX/overmind/internal/aoclient"
	"github.com/OmniMintX/overmind/internal/store"
)

// finishTask records task_done (with the session's first PR URL, if any,
// and the marker summary in the payload) and best-effort-kills the
// finished session to free its AO slot.
func (r *runner) finishTask(ctx context.Context, t store.Task, sess aoclient.Session, summary string) error {
	prURL := ""
	if len(sess.PRs) > 0 {
		prURL = sess.PRs[0].URL
	}
	payload := map[string]any{"marker": "ok"}
	if summary != "" {
		payload["summary"] = summary
	}
	if err := r.St.FinishTask(r.plan.ID, t.ID, r.runID, prURL, jsonPayload(payload)); err != nil {
		return err
	}
	delete(r.clocks, t.ID)
	r.merged[t.ID] = true // its branch was merged on the way here
	if !sess.IsTerminated {
		if _, err := r.AO.KillSession(ctx, sess.ID); err != nil && !isTransport(err) {
			r.logf("task %s: kill session %s after done: %v (ignored)", t.ID, sess.ID, err)
		}
	}
	r.logf("task %s: DONE (%s)", t.ID, summary)
	return nil
}

// failTaskKind records task_failed with the standardized payload:
// {reason, kind, session_id?, ...extra}. kind is one of marker_fail |
// marker_malformed | marker_missing | marker_check_failed | timeout |
// no_signal | session_lost | verify_budget_exhausted | verify_error |
// system_commit_failed | merge_conflict | merge_failed | create_failed |
// internal | dependency_failed.
func (r *runner) failTaskKind(t store.Task, kind, reason, sessionID string, extra map[string]any) error {
	payload := map[string]any{"reason": reason, "kind": kind}
	if sessionID != "" {
		payload["session_id"] = sessionID
	}
	for k, v := range extra {
		payload[k] = v
	}
	if err := r.St.FailTask(r.plan.ID, t.ID, r.runID, jsonPayload(payload)); err != nil {
		return err
	}
	delete(r.clocks, t.ID)
	r.logf("task %s: FAILED (%s): %s", t.ID, kind, reason)
	return nil
}

// killAndFailKind kills the AO session (best effort) then fails the task.
// Used for hung sessions and deterministic marker/merge failures.
func (r *runner) killAndFailKind(ctx context.Context, t store.Task, sess aoclient.Session, kind, reason string, extra map[string]any) error {
	if !sess.IsTerminated {
		if _, err := r.AO.KillSession(ctx, sess.ID); err != nil {
			if isTransport(err) {
				return err
			}
			r.logf("task %s: kill session %s: %v (ignored)", t.ID, sess.ID, err)
		}
	}
	return r.failTaskKind(t, kind, reason, sess.ID, extra)
}
