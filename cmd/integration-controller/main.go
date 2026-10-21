// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Command integration-controller is the edge between maul and its external
// systems: it terminates the provider webhook paths (the cluster's single
// internet-facing entry point), turns scanner deliveries into Finding
// resources, projects Findings as tracking issues, and applies human signals
// (approvals, closures, merges) back onto Findings.
package main

import (
	"os"

	"github.com/devthenet-labs/maul/internal/cli"
)

func main() {
	opts := cli.NewOptions()
	root := cli.NewControllerRoot("integration-controller",
		"Receive scanner webhooks, project findings to tracking systems, and apply human signals", opts)
	root.AddCommand(newServeCmd(opts))
	os.Exit(cli.Execute(root, opts.Log))
}
