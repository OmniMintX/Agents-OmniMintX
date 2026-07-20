package store

import "fmt"

// DerivedState is the plan/task state computed purely from brain_events.
// This is the source of truth; status columns are display caches only.
type DerivedState struct {
	PlanID     string
	PlanStatus string
	TaskStatus map[string]string // task id -> derived status
	LastRunID  string            // run_id of the most recent run_started
}

// PlanState replays the append-only event log and returns the derived
// state. Resume/crash-recovery MUST use this, never the cache columns.
func PlanState(events []Event, taskIDs []string) (*DerivedState, error) {
	st := &DerivedState{
		PlanStatus: PlanDraft,
		TaskStatus: make(map[string]string, len(taskIDs)),
	}
	for _, id := range taskIDs {
		st.TaskStatus[id] = TaskPending
	}
	for _, e := range events {
		st.PlanID = e.PlanID
		switch e.Type {
		case EventPlanCreated:
			st.PlanStatus = PlanDraft
		case EventPlanApproved:
			st.PlanStatus = PlanApproved
		case EventRunStarted:
			st.PlanStatus = PlanRunning
			st.LastRunID = e.RunID
		case EventPlanDone:
			st.PlanStatus = PlanDone
		case EventPlanFailed:
			st.PlanStatus = PlanFailed // TERMINAL in Phase 1
		case EventPlanCancelled:
			st.PlanStatus = PlanCancelled
		case EventAOUnreachable:
			// Informational only: the AO daemon being down never changes state.
		case EventTaskDispatching, EventTaskDispatched, EventTaskStarted,
			EventTaskNeedsHuman, EventTaskResumed, EventTaskDone, EventTaskFailed:
			if e.TaskID == nil {
				return nil, fmt.Errorf("event %d (%s): missing task_id", e.ID, e.Type)
			}
			switch e.Type {
			case EventTaskDispatching:
				st.TaskStatus[*e.TaskID] = TaskDispatching
			case EventTaskDispatched:
				st.TaskStatus[*e.TaskID] = TaskDispatched
			case EventTaskStarted:
				st.TaskStatus[*e.TaskID] = TaskRunning
			case EventTaskNeedsHuman:
				st.TaskStatus[*e.TaskID] = TaskNeedsHuman
			case EventTaskResumed:
				st.TaskStatus[*e.TaskID] = TaskRunning
			case EventTaskDone:
				st.TaskStatus[*e.TaskID] = TaskDone
			case EventTaskFailed:
				st.TaskStatus[*e.TaskID] = TaskFailed
			}
		default:
			return nil, fmt.Errorf("event %d: unknown type %q", e.ID, e.Type)
		}
	}
	return st, nil
}

// PlanState loads events + task IDs from the database and derives state.
func (s *Store) PlanState(planID string) (*DerivedState, error) {
	events, err := s.ListEvents(planID)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT id FROM tasks WHERE plan_id = ?`, planID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var taskIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		taskIDs = append(taskIDs, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	st, err := PlanState(events, taskIDs)
	if err != nil {
		return nil, err
	}
	st.PlanID = planID
	return st, nil
}
