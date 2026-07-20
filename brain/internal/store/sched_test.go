package store

import "testing"

// Tests for the scheduler-facing transitions added for OM-5:
// dispatching (idempotent-dispatch intent), needs_human (escalation, not
// failure), and the informational ao_unreachable event.

func TestDispatchingIntentFlow(t *testing.T) {
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
	must(s.MarkTaskDispatching("p1", "a", "run-1"))

	// A dispatching task is no longer ready (no double dispatch).
	if ready, _ := s.GetReadyTasks("p1"); len(ready) != 0 {
		t.Fatalf("dispatching task must not be ready, got %v", taskIDs(ready))
	}
	if err := s.MarkTaskDispatching("p1", "a", "run-1"); err == nil {
		t.Fatal("double dispatching intent must be rejected")
	}
	st := assertDeriveMatchesCache(t, s, "p1")
	if st.TaskStatus["a"] != TaskDispatching {
		t.Fatalf("task a = %q, want dispatching", st.TaskStatus["a"])
	}
	// Crash-resume completes the intent: dispatching -> dispatched.
	must(s.DispatchTask("p1", "a", "run-1", "sess-a", "ao/sess-a/root"))
	st = assertDeriveMatchesCache(t, s, "p1")
	if st.TaskStatus["a"] != TaskDispatched {
		t.Fatalf("task a = %q, want dispatched", st.TaskStatus["a"])
	}
}

func TestNeedsHumanIsNotFailure(t *testing.T) {
	s := openTestStore(t)
	mustCreatePlan(t, s, "p1", nt("a"), nt("b", "a"))
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.ApprovePlan("p1"))
	must(s.StartRun("p1", "run-1"))
	must(s.MarkTaskDispatching("p1", "a", "run-1"))
	must(s.DispatchTask("p1", "a", "run-1", "sess-a", "ao/sess-a/root"))
	must(s.StartTask("p1", "a", "run-1"))
	must(s.MarkTaskNeedsHuman("p1", "a", "run-1", `{"ao_status":"needs_input"}`))

	st := assertDeriveMatchesCache(t, s, "p1")
	if st.TaskStatus["a"] != TaskNeedsHuman {
		t.Fatalf("task a = %q, want needs_human", st.TaskStatus["a"])
	}
	// Dependents stay blocked but nothing fails.
	if ready, _ := s.GetReadyTasks("p1"); len(ready) != 0 {
		t.Fatalf("no task should be ready, got %v", taskIDs(ready))
	}
	// Human unblocks -> back to running, then done straight from needs_human
	// is also allowed (session may finish while escalated).
	must(s.ResumeTask("p1", "a", "run-1"))
	must(s.MarkTaskNeedsHuman("p1", "a", "run-1", "{}"))
	must(s.FinishTask("p1", "a", "run-1", "https://pr/1"))
	st = assertDeriveMatchesCache(t, s, "p1")
	if st.TaskStatus["a"] != TaskDone {
		t.Fatalf("task a = %q, want done", st.TaskStatus["a"])
	}
	if ready, _ := s.GetReadyTasks("p1"); len(ready) != 1 || ready[0].ID != "b" {
		t.Fatalf("want ready=b, got %v", taskIDs(ready))
	}
}

func TestFailFromDispatchingAndNeedsHuman(t *testing.T) {
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
	must(s.MarkTaskDispatching("p1", "a", "run-1"))
	must(s.FailTask("p1", "a", "run-1", `{"reason":"unresumable"}`))
	must(s.MarkTaskDispatching("p1", "b", "run-1"))
	must(s.DispatchTask("p1", "b", "run-1", "sess-b", "ao/sess-b/root"))
	must(s.MarkTaskNeedsHuman("p1", "b", "run-1", "{}"))
	must(s.FailTask("p1", "b", "run-1", `{"reason":"terminated without marker"}`))
	st := assertDeriveMatchesCache(t, s, "p1")
	if st.TaskStatus["a"] != TaskFailed || st.TaskStatus["b"] != TaskFailed {
		t.Fatalf("want both failed, got %+v", st.TaskStatus)
	}
}

func TestAOUnreachableEventChangesNothing(t *testing.T) {
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
	before := assertDeriveMatchesCache(t, s, "p1")
	must(s.RecordAOUnreachable("p1", "run-1", `{"error":"connection refused"}`))
	after := assertDeriveMatchesCache(t, s, "p1")
	if after.PlanStatus != before.PlanStatus || after.TaskStatus["a"] != before.TaskStatus["a"] {
		t.Fatalf("ao_unreachable must not change state: %+v -> %+v", before, after)
	}
	events, err := s.ListEvents("p1")
	if err != nil {
		t.Fatal(err)
	}
	last := events[len(events)-1]
	if last.Type != EventAOUnreachable || last.PayloadJSON != `{"error":"connection refused"}` {
		t.Fatalf("unexpected last event: %+v", last)
	}
}
