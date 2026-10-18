// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
	"k8s.io/client-go/kubernetes"

	"github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/broker"
	"github.com/bitwise-media-group/patchy/internal/broker/credstore"
	"github.com/bitwise-media-group/patchy/internal/cli"
	"github.com/bitwise-media-group/patchy/internal/kube"
	"github.com/bitwise-media-group/patchy/internal/telemetry"
	"github.com/bitwise-media-group/patchy/internal/usagewire"
	"github.com/bitwise-media-group/patchy/internal/version"
)

func newServeCmd(opts *cli.Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the egress credential broker",
		RunE:  func(cmd *cobra.Command, _ []string) error { return serve(cmd.Context(), opts) },
	}
	f := cmd.Flags()
	f.String("kubeconfig", "", "kubeconfig path (default: in-cluster config)")
	f.String("health-addr", ":8081", "healthz/readyz probe listen address")
	f.String("token-audience", broker.DefaultAudience, "audience callers' projected tokens must be bound to")
	f.Duration("verdict-ttl", broker.DefaultVerdictTTL, "how long one caller token's TokenReview verdict is cached")
	f.Duration("sse-ping-interval", broker.DefaultPingInterval,
		"idle keep-alive period for event-stream responses; negative disables ping injection")
	f.String("agent-namespace", "patchy-agents", "namespace whose agent service account callers must be")
	f.String("agent-service-account", "patchy-agent", "the only service account the broker answers to")
	f.Int("max-request-bytes", broker.DefaultMaxRequestBytes,
		"largest request body a payload-signing route (bedrock) accepts")
	f.Int("max-anthropic-request-bytes", broker.DefaultMaxAnthropicRequestBytes,
		"largest request body every other route accepts (bodies are buffered for inspection)")

	// Enforcement: every limit is off at zero, so an unconfigured broker
	// behaves as before; the chart sizes them.
	f.Int("requests-per-pod", 0, "requests one agent pod may make in its lifetime; 0 disables")
	f.Int("concurrent-per-pod", 0, "in-flight requests one agent pod may hold; 0 disables")
	f.Duration("concurrency-wait", broker.DefaultConcurrencyWait,
		"how long a request over --concurrent-per-pod waits for one of the pod's slots to free before it is "+
			"refused; 0 takes the default, negative refuses at once")
	f.Int("tokens-per-pod", 0,
		"tokens (input, cache creation, cache read, output) one agent pod may consume; 0 disables")
	f.Int("tokens-per-hour", 0, "broker-wide trailing-hour token ceiling; 0 disables")
	f.String("limit-unit", broker.UnitTokens,
		"what the spend limits count: tokens (--tokens-per-pod, --tokens-per-hour) or cost (--cost-per-pod, "+
			"--cost-per-hour, priced per model from the observed tokens, with the token limits as optional "+
			"backstops and each pod's budget annotation read with get pods in --agent-namespace)")
	f.Int("cost-per-pod", 0,
		"micro-USD one agent pod may spend (the smaller of this and the pod's budget annotation); "+
			"required with --limit-unit=cost, refused otherwise")
	f.Int("cost-per-hour", 0,
		"broker-wide trailing-hour cost ceiling in micro-USD; 0 disables; only with --limit-unit=cost")
	f.Int("max-tokens-ceiling", 0, "largest max_tokens a request may set; 0 disables")
	f.String("model-allowlist", "",
		"comma-separated model ids pods may name (canonical or wire form; dated variants and the "+
			"claude-haiku helper family are admitted); empty admits every model")
	f.String("beta-denylist", "",
		"comma-separated anthropic-beta glob patterns to strip; empty uses the built-in list "+
			"(mcp-client-*, web-fetch-*, code-execution-*, files-api-*, context-1m-*), 'none' strips nothing")
	f.Float64("preauth-requests-per-second", 0,
		"per-source-IP request rate admitted before authentication; 0 disables")
	f.Int("preauth-burst", 0, "per-source-IP burst and in-flight cap before authentication")
	f.Float64("token-reviews-per-second", 0, "broker-wide TokenReview rate, with a short queue; 0 disables")

	// A route exists iff its identifying flag is set; at least one must be.
	f.String("anthropic-api-key-file", "",
		"mounted Secret file holding the Anthropic credential; set to enable the anthropic route")
	f.String("anthropic-auth", "key",
		"anthropic credential mode: key (x-api-key) or token (Authorization bearer — a claude setup-token OAuth token)")
	f.String("anthropic-base-url", broker.DefaultAnthropicBaseURL, "Anthropic API base URL")
	f.String("bedrock-region", "", "AWS region for Bedrock SigV4 signing; set to enable the bedrock route")
	f.String("bedrock-base-url", "",
		"Bedrock runtime base URL (default: https://bedrock-runtime.<bedrock-region>.amazonaws.com)")
	f.String("vertex-region", "",
		"GCP region for Vertex AI, and the only location the route admits; set to enable the vertex route")
	f.String("vertex-project", "", "the only GCP project the vertex route admits; required with --vertex-region")
	f.String("vertex-base-url", "",
		"Vertex AI base URL (default: https://<vertex-region>-aiplatform.googleapis.com)")
	f.String("foundry-resource", "", "Microsoft Foundry resource name; set to enable the foundry route")
	f.String("foundry-base-url", "",
		"Foundry base URL (default: https://<foundry-resource>.services.ai.azure.com)")
	f.String("foundry-auth", "key", "foundry credential mode: key (x-api-key from file) or entra (bearer)")
	f.String("foundry-api-key-file", "", "mounted Secret file holding the Foundry API key (foundry-auth key)")

	// The usage endpoint (per-user credentials design, L2): off unless an
	// address is set, and then only for the named controllers.
	f.String("usage-addr", "",
		"listen address of the usage endpoint the run controllers read a settled run's measured tokens from; "+
			"empty disables it")
	f.String("usage-callers", "",
		"comma-separated <namespace>:<serviceaccount> identities the usage endpoint answers to (the run "+
			"controllers); required with --usage-addr")
	f.String("usage-audience", usagewire.Audience,
		"audience the usage callers' projected tokens must be bound to; must differ from --token-audience")

	// Per-user model credentials (design L3): off unless set.
	f.String("user-credentials", string(broker.UserCredentialsOff),
		"per-user model credentials: off (the org credential pays for everything), attribute (read each "+
			"caller pod's attributed-to annotation and record the person; the org credential still pays) or "+
			"enforce (a run attributed to a person uses that person's own credential or is refused)")
	f.String("credential-records-namespace", "patchy",
		"namespace of the ModelCredential records (the release namespace); with --user-credentials only")
	f.String("credentials-namespace", v1alpha1.DefaultCredentialsNamespace,
		"namespace of the people's credential Secrets; with --user-credentials only")
	f.String("org-credential-is-personal", "",
		"set to acknowledged to run --user-credentials while the org credential is a setup-token (a person's "+
			"Claude subscription keeps paying for every run with no human behind it); refused otherwise")
	f.String("credential-probe-model", broker.DefaultProbeModel,
		"model a setup-token health probe asks for one output token of")
	return cmd
}

// userCredentials reads the per-user credential flags: the engine config
// (without its store) and the two namespaces the store reads.
func userCredentials(opts *cli.Options) (broker.UserCredentialConfig, string, string, error) {
	mode, err := broker.ParseUserCredentialMode(opts.String("user-credentials"))
	if err != nil {
		return broker.UserCredentialConfig{}, "", "", fmt.Errorf("--user-credentials: %w", err)
	}
	cfg := broker.UserCredentialConfig{Mode: mode, Health: broker.HealthConfig{
		ProbeModel: opts.String("credential-probe-model"),
	}}
	switch ack := opts.String("org-credential-is-personal"); ack {
	case "":
	case "acknowledged":
		cfg.OrgCredentialIsPersonal = true
	default:
		return broker.UserCredentialConfig{}, "", "", fmt.Errorf(
			"--org-credential-is-personal %q is not empty or acknowledged", ack)
	}
	return cfg, opts.String("credential-records-namespace"), opts.String("credentials-namespace"), nil
}

// usageConfig reads the usage endpoint's flags: its listen address (empty
// when off) and the engine config. The address and the callers come
// together or not at all, so the endpoint can neither listen for nobody
// nor be configured without a listener.
func usageConfig(opts *cli.Options) (string, broker.UsageConfig, error) {
	addr := opts.String("usage-addr")
	pairs := opts.StringList("usage-callers")
	switch {
	case addr == "" && len(pairs) == 0:
		return "", broker.UsageConfig{}, nil
	case addr == "":
		return "", broker.UsageConfig{}, errors.New("--usage-callers needs --usage-addr")
	case len(pairs) == 0:
		return "", broker.UsageConfig{}, errors.New("--usage-addr needs --usage-callers")
	}
	cfg := broker.UsageConfig{Audience: opts.String("usage-audience")}
	for _, p := range pairs {
		ns, name, ok := strings.Cut(p, ":")
		if !ok || ns == "" || name == "" || strings.Contains(name, ":") {
			return "", broker.UsageConfig{}, fmt.Errorf("--usage-callers: %q is not <namespace>:<serviceaccount>", p)
		}
		cfg.Callers = append(cfg.Callers, "system:serviceaccount:"+ns+":"+name)
	}
	return addr, cfg, nil
}

// upstreams builds the route table from the flags: one route per provider
// whose identifying flag is set, at least one required.
func upstreams(ctx context.Context, opts *cli.Options) (map[string]broker.Upstream, error) {
	routes := map[string]broker.Upstream{}
	builders := []struct {
		name  string
		build func(context.Context, *cli.Options) (*broker.Upstream, error)
	}{
		{"anthropic", anthropicUpstream},
		{"bedrock", bedrockUpstream},
		{"vertex", vertexUpstream},
		{"foundry", foundryUpstream},
	}
	for _, b := range builders {
		up, err := b.build(ctx, opts)
		if err != nil {
			return nil, err
		}
		if up != nil {
			routes[b.name] = *up
		}
	}
	if len(routes) == 0 {
		return nil, errors.New("no upstream configured (set at least one of --anthropic-api-key-file / " +
			"--bedrock-region / --vertex-region / --foundry-resource)")
	}
	return routes, nil
}

func anthropicUpstream(_ context.Context, opts *cli.Options) (*broker.Upstream, error) {
	keyFile := opts.String("anthropic-api-key-file")
	if keyFile == "" {
		return nil, nil
	}
	target, err := broker.ParseTarget(opts.String("anthropic-base-url"))
	if err != nil {
		return nil, err
	}
	var bearer bool
	switch mode := opts.String("anthropic-auth"); mode {
	case "key":
	case "token":
		bearer = true
	default:
		return nil, fmt.Errorf("--anthropic-auth %q is not key or token", mode)
	}
	up := broker.Anthropic(target, keyFile, bearer)
	return &up, nil
}

func bedrockUpstream(ctx context.Context, opts *cli.Options) (*broker.Upstream, error) {
	region := opts.String("bedrock-region")
	if region == "" {
		return nil, nil
	}
	raw := opts.String("bedrock-base-url")
	if raw == "" {
		raw = fmt.Sprintf(broker.BedrockBaseURLFormat, region)
	}
	target, err := broker.ParseTarget(raw)
	if err != nil {
		return nil, err
	}
	up, err := broker.Bedrock(ctx, target, region)
	if err != nil {
		return nil, err
	}
	return &up, nil
}

func vertexUpstream(ctx context.Context, opts *cli.Options) (*broker.Upstream, error) {
	region := opts.String("vertex-region")
	if region == "" {
		return nil, nil
	}
	project := opts.String("vertex-project")
	if project == "" {
		return nil, errors.New("--vertex-region requires --vertex-project: the route admits one project only")
	}
	raw := opts.String("vertex-base-url")
	if raw == "" {
		raw = fmt.Sprintf(broker.VertexBaseURLFormat, region)
	}
	target, err := broker.ParseTarget(raw)
	if err != nil {
		return nil, err
	}
	up, err := broker.Vertex(ctx, target, project, region)
	if err != nil {
		return nil, err
	}
	return &up, nil
}

func foundryUpstream(_ context.Context, opts *cli.Options) (*broker.Upstream, error) {
	resource, raw := opts.String("foundry-resource"), opts.String("foundry-base-url")
	if resource == "" && raw == "" {
		return nil, nil
	}
	if raw == "" {
		raw = fmt.Sprintf(broker.FoundryBaseURLFormat, resource)
	}
	target, err := broker.ParseTarget(raw)
	if err != nil {
		return nil, err
	}
	switch mode := opts.String("foundry-auth"); mode {
	case "key":
		keyFile := opts.String("foundry-api-key-file")
		if keyFile == "" {
			return nil, errors.New("--foundry-auth key requires --foundry-api-key-file")
		}
		up := broker.FoundryKey(target, keyFile)
		return &up, nil
	case "entra":
		up, err := broker.FoundryEntra(target)
		if err != nil {
			return nil, err
		}
		return &up, nil
	default:
		return nil, fmt.Errorf("--foundry-auth %q is not key or entra", mode)
	}
}

func serve(ctx context.Context, opts *cli.Options) error {
	prov, shutdown, err := telemetry.Init(ctx, telemetry.Config{
		Dir:            os.Getenv("PATCHY_TELEMETRY_DIR"),
		Level:          opts.LogLevel,
		ServiceName:    "egress-broker",
		ServiceVersion: version.Version,
	})
	if err != nil {
		prov.Logger.LogAttrs(ctx, slog.LevelWarn, "telemetry disabled", slog.Any("error", err))
	}
	defer func() { _ = shutdown(context.WithoutCancel(ctx)) }()
	log := prov.Logger

	restCfg, err := kube.RestConfig(opts.String("kubeconfig"))
	if err != nil {
		return fmt.Errorf("kubernetes config: %w", err)
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("kubernetes clientset: %w", err)
	}

	routes, err := upstreams(ctx, opts)
	if err != nil {
		return err
	}
	usageAddr, usage, err := usageConfig(opts)
	if err != nil {
		return err
	}
	creds, recordsNS, secretsNS, err := userCredentials(opts)
	if err != nil {
		return err
	}
	var store *credstore.Store
	if creds.Mode != broker.UserCredentialsOff {
		if store, err = credstore.New(restCfg, cs, recordsNS, secretsNS); err != nil {
			return err
		}
		creds.Store = store
	}
	// --beta-denylist: empty keeps the engine's built-in list, "none" strips
	// nothing, anything else is the operator's own patterns.
	betas := opts.StringList("beta-denylist")
	disableBetas := len(betas) == 1 && strings.EqualFold(betas[0], "none")
	if disableBetas {
		betas = nil
	}
	srv, err := broker.New(cs, broker.Config{
		Audience:                 opts.String("token-audience"),
		AgentNamespace:           opts.String("agent-namespace"),
		AgentServiceAccount:      opts.String("agent-service-account"),
		VerdictTTL:               opts.Duration("verdict-ttl"),
		PingInterval:             opts.Duration("sse-ping-interval"),
		MaxRequestBytes:          int64(opts.Int("max-request-bytes")),
		MaxAnthropicRequestBytes: int64(opts.Int("max-anthropic-request-bytes")),
		Limits: broker.Limits{
			RequestsPerPod:   int64(opts.Int("requests-per-pod")),
			ConcurrentPerPod: int64(opts.Int("concurrent-per-pod")),
			ConcurrencyWait:  opts.Duration("concurrency-wait"),
			TokensPerPod:     int64(opts.Int("tokens-per-pod")),
			TokensPerHour:    int64(opts.Int("tokens-per-hour")),
			Unit:             opts.String("limit-unit"),
			CostPerPod:       int64(opts.Int("cost-per-pod")),
			CostPerHour:      int64(opts.Int("cost-per-hour")),
			MaxTokensCeiling: int64(opts.Int("max-tokens-ceiling")),
			ModelAllowlist:   opts.StringList("model-allowlist"),
		},
		BetaDenylist:             betas,
		DisableBetaDenylist:      disableBetas,
		PreauthRequestsPerSecond: opts.Float("preauth-requests-per-second"),
		PreauthBurst:             opts.Int("preauth-burst"),
		TokenReviewsPerSecond:    opts.Float("token-reviews-per-second"),
		Upstreams:                routes,
		Usage:                    usage,
		UserCredentials:          creds,
	}, log)
	if err != nil {
		return err
	}

	// The proxy listener has no write or handler deadline on purpose:
	// evaluation runs stream for hours. ReadHeaderTimeout still bounds a
	// stuck caller.
	proxySrv := &http.Server{
		Addr:              opts.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	healthSrv := &http.Server{
		Addr:              opts.String("health-addr"),
		Handler:           srv.HealthHandler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	servers := []*http.Server{proxySrv, healthSrv}
	// The usage endpoint gets a listener of its own, so the chart's
	// NetworkPolicy can open it to the run controllers alone.
	if h := srv.UsageHandler(); h != nil {
		servers = append(servers, &http.Server{
			Addr:              usageAddr,
			Handler:           h,
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       30 * time.Second,
			WriteTimeout:      30 * time.Second,
		})
	}

	names := slices.Sorted(maps.Keys(routes))
	log.LogAttrs(ctx, slog.LevelInfo, "egress-broker starting",
		slog.String("listen_addr", proxySrv.Addr),
		slog.String("health_addr", healthSrv.Addr),
		slog.String("usage_addr", usageAddr),
		slog.String("limit_unit", opts.String("limit-unit")),
		slog.String("user_credentials", string(creds.Mode)),
		slog.Any("routes", names))

	g, ctx := errgroup.WithContext(ctx)
	if store != nil {
		// The credential informers and the health probes. Neither gates
		// readiness, which stays the org credential's alone.
		g.Go(func() error { return store.Start(ctx) })
		g.Go(func() error { srv.Run(ctx); return nil })
	}
	for _, s := range servers {
		g.Go(func() error {
			if err := s.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		})
		g.Go(func() error {
			<-ctx.Done()
			shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			return s.Shutdown(shutdownCtx)
		})
	}
	if err := g.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
