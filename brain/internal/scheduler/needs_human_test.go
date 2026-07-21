package scheduler

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/OmniMintX/overmind/internal/aoclient"
	"github.com/OmniMintX/overmind/internal/store"
)

// TestRunNeedsHumanEscalatesThenResumes: needs_input pauses the task
// (task_needs_human, no failure) and STOPS the timeout clock — the session
// stays needs_input far longer than TaskTimeout without failing. When the
// human unblocks it, the task resumes and finishes normally.
func TestRunNeedsHumanEscalatesThenResumes(t *testing.T) {
	st, ao, s := newHarness(t, []store.NewTask{nt("a1234567")},
		Config{TaskTimeout: time.Minute, PollInterval: 15 * time.Second})
	script := []sessStep{{status: aoclient.StatusWorking}}
	// 20 polls of needs_input = 5 minutes >> 1m TaskTimeout: must NOT fail.
	for i := 0; i < 20; i++ {
		script = append(script, sessStep{status: aoclient.StatusNeedsInput})
	}
	doneStep := stepMarker(aoclient.StatusIdle, "ok: finished after resume")
	doneStep.pr = 7
	script = append(script,
		sessStep{status: aoclient.StatusWorking},
		doneStep,
	)
	ao.scripts[displayNameFor("plan-1", "a1234567")] = script

	if err := s.Run(context.Background(), "plan-1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := planStatus(t, st); got != store.PlanDone {
		t.Fatalf("plan status = %s, want done", got)
	}
	if got := taskStatuses(t, st)["a1234567"]; got != store.TaskDone {
		t.Fatalf("task status = %s, want done", got)
	}
	events, err := st.ListEvents("plan-1")
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	var seen []string
	for _, e := range events {
		switch e.Type {
		case store.EventTaskNeedsHuman, store.EventTaskResumed, store.EventTaskFailed:
			seen = append(seen, e.Type)
		}
	}
	if len(seen) != 2 || seen[0] != store.EventTaskNeedsHuman || seen[1] != store.EventTaskResumed {
		t.Fatalf("event sequence = %v, want [task_needs_human task_resumed]", seen)
	}
	tasks, err := st.GetTasks("plan-1")
	if err != nil {
		t.Fatalf("GetTasks: %v", err)
	}
	if tasks[0].PRURL == nil || *tasks[0].PRURL != "https://example.test/pr/7" {
		t.Fatalf("pr_url not recorded: %+v", tasks[0])
	}
}

// TestRunChangesRequestedIsNeedsHuman: changes_requested is the second
// human-gated status; it must escalate the same way (and never fail).
func TestRunChangesRequestedIsNeedsHuman(t *testing.T) {
	st, ao, s := newHarness(t, []store.NewTask{nt("a1234567")},
		Config{TaskTimeout: time.Minute, PollInterval: 15 * time.Second})
	script := []sessStep{{status: aoclient.StatusPROpen, pr: 9}}
	for i := 0; i < 10; i++ {
		script = append(script, sessStep{status: aoclient.StatusChangesRequested})
	}
	script = append(script, sessStep{status: aoclient.StatusMerged})
	ao.scripts[displayNameFor("plan-1", "a1234567")] = script

	if err := s.Run(context.Background(), "plan-1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := taskStatuses(t, st)["a1234567"]; got != store.TaskDone {
		t.Fatalf("task status = %s, want done", got)
	}
	events, err := st.ListEvents("plan-1")
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	needsHuman := 0
	for _, e := range events {
		if e.Type == store.EventTaskNeedsHuman {
			needsHuman++
		}
		if e.Type == store.EventTaskFailed {
			t.Fatalf("changes_requested must not fail: %s", e.PayloadJSON)
		}
	}
	if needsHuman != 1 {
		t.Fatalf("task_needs_human events = %d, want 1", needsHuman)
	}
}

// TestRunFailedDependencyBlocksChild: when a parent fails, its pending
// child can never become ready; the plan must fail deterministically with
// the failed task named in the payload.
func TestRunFailedDependencyBlocksChild(t *testing.T) {
	st, ao, s := newHarness(t, []store.NewTask{
		nt("a1234567"), nt("b1234567", "a1234567"),
	}, Config{})
	// Parent terminates without the marker -> failed.
	ao.scripts[displayNameFor("plan-1", "a1234567")] = []sessStep{
		{status: aoclient.StatusWorking},
		{status: aoclient.StatusTerminated},
	}
	if err := s.Run(context.Background(), "plan-1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := planStatus(t, st); got != store.PlanFailed {
		t.Fatalf("plan status = %s, want failed", got)
	}
	sts := taskStatuses(t, st)
	if sts["a1234567"] != store.TaskFailed {
		t.Fatalf("parent = %s, want failed", sts["a1234567"])
	}
	// OM-12: the cascade fails the blocked child explicitly (kind=
	// dependency_failed) instead of leaving it pending forever.
	if sts["b1234567"] != store.TaskFailed {
		t.Fatalf("blocked child = %s, want failed (dependency_failed cascade)", sts["b1234567"])
	}
	var cascaded bool
	for _, p := range eventPayloads(t, st, store.EventTaskFailed) {
		if strings.Contains(p, `"kind":"dependency_failed"`) && strings.Contains(p, "dependency a1234567 failed") {
			cascaded = true
		}
	}
	if !cascaded {
		t.Fatal("child task_failed must have kind=dependency_failed naming the failed dep")
	}
}
