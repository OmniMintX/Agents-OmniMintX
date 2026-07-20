package scheduler

import (
	"context"
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
	want := displayNameFor(r.plan.ID, t.ID)
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
// active count stays under MaxParallel, re-checking that every parent's
// branch is in the default branch first (parents are merged locally when
// they finish; this ancestry re-check covers crashes between merge and
// finish, and DBs written by older binaries).
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
		if err := r.createAndDispatch(ctx, t); err != nil {
			return err
		}
		active++
	}
	return nil
}

// createAndDispatch performs the CreateSession call for a task already in
// the dispatching state, then records task_dispatched. The scheduler
// appends the marker-protocol footer here (never trusting the planner LLM
// to relay it). Non-transport create errors (validation, 4xx) fail the
// task, not the plan.
func (r *runner) createAndDispatch(ctx context.Context, t store.Task) error {
	sess, err := r.AO.CreateSession(ctx, aoclient.SpawnSessionRequest{
		ProjectID:   r.plan.ProjectID,
		Kind:        "worker",
		Harness:     aoclient.Harness(t.Harness),
		Prompt:      promptWithFooter(t.Prompt, markerPathFor(r.plan.ID, t.ID)),
		DisplayName: displayNameFor(r.plan.ID, t.ID),
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
	r.logf("task %s: dispatched as session %s (branch %s)", t.ID, sess.ID, sess.Branch)
	return nil
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
