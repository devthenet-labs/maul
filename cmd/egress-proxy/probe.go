// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/mod/module"

	"github.com/bitwise-media-group/patchy/internal/cli"
	"github.com/bitwise-media-group/patchy/internal/egressproxy"
	"github.com/bitwise-media-group/patchy/internal/netprofile"
)

// errProbeFailed is a probe whose request did not end in a 200.
var errProbeFailed = errors.New("probe failed")

func newProbeCmd(opts *cli.Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "probe --registry <name> [--package <package>]",
		Short: "Send one request through a registry route from inside the proxy pod",
		Long: "probe builds the engine from the same configuration serve uses (flags and PATCHY_* variables), " +
			"loads or mints the registry's credential itself, and sends one request for the package (the " +
			"registry's healthPackage when --package is not given) through the first profile granting the " +
			"registry. It prints the outcome, the statuses and the credential's kind, never a credential or a " +
			"response body. Run it in the proxy pod: kubectl exec deploy/<release>-egress-proxy -- " +
			"/usr/local/bin/controller probe --registry <name>.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runProbe(cmd.Context(), opts, cmd.OutOrStdout())
		},
	}
	addEngineFlags(cmd)
	f := cmd.Flags()
	f.String("registry", "", "the registry to probe; required")
	f.String("package", "", "the package to ask for: an npm name, a pip project or a Go module path "+
		"(default: the registry's healthPackage)")
	f.Duration("probe-credential-wait", 30*time.Second, "how long to wait for the credential to load or mint")
	return cmd
}

// runProbe sends one request through the engine and prints the outcome.
func runProbe(ctx context.Context, opts *cli.Options, out io.Writer) error {
	name := opts.String("registry")
	if name == "" {
		return errors.New("--registry is required")
	}
	s, err := configure(opts)
	if err != nil {
		return err
	}
	reg, ok := s.engine.Registries.Get(name)
	if !ok {
		return fmt.Errorf("--registry %s: %w", name, errNoRegistry)
	}
	prof, ok := grantingProfile(s.engine.Profiles, name)
	if !ok {
		return fmt.Errorf("--registry %s: no profile grants it", name)
	}
	pkg := opts.String("package")
	if pkg == "" {
		pkg = reg.HealthPackage
	}
	path, err := probePath(reg, pkg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	src, err := buildSources(ctx, s, opts.Log)
	if err != nil {
		return err
	}
	if src != nil {
		s.engine.Credentials = src
		defer src.close()
		go src.run(ctx)
		waitForCredential(ctx, src, name, opts.Duration("probe-credential-wait"))
	}
	var audit bytes.Buffer
	srv, err := egressproxy.New(s.engine, slog.New(slog.NewJSONHandler(&audit, nil)))
	if err != nil {
		return err
	}
	res, err := egressproxy.Probe(ctx, srv, prof.Name, path, &audit)
	if err != nil {
		return err
	}
	return reportProbe(out, name, prof.Name, path, res)
}

// reportProbe prints the probe's one line, the one the runbook reads, and
// fails unless the request was forwarded and answered 200.
func reportProbe(out io.Writer, registry, profile, path string, res egressproxy.ProbeResult) error {
	outcome := res.Outcome
	if res.OK() {
		outcome = "ok"
	}
	_, _ = fmt.Fprintf(out, "registry=%s profile=%s path=%s status=%d upstream_status=%d outcome=%s reason=%s "+
		"credential=%s\n", registry, profile, path, res.Status, res.UpstreamStatus, outcome, res.Reason,
		res.Credential)
	if !res.OK() {
		return errProbeFailed
	}
	return nil
}

// waitForCredential waits until the registry's credential is there, up to
// wait; a registry without a login has nothing to wait for.
func waitForCredential(ctx context.Context, src *sources, name string, wait time.Duration) {
	if _, ok := src.by[name]; !ok {
		return
	}
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		if _, err := src.Get(name); err == nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// probePath is the registry route path a package's first request takes: an
// npm packument, a pip simple page or a Go module's version list.
func probePath(r netprofile.Registry, pkg string) (string, error) {
	if pkg == "" {
		return "", fmt.Errorf("registry %s names no healthPackage: pass --package", r.Name)
	}
	base := "/r/" + r.Name + "/"
	switch r.Ecosystem {
	case netprofile.EcosystemNPM:
		scope, n, err := netprofile.NPMName(pkg)
		if err != nil {
			return "", fmt.Errorf("--package: %w", err)
		}
		if scope != "" {
			return base + "@" + scope + "%2f" + n, nil
		}
		return base + n, nil
	case netprofile.EcosystemPyPI:
		if !netprofile.PyName(pkg) {
			return "", fmt.Errorf("--package %q is not a pip project name", pkg)
		}
		return base + "simple/" + netprofile.PyNormalize(pkg) + "/", nil
	default:
		esc, err := module.EscapePath(pkg)
		if err != nil {
			return "", fmt.Errorf("--package: %w", err)
		}
		return (&url.URL{Path: base + esc + "/@v/list"}).EscapedPath(), nil
	}
}
