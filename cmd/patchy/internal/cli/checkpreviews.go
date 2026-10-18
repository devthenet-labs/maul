// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/spf13/cobra"

	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/checkreport"
	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/previewcheck"
	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/printer"
)

// checkPreviewsTimeout bounds a whole `check previews` run: a few cluster
// reads, one DNS lookup, one TLS handshake and a handful of HTTP requests
// per Ready Preview.
const checkPreviewsTimeout = 2 * time.Minute

// newCheckPreviewsCmd checks the preview install as a whole.
func newCheckPreviewsCmd(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "previews",
		Short: "Check that the preview install is ready, before any Project uses it",
		Long: "Check the preview install in the release namespace (-n) from your workstation,\n" +
			"before any Project uses it: one line per check, PASS, FAIL, WARN or SKIP, then\n" +
			"the reason. check project judges one Project's previews; this judges what\n" +
			"every Project's previews stand on, with the same checks where they overlap.\n\n" +
			"preview-config reads the controllers' ConfigMaps: previews and sign-in are\n" +
			"on and agree on the host suffix, the preview gateway alone serves preview\n" +
			"hosts (the legacy load balancer path still serving is a WARN, and serving\n" +
			"without sign-in, previewAuth.stage permit, a FAIL), the sign-in\n" +
			"host sits outside the suffix, and the suffix is a registrable domain of its\n" +
			"own, shared with neither the sign-in host, the relay's issuer nor Dex (a\n" +
			"WARN: preview code would run same-site with sign-in).\n\n" +
			"preview-dns, preview-tls, preview-auth, preview-auth-dex, preview-auth-host and\n" +
			"preview-gateway-allowlist are check project's, asked of\n" +
			"patchy-probe-0.<host suffix> (the relay's own probe host, which no Preview\n" +
			"owns) and of every Ready Preview, whatever its Project: the host resolves\n" +
			"to the gateway's edge and serves a trusted certificate, the relay and Dex\n" +
			"answer, the probe host is the gateway's 404 page, each Ready Preview\n" +
			"redirects to sign in, and a spoofed X-Forwarded-For cannot get past\n" +
			"previewAuth.gateway.inboundCIDRs.\n\n" +
			"preview-viewers reads the bindings of the chart's <release>-preview-viewer\n" +
			"ClusterRole: at least one User or Group carrying the relay's claim prefix\n" +
			"should be bound, or every sign-in is refused 403 unless another role grants\n" +
			"get on projects/previews, which this cannot see (a WARN). preview-nodes\n" +
			"reads the nodes: with node isolation required, at least one carries preview-\n" +
			"controller's node selector and its <taint key>=true:NoExecute taint, and no\n" +
			"node carries the selector without that taint (a FAIL: any Pod could run\n" +
			"beside preview code there). A node with the taint but not the selector is\n" +
			"another install's or a label typo (a WARN); with an EKS Auto Mode NodePool,\n" +
			"no node yet is a SKIP; isolation off is a WARN.\n\n" +
			"The check never reads a Secret. Its cluster reads, with your kubeconfig, are\n" +
			"ConfigMaps, the preview placeholder and gateway Ingresses, the gateway's\n" +
			"Service, Previews, RoleBindings, ClusterRoleBindings and Nodes; a read you\n" +
			"may not make is a SKIP. The gateway's edge may admit only\n" +
			"previewAuth.gateway.inboundCIDRs, so from any other address the host checks\n" +
			"time out or are refused, and that is a SKIP, not a FAIL.\n\n" +
			"-o json or -o yaml prints the whole report as data. The exit status is 1 when\n" +
			"any check fails; a WARN never fails the run.",
		Example: "  patchy check previews -n patchy\n" +
			"  patchy check previews -n patchy -o json | jq '.checks[] | select(.status != \"PASS\")'",
		Args:              cobra.NoArgs,
		ValidArgsFunction: noFileCompletion,
		RunE: func(cmd *cobra.Command, _ []string) error {
			deps, err := checkPreviewsNetwork(cmd)
			if err != nil {
				return errUsage(err)
			}
			return runCheckPreviews(cmd.Context(), opts, deps)
		},
	}
	checkPreviewsTestFlags(cmd)
	return cmd
}

// networkCheckPreviewsDeps is what a released binary reaches: the caller's
// own DNS, the system's roots and the hosts by name.
func networkCheckPreviewsDeps() checkPreviewsDeps {
	return checkPreviewsDeps{
		resolver: net.DefaultResolver,
		dialTLS:  previewcheck.DialTLS,
		http:     previewcheck.NewHTTPDoer(),
	}
}

// checkPreviewsDeps is what `check previews` reaches beyond the cluster: the
// caller's DNS and the preview hosts. Tests fake all of them.
type checkPreviewsDeps struct {
	resolver previewcheck.Resolver
	dialTLS  previewcheck.TLSDialer
	http     previewcheck.HTTPDoer
}

// runCheckPreviews runs the install-level checks and renders the report;
// any failed check makes the command fail.
func runCheckPreviews(ctx context.Context, opts *Options, deps checkPreviewsDeps) error {
	format, err := printer.ParseFormat(opts.Output)
	if err != nil {
		return errUsage(err)
	}
	if opts.AllNamespaces {
		return errUsage(errors.New("check previews reads one install: name its release namespace with -n, not -A"))
	}
	env, err := opts.Connect()
	if err != nil {
		return err
	}
	if env.Namespace == "" {
		return errUsage(errors.New("check previews needs the release namespace: pass -n"))
	}
	ctx, cancel := context.WithTimeout(ctx, checkPreviewsTimeout)
	defer cancel()
	opts.debugf("checking the preview install in namespace %s", env.Namespace)
	report := previewcheck.Install(ctx, previewcheck.Config{
		Reader: env.Client, Namespace: env.Namespace,
		Resolver: deps.resolver, DialTLS: deps.dialTLS, HTTP: deps.http,
	})
	if err := checkreport.Render(opts.Out, format, report, report.Lines()); err != nil {
		return err
	}
	if n := report.Failed(); n > 0 {
		return fmt.Errorf("%d check%s failed", n, plural(n, "", "s"))
	}
	return nil
}
