// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

//go:build !unix

package toolexec

import (
	"errors"
	"os/exec"
)

// ownGroup is the default cancellation off unix: the shim runs only in a
// Linux container, so this exists to keep the module building everywhere.
func ownGroup(*exec.Cmd) {}

// killGroup has no group to kill off unix.
func killGroup(*exec.Cmd) {}

// killPID cannot kill off unix; the sweep then reports survivors.
func killPID(int) error { return errors.ErrUnsupported }

// reapOrphans has nothing to reap off unix.
func reapOrphans() {}
