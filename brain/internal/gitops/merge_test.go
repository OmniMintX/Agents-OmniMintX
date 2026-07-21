package gitops

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var ctx = context.Background()

// git runs a git command in dir, failing the test on error.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newRepo creates a repo with main checked out and one initial commit.
func newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-b", "main")
	writeFile(t, dir, "README.md", "init\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-m", "init")
	return dir
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// addBranch creates branch from main with one commit touching name.
func addBranch(t *testing.T, repo, branch, name, content string) {
	t.Helper()
	git(t, repo, "branch", branch, "main")
	wt := filepath.Join(t.TempDir(), branch)
	git(t, repo, "worktree", "add", wt, branch)
	writeFile(t, wt, name, content)
	git(t, wt, "add", "-A")
	git(t, wt, "commit", "-m", "work on "+branch)
	git(t, repo, "worktree", "remove", "--force", wt)
}

func TestMergeInPlaceClean(t *testing.T) {
	repo := newRepo(t)
	addBranch(t, repo, "feat-a", "a.txt", "A\n")
	res, err := Merger{}.Merge(ctx, repo, "feat-a", "main", "merge feat-a")
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if !res.Merged || res.Blocked != "" || res.Conflict != "" {
		t.Fatalf("want merged, got %+v", res)
	}
	if out := git(t, repo, "show", "main:a.txt"); out != "A" {
		t.Fatalf("main:a.txt = %q", out)
	}
}

func TestMergeIdempotentSkip(t *testing.T) {
	repo := newRepo(t)
	addBranch(t, repo, "feat-a", "a.txt", "A\n")
	if _, err := (Merger{}).Merge(ctx, repo, "feat-a", "main", "m"); err != nil {
		t.Fatal(err)
	}
	res, err := Merger{}.Merge(ctx, repo, "feat-a", "main", "m")
	if err != nil {
		t.Fatalf("second Merge: %v", err)
	}
	if res.Merged || res.SHA == "" {
		t.Fatalf("second merge must be a no-op with SHA, got %+v", res)
	}
	ok, err := Merger{}.IsMerged(ctx, repo, "feat-a", "main")
	if err != nil || !ok {
		t.Fatalf("IsMerged = %v, %v; want true", ok, err)
	}
}

func TestMergeFanOutNonFF(t *testing.T) {
	repo := newRepo(t)
	addBranch(t, repo, "feat-b", "b.txt", "B\n")
	addBranch(t, repo, "feat-c", "c.txt", "C\n")
	if _, err := (Merger{}).Merge(ctx, repo, "feat-b", "main", "m b"); err != nil {
		t.Fatal(err)
	}
	res, err := Merger{}.Merge(ctx, repo, "feat-c", "main", "m c")
	if err != nil || !res.Merged {
		t.Fatalf("non-ff merge after divergence: %+v, %v", res, err)
	}
	if git(t, repo, "show", "main:b.txt") != "B" || git(t, repo, "show", "main:c.txt") != "C" {
		t.Fatal("main must contain both fan-out branches")
	}
}

func TestMergeConflict(t *testing.T) {
	repo := newRepo(t)
	addBranch(t, repo, "feat-b", "same.txt", "B\n")
	addBranch(t, repo, "feat-c", "same.txt", "C\n")
	if _, err := (Merger{}).Merge(ctx, repo, "feat-b", "main", "m b"); err != nil {
		t.Fatal(err)
	}
	res, err := Merger{}.Merge(ctx, repo, "feat-c", "main", "m c")
	if err != nil {
		t.Fatalf("conflict must not be an error: %v", err)
	}
	if res.Conflict == "" {
		t.Fatalf("want conflict, got %+v", res)
	}
	if git(t, repo, "status", "--porcelain") != "" {
		t.Fatal("conflicted merge must be aborted, tree left clean")
	}
}

func TestMergeBlockedDirtyCheckout(t *testing.T) {
	repo := newRepo(t)
	addBranch(t, repo, "feat-a", "a.txt", "A\n")
	writeFile(t, repo, "README.md", "dirty edit\n")
	res, err := Merger{}.Merge(ctx, repo, "feat-a", "main", "m")
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if res.Blocked == "" || res.Merged {
		t.Fatalf("want blocked on dirty checkout, got %+v", res)
	}
}

func TestMergeTargetNotCheckedOut(t *testing.T) {
	repo := newRepo(t)
	addBranch(t, repo, "feat-a", "a.txt", "A\n")
	git(t, repo, "checkout", "-b", "elsewhere")
	res, err := Merger{}.Merge(ctx, repo, "feat-a", "main", "m")
	if err != nil || !res.Merged {
		t.Fatalf("temp-worktree merge: %+v, %v", res, err)
	}
	if git(t, repo, "show", "main:a.txt") != "A" {
		t.Fatal("main must contain a.txt after temp-worktree merge")
	}
	if out := git(t, repo, "worktree", "list"); strings.Contains(out, "om-merge-") {
		t.Fatalf("temp worktree not removed: %s", out)
	}
}

func TestMergeRecoversOwnMergeHead(t *testing.T) {
	repo := newRepo(t)
	addBranch(t, repo, "feat-b", "same.txt", "B\n")
	addBranch(t, repo, "feat-c", "same.txt", "C\n")
	if _, err := (Merger{}).Merge(ctx, repo, "feat-b", "main", "m b"); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash mid-merge of feat-c: MERGE_HEAD == tip(feat-c).
	cmd := exec.Command("git", "-C", repo, "merge", "--no-ff", "--no-edit", "refs/heads/feat-c")
	_ = cmd.Run() // conflicts; leaves MERGE_HEAD
	if git(t, repo, "rev-parse", "--verify", "MERGE_HEAD") == "" {
		t.Fatal("setup: MERGE_HEAD expected")
	}
	res, err := Merger{}.Merge(ctx, repo, "feat-c", "main", "m c")
	if err != nil {
		t.Fatalf("Merge after crash: %v", err)
	}
	// Our own MERGE_HEAD is aborted, then the merge re-runs -> real conflict.
	if res.Conflict == "" {
		t.Fatalf("want conflict after recovery, got %+v", res)
	}
}

func TestMergeBlockedForeignMergeHead(t *testing.T) {
	repo := newRepo(t)
	addBranch(t, repo, "feat-a", "a.txt", "A\n")
	addBranch(t, repo, "user-b", "same.txt", "B\n")
	addBranch(t, repo, "user-c", "same.txt", "C\n")
	git(t, repo, "merge", "--no-ff", "--no-edit", "refs/heads/user-b")
	cmd := exec.Command("git", "-C", repo, "merge", "--no-ff", "--no-edit", "refs/heads/user-c")
	_ = cmd.Run() // user's own conflicted merge in progress
	res, err := Merger{}.Merge(ctx, repo, "feat-a", "main", "m")
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if res.Blocked == "" {
		t.Fatalf("foreign MERGE_HEAD must block, got %+v", res)
	}
	if git(t, repo, "rev-parse", "--verify", "MERGE_HEAD") == "" {
		t.Fatal("user's merge must be left untouched")
	}
}

func TestMergeRecoversLeftoverTempWorktree(t *testing.T) {
	repo := newRepo(t)
	addBranch(t, repo, "feat-a", "a.txt", "A\n")
	git(t, repo, "checkout", "-b", "elsewhere")
	// Simulate a crashed previous merge: stale directory at the deterministic path.
	wt := tempWorktreePath(repo, "feat-a", "main")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := Merger{}.Merge(ctx, repo, "feat-a", "main", "m")
	if err != nil || !res.Merged {
		t.Fatalf("merge with leftover temp dir: %+v, %v", res, err)
	}
}

// TestMergeConcurrentSameRepo: two om processes merging different branches
// into the same repo (two plans on one repo) must serialize on the per-repo
// lock and BOTH succeed — not fail hard on git's index.lock.
func TestMergeConcurrentSameRepo(t *testing.T) {
	repo := newRepo(t)
	addBranch(t, repo, "feat-b", "b.txt", "B\n")
	addBranch(t, repo, "feat-c", "c.txt", "C\n")
	var wg sync.WaitGroup
	results := make([]MergeResult, 2)
	errs := make([]error, 2)
	for i, br := range []string{"feat-b", "feat-c"} {
		wg.Add(1)
		go func(i int, br string) {
			defer wg.Done()
			results[i], errs[i] = Merger{}.Merge(ctx, repo, br, "main", "m "+br)
		}(i, br)
	}
	wg.Wait()
	for i := range results {
		if errs[i] != nil || !results[i].Merged || results[i].Blocked != "" || results[i].Conflict != "" {
			t.Fatalf("concurrent merge %d: %+v, %v", i, results[i], errs[i])
		}
	}
	if git(t, repo, "show", "main:b.txt") != "B" || git(t, repo, "show", "main:c.txt") != "C" {
		t.Fatal("main must contain both concurrently merged branches")
	}
}

// TestMergeBlockedWhileRepoLockHeld: a foreign holder keeping the repo lock
// past the wait window must surface as transient Blocked, never an error.
func TestMergeBlockedWhileRepoLockHeld(t *testing.T) {
	repo := newRepo(t)
	addBranch(t, repo, "feat-a", "a.txt", "A\n")
	orig := repoLockWait
	repoLockWait = 50 * time.Millisecond
	t.Cleanup(func() { repoLockWait = orig })
	held, err := acquireRepoLock(ctx, repo)
	if err != nil {
		t.Fatalf("seed lock: %v", err)
	}
	defer releaseRepoLock(held)
	res, err := Merger{}.Merge(ctx, repo, "feat-a", "main", "m")
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if res.Blocked == "" || res.Merged {
		t.Fatalf("want blocked while repo lock held, got %+v", res)
	}
}

// TestAbortOwnMergeGuard: the post-failure abort must only kill a merge
// whose MERGE_HEAD == tip of OUR branch; a merge started by someone else in
// the failure->abort window (TOCTOU) is left untouched.
func TestAbortOwnMergeGuard(t *testing.T) {
	repo := newRepo(t)
	addBranch(t, repo, "user-b", "same.txt", "B\n")
	addBranch(t, repo, "user-c", "same.txt", "C\n")
	git(t, repo, "merge", "--no-ff", "--no-edit", "refs/heads/user-b")
	cmd := exec.Command("git", "-C", repo, "merge", "--no-ff", "--no-edit", "refs/heads/user-c")
	_ = cmd.Run() // conflicts; leaves MERGE_HEAD == tip(user-c)

	abortOwnMerge(ctx, repo, "user-b") // foreign relative to user-b
	if git(t, repo, "rev-parse", "--verify", "MERGE_HEAD") == "" {
		t.Fatal("foreign MERGE_HEAD must survive abortOwnMerge")
	}

	abortOwnMerge(ctx, repo, "user-c") // ours: aborted
	check := exec.Command("git", "-C", repo, "rev-parse", "--verify", "--quiet", "MERGE_HEAD")
	if check.Run() == nil {
		t.Fatal("own MERGE_HEAD must be aborted")
	}
	if git(t, repo, "status", "--porcelain") != "" {
		t.Fatal("tree must be clean after aborting our own merge")
	}
}

// TestHasDiffThreeDot: a branch with a commit has a diff vs main; after a
// sibling merges into main the check must still be three-dot (merge-base),
// and an empty branch has no diff.
func TestHasDiffThreeDot(t *testing.T) {
	repo := newRepo(t)
	addBranch(t, repo, "feat-a", "a.txt", "A\n")
	git(t, repo, "branch", "empty-b", "main")
	has, err := Merger{}.HasDiff(ctx, repo, "feat-a", "main")
	if err != nil || !has {
		t.Fatalf("HasDiff(feat-a) = %v, %v; want true", has, err)
	}
	has, err = Merger{}.HasDiff(ctx, repo, "empty-b", "main")
	if err != nil || has {
		t.Fatalf("HasDiff(empty-b) = %v, %v; want false", has, err)
	}
	// A sibling advancing main must not make empty-b look diffed.
	if _, err := (Merger{}).Merge(ctx, repo, "feat-a", "main", "m"); err != nil {
		t.Fatal(err)
	}
	has, err = Merger{}.HasDiff(ctx, repo, "empty-b", "main")
	if err != nil || has {
		t.Fatalf("HasDiff(empty-b) after sibling merge = %v, %v; want false", has, err)
	}
}

// TestDiffTextThreeDot: DiffText returns the branch's own changes vs the
// merge-base (same three-dot range as HasDiff — sibling merges into main
// never leak in), and truncates at maxBytes with a notice.
func TestDiffTextThreeDot(t *testing.T) {
	repo := newRepo(t)
	addBranch(t, repo, "feat-a", "a.txt", "A\n")
	addBranch(t, repo, "feat-b", "b.txt", "B\n")
	if _, err := (Merger{}).Merge(ctx, repo, "feat-a", "main", "m a"); err != nil {
		t.Fatal(err)
	}
	diff, err := Merger{}.DiffText(ctx, repo, "main", "feat-b", 0)
	if err != nil {
		t.Fatalf("DiffText: %v", err)
	}
	if !strings.Contains(diff, "b.txt") || !strings.Contains(diff, "+B") {
		t.Fatalf("diff must show feat-b's change:\n%s", diff)
	}
	if strings.Contains(diff, "a.txt") {
		t.Fatalf("sibling merge must not leak into the diff:\n%s", diff)
	}
	cut, err := Merger{}.DiffText(ctx, repo, "main", "feat-b", 10)
	if err != nil {
		t.Fatalf("DiffText truncated: %v", err)
	}
	if !strings.HasSuffix(cut, "[diff truncated]") || len(cut) > 10+len("\n[diff truncated]") {
		t.Fatalf("want 10-byte cut + notice, got %d bytes: %q", len(cut), cut)
	}
}

// TestCommitWorktreeRescue: uncommitted files in a session worktree are
// staged and committed (marker files excluded); a clean worktree is a
// no-op; WorktreeFor finds the branch's worktree.
func TestCommitWorktreeRescue(t *testing.T) {
	repo := newRepo(t)
	git(t, repo, "branch", "sess-b", "main")
	wt := filepath.Join(t.TempDir(), "sess-b")
	git(t, repo, "worktree", "add", wt, "sess-b")
	t.Cleanup(func() { git(t, repo, "worktree", "remove", "--force", wt) })

	// git reports symlink-resolved paths (macOS: /var -> /private/var).
	wantWt, err := filepath.EvalSymlinks(wt)
	if err != nil {
		t.Fatal(err)
	}
	found, err := Merger{}.WorktreeFor(ctx, repo, "sess-b")
	if err != nil || found != wantWt {
		t.Fatalf("WorktreeFor = %q, %v; want %q", found, err, wantWt)
	}

	res, err := Merger{}.CommitWorktree(ctx, wt, "om: system-commit", []string{".om-done.*"})
	if err != nil || res.Committed {
		t.Fatalf("clean worktree must be a no-op, got %+v, %v", res, err)
	}

	writeFile(t, wt, "forgot.txt", "work\n")
	writeFile(t, wt, ".om-done.abcd1234", "ok: done\n")
	res, err = Merger{}.CommitWorktree(ctx, wt, "om: system-commit", []string{".om-done.*"})
	if err != nil || !res.Committed || res.SHA == "" {
		t.Fatalf("CommitWorktree = %+v, %v; want a commit", res, err)
	}
	if len(res.Files) != 1 || res.Files[0] != "forgot.txt" {
		t.Fatalf("committed files = %v, want [forgot.txt] (marker excluded)", res.Files)
	}
	if out := git(t, repo, "show", "sess-b:forgot.txt"); out != "work" {
		t.Fatalf("sess-b:forgot.txt = %q", out)
	}
	// The marker survives uncommitted; the worktree is otherwise clean.
	has, err := Merger{}.HasUncommitted(ctx, wt, []string{".om-done.*"})
	if err != nil || has {
		t.Fatalf("HasUncommitted after rescue = %v, %v; want false", has, err)
	}
	has, err = Merger{}.HasUncommitted(ctx, wt, nil)
	if err != nil || !has {
		t.Fatalf("marker file must still be uncommitted, got %v, %v", has, err)
	}
}

func TestDefaultBranchAndRemoteCheck(t *testing.T) {
	repo := newRepo(t)
	branch, err := Merger{}.DefaultBranch(ctx, repo)
	if err != nil || branch != "main" {
		t.Fatalf("DefaultBranch = %q, %v", branch, err)
	}
	has, err := Merger{}.HasRemoteBranch(ctx, repo, "main")
	if err != nil || has {
		t.Fatalf("HasRemoteBranch = %v, %v; want false", has, err)
	}
	git(t, repo, "update-ref", "refs/remotes/origin/main", git(t, repo, "rev-parse", "main"))
	has, err = Merger{}.HasRemoteBranch(ctx, repo, "main")
	if err != nil || !has {
		t.Fatalf("HasRemoteBranch after origin ref = %v, %v; want true", has, err)
	}
}
