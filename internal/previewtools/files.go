// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/devthenet-labs/maul/internal/toolwire"
)

// The files verb (SPEC-S2 Q24): a look at a side's pristine tree, the
// digest-verified source prepare extracted read-only, never at what a
// build rewrote and never executing it. Every path is opened through an
// os.Root over the tree, so no name, ".." or symlink reaches outside it;
// grep and ls never follow a symlink at all. What it shows is the
// repository's (untrusted).
//
//   - ls lists one directory: each entry's name, "/" after a directory, "@"
//     after a symlink, sorted, at most toolwire.MaxListEntries;
//   - read returns a regular file's first toolwire.MaxFileReadBytes;
//   - grep finds Pattern as a literal substring (never a regular
//     expression), line by line, in the regular, non-binary files under
//     Path: "<path>:<line>:<text>", at most toolwire.MaxGrepHits.

// Bounds of a grep: a file over maxGrepFileBytes or holding a NUL in its
// first binarySniffBytes is skipped, a line is shown to maxGrepLineBytes,
// and a walk reads at most maxGrepTotalBytes.
const (
	maxGrepFileBytes  = 4 << 20
	binarySniffBytes  = 8 << 10
	maxGrepLineBytes  = 512
	maxGrepTotalBytes = 256 << 20
)

// errNotInTree is a files path that names nothing in the tree maul shows.
var errNotInTree = errors.New("names nothing in the tree")

// files answers `maul-tool files`.
func (r *Relay) files(req *toolwire.FilesRequest) result {
	side := req.Side.Recorded()
	if !r.cfg.IsWorkspace() {
		return refusal(toolwire.RefusedUnavailable, "files needs a workspace; this tool pod has a preview")
	}
	root, err := os.OpenRoot(filepath.Join(r.trees, string(sideName(side))))
	if err != nil {
		return sideRefusal(side, toolwire.RefusedUnavailable, "the pristine tree is not there")
	}
	defer func() { _ = root.Close() }()
	name := req.Path
	if name == "" {
		name = "."
	}
	var out string
	switch req.Op {
	case toolwire.OpLs:
		out, err = listDir(root, name)
	case toolwire.OpRead:
		out, err = readFile(root, name)
	default:
		out, err = grepTree(root, name, req.Pattern)
	}
	if err != nil {
		return sideRefusal(side, toolwire.RefusedInvalid, fmt.Sprintf("%s %q %v", req.Op, req.Path, err))
	}
	return withSide(side, result{output: out, inline: out})
}

// listDir lists the directory name.
func listDir(root *os.Root, name string) (string, error) {
	fi, err := root.Lstat(name)
	if err != nil || !fi.IsDir() {
		return "", errors.New("is not a directory in the tree")
	}
	entries, err := fs.ReadDir(root.FS(), name)
	if err != nil {
		return "", errNotInTree
	}
	var b strings.Builder
	n := 0
	for _, e := range entries {
		if n == toolwire.MaxListEntries {
			fmt.Fprintf(&b, "[maul: %d more entries not listed]\n", len(entries)-n)
			break
		}
		b.WriteString(e.Name())
		switch {
		case e.Type()&fs.ModeSymlink != 0:
			b.WriteString("@")
		case e.IsDir():
			b.WriteString("/")
		}
		b.WriteString("\n")
		n++
	}
	return b.String(), nil
}

// readFile reads the regular file name, cut at MaxFileReadBytes.
func readFile(root *os.Root, name string) (string, error) {
	f, err := root.Open(name)
	if err != nil {
		return "", errNotInTree
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return "", errors.New("is not a regular file in the tree")
	}
	b, err := io.ReadAll(io.LimitReader(f, toolwire.MaxFileReadBytes+1))
	if err != nil {
		return "", errNotInTree
	}
	s := strings.ToValidUTF8(string(b), "�")
	if len(b) > toolwire.MaxFileReadBytes {
		cut := toolwire.MaxFileReadBytes
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + toolwire.TruncationMarker
	}
	return s, nil
}

// grepTree finds pattern in the files under name.
func grepTree(root *os.Root, name, pattern string) (string, error) {
	fi, err := root.Lstat(name)
	if err != nil || (!fi.IsDir() && !fi.Mode().IsRegular()) {
		return "", errNotInTree
	}
	g := grep{root: root, pattern: []byte(pattern)}
	if !fi.IsDir() {
		g.file(name)
		return g.String(), nil
	}
	err = fs.WalkDir(root.FS(), name, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable entry is skipped
		}
		if g.full() {
			return fs.SkipAll
		}
		if d.Type().IsRegular() {
			g.file(p)
		}
		return nil // a symlink is never followed
	})
	if err != nil {
		return "", errNotInTree
	}
	return g.String(), nil
}

// grep collects hits.
type grep struct {
	root    *os.Root
	pattern []byte
	hits    []string
	read    int64
	cut     bool
}

func (g *grep) full() bool { return len(g.hits) >= toolwire.MaxGrepHits || g.read >= maxGrepTotalBytes }

// file greps one regular file, unless it is too large or binary.
func (g *grep) file(name string) {
	f, err := g.root.Open(name)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxGrepFileBytes {
		return
	}
	data, err := io.ReadAll(io.LimitReader(f, maxGrepFileBytes))
	if err != nil {
		return
	}
	g.read += int64(len(data))
	if bytes.IndexByte(data[:min(len(data), binarySniffBytes)], 0) >= 0 {
		return
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64<<10), maxGrepFileBytes)
	for line := 1; sc.Scan(); line++ {
		if !bytes.Contains(sc.Bytes(), g.pattern) {
			continue
		}
		if len(g.hits) >= toolwire.MaxGrepHits {
			g.cut = true
			return
		}
		text := strings.ToValidUTF8(string(sc.Bytes()), "�")
		if len(text) > maxGrepLineBytes {
			cut := maxGrepLineBytes
			for cut > 0 && !utf8.RuneStart(text[cut]) {
				cut--
			}
			text = text[:cut] + "…"
		}
		g.hits = append(g.hits, fmt.Sprintf("%s:%d:%s", path.Clean(name), line, text))
	}
}

func (g *grep) String() string {
	hits := slices.Clone(g.hits)
	if g.cut || g.read >= maxGrepTotalBytes {
		hits = append(hits, fmt.Sprintf("[maul: stopped at %d hits or %d bytes read]", toolwire.MaxGrepHits,
			maxGrepTotalBytes))
	}
	if len(hits) == 0 {
		return ""
	}
	return strings.Join(hits, "\n") + "\n"
}
