package scheduler

import (
	"context"
	"strings"
	"testing"

	"github.com/OmniMintX/overmind/internal/aoclient"
	"github.com/OmniMintX/overmind/internal/store"
)

// doneScript is the happy path: one working poll, then idle with the
// .om-done marker and an open PR to merge.
func doneScript(pr int) []sessStep {
	return []sessStep{
		{status: aoclient.StatusWorking},
		{status: aoclient.StatusIdle, marker: true, pr: pr},
	}
}

// newHarness builds an in-memory store with one approved plan and a
// scheduler wired to the fake AO and fake clock.
func newHarness(t *testing.T, tasks []store.NewTask, cfg Config) (*store.Store, *fakeAO, *Scheduler) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.CreatePlan("plan-1", "test goal", "proj-1", tasks); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	if err := st.ApprovePlan("plan-1"); err != nil {
		t.Fatalf("approve plan: %v", err)
	}
	ao := newFakeAO()
	clock := newFakeClock()
	s := &Scheduler{St: st, AO: ao, Cfg: cfg, PID: 42, Now: clock.Now, Sleep: clock.Sleep}
	return st, ao, s
}

func taskStatuses(t *testing.T, st *store.Store) map[string]string {
	t.Helper()
	ds, err := st.PlanState("plan-1")
	if err != nil {
		t.Fatalf("plan state: %v", err)
	}
	return ds.TaskStatus
}

func planStatus(t *testing.T, st *store.Store) string {
	t.Helper()
	ds, err := st.PlanState("plan-1")
	if err != nil {
		t.Fatalf("plan state: %v", err)
	}
	return ds.PlanStatus
}

func nt(id string, deps ...string) store.NewTask {
	return store.NewTask{ID: id, Title: "task " + id, Prompt: "do " + id, Harness: "claude-code", DependsOn: deps}
}

// TestRunDAGOrder: chain a -> b -> c must dispatch strictly in dependency
// order, and each parent's PR must be merged BEFORE its child's session is
// created (merge-before-dispatch).
func TestRunDAGOrder(t *testing.T) {
	st, ao, s := newHarness(t, []store.NewTask{
		nt("a1234567"), nt("b1234567", "a1234567"), nt("c1234567", "b1234567"),
	}, Config{})
	ao.scripts[displayNameFor("plan-1", "a1234567")] = doneScript(1)
	ao.scripts[displayNameFor("plan-1", "b1234567")] = doneScript(2)
	ao.scripts[displayNameFor("plan-1", "c1234567")] = doneScript(3)

	if err := s.Run(context.Background(), "plan-1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := planStatus(t, st); got != store.PlanDone {
		t.Fatalf("plan status = %s, want done", got)
	}
	for id, status := range taskStatuses(t, st) {
		if status != store.TaskDone {
			t.Errorf("task %s = %s, want done", id, status)
		}
	}
	var order []string
	for _, e := range ao.events() {
		if strings.HasPrefix(e, "create:") || strings.HasPrefix(e, "merge:") {
			order = append(order, e)
		}
	}
	want := []string{
		"create:" + displayNameFor("plan-1", "a1234567"), "merge:1",
		"create:" + displayNameFor("plan-1", "b1234567"), "merge:2",
		"create:" + displayNameFor("plan-1", "c1234567"), "merge:3",
	}
	if len(order) != len(want) {
		t.Fatalf("event order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("event[%d] = %s, want %s (full: %v)", i, order[i], want[i], order)
		}
	}
}

// TestRunDoneMarkerPreviewFallback: AO 0.10.x daemons answer 404
// ROUTE_NOT_FOUND on the workspace/files listing; the scheduler must fall
// back to the per-file preview route to see .om-done instead of failing
// the task (found live in the OM-6 E2E run against AO 0.10.3).
func TestRunDoneMarkerPreviewFallback(t *testing.T) {
	st, ao, s := newHarness(t, []store.NewTask{nt("a1234567")}, Config{})
	ao.wsFilesRouteMissing = true
	ao.scripts[displayNameFor("plan-1", "a1234567")] = doneScript(1)

	if err := s.Run(context.Background(), "plan-1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := planStatus(t, st); got != store.PlanDone {
		t.Fatalf("plan status = %s, want done", got)
	}
	if got := taskStatuses(t, st)["a1234567"]; got != store.TaskDone {
		t.Fatalf("task a = %s, want done", got)
	}
}

// TestRunMaxParallel: 4 independent tasks with MaxParallel=2 must never
// have more than 2 live AO sessions at once.
func TestRunMaxParallel(t *testing.T) {
	tasks := []store.NewTask{nt("a1234567"), nt("b1234567"), nt("c1234567"), nt("d1234567")}
	st, ao, s := newHarness(t, tasks, Config{MaxParallel: 2})
	for _, tk := range tasks {
		ao.scripts[displayNameFor("plan-1", tk.ID)] = doneScript(0)
	}
	if err := s.Run(context.Background(), "plan-1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := planStatus(t, st); got != store.PlanDone {
		t.Fatalf("plan status = %s, want done", got)
	}
	if ao.maxActive > 2 {
		t.Fatalf("max concurrent sessions = %d, want <= 2", ao.maxActive)
	}
}

// TestRunEmptyPlanIsDone: a plan with zero tasks finishes immediately.
func TestRunEmptyPlanIsDone(t *testing.T) {
	st, _, s := newHarness(t, nil, Config{})
	if err := s.Run(context.Background(), "plan-1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := planStatus(t, st); got != store.PlanDone {
		t.Fatalf("plan status = %s, want done", got)
	}
}

// TestDisplayNameFor: the marker must be deterministic, plan-scoped (same
// task id in different plans must NOT collide — planner ids are t1..tN),
// and within AO's 20-rune displayName cap.
func TestDisplayNameFor(t *testing.T) {
	a := displayNameFor("p-aaaa1111", "t1")
	b := displayNameFor("p-bbbb2222", "t1")
	if a == b {
		t.Fatalf("same task id across plans collided: %s", a)
	}
	if a != displayNameFor("p-aaaa1111", "t1") {
		t.Fatalf("displayNameFor is not deterministic")
	}
	if c := displayNameFor("p-aaaa1111", "t2"); c == a {
		t.Fatalf("different task ids in one plan collided: %s", c)
	}
	for _, n := range []string{a, b} {
		if !strings.HasPrefix(n, "om-") {
			t.Fatalf("displayName %q must start with om-", n)
		}
		if len(n) != len("om-")+8 {
			t.Fatalf("displayName %q length = %d, want %d", n, len(n), len("om-")+8)
		}
	}
}


