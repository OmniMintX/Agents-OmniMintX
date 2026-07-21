package gitops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Per-repo advisory lock: the run lock is per-plan, so two om processes
// running different plans against the SAME repo would otherwise race git's
// own index.lock (which makes the loser fail hard instead of waiting).
// flock is used because the kernel releases it when the holder dies — no
// stale-lock recovery needed.

// errRepoLockBusy means another process held the repo lock for the whole
// wait window; Merge surfaces this as a transient Blocked result.
var errRepoLockBusy = errors.New("repo lock busy")

// repoLockWait bounds how long Merge waits for the repo lock before
// reporting Blocked (retried next scheduler tick). Var so tests shorten it.
var repoLockWait = 10 * time.Second

// repoLockPath is deterministic per repo; symlinks are resolved so
// different spellings of the same path converge on one lock file.
func repoLockPath(repo string) string {
	p := repo
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	return filepath.Join(os.TempDir(), "om-repo-"+shortHash(p)+".lock")
}

// acquireRepoLock takes the exclusive per-repo flock, polling non-blocking
// for up to repoLockWait. Release with releaseRepoLock.
func acquireRepoLock(ctx context.Context, repo string) (*os.File, error) {
	f, err := os.OpenFile(repoLockPath(repo), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open repo lock: %w", err)
	}
	deadline := time.Now().Add(repoLockWait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			f.Close()
			return nil, fmt.Errorf("flock %s: %w", f.Name(), err)
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, errRepoLockBusy
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func releaseRepoLock(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}
