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
	EventAOUnreachable   = "ao_unreachable" // informational; no state change
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
