// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package maultool

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
)

// ScratchDir is the evaluator's scratch directory: its working directory
// and the only place its file tools reach.
const ScratchDir = "/work/scratch"

// ShotsDir is where screenshots land, under the scratch directory.
const ShotsDir = "shots"

// scratch is the scratch directory, opened as a root nothing escapes.
type scratch struct {
	dir string
}

// rel resolves name, relative to the scratch directory or absolute inside
// it, to a slash path under it; anything else is refused.
func (s scratch) rel(name string) (string, error) {
	if name == "" {
		return "", errors.New("no file named")
	}
	if filepath.IsAbs(name) {
		r, err := filepath.Rel(filepath.Clean(s.dir), filepath.Clean(name))
		if err != nil || !filepath.IsLocal(r) {
			return "", fmt.Errorf("%s is not under %s", name, s.dir)
		}
		name = r
	}
	if !filepath.IsLocal(name) {
		return "", fmt.Errorf("%s is not under %s", name, s.dir)
	}
	return filepath.ToSlash(filepath.Clean(name)), nil
}

// read reads a regular file under the scratch directory, at most limit
// bytes, following no symlink out of it. It returns the file's slash path
// relative to the scratch directory.
func (s scratch) read(name string, limit int) (string, []byte, error) {
	rel, err := s.rel(name)
	if err != nil {
		return "", nil, err
	}
	root, err := os.OpenRoot(s.dir)
	if err != nil {
		return "", nil, fmt.Errorf("opening %s: %w", s.dir, err)
	}
	defer func() { _ = root.Close() }()
	f, err := root.Open(filepath.FromSlash(rel))
	if err != nil {
		return "", nil, fmt.Errorf("%s cannot be read under %s", name, s.dir)
	}
	defer func() { _ = f.Close() }()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return "", nil, fmt.Errorf("%s is not a regular file", name)
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil {
		return "", nil, fmt.Errorf("reading %s: %w", name, err)
	}
	if len(data) > limit {
		return "", nil, fmt.Errorf("%s is larger than %d bytes", name, limit)
	}
	return rel, data, nil
}

// shotName is the n-th (from 1) screenshot of ledger entry seq:
// shots/<seq>.png, then shots/<seq>-<n>.png.
func shotName(seq int64, n int) string {
	name := strconv.FormatInt(seq, 10)
	if n > 1 {
		name += "-" + strconv.Itoa(n)
	}
	return ShotsDir + "/" + name + ".png"
}

// writeShot writes a screenshot under the scratch directory and returns
// its path. The name is maul-tool's own, never the tools container's.
func (s scratch) writeShot(seq int64, n int, data []byte) (string, error) {
	root, err := os.OpenRoot(s.dir)
	if err != nil {
		return "", fmt.Errorf("opening %s: %w", s.dir, err)
	}
	defer func() { _ = root.Close() }()
	if err := root.MkdirAll(ShotsDir, 0o700); err != nil {
		return "", fmt.Errorf("creating %s under %s: %w", ShotsDir, s.dir, err)
	}
	name := shotName(seq, n)
	if err := root.WriteFile(name, data, 0o600); err != nil {
		return "", fmt.Errorf("writing %s under %s: %w", name, s.dir, err)
	}
	return filepath.Join(s.dir, filepath.FromSlash(name)), nil
}
