package store

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mustCreatePlan(t *testing.T, s *Store, planID string, tasks ...NewTask) {
	t.Helper()
	if err := s.CreatePlan(planID, "goal of "+planID, "proj-1", tasks); err != nil {
		t.Fatalf("create plan %s: %v", planID, err)
	}
}

func taskIDs(tasks []Task) []string {
	ids := make([]string, len(tasks))
	for i, t := range tasks {
		ids[i] = t.ID
	}
	return ids
}

func TestCreatePlanAndGetters(t *testing.T) {
	s := openTestStore(t)
	mustCreatePlan(t, s, "p1", nt("a"), nt("b", "a"), nt("c", "a"), nt("d", "b", "c"))

	p, err := s.GetPlan("p1")
	if err != nil {
		t.Fatalf("get plan: %v", err)
	}
	if p.Status != PlanDraft || p.ProjectID != "proj-1" || p.ApprovedAt != nil {
		t.Fatalf("unexpected plan: %+v", p)
	}
	tasks, err := s.GetTasks("p1")
	if err != nil {
		t.Fatalf("get tasks: %v", err)
	}
	if len(tasks) != 4 {
		t.Fatalf("want 4 tasks, got %d", len(tasks))
	}
	for _, task := range tasks {
		if task.Status != TaskPending || task.Harness != "claude-code" {
			t.Fatalf("unexpected task: %+v", task)
		}
	}
	events, err := s.ListEvents("p1")
	if err != nil || len(events) != 1 || events[0].Type != EventPlanCreated {
		t.Fatalf("want single plan_created event, got %v (err %v)", events, err)
	}
}

// TestCreatePlanReusesTaskIDsAcrossPlans: the planner emits t1..tN in every
// plan, so a second plan with the same task ids must not collide (found live
// in the OM-6 E2E run: "UNIQUE constraint failed: tasks.id").
func TestCreatePlanReusesTaskIDsAcrossPlans(t *testing.T) {
	s := openTestStore(t)
	mustCreatePlan(t, s, "p1", nt("t1"), nt("t2", "t1"))
	mustCreatePlan(t, s, "p2", nt("t1"), nt("t2", "t1"))

	for _, planID := range []string{"p1", "p2"} {
		tasks, err := s.GetTasks(planID)
		if err != nil || len(tasks) != 2 {
			t.Fatalf("plan %s: want 2 tasks, got %d (err %v)", planID, len(tasks), err)
		}
	}
}

// TestMigrateTasksPK: a database created when tasks.id alone was the primary
// key must be rebuilt on Open so plan-scoped ids work, keeping existing rows.
func TestMigrateTasksPK(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	// Recreate the v1 layout (global PK on id) with one plan already stored.
	stmts := []string{
		`DROP TABLE task_dependencies`,
		`DROP TABLE tasks`,
		`CREATE TABLE tasks (
		    id            TEXT PRIMARY KEY,
		    plan_id       TEXT NOT NULL REFERENCES plans(id),
		    title         TEXT NOT NULL,
		    prompt        TEXT NOT NULL,
		    harness       TEXT NOT NULL,
		    status        TEXT NOT NULL DEFAULT 'pending'
		        CHECK (status IN ('pending','ready','dispatching','dispatched','running','needs_human','done','failed')),
		    ao_session_id TEXT,
		    branch        TEXT,
		    pr_url        TEXT,
		    created_at    TEXT NOT NULL,
		    UNIQUE (id, plan_id)
		)`,
		`CREATE TABLE task_dependencies (
		    plan_id            TEXT NOT NULL,
		    task_id            TEXT NOT NULL,
		    depends_on_task_id TEXT NOT NULL,
		    PRIMARY KEY (task_id, depends_on_task_id),
		    CHECK (task_id <> depends_on_task_id),
		    FOREIGN KEY (task_id, plan_id) REFERENCES tasks(id, plan_id),
		    FOREIGN KEY (depends_on_task_id, plan_id) REFERENCES tasks(id, plan_id)
		)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatalf("prepare old schema: %v", err)
		}
	}
	mustCreatePlan(t, s, "p1", nt("t1"), nt("t2", "t1"))
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen (migration): %v", err)
	}
	t.Cleanup(func() { s2.Close() })
	tasks, err := s2.GetTasks("p1")
	if err != nil || len(tasks) != 2 {
		t.Fatalf("after migration: want 2 tasks, got %d (err %v)", len(tasks), err)
	}
	mustCreatePlan(t, s2, "p2", nt("t1"), nt("t2", "t1"))
}

func TestGetReadyTasksProgression(t *testing.T) {
	s := openTestStore(t)
	mustCreatePlan(t, s, "p1", nt("a"), nt("b", "a"), nt("c", "a"), nt("d", "b", "c"))
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.ApprovePlan("p1"))
	must(s.StartRun("p1", "run-1"))

	ready, _ := s.GetReadyTasks("p1")
	if got := strings.Join(taskIDs(ready), ","); got != "a" {
		t.Fatalf("want ready=a, got %q", got)
	}
	must(s.DispatchTask("p1", "a", "run-1", "sess-a", "ao/sess-a/root"))
	if ready, _ = s.GetReadyTasks("p1"); len(ready) != 0 {
		t.Fatalf("dispatched task must not be ready, got %v", taskIDs(ready))
	}
	must(s.StartTask("p1", "a", "run-1"))
	must(s.FinishTask("p1", "a", "run-1", "https://github.com/x/pr/1", `{"marker":"ok","summary":"did a"}`))

	ready, _ = s.GetReadyTasks("p1")
	if got := strings.Join(taskIDs(ready), ","); got != "b,c" {
		t.Fatalf("want ready=b,c, got %q", got)
	}
	must(s.DispatchTask("p1", "b", "run-1", "sess-b", "ao/sess-b/root"))
	must(s.FinishTask("p1", "b", "run-1", "", ""))
	if ready, _ = s.GetReadyTasks("p1"); len(ready) != 1 || ready[0].ID != "c" {
		t.Fatalf("want ready=c, got %v", taskIDs(ready))
	}
	must(s.DispatchTask("p1", "c", "run-1", "sess-c", "ao/sess-c/root"))
	must(s.FailTask("p1", "c", "run-1", `{"reason":"boom"}`))
	// d depends on failed c -> never ready.
	if ready, _ = s.GetReadyTasks("p1"); len(ready) != 0 {
		t.Fatalf("want no ready tasks, got %v", taskIDs(ready))
	}
}

func TestInvalidTransitionsRejected(t *testing.T) {
	s := openTestStore(t)
	mustCreatePlan(t, s, "p1", nt("a"))
	if err := s.StartRun("p1", "run-1"); err == nil {
		t.Fatal("run on draft plan should fail")
	}
	if err := s.FinishTask("p1", "a", "run-1", "", ""); err == nil {
		t.Fatal("finishing a pending task should fail")
	}
	if err := s.ApprovePlan("p1"); err != nil {
		t.Fatal(err)
	}
	if err := s.ApprovePlan("p1"); err == nil {
		t.Fatal("double approve should fail")
	}
}

// TestFinishTaskPayload: the task_done payload (marker summary) must
// persist into brain_events, and the merge audit events must be listable
// without breaking derived state.
func TestFinishTaskPayload(t *testing.T) {
	s := openTestStore(t)
	mustCreatePlan(t, s, "p1", nt("a"))
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.ApprovePlan("p1"))
	must(s.StartRun("p1", "run-1"))
	must(s.DispatchTask("p1", "a", "run-1", "sess-a", "ao/sess-a/root"))
	must(s.RecordMergeBlocked("p1", "a", "run-1", `{"branch":"ao/sess-a/root","reason":"dirty"}`))
	must(s.RecordTaskBranchMerged("p1", "a", "run-1", `{"branch":"ao/sess-a/root","sha":"abc"}`))
	must(s.FinishTask("p1", "a", "run-1", "", `{"marker":"ok","summary":"did it"}`))
	events, err := s.ListEvents("p1")
	if err != nil {
		t.Fatal(err)
	}
	var done, merged, blocked string
	for _, e := range events {
		switch e.Type {
		case EventTaskDone:
			done = e.PayloadJSON
		case EventTaskBranchMerged:
			merged = e.PayloadJSON
		case EventMergeBlocked:
			blocked = e.PayloadJSON
		}
	}
	if !strings.Contains(done, `"summary":"did it"`) {
		t.Fatalf("task_done payload not persisted: %q", done)
	}
	if !strings.Contains(merged, `"sha":"abc"`) || !strings.Contains(blocked, `"reason":"dirty"`) {
		t.Fatalf("audit payloads wrong: merged=%q blocked=%q", merged, blocked)
	}
	st := assertDeriveMatchesCache(t, s, "p1")
	if st.TaskStatus["a"] != TaskDone {
		t.Fatalf("audit events must not change derived state, got %q", st.TaskStatus["a"])
	}
}

func TestBrainEventsAppendOnly(t *testing.T) {
	s := openTestStore(t)
	mustCreatePlan(t, s, "p1", nt("a"))
	if _, err := s.db.Exec(`UPDATE brain_events SET type = 'hacked'`); err == nil {
		t.Fatal("UPDATE on brain_events should be blocked")
	}
	if _, err := s.db.Exec(`DELETE FROM brain_events`); err == nil {
		t.Fatal("DELETE on brain_events should be blocked")
	}
}

func TestRunLock(t *testing.T) {
	s := openTestStore(t)
	mustCreatePlan(t, s, "p1", nt("a"))
	stale := time.Minute
	if err := s.AcquireRunLock("p1", 100, stale); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if err := s.AcquireRunLock("p1", 200, stale); err == nil {
		t.Fatal("second om run must be rejected while lock is fresh")
	}
	if err := s.AcquireRunLock("p1", 100, stale); err != nil {
		t.Fatalf("re-acquire by same pid: %v", err)
	}
	if err := s.HeartbeatRunLock("p1", 100); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if err := s.HeartbeatRunLock("p1", 200); err == nil {
		t.Fatal("heartbeat by non-holder should fail")
	}
	// Stale lock can be stolen.
	if err := s.AcquireRunLock("p1", 200, -time.Second); err != nil {
		t.Fatalf("steal stale lock: %v", err)
	}
	if err := s.ReleaseRunLock("p1", 200); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := s.AcquireRunLock("p1", 300, stale); err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
}
