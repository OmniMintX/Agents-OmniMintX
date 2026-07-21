package store

import "time"

// Plan statuses (cache values; source of truth is the event log).
const (
	PlanDraft     = "draft"
	PlanApproved  = "approved"
	PlanRunning   = "running"
	PlanDone      = "done"
	PlanFailed    = "failed" // TERMINAL in Phase 1: retry = new plan
	PlanCancelled = "cancelled"
)

// Task statuses. "ready" is never stored: readiness is always derived
// (pending + all dependencies done); see GetReadyTasks.
// "dispatching" is the idempotent-dispatch intent: recorded BEFORE the
// CreateSession HTTP call so a crash in between is detectable on resume.
// "needs_human" pauses the task (and its timeout clock) until a human
// unblocks the AO session; it is NOT a failure.
const (
	TaskPending     = "pending"
	TaskReady       = "ready"
	TaskDispatching = "dispatching"
	TaskDispatched  = "dispatched"
	TaskRunning     = "running"
	TaskNeedsHuman  = "needs_human"
	TaskDone        = "done"
	TaskFailed      = "failed"
)

// Brain event types.
const (
	EventPlanCreated     = "plan_created"
	EventPlanApproved    = "plan_approved"
	EventRunStarted      = "run_started"
	EventPlanDone        = "plan_done"
	EventPlanFailed      = "plan_failed"
	EventPlanCancelled   = "plan_cancelled"
	EventTaskDispatching = "task_dispatching" // intent, written before the HTTP call
	EventTaskDispatched  = "task_dispatched"
	EventTaskStarted     = "task_started"
	EventTaskNeedsHuman  = "task_needs_human" // escalated; NOT a failure
	EventTaskResumed     = "task_resumed"     // human unblocked the session
	EventTaskDone        = "task_done"
	EventTaskFailed      = "task_failed"
	EventTaskRetry       = "task_retry"     // {round, tier, reason, feedback}: verify fail -> back to pending for re-dispatch
	EventAOUnreachable   = "ao_unreachable" // informational; no state change
	// Local-merge audit events (informational; git ancestry is the source
	// of truth for merged-ness, these only record what the scheduler did).
	EventTaskBranchMerged = "task_branch_merged" // {branch, sha}
	EventMergeBlocked     = "merge_blocked"      // {branch, reason}
	// Verify-pipeline audit events (OM-9, informational; the state change
	// on a tier-0 fail is the task_failed event that follows).
	EventTaskVerdict      = "task_verdict"       // {verdict: pass|fail, tier, reason?}
	EventTaskSystemCommit = "task_system_commit" // {branch, sha, files}
)

// Plan is a stored plan row.
type Plan struct {
	ID                 string
	Goal               string
	ProjectID          string
	Status             string
	RunLockPID         *int64
	RunLockHeartbeatAt *time.Time
	CreatedAt          time.Time
	ApprovedAt         *time.Time
}

// Task is a stored task row plus its dependency IDs.
type Task struct {
	ID          string
	PlanID      string
	Title       string
	Prompt      string
	Harness     string
	Check       string // tier-0 verify command from the planner (may be empty)
	Verify      string // verify strategy from the planner, e.g. "llm" (may be empty)
	Status      string
	AOSessionID *string
	Branch      *string // AO-assigned branch, e.g. ao/<sessionID>/root
	PRURL       *string
	DependsOn   []string
	CreatedAt   time.Time
}

// NewTask describes a task when creating a plan.
type NewTask struct {
	ID        string
	Title     string
	Prompt    string
	Harness   string
	Check     string // tier-0 verify command (may be empty)
	Verify    string // verify strategy, e.g. "llm" (may be empty)
	DependsOn []string
}

// Event is one append-only brain_events row.
type Event struct {
	ID          int64
	PlanID      string
	TaskID      *string
	RunID       string
	Type        string
	PayloadJSON string
	CreatedAt   time.Time
}
