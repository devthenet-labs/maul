// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Command kubectl-maul is the kubectl plugin shim: it resolves the real
// maul binary — first beside this executable, then on PATH — and execs
// it with the same arguments. It is a shim, not a thin client: shipping
// the ~2 MiB wrapper instead of a second full build is what keeps the
// maul-cli archive and Homebrew cask from carrying the CLI twice, while
// staying a real file (archives have to stay extractable on windows,
// where symlinks need a privilege most users do not have).
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

func main() {
	path, err := resolve()
	if err != nil {
		fmt.Fprintf(os.Stderr, "kubectl-maul: maul binary not found beside this shim or on PATH: %v\n", err)
		os.Exit(1)
	}
	os.Exit(run(path, os.Args[1:]))
}

// resolve locates the real maul binary: the copy installed beside the
// shim wins (the archive/cask layout), PATH is the fallback.
func resolve() (string, error) {
	name := "maul"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if exe, err := os.Executable(); err == nil {
		sibling := filepath.Join(filepath.Dir(exe), name)
		if info, err := os.Stat(sibling); err == nil && !info.IsDir() {
			return sibling, nil
		}
	}
	return exec.LookPath("maul")
}

// passthrough runs path as a child process with inherited stdio and
// returns its exit code — the fallback for platforms without exec.
func passthrough(path string, args []string) int {
	cmd := exec.Command(path, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "kubectl-maul: %v\n", err)
		return 1
	}
	return 0
}
