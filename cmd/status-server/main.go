// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Command status-server serves the maul status page: a web dashboard over
// the Finding state machine and the FindingRollup statistics, with SSO
// sign-in and RBAC-gated approve/suspend/resume actions. Rollup statistics
// are public; the findings surface requires authentication.
package main

import (
	"os"

	"github.com/devthenet-labs/maul/internal/cli"
)

func main() {
	opts := cli.NewOptions()
	root := cli.NewControllerRoot("status-server",
		"Serve the status page: findings review, human approvals, and rollup statistics", opts)
	root.AddCommand(newServeCmd(opts))
	os.Exit(cli.Execute(root, opts.Log))
}
