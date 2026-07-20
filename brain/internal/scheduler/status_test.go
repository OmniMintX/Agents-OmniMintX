package scheduler

import (
	"testing"

	"github.com/OmniMintX/overmind/internal/aoclient"
)

// TestClassifyAll13Statuses is the deliverable mapping table: every one of
// AO's 13 derived statuses must map to exactly the class chosen after the
// 07/2026 red-team review.
func TestClassifyAll13Statuses(t *testing.T) {
	want := map[aoclient.SessionStatus]Class{
		// In-pipeline statuses keep the task RUNNING (ci_failed included:
		// the harness is expected to iterate on CI failures itself).
		aoclient.StatusWorking:       ClassRunning,
		aoclient.StatusDraft:         ClassRunning,
		aoclient.StatusPROpen:        ClassRunning,
		aoclient.StatusReviewPending: ClassRunning,
		aoclient.StatusApproved:      ClassRunning,
		aoclient.StatusMergeable:     ClassRunning,
		aoclient.StatusCIFailed:      ClassRunning,
		// Human-gated statuses escalate, never fail.
		aoclient.StatusNeedsInput:       ClassNeedsHuman,
		aoclient.StatusChangesRequested: ClassNeedsHuman,
		// Terminal-ish statuses require an outcome decision.
		aoclient.StatusMerged:     ClassTerminal,
		aoclient.StatusTerminated: ClassTerminal,
		aoclient.StatusNoSignal:   ClassTerminal,
		aoclient.StatusIdle:       ClassTerminal,
	}
	if len(want) != 13 {
		t.Fatalf("mapping table covers %d statuses, want 13", len(want))
	}
	for status, class := range want {
		if got := Classify(status); got != class {
			t.Errorf("Classify(%q) = %v, want %v", status, got, class)
		}
	}
}

// Unknown statuses (e.g. a future AO "blocked") must pause + escalate,
// not kill or fail.
func TestClassifyUnknownStatusIsNeedsHuman(t *testing.T) {
	for _, s := range []aoclient.SessionStatus{"blocked", "", "some_future_status"} {
		if got := Classify(s); got != ClassNeedsHuman {
			t.Errorf("Classify(%q) = %v, want NEEDS_HUMAN", s, got)
		}
	}
}

func TestClassString(t *testing.T) {
	cases := map[Class]string{
		ClassRunning:    "RUNNING",
		ClassNeedsHuman: "NEEDS_HUMAN",
		ClassTerminal:   "TERMINAL",
		Class(99):       "UNKNOWN",
	}
	for c, want := range cases {
		if c.String() != want {
			t.Errorf("Class(%d).String() = %q, want %q", int(c), c.String(), want)
		}
	}
}
