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
	withCheck := nt("a")
	withCheck.Check = "test -s a.txt"
	withCheck.Verify = "llm"
	mustCreatePlan(t, s, "p1", withCheck, nt("b", "a"), nt("c", "a"), nt("d", "b", "c"))

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
		want := ""
		if task.ID == "a" {
			want = "test -s a.txt"
		}
		if task.Check != want {
			t.Fatalf("task %s check = %q, want %q", task.ID, task.Check, want)
		}
		wantVerify := ""
		if task.ID == "a" {
			wantVerify = "llm"
		}
		if task.Verify != wantVerify {
			t.Fatalf("task %s verify = %q, want %q", task.ID, task.Verify, wantVerify)
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
	// Populate the way the old binary did (no check_cmd column yet).
	ts := now()
	oldStmts := []string{
		`INSERT INTO plans (id, goal, project_id, status, created_at) VALUES ('p1', 'goal of p1', 'proj-1', 'draft', '` + ts + `')`,
		`INSERT INTO tasks (id, plan_id, title, prompt, harness, status, created_at) VALUES ('t1', 'p1', 't1', 'p', 'claude-code', 'pending', '` + ts + `')`,
		`INSERT INTO tasks (id, plan_id, title, prompt, harness, status, created_at) VALUES ('t2', 'p1', 't2', 'p', 'claude-code', 'pending', '` + ts + `')`,
		`INSERT INTO task_dependencies (plan_id, task_id, depends_on_task_id) VALUES ('p1', 't2', 't1')`,
	}
	for _, q := range oldStmts {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatalf("populate old schema: %v", err)
		}
	}
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

// TestMigrateTasksVerify: a database created before the verify column (OM-10)
// must get it via ALTER TABLE on Open, keeping existing rows, and new plans
// must round-trip Verify through CreatePlan/GetTasks.
func TestMigrateTasksVerify(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	// Recreate the pre-OM-10 layout: plan-scoped PK + check_cmd, no verify.
	stmts := []string{
		`DROP TABLE task_dependencies`,
		`DROP TABLE tasks`,
		`CREATE TABLE tasks (
		    id            TEXT NOT NULL,
		    plan_id       TEXT NOT NULL REFERENCES plans(id),
		    title         TEXT NOT NULL,
		    prompt        TEXT NOT NULL,
		    harness       TEXT NOT NULL,
		    check_cmd     TEXT NOT NULL DEFAULT '',
		    status        TEXT NOT NULL DEFAULT 'pending'
		        CHECK (status IN ('pending','ready','dispatching','dispatched','running','needs_human','done','failed')),
		    ao_session_id TEXT,
		    branch        TEXT,
		    pr_url        TEXT,
		    created_at    TEXT NOT NULL,
		    PRIMARY KEY (id, plan_id)
		)`,
		`CREATE TABLE task_dependencies (
		    plan_id            TEXT NOT NULL,
		    task_id            TEXT NOT NULL,
		    depends_on_task_id TEXT NOT NULL,
		    PRIMARY KEY (plan_id, task_id, depends_on_task_id),
		    CHECK (task_id <> depends_on_task_id),
		    FOREIGN KEY (task_id, plan_id) REFERENCES tasks(id, plan_id),
		    FOREIGN KEY (depends_on_task_id, plan_id) REFERENCES tasks(id, plan_id)
		)`,
		`INSERT INTO plans (id, goal, project_id, status, created_at) VALUES ('p1', 'goal of p1', 'proj-1', 'draft', '` + now() + `')`,
		`INSERT INTO tasks (id, plan_id, title, prompt, harness, status, created_at) VALUES ('t1', 'p1', 't1', 'p', 'claude-code', 'pending', '` + now() + `')`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatalf("prepare old schema: %v", err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen (migration): %v", err)
	}
	t.Cleanup(func() { s2.Close() })
	tasks, err := s2.GetTasks("p1")
	if err != nil || len(tasks) != 1 {
		t.Fatalf("after migration: want 1 task, got %d (err %v)", len(tasks), err)
	}
	if tasks[0].Verify != "" {
		t.Fatalf("pre-migration row verify = %q, want empty", tasks[0].Verify)
	}
	withVerify := nt("t1")
	withVerify.Verify = "llm"
	mustCreatePlan(t, s2, "p2", withVerify)
	tasks, err = s2.GetTasks("p2")
	if err != nil || len(tasks) != 1 || tasks[0].Verify != "llm" {
		t.Fatalf("verify round-trip after migration: %+v (err %v)", tasks, err)
	}
}

// TestRetryTask: verify-fail retries go back to pending (clearing the dead
// session), are rejected from invalid states, and the retry budget is
// derived by counting task_retry events — correct even when retries
// interleave with dispatch/done and after a cache wipe.
func TestRetryTask(t *testing.T) {
	s := openTestStore(t)
	mustCreatePlan(t, s, "p1", nt("a"), nt("b"))
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.ApprovePlan("p1"))
	must(s.StartRun("p1", "run-1"))

	// Pending task cannot be retried.
	if err := s.RetryTask("p1", "a", "run-1", ""); err == nil {
		t.Fatal("retry of a pending task should fail")
	}

	// Round 1: dispatched -> pending, session/branch cleared.
	must(s.DispatchTask("p1", "a", "run-1", "sess-a1", "ao/sess-a1/root"))
	must(s.RetryTask("p1", "a", "run-1", `{"round":1,"tier":1,"reason":"fail","feedback":["x"]}`))
	tasks, err := s.GetTasks("p1")
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		if task.ID != "a" {
			continue
		}
		if task.Status != TaskPending || task.AOSessionID != nil || task.Branch != nil {
			t.Fatalf("after retry: %+v (session/branch must be cleared)", task)
		}
	}
	st := assertDeriveMatchesCache(t, s, "p1")
	if st.VerifyRounds["a"] != 1 || st.VerifyRounds["b"] != 0 {
		t.Fatalf("VerifyRounds = %v, want a=1 b=0", st.VerifyRounds)
	}

	// Round 2: running -> pending. Round 3: needs_human -> pending.
	must(s.DispatchTask("p1", "a", "run-1", "sess-a2", "ao/sess-a2/root"))
	must(s.StartTask("p1", "a", "run-1"))
	must(s.RetryTask("p1", "a", "run-1", `{"round":2}`))
	must(s.DispatchTask("p1", "a", "run-1", "sess-a3", "ao/sess-a3/root"))
	must(s.MarkTaskNeedsHuman("p1", "a", "run-1", ""))
	must(s.RetryTask("p1", "a", "run-1", `{"round":3}`))

	// Interleaved dispatch/done after retries: rounds stay at 3, status done.
	must(s.DispatchTask("p1", "a", "run-1", "sess-a4", "ao/sess-a4/root"))
	must(s.FinishTask("p1", "a", "run-1", "", ""))
	if err := s.RetryTask("p1", "a", "run-1", ""); err == nil {
		t.Fatal("retry of a done task should fail")
	}
	st = assertDeriveMatchesCache(t, s, "p1")
	if st.TaskStatus["a"] != TaskDone || st.VerifyRounds["a"] != 3 {
		t.Fatalf("after done: status=%q rounds=%d, want done/3", st.TaskStatus["a"], st.VerifyRounds["a"])
	}

	// Replay is the source of truth: rounds survive a cache wipe.
	if _, err := s.db.Exec(`UPDATE tasks SET status='pending' WHERE plan_id='p1'`); err != nil {
		t.Fatal(err)
	}
	st2, err := s.PlanState("p1")
	if err != nil {
		t.Fatal(err)
	}
	if st2.TaskStatus["a"] != TaskDone || st2.VerifyRounds["a"] != 3 || st2.VerifyRounds["b"] != 0 {
		t.Fatalf("after cache wipe: %+v / %v", st2.TaskStatus, st2.VerifyRounds)
	}

	// task_retry payload persists into brain_events.
	events, err := s.ListEvents("p1")
	if err != nil {
		t.Fatal(err)
	}
	var retries int
	var firstPayload string
	for _, e := range events {
		if e.Type == EventTaskRetry {
			retries++
			if firstPayload == "" {
				firstPayload = e.PayloadJSON
			}
		}
	}
	if retries != 3 || !strings.Contains(firstPayload, `"reason":"fail"`) {
		t.Fatalf("task_retry events = %d payload = %q", retries, firstPayload)
	}
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
	must(s.RecordTaskVerdict("p1", "a", "run-1", `{"verdict":"pass","tier":0}`))
	must(s.RecordTaskSystemCommit("p1", "a", "run-1", `{"branch":"ao/sess-a/root","sha":"def","files":["x.txt"]}`))
	must(s.RecordTaskBranchMerged("p1", "a", "run-1", `{"branch":"ao/sess-a/root","sha":"abc"}`))
	must(s.FinishTask("p1", "a", "run-1", "", `{"marker":"ok","summary":"did it"}`))
	events, err := s.ListEvents("p1")
	if err != nil {
		t.Fatal(err)
	}
	var done, merged, blocked, verdict, sysCommit string
	for _, e := range events {
		switch e.Type {
		case EventTaskDone:
			done = e.PayloadJSON
		case EventTaskBranchMerged:
			merged = e.PayloadJSON
		case EventMergeBlocked:
			blocked = e.PayloadJSON
		case EventTaskVerdict:
			verdict = e.PayloadJSON
		case EventTaskSystemCommit:
			sysCommit = e.PayloadJSON
		}
	}
	if !strings.Contains(done, `"summary":"did it"`) {
		t.Fatalf("task_done payload not persisted: %q", done)
	}
	if !strings.Contains(merged, `"sha":"abc"`) || !strings.Contains(blocked, `"reason":"dirty"`) {
		t.Fatalf("audit payloads wrong: merged=%q blocked=%q", merged, blocked)
	}
	if !strings.Contains(verdict, `"verdict":"pass"`) || !strings.Contains(sysCommit, `"sha":"def"`) {
		t.Fatalf("verify audit payloads wrong: verdict=%q system_commit=%q", verdict, sysCommit)
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
	// All holders are "alive" here; dead-holder takeover has its own test.
	orig := pidAlive
	pidAlive = func(int64) bool { return true }
	t.Cleanup(func() { pidAlive = orig })
	stale := time.Minute
	if _, err := s.AcquireRunLock("p1", 100, stale); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if _, err := s.AcquireRunLock("p1", 200, stale); err == nil {
		t.Fatal("second om run must be rejected while lock is fresh and holder alive")
	}
	if _, err := s.AcquireRunLock("p1", 100, stale); err != nil {
		t.Fatalf("re-acquire by same pid: %v", err)
	}
	if err := s.HeartbeatRunLock("p1", 100); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if err := s.HeartbeatRunLock("p1", 200); err == nil {
		t.Fatal("heartbeat by non-holder should fail")
	}
	// Stale lock can be stolen even from a live holder.
	if _, err := s.AcquireRunLock("p1", 200, -time.Second); err != nil {
		t.Fatalf("steal stale lock: %v", err)
	}
	if err := s.ReleaseRunLock("p1", 200); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := s.AcquireRunLock("p1", 300, stale); err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
}

// TestRunLockDeadHolder: a FRESH lock whose holder process is dead must be
// taken over immediately (kill -9 leaves the row behind; found live in the
// OM-6b E2E run 3: stale lock blocked resume until the heartbeat aged out).
func TestRunLockDeadHolder(t *testing.T) {
	s := openTestStore(t)
	mustCreatePlan(t, s, "p1", nt("a"))
	orig := pidAlive
	t.Cleanup(func() { pidAlive = orig })
	stale := time.Minute

	pidAlive = func(int64) bool { return true }
	if _, err := s.AcquireRunLock("p1", 100, stale); err != nil {
		t.Fatalf("seed lock: %v", err)
	}

	// Holder alive -> reject.
	if _, err := s.AcquireRunLock("p1", 200, stale); err == nil {
		t.Fatal("live holder with fresh heartbeat must reject")
	}

	// Holder dead -> take over, tookOver=true.
	pidAlive = func(int64) bool { return false }
	tookOver, err := s.AcquireRunLock("p1", 200, stale)
	if err != nil {
		t.Fatalf("takeover from dead holder: %v", err)
	}
	if !tookOver {
		t.Fatal("tookOver = false, want true when stealing from dead holder")
	}
	p, err := s.GetPlan("p1")
	if err != nil {
		t.Fatalf("GetPlan: %v", err)
	}
	if p.RunLockPID == nil || *p.RunLockPID != 200 {
		t.Fatalf("lock holder = %v, want 200", p.RunLockPID)
	}
}
