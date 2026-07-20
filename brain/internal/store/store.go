// Package store is the Overmind data layer: plans are data, state is
// derived from the append-only brain_events log.
//
// Event sourcing discipline:
//   - Every state change is ONE transaction: INSERT brain_event + UPDATE
//     the status cache column.
//   - Resume/replay MUST read from PlanState (derive from events); the
//     cache columns exist only for cheap display (om status).
//
// Phase 1: a failed plan is TERMINAL — to retry, create a new plan.
package store

import (
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

// timeFormat is how timestamps are persisted (TEXT, RFC3339, UTC).
const timeFormat = time.RFC3339Nano

// Store wraps a SQLite database holding Overmind plans and events.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite database at path and applies
// the embedded schema. Use ":memory:" for tests.
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// modernc.org/sqlite: a single connection avoids table-lock races and
	// keeps :memory: databases stable across "connections".
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := migrateTasksPK(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// migrateTasksPK rebuilds the tasks and task_dependencies tables for
// databases created when their primary keys were not plan-scoped: the
// planner reuses t1..tN in every plan, so global PKs made every plan after
// the first fail with a UNIQUE violation. The rebuild swaps in
// PRIMARY KEY (id, plan_id) on tasks and
// PRIMARY KEY (plan_id, task_id, depends_on_task_id) on task_dependencies.
func migrateTasksPK(db *sql.DB) error {
	var tasksDDL, depsDDL string
	err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='tasks'`).Scan(&tasksDDL)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("migrate tasks pk: read tasks ddl: %w", err)
	}
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='task_dependencies'`).Scan(&depsDDL); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("migrate tasks pk: read task_dependencies ddl: %w", err)
	}
	tasksOK := strings.Contains(tasksDDL, "PRIMARY KEY (id, plan_id)")
	depsOK := strings.Contains(depsDDL, "PRIMARY KEY (plan_id, task_id, depends_on_task_id)")
	if tasksOK && depsOK {
		return nil
	}
	// FKs must be off so DROP TABLE does not trip the composite FKs;
	// PRAGMA is per-connection and the pool is capped at one connection.
	stmts := []string{
		`PRAGMA foreign_keys=OFF`,
		`BEGIN IMMEDIATE`,
	}
	if !tasksOK {
		stmts = append(stmts,
			`CREATE TABLE tasks_new (
			    id            TEXT NOT NULL,
			    plan_id       TEXT NOT NULL REFERENCES plans(id),
			    title         TEXT NOT NULL,
			    prompt        TEXT NOT NULL,
			    harness       TEXT NOT NULL,
			    status        TEXT NOT NULL DEFAULT 'pending'
			        CHECK (status IN ('pending','ready','dispatching','dispatched','running','needs_human','done','failed')),
			    ao_session_id TEXT,
			    branch        TEXT,
			    pr_url        TEXT,
			    created_at    TEXT NOT NULL,
			    PRIMARY KEY (id, plan_id)
			)`,
			`INSERT INTO tasks_new (id, plan_id, title, prompt, harness, status, ao_session_id, branch, pr_url, created_at)
			     SELECT id, plan_id, title, prompt, harness, status, ao_session_id, branch, pr_url, created_at FROM tasks`,
			`DROP TABLE tasks`,
			`ALTER TABLE tasks_new RENAME TO tasks`,
			`CREATE INDEX IF NOT EXISTS idx_tasks_plan ON tasks(plan_id)`,
		)
	}
	if !depsOK {
		stmts = append(stmts,
			`CREATE TABLE task_dependencies_new (
			    plan_id            TEXT NOT NULL,
			    task_id            TEXT NOT NULL,
			    depends_on_task_id TEXT NOT NULL,
			    PRIMARY KEY (plan_id, task_id, depends_on_task_id),
			    CHECK (task_id <> depends_on_task_id),
			    FOREIGN KEY (task_id, plan_id) REFERENCES tasks(id, plan_id),
			    FOREIGN KEY (depends_on_task_id, plan_id) REFERENCES tasks(id, plan_id)
			)`,
			`INSERT INTO task_dependencies_new (plan_id, task_id, depends_on_task_id)
			     SELECT plan_id, task_id, depends_on_task_id FROM task_dependencies`,
			`DROP TABLE task_dependencies`,
			`ALTER TABLE task_dependencies_new RENAME TO task_dependencies`,
			`CREATE INDEX IF NOT EXISTS idx_deps_plan ON task_dependencies(plan_id)`,
		)
	}
	stmts = append(stmts, `COMMIT`, `PRAGMA foreign_keys=ON`)
	for _, q := range stmts {
		if _, execErr := db.Exec(q); execErr != nil {
			db.Exec(`ROLLBACK`)
			db.Exec(`PRAGMA foreign_keys=ON`)
			return fmt.Errorf("migrate tasks pk: %w", execErr)
		}
	}
	return nil
}

// Close closes the underlying database.
func (s *Store) Close() error {
	return s.db.Close()
}

// now returns the current time serialized for storage.
func now() string {
	return time.Now().UTC().Format(timeFormat)
}

// inTx runs fn inside a single transaction, committing on nil error.
func (s *Store) inTx(fn func(tx *sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func parseTime(v string) (time.Time, error) {
	return time.Parse(timeFormat, v)
}

func parseNullTime(v sql.NullString) (*time.Time, error) {
	if !v.Valid {
		return nil, nil
	}
	t, err := parseTime(v.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func nullStr(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	s := v.String
	return &s
}
