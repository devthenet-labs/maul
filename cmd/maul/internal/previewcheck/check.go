// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewcheck

import (
	"context"
	"fmt"
	"net"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/devthenet-labs/maul/cmd/maul/internal/checkreport"
	"github.com/devthenet-labs/maul/cmd/maul/internal/tailnet"
	"github.com/devthenet-labs/maul/internal/previewauth"
)

// The checks this package reports, in the order a run reports them.
const (
	CheckPreviewDNS = "preview-dns"
	CheckPreviewTLS = "preview-tls"
	// The preview sign-in checks, reported only while the chart's
	// previewAuth is on.
	CheckPreviewAuth     = "preview-auth"
	CheckPreviewAuthDex  = "preview-auth-dex"
	CheckPreviewAuthHost = "preview-auth-host"
	// CheckPreviewGatewayAllowlist is reported only while the preview
	// gateway is on.
	CheckPreviewGatewayAllowlist = "preview-gateway-allowlist"
)

// Config is what a run reads with.
type Config struct {
	// Reader reads the cluster with the caller's own kubeconfig.
	Reader client.Reader
	// Namespace is the release namespace: the controllers' ConfigMaps, the
	// gateway's exposure and the Previews live there.
	Namespace string
	// Project scopes the run: the probe host is <Project>-0.<host suffix>,
	// which no Preview owns, and only the Project's Ready Previews are
	// probed.
	Project string
	// AllPreviews probes every Ready Preview in the namespace, whatever its
	// Project: the install-level run (Install), whose Project names only the
	// probe host.
	AllPreviews bool
	// Resolver looks the preview host up; nil is net.DefaultResolver.
	Resolver Resolver
	// DialTLS completes the preview TLS handshake; nil is DialTLS.
	DialTLS TLSDialer
	// HTTP sends the preview sign-in checks' requests, never following a
	// redirect; nil is NewHTTPDoer.
	HTTP HTTPDoer
	// Tailnet, when set, is the caller's Tailscale client: in gateway mode
	// a gateway Service the Tailscale operator exposes (its
	// tailscale.com/expose annotation) publishes no address in the
	// cluster but a Secret, so its tailnet device is looked up there
	// instead (tailnet.Lookup), read-only. Nil looks nothing up.
	Tailnet tailnet.StatusFunc
	// Isolation, when set, runs the opt-in ingress isolation probe
	// (preview-netpol, Install only), the one check that writes to the
	// cluster. Nil runs every other check, all of them read-only.
	Isolation *Isolation
}

// Line is one check's verdict. A run's lines are about the install as a
// whole, never about one repository.
type Line struct {
	Name   string             `json:"name"`
	Status checkreport.Status `json:"status"`
	// Reason is printable: it carries text from outside the CLI (DNS, TLS
	// and HTTP errors, Dex's pages), escaped by checkreport.Printable.
	Reason string `json:"reason"`
}

// Checker runs the preview path's checks and collects their lines in the
// order they ran.
type Checker struct {
	cfg   Config
	lines []Line
}

// New is a Checker over cfg, its nil seams the CLI's defaults.
func New(cfg Config) *Checker {
	if cfg.Resolver == nil {
		cfg.Resolver = net.DefaultResolver
	}
	if cfg.DialTLS == nil {
		cfg.DialTLS = DialTLS
	}
	if cfg.HTTP == nil {
		cfg.HTTP = NewHTTPDoer()
	}
	return &Checker{cfg: cfg}
}

// Lines are the lines reported so far.
func (c *Checker) Lines() []Line { return c.lines }

// Host checks the probe host <Project>-0.<suffix> (preview-dns and
// preview-tls) against the path that serves preview hosts: the gateway's
// edge in gateway mode, else the legacy load balancer.
func (c *Checker) Host(ctx context.Context, s Settings, suffix string) {
	if mode := s.probeMode(); mode != previewauth.ProbeALB {
		c.gatewayHost(ctx, s, suffix, mode)
		return
	}
	c.previewHost(ctx, suffix)
}

// Auth checks preview sign-in (preview-auth, preview-auth-dex,
// preview-auth-host and, in gateway mode, preview-gateway-allowlist) when
// the chart turned it on; off, it reports nothing.
func (c *Checker) Auth(ctx context.Context, s Settings, suffix string) {
	c.previewAuth(ctx, s, suffix)
}

func (c *Checker) add(check string, status checkreport.Status, format string, args ...any) {
	c.lines = append(c.lines, Line{Name: check, Status: status,
		Reason: checkreport.Printable(fmt.Sprintf(format, args...))})
}
