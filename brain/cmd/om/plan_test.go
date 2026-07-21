package main

import (
	"path/filepath"
	"testing"

	"github.com/OmniMintX/overmind/internal/planner"
	"github.com/OmniMintX/overmind/internal/store"
)

// TestStoreTasksRoundTrip: planner fields — including the OM-10 verify
// strategy and its deterministic default when absent — survive the
// plan → store.CreatePlan → GetTasks round-trip.
func TestStoreTasksRoundTrip(t *testing.T) {
	plan, err := planner.Parse([]byte(`{"tasks":[
		{"title":"a","prompt":"Do A. Done when a.txt exists.","harness":"codex","check":"test -s a.txt","verify":"llm","depends_on":[]},
		{"title":"b","prompt":"Do B. Done when b.txt exists.","harness":"codex","depends_on":["a"]}
	]}`), []string{"codex"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	if err := st.CreatePlan("p1", "goal", "proj", storeTasks(plan.Tasks)); err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	tasks, err := st.GetTasks("p1")
	if err != nil {
		t.Fatalf("GetTasks: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("want 2 tasks, got %d", len(tasks))
	}
	if tasks[0].Verify != "llm" || tasks[0].Check != "test -s a.txt" {
		t.Fatalf("task a: verify = %q, check = %q; want llm / test -s a.txt", tasks[0].Verify, tasks[0].Check)
	}
	if tasks[1].Verify != "deterministic" {
		t.Fatalf("task b (no verify in JSON): verify = %q, want deterministic default", tasks[1].Verify)
	}
	if len(tasks[1].DependsOn) != 1 || tasks[1].DependsOn[0] != "t1" {
		t.Fatalf("task b depends_on = %v, want [t1]", tasks[1].DependsOn)
	}
}
