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
	runID := newRunID()
	if err := sc.St.StartRun(planID, runID); err != nil {
		return err
	}
	sc.logf("plan %s: run %s started (pid %d)", planID, runID, sc.PID)
	r := &runner{
		Scheduler: &sc,
		plan:      plan,
		runID:     runID,
		clocks:    make(map[string]*taskClock),
		merged:    make(map[string]bool),
	}
	return r.loop(ctx)
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
