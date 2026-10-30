// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package toolexec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/devthenet-labs/maul/internal/toolwire"
)

// The shim's fixed places and bounds.
const (
	// SocketPath is the shim's socket in the emptyDir tools and sandbox
	// share.
	SocketPath = "/run/maul-exec/sock"
	// ScratchDir is where request files are written and programs run.
	ScratchDir = "/work/scratch"
	// Timeout bounds one program (DESIGN 5: a sandbox program at most 60 s).
	Timeout = 60 * time.Second
	// MaxOutputBytes bounds a Result's Output, markers included (DESIGN 5:
	// at most 64 KiB output).
	MaxOutputBytes = 64 << 10
	// waitDelay bounds how long a killed or finished program's leftover
	// processes may hold its output open.
	waitDelay = 2 * time.Second
)

// TimeoutMarker ends the output of a program killed at its time limit.
const TimeoutMarker = "\n[maul: killed at the time limit]"

// Result is one exec's outcome. The tools container records it; Output is
// the program's, so untrusted.
type Result struct {
	// Exit is the program's exit code: -1 when a signal ended it, 127 when
	// it could not be found, 126 when it could not be started.
	Exit int `json:"exit"`
	// Output is stdout and stderr together, in order, bounded and valid
	// UTF-8.
	Output     string `json:"output"`
	Truncated  bool   `json:"truncated,omitempty"`
	TimedOut   bool   `json:"timedOut,omitempty"`
	DurationMs int64  `json:"durationMs"`
	// Refused is a toolwire refusal code when nothing ran; Reason is the
	// shim's words for it.
	Refused string `json:"refused,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// Options configures a Server.
type Options struct {
	// Scratch is the scratch directory; ScratchDir when empty. It is
	// created if missing.
	Scratch string
	// Env is the programs' environment ("NAME=value"); HOME is the scratch
	// directory unless Env names one.
	Env []string
}

// Server runs exec requests, one at a time.
type Server struct {
	scratch string
	env     []string
	timeout time.Duration
	// slot holds the one running program's place.
	slot chan struct{}
}

// NewServer returns a Server over opts, its scratch directory created.
func NewServer(opts Options) (*Server, error) {
	scratch := opts.Scratch
	if scratch == "" {
		scratch = ScratchDir
	}
	scratch, err := filepath.Abs(scratch)
	if err != nil {
		return nil, fmt.Errorf("toolexec: scratch directory: %w", err)
	}
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		return nil, fmt.Errorf("toolexec: create scratch directory: %w", err)
	}
	env := append([]string(nil), opts.Env...)
	if !hasName(env, "HOME") {
		env = append(env, "HOME="+scratch)
	}
	return &Server{scratch: scratch, env: env, timeout: Timeout, slot: make(chan struct{}, 1)}, nil
}

func hasName(env []string, name string) bool {
	for _, kv := range env {
		if strings.HasPrefix(kv, name+"=") {
			return true
		}
	}
	return false
}

// Run checks req, writes its files and runs its argv. A request that is
// not one of the wire, or a file that cannot be written under scratch, is
// refused before anything runs; so is a request still waiting for the
// program before it when ctx ends.
func (s *Server) Run(ctx context.Context, req *toolwire.ExecRequest) Result {
	if err := req.Check(); err != nil {
		return refused(toolwire.RefusedInvalid, err.Error())
	}
	select {
	case s.slot <- struct{}{}:
	case <-ctx.Done():
		return refused(toolwire.RefusedUnavailable, "another program is still running")
	}
	defer func() { <-s.slot }()
	if err := s.writeFiles(req.Files); err != nil {
		return refused(toolwire.RefusedInvalid, err.Error())
	}
	return s.run(ctx, req.Argv)
}

func refused(code, reason string) Result {
	return Result{Exit: -1, Refused: code, Reason: reason}
}

// writeFiles writes each file under scratch through an os.Root, so neither
// a name nor a symlink already there can place one outside it.
func (s *Server) writeFiles(files []toolwire.File) error {
	if len(files) == 0 {
		return nil
	}
	root, err := os.OpenRoot(s.scratch)
	if err != nil {
		return fmt.Errorf("scratch directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	for _, f := range files {
		if dir := filepath.Dir(f.Name); dir != "." {
			if err := root.MkdirAll(dir, 0o700); err != nil {
				return fmt.Errorf("file %q cannot be written under scratch", f.Name)
			}
		}
		if err := root.WriteFile(f.Name, f.Content, 0o700); err != nil {
			return fmt.Errorf("file %q cannot be written under scratch", f.Name)
		}
	}
	return nil
}

// run runs argv in its own process group, killing the group at the time
// limit and again once the program has exited.
func (s *Server) run(ctx context.Context, argv []string) Result {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	out := &capWriter{limit: MaxOutputBytes}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // running argv is the shim's job
	cmd.Dir = s.scratch
	cmd.Env = s.env
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.WaitDelay = waitDelay
	ownGroup(cmd)
	start := time.Now()
	err := cmd.Start()
	if err != nil {
		exit := 126
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			exit = 127
		}
		return Result{Exit: exit, Output: fmt.Sprintf("[maul: could not start %s: %v]", argv[0], err),
			DurationMs: time.Since(start).Milliseconds()}
	}
	_ = cmd.Wait()
	killGroup(cmd)
	timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)
	raw, overflow := out.contents()
	output, truncated := finish(raw, overflow, timedOut)
	return Result{
		Exit:       cmd.ProcessState.ExitCode(),
		Output:     output,
		Truncated:  truncated,
		TimedOut:   timedOut,
		DurationMs: time.Since(start).Milliseconds(),
	}
}

// finish makes raw a bounded, valid UTF-8 output: cut on a rune boundary
// and marked when it does not fit, the timeout marker after it.
func finish(raw []byte, overflow, timedOut bool) (string, bool) {
	suffix := ""
	if timedOut {
		suffix = TimeoutMarker
	}
	room := MaxOutputBytes - len(suffix)
	v := strings.ToValidUTF8(string(raw), string(utf8.RuneError))
	truncated := overflow || len(v) > room
	if truncated {
		cut := room - len(toolwire.TruncationMarker)
		if cut > len(v) {
			cut = len(v)
		}
		for cut > 0 && !utf8.RuneStart(v[cut]) {
			cut--
		}
		v = v[:cut] + toolwire.TruncationMarker
	}
	return v + suffix, truncated
}

// capWriter keeps the first limit bytes written to it and notes that more
// came; it never refuses a write, so a program is never blocked on it.
type capWriter struct {
	mu       sync.Mutex
	limit    int
	buf      []byte
	overflow bool
}

func (w *capWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	room := w.limit - len(w.buf)
	if len(p) > room {
		w.overflow = true
		w.buf = append(w.buf, p[:room]...)
		return len(p), nil
	}
	w.buf = append(w.buf, p...)
	return len(p), nil
}

// contents is what was kept and whether more came.
func (w *capWriter) contents() ([]byte, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf, w.overflow
}
