// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package toolexec

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
)

// A check run starts from the build (review VC-2): the writable tree, the
// build output, HOME, the caches and TMPDIR are shared by every program at
// a side, so an exploratory program, or a check run, may change them, and
// a check run's result would then depend on what ran before it. After a
// build that built, the shim keeps a copy of the tree, the build output
// and HOME beside them, with each one's digest in its own memory, and a
// manifest of the caches (cache.go). Before a fresh request (the first of
// every check run) it restores each kept place from its copy unless it
// already matches its digest, then holds it to the digest; prunes the
// caches to what the build left there, unchanged; and gives the run an
// empty TMPDIR of its own. A program of the sandbox's UID can change the
// copy too, which the digest catches: then nothing runs, never a run on a
// tree the build did not leave.

// keptDir is where the kept places' copies are kept, under the work
// directory.
const keptDir = "kept"

// MaxKeptBytes bounds the kept copy: the tree, the build output and HOME
// together, each file counted in whole 4 KiB blocks. The work volume
// (jobs' 2Gi) holds the tree and build output, their copy beside HOME's,
// and the runs' scratch directories; a build past the bound is not kept,
// so no check runs there (kept-copy-failed), rather than a copy filling
// the volume past its limit, where the kubelet evicts the pod.
const MaxKeptBytes = 768 << 20

// checkTmpName is a check run's TMPDIR under the tmp root, emptied at each
// check run's start.
const checkTmpName = ".maul-check-run"

// keptPlace is one place a check run starts from as the build left it:
// name is its copy's, under keptDir.
type keptPlace struct{ name, path string }

// keptPlaces are the places the shim keeps a copy of: the tree, the build
// output and HOME.
func (s *Server) keptPlaces() []keptPlace {
	return []keptPlace{
		{"tree", filepath.Join(s.work, "tree")},
		{"bin", filepath.Join(s.work, "bin")},
		{"home", s.home},
	}
}

// errSpecial refuses a tree holding what the copy cannot make again.
var errSpecial = errors.New("the tree holds a file that is not a regular file, a directory or a link")

// keep copies the kept places as the build left them, records each one's
// digest and the caches' manifest. It runs holding slot, after the sweep
// found no process left, so nothing changes them while it copies.
func (s *Server) keep() error {
	places := s.keptPlaces()
	bound := s.keptBound
	if bound == 0 {
		bound = MaxKeptBytes
	}
	if n, err := keptBytes(places); err != nil {
		return err
	} else if n > bound {
		return fmt.Errorf("the tree, build output and HOME take %d bytes, more than the %d the copy may", n, bound)
	}
	dst := filepath.Join(s.work, keptDir)
	if err := removeAll(dst); err != nil {
		return err
	}
	if err := os.Mkdir(dst, 0o700); err != nil {
		return err
	}
	digests, err := keepPlaces(places, dst)
	if err == nil {
		var m manifest
		if m, err = recordCache(s.cache); err == nil {
			s.built, s.cached = digests, m
			return nil
		}
	}
	_ = removeAll(dst)
	return err
}

// keepPlaces copies each place present to dst/<name> and returns each
// one's digest, which its copy must have too.
func keepPlaces(places []keptPlace, dst string) ([]string, error) {
	digests := make([]string, len(places))
	for i, p := range places {
		copied := filepath.Join(dst, p.name)
		if err := copyPresent(p.path, copied); err != nil {
			return nil, err
		}
		built, err := digestOf(p.path, p.name)
		if err != nil {
			return nil, err
		}
		if c, err := digestOf(copied, p.name); err != nil || c != built {
			return nil, errors.New("the copy is not the build's")
		}
		digests[i] = built
	}
	return digests, nil
}

// fresh starts a check run named run: the kept places as the build left
// them, the caches pruned to the build's entries and an empty TMPDIR of
// the run's own. It runs holding slot, with no process left.
func (s *Server) fresh(run string) error {
	if s.built == nil {
		return errors.New("no build left them")
	}
	for i, p := range s.keptPlaces() {
		if err := restorePlace(p, filepath.Join(s.work, keptDir, p.name), s.built[i]); err != nil {
			return err
		}
	}
	if err := s.cached.prune(s.cache); err != nil {
		return fmt.Errorf("the caches: %w", err)
	}
	tmp := filepath.Join(s.tmp, checkTmpName)
	if err := removeAll(tmp); err != nil {
		return fmt.Errorf("the run's TMPDIR: %w", err)
	}
	if err := os.Mkdir(tmp, 0o700); err != nil {
		return fmt.Errorf("the run's TMPDIR: %w", err)
	}
	s.checkRun = run
	return nil
}

// restorePlace makes p as the build left it: as it is when it matches
// built, else emptied (never removed: HOME is a mount point) and copied
// back from its copy at kept, then held to built.
func restorePlace(p keptPlace, kept, built string) error {
	if d, err := digestOf(p.path, p.name); err == nil && d == built {
		return nil
	}
	if _, err := os.Lstat(kept); errors.Is(err, fs.ErrNotExist) {
		if err := removeAll(p.path); err != nil {
			return err
		}
	} else if err := emptyDir(p.path); err != nil {
		return err
	} else if err := copyTree(kept, p.path); err != nil {
		return err
	}
	if d, err := digestOf(p.path, p.name); err != nil || d != built {
		return fmt.Errorf("the kept copy of %s is not the build's", p.name)
	}
	return nil
}

// emptyDir makes p an empty directory: what it holds removed, or p itself
// when it is not a directory. A directory is never removed itself, since a
// mount point cannot be.
func emptyDir(p string) error {
	fi, err := os.Lstat(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return os.Mkdir(p, 0o700)
	case err != nil:
		return err
	case !fi.IsDir():
		if err := os.Remove(p); err != nil {
			return err
		}
		return os.Mkdir(p, 0o700)
	}
	if err := os.Chmod(p, 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(p)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := removeAll(filepath.Join(p, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// keptBytes is what a copy of places takes: each regular file and link in
// whole 4 KiB blocks, each directory one.
func keptBytes(places []keptPlace) (int64, error) {
	const block = 4 << 10
	var n int64
	for _, p := range places {
		if _, err := os.Lstat(p.path); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		err := filepath.WalkDir(p.path, func(_ string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			size := int64(0)
			if d.Type().IsRegular() {
				info, err := d.Info()
				if err != nil {
					return err
				}
				size = info.Size()
			}
			n += max(1, (size+block-1)/block) * block
			return nil
		})
		if err != nil {
			return 0, err
		}
	}
	return n, nil
}

// copyPresent copies src to dst, which must not exist, unless src is
// absent.
func copyPresent(src, dst string) error {
	if _, err := os.Lstat(src); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	return copyTree(src, dst)
}

// copyTree copies src to dst, which must not exist or be an empty
// directory: directories, regular files (their mode bits and modification
// time) and links as links, never followed. A directory gets its mode once
// everything in it is written.
func copyTree(src, dst string) error {
	type dir struct {
		path string
		mode fs.FileMode
	}
	var dirs []dir
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		to := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			dirs = append(dirs, dir{to, info.Mode().Perm()})
			if err := os.Mkdir(to, 0o700); err != nil && (rel != "." || !errors.Is(err, fs.ErrExist)) {
				return err
			}
			return nil
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(target, to)
		case d.Type().IsRegular():
			return copyFile(p, to, info)
		}
		return errSpecial
	})
	if err != nil {
		return err
	}
	for _, d := range slices.Backward(dirs) {
		if err := os.Chmod(d.path, d.mode); err != nil {
			return err
		}
	}
	return nil
}

// copyFile copies the regular file src to the new file dst.
func copyFile(src, dst string, info fs.FileInfo) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Chmod(dst, info.Mode().Perm()); err != nil {
		return err
	}
	return os.Chtimes(dst, info.ModTime(), info.ModTime())
}

// removeAll removes p and everything under it, first making each of its
// directories readable and writable (a build may leave read-only ones, a
// program unreadable ones) before WalkDir reads it. A link is removed,
// never followed.
func removeAll(p string) error {
	_ = filepath.WalkDir(p, func(q string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(q, 0o700)
		}
		return nil
	})
	return os.RemoveAll(p)
}

// digestOf is the digest of the place named name at root: each entry's
// path, type and mode bits, a regular file's content and a link's target,
// in lexical order; a place absent is recorded as absent.
func digestOf(root, name string) (string, error) {
	h := sha256.New()
	if err := digestTree(h, root, name); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func digestTree(h hash.Hash, root, name string) error {
	if _, err := os.Lstat(root); errors.Is(err, fs.ErrNotExist) {
		_, _ = fmt.Fprintf(h, "%q absent\n", name)
		return nil
	}
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(h, "%q %q %v %v ", name, rel, info.Mode().Type(), info.Mode().Perm())
		switch {
		case d.IsDir():
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(h, "%q", target)
		case d.Type().IsRegular():
			if err := digestFile(h, p); err != nil {
				return err
			}
		default:
			return errSpecial
		}
		_, _ = io.WriteString(h, "\n")
		return nil
	})
}

func digestFile(h hash.Hash, p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	fh := sha256.New()
	if _, err := io.Copy(fh, f); err != nil {
		return err
	}
	_, _ = io.WriteString(h, hex.EncodeToString(fh.Sum(nil)))
	return nil
}
