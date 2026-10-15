// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package sandboxprobe

import (
	"context"
	"log/slog"
)

// Command is the agent-runner subcommand that runs the probe instead of a
// stage. internal/jobs writes it into the prepare script and
// cmd/agent-runner dispatches on it; it is defined once, here, because the
// two sides of the pod boundary must agree and cmd/ cannot be imported.
const Command = "sandbox-probe"

// Exit statuses of a probe that reached no verdict. Neither is 0, which
// hands over to the agent container, nor ExitUnenforced, the verdict the
// collectors map to SandboxUnenforced: the prepare init fails as an
// ordinary error and the untrusted image never starts.
const (
	exitInterrupted   = 1
	exitMisconfigured = 2
)

// Main runs the probe the way `agent-runner sandbox-probe [args]` does: its
// configuration from the arguments (Parse) and the environment, one line to
// log, and the process exit status as the result. With no arguments it is
// the strict probe (Exit); with --mode wait it is the wait probe (Wait).
func Main(ctx context.Context, args []string, getenv func(string) string, log *slog.Logger) int {
	p, mode, err := Parse(args, getenv)
	if err != nil {
		log.LogAttrs(ctx, slog.LevelError, "invalid sandbox probe configuration", slog.Any("error", err))
		return exitMisconfigured
	}
	if mode == ModeWait {
		return p.Wait(ctx, log)
	}
	return p.Exit(ctx, log)
}

// Wait runs the probe in the wait mode: it returns once the targets have
// been blocked for the confirming rounds or the window has closed, and
// exits 0 either way, logging the verdict line (at error level when a
// target is still open, so it stands out in the prepare log). It never
// exits ExitUnenforced, so a default-image Job is never refused, its
// attempt never spent and no breaker tripped by it; exitInterrupted when
// the context ended first.
func (p Prober) Wait(ctx context.Context, log *slog.Logger) int {
	res, err := p.Run(ctx)
	if err != nil {
		log.LogAttrs(ctx, slog.LevelError, "sandbox probe interrupted", slog.Any("error", err))
		return exitInterrupted
	}
	level := slog.LevelInfo
	if !res.Enforced {
		level = slog.LevelError
	}
	log.LogAttrs(ctx, level, res.WaitVerdict())
	return 0
}

// Exit runs the probe and maps its conclusion onto the process exit status,
// logging the verdict line: 0 when every target was blocked, ExitUnenforced
// when one still answered as the window closed, exitInterrupted when the
// context ended first (never a verdict).
func (p Prober) Exit(ctx context.Context, log *slog.Logger) int {
	res, err := p.Run(ctx)
	if err != nil {
		log.LogAttrs(ctx, slog.LevelError, "sandbox probe interrupted", slog.Any("error", err))
		return exitInterrupted
	}
	if !res.Enforced {
		log.LogAttrs(ctx, slog.LevelError, res.Verdict())
		return ExitUnenforced
	}
	log.LogAttrs(ctx, slog.LevelInfo, res.Verdict())
	return 0
}
