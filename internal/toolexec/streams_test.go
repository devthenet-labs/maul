// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package toolexec

import (
	"context"
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/toolwire"
)

// TestProgramStreamsApart (code review 2026-11-18, checks 6): a program's
// stdout and stderr each come back apart, as it wrote them, beside its
// combined output, so the tools container judges a stdout assertion on
// stdout alone; a slot program and a workspace program alike. A build keeps
// only its combined tail.
func TestProgramStreamsApart(t *testing.T) {
	script := "printf 'out-1\\n'; printf 'err-1\\n' >&2; printf 'out-2'; printf 'err-2' >&2; exit 3"
	ws, _ := newWorkspace(t)
	for name, run := range map[string]func() Result{
		"slot": func() Result {
			return newServer(t).Run(context.Background(), &toolwire.ExecRequest{Argv: []string{"sh", "-c", script}})
		},
		"workspace": func() Result {
			return ws.Do(context.Background(), &Request{Argv: []string{"sh", "-c", script}, Run: "v1a1t1-abcdef",
				Env: pathEnv()})
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := run()
			if got.Exit != 3 {
				t.Fatalf("exit %d, want 3 (%+v)", got.Exit, got)
			}
			if got.Stdout == nil || *got.Stdout != "out-1\nout-2" {
				t.Errorf("stdout = %v, want %q", deref(got.Stdout), "out-1\nout-2")
			}
			if got.Stderr == nil || *got.Stderr != "err-1\nerr-2" {
				t.Errorf("stderr = %v, want %q", deref(got.Stderr), "err-1\nerr-2")
			}
			for _, part := range []string{"out-1\n", "err-1\n", "out-2", "err-2"} {
				if !strings.Contains(got.Output, part) {
					t.Errorf("output %q lacks %q", got.Output, part)
				}
			}
			if len(got.Output) != len("out-1\nout-2err-1\nerr-2") {
				t.Errorf("output %q is not the two streams together", got.Output)
			}
		})
	}
	t.Run("build", func(t *testing.T) {
		got := ws.Do(context.Background(), &Request{Kind: KindBuild, Argv: []string{"sh", "-c", "echo out; echo err >&2"},
			Env: pathEnv()})
		if got.Stdout != nil || got.Stderr != nil {
			t.Errorf("a build's streams kept apart: %+v", got)
		}
		if got.Output != "out\nerr\n" {
			t.Errorf("build output %q, want both streams in the order written", got.Output)
		}
	})
	t.Run("could not start", func(t *testing.T) {
		got := newServer(t).Run(context.Background(), &toolwire.ExecRequest{Argv: []string{"maul-no-such-program"}})
		if got.Exit != 127 || got.Stdout == nil || *got.Stdout != "" || got.Stderr == nil ||
			*got.Stderr != got.Output {
			t.Errorf("result %+v: maul's words on stderr, nothing on stdout", got)
		}
	})
}

// TestProgramStreamCap: a stream past the cap marks the run truncated.
func TestProgramStreamCap(t *testing.T) {
	got := newServer(t).Run(context.Background(), &toolwire.ExecRequest{Argv: []string{"sh", "-c",
		"head -c 1000000 /dev/zero | tr '\\0' a >&2"}})
	if !got.Truncated || got.Stderr == nil || len(*got.Stderr) > MaxOutputBytes || got.Stdout == nil ||
		*got.Stdout != "" {
		t.Errorf("truncated %v, stdout %v, stderr %d bytes", got.Truncated, deref(got.Stdout), len(deref(got.Stderr)))
	}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}
