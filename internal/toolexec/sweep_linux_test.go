// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

//go:build linux

package toolexec

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// zombieLeaderEnv makes the test binary, re-executed, a process whose main
// thread exits while another thread keeps running.
const zombieLeaderEnv = "MAUL_TOOLEXEC_ZOMBIE_LEADER"

// init runs on the main thread (the thread-group leader) before any test:
// under zombieLeaderEnv it starts a thread of its own and exits the main
// thread alone, leaving /proc/<pid>/status at State Z with Threads > 1.
func init() {
	if os.Getenv(zombieLeaderEnv) != "1" {
		return
	}
	started := make(chan struct{})
	go func() {
		runtime.LockOSThread()
		close(started)
		for {
			time.Sleep(time.Hour)
		}
	}()
	<-started
	_, _, _ = syscall.RawSyscall(syscall.SYS_EXIT, 0, 0, 0)
}

// procState reads a process's State letter and Threads count straight from
// /proc; ok is false once it is gone.
func procState(pid int) (state string, threads int, ok bool) {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if err != nil {
		return "", 0, false
	}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		key, val, _ := strings.Cut(sc.Text(), ":")
		fields := strings.Fields(val)
		switch {
		case key == "State" && len(fields) > 0:
			state = fields[0]
		case key == "Threads" && len(fields) > 0:
			threads, _ = strconv.Atoi(fields[0])
		}
	}
	return state, threads, true
}

// TestSweepKillsZombieLeaderWithLiveThreads: a real process whose leader is
// a zombie while another thread runs is not dead; the sweep kills it, and
// once only the unreaped zombie is left the sweep is clean.
func TestSweepKillsZombieLeaderWithLiveThreads(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), zombieLeaderEnv+"=1")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start the helper: %v", err)
	}
	pid := cmd.Process.Pid
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		select {
		case <-waited:
		case <-time.After(10 * time.Second):
			t.Error("the helper was never reaped")
		}
	})

	deadline := time.Now().Add(10 * time.Second)
	for {
		state, threads, ok := procState(pid)
		if ok && state == "Z" && threads > 1 {
			break
		}
		if !ok || time.Now().After(deadline) {
			t.Skipf("helper never became a zombie leader with live threads (state %q, threads %d, present %v)",
				state, threads, ok)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// A proc filesystem showing only the helper, so the sweep can touch
	// nothing else of this UID.
	proc := t.TempDir()
	if err := os.Symlink(filepath.Join("/proc", strconv.Itoa(pid)), filepath.Join(proc, strconv.Itoa(pid))); err != nil {
		t.Fatal(err)
	}
	var killed []int
	sw := sweeper{proc: proc, uid: os.Getuid(), self: os.Getpid(), kill: func(p int) error {
		killed = append(killed, p)
		if p != pid {
			t.Errorf("sweep killed pid %d, not the helper %d", p, pid)
			return nil
		}
		return killPID(p)
	}}
	if err := sw.run(); err != nil {
		t.Fatalf("sweep: %v (an unreaped zombie with no live thread must count as dead)", err)
	}
	if len(killed) == 0 {
		t.Error("the sweep left a zombie leader with live threads running")
	}
	if state, threads, ok := procState(pid); ok && (state != "Z" || threads > 1) {
		t.Errorf("after the sweep the helper is state %q with %d threads", state, threads)
	}
}
