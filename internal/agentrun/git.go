// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// git runs one git command in dir, returning trimmed stdout.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := gitRaw(ctx, dir, args...)
	return strings.TrimSpace(string(out)), err
}

// gitRaw runs one git command in dir, returning stdout byte-exact (blob
// contents must not be trimmed).
func gitRaw(ctx context.Context, dir string, args ...string) ([]byte, error) {
	return gitRawEnv(ctx, dir, nil, args...)
}

// gitRawEnv is gitRaw with env appended to the process's environment.
func gitRawEnv(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out.Bytes(), nil
}

// headSHA resolves the clone's current commit. Captured before the
// remediation branch is created, it is the pinned base the changeset is
// diffed against and the pushed commit's parent.
func headSHA(ctx context.Context, dir string) (string, error) {
	return git(ctx, dir, "rev-parse", "HEAD")
}

// ensureIdentity configures a commit identity when the clone has none, so
// commit.sh can commit.
func ensureIdentity(ctx context.Context, dir string) error {
	if _, err := git(ctx, dir, "config", "user.email"); err == nil {
		return nil
	}
	if _, err := git(ctx, dir, "config", "user.email", "patchy[bot]@users.noreply.github.com"); err != nil {
		return err
	}
	_, err := git(ctx, dir, "config", "user.name", "patchy[bot]")
	return err
}

// checkoutBranch creates (or resets to HEAD) the remediation branch.
func checkoutBranch(ctx context.Context, dir, branch string) error {
	_, err := git(ctx, dir, "checkout", "-B", branch)
	return err
}

// verifyCommitted checks the working tree is clean and the branch carries
// at least one commit over base.
func verifyCommitted(ctx context.Context, dir, base, branch string) error {
	status, err := git(ctx, dir, "status", "--porcelain")
	if err != nil {
		return err
	}
	if status != "" {
		return fmt.Errorf("working tree not clean after commit.sh:\n%s", status)
	}
	count, err := git(ctx, dir, "rev-list", "--count", base+".."+branch)
	if err != nil {
		return err
	}
	n, err := strconv.Atoi(count)
	if err != nil || n < 1 {
		return fmt.Errorf("branch %s carries no commits over %s", branch, base)
	}
	return nil
}

// fingerprint identifies a clone's state as a report repair must leave it:
// the commit HEAD names, the tree its index stages, and the tree of its
// working files, untracked ones included and ignored ones not.
type fingerprint struct {
	head, staged, worktree string
}

// treeFingerprint fingerprints the clone in dir. Both trees are written
// from a copy of the clone's index, inside its git directory, so the
// clone's own index — what commit.sh will commit — is never touched; the
// copy keeps git's stat cache, so only files changed since it was written
// are hashed again.
//
// It catches a repair that edits, adds, stages or commits anything, which
// is what a model told to fix only its report gets wrong. It is not a
// sandbox against one set on hiding an edit: ignored paths are not hashed
// (that would hash every dependency tree), and commit.sh, which a repair
// may rewrite, runs afterwards — as a first run's commit.sh already does,
// with the same reach.
func treeFingerprint(ctx context.Context, dir string) (fingerprint, error) {
	head, err := git(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return fingerprint{}, err
	}
	gitDir, err := git(ctx, dir, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return fingerprint{}, err
	}
	index, err := os.ReadFile(filepath.Join(gitDir, "index"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fingerprint{}, err
	}
	tmp, err := os.CreateTemp(gitDir, "patchy-fingerprint-index-")
	if err != nil {
		return fingerprint{}, err
	}
	copied := tmp.Name()
	defer func() { _ = os.Remove(copied) }()
	_, err = tmp.Write(index)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fingerprint{}, err
	}
	if len(index) == 0 {
		// No index yet: git reads a missing index as an empty one, but an
		// empty file as a broken one.
		if err := os.Remove(copied); err != nil {
			return fingerprint{}, err
		}
	}
	env := []string{"GIT_INDEX_FILE=" + copied}
	staged, err := gitRawEnv(ctx, dir, env, "write-tree")
	if err != nil {
		return fingerprint{}, err
	}
	if _, err := gitRawEnv(ctx, dir, env, "add", "--all"); err != nil {
		return fingerprint{}, err
	}
	worktree, err := gitRawEnv(ctx, dir, env, "write-tree")
	if err != nil {
		return fingerprint{}, err
	}
	return fingerprint{
		head: head, staged: strings.TrimSpace(string(staged)), worktree: strings.TrimSpace(string(worktree)),
	}, nil
}

// changedPathsShown bounds how many changed paths a refusal names, and
// changedPathBytes how much of each.
const (
	changedPathsShown = 3
	changedPathBytes  = 120
)

// changed describes how the clone in dir moved from before to after, for a
// refused round's detail: whether it committed, staged, or changed working
// files, naming the first few of those. noun names the round ("a repair").
func changed(ctx context.Context, dir string, before, after fingerprint, noun string) string {
	var parts []string
	if after.head != before.head {
		parts = append(parts, "it moved HEAD (a commit)")
	}
	if after.staged != before.staged {
		parts = append(parts, "it staged changes")
	}
	if after.worktree != before.worktree {
		what := "it changed the working tree"
		out, err := git(ctx, dir, "diff", "--name-only", "--no-renames", before.worktree, after.worktree)
		if err == nil && out != "" {
			paths := strings.Split(out, "\n")
			shown := paths[:min(len(paths), changedPathsShown)]
			for i, p := range shown {
				shown[i] = oneLine(p, changedPathBytes)
			}
			what += ": " + strings.Join(shown, ", ")
			if more := len(paths) - len(shown); more > 0 {
				what += fmt.Sprintf(" and %d more", more)
			}
		}
		parts = append(parts, what)
	}
	return strings.Join(parts, "; ") + ", and " + noun + " may change nothing but the report and commit.sh"
}

// treeEntry is one path of a git tree: its mode and the object it names.
type treeEntry struct {
	mode, object string
}

// treeChange is one path a commit range changed: the entry it ends with,
// or deleted.
type treeChange struct {
	path    string
	entry   treeEntry
	deleted bool
}

// lsTree lists every path of tree in the clone in dir, recursively.
func lsTree(ctx context.Context, dir, tree string) (map[string]treeEntry, error) {
	out, err := gitRaw(ctx, dir, "ls-tree", "-r", "-z", "--full-tree", tree)
	if err != nil {
		return nil, err
	}
	entries := map[string]treeEntry{}
	for rec := range strings.SplitSeq(strings.TrimSuffix(string(out), "\x00"), "\x00") {
		if rec == "" {
			continue
		}
		meta, path, ok := strings.Cut(rec, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 {
			return nil, fmt.Errorf("git ls-tree: unexpected record %q", rec)
		}
		entries[path] = treeEntry{mode: fields[0], object: fields[2]}
	}
	return entries, nil
}

// diffTree lists the paths base..branch changed in the clone in dir,
// renames as a delete and an add.
func diffTree(ctx context.Context, dir, base, branch string) ([]treeChange, error) {
	out, err := gitRaw(ctx, dir, "diff-tree", "-r", "-z", "--no-renames", base, branch)
	if err != nil {
		return nil, err
	}
	recs := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	var changes []treeChange
	for i := 0; i+1 < len(recs); i += 2 {
		// ":<src mode> <dst mode> <src object> <dst object> <status>", then the path.
		fields := strings.Fields(strings.TrimPrefix(recs[i], ":"))
		if len(fields) != 5 {
			return nil, fmt.Errorf("git diff-tree: unexpected record %q", recs[i])
		}
		changes = append(changes, treeChange{
			path: recs[i+1], entry: treeEntry{mode: fields[1], object: fields[3]}, deleted: fields[4] == "D",
		})
	}
	return changes, nil
}

// outsideBaseline returns the paths of changes the baseline tree does not
// back: a path written with a mode or content the baseline does not hold
// there, or a path deleted that the baseline still holds. It is pure;
// TestOutsideBaselineProperty states it.
func outsideBaseline(changes []treeChange, baseline map[string]treeEntry) []string {
	var out []string
	for _, c := range changes {
		held, ok := baseline[c.path]
		if c.deleted == ok || (!c.deleted && held != c.entry) {
			out = append(out, c.path)
		}
	}
	return out
}

// committedSubset checks that what commit.sh committed after a wrap-up,
// base..branch in the clone in dir, is a subset of the working tree as the
// wrap-up found it (baseline, a tree treeFingerprint wrote): every path it
// writes has exactly the content and mode it had there, and every path it
// deletes was absent there. commit.sh may commit less than the tree held —
// leave scratch files out — but nothing the run had not written: a wrap-up
// adds a report and a commit recipe, never code, and a recipe that wrote or
// force-added a file of its own would be exactly that. It returns why the
// commits are refused, or "" when they pass.
func committedSubset(ctx context.Context, dir, base, branch, baseline string) string {
	held, err := lsTree(ctx, dir, baseline)
	if err != nil {
		return "the working tree the wrap-up found could not be read: " + err.Error()
	}
	changes, err := diffTree(ctx, dir, base, branch)
	if err != nil {
		return "the committed change could not be read: " + err.Error()
	}
	paths := outsideBaseline(changes, held)
	if len(paths) == 0 {
		return ""
	}
	shown := paths[:min(len(paths), changedPathsShown)]
	for i, p := range shown {
		shown[i] = oneLine(p, changedPathBytes)
	}
	what := strings.Join(shown, ", ")
	if more := len(paths) - len(shown); more > 0 {
		what += fmt.Sprintf(" and %d more", more)
	}
	return "commit.sh committed " + what + " other than the working tree held when the wrap-up began, and a " +
		"wrap-up build may commit only what its run had written"
}
