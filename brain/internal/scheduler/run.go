package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/OmniMintX/overmind/internal/store"
)

// Run executes one plan until it is done or failed. It is safe to re-run
// after a crash: state is derived from events, dispatch is idempotent, and
// the advisory run lock rejects a second concurrent `om run`.
func (s *Scheduler) Run(ctx context.Context, planID string) error {
	sc := *s
	sc.Cfg = s.Cfg.withDefaults()
	if sc.Now == nil {
		sc.Now = time.Now
	}
	if sc.Sleep == nil {
		sc.Sleep = defaultSleep
	}
	plan, err := sc.St.GetPlan(planID)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("plan %s not found (see: om status)", planID)
	}
	if err != nil {
		return fmt.Errorf("load plan %s: %w", planID, err)
	}
	st, err := sc.St.PlanState(planID)
	if err != nil {
		return err
	}
	if st.PlanStatus != store.PlanApproved && st.PlanStatus != store.PlanRunning {
		return fmt.Errorf("plan %s is %s; om run needs an approved (or interrupted running) plan", planID, st.PlanStatus)
	}
	if err := sc.St.AcquireRunLock(planID, sc.PID, sc.Cfg.LockStaleAfter); err != nil {
		return err
	}
	defer sc.St.ReleaseRunLock(planID, sc.PID)
	repo, defBranch, err := sc.resolveRepo(ctx, plan.ProjectID)
	if err != nil {
		return err
	}
	runID := newRunID()
	if err := sc.St.StartRun(planID, runID); err != nil {
		return err
	}
	sc.logf("plan %s: run %s started (pid %d, repo %s, default branch %s)",
		planID, runID, sc.PID, repo, defBranch)
	r := &runner{
		Scheduler:     &sc,
		plan:          plan,
		runID:         runID,
		repo:          repo,
		defaultBranch: defBranch,
		clocks:        make(map[string]*taskClock),
		merged:        make(map[string]bool),
	}
	return r.loop(ctx)
}

// resolveRepo maps the plan's AO project id to its local repo path and
// default branch, and fail-fasts on repos with origin/<default>: AO bases
// new worktrees on origin/<default> first, so local merges could never
// reach children (Phase 1 supports remoteless repos only).
func (s *Scheduler) resolveRepo(ctx context.Context, projectID string) (repo, defBranch string, err error) {
	projects, err := s.AO.ListProjects(ctx)
	if err != nil {
		return "", "", fmt.Errorf("list AO projects: %w", err)
	}
	for _, p := range projects {
		if p.ID == projectID {
			repo = p.Path
			break
		}
	}
	if repo == "" {
		return "", "", fmt.Errorf("AO project %s not found (was it removed from AO?)", projectID)
	}
	defBranch, err = s.Git.DefaultBranch(ctx, repo)
	if err != nil {
		return "", "", err
	}
	hasOrigin, err := s.Git.HasRemoteBranch(ctx, repo, defBranch)
	if err != nil {
		return "", "", err
	}
	if hasOrigin {
		return "", "", fmt.Errorf(
			"repo %s has origin/%s: Phase 1 chaining only supports repos WITHOUT a remote default branch (AO bases new worktrees on origin/%s, which local merges cannot advance)",
			repo, defBranch, defBranch)
	}
	return repo, defBranch, nil
}

// loop is the poll cycle: heartbeat -> tick -> completion check -> sleep.
// AO-unreachable ticks back off exponentially (one ao_unreachable event per
// outage) and NEVER fail the plan; any other error aborts the run.
func (r *runner) loop(ctx context.Context) error {
	var backoff time.Duration
	outage := false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := r.St.HeartbeatRunLock(r.plan.ID, r.PID); err != nil {
			return err // lock stolen (we must have been presumed dead): stop
		}
		if err := r.tick(ctx); err != nil {
			if !isTransport(err) {
				return err
			}
			if !outage {
				outage = true
				payload := jsonPayload(map[string]any{"error": err.Error()})
				if rerr := r.St.RecordAOUnreachable(r.plan.ID, r.runID, payload); rerr != nil {
					return rerr
				}
			}
			if backoff <= 0 {
				backoff = 2 * time.Second
			} else if backoff *= 2; backoff > r.Cfg.MaxBackoff {
				backoff = r.Cfg.MaxBackoff
			}
			r.logf("plan %s: AO unreachable, retrying in %s", r.plan.ID, backoff)
			if err := r.Sleep(ctx, backoff); err != nil {
				return err
			}
			continue
		}
		if outage {
			outage, backoff = false, 0
			r.logf("plan %s: AO reachable again", r.plan.ID)
		}
		done, err := r.checkCompletion()
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		if err := r.Sleep(ctx, r.Cfg.PollInterval); err != nil {
			return err
		}
	}
}

// checkCompletion derives the plan outcome: all tasks done -> plan_done;
// nothing active AND nothing dispatchable -> plan_failed (failed tasks, or
// pending tasks forever blocked by a failed dependency).
func (r *runner) checkCompletion() (bool, error) {
	st, err := r.St.PlanState(r.plan.ID)
	if err != nil {
		return false, err
	}
	allDone, anyActive := true, false
	var failed []string
	for id, status := range st.TaskStatus {
		switch status {
		case store.TaskDone:
		case store.TaskFailed:
			allDone = false
			failed = append(failed, id)
		case store.TaskDispatching, store.TaskDispatched, store.TaskRunning, store.TaskNeedsHuman:
			allDone, anyActive = false, true
		default: // pending
			allDone = false
		}
	}
	if allDone {
		if err := r.St.FinishPlan(r.plan.ID, r.runID); err != nil {
			return false, err
		}
		r.logf("plan %s: done", r.plan.ID)
		return true, nil
	}
	if anyActive {
		return false, nil
	}
	ready, err := r.St.GetReadyTasks(r.plan.ID)
	if err != nil || len(ready) > 0 {
		return false, err
	}
	sort.Strings(failed)
	payload := jsonPayload(map[string]any{"reason": "no runnable tasks remain", "failed_tasks": failed})
	if err := r.St.FailPlan(r.plan.ID, r.runID, payload); err != nil {
		return false, err
	}
	r.logf("plan %s: failed (%s)", r.plan.ID, payload)
	return true, nil
}

func jsonPayload(m map[string]any) string {
	b, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(b)
}
