package scheduler

import (
	"context"
	"fmt"
	"strconv"

	"github.com/OmniMintX/overmind/internal/aoclient"
	"github.com/OmniMintX/overmind/internal/store"
)

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
			if err := r.reconcileDispatching(ctx, t); err != nil {
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
func (r *runner) reconcileDispatching(ctx context.Context, t store.Task) error {
	sessions, err := r.AO.ListSessions(ctx, aoclient.ListSessionsFilter{Project: r.plan.ProjectID})
	if err != nil {
		return err
	}
	want := displayNameFor(t.ID)
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
		return r.createAndDispatch(ctx, t)
	}
	r.logf("task %s: adopted existing session %s", t.ID, match.ID)
	return r.St.DispatchTask(r.plan.ID, t.ID, r.runID, match.ID, match.Branch)
}

// dispatchReady starts ready tasks (pending + all deps done) while the
// active count stays under MaxParallel, merging parent PRs first
// (merge-before-dispatch: children must see parent code in the base branch).
func (r *runner) dispatchReady(ctx context.Context, tasks []store.Task) error {
	st, err := r.St.PlanState(r.plan.ID)
	if err != nil {
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
		if active >= r.Cfg.MaxParallel {
			return nil
		}
		failReason, err := r.ensureParentsMerged(ctx, t, byID)
		if err != nil {
			return err
		}
		if err := r.St.MarkTaskDispatching(r.plan.ID, t.ID, r.runID); err != nil {
			return err
		}
		if failReason != "" {
			// Merge conflicts do not self-heal: fail THIS child deterministically.
			if err := r.failTask(t, failReason, ""); err != nil {
				return err
			}
			continue
		}
		if err := r.createAndDispatch(ctx, t); err != nil {
			return err
		}
		active++
	}
	return nil
}

// createAndDispatch performs the CreateSession call for a task already in
// the dispatching state, then records task_dispatched. Non-transport
// create errors (validation, 4xx) fail the task, not the plan.
func (r *runner) createAndDispatch(ctx context.Context, t store.Task) error {
	sess, err := r.AO.CreateSession(ctx, aoclient.SpawnSessionRequest{
		ProjectID:   r.plan.ProjectID,
		Kind:        "worker",
		Harness:     aoclient.Harness(t.Harness),
		Prompt:      t.Prompt,
		DisplayName: displayNameFor(t.ID),
	})
	if err != nil {
		if isTransport(err) {
			return err
		}
		return r.failTask(t, fmt.Sprintf("create session: %v", err), "")
	}
	if err := r.St.DispatchTask(r.plan.ID, t.ID, r.runID, sess.ID, sess.Branch); err != nil {
		return err
	}
	r.logf("task %s: dispatched as session %s (branch %s)", t.ID, sess.ID, sess.Branch)
	return nil
}

// ensureParentsMerged verifies every dependency's PRs are merged before a
// child is dispatched. Returns a non-empty failReason when the child must
// fail (unmergeable parent PR); errors are transport/store-fatal only.
func (r *runner) ensureParentsMerged(ctx context.Context, t store.Task, byID map[string]store.Task) (string, error) {
	for _, dep := range t.DependsOn {
		if r.merged[dep] {
			continue
		}
		parent, ok := byID[dep]
		if !ok || parent.AOSessionID == nil {
			r.merged[dep] = true // no session -> nothing to merge
			continue
		}
		sess, err := r.AO.GetSession(ctx, *parent.AOSessionID)
		if err != nil {
			if isTransport(err) {
				return "", err
			}
			return fmt.Sprintf("parent %s: get session: %v", dep, err), nil
		}
		for _, pr := range sess.PRs {
			if pr.State == "merged" {
				continue
			}
			if _, err := r.AO.MergePR(ctx, strconv.Itoa(pr.Number)); err != nil {
				if isTransport(err) {
					return "", err
				}
				return fmt.Sprintf("parent %s: merge PR #%d: %v", dep, pr.Number, err), nil
			}
			r.logf("task %s: merged parent %s PR #%d before dispatch", t.ID, dep, pr.Number)
		}
		r.merged[dep] = true
	}
	return "", nil
}
