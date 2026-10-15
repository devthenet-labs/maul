// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Command egress-proxy serves the operator's network profiles: a
// GOPROXY-protocol reverse proxy to proxy.golang.org and sum.golang.org,
// one listener per profile, through which a profile Job's go command
// fetches the profile's allowlisted modules (tagged releases only, unless
// the profile admits pseudo-versions). The listener port is the identity:
// NetworkPolicy on both sides admits only the profile's labelled agent
// pods to it. It holds no credential and caches nothing. Not a controller:
// no reconcilers, no leases, and no Kubernetes access. Engine in
// internal/egressproxy.
package main

import (
	"os"

	"github.com/bitwise-media-group/patchy/internal/cli"
)

func main() {
	opts := cli.NewOptions()
	root := cli.NewControllerRoot("egress-proxy",
		"Serve the dependency routes of the operator's network profiles to agent pods", opts)
	root.AddCommand(newServeCmd(opts))
	os.Exit(cli.Execute(root, opts.Log))
}
