// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

// subagentWorktreeDir is where the claude CLI adds the worktree an Agent
// call's isolation "worktree" runs its subagent in, relative to the tree
// (measured on the pinned CLI: <tree>/.claude/worktrees/agent-<id>, on a
// branch of its own). The argument cannot be denied the way a tool can.
const subagentWorktreeDir = ".claude/worktrees"

// removeSubagentWorktrees removes every linked worktree of the clone at
// repo that lies under subagentWorktreeDir, and returns their paths
// relative to repo. A build granted subagents calls it once its runs are
// over and before its report is read: left there, such a worktree is an
// untracked nested repository in the tree, which fails commit.sh's
// clean-tree check, and its work is not on the branch the changeset is
// built from. A worktree anywhere else is not the CLI's and is left alone,
// and so is the branch each removed one had checked out, which never
// leaves the pod. Removal is forced (twice, so a locked worktree goes too):
// what is left in one is a subagent's, and work left only there is not part
// of the change, as the build prompt says.
func removeSubagentWorktrees(ctx context.Context, repo string) ([]string, error) {
	real := repo
	if r, err := filepath.EvalSymlinks(repo); err == nil {
		real = r
	}
	root := filepath.Join(real, filepath.FromSlash(subagentWorktreeDir)) + string(filepath.Separator)
	list, err := git(ctx, repo, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("listing the clone's worktrees: %w", err)
	}
	var removed []string
	for line := range strings.SplitSeq(list, "\n") {
		path, ok := strings.CutPrefix(line, "worktree ")
		if !ok || !strings.HasPrefix(filepath.Clean(path), root) {
			continue
		}
		if _, err := git(ctx, repo, "worktree", "remove", "--force", "--force", path); err != nil {
			return removed, fmt.Errorf("removing the worktree at %s: %w", path, err)
		}
		rel, err := filepath.Rel(real, path)
		if err != nil {
			rel = path
		}
		removed = append(removed, filepath.ToSlash(rel))
	}
	return removed, nil
}
