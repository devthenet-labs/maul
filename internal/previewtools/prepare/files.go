// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package prepare

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// The work volume's layout (SPEC Q23): the writable tree, the build's
// output and the per-run scratch.
const (
	WorkTree    = "tree"
	WorkBin     = "bin"
	WorkScratch = "scratch"
)

// Readable makes every entry below dir readable by all, and searchable or
// executable by all where it is a directory or anyone may execute it
// (chmod a+rX), as `find dir -mindepth 1` would: dir itself, a volume's
// root the init does not own, is left alone, and no link is followed.
func Readable(dir string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == "." || d.Type()&fs.ModeSymlink != 0 {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		mode := info.Mode().Perm() | 0o444
		if d.IsDir() || mode&0o111 != 0 {
			mode |= 0o111
		}
		return root.Chmod(p, mode)
	})
}

// Install copies the regular file src into dir under its own name, mode
// 0555, never over an existing file.
func Install(src, dir string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return refuse("%s is not a regular file", src)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	name := filepath.Base(src)
	out, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o555)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("install %s: %w", name, err)
	}
	return root.Chmod(name, 0o555)
}

// Seed makes work's layout (WorkTree, WorkBin, WorkScratch; the last two
// may exist already) and copies the pristine tree src into work/WorkTree,
// which must not: directories 0755, files with their pristine mode plus
// the owner's write, links as they are. Every write goes through an
// os.Root at work/WorkTree or work, so nothing lands outside work; the
// same hygiene as Extract holds, a link pointing or resolving outside the
// tree or an entry that is not a directory, file or link refused.
func Seed(src, work string) error {
	dst, err := os.OpenRoot(work)
	if err != nil {
		return err
	}
	defer func() { _ = dst.Close() }()
	if _, err := dst.Lstat(WorkTree); !errors.Is(err, fs.ErrNotExist) {
		if err == nil {
			return refuse("%s already exists in %s", WorkTree, work)
		}
		return err
	}
	for _, d := range []string{WorkBin, WorkScratch} {
		if err := mkdirP(dst, d); err != nil {
			return err
		}
	}
	if err := dst.Mkdir(WorkTree, dirMode); err != nil {
		return err
	}
	if err := dst.Chmod(WorkTree, dirMode); err != nil {
		return err
	}
	tree, err := dst.OpenRoot(WorkTree)
	if err != nil {
		return err
	}
	defer func() { _ = tree.Close() }()
	from, err := os.OpenRoot(src)
	if err != nil {
		return err
	}
	defer func() { _ = from.Close() }()
	links, err := copyTree(from, tree)
	if err != nil {
		return err
	}
	return linksResolveInside(tree, links)
}

// mkdirP makes the directory name in root unless it is one already.
func mkdirP(root *os.Root, name string) error {
	info, err := root.Lstat(name)
	if err == nil {
		if !info.IsDir() {
			return refuse("%s exists and is not a directory", name)
		}
		return nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := root.Mkdir(name, dirMode); err != nil {
		return err
	}
	return root.Chmod(name, dirMode)
}

// copyTree copies every entry of from into to, returning the links made.
func copyTree(from, to *os.Root) ([]string, error) {
	var links []string
	err := fs.WalkDir(from.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == "." {
			return err
		}
		switch t := d.Type(); {
		case t.IsDir():
			if err := to.Mkdir(p, dirMode); err != nil {
				return err
			}
			return to.Chmod(p, dirMode)
		case t.IsRegular():
			return copyFile(from, to, p)
		case t&fs.ModeSymlink != 0:
			target, err := from.Readlink(p)
			if err != nil {
				return err
			}
			if !linkInside(p, target) {
				return refuse("link %q points outside the tree (%q)", p, target)
			}
			links = append(links, p)
			return to.Symlink(target, p)
		default:
			return refuse("%q has type %v; a tree holds only directories, files and links", p, t)
		}
	})
	return links, err
}

// copyFile copies the regular file p, writable by its owner.
func copyFile(from, to *os.Root, p string) error {
	in, err := from.Open(p)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return refuse("%q changed type while it was copied", p)
	}
	perm := (info.Mode().Perm() | 0o200) &^ 0o022
	out, err := to.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("copy %s: %w", p, err)
	}
	return to.Chmod(p, perm)
}
