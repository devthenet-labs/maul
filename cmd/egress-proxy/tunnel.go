// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/bitwise-media-group/patchy/internal/cli"
	"github.com/bitwise-media-group/patchy/internal/egressproxy"
	"github.com/bitwise-media-group/patchy/internal/netprofile"
)

// addTunnelFlags registers the CONNECT tunnel's flags (§2.5 of the step-3
// spec; PATCHY_TUNNEL_* in the environment). A zero value takes the
// default, except --tunnel-progress-interval, where 0 turns the progress
// line off. No flag turns the tunnel on: a profile with hosts does.
func addTunnelFlags(cmd *cobra.Command) {
	f := cmd.Flags()
	f.Int("tunnel-max-concurrent", egressproxy.DefaultTunnelMaxConcurrent, "open tunnels across every source")
	f.Int("tunnel-max-concurrent-per-source", egressproxy.DefaultTunnelPerSourceConcurrent,
		"open tunnels from one source IP")
	f.Float64("tunnel-rate-per-source", egressproxy.DefaultTunnelPerSourceRate,
		"new tunnels per second from one source IP")
	f.Int("tunnel-burst-per-source", egressproxy.DefaultTunnelPerSourceBurst, "new-tunnel burst of one source IP")
	f.Duration("tunnel-limit-wait", egressproxy.DefaultTunnelLimitWait,
		"how long a CONNECT queues for the tunnel limits before it is answered 503")
	f.Duration("tunnel-dial-timeout", egressproxy.DefaultTunnelDialTimeout, "bound on a tunnel's resolve and connect")
	f.Duration("tunnel-hello-timeout", egressproxy.DefaultTunnelHelloTimeout,
		"bound on the wait from the 200 to a complete TLS ClientHello")
	f.Duration("tunnel-idle-timeout", egressproxy.DefaultTunnelIdleTimeout,
		"a tunnel with no byte in either direction for this long is closed")
	f.Duration("tunnel-max-duration", egressproxy.DefaultTunnelMaxDuration, "absolute lifetime of one tunnel")
	f.Int64("tunnel-max-upload-bytes", egressproxy.DefaultTunnelMaxUploadBytes,
		"client-to-upstream bytes on one tunnel (a profile's upload.perTunnel overrides it)")
	f.Int64("tunnel-max-download-bytes", egressproxy.DefaultTunnelMaxDownloadBytes,
		"upstream-to-client bytes on one tunnel")
	f.Int64("tunnel-source-upload-budget", egressproxy.DefaultTunnelSourceUploadBudget,
		"client-to-upstream bytes of every tunnel of one source IP on one profile, per budget window "+
			"(a profile's upload.perSource overrides it)")
	f.Int64("tunnel-source-download-budget", egressproxy.DefaultTunnelSourceDownloadBudget,
		"upstream-to-client bytes of every tunnel of one source IP on one profile, per budget window")
	f.Duration("tunnel-source-budget-idle", egressproxy.DefaultTunnelSourceBudgetIdle,
		"a source's budget entry expires this long after its last tunnel closed; at least "+
			"--tunnel-min-budget-idle")
	f.Duration("tunnel-min-budget-idle", egressproxy.DefaultTunnelMinBudgetIdle,
		"the floor of --tunnel-source-budget-idle: the longest agent Job deadline, so no pod can pause long "+
			"enough to refill its own budget")
	f.Int("tunnel-max-sources", egressproxy.DefaultTunnelMaxSources,
		"budget-table entries per profile; a full table refuses a new source and never evicts one")
	f.Duration("tunnel-progress-interval", egressproxy.DefaultTunnelProgressInterval,
		"period of an open tunnel's progress audit line; 0 turns it off")
	f.String("tunnel-deny-cidrs", "",
		"comma-separated CIDRs the tunnel never dials, beside --deny-cidrs (the Kubernetes API server's "+
			"addresses); the Go and registry routes never read them")
}

// configureTunnel validates the tunnel flags into the engine's limits. It
// refuses an unparsable CIDR and a budget window below its floor, naming
// the flags.
func configureTunnel(opts *cli.Options) (egressproxy.TunnelLimits, error) {
	deny, err := egressproxy.ParseDenyCIDRs(opts.StringList("tunnel-deny-cidrs"))
	if err != nil {
		return egressproxy.TunnelLimits{}, fmt.Errorf("--tunnel-deny-cidrs: %w", err)
	}
	idle, minIdle := opts.Duration("tunnel-source-budget-idle"), opts.Duration("tunnel-min-budget-idle")
	if idle > 0 && minIdle > 0 && idle < minIdle {
		return egressproxy.TunnelLimits{}, fmt.Errorf("--tunnel-source-budget-idle %s is below "+
			"--tunnel-min-budget-idle %s: a pod could pause long enough to refill its own byte budget", idle, minIdle)
	}
	progress := opts.Duration("tunnel-progress-interval")
	if progress == 0 {
		progress = -1 // the engine's "off"; its zero is the default
	}
	return egressproxy.TunnelLimits{
		MaxConcurrent:        opts.Int("tunnel-max-concurrent"),
		PerSourceConcurrent:  opts.Int("tunnel-max-concurrent-per-source"),
		PerSourceRate:        opts.Float("tunnel-rate-per-source"),
		PerSourceBurst:       opts.Int("tunnel-burst-per-source"),
		Wait:                 opts.Duration("tunnel-limit-wait"),
		DialTimeout:          opts.Duration("tunnel-dial-timeout"),
		HelloTimeout:         opts.Duration("tunnel-hello-timeout"),
		IdleTimeout:          opts.Duration("tunnel-idle-timeout"),
		MaxDuration:          opts.Duration("tunnel-max-duration"),
		MaxUploadBytes:       int64(opts.Int("tunnel-max-upload-bytes")),
		MaxDownloadBytes:     int64(opts.Int("tunnel-max-download-bytes")),
		SourceUploadBudget:   int64(opts.Int("tunnel-source-upload-budget")),
		SourceDownloadBudget: int64(opts.Int("tunnel-source-download-budget")),
		SourceBudgetIdle:     idle,
		MinBudgetIdle:        minIdle,
		MaxSources:           opts.Int("tunnel-max-sources"),
		ProgressInterval:     progress,
		Deny:                 deny,
	}, nil
}

// logTunnel writes the tunnel's startup lines: one per profile with hosts,
// a warning per client preset whose usual hosts the profile does not list,
// and a warning when profiles tunnel and no tunnel deny range is set (a
// public API server endpoint under a name the deny table does not know
// would then be reachable if an operator listed it).
func logTunnel(ctx context.Context, log *slog.Logger, set netprofile.Set, l egressproxy.TunnelLimits) {
	tunnels := false
	for _, p := range set.Profiles() {
		if !p.HasTunnel() {
			continue
		}
		tunnels = true
		log.LogAttrs(ctx, slog.LevelInfo, "tunnel configured",
			slog.String("profile", p.Name),
			slog.Int("port", p.Port),
			slog.Any("hosts", []string(p.Hosts)),
			slog.Any("clients", p.Clients))
		for _, w := range p.ClientWarnings() {
			log.LogAttrs(ctx, slog.LevelWarn, w, slog.String("profile", p.Name))
		}
	}
	if tunnels && len(l.Deny) == 0 {
		log.LogAttrs(ctx, slog.LevelWarn, "profiles tunnel and --tunnel-deny-cidrs is empty: the tunnel refuses "+
			"managed control-plane names and cluster addresses, but a public Kubernetes API endpoint under "+
			"another name is refused only if its addresses are listed (attribution.apiServerCIDRs or "+
			"egressProxy.tunnel.denyCIDRs)")
	}
}
