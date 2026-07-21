// Package gitops performs local git operations for Overmind's chaining:
// AO 0.10.x workers never open PRs (the daemon's PR merge endpoint is a
// stub), so the scheduler merges each finished task's session branch into
// the repo's default branch itself, directly on the local filesystem.
// Git ancestry is the source of truth for "merged-ness"; events are audit.
package gitops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// MergeResult is the outcome of one Merge call. Exactly one of the three
// shapes holds: success (SHA set), Blocked (transient, retry next tick),
// or Conflict (deterministic failure, does not self-heal).
type MergeResult struct {
	SHA      string // tip of target after the call
	Merged   bool   // a new merge commit was created (false: already ancestor)
	Blocked  string // non-empty: dirty checkout / foreign merge in progress
	Conflict string // non-empty: real merge conflict
}

// CommitResult is the outcome of one CommitWorktree call. Committed=false
// means the worktree was already clean (idempotent no-op).
type CommitResult struct {
	SHA       string   // commit created (empty when nothing to commit)
	Files     []string // paths staged into the commit
	Committed bool
}

// Merger shells out to the git CLI. The zero value is ready to use.
type Merger struct{}

// run executes git -C dir args... and returns trimmed combined output.
func run(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil {
		return s, fmt.Errorf("git %s: %w (%s)", strings.Join(args, " "), err, s)
	}
	return s, nil
}

// exitOne reports whether err is an exec.ExitError with status 1 (git's
// "no" answer for boolean queries, as opposed to a real failure).
func exitOne(err error) bool {
	var ee *exec.ExitError
	return errors.As(err, &ee) && ee.ExitCode() == 1
}

// IsMerged reports whether branch is already an ancestor of target.
func (Merger) IsMerged(ctx context.Context, repo, branch, target string) (bool, error) {
	if _, err := run(ctx, repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err != nil {
		return false, fmt.Errorf("branch %q not found in %s: %w", branch, repo, err)
	}
	cmd := exec.CommandContext(ctx, "git", "-C", repo, "merge-base", "--is-ancestor",
		"refs/heads/"+branch, "refs/heads/"+target)
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	if exitOne(err) {
		return false, nil
	}
	return false, fmt.Errorf("git merge-base --is-ancestor %s %s: %w", branch, target, err)
}

// DefaultBranch resolves the repo's default branch from the primary
// checkout's HEAD; detached HEAD falls back to "main" (AO's own last
// base-ref candidate).
func (Merger) DefaultBranch(ctx context.Context, repo string) (string, error) {
	out, err := run(ctx, repo, "symbolic-ref", "--short", "HEAD")
	if err == nil {
		return out, nil
	}
	if _, gerr := run(ctx, repo, "rev-parse", "--git-dir"); gerr != nil {
		return "", fmt.Errorf("%s is not a git repository: %w", repo, gerr)
	}
	return "main", nil
}

// WorktreeFor returns the directory where branch is checked out ("" when
// none) — for AO sessions this is the session worktree the worker ran in.
func (Merger) WorktreeFor(ctx context.Context, repo, branch string) (string, error) {
	return checkoutOf(ctx, repo, branch)
}

// HasDiff reports whether branch carries committed changes vs base
// (three-dot: diff from merge-base to branch, so sibling merges into base
// never mask an empty branch).
func (Merger) HasDiff(ctx context.Context, repo, branch, base string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", repo, "diff", "--quiet",
		"refs/heads/"+base+"...refs/heads/"+branch)
	err := cmd.Run()
	if err == nil {
		return false, nil
	}
	if exitOne(err) {
		return true, nil
	}
	return false, fmt.Errorf("git diff %s...%s: %w", base, branch, err)
}

// DiffText returns the textual diff of branch vs base (three-dot: from the
// merge-base to branch, same range as HasDiff) for the tier-1 LLM verifier.
// Output longer than maxBytes is cut at maxBytes with a truncation notice
// appended (maxBytes <= 0 means unlimited).
func (Merger) DiffText(ctx context.Context, repo, base, branch string, maxBytes int) (string, error) {
	out, err := run(ctx, repo, "diff", "refs/heads/"+base+"...refs/heads/"+branch)
	if err != nil {
		return "", fmt.Errorf("git diff %s...%s: %w", base, branch, err)
	}
	if maxBytes > 0 && len(out) > maxBytes {
		out = out[:maxBytes] + "\n[diff truncated]"
	}
	return out, nil
}

// HasUncommitted reports whether dir has uncommitted changes (tracked or
// untracked), ignoring the exclude pathspecs (e.g. the .om-done.* markers).
func (Merger) HasUncommitted(ctx context.Context, dir string, exclude []string) (bool, error) {
	out, err := run(ctx, dir, append([]string{"status", "--porcelain"}, excludePathspec(exclude)...)...)
	if err != nil {
		return false, err
	}
	return out != "", nil
}

// CommitWorktree stages and commits everything left uncommitted in dir
// (staging is naturally limited to that worktree: linked worktrees have
// their own index), skipping the exclude pathspecs. A clean worktree is an
// idempotent no-op. Used by the scheduler's system-commit step to rescue
// work the AO worker forgot to commit before the merge.
func (Merger) CommitWorktree(ctx context.Context, dir, msg string, exclude []string) (CommitResult, error) {
	if _, err := run(ctx, dir, append([]string{"add", "-A"}, excludePathspec(exclude)...)...); err != nil {
		return CommitResult{}, err
	}
	staged, err := run(ctx, dir, "diff", "--cached", "--name-only")
	if err != nil {
		return CommitResult{}, err
	}
	if staged == "" {
		return CommitResult{}, nil
	}
	// Explicit identity: system commits must never fail on a repo without
	// user.name/user.email, and the author marks them as Overmind's.
	if _, err := run(ctx, dir, "-c", "user.name=overmind", "-c", "user.email=overmind@localhost",
		"commit", "-m", msg); err != nil {
		return CommitResult{}, err
	}
	sha, err := run(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return CommitResult{}, err
	}
	return CommitResult{SHA: sha, Files: strings.Split(staged, "\n"), Committed: true}, nil
}

// excludePathspec renders exclude patterns as git pathspecs rooted at ".".
func excludePathspec(exclude []string) []string {
	specs := []string{"--", "."}
	for _, e := range exclude {
		specs = append(specs, ":(exclude)"+e)
	}
	return specs
}

// HasRemoteBranch reports whether refs/remotes/origin/<branch> exists.
// Phase 1 fails fast on such repos: AO bases new worktrees on
// origin/<default> first, which local merges can never advance.
func (Merger) HasRemoteBranch(ctx context.Context, repo, branch string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", repo, "show-ref", "--verify", "--quiet",
		"refs/remotes/origin/"+branch)
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return false, nil
	}
	return false, fmt.Errorf("git show-ref origin/%s: %w", branch, err)
}

// Merge merges branch into target with a real 3-way merge (--no-ff).
// Already-ancestor is an idempotent no-op. If target is checked out in
// some worktree the merge runs in place (clean tree required, else
// Blocked); otherwise a deterministic temp worktree is used and removed
// afterwards (leftovers from a crash are recovered first). The whole call
// holds the per-repo lock so concurrent om processes serialize instead of
// racing git's index.lock (lock busy too long -> Blocked, retried).
func (m Merger) Merge(ctx context.Context, repo, branch, target, msg string) (MergeResult, error) {
	lock, err := acquireRepoLock(ctx, repo)
	if errors.Is(err, errRepoLockBusy) {
		return MergeResult{Blocked: fmt.Sprintf("repo %s is locked by another om merge", repo)}, nil
	}
	if err != nil {
		return MergeResult{}, err
	}
	defer releaseRepoLock(lock)
	merged, err := m.IsMerged(ctx, repo, branch, target)
	if err != nil {
		return MergeResult{}, err
	}
	if merged {
		sha, err := run(ctx, repo, "rev-parse", "refs/heads/"+target)
		return MergeResult{SHA: sha}, err
	}
	dir, err := checkoutOf(ctx, repo, target)
	if err != nil {
		return MergeResult{}, err
	}
	if dir != "" {
		return mergeAt(ctx, dir, branch, target, msg, false)
	}
	wt := tempWorktreePath(repo, branch, target)
	if _, err := os.Stat(wt); err == nil {
		// Leftover from a crashed previous merge: remove and start over.
		_, _ = run(ctx, repo, "worktree", "remove", "--force", wt)
		_ = os.RemoveAll(wt)
		_, _ = run(ctx, repo, "worktree", "prune")
	}
	if _, err := run(ctx, repo, "worktree", "add", wt, target); err != nil {
		return MergeResult{}, err
	}
	defer func() {
		_, _ = run(context.Background(), repo, "worktree", "remove", "--force", wt)
	}()
	return mergeAt(ctx, wt, branch, target, msg, true)
}

// mergeAt runs the actual merge inside dir, which has target checked out.
// temp worktrees skip the dirty check (freshly created, always clean).
func mergeAt(ctx context.Context, dir, branch, target, msg string, temp bool) (MergeResult, error) {
	if !temp {
		if blocked, err := recoverMergeHead(ctx, dir, branch); err != nil {
			return MergeResult{}, err
		} else if blocked != "" {
			return MergeResult{Blocked: blocked}, nil
		}
		status, err := run(ctx, dir, "status", "--porcelain")
		if err != nil {
			return MergeResult{}, err
		}
		if status != "" {
			return MergeResult{Blocked: fmt.Sprintf("checkout of %s at %s has uncommitted changes", target, dir)}, nil
		}
	}
	out, err := run(ctx, dir, "merge", "--no-ff", "--no-edit", "-m", msg, "refs/heads/"+branch)
	if err != nil {
		abortOwnMerge(ctx, dir, branch)
		if strings.Contains(strings.ToLower(out), "conflict") {
			return MergeResult{Conflict: out}, nil
		}
		return MergeResult{}, err
	}
	sha, err := run(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return MergeResult{}, err
	}
	return MergeResult{SHA: sha, Merged: true}, nil
}

// abortOwnMerge aborts an in-progress merge only when MERGE_HEAD proves it
// is OURS (== tip of branch): between our failed merge and the abort some
// other actor may have started a merge of their own (TOCTOU), and that one
// must never be destroyed. No MERGE_HEAD means our merge failed before
// starting — nothing to abort.
func abortOwnMerge(ctx context.Context, dir, branch string) {
	mh, err := run(ctx, dir, "rev-parse", "--verify", "--quiet", "MERGE_HEAD")
	if err != nil {
		return
	}
	tip, err := run(ctx, dir, "rev-parse", "refs/heads/"+branch)
	if err != nil || mh != tip {
		return
	}
	_, _ = run(ctx, dir, "merge", "--abort")
}

// recoverMergeHead handles a MERGE_HEAD left in dir by a crash: if it is
// OUR merge (MERGE_HEAD == tip of branch) abort it and continue; anything
// else belongs to the user -> blocked, never touched.
func recoverMergeHead(ctx context.Context, dir, branch string) (blocked string, err error) {
	mh, err := run(ctx, dir, "rev-parse", "--verify", "--quiet", "MERGE_HEAD")
	if err != nil {
		return "", nil // no merge in progress
	}
	tip, err := run(ctx, dir, "rev-parse", "refs/heads/"+branch)
	if err != nil {
		return "", err
	}
	if mh != tip {
		return fmt.Sprintf("a foreign merge is in progress at %s (MERGE_HEAD %s)", dir, mh[:12]), nil
	}
	if _, err := run(ctx, dir, "merge", "--abort"); err != nil {
		return "", err
	}
	return "", nil
}

// checkoutOf returns the worktree directory where target is currently
// checked out ("" when none). Overmind's own temp worktrees are ignored
// (a crashed leftover must not force an in-place merge path).
func checkoutOf(ctx context.Context, repo, target string) (string, error) {
	out, err := run(ctx, repo, "worktree", "list", "--porcelain")
	if err != nil {
		return "", err
	}
	var dir string
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			dir = strings.TrimPrefix(line, "worktree ")
		case line == "branch refs/heads/"+target:
			if strings.HasPrefix(filepath.Base(dir), "om-merge-") {
				continue
			}
			return dir, nil
		}
	}
	return "", nil
}

// tempWorktreePath is deterministic per (repo, target) so a crashed merge
// leaves a recoverable, predictable path (R11).
func tempWorktreePath(repo, _ /*branch*/, target string) string {
	return filepath.Join(os.TempDir(), "om-merge-"+shortHash(repo+"\x00"+target))
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:4])
}
