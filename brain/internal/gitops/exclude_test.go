package gitops

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// excludePath is the primary repo's info/exclude file (common dir == .git).
func excludePath(repo string) string {
	return filepath.Join(repo, ".git", "info", "exclude")
}

func TestEnsureExcludedCreatesFile(t *testing.T) {
	repo := newRepo(t)
	if err := (Merger{}).EnsureExcluded(ctx, repo, markerExcludeLine); err != nil {
		t.Fatalf("EnsureExcluded: %v", err)
	}
	data, err := os.ReadFile(excludePath(repo))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), markerExcludeLine) {
		t.Fatalf("exclude file must contain %q, got %q", markerExcludeLine, data)
	}
	writeFile(t, repo, ".om-done.abc12345", "ok: done\n")
	if out := git(t, repo, "status", "--porcelain"); out != "" {
		t.Fatalf("marker must be invisible to git status, got %q", out)
	}
}

func TestEnsureExcludedIdempotent(t *testing.T) {
	repo := newRepo(t)
	for i := 0; i < 2; i++ {
		if err := (Merger{}).EnsureExcluded(ctx, repo, markerExcludeLine); err != nil {
			t.Fatalf("EnsureExcluded call %d: %v", i+1, err)
		}
	}
	data, err := os.ReadFile(excludePath(repo))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(data), markerExcludeLine); n != 1 {
		t.Fatalf("pattern must appear once, got %d in %q", n, data)
	}
}

func TestEnsureExcludedPreservesExisting(t *testing.T) {
	repo := newRepo(t)
	if err := os.MkdirAll(filepath.Dir(excludePath(repo)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(excludePath(repo), []byte("foo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := (Merger{}).EnsureExcluded(ctx, repo, markerExcludeLine); err != nil {
		t.Fatalf("EnsureExcluded: %v", err)
	}
	data, err := os.ReadFile(excludePath(repo))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "foo\n"+markerExcludeLine+"\n" {
		t.Fatalf("existing content must be preserved, got %q", data)
	}
}

func TestEnsureExcludedExistingNoTrailingNewline(t *testing.T) {
	repo := newRepo(t)
	if err := os.MkdirAll(filepath.Dir(excludePath(repo)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(excludePath(repo), []byte("foo"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := (Merger{}).EnsureExcluded(ctx, repo, markerExcludeLine); err != nil {
		t.Fatalf("EnsureExcluded: %v", err)
	}
	data, err := os.ReadFile(excludePath(repo))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "foo\n"+markerExcludeLine+"\n" {
		t.Fatalf("pattern must land on its own line, got %q", data)
	}
}

func TestEnsureExcludedBareRepo(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "--bare")
	if err := (Merger{}).EnsureExcluded(ctx, dir, markerExcludeLine); err != nil {
		t.Fatalf("EnsureExcluded on bare repo: %v", err)
	}
}

func TestEnsureExcludedLinkedWorktree(t *testing.T) {
	repo := newRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	git(t, repo, "worktree", "add", wt, "-b", "wt-branch")
	t.Cleanup(func() { git(t, repo, "worktree", "remove", "--force", wt) })
	if err := (Merger{}).EnsureExcluded(ctx, repo, markerExcludeLine); err != nil {
		t.Fatalf("EnsureExcluded: %v", err)
	}
	writeFile(t, wt, ".om-done.abc12345", "ok: done\n")
	if out := git(t, wt, "status", "--porcelain"); out != "" {
		t.Fatalf("marker must be invisible in linked worktree, got %q", out)
	}
}

// TestEnsureExcludedPlannerCheckRegression reproduces the E2E round-6 bug:
// planner tier-0 checks like `test -z "$(git status --porcelain)"` run via
// scheduler runCheck (sh -c inside the session worktree) while the protocol
// marker sits there untracked, so they failed every round. After
// EnsureExcluded the check must pass with only the marker present, must
// still fail on real uncommitted files, and the marker must survive both
// runs untouched (resolveTerminal reads it afterwards).
func TestEnsureExcludedPlannerCheckRegression(t *testing.T) {
	repo := newRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	git(t, repo, "worktree", "add", wt, "-b", "task-branch")
	t.Cleanup(func() { git(t, repo, "worktree", "remove", "--force", wt) })
	if err := (Merger{}).EnsureExcluded(ctx, repo, markerExcludeLine); err != nil {
		t.Fatalf("EnsureExcluded: %v", err)
	}
	const marker, markerContent = ".om-done.abc12345", "ok: all done\n"
	writeFile(t, wt, marker, markerContent)

	runCheck := func() error { // mirrors scheduler runCheck: sh -c in the worktree
		cmd := exec.Command("sh", "-c", `test -z "$(git status --porcelain)"`)
		cmd.Dir = wt
		return cmd.Run()
	}
	markerIntact := func(step string) {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(wt, marker))
		if err != nil {
			t.Fatalf("%s: marker gone: %v", step, err)
		}
		if string(data) != markerContent {
			t.Fatalf("%s: marker content changed, got %q", step, data)
		}
	}

	if err := runCheck(); err != nil {
		t.Fatalf("check must pass when only the uncommitted marker exists: %v", err)
	}
	markerIntact("after passing check")

	writeFile(t, wt, "junk.txt", "dirt\n")
	if err := runCheck(); err == nil {
		t.Fatal("check must still fail when real uncommitted files exist")
	}
	markerIntact("after failing check")
}
