package store

import (
	"path/filepath"
	"testing"
)

// cacheStatuses reads the display-cache columns (plan + task status).
func cacheStatuses(t *testing.T, s *Store, planID string) (string, map[string]string) {
	t.Helper()
	p, err := s.GetPlan(planID)
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := s.GetTasks(planID)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]string{}
	for _, task := range tasks {
		m[task.ID] = task.Status
	}
	return p.Status, m
}

func assertDeriveMatchesCache(t *testing.T, s *Store, planID string) *DerivedState {
	t.Helper()
	st, err := s.PlanState(planID)
	if err != nil {
		t.Fatal(err)
	}
	planCache, taskCache := cacheStatuses(t, s, planID)
	if st.PlanStatus != planCache {
		t.Fatalf("derived plan status %q != cache %q", st.PlanStatus, planCache)
	}
	for id, cached := range taskCache {
		if st.TaskStatus[id] != cached {
			t.Fatalf("task %s: derived %q != cache %q", id, st.TaskStatus[id], cached)
		}
	}
	return st
}

func runHalfPlan(t *testing.T, s *Store) {
	t.Helper()
	mustCreatePlan(t, s, "p1", nt("a"), nt("b", "a"), nt("c", "b"))
	steps := []error{
		s.ApprovePlan("p1"),
		s.StartRun("p1", "run-1"),
		s.DispatchTask("p1", "a", "run-1", "sess-a", "ao/sess-a/root"),
		s.StartTask("p1", "a", "run-1"),
		s.FinishTask("p1", "a", "run-1", "https://pr/1", ""),
		s.DispatchTask("p1", "b", "run-1", "sess-b", "ao/sess-b/root"),
		s.StartTask("p1", "b", "run-1"),
	}
	for i, err := range steps {
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
}

func TestDeriveMatchesCacheAndSurvivesCacheWipe(t *testing.T) {
	s := openTestStore(t)
	runHalfPlan(t, s)
	before := assertDeriveMatchesCache(t, s, "p1")

	// Wipe the caches; derive must still return the same state.
	if _, err := s.db.Exec(`UPDATE plans SET status='draft' WHERE id='p1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE tasks SET status='pending' WHERE plan_id='p1'`); err != nil {
		t.Fatal(err)
	}
	st, err := s.PlanState("p1")
	if err != nil {
		t.Fatal(err)
	}
	if st.PlanStatus != before.PlanStatus {
		t.Fatalf("plan status after cache wipe: %q, want %q", st.PlanStatus, before.PlanStatus)
	}
	for id, want := range before.TaskStatus {
		if st.TaskStatus[id] != want {
			t.Fatalf("task %s after cache wipe: %q, want %q", id, st.TaskStatus[id], want)
		}
	}
	if st.PlanStatus != PlanRunning || st.TaskStatus["a"] != TaskDone || st.TaskStatus["b"] != TaskRunning || st.TaskStatus["c"] != TaskPending {
		t.Fatalf("unexpected derived state: %+v", st)
	}
}

func TestResumeAfterReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resume.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	runHalfPlan(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	st := assertDeriveMatchesCache(t, s2, "p1")
	if st.PlanStatus != PlanRunning || st.LastRunID != "run-1" {
		t.Fatalf("unexpected state after reopen: %+v", st)
	}
	if st.TaskStatus["a"] != TaskDone || st.TaskStatus["b"] != TaskRunning {
		t.Fatalf("unexpected task state after reopen: %+v", st.TaskStatus)
	}
	// A new run gets a new run_id.
	if err := s2.StartRun("p1", "run-2"); err != nil {
		t.Fatal(err)
	}
	st2, err := s2.PlanState("p1")
	if err != nil {
		t.Fatal(err)
	}
	if st2.LastRunID != "run-2" {
		t.Fatalf("LastRunID = %q, want run-2", st2.LastRunID)
	}
}
