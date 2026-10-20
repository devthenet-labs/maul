// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Command agent-runner is the coding-agent runtime that executes inside the
// ephemeral Job pod: it drives the classification and remediation harness
// stages against a pre-cloned repository and reports results as an event
// stream on stdout. It never talks to GitHub.
//
// Invoked as `agent-runner sandbox-probe` (sandboxprobe.Command) it instead
// runs the negative-connectivity check the trusted prepare init container
// performs before a repository-declared image gets to run anything, and
// exits sandboxprobe.ExitUnenforced when egress is still open at the end of
// the window; with --mode=wait (a default-image Job's prepare) it holds
// until egress is blocked or the window closes and exits 0 either way.
// Invoked as `agent-runner preflight [harness]` (agentrun.PreflightCommand)
// it runs only the preflight a stage runs first
// on a repository-declared image, prints the verdict on stdout and exits
// agentrun.ExitPreflightFailed when the image cannot run the harness; the
// workstation check `maul check image --run` drives it.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"syscall"

	"github.com/devthenet-labs/maul/internal/agentrun"
	"github.com/devthenet-labs/maul/internal/netprofile"
	"github.com/devthenet-labs/maul/internal/runner"
	"github.com/devthenet-labs/maul/internal/sandboxprobe"
	"github.com/devthenet-labs/maul/internal/templates"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args, os.Getenv, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run is the process: args and the environment in, diagnostics to stderr
// (a stage's stdout is reserved for the envelope event stream the
// controller parses; the preflight subcommand prints its verdict there
// instead), the exit status out.
func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	log := slog.New(slog.NewTextHandler(stderr, nil))

	if len(args) > 1 {
		switch args[1] {
		case sandboxprobe.Command:
			return sandboxprobe.Main(ctx, args[2:], getenv, log)
		case agentrun.PreflightCommand:
			return agentrun.PreflightMain(ctx, args[2:], getenv, &runner.Exec{}, stdout, log)
		}
	}

	cfg, err := config(getenv)
	if err != nil {
		log.LogAttrs(ctx, slog.LevelError, "invalid configuration", slog.Any("error", err))
		return 2
	}
	cfg.Log = log

	if err := agentrun.New(cfg, &runner.Exec{}).Run(ctx); err != nil {
		// The failure was already emitted as a fatal envelope event; exit 2
		// so the Job is marked failed for the controller's orphan handling.
		log.LogAttrs(ctx, slog.LevelError, "agent run failed", slog.Any("error", err))
		return 2
	}
	return 0
}

// config is the stage configuration: agentrun.FromEnv, plus the network
// profile the Job carries (netprofile.EnvProfile), for a profile with
// registry grants its ecosystems and grants (netprofile.EnvEcosystems,
// netprofile.EnvGrants), and for a profile with tunnel hosts its routes and
// hosts (netprofile.EnvRoutes, netprofile.EnvHosts), so a tunnel-only
// profile is never described as a Go route. They are read here through the
// same getenv rather than in FromEnv, so they are no agentrun configuration
// keys, which a repository-image Job would blank. Only the controller sets
// them; a value that does not hold together is a configuration error, never
// a prompt.
func config(getenv func(string) string) (agentrun.Config, error) {
	cfg, err := agentrun.FromEnv(getenv)
	if err != nil {
		return agentrun.Config{}, err
	}
	cfg.NetworkProfile = getenv(netprofile.EnvProfile)
	ecosystems, err := netprofile.ParseEcosystems(getenv(netprofile.EnvEcosystems))
	if err != nil {
		return agentrun.Config{}, err
	}
	grants, err := netprofile.DecodePodGrants(getenv(netprofile.EnvGrants))
	if err != nil {
		return agentrun.Config{}, err
	}
	if err := netprofile.CheckPodRoutes(ecosystems, grants); err != nil {
		return agentrun.Config{}, err
	}
	if len(grants) > 0 && cfg.NetworkProfile == "" {
		return agentrun.Config{}, fmt.Errorf("%s is set without %s", netprofile.EnvGrants, netprofile.EnvProfile)
	}
	tunnel, err := netprofile.ParsePodTunnel(getenv(netprofile.EnvRoutes), getenv(netprofile.EnvHosts))
	if err != nil {
		return agentrun.Config{}, err
	}
	if len(tunnel.Hosts) > 0 {
		if cfg.NetworkProfile == "" {
			return agentrun.Config{}, fmt.Errorf("%s is set without %s", netprofile.EnvHosts, netprofile.EnvProfile)
		}
		// With grants the ecosystems already say whether Go is served: by
		// the public route the routes list, or by a go grant.
		goGrant := slices.ContainsFunc(grants, func(g netprofile.PodGrant) bool {
			return g.Ecosystem == netprofile.EcosystemGo
		})
		if len(ecosystems) > 0 && slices.Contains(ecosystems, netprofile.EcosystemGo) != (tunnel.GoRoute || goGrant) {
			return agentrun.Config{}, fmt.Errorf("%s and %s disagree on the go route", netprofile.EnvRoutes,
				netprofile.EnvEcosystems)
		}
	}
	cfg.NetworkRoutes = templates.PodRoutes(ecosystems, grants).WithTunnel(tunnel, ecosystems)
	return cfg, nil
}
