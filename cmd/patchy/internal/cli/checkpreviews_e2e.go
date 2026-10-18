// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

//go:build e2edial

package cli

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"

	"github.com/spf13/cobra"

	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/previewcheck"
)

// The e2e suite's network seam for check previews (preview setup design §9,
// W10): envtest has no DNS for the names a generated install answers, so the
// suite builds this binary with the e2edial tag, maps each <host>:<port> the
// checks ask to a loopback listener, and trusts its test CA. The checks
// themselves are the released binary's; only what they dial differs. Never
// in a released binary (checkpreviews_net.go).

const (
	checkDialMapFlag = "e2e-dial-map"
	checkCAFileFlag  = "e2e-ca-file"
)

func checkPreviewsTestFlags(cmd *cobra.Command) {
	cmd.Flags().String(checkDialMapFlag, "", `TEST BUILDS ONLY: a JSON object mapping "<host>:<port>" to the `+
		"loopback address dialled instead; any other address is refused, and a host in it resolves to that address")
	cmd.Flags().String(checkCAFileFlag, "", "TEST BUILDS ONLY: the PEM roots trusted instead of the system's")
	_ = cmd.Flags().MarkHidden(checkDialMapFlag)
	_ = cmd.Flags().MarkHidden(checkCAFileFlag)
}

// checkPreviewsNetwork is the caller's network without --e2e-dial-map, and
// the mapped one with it.
func checkPreviewsNetwork(cmd *cobra.Command) (checkPreviewsDeps, error) {
	raw, _ := cmd.Flags().GetString(checkDialMapFlag)
	if raw == "" {
		return networkCheckPreviewsDeps(), nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return checkPreviewsDeps{}, fmt.Errorf("--%s: %w", checkDialMapFlag, err)
	}
	hosts := map[string]string{}
	for from, to := range m {
		host, _, err := net.SplitHostPort(to)
		if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
			return checkPreviewsDeps{}, fmt.Errorf("--%s: %s maps to %q, not a loopback address", checkDialMapFlag,
				from, to)
		}
		name, _, err := net.SplitHostPort(from)
		if err != nil {
			return checkPreviewsDeps{}, fmt.Errorf("--%s: %q is not <host>:<port>", checkDialMapFlag, from)
		}
		hosts[name] = host
	}
	var roots *x509.CertPool
	if path, _ := cmd.Flags().GetString(checkCAFileFlag); path != "" {
		pem, err := os.ReadFile(path) //nolint:gosec // a test build's own CA file
		if err != nil {
			return checkPreviewsDeps{}, fmt.Errorf("--%s: %w", checkCAFileFlag, err)
		}
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			return checkPreviewsDeps{}, fmt.Errorf("--%s: no certificate in %s", checkCAFileFlag, path)
		}
	}
	var d net.Dialer
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		to, ok := m[addr]
		if !ok {
			return nil, fmt.Errorf("e2e dial map: no listener for %s", addr)
		}
		return d.DialContext(ctx, network, to)
	}
	client, ok := previewcheck.NewHTTPDoer().(*http.Client)
	if !ok {
		return checkPreviewsDeps{}, errors.New("previewcheck.NewHTTPDoer is no longer an *http.Client")
	}
	client.Transport = &http.Transport{DialContext: dial,
		TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
	return checkPreviewsDeps{
		resolver: mappedResolver(hosts),
		dialTLS: func(ctx context.Context, host string) (tls.ConnectionState, error) {
			conn, err := dial(ctx, "tcp", net.JoinHostPort(host, "443"))
			if err != nil {
				return tls.ConnectionState{}, err
			}
			tc := tls.Client(conn, &tls.Config{ServerName: host, RootCAs: roots, MinVersion: tls.VersionTLS12})
			defer tc.Close() //nolint:errcheck // nothing was written; the handshake is all that was wanted
			if err := tc.HandshakeContext(ctx); err != nil {
				return tls.ConnectionState{}, err
			}
			return tc.ConnectionState(), nil
		},
		http: client,
	}, nil
}

// mappedResolver answers a mapped host with its listener's address and
// every other one as a name that does not exist.
type mappedResolver map[string]string

func (r mappedResolver) LookupHost(_ context.Context, host string) ([]string, error) {
	if addr, ok := r[host]; ok {
		return []string{addr}, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}
