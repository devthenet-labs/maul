// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package toolexec

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/devthenet-labs/maul/internal/toolwire"
)

// workspace is a workspace Server's directories in a test.
type workspace struct {
	work, home, cache, proc string
}

// newWorkspace returns a workspace Server over a fresh /work-shaped tree,
// its tree seeded, and its sweep confined to a fake proc filesystem that
// only the processes a test registers (registerScript) appear in, so the
// sweep can never reach the test's own processes.
func newWorkspace(t *testing.T) (*Server, workspace) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh on PATH")
	}
	root := t.TempDir()
	ws := workspace{
		work:  filepath.Join(root, "work"),
		home:  filepath.Join(root, "work", "home"),
		cache: filepath.Join(root, "work", "cache"),
		proc:  filepath.Join(root, "proc"),
	}
	for _, d := range []string{filepath.Join(ws.work, "tree"), filepath.Join(ws.work, "bin"), ws.proc} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	s, err := NewServer(Options{Work: ws.work, Home: ws.home, Cache: ws.cache, Procfs: ws.proc})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return s, ws
}

// pathEnv is the request env every workspace test passes: the test host's
// PATH, as tools passes the Repository's search path.
func pathEnv() []EnvVar { return []EnvVar{{Name: "PATH", Value: os.Getenv("PATH")}} }

// envOf parses `env` output into a map.
func envOf(out string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			m[k] = v
		}
	}
	return m
}

// TestWorkspaceEnv: a program's environment is the request's allowlisted
// values, the shim's defaults for what the request leaves out, and the
// shim's own fixed places; the shim's own environment never reaches it.
func TestWorkspaceEnv(t *testing.T) {
	t.Setenv("MAUL_TOOLEXEC_SECRET", "leak")
	t.Setenv("GOPROXY", "https://leak.example")
	s, ws := newWorkspace(t)
	fixed := map[string]string{
		"HOME":             ws.home,
		"TMPDIR":           "/tmp",
		"GOCACHE":          filepath.Join(ws.cache, "go-build"),
		"NPM_CONFIG_CACHE": filepath.Join(ws.cache, "npm"),
		"GOTOOLCHAIN":      "local",
	}
	tests := []struct {
		name string
		req  Request
		want map[string]string
	}{
		{
			name: "an exec with no profile: no network, readonly modules",
			req:  Request{Argv: []string{"env"}, Run: "v1a1t1-abcdef", Env: pathEnv()},
			want: map[string]string{"GOPROXY": "off", "GOFLAGS": "-mod=readonly"},
		},
		{
			name: "a build with no profile: no network, the build may warm the module cache",
			req:  Request{Kind: KindBuild, Argv: []string{"env"}, Env: pathEnv()},
			want: map[string]string{"GOPROXY": "off", "GOFLAGS": "-mod=mod"},
		},
		{
			name: "the profile's values from tools",
			req: Request{Kind: KindBuild, Argv: []string{"env"}, Env: append(pathEnv(),
				EnvVar{Name: "GOPROXY", Value: "http://egress-proxy:8100/go,file:///opt/go/pkg/mod/cache/download"},
				EnvVar{Name: "GONOSUMDB", Value: "maul.invalid"},
				EnvVar{Name: "GOMODCACHE", Value: "/opt/go/pkg/mod"},
				EnvVar{Name: "HTTPS_PROXY", Value: "http://egress-proxy:8100"},
				EnvVar{Name: "NPM_CONFIG_REGISTRY", Value: "http://egress-proxy:8100/r/npm/"},
				EnvVar{Name: "PIP_INDEX_URL", Value: "http://egress-proxy:8100/r/pypi/simple/"},
				EnvVar{Name: "GOFLAGS", Value: "-mod=mod -trimpath"},
			)},
			want: map[string]string{
				"GOPROXY":             "http://egress-proxy:8100/go,file:///opt/go/pkg/mod/cache/download",
				"GONOSUMDB":           "maul.invalid",
				"GOMODCACHE":          "/opt/go/pkg/mod",
				"HTTPS_PROXY":         "http://egress-proxy:8100",
				"NPM_CONFIG_REGISTRY": "http://egress-proxy:8100/r/npm/",
				"PIP_INDEX_URL":       "http://egress-proxy:8100/r/pypi/simple/",
				"GOFLAGS":             "-mod=mod -trimpath",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := s.Do(context.Background(), &tc.req)
			if got.Exit != 0 || got.Refused != "" {
				t.Fatalf("%+v", got)
			}
			env := envOf(got.Output)
			for k, v := range fixed {
				if env[k] != v {
					t.Errorf("%s = %q, want %q", k, env[k], v)
				}
			}
			for k, v := range tc.want {
				if env[k] != v {
					t.Errorf("%s = %q, want %q", k, env[k], v)
				}
			}
			if env["PATH"] != os.Getenv("PATH") {
				t.Errorf("PATH = %q, not the request's", env["PATH"])
			}
			if _, ok := env["MAUL_TOOLEXEC_SECRET"]; ok {
				t.Error("the shim's own environment reached the program")
			}
			allowed := map[string]bool{"PWD": true, "SHLVL": true, "_": true, "OLDPWD": true}
			for k := range fixed {
				allowed[k] = true
			}
			for _, e := range tc.req.Env {
				allowed[e.Name] = true
			}
			for _, k := range []string{"GOPROXY", "GOFLAGS"} {
				allowed[k] = true
			}
			for k := range env {
				if !allowed[k] {
					t.Errorf("%s is in the program's environment, from neither the request nor the shim", k)
				}
			}
		})
	}
}

// TestWorkspaceEnvRefused: a name outside the allowlist, one the shim owns,
// a repeated one, a malformed value or a module cache inside the writable
// cache is refused before anything runs.
func TestWorkspaceEnvRefused(t *testing.T) {
	s, ws := newWorkspace(t)
	marker := filepath.Join(ws.work, "ran")
	tests := []struct {
		name string
		env  []EnvVar
	}{
		{"a name not on the allowlist", []EnvVar{{Name: "LD_PRELOAD", Value: "/x.so"}}},
		{"a name of the shim's own", []EnvVar{{Name: "HOME", Value: "/root"}}},
		{"the build cache", []EnvVar{{Name: "GOCACHE", Value: "/tmp/c"}}},
		{"the toolchain switch", []EnvVar{{Name: "GOTOOLCHAIN", Value: "auto"}}},
		{"TMPDIR", []EnvVar{{Name: "TMPDIR", Value: "/work"}}},
		{"a repeated name", []EnvVar{{Name: "GOPROXY", Value: "off"}, {Name: "GOPROXY", Value: "direct"}}},
		{"a value with a NUL", []EnvVar{{Name: "GOPROXY", Value: "off\x00"}}},
		{"a value too long", []EnvVar{{Name: "GOPROXY", Value: strings.Repeat("a", MaxEnvValueBytes+1)}}},
		{"a relative PATH entry", []EnvVar{{Name: "PATH", Value: "/usr/bin:bin"}}},
		{"an empty name", []EnvVar{{Name: "", Value: "x"}}},
		{"a module cache in the writable cache", []EnvVar{{Name: "GOMODCACHE", Value: filepath.Join(ws.cache, "mod")}}},
		{"the writable cache as module cache", []EnvVar{{Name: "GOMODCACHE", Value: ws.cache}}},
		{"a relative module cache", []EnvVar{{Name: "GOMODCACHE", Value: "mod"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := s.Do(context.Background(), &Request{Kind: KindBuild, Argv: []string{"touch", marker}, Env: tc.env})
			if got.Refused != toolwire.RefusedInvalid {
				t.Fatalf("not refused: %+v", got)
			}
			if _, err := os.Stat(marker); err == nil {
				t.Fatal("the program ran")
			}
		})
	}
}

// TestWorkspaceDirs: a build runs in the tree; an exec in its own run's
// scratch directory, which the shim creates, with its files written there.
func TestWorkspaceDirs(t *testing.T) {
	s, ws := newWorkspace(t)
	got := s.Do(context.Background(), &Request{Kind: KindBuild, Argv: []string{"pwd"}, Env: pathEnv()})
	if want := filepath.Join(ws.work, "tree") + "\n"; got.Output != want {
		t.Errorf("build ran in %q, want %q", got.Output, want)
	}
	got = s.Do(context.Background(), &Request{
		Argv: []string{"sh", "-c", "pwd; cat a/in.txt"}, Run: "v1a1t2-abcdef", Env: pathEnv(),
		Files: []toolwire.File{{Name: "a/in.txt", Content: []byte("hi")}},
	})
	if want := filepath.Join(ws.work, "scratch", "v1a1t2-abcdef") + "\nhi"; got.Output != want {
		t.Errorf("exec output %q, want %q", got.Output, want)
	}
	// A second step of the same run (a run verb's execute after its
	// compile) sees what the first left.
	got = s.Do(context.Background(), &Request{Argv: []string{"cat", "a/in.txt"}, Run: "v1a1t2-abcdef", Env: pathEnv()})
	if got.Output != "hi" {
		t.Errorf("second step output %q", got.Output)
	}
}

// TestWorkspaceConfined: neither a file name nor a symlink a program left
// in the scratch directory places a file outside the run's scratch.
func TestWorkspaceConfined(t *testing.T) {
	s, ws := newWorkspace(t)
	outside := t.TempDir()
	scratch := filepath.Join(ws.work, "scratch")
	if err := os.MkdirAll(filepath.Join(scratch, "run-a"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(scratch, "run-a", "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(scratch, "run-b")); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, run, file string
	}{
		{"a symlink inside the run's scratch", "run-a", "link/x"},
		{"the run's scratch itself a symlink", "run-b", "x"},
		{"a parent path", "run-a", "../x"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := s.Do(context.Background(), &Request{
				Argv: []string{"true"}, Run: tc.run, Env: pathEnv(),
				Files: []toolwire.File{{Name: tc.file, Content: []byte("x")}},
			})
			if got.Refused == "" {
				t.Fatalf("not refused: %+v", got)
			}
			if entries, _ := os.ReadDir(outside); len(entries) != 0 {
				t.Errorf("written outside: %v", entries)
			}
		})
	}
}

// TestWorkspaceRequestRefused: a request the workspace cannot take never
// runs.
func TestWorkspaceRequestRefused(t *testing.T) {
	s, ws := newWorkspace(t)
	marker := filepath.Join(ws.work, "ran")
	touch := []string{"touch", marker}
	tests := []struct {
		name string
		req  Request
	}{
		{"an exec with no run", Request{Argv: touch}},
		{"a run that is not a name", Request{Argv: touch, Run: "../x"}},
		{"a run with a slash", Request{Argv: touch, Run: "a/b"}},
		{"a build with files", Request{Kind: KindBuild, Argv: touch, Files: []toolwire.File{{Name: "x"}}}},
		{"a build with a run", Request{Kind: KindBuild, Argv: touch, Run: "r"}},
		{"a build past the ceiling", Request{Kind: KindBuild, Argv: touch,
			TimeoutMs: (MaxBuildTimeout + time.Millisecond).Milliseconds()}},
		{"a negative timeout", Request{Kind: KindBuild, Argv: touch, TimeoutMs: -1}},
		{"an unknown kind", Request{Kind: "shell", Argv: touch}},
		{"a fresh build", Request{Kind: KindBuild, Argv: touch, Fresh: true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := s.Do(context.Background(), &tc.req)
			if got.Refused != toolwire.RefusedInvalid {
				t.Fatalf("not refused: %+v", got)
			}
			if _, err := os.Stat(marker); err == nil {
				t.Fatal("the program ran")
			}
		})
	}
}

// TestSlotRefusesWorkspace: a slot shim (S1) refuses every workspace field,
// so its behaviour stays exactly the wire's exec.
func TestSlotRefusesWorkspace(t *testing.T) {
	s := newServer(t)
	for _, req := range []Request{
		{Argv: []string{"true"}, Kind: KindBuild},
		{Argv: []string{"true"}, Run: "r"},
		{Argv: []string{"true"}, Env: []EnvVar{{Name: "GOPROXY", Value: "off"}}},
		{Argv: []string{"true"}, TimeoutMs: 1},
		{Argv: []string{"true"}, Fresh: true},
	} {
		if got := s.Do(context.Background(), &req); got.Refused != toolwire.RefusedInvalid {
			t.Errorf("%+v not refused: %+v", req, got)
		}
	}
	if got := s.Do(context.Background(), &Request{Argv: []string{"echo", "ok"}}); got.Output != "ok\n" {
		t.Errorf("a plain exec: %+v", got)
	}
}

// TestWorkspaceExecTimeout: an exec's own timeout bounds its program from
// the program's start, after any restore (a check's timeout, review VC-2),
// and never past the exec bound; a fresh exec, as a check run sends, takes
// it too.
func TestWorkspaceExecTimeout(t *testing.T) {
	s, _ := newWorkspace(t)
	s.timeout = 400 * time.Millisecond
	if got := s.Do(context.Background(), &Request{Kind: KindBuild, Argv: []string{"true"},
		Env: pathEnv()}); got.Exit != 0 || got.KeepFailed {
		t.Fatalf("build: %+v", got)
	}
	tests := []struct {
		name      string
		argv      []string
		timeoutMs int64
		fresh     bool
		timedOut  bool
	}{
		{"within its timeout", []string{"sh", "-c", "sleep 0.1; echo ok"}, 300, false, false},
		{"past its timeout", []string{"sleep", "2"}, 100, false, true},
		{"a timeout past the exec bound keeps the bound", []string{"sleep", "2"}, 5000, false, true},
		{"a fresh exec within its timeout", []string{"sh", "-c", "sleep 0.1; echo ok"}, 300, true, false},
		{"a fresh exec past its timeout", []string{"sleep", "2"}, 100, true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			got := s.Do(context.Background(), &Request{Argv: tc.argv, Run: "r", Env: pathEnv(),
				TimeoutMs: tc.timeoutMs, Fresh: tc.fresh})
			if got.Refused != "" || got.TimedOut != tc.timedOut {
				t.Fatalf("timed out %v, want %v: %+v", got.TimedOut, tc.timedOut, got)
			}
			if took := time.Since(start); tc.timedOut && took > 1500*time.Millisecond {
				t.Errorf("killed after %v", took)
			}
		})
	}
}

// TestWorkspaceBuildTimeout: a build gets the request's timeout, not the
// exec bound, and past it is killed with its group; an exec keeps the
// exec bound.
func TestWorkspaceBuildTimeout(t *testing.T) {
	s, ws := newWorkspace(t)
	s.timeout = 200 * time.Millisecond
	got := s.Do(context.Background(), &Request{Kind: KindBuild, Argv: []string{"sh", "-c", "sleep 0.5; echo built"},
		Env: pathEnv(), TimeoutMs: 5000})
	if got.TimedOut || got.Output != "built\n" {
		t.Errorf("a build within its own timeout: %+v", got)
	}
	pidFile := filepath.Join(ws.work, "child.pid")
	start := time.Now()
	got = s.Do(context.Background(), &Request{Kind: KindBuild,
		Argv: []string{"sh", "-c", "echo before; sleep 30 & echo $! > " + pidFile + "; wait"},
		Env:  pathEnv(), TimeoutMs: 300})
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("took %v", took)
	}
	if !got.TimedOut || got.Exit != -1 || !strings.HasSuffix(got.Output, TimeoutMarker) {
		t.Errorf("a build past its timeout: %+v", got)
	}
	waitGone(t, readPid(t, pidFile))
	got = s.Do(context.Background(), &Request{Argv: []string{"sleep", "2"}, Run: "r", Env: pathEnv()})
	if !got.TimedOut {
		t.Errorf("an exec past the exec bound: %+v", got)
	}
}

// TestBuildTimeoutDefault: a build with no timeout gets the default.
func TestBuildTimeoutDefault(t *testing.T) {
	for _, tc := range []struct {
		ms   int64
		want time.Duration
	}{{0, DefaultBuildTimeout}, {1, time.Millisecond}, {MaxBuildTimeout.Milliseconds(), MaxBuildTimeout}} {
		if got := (&Request{Kind: KindBuild, TimeoutMs: tc.ms}).buildTimeout(); got != tc.want {
			t.Errorf("%d ms: %v, want %v", tc.ms, got, tc.want)
		}
	}
	if DefaultBuildTimeout != 10*time.Minute || MaxBuildTimeout != 20*time.Minute {
		t.Errorf("bounds %v %v", DefaultBuildTimeout, MaxBuildTimeout)
	}
}

// TestWorkspaceBuildTail: a build's output keeps its last
// MaxBuildOutputBytes (where a compiler's errors are), valid UTF-8, marked
// at the front when cut; an exec's keeps the exec bound.
func TestWorkspaceBuildTail(t *testing.T) {
	s, _ := newWorkspace(t)
	tests := []struct {
		name, script string
		cut          bool
	}{
		{"under the bound", "head -c 1000 /dev/zero | tr '\\0' a; echo END", false},
		{"over the bound", "head -c 1000000 /dev/zero | tr '\\0' a; echo END", true},
		{"multibyte across the cut", "yes é | head -c 1000001 | tr -d '\\n'; echo END", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := s.Do(context.Background(), &Request{Kind: KindBuild, Argv: []string{"sh", "-c", tc.script},
				Env: pathEnv()})
			if got.Truncated != tc.cut {
				t.Errorf("truncated %v, want %v", got.Truncated, tc.cut)
			}
			if len(got.Output) > MaxBuildOutputBytes {
				t.Errorf("output %d bytes, over %d", len(got.Output), MaxBuildOutputBytes)
			}
			if !strings.HasSuffix(got.Output, "END\n") {
				t.Errorf("the tail is lost: %q", got.Output[max(0, len(got.Output)-40):])
			}
			if tc.cut != strings.HasPrefix(got.Output, TailMarker) {
				t.Errorf("marker at the front %v, want %v", !tc.cut, tc.cut)
			}
			if !utf8.ValidString(got.Output) || strings.ContainsRune(got.Output, utf8.RuneError) {
				t.Error("output is not clean UTF-8")
			}
			if tc.cut && len(got.Output) < MaxBuildOutputBytes-4 {
				t.Errorf("output %d bytes, the tail is not kept whole", len(got.Output))
			}
		})
	}
	got := s.Do(context.Background(), &Request{Argv: []string{"sh", "-c", "head -c 1000000 /dev/zero | tr '\\0' a"},
		Run: "r", Env: pathEnv()})
	if len(got.Output) != MaxOutputBytes || !got.Truncated || !strings.HasSuffix(got.Output, toolwire.TruncationMarker) {
		t.Errorf("an exec's output: %d bytes, truncated %v", len(got.Output), got.Truncated)
	}
}

// TestHelperProcess is not a test: TestWorkspaceSweep runs this binary
// through it as a program's escaped background listener.
func TestHelperProcess(t *testing.T) {
	args := os.Args
	i := slices.Index(args, "--")
	if i < 0 || len(args) < i+4 || args[i+1] != "listen" {
		return
	}
	addrFile, proc := args[i+2], args[i+3]
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		os.Exit(3)
	}
	pid := strconv.Itoa(os.Getpid())
	// Register in the test's fake proc filesystem, the only one its
	// sweep reads.
	if err := os.Symlink(filepath.Join("/proc", pid), filepath.Join(proc, pid)); err != nil {
		os.Exit(4)
	}
	if err := os.WriteFile(addrFile+".tmp", []byte(pid+" "+ln.Addr().String()), 0o600); err != nil {
		os.Exit(5)
	}
	if err := os.Rename(addrFile+".tmp", addrFile); err != nil {
		os.Exit(6)
	}
	for {
		c, err := ln.Accept()
		if err == nil {
			_ = c.Close()
		}
	}
}

// TestWorkspaceSweep: a program that leaves a listener running in a
// session of its own (out of its process group) leaves nothing alive
// once the call returns: the shim kills every process of its UID.
func TestWorkspaceSweep(t *testing.T) {
	if _, err := os.Stat("/proc/self/status"); err != nil {
		t.Skip("no /proc")
	}
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("no setsid on PATH")
	}
	s, ws := newWorkspace(t)
	addrFile := filepath.Join(ws.work, "listener")
	script := fmt.Sprintf(`setsid %q -test.run='^TestHelperProcess$' -- listen %q %q >/dev/null 2>&1 &
while [ ! -s %q ]; do sleep 0.05; done; echo started`, os.Args[0], addrFile, ws.proc, addrFile)
	for _, kind := range []Kind{KindBuild, KindExec} {
		t.Run(string(kind), func(t *testing.T) {
			_ = os.Remove(addrFile)
			req := Request{Kind: kind, Argv: []string{"sh", "-c", script}, Env: pathEnv()}
			if kind == KindExec {
				req.Run = "sweep"
			}
			got := s.Do(context.Background(), &req)
			if got.Output != "started\n" || got.Unclean {
				t.Fatalf("%+v", got)
			}
			raw, err := os.ReadFile(addrFile)
			if err != nil {
				t.Fatal(err)
			}
			pid, addr, _ := strings.Cut(string(raw), " ")
			if alive(pid) {
				t.Errorf("listener %s is alive after the call returned", pid)
				_ = exec.Command("kill", "-9", pid).Run()
			}
			if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
				_ = c.Close()
				t.Errorf("listener %s still accepts", addr)
			}
		})
	}
}

// TestWorkspaceUnclean: when processes survive the sweep the result says
// so, and the shim runs nothing more until a sweep finds none.
func TestWorkspaceUnclean(t *testing.T) {
	s, ws := newWorkspace(t)
	s.sweep.kill = func(int) error { return nil } // nothing dies
	s.sweep.rounds = 2
	fake := filepath.Join(ws.proc, "4242")
	if err := os.MkdirAll(fake, 0o700); err != nil {
		t.Fatal(err)
	}
	status := fmt.Sprintf("Name:\tsleep\nState:\tS (sleeping)\nUid:\t%d\t%d\t%d\t%d\n", os.Getuid(), os.Getuid(),
		os.Getuid(), os.Getuid())
	if err := os.WriteFile(filepath.Join(fake, "status"), []byte(status), 0o600); err != nil {
		t.Fatal(err)
	}
	got := s.Do(context.Background(), &Request{Argv: []string{"true"}, Run: "r", Env: pathEnv()})
	if !got.Unclean {
		t.Fatalf("not unclean: %+v", got)
	}
	marker := filepath.Join(ws.work, "ran")
	got = s.Do(context.Background(), &Request{Argv: []string{"touch", marker}, Run: "r", Env: pathEnv()})
	if got.Refused != toolwire.RefusedUnavailable {
		t.Fatalf("not refused: %+v", got)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("ran beside a survivor")
	}
	if err := os.RemoveAll(fake); err != nil {
		t.Fatal(err)
	}
	got = s.Do(context.Background(), &Request{Argv: []string{"touch", marker}, Run: "r", Env: pathEnv()})
	if got.Refused != "" || got.Unclean {
		t.Fatalf("once clean: %+v", got)
	}
}

// TestNetError: output naming a bind or connection error is marked, so
// the relay counts the run an error, never an assertion.
func TestNetError(t *testing.T) {
	for _, tc := range []struct {
		out  string
		want bool
	}{
		{"listen tcp 127.0.0.1:8080: bind: address already in use", true},
		{"dial tcp 127.0.0.1:9000: connect: connection refused", true},
		{"Error: connect ECONNREFUSED 127.0.0.1:3000", true},
		{"Error: listen EADDRINUSE: address already in use :::3000", true},
		{"OSError: [Errno 99] Cannot assign requested address", true},
		{"read tcp 127.0.0.1:1->127.0.0.1:2: read: connection reset by peer", true},
		{"dial tcp 10.0.0.1:443: connect: network is unreachable", true},
		{"dial tcp 10.0.0.1:443: connect: no route to host", true},
		{"ok bpm=150", false},
		{"%M stdout: want 3 got 2", false},
		{"", false},
	} {
		if got := NetError(tc.out); got != tc.want {
			t.Errorf("NetError(%q) = %v, want %v", tc.out, got, tc.want)
		}
	}
	s, _ := newWorkspace(t)
	got := s.Do(context.Background(), &Request{Argv: []string{"echo", "bind: address already in use"}, Run: "r",
		Env: pathEnv()})
	if !got.NetError {
		t.Errorf("not marked: %+v", got)
	}
	got = s.Do(context.Background(), &Request{Argv: []string{"echo", "fine"}, Run: "r", Env: pathEnv()})
	if got.NetError {
		t.Errorf("marked: %+v", got)
	}
}

// TestNewServerWorkspace: the workspace's places must be absolute, so the
// container's working directory (an image's WORKDIR, or "/") never
// matters; the scratch directory is under the workspace.
func TestNewServerWorkspace(t *testing.T) {
	root := t.TempDir()
	for _, opts := range []Options{
		{Work: "work", Home: filepath.Join(root, "h"), Cache: filepath.Join(root, "c")},
		{Work: root, Home: "home", Cache: filepath.Join(root, "c")},
		{Work: root, Home: filepath.Join(root, "h"), Cache: "cache"},
		{Work: root, Home: filepath.Join(root, "h"), Cache: filepath.Join(root, "c"), Scratch: root},
		{Work: root, Home: filepath.Join(root, "h"), Cache: filepath.Join(root, "c"), Env: []string{"A=b"}},
	} {
		if _, err := NewServer(opts); err == nil {
			t.Errorf("%+v accepted", opts)
		}
	}
	t.Chdir("/")
	s, err := NewServer(Options{Work: filepath.Join(root, "w"), Home: filepath.Join(root, "w", "home"),
		Cache: filepath.Join(root, "w", "cache")})
	if err != nil {
		t.Fatalf("NewServer from /: %v", err)
	}
	for _, d := range []string{"scratch", "home", "cache"} {
		if fi, err := os.Stat(filepath.Join(root, "w", d)); err != nil || !fi.IsDir() {
			t.Errorf("%s not created: %v", d, err)
		}
	}
	_ = s
}

// readPid reads a pid a test's program wrote.
func readPid(t *testing.T, file string) string {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(raw))
}

// waitGone waits for pid to be gone.
func waitGone(t *testing.T, pid string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for alive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("process %s outlived its program", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestWorkspaceSearchPath: a program named without a directory is found on
// the request's PATH (the Repository's search path), never the shim's own,
// which on a repository image may differ.
func TestWorkspaceSearchPath(t *testing.T) {
	s, _ := newWorkspace(t)
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on PATH")
	}
	onlyRequest, onlyShim := t.TempDir(), t.TempDir()
	for dir, word := range map[string]string{onlyRequest: "request", onlyShim: "shim"} {
		script := "#!" + sh + "\necho " + word + "\n"
		if err := os.WriteFile(filepath.Join(dir, "maul-probe-"+word), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", onlyShim+string(filepath.ListSeparator)+os.Getenv("PATH"))
	env := []EnvVar{{Name: "PATH", Value: onlyRequest}}

	res := s.Do(context.Background(), &Request{Argv: []string{"maul-probe-request"}, Run: "r1", Env: env})
	if res.Refused != "" || res.Exit != 0 || strings.TrimSpace(res.Output) != "request" {
		t.Errorf("request PATH: got %+v, want the program found on the request's PATH", res)
	}
	res = s.Do(context.Background(), &Request{Argv: []string{"maul-probe-shim"}, Run: "r2", Env: env})
	if res.Refused != "" || res.Exit != 127 {
		t.Errorf("shim PATH: got %+v, want exit 127 (not on the request's PATH)", res)
	}
	res = s.Do(context.Background(), &Request{Argv: []string{"maul-probe-shim"}, Kind: KindBuild, Env: env})
	if res.Refused != "" || res.Exit != 127 {
		t.Errorf("build, shim PATH: got %+v, want exit 127", res)
	}
}
