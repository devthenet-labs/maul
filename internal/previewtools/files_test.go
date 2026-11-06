// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/quick"

	"github.com/devthenet-labs/maul/internal/toolwire"
)

// secret is what sits outside the pristine trees: no files call may show it.
const secret = "OUTSIDE-THE-TREE-7f3a"

// filesTree is a pristine tree with an outside secret and symlinks that
// try to reach it.
func filesTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "secret.txt"), []byte(secret), 0o644); err != nil {
		t.Fatal(err)
	}
	head := filepath.Join(root, "trees", "head")
	writeTree(t, head, map[string]string{
		"go.mod":             treeGoMod,
		"cmd/bb/main.go":     "package main\n\nfunc main() { println(\"count\") }\n",
		"internal/x/x.go":    "package x\n\n// Count counts.\nfunc Count() int { return 3 }\n",
		"bin.dat":            "a\x00b count",
		"docs/notes.md":      "count me\nand me: count\n",
		"internal/x/deep.go": "package x\n",
	})
	writeTree(t, filepath.Join(root, "trees", "base"), map[string]string{"go.mod": treeGoMod, "base.go": "package bb\n"})
	for name, target := range map[string]string{
		"escape":           filepath.Join(root, "secret.txt"),
		"up":               "../../secret.txt",
		"internal/updir":   "../../..",
		"inside":           "go.mod",
		"docs/outside.txt": filepath.Join(root, "secret.txt"),
	} {
		if err := os.Symlink(target, filepath.Join(head, name)); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(root, "trees")
}

func filesHarness(t *testing.T) *harness {
	t.Helper()
	trees := filesTree(t)
	h := newHarness(t, wsConfig(), routes{}, func(o *Options) {
		o.Head, o.Base, o.Trees, o.Key = &fakeSandbox{}, &fakeSandbox{}, trees, wsKey
	})
	return h
}

func TestFilesVerb(t *testing.T) {
	h := filesHarness(t)
	tests := []struct {
		name     string
		req      toolwire.FilesRequest
		refused  string
		contains []string
		absent   []string
	}{
		{"ls the root", toolwire.FilesRequest{Op: toolwire.OpLs}, "",
			[]string{"cmd/\n", "go.mod\n", "internal/\n", "escape@\n"}, []string{secret}},
		{"ls a directory", toolwire.FilesRequest{Op: toolwire.OpLs, Path: "internal/x"}, "",
			[]string{"x.go\n", "deep.go\n"}, nil},
		{"ls the base", toolwire.FilesRequest{Op: toolwire.OpLs, Side: toolwire.SideBase}, "",
			[]string{"base.go\n"}, []string{"cmd/"}},
		{"read", toolwire.FilesRequest{Op: toolwire.OpRead, Path: "internal/x/x.go"}, "",
			[]string{"func Count() int"}, nil},
		{"read through an inside symlink", toolwire.FilesRequest{Op: toolwire.OpRead, Path: "inside"}, "",
			[]string{"module example.com/bb"}, nil},
		{"read an absolute escape", toolwire.FilesRequest{Op: toolwire.OpRead, Path: "escape"},
			toolwire.RefusedInvalid, nil, []string{secret}},
		{"read a relative escape", toolwire.FilesRequest{Op: toolwire.OpRead, Path: "up"},
			toolwire.RefusedInvalid, nil, []string{secret}},
		{"read through an escaping directory", toolwire.FilesRequest{Op: toolwire.OpRead, Path: "internal/updir/secret.txt"},
			toolwire.RefusedInvalid, nil, []string{secret}},
		{"read a directory", toolwire.FilesRequest{Op: toolwire.OpRead, Path: "cmd"}, toolwire.RefusedInvalid, nil, nil},
		{"read a missing file", toolwire.FilesRequest{Op: toolwire.OpRead, Path: "nope.go"}, toolwire.RefusedInvalid,
			nil, nil},
		{"grep is a literal substring", toolwire.FilesRequest{Op: toolwire.OpGrep, Pattern: "Count()"}, "",
			[]string{"internal/x/x.go:4:func Count() int { return 3 }"}, nil},
		{"grep a regexp is literal", toolwire.FilesRequest{Op: toolwire.OpGrep, Pattern: "Coun.*"}, "",
			nil, []string{"x.go"}},
		{"grep skips binary files and symlinks", toolwire.FilesRequest{Op: toolwire.OpGrep, Pattern: "count"}, "",
			[]string{"docs/notes.md:1:count me", "docs/notes.md:2:and me: count", "cmd/bb/main.go:3:"},
			[]string{"bin.dat", secret, "outside.txt"}},
		{"grep under a path", toolwire.FilesRequest{Op: toolwire.OpGrep, Path: "docs", Pattern: "count"}, "",
			[]string{"docs/notes.md:1:"}, []string{"main.go"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, resp := h.callKeyed(toolwire.VerbFiles, mustJSON(t, tt.req))
			if resp.Refused != tt.refused {
				t.Fatalf("refused %q (%s), want %q", resp.Refused, resp.Reason, tt.refused)
			}
			for _, s := range tt.contains {
				if !strings.Contains(resp.Untrusted, s) {
					t.Errorf("output lacks %q:\n%s", s, resp.Untrusted)
				}
			}
			for _, s := range append(tt.absent, secret) {
				if strings.Contains(resp.Untrusted, s) || strings.Contains(resp.Reason, s) {
					t.Errorf("output shows %q:\n%s", s, resp.Untrusted)
				}
			}
		})
	}
	l := h.finishKeyed()
	for _, e := range l.Entries {
		if strings.Contains(e.Output, secret) {
			t.Errorf("entry %d records the secret", e.Seq)
		}
		if e.Verb == toolwire.VerbFiles && e.Kind != toolwire.KindAgent {
			t.Errorf("entry %d: kind %s", e.Seq, e.Kind)
		}
	}
}

// TestFilesBounds: a read is cut at MaxFileReadBytes, a grep at
// MaxGrepHits, an ls at MaxListEntries.
func TestFilesBounds(t *testing.T) {
	root := t.TempDir()
	head := filepath.Join(root, "head")
	files := map[string]string{"big.txt": strings.Repeat("x", toolwire.MaxFileReadBytes+100),
		"hits.txt": strings.Repeat("hit\n", toolwire.MaxGrepHits+50)}
	for i := range toolwire.MaxListEntries + 10 {
		files[fmt.Sprintf("many/f%05d", i)] = ""
	}
	writeTree(t, head, files)
	if err := os.MkdirAll(filepath.Join(root, "base"), 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := NewRelay(Options{Config: wsConfig(), Out: &strings.Builder{}, Client: NewClient(ClientOptions{}),
		Head: &fakeSandbox{}, Base: &fakeSandbox{}, Trees: root, Key: wsKey})
	if err != nil {
		t.Fatal(err)
	}
	read := r.files(&toolwire.FilesRequest{Op: toolwire.OpRead, Path: "big.txt"})
	if len(read.output) > toolwire.MaxFileReadBytes+len(toolwire.TruncationMarker) ||
		!strings.HasSuffix(read.output, toolwire.TruncationMarker) {
		t.Fatalf("read of %d bytes", len(read.output))
	}
	grep := r.files(&toolwire.FilesRequest{Op: toolwire.OpGrep, Pattern: "hit"})
	if n := strings.Count(grep.output, "hits.txt:"); n != toolwire.MaxGrepHits {
		t.Fatalf("grep returned %d hits", n)
	}
	ls := r.files(&toolwire.FilesRequest{Op: toolwire.OpLs, Path: "many"})
	if n := strings.Count(ls.output, "\nf") + 1; n != toolwire.MaxListEntries {
		t.Fatalf("ls returned %d entries", n)
	}
}

// TestFilesPathHygieneProperty: whatever path a files call names, and
// whatever symlinks the tree holds, nothing outside the tree is shown.
func TestFilesPathHygieneProperty(t *testing.T) {
	trees := filesTree(t)
	r, err := NewRelay(Options{Config: wsConfig(), Out: &strings.Builder{}, Client: NewClient(ClientOptions{}),
		Head: &fakeSandbox{}, Base: &fakeSandbox{}, Trees: trees, Key: wsKey})
	if err != nil {
		t.Fatal(err)
	}
	segs := []string{"..", ".", "", "escape", "up", "internal", "updir", "secret.txt", "docs", "outside.txt", "go.mod",
		"inside", "/", "x", "%2e%2e", "~"}
	prop := func(seed uint64) bool {
		rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
		n := 1 + rng.IntN(6)
		parts := make([]string, n)
		for i := range parts {
			parts[i] = segs[rng.IntN(len(segs))]
		}
		ops := []toolwire.Op{toolwire.OpLs, toolwire.OpRead, toolwire.OpGrep}
		req := toolwire.FilesRequest{Op: ops[rng.IntN(len(ops))], Path: strings.Join(parts, "/")}
		if req.Op == toolwire.OpGrep {
			req.Pattern = "OUTSIDE"
		}
		if req.Check() != nil {
			return true // the wire refuses it before the relay sees it
		}
		res := r.files(&req)
		return !strings.Contains(res.output, secret) && !strings.Contains(res.reason, secret)
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 2000}); err != nil {
		t.Fatal(err)
	}
}
