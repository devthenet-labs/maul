// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Command context-controller reacts to newly opened security-finding issues
// and enhances their context from first/third parties (CMDB ownership,
// associated infrastructure) before remediation picks them up.
package main

import (
	"os"

	"github.com/devthenet-labs/maul/internal/cli"
)

func main() {
	opts := cli.NewOptions()
	root := cli.NewControllerRoot("context-controller",
		"Enhance security-finding issues with ownership and infrastructure context", opts)
	root.AddCommand(newServeCmd(opts))
	os.Exit(cli.Execute(root, opts.Log))
}
