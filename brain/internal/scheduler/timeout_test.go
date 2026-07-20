package scheduler

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/OmniMintX/overmind/internal/aoclient"
	"github.com/OmniMintX/overmind/internal/store"
)

// TestRunAODownMidRun: AO becomes unreachable for a few polls mid-run. The
// scheduler must record exactly ONE ao_unreachable event, back off, and
// finish the plan once AO is back — never failing plan or task.
func TestRunAODownMidRun(t *testing.T) {
	st, ao, s := newHarness(t, []store.NewTask{nt("a1234567")}, Config{})
	ao.scripts[displayNameFor("a1234567")] = []sessStep{
		{status: aoclient.StatusWorking},
		{status: aoclient.StatusWorking},
		{status: aoclient.StatusIdle, marker: true},
	}
	created := false
	ao.onCreate = func(f *fakeAO, _ aoclient.SpawnSessionRequest) {
		if !created {
			created = true
			f.failGets = 4 // next 4 GetSession calls: daemon down
		}
	}
	if err := s.Run(context.Background(), "plan-1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := planStatus(t, st); got != store.PlanDone {
		t.Fatalf("plan status = %s, want done", got)
	}
	events, err := st.ListEvents("plan-1")
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	unreachable := 0
	for _, e := range events {
		if e.Type == store.EventAOUnreachable {
			unreachable++
		}
		if e.Type == store.EventPlanFailed || e.Type == store.EventTaskFailed {
			t.Fatalf("AO downtime must not fail anything, got %s (%s)", e.Type, e.PayloadJSON)
		}
	}
	if unreachable != 1 {
		t.Fatalf("ao_unreachable events = %d, want exactly 1 per outage", unreachable)
	}
}

// TestRunIdleNoMarkerTimeout: a session that sits idle WITHOUT the .om-done
// marker beyond TaskTimeout must be killed and its task failed.
func TestRunIdleNoMarkerTimeout(t *testing.T) {
	st, ao, s := newHarness(t, []store.NewTask{nt("a1234567")},
		Config{TaskTimeout: time.Minute, PollInterval: 15 * time.Second})
	ao.scripts[displayNameFor("a1234567")] = []sessStep{
		{status: aoclient.StatusIdle}, // idle forever, no marker
	}
	if err := s.Run(context.Background(), "plan-1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := planStatus(t, st); got != store.PlanFailed {
		t.Fatalf("plan status = %s, want failed", got)
	}
	if got := taskStatuses(t, st)["a1234567"]; got != store.TaskFailed {
		t.Fatalf("task status = %s, want failed", got)
	}
	killed := false
	for _, e := range ao.events() {
		if strings.HasPrefix(e, "kill:") {
			killed = true
		}
	}
	if !killed {
		t.Fatalf("hung session was not killed: %v", ao.events())
	}
	events, err := st.ListEvents("plan-1")
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	found := false
	for _, e := range events {
		if e.Type == store.EventTaskFailed && strings.Contains(e.PayloadJSON, DoneMarker) {
			found = true
		}
	}
	if !found {
		t.Fatalf("task_failed payload should name the missing %s marker", DoneMarker)
	}
}

// TestRunStuckWorkingTimeout: a session whose status never changes (working
// forever) must hit TaskTimeout and fail.
func TestRunStuckWorkingTimeout(t *testing.T) {
	st, ao, s := newHarness(t, []store.NewTask{nt("a1234567")},
		Config{TaskTimeout: time.Minute, PollInterval: 15 * time.Second})
	ao.scripts[displayNameFor("a1234567")] = []sessStep{{status: aoclient.StatusWorking}}
	if err := s.Run(context.Background(), "plan-1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := taskStatuses(t, st)["a1234567"]; got != store.TaskFailed {
		t.Fatalf("task status = %s, want failed", got)
	}
	if got := planStatus(t, st); got != store.PlanFailed {
		t.Fatalf("plan status = %s, want failed", got)
	}
}

// TestRunNoSignalTimeout: continuous no_signal beyond NoSignalTimeout is a
// hung agent -> kill + fail.
func TestRunNoSignalTimeout(t *testing.T) {
	st, ao, s := newHarness(t, []store.NewTask{nt("a1234567")},
		Config{NoSignalTimeout: 30 * time.Second, PollInterval: 15 * time.Second})
	ao.scripts[displayNameFor("a1234567")] = []sessStep{{status: aoclient.StatusNoSignal}}
	if err := s.Run(context.Background(), "plan-1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := taskStatuses(t, st)["a1234567"]; got != store.TaskFailed {
		t.Fatalf("task status = %s, want failed", got)
	}
	killed := false
	for _, e := range ao.events() {
		if strings.HasPrefix(e, "kill:") {
			killed = true
		}
	}
	if !killed {
		t.Fatalf("no_signal session was not killed: %v", ao.events())
	}
}
