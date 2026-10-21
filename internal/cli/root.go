// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/devthenet-labs/maul/internal/version"
)

// NewRoot returns the conventional root command shared by every maul
// binary: silenced usage/errors (errors are failures, not usage mistakes;
// main logs the error once) and cobra's built-in --version reporting the
// build metadata stamped into internal/version.
func NewRoot(name, short string) *cobra.Command {
	root := &cobra.Command{
		Use:           name,
		Short:         short,
		Version:       fmt.Sprintf("%s (commit %s, built %s)", version.Version, version.Commit, version.BuildDate),
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	return root
}

// Execute runs root under a context cancelled by SIGINT/SIGTERM and returns
// the process exit code; main is the only place allowed to call os.Exit.
func Execute(root *cobra.Command, log *slog.Logger) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := root.ExecuteContext(ctx); err != nil {
		log.LogAttrs(ctx, slog.LevelError, "fatal", slog.Any("error", err))
		return 1
	}
	return 0
}
