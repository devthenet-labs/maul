// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package toolexec

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/devthenet-labs/maul/internal/toolwire"
)

func newServer(t *testing.T) *Server {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh on PATH")
	}
	s, err := NewServer(Options{
		Scratch: filepath.Join(t.TempDir(), "scratch"),
		Env:     []string{"PATH=" + os.Getenv("PATH")},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return s
}

// TestRun: argv runs without a shell, in the scratch directory, with the
// files written first and the shim's own environment withheld.
func TestRun(t *testing.T) {
	t.Setenv("MAUL_TOOLEXEC_SECRET", "leak")
	s := newServer(t)
	tests := []struct {
		name     string
		req      toolwire.ExecRequest
		exit     int
		output   string
		contains string
	}{
		{
			name:   "argv is not a shell line",
			req:    toolwire.ExecRequest{Argv: []string{"echo", "a;", "$HOME", "`id`"}},
			output: "a; $HOME `id`\n",
		},
		{
			name:   "a shell runs only when argv names one",
			req:    toolwire.ExecRequest{Argv: []string{"sh", "-c", "echo out; echo err >&2; exit 3"}},
			exit:   3,
			output: "out\nerr\n",
		},
		{
			name: "files are written under scratch and the program runs there",
			req: toolwire.ExecRequest{
				Argv: []string{"sh", "dir/run.sh"},
				Files: []toolwire.File{
					{Name: "dir/run.sh", Content: []byte("pwd; cat dir/data.txt\n")},
					{Name: "dir/data.txt", Content: []byte("hello")},
				},
			},
			output: s.scratch + "\nhello",
		},
		{
			name:   "the shim's environment is withheld",
			req:    toolwire.ExecRequest{Argv: []string{"sh", "-c", "echo \"[$MAUL_TOOLEXEC_SECRET]$HOME\""}},
			output: "[]" + s.scratch + "\n",
		},
		{
			name:     "a missing program is exit 127 with maul's words",
			req:      toolwire.ExecRequest{Argv: []string{"maul-no-such-program"}},
			exit:     127,
			contains: "[maul: could not start",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := s.Run(context.Background(), &tc.req)
			if got.Exit != tc.exit {
				t.Errorf("exit %d, want %d (output %q)", got.Exit, tc.exit, got.Output)
			}
			if tc.contains != "" {
				if !strings.Contains(got.Output, tc.contains) {
					t.Errorf("output %q does not contain %q", got.Output, tc.contains)
				}
			} else if got.Output != tc.output {
				t.Errorf("output %q, want %q", got.Output, tc.output)
			}
			if got.Truncated || got.TimedOut {
				t.Errorf("truncated %v, timed out %v", got.Truncated, got.TimedOut)
			}
		})
	}
}

// TestRunRefusesEscapes: a file name leaving scratch is refused before
// anything runs, and so is one that would follow a symlink out of it.
func TestRunRefusesEscapes(t *testing.T) {
	s := newServer(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(s.scratch, "link")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../x", "a/../../x", "/etc/passwd", "./x", "a//b", "", "link/x"} {
		t.Run(name, func(t *testing.T) {
			marker := filepath.Join(s.scratch, "ran")
			got := s.Run(context.Background(), &toolwire.ExecRequest{
				Argv:  []string{"touch", marker},
				Files: []toolwire.File{{Name: name, Content: []byte("x")}},
			})
			if got.Refused == "" {
				t.Fatalf("not refused: %+v", got)
			}
			if _, err := os.Stat(marker); err == nil {
				t.Error("the program ran after a refused file")
			}
			entries, err := os.ReadDir(outside)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Errorf("a file was written outside scratch: %v", entries)
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(s.scratch), "x")); err == nil {
				t.Error("a file was written beside scratch")
			}
		})
	}
}

// TestRunTimeout: a program past its time is killed with everything it
// started, and says so.
func TestRunTimeout(t *testing.T) {
	s := newServer(t)
	s.timeout = 300 * time.Millisecond
	pidFile := filepath.Join(s.scratch, "child.pid")
	start := time.Now()
	got := s.Run(context.Background(), &toolwire.ExecRequest{
		Argv: []string{"sh", "-c", "echo before; sleep 30 & echo $! > " + pidFile + "; wait"},
	})
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("took %v", took)
	}
	if !got.TimedOut || got.Exit != -1 {
		t.Errorf("timed out %v exit %d, want true -1", got.TimedOut, got.Exit)
	}
	if !strings.HasPrefix(got.Output, "before\n") || !strings.HasSuffix(got.Output, TimeoutMarker) {
		t.Errorf("output %q", got.Output)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid := strings.TrimSpace(string(raw))
	deadline := time.Now().Add(5 * time.Second)
	for alive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("background child %s outlived the timeout", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestRunReapsBackground: a program that exits leaves nothing running.
func TestRunReapsBackground(t *testing.T) {
	s := newServer(t)
	pidFile := filepath.Join(s.scratch, "child.pid")
	got := s.Run(context.Background(), &toolwire.ExecRequest{
		Argv: []string{"sh", "-c", "sleep 30 >/dev/null 2>&1 & echo $! > " + pidFile},
	})
	if got.Exit != 0 || got.TimedOut {
		t.Fatalf("%+v", got)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid := strings.TrimSpace(string(raw))
	deadline := time.Now().Add(5 * time.Second)
	for alive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("background child %s outlived its program", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// alive reports a process that exists and is not a zombie.
func alive(pid string) bool {
	raw, err := os.ReadFile(filepath.Join("/proc", pid, "stat"))
	if err != nil {
		return false
	}
	_, rest, _ := strings.Cut(string(raw), ") ")
	return !strings.HasPrefix(rest, "Z")
}

// TestRunOutputCap: output past the cap is cut, valid UTF-8, and marked.
func TestRunOutputCap(t *testing.T) {
	s := newServer(t)
	tests := []struct {
		name      string
		script    string
		truncated bool
		size      int
	}{
		{"under the cap", "head -c 1000 /dev/zero | tr '\\0' a", false, 1000},
		{"exactly the cap", "head -c 65536 /dev/zero | tr '\\0' a", false, MaxOutputBytes},
		{"over the cap", "head -c 1000000 /dev/zero | tr '\\0' a", true, MaxOutputBytes},
		{
			"multibyte across the cut",
			"head -c 65500 /dev/zero | tr '\\0' a; yes é | head -c 200000 | tr -d '\\n'",
			true, -1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := s.Run(context.Background(), &toolwire.ExecRequest{Argv: []string{"sh", "-c", tc.script}})
			if got.Truncated != tc.truncated {
				t.Errorf("truncated %v, want %v", got.Truncated, tc.truncated)
			}
			if len(got.Output) > MaxOutputBytes {
				t.Errorf("output is %d bytes, over %d", len(got.Output), MaxOutputBytes)
			}
			if tc.size >= 0 && len(got.Output) != tc.size {
				t.Errorf("output is %d bytes, want %d", len(got.Output), tc.size)
			}
			if !utf8.ValidString(got.Output) {
				t.Error("output is not valid UTF-8")
			}
			if tc.truncated != strings.HasSuffix(got.Output, toolwire.TruncationMarker) {
				t.Errorf("marker present %v, want %v", !tc.truncated, tc.truncated)
			}
		})
	}
}

// TestRunInvalidUTF8: bytes a program prints that are not UTF-8 are shown
// as replacement characters, never passed on raw.
func TestRunInvalidUTF8(t *testing.T) {
	s := newServer(t)
	got := s.Run(context.Background(), &toolwire.ExecRequest{Argv: []string{"printf", `a\377b`}})
	if got.Output != "a�b" {
		t.Errorf("output %q", got.Output)
	}
}

// TestRunRefusesInvalid: a request outside the wire's grammar never runs.
func TestRunRefusesInvalid(t *testing.T) {
	s := newServer(t)
	for _, req := range []toolwire.ExecRequest{
		{},
		{Argv: []string{""}},
		{Argv: []string{"true"}, Files: []toolwire.File{{Name: "a"}, {Name: "a"}}},
	} {
		if got := s.Run(context.Background(), &req); got.Refused == "" {
			t.Errorf("%+v not refused: %+v", req, got)
		}
	}
}
