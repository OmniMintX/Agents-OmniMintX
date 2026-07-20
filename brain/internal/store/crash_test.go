package store

import (
	"path/filepath"
	"testing"
)

// TestCrashMidTransaction simulates a crash between the event INSERT and
// the cache UPDATE: because both happen in ONE transaction, neither may
// survive, and derive(events) must stay consistent with the cache.
func TestCrashMidTransaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "crash.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	runHalfPlan(t, s)
	before := assertDeriveMatchesCache(t, s, "p1")

	// Begin the "finish task b" transaction but crash before commit.
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := appendEvent(tx, "p1", "b", "run-1", EventTaskDone, "{}"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE tasks SET status='done' WHERE id='b' AND plan_id='p1'`); err != nil {
		t.Fatal(err)
	}
	// Crash before COMMIT: SQLite discards the whole transaction on
	// recovery — simulated here with Rollback, then reopening the file.
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	after := assertDeriveMatchesCache(t, s2, "p1")
	if after.TaskStatus["b"] != TaskRunning {
		t.Fatalf("task b = %q after crash, want running (tx must roll back)", after.TaskStatus["b"])
	}
	if after.PlanStatus != before.PlanStatus {
		t.Fatalf("plan status changed across crash: %q -> %q", before.PlanStatus, after.PlanStatus)
	}
	events, err := s2.ListEvents("p1")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Type == EventTaskDone && e.TaskID != nil && *e.TaskID == "b" {
			t.Fatal("orphan task_done event for b survived the crash")
		}
	}
	// The interrupted transition can be replayed cleanly after resume.
	if err := s2.FinishTask("p1", "b", "run-1", "", ""); err != nil {
		t.Fatalf("replay finish after crash: %v", err)
	}
	final := assertDeriveMatchesCache(t, s2, "p1")
	if final.TaskStatus["b"] != TaskDone {
		t.Fatalf("task b = %q, want done", final.TaskStatus["b"])
	}
	ready, err := s2.GetReadyTasks("p1")
	if err != nil {
		t.Fatal(err)
	}
	if len(ready) != 1 || ready[0].ID != "c" {
		t.Fatalf("want ready=c after resume, got %v", taskIDs(ready))
	}
}

// TestFailedPlanIsTerminal documents the Phase 1 rule: a failed plan
// cannot be re-run; retry means creating a new plan.
func TestFailedPlanIsTerminal(t *testing.T) {
	s := openTestStore(t)
	mustCreatePlan(t, s, "p1", nt("a"))
	if err := s.ApprovePlan("p1"); err != nil {
		t.Fatal(err)
	}
	if err := s.StartRun("p1", "run-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.FailPlan("p1", "run-1", `{"reason":"task failed"}`); err != nil {
		t.Fatal(err)
	}
	if err := s.StartRun("p1", "run-2"); err == nil {
		t.Fatal("failed plan is terminal: new run must be rejected")
	}
	st, err := s.PlanState("p1")
	if err != nil {
		t.Fatal(err)
	}
	if st.PlanStatus != PlanFailed {
		t.Fatalf("derived plan status = %q, want failed", st.PlanStatus)
	}
}
