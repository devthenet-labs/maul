// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
	"k8s.io/client-go/kubernetes"

	"github.com/bitwise-media-group/patchy/internal/cli"
	"github.com/bitwise-media-group/patchy/internal/egressproxy"
	"github.com/bitwise-media-group/patchy/internal/egressproxy/credfile"
	"github.com/bitwise-media-group/patchy/internal/egressproxy/podindex"
	"github.com/bitwise-media-group/patchy/internal/kube"
	"github.com/bitwise-media-group/patchy/internal/netprofile"
	"github.com/bitwise-media-group/patchy/internal/telemetry"
	"github.com/bitwise-media-group/patchy/internal/version"
)

// Server timeouts for the profile listeners. There is no server-wide write
// timeout: a module zip streams for up to --request-timeout, and the engine
// sets each forwarded response's write deadline to that request's own
// deadline, so a client that stops reading cannot hold its limit slots past
// it.
const (
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 90 * time.Second
	maxHeaderBytes    = 16 << 10
)

// tunnelDrainTimeout bounds the wait at exit for the open tunnels' close
// lines once they are closed; ending a tunnel is a close and one log line,
// so it is short.
const tunnelDrainTimeout = 5 * time.Second

func newServeCmd(opts *cli.Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the egress proxy",
		RunE:  func(cmd *cobra.Command, _ []string) error { return serve(cmd.Context(), opts) },
	}
	addEngineFlags(cmd)
	f := cmd.Flags()
	f.Bool("attribute-pods", false,
		"attribute source IPs to agent pods in the audit line (best-effort; needs get/list/watch on pods "+
			"in --agent-namespace)")
	f.String("agent-namespace", "patchy-agents", "namespace of the agent pods attributed (--attribute-pods only)")
	f.String("kubeconfig", "", "kubeconfig path (default: in-cluster config; --attribute-pods only)")
	return cmd
}

// addEngineFlags registers the flags serve and probe share: everything the
// engine is built from.
func addEngineFlags(cmd *cobra.Command) {
	f := cmd.Flags()
	f.String("network-profiles", "",
		"the operator's network profiles as the chart renders them (a JSON array); required")
	f.String("health-addr", ":8081", "healthz/readyz probe listen address")
	f.String("go-proxy-upstream", egressproxy.DefaultGoProxy, "module proxy every go route forwards to (https only)")
	f.String("go-sumdb-upstream", egressproxy.DefaultSumDB,
		"checksum database every sumdb route forwards to (https only)")
	f.String("deny-cidrs", "",
		"comma-separated CIDRs never dialled, beside the built-in private, link-local and reserved ranges "+
			"(the cluster's pod, service and node CIDRs and its DNS address)")
	f.Int("max-concurrent", egressproxy.DefaultMaxConcurrent, "requests in flight across every source")
	f.Int("max-concurrent-per-source", egressproxy.DefaultPerSourceConcurrent,
		"requests in flight from one source IP")
	f.Float64("requests-per-second-per-source", egressproxy.DefaultPerSourceRate, "request rate of one source IP")
	f.Int("burst-per-source", egressproxy.DefaultPerSourceBurst, "request burst of one source IP")
	f.Duration("limit-wait", egressproxy.DefaultLimitWait,
		"how long a request queues for the limits above before it is answered 503")
	f.Duration("request-timeout", egressproxy.DefaultRequestTimeout,
		"bound on one whole request, the body stream included")
	f.Duration("upstream-header-timeout", egressproxy.DefaultHeaderTimeout,
		"bound on the wait for an upstream's response headers")
	f.Int64("max-zip-bytes", egressproxy.DefaultMaxZipBytes, "largest module zip passed through")
	f.Bool("suspended", false,
		"answer every profile request 410 suspended: the operator's immediate kill switch")
	f.String("registries", "",
		"the operator's credentialed upstream registries as the chart renders them (a JSON array; "+
			"environment PATCHY_EGRESS_REGISTRIES); empty: no registry route")
	f.String("registry-credentials-dir", defaultCredentialsDir,
		"directory of the mounted registry credentials (<registry>/token, or <registry>/username and password)")
	f.String("registry-ca-dir", defaultCADir, "directory of the registries' CA bundles (<registry>.pem)")
	f.Int64("max-artifact-bytes", egressproxy.DefaultMaxArtifactBytes,
		"largest npm tarball, pip file or go zip passed through a registry route, unless the registry sets its own")
	f.Duration("credential-poll-interval", credfile.DefaultInterval,
		"how often the mounted registry credentials are re-read (at least 5s)")
	f.String("deny-hosts", "",
		"comma-separated hosts no registry, file host or redirect hop may be, each with every name below it, "+
			"beside the built-in forge and model API table and the profiles' forge hosts (the egress broker's "+
			"base-URL overrides)")
	f.String("always-deny-cidrs", "",
		"comma-separated CIDRs never dialled even when an internal registry lists them (cluster DNS, the API "+
			"server endpoints), beside the built-in always-denied ranges and KUBERNETES_SERVICE_HOST")
	addTunnelFlags(cmd)
}

// settings is the validated configuration of one serve run.
type settings struct {
	engine     egressproxy.Config
	healthAddr string
	// credentialsDir and pollInterval are the file-backed credentials'.
	credentialsDir string
	pollInterval   time.Duration
	// attribution: whether to index agent pods, where, and how to reach
	// the API server.
	attributePods  bool
	agentNamespace string
	kubeconfig     string
}

// configure validates the flags into the engine configuration. It refuses
// an empty or invalid profile set, a profile on the health port, a
// non-https upstream and an unparsable CIDR.
func configure(opts *cli.Options) (settings, error) {
	raw := opts.String("network-profiles")
	set, err := netprofile.Parse([]byte(raw))
	if err != nil {
		return settings{}, fmt.Errorf("--network-profiles: %w", err)
	}
	if set.Len() == 0 {
		return settings{}, errors.New("--network-profiles: no profile configured")
	}
	healthAddr := opts.String("health-addr")
	_, healthPort, err := net.SplitHostPort(healthAddr)
	if err != nil {
		return settings{}, fmt.Errorf("--health-addr %q: %w", healthAddr, err)
	}
	for _, p := range set.Profiles() {
		if strconv.Itoa(p.Port) == healthPort {
			return settings{}, fmt.Errorf("profile %s: port %d is --health-addr's port", p.Name, p.Port)
		}
	}
	goProxy, err := httpsUpstream("go-proxy-upstream", opts.String("go-proxy-upstream"))
	if err != nil {
		return settings{}, err
	}
	sumDB, err := httpsUpstream("go-sumdb-upstream", opts.String("go-sumdb-upstream"))
	if err != nil {
		return settings{}, err
	}
	deny, err := egressproxy.ParseDenyCIDRs(opts.StringList("deny-cidrs"))
	if err != nil {
		return settings{}, fmt.Errorf("--deny-cidrs: %w", err)
	}
	agentNamespace := opts.String("agent-namespace")
	if opts.Bool("attribute-pods") && agentNamespace == "" {
		return settings{}, errors.New("--attribute-pods needs --agent-namespace")
	}
	reg, err := configureRegistries(opts, set)
	if err != nil {
		return settings{}, err
	}
	tunnel, err := configureTunnel(opts)
	if err != nil {
		return settings{}, err
	}
	return settings{
		credentialsDir: opts.String("registry-credentials-dir"),
		pollInterval:   reg.pollInterval,
		healthAddr:     healthAddr,
		attributePods:  opts.Bool("attribute-pods"),
		agentNamespace: agentNamespace,
		kubeconfig:     opts.String("kubeconfig"),
		engine: egressproxy.Config{
			Profiles: set,
			GoProxy:  goProxy,
			SumDB:    sumDB,
			Policy:   egressproxy.AddrPolicy{Deny: deny},
			Limits: egressproxy.Limits{
				MaxConcurrent:       opts.Int("max-concurrent"),
				PerSourceConcurrent: opts.Int("max-concurrent-per-source"),
				PerSourceRate:       opts.Float("requests-per-second-per-source"),
				PerSourceBurst:      opts.Int("burst-per-source"),
				Wait:                opts.Duration("limit-wait"),
				RequestTimeout:      opts.Duration("request-timeout"),
				HeaderTimeout:       opts.Duration("upstream-header-timeout"),
				MaxZipBytes:         int64(opts.Int("max-zip-bytes")),
			},
			TunnelLimits:      tunnel,
			Suspended:         opts.Bool("suspended"),
			Registries:        reg.set,
			MaxArtifactBytes:  reg.maxArtifactBytes,
			AlwaysDeny:        reg.alwaysDeny,
			DenyHosts:         reg.denyHosts,
			RegistryCAs:       reg.cas,
			SelfCheckInterval: reg.pollInterval,
		},
	}, nil
}

// httpsUpstream parses an upstream flag, which admits https only: there is
// no insecure or loopback upstream on the binary.
func httpsUpstream(flag, raw string) (*url.URL, error) {
	u, err := egressproxy.ParseUpstream(raw)
	if err != nil {
		return nil, fmt.Errorf("--%s: %w", flag, err)
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("--%s %q: the upstream must be https", flag, raw)
	}
	return u, nil
}

func serve(ctx context.Context, opts *cli.Options) error {
	prov, shutdown, err := telemetry.Init(ctx, telemetry.Config{
		Dir:            os.Getenv("PATCHY_TELEMETRY_DIR"),
		Level:          opts.LogLevel,
		ServiceName:    "egress-proxy",
		ServiceVersion: version.Version,
	})
	if err != nil {
		prov.Logger.LogAttrs(ctx, slog.LevelWarn, "telemetry disabled", slog.Any("error", err))
	}
	defer func() { _ = shutdown(context.WithoutCancel(ctx)) }()
	log := prov.Logger

	s, err := configure(opts)
	if err != nil {
		return err
	}
	if s.attributePods {
		idx, err := podIndex(s)
		if err != nil {
			return err
		}
		idx.Start(ctx, log)
		s.engine.Pods = idx
	}
	src, err := buildSources(ctx, s, log)
	if err != nil {
		return err
	}
	if src != nil {
		s.engine.Credentials = src
		defer src.close()
		go src.run(ctx)
	}
	srv, err := egressproxy.New(s.engine, log)
	if err != nil {
		return err
	}
	logRegistries(ctx, log, s.engine.Registries)
	logTunnel(ctx, log, s.engine.Profiles, s.engine.TunnelLimits)
	return run(ctx, srv, s.healthAddr, net.Listen, log)
}

// podIndex builds the attribution index over the agent namespace's pods.
func podIndex(s settings) (*podindex.Index, error) {
	restCfg, err := kube.RestConfig(s.kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("--attribute-pods: kubernetes config: %w", err)
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("--attribute-pods: kubernetes clientset: %w", err)
	}
	return podindex.New(cs, s.agentNamespace)
}

// listenFunc binds a listener; tests substitute one.
type listenFunc func(network, address string) (net.Listener, error)

// run binds the health listener and one listener per profile, marks the
// engine ready once every profile listener is bound, and serves until ctx
// ends.
func run(ctx context.Context, srv *egressproxy.Server, healthAddr string, listen listenFunc, log *slog.Logger) error {
	type bound struct {
		ln  net.Listener
		srv *http.Server
	}
	var servers []bound
	closeAll := func() {
		for _, b := range servers {
			_ = b.ln.Close()
		}
	}
	healthLn, err := listen("tcp", healthAddr)
	if err != nil {
		return fmt.Errorf("health listener %s: %w", healthAddr, err)
	}
	servers = append(servers, bound{healthLn, &http.Server{
		Handler:           srv.HealthHandler(),
		ReadHeaderTimeout: readHeaderTimeout,
	}})
	profiles := srv.Profiles().Profiles()
	names := make([]string, 0, len(profiles))
	for _, p := range profiles {
		h, err := srv.Handler(p.Name)
		if err != nil {
			closeAll()
			return err
		}
		addr := ":" + strconv.Itoa(p.Port)
		ln, err := listen("tcp", addr)
		if err != nil {
			closeAll()
			return fmt.Errorf("profile %s listener %s: %w", p.Name, addr, err)
		}
		hs := &http.Server{
			Handler:           h,
			ReadHeaderTimeout: readHeaderTimeout,
			IdleTimeout:       idleTimeout,
			MaxHeaderBytes:    maxHeaderBytes,
		}
		// HTTP/1 only, and Shutdown closes the hijacked tunnels.
		srv.ConfigureListener(hs)
		servers = append(servers, bound{ln, hs})
		names = append(names, fmt.Sprintf("%s:%d", p.Name, p.Port))
	}
	srv.SetReady()
	log.LogAttrs(ctx, slog.LevelInfo, "egress-proxy starting",
		slog.String("health_addr", healthLn.Addr().String()),
		slog.Any("profiles", names))

	g, ctx := errgroup.WithContext(ctx)
	for _, b := range servers {
		g.Go(func() error {
			if err := b.srv.Serve(b.ln); !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		})
		g.Go(func() error {
			<-ctx.Done()
			shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			return b.srv.Shutdown(shutdownCtx)
		})
	}
	err = g.Wait()
	// Shutdown returns without waiting for the hijacked tunnels; wait for
	// their close lines here, bounded, before the process exits.
	drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), tunnelDrainTimeout)
	defer cancel()
	if derr := srv.DrainTunnels(drainCtx); derr != nil {
		log.LogAttrs(drainCtx, slog.LevelWarn, "egress-proxy: tunnels still closing at exit",
			slog.String("error", derr.Error()))
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
