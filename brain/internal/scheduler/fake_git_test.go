package scheduler

import (
	"context"
	"strings"
	"sync"

	"github.com/OmniMintX/overmind/internal/gitops"
)

// fakeGit is an in-memory LocalMerger. It shares fakeAO's event log so
// tests can assert create/merge ordering across both fakes. Tier-0
// defaults are the happy path: every branch has a worktree, a non-empty
// diff, and nothing uncommitted.
type fakeGit struct {
	mu          sync.Mutex
	ao          *fakeAO           // shared log sink
	merged      map[string]bool   // branch -> merged into default
	conflicts   map[string]bool   // branch -> Merge answers Conflict
	blocked     int               // next N Merge calls answer Blocked
	hasOrigin   bool              // HasRemoteBranch answer (precheck)
	defBranch   string            // DefaultBranch answer (default "main")
	emptyDiff   map[string]bool   // branch -> HasDiff answers false
	uncommitted map[string]bool   // branch -> worktree has uncommitted changes
	noWorktree  map[string]bool   // branch -> WorktreeFor answers ""
	diffText    map[string]string // branch -> DiffText answer (default "diff --git fake")
}

func newFakeGit(ao *fakeAO) *fakeGit {
	return &fakeGit{
		ao: ao, merged: map[string]bool{}, conflicts: map[string]bool{}, defBranch: "main",
		emptyDiff: map[string]bool{}, uncommitted: map[string]bool{}, noWorktree: map[string]bool{},
		diffText: map[string]string{},
	}
}

const fakeWorktreeRoot = "/fake/wt/"

func (g *fakeGit) WorktreeFor(_ context.Context, _, branch string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.noWorktree[branch] {
		return "", nil
	}
	return fakeWorktreeRoot + branch, nil
}

func (g *fakeGit) HasDiff(_ context.Context, _, branch, _ string) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return !g.emptyDiff[branch], nil
}

func (g *fakeGit) DiffText(_ context.Context, _, _, branch string, _ int) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if d, ok := g.diffText[branch]; ok {
		return d, nil
	}
	return "diff --git a/fake b/fake\n+work on " + branch + "\n", nil
}

func (g *fakeGit) HasUncommitted(_ context.Context, dir string, _ []string) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.uncommitted[strings.TrimPrefix(dir, fakeWorktreeRoot)], nil
}

func (g *fakeGit) CommitWorktree(_ context.Context, dir, _ string, _ []string) (gitops.CommitResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	branch := strings.TrimPrefix(dir, fakeWorktreeRoot)
	if !g.uncommitted[branch] {
		return gitops.CommitResult{}, nil
	}
	g.uncommitted[branch] = false
	g.emptyDiff[branch] = false // rescued work is now committed on the branch
	g.ao.appendLog("syscommit:" + branch)
	return gitops.CommitResult{SHA: "sha-" + branch + "-sys", Files: []string{"rescued.txt"}, Committed: true}, nil
}

func (g *fakeGit) IsMerged(_ context.Context, _, branch, _ string) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.merged[branch], nil
}

func (g *fakeGit) Merge(_ context.Context, _, branch, _, _ string) (gitops.MergeResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.merged[branch] {
		return gitops.MergeResult{SHA: "sha-" + branch}, nil
	}
	if g.conflicts[branch] {
		return gitops.MergeResult{Conflict: "CONFLICT (content): " + branch}, nil
	}
	if g.blocked > 0 {
		g.blocked--
		return gitops.MergeResult{Blocked: "checkout dirty (fake)"}, nil
	}
	g.merged[branch] = true
	g.ao.appendLog("merge:" + branch)
	return gitops.MergeResult{SHA: "sha-" + branch + "-merge", Merged: true}, nil
}

func (g *fakeGit) DefaultBranch(_ context.Context, _ string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.defBranch, nil
}

func (g *fakeGit) HasRemoteBranch(_ context.Context, _, _ string) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.hasOrigin, nil
}

func (g *fakeGit) EnsureExcluded(_ context.Context, _, _ string) error { return nil }
