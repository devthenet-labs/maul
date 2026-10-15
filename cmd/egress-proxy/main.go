// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Command egress-proxy serves the operator's network profiles: a
// GOPROXY-protocol reverse proxy to proxy.golang.org and sum.golang.org,
// one listener per profile, through which a profile Job's go command
// fetches the profile's allowlisted modules (tagged releases only, unless
// the profile admits pseudo-versions). The listener port is the identity:
// NetworkPolicy on both sides admits only the profile's labelled agent
// pods to it. A profile granted an operator-defined registry (Go, npm or
// pip, behind a login) is also served /r/<registry>/: the proxy adds the
// registry's credential, read from a mounted Secret or minted from
// CodeArtifact with its own role, and the agent never sees it. It caches
// nothing. Not a controller: no reconcilers and no leases; its only
// Kubernetes access is the opt-in --attribute-pods (get/list/watch on pods
// in the agent namespace, audit garnish only). probe sends one registry
// request from inside the pod, for debugging without a log sink. Engine in
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
	root.AddCommand(newServeCmd(opts), newProbeCmd(opts))
	os.Exit(cli.Execute(root, opts.Log))
}
