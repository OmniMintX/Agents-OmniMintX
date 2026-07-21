package store

import (
	"database/sql"
	"fmt"
)

// CreatePlan validates the DAG and stores plan + tasks + dependencies +
// the plan_created event in one transaction.
func (s *Store) CreatePlan(planID, goal, projectID string, tasks []NewTask) error {
	if err := validateDAG(tasks); err != nil {
		return err
	}
	ts := now()
	return s.inTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(
			`INSERT INTO plans (id, goal, project_id, status, created_at) VALUES (?, ?, ?, ?, ?)`,
			planID, goal, projectID, PlanDraft, ts,
		); err != nil {
			return fmt.Errorf("insert plan: %w", err)
		}
		for _, t := range tasks {
			if _, err := tx.Exec(
				`INSERT INTO tasks (id, plan_id, title, prompt, harness, check_cmd, verify, status, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				t.ID, planID, t.Title, t.Prompt, t.Harness, t.Check, t.Verify, TaskPending, ts,
			); err != nil {
				return fmt.Errorf("insert task %s: %w", t.ID, err)
			}
		}
		for _, t := range tasks {
			for _, dep := range t.DependsOn {
				if _, err := tx.Exec(
					`INSERT INTO task_dependencies (plan_id, task_id, depends_on_task_id) VALUES (?, ?, ?)`,
					planID, t.ID, dep,
				); err != nil {
					return fmt.Errorf("insert dependency %s -> %s: %w", t.ID, dep, err)
				}
			}
		}
		return appendEvent(tx, planID, "", "", EventPlanCreated, "{}")
	})
}

// GetPlan returns one plan row.
func (s *Store) GetPlan(planID string) (*Plan, error) {
	row := s.db.QueryRow(
		`SELECT id, goal, project_id, status, run_lock_pid, run_lock_heartbeat_at, created_at, approved_at
		 FROM plans WHERE id = ?`, planID)
	var p Plan
	var pid sql.NullInt64
	var hb, created, approved sql.NullString
	if err := row.Scan(&p.ID, &p.Goal, &p.ProjectID, &p.Status, &pid, &hb, &created, &approved); err != nil {
		return nil, err
	}
	if pid.Valid {
		p.RunLockPID = &pid.Int64
	}
	var err error
	if p.RunLockHeartbeatAt, err = parseNullTime(hb); err != nil {
		return nil, err
	}
	if p.CreatedAt, err = parseTime(created.String); err != nil {
		return nil, err
	}
	if p.ApprovedAt, err = parseNullTime(approved); err != nil {
		return nil, err
	}
	return &p, nil
}

// ListPlans returns all plans, newest first (for `om status`).
func (s *Store) ListPlans() ([]Plan, error) {
	rows, err := s.db.Query(
		`SELECT id, goal, project_id, status, run_lock_pid, run_lock_heartbeat_at, created_at, approved_at
		 FROM plans ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var plans []Plan
	for rows.Next() {
		var p Plan
		var pid sql.NullInt64
		var hb, created, approved sql.NullString
		if err := rows.Scan(&p.ID, &p.Goal, &p.ProjectID, &p.Status, &pid, &hb, &created, &approved); err != nil {
			return nil, err
		}
		if pid.Valid {
			p.RunLockPID = &pid.Int64
		}
		if p.RunLockHeartbeatAt, err = parseNullTime(hb); err != nil {
			return nil, err
		}
		if p.CreatedAt, err = parseTime(created.String); err != nil {
			return nil, err
		}
		if p.ApprovedAt, err = parseNullTime(approved); err != nil {
			return nil, err
		}
		plans = append(plans, p)
	}
	return plans, rows.Err()
}

// GetTasks returns all tasks of a plan (with dependency IDs), ordered by id.
func (s *Store) GetTasks(planID string) ([]Task, error) {
	rows, err := s.db.Query(
		`SELECT id, plan_id, title, prompt, harness, check_cmd, verify, status, ao_session_id, branch, pr_url, created_at
		 FROM tasks WHERE plan_id = ? ORDER BY id`, planID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tasks []Task
	for rows.Next() {
		var t Task
		var sess, branch, pr, created sql.NullString
		if err := rows.Scan(&t.ID, &t.PlanID, &t.Title, &t.Prompt, &t.Harness, &t.Check, &t.Verify, &t.Status, &sess, &branch, &pr, &created); err != nil {
			return nil, err
		}
		t.AOSessionID, t.Branch, t.PRURL = nullStr(sess), nullStr(branch), nullStr(pr)
		if t.CreatedAt, err = parseTime(created.String); err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	byID := make(map[string]*Task, len(tasks))
	for i := range tasks {
		byID[tasks[i].ID] = &tasks[i]
	}
	deps, err := s.db.Query(
		`SELECT task_id, depends_on_task_id FROM task_dependencies WHERE plan_id = ? ORDER BY task_id, depends_on_task_id`, planID)
	if err != nil {
		return nil, err
	}
	defer deps.Close()
	for deps.Next() {
		var taskID, depID string
		if err := deps.Scan(&taskID, &depID); err != nil {
			return nil, err
		}
		if t := byID[taskID]; t != nil {
			t.DependsOn = append(t.DependsOn, depID)
		}
	}
	return tasks, deps.Err()
}

// GetReadyTasks returns pending tasks whose dependencies are all done
// (beads "ready work" pattern). Statuses come from the DERIVED event
// state, not the cache columns, so resume after crash is always correct.
func (s *Store) GetReadyTasks(planID string) ([]Task, error) {
	tasks, err := s.GetTasks(planID)
	if err != nil {
		return nil, err
	}
	st, err := s.PlanState(planID)
	if err != nil {
		return nil, err
	}
	var ready []Task
	for _, t := range tasks {
		if st.TaskStatus[t.ID] != TaskPending {
			continue
		}
		ok := true
		for _, dep := range t.DependsOn {
			if st.TaskStatus[dep] != TaskDone {
				ok = false
				break
			}
		}
		if ok {
			ready = append(ready, t)
		}
	}
	return ready, nil
}
