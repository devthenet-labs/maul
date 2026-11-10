// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package toolexec

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// The caches (GOCACHE and npm's, under Options.Cache) are too large to
// copy beside the tree: a large Go tree's build cache is gigabytes, more
// than the work volume holds. Both are content-addressed: a Go cache entry
// missing is only a miss, which the go command builds again, never a
// different answer; an npm entry missing is fetched again where the
// network allows, else an install error, never another package. Only an
// entry a program changed or added is removed, and an exploration's or
// check's own install leaves nothing the build's did not. The shim records, after a build that built,
// a manifest of every entry the build left: its type and mode, a file's
// size, its inode and its change time (ctime), which no program can set.
// Before a check run it removes every entry the manifest does not hold
// exactly, never following a link: what remains is the build's own, as
// the build left it, and an entry a program deleted stays deleted.

// fileID is what identifies a file's content without reading it: its
// inode and change time, which every write, rename, link and chmod moves
// on.
type fileID struct {
	ino   uint64
	ctime int64
}

// cacheEntry is one entry of the manifest.
type cacheEntry struct {
	mode fs.FileMode
	size int64
	id   fileID
}

// manifest maps each cache entry's path, relative to the cache directory
// ("." the directory itself), to what the build left there.
type manifest map[string]cacheEntry

// ctimeWait bounds how long recordCache waits for the clock to pass the
// last change it recorded.
const ctimeWait = 2 * time.Second

// recordCache records the manifest of the cache directory dir. An entry
// it cannot read is left out, so the next check run removes it. It returns
// once a file made now has a later change time than every entry recorded,
// so no later change can carry a recorded one's.
func recordCache(dir string) (manifest, error) {
	m := manifest{}
	var last int64
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		rel, _ := filepath.Rel(dir, p)
		if err != nil {
			delete(m, rel)
			if d != nil && d.IsDir() && rel != "." {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		id, ok := idOf(info)
		if !ok {
			return nil
		}
		m[rel] = cacheEntry{mode: info.Mode(), size: info.Size(), id: id}
		last = max(last, id.ctime)
		return nil
	})
	if err := waitPast(dir, last); err != nil {
		return nil, err
	}
	return m, nil
}

// waitPast waits until a file made in dir has a later change time than
// last (a file system's clock moves in ticks).
func waitPast(dir string, last int64) error {
	deadline := time.Now().Add(ctimeWait)
	for {
		f, err := os.CreateTemp(dir, ".maul-tick-")
		if err != nil {
			return err
		}
		info, err := f.Stat()
		_ = f.Close()
		_ = os.Remove(f.Name())
		if err != nil {
			return err
		}
		id, ok := idOf(info)
		if !ok || id.ctime > last {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("the clock did not move past the cache's last change")
		}
		time.Sleep(time.Millisecond)
	}
}

// holds reports that info is the entry the build left at rel: its type,
// mode and inode, and for anything but a directory (whose change time
// moves whenever an entry is added) its size and change time.
func (m manifest) holds(rel string, info fs.FileInfo) bool {
	e, ok := m[rel]
	if !ok || e.mode != info.Mode() {
		return false
	}
	id, ok := idOf(info)
	if !ok || id.ino != e.id.ino {
		return false
	}
	return info.IsDir() || id.ctime == e.id.ctime && info.Size() == e.size
}

// prune removes from the cache directory dir every entry m does not hold
// exactly. dir itself is a mount point, so it is emptied and given its
// mode back, never removed.
func (m manifest) prune(dir string) error {
	root, ok := m["."]
	info, err := os.Lstat(dir)
	if err != nil || !ok || !info.IsDir() || !root.mode.IsDir() {
		return emptyDir(dir)
	}
	if info.Mode() != root.mode {
		if err := chmod(dir, root.mode.Perm()); err != nil {
			return err
		}
	}
	if err := m.pruneDir(dir, "."); err != nil {
		return fmt.Errorf("prune: %w", err)
	}
	return nil
}

// pruneDir removes every entry under dir (rel in the manifest) that m does
// not hold, and prunes each directory it does. A directory it cannot read
// is removed whole.
func (m manifest) pruneDir(dir, rel string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		p, r := filepath.Join(dir, e.Name()), filepath.Join(rel, e.Name())
		info, err := os.Lstat(p)
		if err == nil && m.holds(r, info) && (!info.IsDir() || m.pruneDir(p, r) == nil) {
			continue
		}
		if err := removeAll(p); err != nil {
			return err
		}
	}
	return nil
}
