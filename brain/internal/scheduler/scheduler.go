// Package scheduler is Overmind's execution loop (om run): it dispatches
// ready tasks of an approved plan as AO sessions, polls them, and derives
// task outcomes until the plan is done or failed.
//
// Rules locked after the 07/2026 red-team review:
//   - Idempotent dispatch: task_dispatching (intent) is recorded BEFORE the
//     CreateSession HTTP call; displayName "om-"+hash(plan_id,task_id)[:8]
//     is the idempotency marker checked against ListSessions on resume.
//   - "Done" = idle session with the .om-done marker file; its PRs are then
//     merged (merge-before-dispatch keeps children able to see parent code).
//   - needs_input/changes_requested = NEEDS_HUMAN: escalate, stop the
//     timeout clock, never fail.
//   - AO unreachable = exponential backoff + one ao_unreachable event; the
//     plan never fails because the daemon is down.
package scheduler

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/OmniMintX/overmind/internal/aoclient"
	"github.com/OmniMintX/overmind/internal/store"
)

// DoneMarker is the file an agent must create at the workspace root to
// signal "my work is complete" (AO has no done/success session status).
const DoneMarker = ".om-done"

// AO is the slice of the AO daemon API the scheduler needs. *aoclient.Client
// satisfies it; tests substitute a fake.
type AO interface {
	CreateSession(ctx context.Context, in aoclient.SpawnSessionRequest) (aoclient.Session, error)
	GetSession(ctx context.Context, sessionID string) (aoclient.Session, error)
	ListSessions(ctx context.Context, filter aoclient.ListSessionsFilter) ([]aoclient.Session, error)
	KillSession(ctx context.Context, sessionID string) (aoclient.KillSessionResult, error)
	MergePR(ctx context.Context, prID string) (aoclient.MergePRResult, error)
	ListWorkspaceFiles(ctx context.Context, sessionID string) (aoclient.WorkspaceFiles, error)
}

// Config are the scheduler knobs (from ~/.overmind/config.yaml).
type Config struct {
	MaxParallel     int           // concurrent AO sessions (default 3)
	PollInterval    time.Duration // session poll cadence (default 15s)
	TaskTimeout     time.Duration // max time without a status change (default 45m)
	NoSignalTimeout time.Duration // max continuous no_signal (default 10m)
	LockStaleAfter  time.Duration // run-lock heartbeat freshness (default 60s)
	MaxBackoff      time.Duration // AO-unreachable backoff cap (default 60s)
}

func (c Config) withDefaults() Config {
	if c.MaxParallel <= 0 {
		c.MaxParallel = 3
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 15 * time.Second
	}
	if c.TaskTimeout <= 0 {
		c.TaskTimeout = 45 * time.Minute
	}
	if c.NoSignalTimeout <= 0 {
		c.NoSignalTimeout = 10 * time.Minute
	}
	if c.LockStaleAfter <= 0 {
		c.LockStaleAfter = time.Minute
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = time.Minute
	}
	return c
}

// Scheduler runs one plan against the AO daemon. Zero-value hooks (Log,
// Now, Sleep) get sensible defaults; tests inject fakes.
type Scheduler struct {
	St  *store.Store
	AO  AO
	Cfg Config
	PID int64 // run-lock owner (os.Getpid())

	Log   io.Writer                                        // progress lines (default: discard)
	Now   func() time.Time                                 // clock (default: time.Now)
	Sleep func(ctx context.Context, d time.Duration) error // (default: timer+ctx)
}

// taskClock tracks per-task timing observed by THIS process. It resets on
// resume (Phase 1: timeout clocks are in-memory, not derived from events).
type taskClock struct {
	lastStatus   aoclient.SessionStatus
	lastChangeAt time.Time // when the observed AO status last changed
	noSignalAt   time.Time // start of the current no_signal streak (zero = none)
}

// runner is the per-run state of one Scheduler.Run invocation.
type runner struct {
	*Scheduler
	plan   *store.Plan
	runID  string
	clocks map[string]*taskClock
	merged map[string]bool // parent task id -> all its PRs verified merged
}

func (s *Scheduler) logf(format string, args ...any) {
	if s.Log != nil {
		fmt.Fprintf(s.Log, format+"\n", args...)
	}
}

func newRunID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("run-%d", time.Now().UnixNano())
	}
	return "run-" + hex.EncodeToString(b[:])
}

// displayNameFor builds the AO session display name that doubles as the
// idempotent-dispatch marker: "om-" + first 8 hex chars of
// sha256(planID, taskID). Planner task ids are sequential (t1..tN), so the
// plan id must participate or plans in the same project would collide and
// resume could adopt another plan's session. Always 11 runes, under AO's
// 20-rune cap.
func displayNameFor(planID, taskID string) string {
	sum := sha256.Sum256([]byte(planID + "\x00" + taskID))
	return "om-" + hex.EncodeToString(sum[:4])
}

// isTransport reports whether err means "the AO daemon is unreachable"
// (backoff, never fail) as opposed to an API-level answer.
func isTransport(err error) bool {
	return errors.Is(err, aoclient.ErrDaemonNotRunning)
}

func defaultSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
