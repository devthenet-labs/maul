// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

//go:build !linux

package cdpshim

import "os/exec"

// ownGroup does nothing off Linux: the shim runs only in a Linux container,
// so this exists to keep the module building everywhere.
func ownGroup(*exec.Cmd) {}

// killGroup kills only the process itself off Linux.
func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
