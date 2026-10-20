// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Command remediation-controller picks up context-enhanced security-finding
// issues, runs the classification/remediation coding agent in an ephemeral
// Kubernetes Job, and performs every GitHub side effect on its behalf —
// issue updates, alert dismissal, branch pushes, and pull requests.
package main

import (
	"os"

	"github.com/devthenet-labs/maul/internal/cli"
)

func main() {
	opts := cli.NewOptions()
	root := cli.NewControllerRoot("remediation-controller",
		"Run coding-agent classification/remediation Jobs and apply their GitHub side effects", opts)
	root.AddCommand(newServeCmd(opts))
	os.Exit(cli.Execute(root, opts.Log))
}
