// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Command maul-tool is the agent evaluator's CLI (DESIGN 2.1, 2.7; SPEC
// S1-09), on PATH in the claude and codex agent-runner images: the
// evaluator runs it by bare name, one command per Bash call, to reach its
// tool pod's tools container (http, browser, exec, logs, events, check run,
// check results). The engine is internal/maultool; this is its thin
// wiring. It reads only MAUL_TOOLS_URL, sends the per-evaluation key from
// /work/input/tools-key when that file exists, and otherwise reads and
// writes files only under /work/scratch. Exit 0 ok, 2 usage, 3 refused, 4 tool
// error.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/devthenet-labs/maul/internal/maultool"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := maultool.Run(ctx, os.Args[1:], maultool.Options{
		Getenv:  os.Getenv,
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
		Scratch: scratchDir(os.Getenv),
		KeyFile: maultool.KeyFile,
	})
	stop()
	os.Exit(code)
}
