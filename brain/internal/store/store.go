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
	"fmt"
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
	return &Store{db: db}, nil
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
