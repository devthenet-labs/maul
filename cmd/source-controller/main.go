// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Command source-controller receives security findings from external sources
// (GitHub Advanced Security first) via GitHub webhooks and accumulates them
// into GitHub issues — the pipeline's state machine.
package main

import (
	"os"

	"github.com/devthenet-labs/maul/internal/cli"
)

func main() {
	opts := cli.NewOptions()
	root := cli.NewControllerRoot("source-controller",
		"Receive security findings via webhooks and accumulate them into GitHub issues", opts)
	root.AddCommand(newServeCmd(opts))
	os.Exit(cli.Execute(root, opts.Log))
}
