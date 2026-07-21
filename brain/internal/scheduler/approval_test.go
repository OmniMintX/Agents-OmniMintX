package scheduler

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OmniMintX/overmind/internal/aoclient"
	"github.com/OmniMintX/overmind/internal/store"
)

// fakeNotifier records every Notify call as "title | body".
type fakeNotifier struct {
	mu   sync.Mutex
	msgs []string
}

func (f *fakeNotifier) Notify(title, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.msgs = append(f.msgs, title+" | "+body)
}

func (f *fakeNotifier) all() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.msgs...)
}

// ntGated is nt() with requires_approval=true.
func ntGated(id string, deps ...string) store.NewTask {
	t := nt(id, deps...)
	t.RequiresApproval = true
	return t
}

// onSleep hooks the scheduler's Sleep so a test can act (approve, reject)
// between ticks, like a human running the CLI while om run polls.
func onSleep(s *Scheduler, fn func()) {
	base := s.Sleep
	s.Sleep = func(ctx context.Context, d time.Duration) error {
		fn()
		return base(ctx, d)
	}
}

// approveWhenAwaiting approves taskID on the first sleep that observes it
// awaiting_approval (once ready is true, if given).
func approveWhenAwaiting(t *testing.T, st *store.Store, s *Scheduler, taskID string, ready func(map[string]string) bool) {
	t.Helper()
	onSleep(s, func() {
		ds, err := st.PlanState("plan-1")
		if err != nil {
			t.Fatalf("plan state: %v", err)
		}
		if ds.TaskStatus[taskID] != store.TaskAwaitingApproval {
			return
		}
		if ready != nil && !ready(ds.TaskStatus) {
			return
		}
		if err := st.ApproveTask("plan-1", taskID, ds.LastRunID); err != nil {
			t.Fatalf("approve: %v", err)
		}
	})
}

// TestRunApprovalGateBlocksThenApproves: a requires_approval task must be
// parked in awaiting_approval BEFORE any AO session exists (the scheduler
// is the only gatekeeper), the plan must stay alive while it waits, and an
// `om approve-task` (simulated between ticks) must unblock dispatch and
// finish the plan. The notifier gets the approval-needed and plan-done
// notifications with the exact CLI hint.
func TestRunApprovalGateBlocksThenApproves(t *testing.T) {
	st, ao, s := newHarness(t, []store.NewTask{ntGated("a1234567")}, Config{})
	fn := &fakeNotifier{}
	s.Cfg.Notifier = fn
	ao.scripts[displayNameFor("plan-1", "a1234567")] = doneScript(0)

	sawAwaitingWithoutSession := false
	onSleep(s, func() {
		ds, err := st.PlanState("plan-1")
		if err != nil {
			t.Fatalf("plan state: %v", err)
		}
		if ds.TaskStatus["a1234567"] != store.TaskAwaitingApproval {
			return
		}
		if len(ao.events()) != 0 {
			t.Fatalf("awaiting_approval task must have NO AO session: %v", ao.events())
		}
		sawAwaitingWithoutSession = true
		if err := st.ApproveTask("plan-1", "a1234567", ds.LastRunID); err != nil {
			t.Fatalf("approve: %v", err)
		}
	})
	if err := s.Run(context.Background(), "plan-1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !sawAwaitingWithoutSession {
		t.Fatal("task was never observed awaiting approval")
	}
	if got := planStatus(t, st); got != store.PlanDone {
		t.Fatalf("plan status = %s, want done", got)
	}
	if got := taskStatuses(t, st)["a1234567"]; got != store.TaskDone {
		t.Fatalf("task = %s, want done", got)
	}
	if n := len(eventPayloads(t, st, store.EventTaskApprovalRequested)); n != 1 {
		t.Fatalf("task_approval_requested events = %d, want 1", n)
	}
	msgs := fn.all()
	if len(msgs) != 2 {
		t.Fatalf("notifications = %v, want [approval needed, plan done]", msgs)
	}
	if !strings.Contains(msgs[0], "approval needed") || !strings.Contains(msgs[0], "om approve-task plan-1 a1234567") {
		t.Fatalf("approval notification must carry the CLI hint: %q", msgs[0])
	}
	if !strings.Contains(msgs[1], "plan done") {
		t.Fatalf("want plan-done notification, got %q", msgs[1])
	}
}

// TestRunApprovalGateBurnsNoSlot: MaxParallel=1 with a gated task first in
// id order — the parked task must NOT consume the only slot, so the
// independent task still dispatches and finishes while the gated one
// waits. Approval then lets the gated task run to done.
func TestRunApprovalGateBurnsNoSlot(t *testing.T) {
	st, ao, s := newHarness(t, []store.NewTask{ntGated("a1234567"), nt("b1234567")}, Config{MaxParallel: 1})
	ao.scripts[displayNameFor("plan-1", "a1234567")] = doneScript(0)
	ao.scripts[displayNameFor("plan-1", "b1234567")] = doneScript(0)
	// Approve a ONLY after b finished: if the parked task burned the slot,
	// b could never dispatch and the run would hang instead of finishing.
	approveWhenAwaiting(t, st, s, "a1234567", func(sts map[string]string) bool {
		return sts["b1234567"] == store.TaskDone
	})
	if err := s.Run(context.Background(), "plan-1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := planStatus(t, st); got != store.PlanDone {
		t.Fatalf("plan status = %s, want done", got)
	}
	if ao.maxActive > 1 {
		t.Fatalf("maxActive = %d, want <= 1", ao.maxActive)
	}
}

// TestRunApprovalPermanentAcrossRetries: approval is PERMANENT — a tier-1
// verify fail sends the approved task back to pending (task_retry), and
// the round-1 re-dispatch must NOT be re-gated: exactly one
// task_approval_requested event for the whole run.
func TestRunApprovalPermanentAcrossRetries(t *testing.T) {
	gated := ntGated("a1234567")
	gated.Verify = "llm"
	st, ao, s := newHarness(t, []store.NewTask{gated}, Config{MaxVerifyRounds: 2})
	ao.scripts[displayNameFor("plan-1", "a1234567")] = doneScript(0)
	ao.scripts[displayNameForRound("plan-1", "a1234567", 1)] = doneScript(0)
	s.Verify = &fakeVerifier{steps: []verifyStep{vFail("not good enough"), vOK()}}
	approveWhenAwaiting(t, st, s, "a1234567", nil)
	if err := s.Run(context.Background(), "plan-1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := taskStatuses(t, st)["a1234567"]; got != store.TaskDone {
		t.Fatalf("task = %s, want done", got)
	}
	if n := len(eventPayloads(t, st, store.EventTaskRetry)); n != 1 {
		t.Fatalf("task_retry events = %d, want 1", n)
	}
	if n := len(eventPayloads(t, st, store.EventTaskApprovalRequested)); n != 1 {
		t.Fatalf("task_approval_requested events = %d, want 1 (retry must not re-gate)", n)
	}
}

// TestRunRejectFailsTaskAndCascades: an `om reject-task` (FailTask with
// kind=rejected, simulated between ticks) must terminate the gated task,
// cascade dependency_failed onto its pending dependent, and fail the plan
// with both listed in failed_tasks — while an independent branch still
// runs to done.
func TestRunRejectFailsTaskAndCascades(t *testing.T) {
	st, ao, s := newHarness(t, []store.NewTask{
		ntGated("a1234567"), nt("b1234567", "a1234567"), nt("c1234567"),
	}, Config{})
	ao.scripts[displayNameFor("plan-1", "c1234567")] = doneScript(0)
	onSleep(s, func() {
		ds, err := st.PlanState("plan-1")
		if err != nil {
			t.Fatalf("plan state: %v", err)
		}
		if ds.TaskStatus["a1234567"] != store.TaskAwaitingApproval {
			return
		}
		if err := st.FailTask("plan-1", "a1234567", ds.LastRunID, `{"kind":"rejected","reason":"nope"}`); err != nil {
			t.Fatalf("reject: %v", err)
		}
	})
	if err := s.Run(context.Background(), "plan-1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	sts := taskStatuses(t, st)
	if sts["a1234567"] != store.TaskFailed || sts["b1234567"] != store.TaskFailed {
		t.Fatalf("want a+b failed, got a=%s b=%s", sts["a1234567"], sts["b1234567"])
	}
	if sts["c1234567"] != store.TaskDone {
		t.Fatalf("independent task = %s, want done", sts["c1234567"])
	}
	if got := planStatus(t, st); got != store.PlanFailed {
		t.Fatalf("plan status = %s, want failed", got)
	}
	var cascaded bool
	for _, p := range eventPayloads(t, st, store.EventTaskFailed) {
		if strings.Contains(p, `"kind":"dependency_failed"`) && strings.Contains(p, "dependency a1234567 failed") {
			cascaded = true
		}
	}
	if !cascaded {
		t.Fatal("dependent must fail with kind=dependency_failed naming the rejected dep")
	}
	for _, p := range eventPayloads(t, st, store.EventPlanFailed) {
		for _, want := range []string{"a1234567", "b1234567"} {
			if !strings.Contains(p, want) {
				t.Fatalf("plan_failed payload missing %s: %s", want, p)
			}
		}
	}
}

// TestResumeAwaitingApprovalIdempotent: a crash after the approval request
// leaves the task awaiting_approval; the resumed run must NOT append a
// second task_approval_requested (RequestTaskApproval only fires for READY
// tasks, and awaiting_approval is not pending). Approval then completes
// the plan.
func TestResumeAwaitingApprovalIdempotent(t *testing.T) {
	st, ao, s := newHarness(t, []store.NewTask{ntGated("a1234567")}, Config{})
	ao.scripts[displayNameFor("plan-1", "a1234567")] = doneScript(0)
	// Simulate the crashed first run: the gate already parked the task.
	if err := st.StartRun("plan-1", "run-crashed"); err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if err := st.RequestTaskApproval("plan-1", "a1234567", "run-crashed"); err != nil {
		t.Fatalf("RequestTaskApproval: %v", err)
	}
	approveWhenAwaiting(t, st, s, "a1234567", nil)
	if err := s.Run(context.Background(), "plan-1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := planStatus(t, st); got != store.PlanDone {
		t.Fatalf("plan status = %s, want done", got)
	}
	if n := len(eventPayloads(t, st, store.EventTaskApprovalRequested)); n != 1 {
		t.Fatalf("task_approval_requested events = %d, want 1 (resume must not re-request)", n)
	}
}

// TestRunNotifierNeedsHumanAndPlanFailed: the two remaining notification
// points — task_needs_human (once per escalation, not per poll) and plan
// failed. A nil Notifier (every other test) is the silent default.
func TestRunNotifierNeedsHumanAndPlanFailed(t *testing.T) {
	st, ao, s := newHarness(t, []store.NewTask{nt("a1234567")},
		Config{TaskTimeout: time.Minute, PollInterval: 15 * time.Second})
	fn := &fakeNotifier{}
	s.Cfg.Notifier = fn
	ao.scripts[displayNameFor("plan-1", "a1234567")] = []sessStep{
		{status: aoclient.StatusWorking},
		{status: aoclient.StatusNeedsInput},
		{status: aoclient.StatusNeedsInput},
		{status: aoclient.StatusNeedsInput},
		{status: aoclient.StatusTerminated}, // human gave up: terminated, no marker -> failed
	}
	if err := s.Run(context.Background(), "plan-1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := planStatus(t, st); got != store.PlanFailed {
		t.Fatalf("plan status = %s, want failed", got)
	}
	msgs := fn.all()
	if len(msgs) != 2 {
		t.Fatalf("notifications = %v, want [needs human, plan failed]", msgs)
	}
	if !strings.Contains(msgs[0], "needs human") || !strings.Contains(msgs[0], "a1234567") {
		t.Fatalf("want needs-human notification naming the task, got %q", msgs[0])
	}
	if !strings.Contains(msgs[1], "plan failed") || !strings.Contains(msgs[1], "a1234567") {
		t.Fatalf("want plan-failed notification naming the task, got %q", msgs[1])
	}
}
