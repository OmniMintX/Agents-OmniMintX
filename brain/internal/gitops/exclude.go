package gitops

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// markerExcludeLine is the exclude pattern covering the per-task
// ".om-done.<hex8>" completion markers.
const markerExcludeLine = ".om-done.*"

// EnsureExcluded appends pattern to the repo's <commondir>/info/exclude so
// untracked marker files stay invisible to `git status` in every linked
// worktree (the planner's tier-0 checks often assert a clean tree).
// Idempotent: an existing line is left untouched. Bare repos are a no-op
// (they have no worktrees to check).
func (Merger) EnsureExcluded(ctx context.Context, repo, pattern string) error {
	bare, err := run(ctx, repo, "rev-parse", "--is-bare-repository")
	if err != nil {
		return fmt.Errorf("bare-repo check for %s: %w", repo, err)
	}
	if bare == "true" {
		return nil
	}
	common, err := run(ctx, repo, "rev-parse", "--git-common-dir")
	if err != nil {
		return fmt.Errorf("resolve git common dir of %s: %w", repo, err)
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(repo, common)
	}
	infoDir := filepath.Join(common, "info")
	if err := os.MkdirAll(infoDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", infoDir, err)
	}
	path := filepath.Join(infoDir, "exclude")
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	content := string(data)
	// Exact line match, not Contains: "foo.om-done.*bar" must not count.
	for _, line := range strings.Split(content, "\n") {
		if strings.TrimSpace(line) == pattern {
			return nil
		}
	}
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	content += pattern + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
