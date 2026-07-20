package scheduler

import (
	"context"

	"github.com/OmniMintX/overmind/internal/aoclient"
	"github.com/OmniMintX/overmind/internal/store"
)

// finishTask records task_done (with the session's first PR URL, if any)
// and best-effort-kills the finished session to free its AO slot.
func (r *runner) finishTask(ctx context.Context, t store.Task, sess aoclient.Session) error {
	prURL := ""
	if len(sess.PRs) > 0 {
		prURL = sess.PRs[0].URL
	}
	if err := r.St.FinishTask(r.plan.ID, t.ID, r.runID, prURL); err != nil {
		return err
	}
	delete(r.clocks, t.ID)
	r.merged[t.ID] = true // its PRs were merged on the way here
	if !sess.IsTerminated {
		if _, err := r.AO.KillSession(ctx, sess.ID); err != nil && !isTransport(err) {
			r.logf("task %s: kill session %s after done: %v (ignored)", t.ID, sess.ID, err)
		}
	}
	r.logf("task %s: DONE (pr: %s)", t.ID, prURL)
	return nil
}

// failTask records task_failed with a structured reason payload.
func (r *runner) failTask(t store.Task, reason, sessionID string) error {
	payload := map[string]any{"reason": reason}
	if sessionID != "" {
		payload["session_id"] = sessionID
	}
	if err := r.St.FailTask(r.plan.ID, t.ID, r.runID, jsonPayload(payload)); err != nil {
		return err
	}
	delete(r.clocks, t.ID)
	r.logf("task %s: FAILED: %s", t.ID, reason)
	return nil
}

// killAndFail kills the AO session (best effort) then fails the task.
// Used for hung sessions: timeout, idle-without-marker, no_signal.
func (r *runner) killAndFail(ctx context.Context, t store.Task, sess aoclient.Session, reason string) error {
	if !sess.IsTerminated {
		if _, err := r.AO.KillSession(ctx, sess.ID); err != nil {
			if isTransport(err) {
				return err
			}
			r.logf("task %s: kill session %s: %v (ignored)", t.ID, sess.ID, err)
		}
	}
	return r.failTask(t, reason, sess.ID)
}
