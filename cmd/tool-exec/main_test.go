// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/devthenet-labs/maul/internal/toolexec"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// TestRun: the binary serves on its socket, hands programs only the
// environment it passes on, and exits 0 when told to stop.
func TestRun(t *testing.T) {
	dir, err := os.MkdirTemp("", "tool-exec")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "sock")
	scratch := filepath.Join(dir, "scratch")
	env := map[string]string{"PATH": os.Getenv("PATH"), "LANG": "C.UTF-8", "MAUL_SECRET": "leak"}
	getenv := func(k string) string { return env[k] }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stderr bytes.Buffer
	done := make(chan int)
	go func() { done <- run(ctx, []string{"--socket", sock, "--scratch", scratch}, getenv, &stderr) }()

	c := toolexec.NewClient(sock)
	deadline := time.Now().Add(5 * time.Second)
	for c.Ready(ctx) != nil {
		if time.Now().After(deadline) {
			t.Fatal("never ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
	res, err := c.Exec(ctx, &toolwire.ExecRequest{Argv: []string{"sh", "-c", `echo "$LANG|$MAUL_SECRET|$HOME"`}})
	if err != nil {
		t.Fatal(err)
	}
	if want := "C.UTF-8||" + scratch + "\n"; res.Output != want {
		t.Errorf("output %q, want %q", res.Output, want)
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit %d; stderr %s", code, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not stop")
	}
}

// TestRunRefusesArgs: a flag it does not know is exit 2.
func TestRunRefusesArgs(t *testing.T) {
	var stderr bytes.Buffer
	if code := run(context.Background(), []string{"--shell"}, func(string) string { return "" }, &stderr); code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "shell") {
		t.Errorf("stderr %q", stderr.String())
	}
}

// TestRunWorkspace: with --workdir the binary is a workspace shim: the
// programs' environment comes only from each request and the shim's own
// places (home and cache under the workspace unless named), never from the
// shim's environment; and the sweep reads only its proc filesystem.
func TestRunWorkspace(t *testing.T) {
	dir, err := os.MkdirTemp("", "tool-exec")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "sock")
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(filepath.Join(work, "tree"), 0o700); err != nil {
		t.Fatal(err)
	}
	fakeProc(t)
	env := map[string]string{"PATH": "/leak", "LANG": "C.UTF-8", "GOPROXY": "https://leak.example",
		"MAUL_SECRET": "leak"}
	getenv := func(k string) string { return env[k] }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stderr bytes.Buffer
	done := make(chan int)
	go func() { done <- run(ctx, []string{"--socket", sock, "--workdir", work}, getenv, &stderr) }()

	c := toolexec.NewClient(sock)
	deadline := time.Now().Add(5 * time.Second)
	for c.Ready(ctx) != nil {
		if time.Now().After(deadline) {
			t.Fatalf("never ready: %s", stderr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	res, err := c.Do(ctx, &toolexec.Request{
		Kind: toolexec.KindBuild,
		Argv: []string{"sh", "-c", `echo "$PWD|$PATH|$LANG|$GOPROXY|$MAUL_SECRET|$HOME|$GOCACHE"`},
		Env:  []toolexec.EnvVar{{Name: "PATH", Value: os.Getenv("PATH")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{filepath.Join(work, "tree"), os.Getenv("PATH"), "", "off", "",
		filepath.Join(work, "home"), filepath.Join(work, "cache", "go-build")}, "|") + "\n"
	if res.Output != want {
		t.Errorf("output %q, want %q", res.Output, want)
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit %d; stderr %s", code, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not stop")
	}
}

// TestRunRefuses: a workspace with a scratch directory or a relative
// place is a bad flag, and the shim never serves as root.
func TestRunRefuses(t *testing.T) {
	dir := t.TempDir()
	fakeProc(t)
	tests := []struct {
		name string
		args []string
		uid  int
		want int
	}{
		{"a workspace and a scratch directory", []string{"--workdir", dir, "--scratch", dir}, 1000, 2},
		{"a relative workspace", []string{"--workdir", "work"}, 1000, 2},
		{"a relative home", []string{"--workdir", dir, "--home", "home"}, 1000, 2},
		{"a home without a workspace", []string{"--home", dir}, 1000, 2},
		{"a slot shim as root", []string{"--scratch", filepath.Join(dir, "s")}, 0, 1},
		{"a workspace shim as root", []string{"--workdir", dir}, 0, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			old := getuid
			getuid = func() int { return tc.uid }
			defer func() { getuid = old }()
			var stderr bytes.Buffer
			if code := run(context.Background(), tc.args, func(string) string { return "" }, &stderr); code != tc.want {
				t.Errorf("exit %d, want %d; stderr %s", code, tc.want, stderr.String())
			}
		})
	}
}

// fakeProc points the sweep at an empty proc filesystem for the test, so
// it can never reach the test's own processes.
func fakeProc(t *testing.T) {
	t.Helper()
	old := procfs
	procfs = t.TempDir()
	t.Cleanup(func() { procfs = old })
}
