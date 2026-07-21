package scheduler

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/OmniMintX/overmind/internal/aoclient"
	"github.com/OmniMintX/overmind/internal/store"
)

// TestResumeCrashAfterCreateSession: process died between task_dispatching
// and task_dispatched, but the CreateSession HTTP call HAD landed. On
// resume the scheduler must adopt the existing session by its displayName
// marker instead of creating a duplicate.
func TestResumeCrashAfterCreateSession(t *testing.T) {
	st, ao, s := newHarness(t, []store.NewTask{nt("a1234567")}, Config{})
	// Simulate the crashed first run: intent recorded, session created.
	if err := st.StartRun("plan-1", "run-crashed"); err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if err := st.MarkTaskDispatching("plan-1", "a1234567", "run-crashed"); err != nil {
		t.Fatalf("MarkTaskDispatching: %v", err)
	}
	ao.addSession(displayNameFor("plan-1", "a1234567"), false, doneScript(0))

	if err := s.Run(context.Background(), "plan-1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := planStatus(t, st); got != store.PlanDone {
		t.Fatalf("plan status = %s, want done", got)
	}
	for _, e := range ao.events() {
		if strings.HasPrefix(e, "create:") {
			t.Fatalf("resume created a duplicate session: %v", ao.events())
		}
	}
	tasks, err := st.GetTasks("plan-1")
	if err != nil {
		t.Fatalf("GetTasks: %v", err)
	}
	if tasks[0].AOSessionID == nil || *tasks[0].AOSessionID != "sess-1" {
		t.Fatalf("task did not adopt sess-1: %+v", tasks[0])
	}
}

// TestResumeCrashBeforeCreateSession: intent recorded but the process died
// BEFORE the HTTP call landed — no session exists. Resume must dispatch
// (exactly one create) and finish the plan.
func TestResumeCrashBeforeCreateSession(t *testing.T) {
	st, ao, s := newHarness(t, []store.NewTask{nt("a1234567")}, Config{})
	if err := st.StartRun("plan-1", "run-crashed"); err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if err := st.MarkTaskDispatching("plan-1", "a1234567", "run-crashed"); err != nil {
		t.Fatalf("MarkTaskDispatching: %v", err)
	}
	ao.scripts[displayNameFor("plan-1", "a1234567")] = doneScript(0)

	if err := s.Run(context.Background(), "plan-1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := planStatus(t, st); got != store.PlanDone {
		t.Fatalf("plan status = %s, want done", got)
	}
	creates := 0
	for _, e := range ao.events() {
		if strings.HasPrefix(e, "create:") {
			creates++
		}
	}
	if creates != 1 {
		t.Fatalf("creates = %d, want exactly 1 (%v)", creates, ao.events())
	}
}

// TestRunLockContention: while a LIVE pid holds a FRESH run lock, om run
// must refuse to start; after the holder releases, it must succeed. The
// holder is this test process itself so the liveness check sees it alive
// (dead holders are taken over — covered by store.TestRunLockDeadHolder).
func TestRunLockContention(t *testing.T) {
	st, ao, s := newHarness(t, []store.NewTask{nt("a1234567")}, Config{})
	ao.scripts[displayNameFor("plan-1", "a1234567")] = doneScript(0)
	holder := int64(os.Getpid())
	if _, err := st.AcquireRunLock("plan-1", holder, s.Cfg.withDefaults().LockStaleAfter); err != nil {
		t.Fatalf("seed lock: %v", err)
	}
	err := s.Run(context.Background(), "plan-1")
	if err == nil || !strings.Contains(err.Error(), "locked by another om run") {
		t.Fatalf("want lock-contention error, got %v", err)
	}
	if got := planStatus(t, st); got != store.PlanApproved {
		t.Fatalf("plan status after refused run = %s, want approved", got)
	}
	if err := st.ReleaseRunLock("plan-1", holder); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := s.Run(context.Background(), "plan-1"); err != nil {
		t.Fatalf("Run after release: %v", err)
	}
	if got := planStatus(t, st); got != store.PlanDone {
		t.Fatalf("plan status = %s, want done", got)
	}
}

// TestBackoffSleepChunksAndHeartbeats: a long AO outage must not leave the
// run-lock heartbeat stale (MaxBackoff == LockStaleAfter == 60s): backoff
// sleeps are chunked to LockStaleAfter/3 so heartbeats keep flowing, and
// the plan still finishes once AO is back.
func TestBackoffSleepChunksAndHeartbeats(t *testing.T) {
	st, ao, s := newHarness(t, []store.NewTask{nt("a1234567")}, Config{})
	ao.scripts[displayNameFor("plan-1", "a1234567")] = doneScript(0)
	created := false
	ao.onCreate = func(f *fakeAO, _ aoclient.SpawnSessionRequest) {
		if !created {
			created = true
			f.failGets = 8 // backoff walks 2,4,8,16,32,60,60,60 — well past LockStaleAfter
		}
	}
	step := Config{}.withDefaults().LockStaleAfter / 3
	var sleeps []time.Duration
	base := s.Sleep
	s.Sleep = func(ctx context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		return base(ctx, d)
	}
	if err := s.Run(context.Background(), "plan-1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := planStatus(t, st); got != store.PlanDone {
		t.Fatalf("plan status = %s, want done", got)
	}
	chunked := false
	for _, d := range sleeps {
		if d > step {
			t.Fatalf("slept %s in one go — heartbeat gap exceeds LockStaleAfter/3 (%s)", d, step)
		}
		if d == step {
			chunked = true
		}
	}
	if !chunked {
		t.Fatalf("no backoff sleep was chunked to %s; sleeps = %v", step, sleeps)
	}
}

// TestBackoffLockStolenStops: losing the run lock mid-backoff must be
// detected by the in-backoff heartbeat — the runner stops before sleeping
// any further chunk (a stolen lock means another om run is live; sleeping
// on and ticking again would double-drive the plan).
func TestBackoffLockStolenStops(t *testing.T) {
	st, ao, s := newHarness(t, []store.NewTask{nt("a1234567")}, Config{})
	ao.scripts[displayNameFor("plan-1", "a1234567")] = doneScript(0)
	created := false
	ao.onCreate = func(f *fakeAO, _ aoclient.SpawnSessionRequest) {
		if !created {
			created = true
			f.failGets = 1000 // outage never ends
		}
	}
	step := Config{}.withDefaults().LockStaleAfter / 3
	stolen := false
	sleepsAfterSteal := 0
	base := s.Sleep
	s.Sleep = func(ctx context.Context, d time.Duration) error {
		if stolen {
			sleepsAfterSteal++
		} else if d == step {
			// First chunk of a long backoff: lose the lock during the sleep.
			stolen = true
			if err := st.ReleaseRunLock("plan-1", s.PID); err != nil {
				t.Fatalf("release: %v", err)
			}
			if _, err := st.AcquireRunLock("plan-1", 99999, time.Minute); err != nil {
				t.Fatalf("steal: %v", err)
			}
		}
		return base(ctx, d)
	}
	err := s.Run(context.Background(), "plan-1")
	if err == nil || !strings.Contains(err.Error(), "run lock") {
		t.Fatalf("want run-lock error after steal, got %v", err)
	}
	if sleepsAfterSteal != 0 {
		t.Fatalf("runner slept %d more times after losing the lock — in-backoff heartbeat missing", sleepsAfterSteal)
	}
}
