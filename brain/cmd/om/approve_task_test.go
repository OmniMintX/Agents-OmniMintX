package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/OmniMintX/overmind/internal/config"
	"github.com/OmniMintX/overmind/internal/store"
)

// gatedPlanConfig seeds a running plan whose given tasks are already
// awaiting_approval and returns a Config pointing at its DB.
func gatedPlanConfig(t *testing.T, awaiting ...string) config.Config {
	t.Helper()
	cfg := config.Config{DBPath: filepath.Join(t.TempDir(), "om.db")}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	tasks := []store.NewTask{
		{ID: "t1", Title: "one", Prompt: "p", Harness: "claude-code", RequiresApproval: true},
		{ID: "t2", Title: "two", Prompt: "p", Harness: "claude-code", RequiresApproval: true},
		{ID: "t3", Title: "three", Prompt: "p", Harness: "claude-code"},
	}
	if err := st.CreatePlan("plan-1", "goal", "proj-1", tasks); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	if err := st.ApprovePlan("plan-1"); err != nil {
		t.Fatalf("approve plan: %v", err)
	}
	if err := st.StartRun("plan-1", "run-1"); err != nil {
		t.Fatalf("start run: %v", err)
	}
	for _, id := range awaiting {
		if err := st.RequestTaskApproval("plan-1", id, "run-1"); err != nil {
			t.Fatalf("request approval %s: %v", id, err)
		}
	}
	return cfg
}

func planTaskStatus(t *testing.T, cfg config.Config, taskID string) string {
	t.Helper()
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	ds, err := st.PlanState("plan-1")
	if err != nil {
		t.Fatalf("plan state: %v", err)
	}
	return ds.TaskStatus[taskID]
}

func TestApproveTaskSingle(t *testing.T) {
	cfg := gatedPlanConfig(t, "t1")
	if err := runApproveTask(cfg, "plan-1", "t1", false); err != nil {
		t.Fatalf("approve-task: %v", err)
	}
	if got := planTaskStatus(t, cfg, "t1"); got != store.TaskPending {
		t.Fatalf("t1 = %s, want pending", got)
	}
}

func TestApproveTaskErrors(t *testing.T) {
	cfg := gatedPlanConfig(t, "t1")
	if err := runApproveTask(cfg, "plan-x", "t1", false); err == nil {
		t.Error("unknown plan must error")
	}
	if err := runApproveTask(cfg, "plan-1", "nope", false); err == nil {
		t.Error("unknown task must error")
	}
	if err := runApproveTask(cfg, "plan-1", "t3", false); err == nil ||
		!strings.Contains(err.Error(), "cannot approve") {
		t.Errorf("approving a pending task: want status error, got %v", err)
	}
	if err := runApproveTask(cfg, "plan-1", "t1", true); err == nil ||
		!strings.Contains(err.Error(), "not both") {
		t.Errorf("--all with task id: want clear error, got %v", err)
	}
	if err := runApproveTask(cfg, "plan-1", "", false); err == nil {
		t.Error("neither task id nor --all must error")
	}
}

func TestApproveTaskAll(t *testing.T) {
	cfg := gatedPlanConfig(t, "t1", "t2")
	if err := runApproveTask(cfg, "plan-1", "", true); err != nil {
		t.Fatalf("approve-task --all: %v", err)
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	ds, err := st.PlanState("plan-1")
	if err != nil {
		t.Fatalf("plan state: %v", err)
	}
	if ds.TaskStatus["t1"] != store.TaskPending || ds.TaskStatus["t2"] != store.TaskPending {
		t.Fatalf("t1=%s t2=%s, want both pending", ds.TaskStatus["t1"], ds.TaskStatus["t2"])
	}
	if ds.TaskStatus["t3"] != store.TaskPending {
		t.Fatalf("t3 = %s, must be untouched", ds.TaskStatus["t3"])
	}
	// Exactly one task_approved per awaiting task — none for t3.
	events, err := st.ListEvents("plan-1")
	if err != nil {
		t.Fatal(err)
	}
	approved := map[string]int{}
	for _, e := range events {
		if e.Type == store.EventTaskApproved {
			approved[*e.TaskID]++
		}
	}
	if approved["t1"] != 1 || approved["t2"] != 1 || approved["t3"] != 0 {
		t.Fatalf("task_approved counts = %v, want t1:1 t2:1", approved)
	}
}

func TestApproveTaskAllNoneWaiting(t *testing.T) {
	cfg := gatedPlanConfig(t) // no task awaiting
	if err := runApproveTask(cfg, "plan-1", "", true); err != nil {
		t.Fatalf("--all with nothing waiting must succeed (exit 0), got %v", err)
	}
}

func TestRejectTask(t *testing.T) {
	cfg := gatedPlanConfig(t, "t1")
	if err := runRejectTask(cfg, "plan-1", "t1", "too risky"); err != nil {
		t.Fatalf("reject-task: %v", err)
	}
	if got := planTaskStatus(t, cfg, "t1"); got != store.TaskFailed {
		t.Fatalf("t1 = %s, want failed", got)
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	events, err := st.ListEvents("plan-1")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range events {
		if e.Type == store.EventTaskFailed &&
			strings.Contains(e.PayloadJSON, `"kind":"rejected"`) &&
			strings.Contains(e.PayloadJSON, "too risky") {
			found = true
		}
	}
	if !found {
		t.Fatal("task_failed must carry kind=rejected and the --reason")
	}
}

func TestRejectTaskErrors(t *testing.T) {
	cfg := gatedPlanConfig(t, "t1")
	if err := runRejectTask(cfg, "plan-x", "t1", ""); err == nil {
		t.Error("unknown plan must error")
	}
	if err := runRejectTask(cfg, "plan-1", "nope", ""); err == nil ||
		!strings.Contains(err.Error(), "not found") {
		t.Errorf("unknown task: want not-found error, got %v", err)
	}
	if err := runRejectTask(cfg, "plan-1", "t3", ""); err == nil ||
		!strings.Contains(err.Error(), "cannot reject") {
		t.Errorf("rejecting a pending task: want status error, got %v", err)
	}
}

// TestRejectTaskLosesRaceToApproveDispatch: the scheduler approves and
// dispatches the task between the CLI's PlanState read and the reject
// write (the OM-12 TOCTOU). RejectTask's in-transaction guard must refuse
// with the ACTUAL status — no task_failed event, no orphaned AO session.
func TestRejectTaskLosesRaceToApproveDispatch(t *testing.T) {
	cfg := gatedPlanConfig(t, "t1")
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	if err := st.ApproveTask("plan-1", "t1", "run-1"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if err := st.MarkTaskDispatching("plan-1", "t1", "run-1"); err != nil {
		t.Fatalf("mark dispatching: %v", err)
	}
	if err := st.DispatchTask("plan-1", "t1", "run-1", "sess-1", "branch-1"); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	err = runRejectTask(cfg, "plan-1", "t1", "changed my mind")
	if err == nil || !strings.Contains(err.Error(), "cannot reject") ||
		!strings.Contains(err.Error(), store.TaskDispatched) {
		t.Fatalf("want cannot-reject error naming status %q, got %v", store.TaskDispatched, err)
	}
	if got := planTaskStatus(t, cfg, "t1"); got != store.TaskDispatched {
		t.Fatalf("t1 = %s, want dispatched (session must not be orphaned)", got)
	}
	events, err := st.ListEvents("plan-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Type == store.EventTaskFailed {
			t.Fatalf("lost reject race must append no task_failed event, got %s", e.PayloadJSON)
		}
	}
}
