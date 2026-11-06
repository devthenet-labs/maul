// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/devthenet-labs/maul/internal/toolexec"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// TestRenderConsumer: maul writes the consumer module itself (SPEC Q25,
// amended): a go.mod naming only maulcheck at the tree's go version (the
// go command then makes the workspace of it and the side's writable tree),
// so the tree's own go.mod, go.sum and replaces are the build's, unchanged. A
// vendored tree, a replace pointing outside the tree, or a go.mod maul
// cannot read refuses the run, by name.
func TestRenderConsumer(t *testing.T) {
	tests := []struct {
		name   string
		files  map[string]string
		gomod  string
		reason string
	}{
		{"plain", map[string]string{"go.mod": treeGoMod}, "module maulcheck\n\ngo 1.24\n", ""},
		{"toolchain and patch version",
			map[string]string{"go.mod": "module example.com/bb\n\ngo 1.26.10\n\ntoolchain go1.26.10\n"},
			"module maulcheck\n\ngo 1.26.10\n", ""},
		{"no go line", map[string]string{"go.mod": "module example.com/bb\n"}, "module maulcheck\n", ""},
		{"a replace inside the tree", map[string]string{"go.mod": treeGoMod + "replace example.com/sub => ./sub\n"},
			"module maulcheck\n\ngo 1.24\n", ""},
		{"a module replace",
			map[string]string{"go.mod": treeGoMod + "replace golang.org/x/mod => golang.org/x/mod v0.40.0\n"},
			"module maulcheck\n\ngo 1.24\n", ""},
		{"a replace above the tree", map[string]string{"go.mod": treeGoMod + "replace example.com/sub => ../sub\n"},
			"", ReasonReplaceOutsideTree},
		{"a replace sneaking above the tree",
			map[string]string{"go.mod": treeGoMod + "replace example.com/sub => ./a/../../sub\n"},
			"", ReasonReplaceOutsideTree},
		{"an absolute replace", map[string]string{"go.mod": treeGoMod + "replace example.com/sub => /work/scratch/x\n"},
			"", ReasonReplaceOutsideTree},
		{"vendored", map[string]string{"go.mod": treeGoMod, "vendor/modules.txt": "# x\n"}, "", ReasonVendoredTree},
		{"no go.mod", map[string]string{"main.go": "package main\n"}, "", ReasonNoModule},
		{"a go.mod maul cannot parse", map[string]string{"go.mod": "module\n"}, "", ReasonNoModule},
		{"a tree named maulcheck", map[string]string{"go.mod": "module maulcheck\n"}, "", ReasonNoModule},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeTree(t, dir, tt.files)
			c, err := renderConsumer(dir, "/work/tree")
			if tt.reason != "" {
				if err == nil || !strings.HasPrefix(err.Error(), tt.reason) {
					t.Fatalf("renderConsumer = %+v, %v; want %s", c, err, tt.reason)
				}
				return
			}
			if err != nil || string(c.goMod) != tt.gomod {
				t.Fatalf("renderConsumer = %q, %v", c.goMod, err)
			}
		})
	}
}

// TestRenderConsumerSymlinkedGoMod: a go.mod that is a symlink out of the
// tree is not read.
func TestRenderConsumerSymlinkedGoMod(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "outside.mod"), []byte("module evil.example/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tree := filepath.Join(root, "tree")
	if err := os.MkdirAll(tree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "outside.mod"), filepath.Join(tree, "go.mod")); err != nil {
		t.Fatal(err)
	}
	if _, err := renderConsumer(tree, "/work/tree"); err == nil {
		t.Fatal("a go.mod outside the tree was read")
	}
}

// liveWorkspace is a real workspace shim over a real tree, its sweep on a
// fake proc filesystem so it can never reach the test's own processes.
type liveWorkspace struct {
	work, trees string
	client      *toolexec.Client
}

// sumLines are the repository's go.sum lines for module at version, so the
// tree's build never asks the checksum database.
func sumLines(t *testing.T, module, version string) string {
	t.Helper()
	f, err := os.Open("../../go.sum")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var b strings.Builder
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), module+" "+version+" ") || strings.HasPrefix(sc.Text(), module+" "+version+"/") {
			b.WriteString(sc.Text() + "\n")
		}
	}
	if b.Len() == 0 {
		t.Skipf("go.sum has no %s %s", module, version)
	}
	return b.String()
}

// newLiveWorkspace seeds a tree requiring golang.org/x/mod (a dependency of
// this repository, so in the module cache) at both a pristine and a
// writable place, and serves a real workspace shim on a unix socket.
func newLiveWorkspace(t *testing.T) liveWorkspace {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go on PATH")
	}
	short, err := os.MkdirTemp("", "pt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(short) })
	root := t.TempDir()
	lw := liveWorkspace{work: filepath.Join(root, "work"), trees: filepath.Join(root, "trees")}
	const version = "v0.41.0"
	tree := map[string]string{
		"go.mod": "module example.com/bb\n\ngo 1.24\n\nrequire golang.org/x/mod " + version + "\n",
		"go.sum": sumLines(t, "golang.org/x/mod", version),
		"bb.go": "package bb\n\nimport \"golang.org/x/mod/modfile\"\n\n" +
			"// Module is a go.mod's module path.\nfunc Module(b []byte) string { return modfile.ModulePath(b) }\n" +
			"\ntype T struct{ A int }\n",
	}
	for _, d := range []string{filepath.Join(lw.trees, "head"), filepath.Join(lw.trees, "base"),
		filepath.Join(lw.work, "tree")} {
		writeTree(t, d, tree)
	}
	for _, d := range []string{filepath.Join(lw.work, "bin"), filepath.Join(root, "proc")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	s, err := toolexec.NewServer(toolexec.Options{Work: lw.work, Home: filepath.Join(lw.work, "home"),
		Cache: filepath.Join(lw.work, "cache"), Procfs: filepath.Join(root, "proc")})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	sock := filepath.Join(short, "sock")
	ln, err := toolexec.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	lw.client = toolexec.NewClient(sock)
	return lw
}

func liveConfig(t *testing.T) Config {
	t.Helper()
	modcache, err := exec.Command("go", "env", "GOMODCACHE").Output()
	if err != nil {
		t.Skip("go env failed")
	}
	cfg := wsConfig()
	cfg.Workspace.Build, cfg.Workspace.Binary, cfg.Workspace.Language = nil, "", "go"
	cfg.Workspace.SearchPath = filepath.Dir(must(exec.LookPath("go")))
	cfg.Workspace.Env = []toolexec.EnvVar{{Name: "GOMODCACHE", Value: strings.TrimSpace(string(modcache))}}
	return cfg
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func fileSums(t *testing.T, dir string, names ...string) map[string][32]byte {
	t.Helper()
	out := map[string][32]byte{}
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			t.Fatal(err)
		}
		out[n] = sha256.Sum256(b)
	}
	return out
}

// TestRunGoLive drives the run verb end to end through a real workspace
// shim and the real go command (SPEC Q25, SP-8): the consumer compiles
// offline against the tree's own module graph, after the tree's build, the
// tree's go.mod and go.sum come out byte-identical, and each kind of
// compile failure is shown as the compiler's, the run never executed.
func TestRunGoLive(t *testing.T) {
	if testing.Short() {
		t.Skip("builds with the go command")
	}
	lw := newLiveWorkspace(t)
	cfg := liveConfig(t)
	h := newHarness(t, cfg, routes{}, func(o *Options) {
		o.Head, o.Base, o.Trees, o.Key, o.WorkTree = lw.client, lw.client, lw.trees, wsKey, filepath.Join(lw.work, "tree")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	h.relay.buildTrees(ctx)
	before := fileSums(t, filepath.Join(lw.work, "tree"), "go.mod", "go.sum")
	run := func(src string) toolwire.Response {
		t.Helper()
		_, resp := h.callKeyed(toolwire.VerbRun, mustJSON(t, toolwire.RunRequest{Language: "go",
			Files: []toolwire.File{{Name: "main.go", Content: []byte(src)}}, Args: []string{"go.mod"}}))
		return resp
	}
	resp := run("package main\n\nimport (\n\t\"fmt\"\n\t\"os\"\n\n\t\"example.com/bb\"\n)\n\n" +
		"func main() {\n\tb, _ := os.ReadFile(\"go.mod\")\n\tfmt.Println(bb.Module(b), len(os.Args))\n}\n")
	if resp.Refused != "" || resp.Observed == nil || *resp.Observed.Exit != 0 ||
		!strings.Contains(resp.Untrusted, "maulcheck 2") {
		t.Fatalf("a consumer of the tree: %+v", resp)
	}
	for _, tt := range []struct{ src, says string }{
		{"package main\n\nimport \"example.com/bb\"\n\nfunc main() { bb.Parse() }\n", "undefined: bb.Parse"},
		{"package main\n\nimport \"nosuchstd\"\n\nfunc main() { nosuchstd.X() }\n", "is not in std"},
		{"package main\n\nimport \"example.com/other/x\"\n\nfunc main() { x.X() }\n", "module providing package"},
		{"package main\n\nimport \"example.com/bb\"\n\nfunc main() { _ = bb.T{}.B }\n", "has no field or method"},
	} {
		resp := run(tt.src)
		if resp.Refused != "" || resp.Observed == nil || *resp.Observed.Exit == 0 ||
			!strings.Contains(resp.Untrusted, tt.says) {
			t.Errorf("%q: %+v", tt.says, resp)
		}
		if !strings.Contains(resp.Reason, "did not compile") {
			t.Errorf("%q: reason %q", tt.says, resp.Reason)
		}
	}
	after := fileSums(t, filepath.Join(lw.work, "tree"), "go.mod", "go.sum")
	if before["go.mod"] != after["go.mod"] || before["go.sum"] != after["go.sum"] {
		t.Fatal("the consumer's compile changed the tree's go.mod or go.sum")
	}
	// A run check's compile failure is an error, never a fail.
	check := `{"id":"bb.parse","outcome":"bb","form":"run","run":{"language":"go","files":[
 {"path":"main.go","content":
  "package main\n\nimport (\"fmt\"; \"example.com/bb\")\n\nfunc main() { fmt.Println(bb.Parse()) }\n"}],
 "expect":{"exit":0,"stdoutContains":"true"}}}`
	_, cr := h.callKeyed(toolwire.VerbCheckRun, mustJSON(t, map[string]json.RawMessage{"check": json.RawMessage(check)}))
	if cr.Refused != "" || cr.Observed.CheckRun.Result != toolwire.RunError {
		t.Fatalf("a run check that does not compile: %+v", cr)
	}
	l := h.finishKeyed()
	if b := buildEntries(t, l); b[""].Result != "built" {
		t.Fatalf("the live tree did not build: %+v\n%s", b, l.Entries[0].Output)
	}
}

// TestExecSocketDirsUntouched: the relay does nothing under a sandbox's
// exec-socket directory but dial its socket. The directories are 0777
// emptyDirs a sandbox can plant files and symlinks in.
func TestExecSocketDirsUntouched(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, side := range []string{"head", "base"} {
		d := filepath.Join(dir, side)
		if err := os.MkdirAll(d, 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(d, "sock")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "planted"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	snap := func() string {
		var b bytes.Buffer
		_ = filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			b.WriteString(p + " " + fi.Mode().String() + " " + fi.ModTime().String() + "\n")
			return nil
		})
		data, _ := os.ReadFile(target)
		b.Write(data)
		return b.String()
	}
	was := snap()
	head, base := NewSandboxes(dir)
	h := newHarness(t, wsConfig(), routes{}, func(o *Options) {
		o.Head, o.Base, o.Trees, o.Key, o.ReadyWait = head, base, t.TempDir(), wsKey, 100*time.Millisecond
	})
	h.relay.buildTrees(context.Background())
	h.callKeyed(toolwire.VerbExec, `{"argv":["ls"]}`)
	h.finishKeyed()
	if now := snap(); now != was {
		t.Fatalf("the exec-socket directories changed:\nwas %s\nnow %s", was, now)
	}
	if err := head.Ready(context.Background()); err == nil {
		t.Fatal("a symlink to a file answered as a shim")
	}
}
