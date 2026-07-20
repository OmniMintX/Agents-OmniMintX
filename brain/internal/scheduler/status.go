package scheduler

import "github.com/OmniMintX/overmind/internal/aoclient"

// Class is Overmind's interpretation of an AO session status. AO exposes 13
// derived statuses (docs/repo/agent-orchestrator/backend/internal/domain/
// status.go); the scheduler collapses them into three classes.
type Class int

const (
	// ClassRunning: the session is progressing through the AO pipeline;
	// the per-task timeout clock is ticking.
	ClassRunning Class = iota
	// ClassNeedsHuman: a human must unblock the session. The task is
	// escalated (task_needs_human, surfaced by om status), its timeout
	// clock STOPS, and it is NOT a failure.
	ClassNeedsHuman
	// ClassTerminal: an outcome must be decided. merged = done; idle is
	// resolved via the .om-done marker; terminated is classified by
	// marker + merge state; no_signal for too long = hung -> kill+failed.
	ClassTerminal
)

// String implements fmt.Stringer for logs and payloads.
func (c Class) String() string {
	switch c {
	case ClassRunning:
		return "RUNNING"
	case ClassNeedsHuman:
		return "NEEDS_HUMAN"
	case ClassTerminal:
		return "TERMINAL"
	default:
		return "UNKNOWN"
	}
}

// Classify maps each of AO's 13 session statuses to an Overmind class:
//
//	RUNNING:     working, draft, pr_open, review_pending, approved,
//	             mergeable, ci_failed (still inside the pipeline)
//	NEEDS_HUMAN: needs_input, changes_requested
//	TERMINAL:    merged, terminated, no_signal, idle
//
// Unknown/future statuses map to NEEDS_HUMAN: the safe reaction to an
// unrecognized state is to pause and escalate, never to kill or fail.
func Classify(s aoclient.SessionStatus) Class {
	switch s {
	case aoclient.StatusWorking, aoclient.StatusDraft, aoclient.StatusPROpen,
		aoclient.StatusReviewPending, aoclient.StatusApproved,
		aoclient.StatusMergeable, aoclient.StatusCIFailed:
		return ClassRunning
	case aoclient.StatusNeedsInput, aoclient.StatusChangesRequested:
		return ClassNeedsHuman
	case aoclient.StatusMerged, aoclient.StatusTerminated,
		aoclient.StatusNoSignal, aoclient.StatusIdle:
		return ClassTerminal
	default:
		return ClassNeedsHuman
	}
}
