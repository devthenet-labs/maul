// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package prepare

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func mkfifo(t *testing.T, p string) {
	t.Helper()
	if err := syscall.Mkfifo(p, 0o644); err != nil {
		t.Fatal(err)
	}
}

// entry is one tar entry of a test archive.
type entry struct {
	name, body, link string
	typ              byte
	mode             int64
}

func file(name, body string) entry {
	return entry{name: name, body: body, typ: tar.TypeReg, mode: 0o644}
}
func exe(name, body string) entry {
	return entry{name: name, body: body, typ: tar.TypeReg, mode: 0o755}
}
func dir(name string) entry { return entry{name: name, typ: tar.TypeDir, mode: 0o755} }
func link(name, to string) entry {
	return entry{name: name, link: to, typ: tar.TypeSymlink, mode: 0o777}
}

// globalHeader is the pax global header GitHub's archives open with.
func globalHeader() entry { return entry{name: "pax_global_header", typ: tar.TypeXGlobalHeader} }

// tarball is a gzipped tar of entries.
func tarball(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: e.mode, Linkname: e.link, Format: tar.FormatPAX}
		switch e.typ {
		case tar.TypeReg:
			h.Size = int64(len(e.body))
		case tar.TypeXGlobalHeader:
			h.PAXRecords = map[string]string{"comment": "0123456789abcdef0123456789abcdef01234567"}
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// top is the archive's one top-level directory, as GitHub names it.
const top = "owner-repo-0123456/"

// githubEntries are a well-formed archive: a global header, one top
// directory, nested files, an executable and a link inside the tree.
func githubEntries() []entry {
	return []entry{
		globalHeader(),
		dir(top),
		file(top+"go.mod", "module example.com/repo\n"),
		dir(top + "cmd/"),
		dir(top + "cmd/tool/"),
		file(top+"cmd/tool/main.go", "package main\n"),
		exe(top+"run.sh", "#!/bin/sh\n"),
		link(top+"cmd/README", "../go.mod"),
	}
}

// githubListing is githubEntries extracted: the top directory stripped,
// everything readable by all.
var githubListing = map[string]string{
	"go.mod":           "-rw-r--r--",
	"cmd":              "drwxr-xr-x",
	"cmd/tool":         "drwxr-xr-x",
	"cmd/tool/main.go": "-rw-r--r--",
	"run.sh":           "-rwxr-xr-x",
	"cmd/README":       "Lrwxrwxrwx",
}

// listing is every path below dir, slash-separated, with its mode.
func listing(t *testing.T, dir string) map[string]string {
	t.Helper()
	got := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, _ fs.DirEntry, err error) error {
		if err != nil || p == dir {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		got[filepath.ToSlash(rel)] = info.Mode().String()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// sameListing reports whether got and want hold the same paths and modes.
func sameListing(t *testing.T, got, want map[string]string) {
	t.Helper()
	for p, m := range want {
		if got[p] != m {
			t.Errorf("%s: mode %q, want %q", p, got[p], m)
		}
	}
	for p := range got {
		if _, ok := want[p]; !ok {
			t.Errorf("unexpected %s (%s)", p, got[p])
		}
	}
}
