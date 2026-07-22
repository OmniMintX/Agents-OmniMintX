package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/OmniMintX/overmind/internal/aoclient"
	"github.com/OmniMintX/overmind/internal/store"
)

// errSkipDispatch is an internal signal from ensureParentsMerged: the
// parent merge is transiently blocked, skip this child this tick.
var errSkipDispatch = errors.New("skip dispatch this tick")

// tick is one scheduler pass: reconcile crashed dispatches, poll active
// sessions, then dispatch ready tasks. Transport errors (AO unreachable)
// bubble up untouched so the loop can back off.
func (r *runner) tick(ctx context.Context) error {
	tasks, err := r.St.GetTasks(r.plan.ID)
	if err != nil {
		return err
	}
	st, err := r.St.PlanState(r.plan.ID)
	if err != nil {
		return err
	}
	for _, t := range tasks {
		if st.TaskStatus[t.ID] == store.TaskDispatching {
			if err := r.reconcileDispatching(ctx, t, st.VerifyRounds[t.ID]); err != nil {
				return err
			}
		}
	}
	for _, t := range tasks {
		switch st.TaskStatus[t.ID] {
		case store.TaskDispatched, store.TaskRunning, store.TaskNeedsHuman:
			if err := r.pollTask(ctx, t, st.TaskStatus[t.ID]); err != nil {
				return err
			}
		}
	}
	return r.dispatchReady(ctx, tasks)
}

// reconcileDispatching resolves a task stuck in the dispatch INTENT state
// (crash between task_dispatching and task_dispatched): if a session with
// our displayName marker already exists, adopt it; otherwise the HTTP call
// never landed, so dispatch again. This is what makes dispatch idempotent.
// round is the task's CURRENT verify round from derived state: a retry
// dispatch must only ever adopt the current round's session, never a
// previous round's (their displayNames differ by construction).
func (r *runner) reconcileDispatching(ctx context.Context, t store.Task, round int) error {
	sessions, err := r.AO.ListSessions(ctx, aoclient.ListSessionsFilter{Project: r.plan.ProjectID})
	if err != nil {
		return err
	}
	want := displayNameForRound(r.plan.ID, t.ID, round)
	var match *aoclient.Session
	for i := range sessions {
		s := &sessions[i]
		if s.DisplayName != want {
			continue
		}
		if match == nil ||
			(match.IsTerminated && !s.IsTerminated) ||
			(match.IsTerminated == s.IsTerminated && s.CreatedAt.After(match.CreatedAt)) {
			match = s
		}
	}
	if match == nil {
		r.logf("task %s: dispatch intent had no session, re-dispatching", t.ID)
		return r.createAndDispatch(ctx, t, round)
	}
	r.logf("task %s: adopted existing session %s", t.ID, match.ID)
	return r.St.DispatchTask(r.plan.ID, t.ID, r.runID, match.ID, match.Branch)
}

// dispatchReady starts ready tasks (pending + all deps done) while the
// active count stays under MaxParallel, re-checking that every parent's
// branch is in the default branch first (parents are merged locally when
// they finish; this ancestry re-check covers crashes between merge and
// finish, and DBs written by older binaries).
// Approval gate (OM-12): a requires_approval task that was never approved
// is parked in awaiting_approval INSTEAD of dispatching — it burns no
// MaxParallel slot and has no timeout clock (clocks only exist for
// dispatched sessions). The scheduler is the ONLY gatekeeper: the store's
// RequestTaskApproval does not check the flag. Approval is permanent
// (st.Approved), so OM-10 retry rounds are never re-gated.
func (r *runner) dispatchReady(ctx context.Context, tasks []store.Task) error {
	st, err := r.St.PlanState(r.plan.ID)
	if err != nil {
		return err
	}
	if err := r.failBlockedDependents(tasks, st); err != nil {
		return err
	}
	active := 0
	for _, status := range st.TaskStatus {
		switch status {
		case store.TaskDispatching, store.TaskDispatched, store.TaskRunning, store.TaskNeedsHuman:
			active++
		}
	}
	ready, err := r.St.GetReadyTasks(r.plan.ID) // ordered by id: deterministic
	if err != nil {
		return err
	}
	byID := make(map[string]store.Task, len(tasks))
	for _, t := range tasks {
		byID[t.ID] = t
	}
	for _, t := range ready {
		if t.RequiresApproval && !st.Approved[t.ID] {
			if err := r.St.RequestTaskApproval(r.plan.ID, t.ID, r.runID); err != nil {
				return err
			}
			r.notify("Overmind: approval needed",
				fmt.Sprintf("task %s (%s) awaits approval — om approve-task %s %s", t.ID, t.Title, r.plan.ID, t.ID))
			r.logf("task %s: AWAITING APPROVAL — om approve-task %s %s", t.ID, r.plan.ID, t.ID)
			continue
		}
		if active >= r.Cfg.MaxParallel {
			// No free slot: keep scanning so gated tasks later in the
			// ready list still get their approval request this tick.
			continue
		}
		failReason, err := r.ensureParentsMerged(ctx, t, byID)
		if errors.Is(err, errSkipDispatch) {
			continue // parent merge transiently blocked: retry next tick
		}
		if err != nil {
			return err
		}
		if err := r.St.MarkTaskDispatching(r.plan.ID, t.ID, r.runID); err != nil {
			return err
		}
		if failReason != "" {
			// Merge conflicts do not self-heal: fail THIS child deterministically.
			if err := r.failTaskKind(t, "dependency_failed", failReason, "", nil); err != nil {
				return err
			}
			continue
		}
		if err := r.createAndDispatch(ctx, t, st.VerifyRounds[t.ID]); err != nil {
			return err
		}
		active++
	}
	return nil
}

// failBlockedDependents fails every pending task that (transitively)
// depends on a failed task with kind=dependency_failed: such a task can
// never become ready, and failing it per-task (instead of leaving it
// pending until the plan-level failure) surfaces WHY it will never run —
// e.g. dependents of a rejected requires_approval task. The transition is
// a DIRECT pending -> failed (store.FailPendingTask): the task never
// dispatches, so no synthetic task_dispatching intent pollutes the audit
// log. Independent branches are untouched. st.TaskStatus is updated in
// place so the caller sees the cascade within the same tick.
func (r *runner) failBlockedDependents(tasks []store.Task, st *store.DerivedState) error {
	for changed := true; changed; {
		changed = false
		for _, t := range tasks {
			if st.TaskStatus[t.ID] != store.TaskPending {
				continue
			}
			failedDep := ""
			for _, dep := range t.DependsOn {
				if st.TaskStatus[dep] == store.TaskFailed {
					failedDep = dep
					break
				}
			}
			if failedDep == "" {
				continue
			}
			reason := fmt.Sprintf("dependency %s failed", failedDep)
			payload := jsonPayload(map[string]any{"reason": reason, "kind": "dependency_failed"})
			if err := r.St.FailPendingTask(r.plan.ID, t.ID, r.runID, payload); err != nil {
				return err
			}
			r.logf("task %s: FAILED (dependency_failed): %s", t.ID, reason)
			st.TaskStatus[t.ID] = store.TaskFailed
			changed = true
		}
	}
	return nil
}

// createAndDispatch performs the CreateSession call for a task already in
// the dispatching state, then records task_dispatched. The scheduler
// appends the marker-protocol footer here (never trusting the planner LLM
// to relay it); round > 0 re-dispatches also inject the latest verify
// feedback (from the task_retry event, so it survives crashes). The whole
// prompt is kept within AO's 4096-byte cap. Non-transport create errors
// (validation, 4xx) fail the task, not the plan.
func (r *runner) createAndDispatch(ctx context.Context, t store.Task, round int) error {
	feedback := ""
	if round > 0 {
		var err error
		if feedback, err = r.retryFeedback(t.ID); err != nil {
			return err
		}
	}
	sess, err := r.AO.CreateSession(ctx, aoclient.SpawnSessionRequest{
		ProjectID:   r.plan.ProjectID,
		Kind:        "worker",
		Harness:     aoclient.Harness(t.Harness),
		Prompt:      promptWithFeedbackAndFooter(t.Prompt, feedback, markerPathFor(r.plan.ID, t.ID)),
		DisplayName: displayNameForRound(r.plan.ID, t.ID, round),
	})
	if err != nil {
		if isTransport(err) {
			return err
		}
		return r.failTaskKind(t, "create_failed", fmt.Sprintf("create session: %v", err), "", nil)
	}
	if err := r.St.DispatchTask(r.plan.ID, t.ID, r.runID, sess.ID, sess.Branch); err != nil {
		return err
	}
	r.logf("task %s: dispatched as session %s (branch %s, round %d)", t.ID, sess.ID, sess.Branch, round)
	return nil
}

// retryFeedback returns the feedback text of the task's LATEST task_retry
// event (recorded when a verify round failed). Reading it from the event
// log — not memory — keeps retry prompts correct across crash/resume.
func (r *runner) retryFeedback(taskID string) (string, error) {
	events, err := r.St.ListEvents(r.plan.ID)
	if err != nil {
		return "", err
	}
	feedback := ""
	for _, e := range events {
		if e.Type == store.EventTaskRetry && e.TaskID != nil && *e.TaskID == taskID {
			var p struct {
				Feedback string `json:"feedback"`
			}
			if json.Unmarshal([]byte(e.PayloadJSON), &p) == nil {
				feedback = p.Feedback
			}
		}
	}
	return feedback, nil
}

// ensureParentsMerged verifies every dependency's branch is an ancestor of
// the default branch before a child is dispatched (git ancestry is the
// source of truth; parents normally merge when they finish). Returns a
// non-empty failReason when the child must fail; errors are
// transport/store-fatal only.
func (r *runner) ensureParentsMerged(ctx context.Context, t store.Task, byID map[string]store.Task) (string, error) {
	for _, dep := range t.DependsOn {
		if r.merged[dep] {
			continue
		}
		parent, ok := byID[dep]
		if !ok || parent.AOSessionID == nil {
			r.merged[dep] = true // never dispatched -> nothing to merge
			continue
		}
		if parent.Branch == nil || *parent.Branch == "" {
			return fmt.Sprintf("parent %s: no branch recorded, cannot verify its code is merged", dep), nil
		}
		merged, err := r.Git.IsMerged(ctx, r.repo, *parent.Branch, r.defaultBranch)
		if err != nil {
			return fmt.Sprintf("parent %s: ancestry check %s: %v", dep, *parent.Branch, err), nil
		}
		if !merged {
			// Crash between merge and FinishTask: redo the (idempotent) merge.
			res, err := r.Git.Merge(ctx, r.repo, *parent.Branch, r.defaultBranch,
				fmt.Sprintf("om: merge task %s (%s) into %s", dep, *parent.Branch, r.defaultBranch))
			if err != nil {
				return fmt.Sprintf("parent %s: merge %s: %v", dep, *parent.Branch, err), nil
			}
			if res.Conflict != "" {
				return fmt.Sprintf("parent %s: merge %s into %s conflicts", dep, *parent.Branch, r.defaultBranch), nil
			}
			if res.Blocked != "" {
				// Transient: skip dispatching this child this tick; retried later.
				r.logf("task %s: parent %s merge blocked (%s) — retrying next tick", t.ID, dep, res.Blocked)
				return "", errSkipDispatch
			}
			r.logf("task %s: merged parent %s branch %s before dispatch", t.ID, dep, *parent.Branch)
		}
		r.merged[dep] = true
	}
	return "", nil
}
