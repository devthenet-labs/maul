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
