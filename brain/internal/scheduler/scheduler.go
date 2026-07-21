// Package scheduler is Overmind's execution loop (om run): it dispatches
// ready tasks of an approved plan as AO sessions, polls them, and derives
// task outcomes until the plan is done or failed.
//
// Rules locked after the 07/2026 red-team reviews:
//   - Idempotent dispatch: task_dispatching (intent) is recorded BEFORE the
//     CreateSession HTTP call; displayName "om-"+hash(plan_id,task_id)[:8]
//     is the idempotency marker checked against ListSessions on resume.
//   - "Done" = terminal session whose per-task marker .om-done.<hex8> starts
//     with "ok:". The scheduler injects the marker protocol as a prompt
//     footer at dispatch (never trusts the planner LLM to relay it).
//   - Chaining: AO 0.10.x workers never open PRs (the daemon MergePR is a
//     stub), so on "ok" the scheduler merges the session branch into the
//     repo's default branch itself (gitops.Merger) BEFORE FinishTask; git
//     ancestry is the source of truth, ensureParentsMerged only re-checks.
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
	"os/exec"
	"strconv"
	"time"

	"github.com/OmniMintX/overmind/internal/aoclient"
	"github.com/OmniMintX/overmind/internal/gitops"
	"github.com/OmniMintX/overmind/internal/store"
	"github.com/OmniMintX/overmind/internal/verifier"
)

// DoneMarkerPrefix prefixes the per-task completion marker file name:
// ".om-done.<hex8>" where hex8 is the same hash used in the session
// displayName ("om-<hex8>"). Per-task names make a parent's committed
// marker harmless to its children (no inherited false-done).
const DoneMarkerPrefix = ".om-done."

// AO is the slice of the AO daemon API the scheduler needs. *aoclient.Client
// satisfies it; tests substitute a fake.
type AO interface {
	CreateSession(ctx context.Context, in aoclient.SpawnSessionRequest) (aoclient.Session, error)
	GetSession(ctx context.Context, sessionID string) (aoclient.Session, error)
	ListSessions(ctx context.Context, filter aoclient.ListSessionsFilter) ([]aoclient.Session, error)
	KillSession(ctx context.Context, sessionID string) (aoclient.KillSessionResult, error)
	ListProjects(ctx context.Context) ([]aoclient.ProjectSummary, error)
	PreviewFile(ctx context.Context, sessionID, filePath string) (string, bool, error)
}

// LocalMerger is the git surface the scheduler needs for chaining and the
// tier-0 verify + system-commit pipeline. gitops.Merger satisfies it;
// tests substitute a fake.
type LocalMerger interface {
	IsMerged(ctx context.Context, repo, branch, target string) (bool, error)
	Merge(ctx context.Context, repo, branch, target, msg string) (gitops.MergeResult, error)
	DefaultBranch(ctx context.Context, repo string) (string, error)
	HasRemoteBranch(ctx context.Context, repo, branch string) (bool, error)
	WorktreeFor(ctx context.Context, repo, branch string) (string, error)
	HasDiff(ctx context.Context, repo, branch, base string) (bool, error)
	HasUncommitted(ctx context.Context, dir string, exclude []string) (bool, error)
	CommitWorktree(ctx context.Context, dir, msg string, exclude []string) (gitops.CommitResult, error)
	DiffText(ctx context.Context, repo, base, branch string, maxBytes int) (string, error)
}

// Verifier is the tier-1 LLM gate: it grades one finished task's diff
// AFTER tier 0 and the system-commit. verifier.Verify with the
// roles.verifier LLM satisfies it (via VerifyFunc); a nil Scheduler.Verify
// skips tier 1 entirely.
type Verifier interface {
	Verify(ctx context.Context, in verifier.Input) (verifier.Verdict, error)
}

// VerifyFunc adapts a plain function to the Verifier interface.
type VerifyFunc func(ctx context.Context, in verifier.Input) (verifier.Verdict, error)

func (f VerifyFunc) Verify(ctx context.Context, in verifier.Input) (verifier.Verdict, error) {
	return f(ctx, in)
}

// Config are the scheduler knobs (from ~/.overmind/config.yaml).
type Config struct {
	MaxParallel         int           // concurrent AO sessions (default 3)
	PollInterval        time.Duration // session poll cadence (default 15s)
	TaskTimeout         time.Duration // max time without a status change (default 45m)
	NoSignalTimeout     time.Duration // max continuous no_signal (default 10m)
	IdleNoMarkerTimeout time.Duration // idle without the done marker (default 10m)
	LockStaleAfter      time.Duration // run-lock heartbeat freshness (default 60s)
	MaxBackoff          time.Duration // AO-unreachable backoff cap (default 60s)
	CheckTimeout        time.Duration // tier-0 check command budget (default 5m)
	// MaxVerifyRounds is the per-task retry budget on a verify fail
	// (tier 0 or 1): 0 means fail on the first verify fail. The consumed
	// rounds are derived from task_retry events (DerivedState.VerifyRounds),
	// never from in-memory state.
	MaxVerifyRounds int
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
	if c.IdleNoMarkerTimeout <= 0 {
		c.IdleNoMarkerTimeout = 10 * time.Minute
	}
	if c.LockStaleAfter <= 0 {
		c.LockStaleAfter = time.Minute
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = time.Minute
	}
	if c.CheckTimeout <= 0 {
		c.CheckTimeout = 5 * time.Minute
	}
	if c.MaxVerifyRounds < 0 {
		c.MaxVerifyRounds = 0
	}
	return c
}

// Scheduler runs one plan against the AO daemon. Zero-value hooks (Log,
// Now, Sleep) get sensible defaults; tests inject fakes.
type Scheduler struct {
	St  *store.Store
	AO  AO
	Git LocalMerger
	Cfg Config
	PID int64 // run-lock owner (os.Getpid())

	Log   io.Writer                                        // progress lines (default: discard)
	Now   func() time.Time                                 // clock (default: time.Now)
	Sleep func(ctx context.Context, d time.Duration) error // (default: timer+ctx)
	// RunCheck executes the planner's per-task tier-0 check command inside
	// the session worktree (default: sh -c, capped by Cfg.CheckTimeout).
	RunCheck func(ctx context.Context, dir, command string) (output string, err error)
	// Verify is the tier-1 LLM verifier for tasks with verify=llm. nil
	// skips tier 1 (om run fails fast earlier when the plan needs it).
	Verify Verifier
}

// taskClock tracks per-task timing observed by THIS process. It resets on
// resume (Phase 1: timeout clocks are in-memory, not derived from events).
type taskClock struct {
	lastStatus      aoclient.SessionStatus
	lastChangeAt    time.Time // when the observed AO status last changed
	noSignalAt      time.Time // start of the current no_signal streak (zero = none)
	markerBad       bool      // previous poll saw a malformed marker (grace of one poll)
	blockedNoted    bool      // merge_blocked already recorded for the current streak
	verified        bool      // tier-0 verify passed (blocked-merge re-polls skip it)
	systemCommitted bool      // system-commit step already performed
	verified1       bool      // tier-1 verify passed (blocked-merge re-polls skip it)
	llmErrs         int       // consecutive tier-1 LLM transport/call failures
}

// runner is the per-run state of one Scheduler.Run invocation.
type runner struct {
	*Scheduler
	plan          *store.Plan
	runID         string
	repo          string // project repo path (from AO ListProjects)
	defaultBranch string // merge target, e.g. "main"
	clocks        map[string]*taskClock
	merged        map[string]bool // parent task id -> branch verified merged
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

// taskHash8 is the shared 8-hex-char identity of one (plan, task) pair,
// used by both the session displayName ("om-<hex8>") and the per-task done
// marker (".om-done.<hex8>"). Planner task ids are sequential (t1..tN), so
// the plan id must participate or plans in the same project would collide.
func taskHash8(planID, taskID string) string {
	sum := sha256.Sum256([]byte(planID + "\x00" + taskID))
	return hex.EncodeToString(sum[:4])
}

// displayNameFor builds the AO session display name that doubles as the
// idempotent-dispatch marker (verify round 0). Always 11 runes, under AO's
// 20-rune cap.
func displayNameFor(planID, taskID string) string {
	return "om-" + taskHash8(planID, taskID)
}

// displayNameForRound is the round-aware displayName: round 0 keeps the
// original hash (backward compatible with pre-retry sessions); round > 0
// hashes the round in so each retry is its own idempotent-dispatch marker
// (reconcileDispatching must look for the CURRENT round's session, never
// adopt a previous round's).
func displayNameForRound(planID, taskID string, round int) string {
	if round <= 0 {
		return displayNameFor(planID, taskID)
	}
	sum := sha256.Sum256([]byte(planID + "\x00" + taskID + "\x00r" + strconv.Itoa(round)))
	return "om-" + hex.EncodeToString(sum[:4])
}

// markerPathFor is the repo-root file the task's agent must create as its
// last action: ".om-done.<hex8>", first line "ok: ..." or "fail: ...".
func markerPathFor(planID, taskID string) string {
	return DoneMarkerPrefix + taskHash8(planID, taskID)
}

// isTransport reports whether err means "the AO daemon is unreachable"
// (backoff, never fail) as opposed to an API-level answer.
func isTransport(err error) bool {
	return errors.Is(err, aoclient.ErrDaemonNotRunning)
}

// runCheck executes one tier-0 check command in dir via the injected
// RunCheck hook or the default sh -c runner (combined output, bounded by
// Cfg.CheckTimeout).
func (r *runner) runCheck(ctx context.Context, dir, command string) (string, error) {
	if r.RunCheck != nil {
		return r.RunCheck(ctx, dir, command)
	}
	cctx, cancel := context.WithTimeout(ctx, r.Cfg.CheckTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "sh", "-c", command)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
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
