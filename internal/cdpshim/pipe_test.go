// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package cdpshim

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// helperEnv makes the test binary a fake Chromium (TestMain): it records its
// arguments, starts a grandchild in its process group, and echoes every
// NUL-delimited message it reads on fd 3 back on fd 4.
const (
	helperEnv     = "CDPSHIM_FAKE_CHROMIUM"
	helperArgsEnv = "CDPSHIM_FAKE_CHROMIUM_ARGS"
	helperKidEnv  = "CDPSHIM_FAKE_CHROMIUM_KID"
)

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "1" {
		fakeChromium()
		return
	}
	os.Exit(m.Run())
}

// fakeChromium is the pipe peer Chromium would be, run as a real child
// process so the launcher's fd wiring and group kill are exercised.
func fakeChromium() {
	if p := os.Getenv(helperArgsEnv); p != "" {
		_ = os.WriteFile(p, []byte(strings.Join(os.Args[1:], "\n")), 0o600)
	}
	if p := os.Getenv(helperKidEnv); p != "" {
		kid := exec.Command("sleep", "60")
		if err := kid.Start(); err == nil {
			_ = os.WriteFile(p, []byte(strconv.Itoa(kid.Process.Pid)), 0o600)
		}
	}
	in := os.NewFile(3, "cdp-in")
	out := os.NewFile(4, "cdp-out")
	if in == nil || out == nil {
		os.Exit(3)
	}
	rd := bufio.NewReader(in)
	for {
		msg, err := rd.ReadBytes(0)
		if err != nil {
			os.Exit(0)
		}
		if _, err := out.Write(msg); err != nil {
			os.Exit(0)
		}
	}
}

// newPeerPipe is a pipe whose far end is an in-process fake peer: a
// goroutine running answer over the browser's input and output. kill runs
// on the pipe's Kill.
func newPeerPipe(t *testing.T, answer func(in *bufio.Reader, out io.Writer), kill func()) *pipe {
	t.Helper()
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		answer(bufio.NewReader(inR), outW)
		_ = outW.Close()
		_ = inR.Close()
	}()
	if kill == nil {
		kill = func() {}
	}
	p := newPipe(outR, inW, kill, func() error { return nil })
	t.Cleanup(p.Kill)
	return p
}

func echo(in *bufio.Reader, out io.Writer) {
	for {
		msg, err := in.ReadBytes(0)
		if err != nil {
			return
		}
		if _, err := out.Write(msg); err != nil {
			return
		}
	}
}

// TestPipeFraming: messages go out NUL-terminated and come back split on
// NUL, a message holding a NUL is refused before it is written (it would
// smuggle a second message), and one past MaxMessageBytes ends the pipe.
func TestPipeFraming(t *testing.T) {
	t.Run("round trip", func(t *testing.T) {
		p := newPeerPipe(t, echo, nil)
		for _, msg := range []string{`{"id":1,"method":"Browser.getVersion"}`, `{}`, strings.Repeat("x", 1<<20)} {
			if err := p.Send([]byte(msg)); err != nil {
				t.Fatalf("Send: %v", err)
			}
			got, err := p.Recv()
			if err != nil {
				t.Fatalf("Recv: %v", err)
			}
			if string(got) != msg {
				t.Errorf("got %d bytes, want %d", len(got), len(msg))
			}
		}
	})
	t.Run("several messages in one write", func(t *testing.T) {
		p := newPeerPipe(t, func(_ *bufio.Reader, out io.Writer) {
			_, _ = out.Write([]byte("a\x00bb\x00ccc\x00"))
		}, nil)
		var got []string
		for {
			msg, err := p.Recv()
			if err != nil {
				if !errors.Is(err, io.EOF) {
					t.Errorf("Recv: %v", err)
				}
				break
			}
			got = append(got, string(msg))
		}
		if want := []string{"a", "bb", "ccc"}; !slices.Equal(got, want) {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	t.Run("NUL refused", func(t *testing.T) {
		var seen bytes.Buffer
		done := make(chan struct{})
		p := newPeerPipe(t, func(in *bufio.Reader, _ io.Writer) {
			_, _ = io.Copy(&seen, in)
			close(done)
		}, nil)
		if err := p.Send([]byte("a\x00{\"id\":2}")); !errors.Is(err, ErrNUL) {
			t.Fatalf("Send: %v, want ErrNUL", err)
		}
		p.Kill()
		<-done
		if seen.Len() != 0 {
			t.Errorf("peer read %q", seen.String())
		}
	})
	t.Run("too long", func(t *testing.T) {
		p := newPeerPipe(t, func(_ *bufio.Reader, out io.Writer) {
			_, _ = out.Write(bytes.Repeat([]byte("y"), MaxMessageBytes+1))
			_, _ = out.Write([]byte{0})
		}, nil)
		if _, err := p.Recv(); err == nil {
			t.Fatal("Recv of an over-long message succeeded")
		}
	})
	t.Run("kill once", func(t *testing.T) {
		calls := 0
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		p := newPipe(r, w, func() { calls++ }, func() error { return nil })
		p.Kill()
		p.Kill()
		if calls != 1 {
			t.Errorf("kill ran %d times", calls)
		}
		if _, err := p.Recv(); err == nil {
			t.Error("Recv after Kill succeeded")
		}
		if err := p.Send([]byte("x")); err == nil {
			t.Error("Send after Kill succeeded")
		}
	})
}

// TestArgs: Chromium speaks CDP only over the pipe, headless, without its
// own sandbox (SP-3: RuntimeDefault refuses its namespaces), never with a
// TCP debugging port or --remote-allow-origins.
func TestArgs(t *testing.T) {
	args := Args("/tmp/profile")
	wants := []string{"--headless", "--remote-debugging-pipe", "--no-sandbox", "--user-data-dir=/tmp/profile"}
	for _, want := range wants {
		if !slices.Contains(args, want) {
			t.Errorf("args %q lack %s", args, want)
		}
	}
	for _, a := range args {
		for _, bad := range []string{"--remote-debugging-port", "--remote-debugging-address", "--remote-allow-origins"} {
			if strings.HasPrefix(a, bad) {
				t.Errorf("args carry %s", a)
			}
		}
	}
	if args[len(args)-1] != "about:blank" {
		t.Errorf("last arg %q, want about:blank", args[len(args)-1])
	}
}

// TestChromiumLaunch runs a real child on the pipe: fds 3 and 4 carry the
// messages, Kill takes the whole process group down (a grandchild
// included), and Wait removes the profile directory.
func TestChromiumLaunch(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the shim runs only on Linux")
	}
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	kidFile := filepath.Join(dir, "kid")
	t.Setenv(helperEnv, "1")
	t.Setenv(helperArgsEnv, argsFile)
	t.Setenv(helperKidEnv, kidFile)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(dir, "tmp")
	if err := os.Mkdir(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	b, err := Chromium{Path: exe, TempDir: tmp}.Launch()
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if err := b.Send([]byte(`{"id":1}`)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	got, err := b.Recv()
	if err != nil || string(got) != `{"id":1}` {
		t.Fatalf("Recv = %q, %v", got, err)
	}
	recorded, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(string(recorded), "\n")
	if !slices.Contains(args, "--remote-debugging-pipe") {
		t.Errorf("child args %q", args)
	}
	var profile string
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, "--user-data-dir="); ok {
			profile = v
		}
	}
	if filepath.Dir(profile) != tmp {
		t.Errorf("profile %q not under %s", profile, tmp)
	}
	kid := waitPid(t, kidFile)

	b.Kill()
	if err := b.Wait(); err == nil {
		t.Error("Wait after Kill reported a clean exit")
	}
	if _, err := os.Stat(profile); !os.IsNotExist(err) {
		t.Errorf("profile left behind: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(kid, 0) == nil {
		if time.Now().After(deadline) {
			_ = syscall.Kill(kid, syscall.SIGKILL)
			t.Fatalf("grandchild %d survived Kill", kid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestChromiumLaunchMissing: a binary that cannot start is an error and
// leaves no profile directory.
func TestChromiumLaunchMissing(t *testing.T) {
	tmp := t.TempDir()
	if _, err := (Chromium{Path: filepath.Join(tmp, "nope"), TempDir: tmp}).Launch(); err == nil {
		t.Fatal("Launch of a missing binary succeeded")
	}
	left, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("left %v", left)
	}
}

// waitPid reads the pid the fake Chromium wrote.
func waitPid(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		raw, err := os.ReadFile(path)
		if err == nil && len(raw) > 0 {
			pid, err := strconv.Atoi(string(raw))
			if err != nil {
				t.Fatal(err)
			}
			return pid
		}
		if time.Now().After(deadline) {
			t.Fatalf("no pid in %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
