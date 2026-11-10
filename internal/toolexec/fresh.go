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

// A check run starts from the build (review VC-2): the writable tree and
// the build output are shared by every program at a side, so an
// exploratory program, or a check, may change them. After a build that
// built, the shim keeps a copy of both beside them and their digest in its
// own memory; before a fresh request it restores them from the copy unless
// they already match the digest, then holds them to it. A program of the
// sandbox's UID can change the copy too, which the digest catches: then
// nothing runs, never a run on a tree the build did not leave.

// kept are the work directory's entries a fresh request starts from;
// keptDir is where their copy is kept, under the work directory.
var kept = []string{"tree", "bin"}

const keptDir = "kept"

// errSpecial refuses a tree holding what the copy cannot make again.
var errSpecial = errors.New("the tree holds a file that is not a regular file, a directory or a link")

// keep copies the tree and build output as the build left them and
// records their digest. It runs holding slot, after the sweep found no
// process left, so nothing changes them while it copies.
func (s *Server) keep() error {
	dst := filepath.Join(s.work, keptDir)
	if err := removeAll(dst); err != nil {
		return err
	}
	if err := os.Mkdir(dst, 0o700); err != nil {
		return err
	}
	if err := copyKept(s.work, dst); err != nil {
		_ = removeAll(dst)
		return err
	}
	built, err := digestOf(s.work)
	if err != nil {
		_ = removeAll(dst)
		return err
	}
	if copied, err := digestOf(dst); err != nil || copied != built {
		_ = removeAll(dst)
		return errors.New("the copy is not the build's")
	}
	s.built = built
	return nil
}

// restore makes the tree and build output the build's again: as they are
// when they match its digest, else copied back from the kept copy and held
// to the digest. It runs holding slot, with no process left.
func (s *Server) restore() error {
	if s.built == "" {
		return errors.New("no build left them")
	}
	if d, err := digestOf(s.work); err == nil && d == s.built {
		return nil
	}
	for _, name := range kept {
		if err := removeAll(filepath.Join(s.work, name)); err != nil {
			return err
		}
	}
	if err := copyKept(filepath.Join(s.work, keptDir), s.work); err != nil {
		return err
	}
	if d, err := digestOf(s.work); err != nil || d != s.built {
		return errors.New("the kept copy is not the build's")
	}
	return nil
}

// copyKept copies each kept entry present under src to dst.
func copyKept(src, dst string) error {
	for _, name := range kept {
		from := filepath.Join(src, name)
		if _, err := os.Lstat(from); errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		if err := copyTree(from, filepath.Join(dst, name)); err != nil {
			return err
		}
	}
	return nil
}

// copyTree copies src to dst, which must not exist: directories, regular
// files (their mode bits and modification time) and links as links, never
// followed. A directory gets its mode once everything in it is written.
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
			return os.Mkdir(to, 0o700)
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

// digestOf is the digest of the kept entries under work: each entry's path,
// type and mode bits, a regular file's content and a link's target, in
// lexical order; an entry absent is recorded as absent.
func digestOf(work string) (string, error) {
	h := sha256.New()
	for _, name := range kept {
		if err := digestTree(h, filepath.Join(work, name), name); err != nil {
			return "", err
		}
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
