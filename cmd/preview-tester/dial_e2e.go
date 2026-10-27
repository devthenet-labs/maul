// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

//go:build e2edial

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"

	"github.com/devthenet-labs/maul/internal/previewtester"
)

// The e2e suite's seams: envtest has no kubelet, kube-proxy or cluster DNS,
// so the suite's fake kubelet runs this binary built with the e2edial tag,
// maps each slot Service address to a loopback listener and names a
// termination message file of its own. Never in a released binary.

const (
	// dialMapEnv is a JSON object mapping "<service>:<port>" to the
	// loopback address dialled instead; any other address is refused.
	dialMapEnv = "MAUL_E2E_DIAL_MAP"
	// terminationEnv replaces the termination message path when set.
	terminationEnv = "MAUL_E2E_TERMINATION_PATH"
)

func testDial(getenv func(string) string) (previewtester.DialFunc, error) {
	raw := getenv(dialMapEnv)
	if raw == "" {
		return nil, nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("%s: %w", dialMapEnv, err)
	}
	for from, to := range m {
		host, _, err := net.SplitHostPort(to)
		if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
			return nil, fmt.Errorf("%s: %s maps to %q, not a loopback address", dialMapEnv, from, to)
		}
	}
	var d net.Dialer
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		to, ok := m[addr]
		if !ok {
			return nil, &net.DNSError{Err: "e2e dial map: no listener", Name: addr, IsNotFound: true}
		}
		return d.DialContext(ctx, network, to)
	}, nil
}

func terminationPath(getenv func(string) string) string {
	if p := getenv(terminationEnv); p != "" {
		return p
	}
	return previewtester.TerminationPath
}
