package scheduler

import (
	"context"
	"strings"
	"testing"

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
	ao.addSession(displayNameFor("a1234567"), false, doneScript(0))

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
	ao.scripts[displayNameFor("a1234567")] = doneScript(0)

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

// TestRunLockContention: while another pid holds a FRESH run lock, om run
// must refuse to start; after the holder releases, it must succeed.
func TestRunLockContention(t *testing.T) {
	st, ao, s := newHarness(t, []store.NewTask{nt("a1234567")}, Config{})
	ao.scripts[displayNameFor("a1234567")] = doneScript(0)
	if err := st.AcquireRunLock("plan-1", 999, s.Cfg.withDefaults().LockStaleAfter); err != nil {
		t.Fatalf("seed lock: %v", err)
	}
	err := s.Run(context.Background(), "plan-1")
	if err == nil || !strings.Contains(err.Error(), "locked by another om run") {
		t.Fatalf("want lock-contention error, got %v", err)
	}
	if got := planStatus(t, st); got != store.PlanApproved {
		t.Fatalf("plan status after refused run = %s, want approved", got)
	}
	if err := st.ReleaseRunLock("plan-1", 999); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := s.Run(context.Background(), "plan-1"); err != nil {
		t.Fatalf("Run after release: %v", err)
	}
	if got := planStatus(t, st); got != store.PlanDone {
		t.Fatalf("plan status = %s, want done", got)
	}
}
