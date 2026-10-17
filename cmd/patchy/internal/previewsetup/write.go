// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewsetup

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/pmezard/go-difflib/difflib"

	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/ghapp"
)

// State is what a comparison found for one file.
type State string

const (
	StateNew       State = "new"
	StateChanged   State = "changed"
	StateUnchanged State = "unchanged"
	// StateUnsafe is a path never written through: not a regular file, or
	// reached through a symbolic link or a non-directory.
	StateUnsafe State = "unsafe"
)

// Change is one file of a plan against the disk.
type Change struct {
	File  File
	State State
	// Diff is a unified diff, old (nothing, for a new file) to new, for a
	// file that is not a secret and would change; "" otherwise.
	Diff string
}

// Comparison is a plan against the disk.
type Comparison struct {
	Dir     string
	Changes []Change
	// Stale are files under the generated directories an earlier run
	// wrote that this plan no longer generates (another edge, say). They
	// are reported, never deleted: whether they are still applied is the
	// operator's to say.
	Stale []string
}

// generatedDirs are the directories a plan writes under, scanned for stale
// files.
var generatedDirs = []string{"manifests", "secrets", "upstream"}

// Compare reads what is at each of files' paths under dir.
func Compare(dir string, files []File) (Comparison, error) {
	c := Comparison{Dir: dir}
	planned := map[string]bool{}
	for _, f := range files {
		planned[f.Path] = true
		ch := Change{File: f}
		state, old, err := read(dir, f.Path)
		if err != nil {
			return Comparison{}, err
		}
		switch {
		case state == StateUnsafe:
			ch.State = state
		case state == StateNew:
			ch.State = state
			if !f.Secret {
				ch.Diff = unifiedDiff(f.Path, nil, f.Data)
			}
		case bytes.Equal(old, f.Data):
			ch.State = StateUnchanged
		default:
			ch.State = StateChanged
			if !f.Secret {
				ch.Diff = unifiedDiff(f.Path, old, f.Data)
			}
		}
		c.Changes = append(c.Changes, ch)
	}
	for _, sub := range generatedDirs {
		entries, err := os.ReadDir(filepath.Join(dir, sub))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return Comparison{}, err
		}
		for _, e := range entries {
			p := sub + "/" + e.Name()
			if !e.IsDir() && strings.HasSuffix(p, ".yaml") && !planned[p] {
				c.Stale = append(c.Stale, p)
			}
		}
	}
	slices.Sort(c.Stale)
	return c, nil
}

// read is the state of rel under dir and, for a regular file, its bytes.
func read(dir, rel string) (State, []byte, error) {
	parts := strings.Split(rel, "/")
	current := dir
	for i, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			return StateNew, nil, nil
		}
		if err != nil {
			return "", nil, err
		}
		last := i == len(parts)-1
		switch {
		case info.Mode()&fs.ModeSymlink != 0, !last && !info.IsDir(), last && !info.Mode().IsRegular():
			return StateUnsafe, nil, nil
		}
	}
	data, err := os.ReadFile(current)
	if err != nil {
		return "", nil, err
	}
	return StateChanged, data, nil
}

// unifiedDiff is old to new as a unified diff with three lines of context.
func unifiedDiff(path string, old, cur []byte) string {
	lines := func(b []byte) []string {
		if len(b) == 0 {
			return nil // SplitLines would make one empty line of nothing
		}
		return difflib.SplitLines(string(b))
	}
	out, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A: lines(old), B: lines(cur),
		FromFile: "a/" + path, ToFile: "b/" + path, Context: 3,
	})
	if err != nil {
		return fmt.Sprintf("(no diff: %v)\n", err)
	}
	return out
}

// Pending reports whether writing would change anything.
func (c Comparison) Pending() bool {
	return slices.ContainsFunc(c.Changes, func(ch Change) bool { return ch.State != StateUnchanged })
}

// ConflictError is a refusal to write: files that would change without
// being accepted, and paths never written through.
type ConflictError struct {
	Changed []string
	Unsafe  []string
}

func (e *ConflictError) Error() string {
	var parts []string
	if len(e.Changed) > 0 {
		parts = append(parts, fmt.Sprintf("%d file(s) would change (%s): review the difference and pass --force "+
			"to replace them", len(e.Changed), strings.Join(e.Changed, ", ")))
	}
	if len(e.Unsafe) > 0 {
		parts = append(parts, "not a regular file, or behind a symbolic link, so never written: "+
			strings.Join(e.Unsafe, ", "))
	}
	return strings.Join(parts, "; ")
}

// Accept says which changed files a write may replace.
type Accept struct {
	// All accepts every changed file (--force).
	All bool
	// Secrets accepts changed secret files only (a rotation).
	Secrets bool
}

// Write applies c: it creates the directory (0700) and writes every new
// file, and every changed one accept allows, each with mode 0600 through
// ghapp's writer (a new file is created exclusively, a replaced one
// renamed over, so a symbolic link is never followed). It checks every
// file before writing any: a changed file not accepted, or an unsafe
// path, writes nothing and returns a *ConflictError.
func Write(c Comparison, accept Accept) ([]string, error) {
	conflict := &ConflictError{}
	for _, ch := range c.Changes {
		switch {
		case ch.State == StateUnsafe:
			conflict.Unsafe = append(conflict.Unsafe, ch.File.Path)
		case ch.State == StateChanged && !accept.All && (!accept.Secrets || !ch.File.Secret):
			conflict.Changed = append(conflict.Changed, ch.File.Path)
		}
	}
	if len(conflict.Changed) > 0 || len(conflict.Unsafe) > 0 {
		return nil, conflict
	}
	if err := mkdirPrivate(c.Dir); err != nil {
		return nil, err
	}
	var written []string
	for _, ch := range c.Changes {
		if ch.State == StateUnchanged {
			continue
		}
		target := filepath.Join(c.Dir, filepath.FromSlash(ch.File.Path))
		if err := mkdirPrivate(filepath.Dir(target)); err != nil {
			return written, err
		}
		if err := ghapp.WriteFile(target, ch.File.Data, ch.State == StateChanged); err != nil {
			return written, err
		}
		written = append(written, ch.File.Path)
	}
	return written, nil
}

// mkdirPrivate creates dir, and any parent, with mode 0700; an existing
// directory is left as it is.
func mkdirPrivate(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	return nil
}
