// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

//go:build e2edial

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"

	"github.com/spf13/cobra"

	"github.com/devthenet-labs/maul/internal/cli"
	"github.com/devthenet-labs/maul/internal/previewauth/httpapi"
)

// The e2e suite's dial seam (design §9 W6): envtest has no kube-proxy or
// cluster DNS, so the suite builds this binary with the e2edial tag and maps
// each slot Service's name to a local listener. Never in a released binary.

const dialMapFlag = "e2e-gateway-dial-map"

func testDialFlags(cmd *cobra.Command) {
	cmd.Flags().String(dialMapFlag, "", `TEST BUILDS ONLY: a JSON object mapping "<service>.<namespace>.svc:<port>" `+
		"to the loopback address the gateway dials instead; any other address is refused")
}

// upstreamDial maps the configured Service addresses to their listeners and
// refuses every other one, so a test sees a route it did not expect.
func upstreamDial(opts *cli.Options) (httpapi.DialFunc, error) {
	raw := opts.String(dialMapFlag)
	if raw == "" {
		return nil, nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("--%s: %w", dialMapFlag, err)
	}
	for from, to := range m {
		host, _, err := net.SplitHostPort(to)
		if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
			return nil, fmt.Errorf("--%s: %s maps to %q, not a loopback address", dialMapFlag, from, to)
		}
	}
	var d net.Dialer
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		to, ok := m[addr]
		if !ok {
			return nil, fmt.Errorf("e2e dial map: no listener for %s", addr)
		}
		return d.DialContext(ctx, network, to)
	}, nil
}
