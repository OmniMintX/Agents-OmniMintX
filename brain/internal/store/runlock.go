package store

import (
	"errors"
	"fmt"
	"syscall"
	"time"
)

// Advisory run lock: prevents two `om run` processes on the same plan.
// A lock is stale (and can be stolen) when its heartbeat is older than
// staleAfter OR its holder process is no longer alive (kill -9 leaves the
// row behind; without the liveness check a dead holder blocks resume for
// the whole staleAfter window).

// pidAlive reports whether a process with the given pid exists. Variable
// so tests can force both branches deterministically. EPERM means the
// process exists but belongs to another user — treat as alive.
var pidAlive = func(pid int64) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(int(pid), 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// AcquireRunLock claims the plan's run lock for pid. It succeeds when the
// lock is free, stale, or held by a dead process; otherwise it returns an
// error naming the holder. tookOver=true means the lock was stolen from a
// holder whose process is dead (caller should log a warning).
func (s *Store) AcquireRunLock(planID string, pid int64, staleAfter time.Duration) (tookOver bool, err error) {
	cutoff := time.Now().UTC().Add(-staleAfter).Format(timeFormat)
	res, err := s.db.Exec(
		`UPDATE plans SET run_lock_pid = ?, run_lock_heartbeat_at = ?
		 WHERE id = ? AND (run_lock_pid IS NULL OR run_lock_pid = ? OR run_lock_heartbeat_at < ?)`,
		pid, now(), planID, pid, cutoff)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 {
		p, gerr := s.GetPlan(planID)
		if gerr != nil {
			return false, fmt.Errorf("run lock busy on plan %s", planID)
		}
		holder := int64(0)
		if p.RunLockPID != nil {
			holder = *p.RunLockPID
		}
		if holder != 0 && !pidAlive(holder) {
			// Holder is dead: steal the lock (CAS on the holder pid so two
			// concurrent stealers cannot both win).
			res, err := s.db.Exec(
				`UPDATE plans SET run_lock_pid = ?, run_lock_heartbeat_at = ?
				 WHERE id = ? AND run_lock_pid = ?`,
				pid, now(), planID, holder)
			if err != nil {
				return false, err
			}
			if sn, err := res.RowsAffected(); err == nil && sn == 1 {
				return true, nil
			}
		}
		return false, fmt.Errorf("plan %s is locked by another om run (pid %d)", planID, holder)
	}
	return false, nil
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
