// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

//go:build unix

package toolexec

import (
	"os/exec"
	"syscall"
)

// ownGroup starts cmd in a process group of its own, whose whole group its
// cancellation kills.
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		killGroup(cmd)
		return nil
	}
}

// killGroup kills every process left in cmd's group.
func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// killPID sends SIGKILL to one process.
func killPID(pid int) error { return syscall.Kill(pid, syscall.SIGKILL) }

// reapOrphans collects every exited child without waiting, so the
// processes the shim inherits as the container's pid 1 never linger as
// zombies. Only pid 1 reaps: anywhere else a child is a running program's,
// whose Wait must see it.
func reapOrphans() {
	if syscall.Getpid() != 1 {
		return
	}
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
		if pid <= 0 || err != nil {
			return
		}
	}
}
