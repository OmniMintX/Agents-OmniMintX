package scheduler

import (
	"context"
	"sync"

	"github.com/OmniMintX/overmind/internal/gitops"
)

// fakeGit is an in-memory LocalMerger. It shares fakeAO's event log so
// tests can assert create/merge ordering across both fakes.
type fakeGit struct {
	mu        sync.Mutex
	ao        *fakeAO         // shared log sink
	merged    map[string]bool // branch -> merged into default
	conflicts map[string]bool // branch -> Merge answers Conflict
	blocked   int             // next N Merge calls answer Blocked
	hasOrigin bool            // HasRemoteBranch answer (precheck)
	defBranch string          // DefaultBranch answer (default "main")
}

func newFakeGit(ao *fakeAO) *fakeGit {
	return &fakeGit{ao: ao, merged: map[string]bool{}, conflicts: map[string]bool{}, defBranch: "main"}
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
