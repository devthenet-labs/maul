// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package prepare

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// ErrRefused wraps every refusal of a tree: what the archive or the
// destination holds, never a transient failure, so it is never retried.
var ErrRefused = errors.New("prepare: refused")

// MaxTreeBytes bounds one extracted tree's regular files, within the
// tool pod's trees volume (internal/jobs: 512Mi per side), so an oversized
// tree is a named refusal rather than an eviction.
const MaxTreeBytes = 480 << 20

// dirMode is every directory's mode in a tree.
const dirMode fs.FileMode = 0o755

// refuse is an ErrRefused with its reason.
func refuse(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrRefused, fmt.Sprintf(format, args...))
}

// Extract unpacks a gzipped tar, as GitHub's archive endpoint lays one
// out (an optional pax global header, then one top-level directory holding
// the tree), into dir, the top-level directory stripped as
// `tar --strip-components=1` would. dir must be empty. A file's mode is its
// archive mode's permission bits, never writable by group or others; a
// directory's is 0755. Regular files past limit bytes in all are refused.
func Extract(r io.Reader, dir string, limit int64) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if err := mustBeEmpty(root, dir); err != nil {
		return err
	}
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("open archive: %w", err)
	}
	defer func() { _ = gz.Close() }()
	x := &extractor{root: root, left: limit, kinds: map[string]kind{}}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read archive: %w", err)
		}
		if err := x.entry(hdr, tr); err != nil {
			return err
		}
	}
	// The gzip trailer's checksum is read only at the stream's end.
	if _, err := io.Copy(io.Discard, gz); err != nil {
		return fmt.Errorf("read archive: %w", err)
	}
	if x.top == "" {
		return refuse("the archive holds no tree")
	}
	return linksResolveInside(root, x.links)
}

// kind is what a tree path already is.
type kind int

const (
	kindImplicitDir kind = iota + 1 // a parent made for a later entry
	kindDir
	kindFile
	kindLink
)

// extractor is one Extract's state.
type extractor struct {
	root  *os.Root
	top   string
	left  int64
	kinds map[string]kind
	links []string
}

// entry writes one archive entry.
func (x *extractor) entry(hdr *tar.Header, r io.Reader) error {
	if hdr.Typeflag == tar.TypeXGlobalHeader {
		return nil
	}
	name, err := x.strip(hdr)
	if err != nil || name == "" {
		return err
	}
	if err := x.parents(name); err != nil {
		return err
	}
	switch hdr.Typeflag {
	case tar.TypeDir:
		return x.dir(name)
	case tar.TypeReg:
		return x.file(name, hdr, r)
	case tar.TypeSymlink:
		return x.link(name, hdr.Linkname)
	default:
		return refuse("entry %q has type %q; a tree holds only directories, files and links", hdr.Name,
			hdr.Typeflag)
	}
}

// strip is hdr's path below the top-level directory, cleaned; "" for the
// top-level directory itself.
func (x *extractor) strip(hdr *tar.Header) (string, error) {
	if !filepath.IsLocal(hdr.Name) {
		return "", refuse("entry %q escapes the tree", hdr.Name)
	}
	first, rest, _ := strings.Cut(hdr.Name, "/")
	rest = strings.TrimSuffix(rest, "/")
	if first == "." || (rest == "" && hdr.Typeflag != tar.TypeDir) {
		return "", refuse("entry %q is not below the archive's one top-level directory", hdr.Name)
	}
	if x.top == "" {
		x.top = first
	}
	if first != x.top {
		return "", refuse("entry %q is not below the archive's one top-level directory %q", hdr.Name, x.top)
	}
	if rest == "" {
		return "", nil
	}
	if !filepath.IsLocal(rest) {
		return "", refuse("entry %q escapes the tree", hdr.Name)
	}
	return filepath.Clean(rest), nil
}

// parents makes name's parent directories, refusing a path through a link
// or a file.
func (x *extractor) parents(name string) error {
	parent := filepath.Dir(name)
	if parent == "." {
		return nil
	}
	parts := strings.Split(parent, string(filepath.Separator))
	for i := range parts {
		p := filepath.Join(parts[:i+1]...)
		switch x.kinds[p] {
		case kindDir, kindImplicitDir:
			continue
		case kindLink:
			return refuse("entry %q is reached through the link %q", name, p)
		case kindFile:
			return refuse("entry %q is below the file %q", name, p)
		}
		if err := x.mkdir(p); err != nil {
			return err
		}
		x.kinds[p] = kindImplicitDir
	}
	return nil
}

// dir makes the directory name, once.
func (x *extractor) dir(name string) error {
	switch x.kinds[name] {
	case kindImplicitDir:
		x.kinds[name] = kindDir
		return nil
	case 0:
	default:
		return refuse("%q appears twice", name)
	}
	if err := x.mkdir(name); err != nil {
		return err
	}
	x.kinds[name] = kindDir
	return nil
}

func (x *extractor) mkdir(name string) error {
	if err := x.root.Mkdir(name, dirMode); err != nil {
		return err
	}
	return x.root.Chmod(name, dirMode)
}

// file writes the regular file name, within the size left.
func (x *extractor) file(name string, hdr *tar.Header, r io.Reader) error {
	if x.kinds[name] != 0 {
		return refuse("%q appears twice", name)
	}
	if hdr.Size > x.left {
		return refuse("the tree is over its limit at %q", name)
	}
	x.kinds[name] = kindFile
	perm := fs.FileMode(hdr.Mode).Perm() &^ 0o022
	f, err := x.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(r, x.left+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	if x.left -= n; x.left < 0 {
		return refuse("the tree is over its limit at %q", name)
	}
	return x.root.Chmod(name, perm)
}

// link makes the symbolic link name to target, which must stay inside the
// tree.
func (x *extractor) link(name, target string) error {
	if x.kinds[name] != 0 {
		return refuse("%q appears twice", name)
	}
	if !linkInside(name, target) {
		return refuse("link %q points outside the tree (%q)", name, target)
	}
	x.kinds[name] = kindLink
	x.links = append(x.links, name)
	return x.root.Symlink(target, name)
}

// linkInside reports whether a link at name to target names a path inside
// the tree, read lexically from the link's own directory.
func linkInside(name, target string) bool {
	if target == "" || filepath.IsAbs(target) {
		return false
	}
	return filepath.IsLocal(filepath.Join(filepath.Dir(name), target))
}

// linksResolveInside refuses a link that, followed through the tree's
// other links, resolves outside root; a link to nothing is left as it is.
func linksResolveInside(root *os.Root, links []string) error {
	for _, l := range slices.Sorted(slices.Values(links)) {
		if _, err := root.Stat(l); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return refuse("link %q resolves outside the tree: %v", l, err)
		}
	}
	return nil
}

// mustBeEmpty refuses a destination that holds anything.
func mustBeEmpty(root *os.Root, dir string) error {
	d, err := root.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	names, err := d.Readdirnames(1)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if len(names) > 0 {
		return refuse("%s is not empty", dir)
	}
	return nil
}
