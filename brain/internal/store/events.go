package store

import (
	"database/sql"
	"fmt"
)

// appendEvent inserts one append-only brain_events row inside tx.
// taskID/runID may be empty; payload defaults to "{}".
func appendEvent(tx *sql.Tx, planID, taskID, runID, eventType, payloadJSON string) error {
	if payloadJSON == "" {
		payloadJSON = "{}"
	}
	var taskVal any
	if taskID != "" {
		taskVal = taskID
	}
	if _, err := tx.Exec(
		`INSERT INTO brain_events (plan_id, task_id, run_id, type, payload_json, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		planID, taskVal, runID, eventType, payloadJSON, now(),
	); err != nil {
		return fmt.Errorf("append event %s: %w", eventType, err)
	}
	return nil
}

// transition appends an event and updates the matching cache column in
// ONE transaction (the event-sourcing discipline).
func (s *Store) planTransition(planID, runID, eventType, payloadJSON, cacheUpdate string, args ...any) error {
	return s.inTx(func(tx *sql.Tx) error {
		res, err := tx.Exec(cacheUpdate, args...)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("plan %s: no row updated (unknown plan or invalid state)", planID)
		}
		return appendEvent(tx, planID, "", runID, eventType, payloadJSON)
	})
}

// ApprovePlan: draft -> approved.
func (s *Store) ApprovePlan(planID string) error {
	return s.planTransition(planID, "", EventPlanApproved, "",
		`UPDATE plans SET status = ?, approved_at = ? WHERE id = ? AND status = ?`,
		PlanApproved, now(), planID, PlanDraft)
}

// StartRun records a new run (fresh runID per `om run`): approved|running -> running.
func (s *Store) StartRun(planID, runID string) error {
	return s.planTransition(planID, runID, EventRunStarted, "",
		`UPDATE plans SET status = ? WHERE id = ? AND status IN (?, ?)`,
		PlanRunning, planID, PlanApproved, PlanRunning)
}

// FinishPlan marks the plan done.
func (s *Store) FinishPlan(planID, runID string) error {
	return s.planTransition(planID, runID, EventPlanDone, "",
		`UPDATE plans SET status = ? WHERE id = ? AND status = ?`,
		PlanDone, planID, PlanRunning)
}

// FailPlan marks the plan failed. TERMINAL in Phase 1: retry = new plan.
func (s *Store) FailPlan(planID, runID, payloadJSON string) error {
	return s.planTransition(planID, runID, EventPlanFailed, payloadJSON,
		`UPDATE plans SET status = ? WHERE id = ? AND status NOT IN (?, ?, ?)`,
		PlanFailed, planID, PlanDone, PlanFailed, PlanCancelled)
}

// CancelPlan marks the plan cancelled.
func (s *Store) CancelPlan(planID, runID string) error {
	return s.planTransition(planID, runID, EventPlanCancelled, "",
		`UPDATE plans SET status = ? WHERE id = ? AND status NOT IN (?, ?, ?)`,
		PlanCancelled, planID, PlanDone, PlanFailed, PlanCancelled)
}

// taskTransition is the single-transaction event+cache update for tasks.
func (s *Store) taskTransition(planID, taskID, runID, eventType, payloadJSON, cacheUpdate string, args ...any) error {
	return s.inTx(func(tx *sql.Tx) error {
		res, err := tx.Exec(cacheUpdate, args...)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("task %s: no row updated (unknown task or invalid state)", taskID)
		}
		return appendEvent(tx, planID, taskID, runID, eventType, payloadJSON)
	})
}

// MarkTaskDispatching records the dispatch INTENT (pending -> dispatching)
// BEFORE the CreateSession HTTP call, so a crash between intent and
// task_dispatched is detectable on resume (idempotent dispatch).
func (s *Store) MarkTaskDispatching(planID, taskID, runID string) error {
	return s.taskTransition(planID, taskID, runID, EventTaskDispatching, "",
		`UPDATE tasks SET status = ? WHERE id = ? AND plan_id = ? AND status = ?`,
		TaskDispatching, taskID, planID, TaskPending)
}

// DispatchTask: dispatching -> dispatched, recording AO session + branch.
func (s *Store) DispatchTask(planID, taskID, runID, aoSessionID, branch string) error {
	return s.taskTransition(planID, taskID, runID, EventTaskDispatched, "",
		`UPDATE tasks SET status = ?, ao_session_id = ?, branch = ? WHERE id = ? AND plan_id = ? AND status IN (?, ?)`,
		TaskDispatched, aoSessionID, branch, taskID, planID, TaskPending, TaskDispatching)
}

// StartTask: dispatched -> running.
func (s *Store) StartTask(planID, taskID, runID string) error {
	return s.taskTransition(planID, taskID, runID, EventTaskStarted, "",
		`UPDATE tasks SET status = ? WHERE id = ? AND plan_id = ? AND status = ?`,
		TaskRunning, taskID, planID, TaskDispatched)
}

// MarkTaskNeedsHuman: dispatched|running -> needs_human. NOT a failure:
// the timeout clock stops and om status escalates it to the user.
func (s *Store) MarkTaskNeedsHuman(planID, taskID, runID, payloadJSON string) error {
	return s.taskTransition(planID, taskID, runID, EventTaskNeedsHuman, payloadJSON,
		`UPDATE tasks SET status = ? WHERE id = ? AND plan_id = ? AND status IN (?, ?)`,
		TaskNeedsHuman, taskID, planID, TaskDispatched, TaskRunning)
}

// ResumeTask: needs_human -> running (a human unblocked the AO session).
func (s *Store) ResumeTask(planID, taskID, runID string) error {
	return s.taskTransition(planID, taskID, runID, EventTaskResumed, "",
		`UPDATE tasks SET status = ? WHERE id = ? AND plan_id = ? AND status = ?`,
		TaskRunning, taskID, planID, TaskNeedsHuman)
}

// RequestTaskApproval: pending -> awaiting_approval. The scheduler records
// this ONCE when a requires_approval task comes up for dispatch; the task
// then blocks BEFORE any AO session exists until ApproveTask (or
// RejectTask). Idempotence comes from the state guard: a task already
// awaiting approval is no longer pending, so a second call fails instead
// of double-appending the event.
func (s *Store) RequestTaskApproval(planID, taskID, runID string) error {
	return s.taskTransition(planID, taskID, runID, EventTaskApprovalRequested, "",
		`UPDATE tasks SET status = ? WHERE id = ? AND plan_id = ? AND status = ?`,
		TaskAwaitingApproval, taskID, planID, TaskPending)
}

// ApproveTask: awaiting_approval -> pending (`om approve-task`). The task
// becomes dispatchable again under the usual readiness rules. Approving a
// task in any other status is an explicit error naming the actual status.
func (s *Store) ApproveTask(planID, taskID, runID string) error {
	return s.inTx(func(tx *sql.Tx) error {
		res, err := tx.Exec(
			`UPDATE tasks SET status = ? WHERE id = ? AND plan_id = ? AND status = ?`,
			TaskPending, taskID, planID, TaskAwaitingApproval)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			var cur string
			if scanErr := tx.QueryRow(
				`SELECT status FROM tasks WHERE id = ? AND plan_id = ?`, taskID, planID,
			).Scan(&cur); scanErr != nil {
				return fmt.Errorf("task %s: not found in plan %s", taskID, planID)
			}
			return fmt.Errorf("task %s: cannot approve from status %q (only awaiting_approval tasks can be approved)", taskID, cur)
		}
		return appendEvent(tx, planID, taskID, runID, EventTaskApproved, "")
	})
}

// RejectTask: awaiting_approval -> failed (`om reject-task`), TERMINAL.
// payloadJSON carries {kind: "rejected", reason?}. The status guard and the
// event live in ONE transaction (same pattern as ApproveTask), so a task the
// scheduler approved+dispatched in between cannot be rejected out from under
// its live AO session; the error names the actual status.
func (s *Store) RejectTask(planID, taskID, runID, payloadJSON string) error {
	return s.inTx(func(tx *sql.Tx) error {
		res, err := tx.Exec(
			`UPDATE tasks SET status = ? WHERE id = ? AND plan_id = ? AND status = ?`,
			TaskFailed, taskID, planID, TaskAwaitingApproval)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			var cur string
			if scanErr := tx.QueryRow(
				`SELECT status FROM tasks WHERE id = ? AND plan_id = ?`, taskID, planID,
			).Scan(&cur); scanErr != nil {
				return fmt.Errorf("task %s: not found in plan %s", taskID, planID)
			}
			return fmt.Errorf("task %s: cannot reject from status %q (only awaiting_approval tasks can be rejected)", taskID, cur)
		}
		return appendEvent(tx, planID, taskID, runID, EventTaskFailed, payloadJSON)
	})
}

// RetryTask: dispatched|running|needs_human -> pending (verify failed, the
// task will be re-dispatched with feedback). Clears ao_session_id and branch:
// the old session is terminated and a retry always gets a NEW session. The
// retry budget is derived by counting task_retry events at replay
// (DerivedState.VerifyRounds), never from in-memory state.
func (s *Store) RetryTask(planID, taskID, runID, payloadJSON string) error {
	return s.taskTransition(planID, taskID, runID, EventTaskRetry, payloadJSON,
		`UPDATE tasks SET status = ?, ao_session_id = NULL, branch = NULL WHERE id = ? AND plan_id = ? AND status IN (?, ?, ?)`,
		TaskPending, taskID, planID, TaskDispatched, TaskRunning, TaskNeedsHuman)
}

// FinishTask: dispatched|running|needs_human -> done, with optional PR URL
// and a structured task_done payload (marker summary etc.).
func (s *Store) FinishTask(planID, taskID, runID, prURL, payloadJSON string) error {
	var pr any
	if prURL != "" {
		pr = prURL
	}
	return s.taskTransition(planID, taskID, runID, EventTaskDone, payloadJSON,
		`UPDATE tasks SET status = ?, pr_url = ? WHERE id = ? AND plan_id = ? AND status IN (?, ?, ?)`,
		TaskDone, pr, taskID, planID, TaskDispatched, TaskRunning, TaskNeedsHuman)
}

// FailTask: dispatching|dispatched|running|needs_human|awaiting_approval
// -> failed, TERMINAL. Rejects go through RejectTask (awaiting_approval
// only, one-transaction guard); pending dependents of a failed task go
// through FailPendingTask.
func (s *Store) FailTask(planID, taskID, runID, payloadJSON string) error {
	return s.taskTransition(planID, taskID, runID, EventTaskFailed, payloadJSON,
		`UPDATE tasks SET status = ? WHERE id = ? AND plan_id = ? AND status IN (?, ?, ?, ?, ?)`,
		TaskFailed, taskID, planID, TaskDispatching, TaskDispatched, TaskRunning, TaskNeedsHuman, TaskAwaitingApproval)
}

// FailPendingTask: pending -> failed, TERMINAL. Used by the scheduler's
// dependency_failed cascade: a pending task whose dependency failed never
// dispatches, so it fails DIRECTLY — no synthetic task_dispatching intent
// in the audit log for a session that never existed.
func (s *Store) FailPendingTask(planID, taskID, runID, payloadJSON string) error {
	return s.taskTransition(planID, taskID, runID, EventTaskFailed, payloadJSON,
		`UPDATE tasks SET status = ? WHERE id = ? AND plan_id = ? AND status = ?`,
		TaskFailed, taskID, planID, TaskPending)
}

// RecordAOUnreachable appends the informational ao_unreachable event (no
// cache/state change): the scheduler writes it ONCE per outage while it
// backs off; the plan must NOT fail because the AO daemon is down.
func (s *Store) RecordAOUnreachable(planID, runID, payloadJSON string) error {
	return s.inTx(func(tx *sql.Tx) error {
		return appendEvent(tx, planID, "", runID, EventAOUnreachable, payloadJSON)
	})
}

// RecordTaskBranchMerged appends the informational task_branch_merged
// audit event (no state change; git ancestry is the source of truth).
func (s *Store) RecordTaskBranchMerged(planID, taskID, runID, payloadJSON string) error {
	return s.inTx(func(tx *sql.Tx) error {
		return appendEvent(tx, planID, taskID, runID, EventTaskBranchMerged, payloadJSON)
	})
}

// RecordMergeBlocked appends the informational merge_blocked event: the
// local merge cannot proceed yet (dirty checkout / foreign merge) and will
// be retried next tick without failing the task.
func (s *Store) RecordMergeBlocked(planID, taskID, runID, payloadJSON string) error {
	return s.inTx(func(tx *sql.Tx) error {
		return appendEvent(tx, planID, taskID, runID, EventMergeBlocked, payloadJSON)
	})
}

// RecordTaskVerdict appends the informational task_verdict audit event
// ({verdict: pass|fail, tier, reason?}); a fail verdict is followed by its
// own task_failed state change.
func (s *Store) RecordTaskVerdict(planID, taskID, runID, payloadJSON string) error {
	return s.inTx(func(tx *sql.Tx) error {
		return appendEvent(tx, planID, taskID, runID, EventTaskVerdict, payloadJSON)
	})
}

// RecordTaskSystemCommit appends the informational task_system_commit
// audit event: the scheduler committed changes the worker left uncommitted
// in its session worktree before merging ({branch, sha, files}).
func (s *Store) RecordTaskSystemCommit(planID, taskID, runID, payloadJSON string) error {
	return s.inTx(func(tx *sql.Tx) error {
		return appendEvent(tx, planID, taskID, runID, EventTaskSystemCommit, payloadJSON)
	})
}

// ListEvents returns all events of a plan in append order (for `om events`).
func (s *Store) ListEvents(planID string) ([]Event, error) {
	rows, err := s.db.Query(
		`SELECT id, plan_id, task_id, run_id, type, payload_json, created_at
		 FROM brain_events WHERE plan_id = ? ORDER BY id`, planID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []Event
	for rows.Next() {
		var e Event
		var taskID, created sql.NullString
		if err := rows.Scan(&e.ID, &e.PlanID, &taskID, &e.RunID, &e.Type, &e.PayloadJSON, &created); err != nil {
			return nil, err
		}
		e.TaskID = nullStr(taskID)
		if e.CreatedAt, err = parseTime(created.String); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}
