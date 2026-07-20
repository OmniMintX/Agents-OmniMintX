package store

import (
	"fmt"
	"time"
)

// Advisory run lock: prevents two `om run` processes on the same plan.
// A lock is stale (and can be stolen) when its heartbeat is older than
// staleAfter; the holder must call HeartbeatRunLock periodically.

// AcquireRunLock claims the plan's run lock for pid. It succeeds when the
// lock is free or stale; otherwise it returns an error naming the holder.
func (s *Store) AcquireRunLock(planID string, pid int64, staleAfter time.Duration) error {
	cutoff := time.Now().UTC().Add(-staleAfter).Format(timeFormat)
	res, err := s.db.Exec(
		`UPDATE plans SET run_lock_pid = ?, run_lock_heartbeat_at = ?
		 WHERE id = ? AND (run_lock_pid IS NULL OR run_lock_pid = ? OR run_lock_heartbeat_at < ?)`,
		pid, now(), planID, pid, cutoff)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		p, gerr := s.GetPlan(planID)
		if gerr != nil {
			return fmt.Errorf("run lock busy on plan %s", planID)
		}
		holder := int64(0)
		if p.RunLockPID != nil {
			holder = *p.RunLockPID
		}
		return fmt.Errorf("plan %s is locked by another om run (pid %d)", planID, holder)
	}
	return nil
}

// HeartbeatRunLock refreshes the heartbeat; fails if pid no longer holds it.
func (s *Store) HeartbeatRunLock(planID string, pid int64) error {
	res, err := s.db.Exec(
		`UPDATE plans SET run_lock_heartbeat_at = ? WHERE id = ? AND run_lock_pid = ?`,
		now(), planID, pid)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("plan %s: run lock not held by pid %d", planID, pid)
	}
	return nil
}

// ReleaseRunLock clears the lock if held by pid (no-op otherwise).
func (s *Store) ReleaseRunLock(planID string, pid int64) error {
	_, err := s.db.Exec(
		`UPDATE plans SET run_lock_pid = NULL, run_lock_heartbeat_at = NULL
		 WHERE id = ? AND run_lock_pid = ?`, planID, pid)
	return err
}
