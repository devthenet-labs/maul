// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/devthenet-labs/maul/internal/attribution"
	"github.com/devthenet-labs/maul/internal/changeset"
	"github.com/devthenet-labs/maul/internal/cli"
	"github.com/devthenet-labs/maul/internal/controller/remediation"
	"github.com/devthenet-labs/maul/internal/controller/rollup"
	"github.com/devthenet-labs/maul/internal/credgate"
	"github.com/devthenet-labs/maul/internal/credgate/kubecred"
	"github.com/devthenet-labs/maul/internal/forge"
	"github.com/devthenet-labs/maul/internal/jobs"
	"github.com/devthenet-labs/maul/internal/kube"
	"github.com/devthenet-labs/maul/internal/priority"
	"github.com/devthenet-labs/maul/internal/runnercfg"
	"github.com/devthenet-labs/maul/internal/runnerguard"
	"github.com/devthenet-labs/maul/internal/schedule"
	"github.com/devthenet-labs/maul/internal/telemetry"
	"github.com/devthenet-labs/maul/internal/usagerollup"
	"github.com/devthenet-labs/maul/internal/version"
)

func newServeCmd(opts *cli.Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run queue admission, the remediation agent scheduler, and the rollup/TTL loop",
		RunE:  func(cmd *cobra.Command, _ []string) error { return serve(cmd.Context(), opts) },
	}
	f := cmd.Flags()
	f.String("namespace", "", "namespace the maul resources live in (default: POD_NAMESPACE)")
	f.String("kubeconfig", "", "kubeconfig path (default: in-cluster config)")
	f.String("health-addr", ":8081", "healthz/readyz probe listen address")
	f.Int("max-attempts", 2, "remediation attempts per finding before it fails")
	f.Int("max-concurrent-remediations", 1, "simultaneously running remediation jobs")
	f.Duration("priority-aging-interval", 24*time.Hour, "wait per effective-priority point of aging boost")
	f.Int("priority-aging-cap", 25, "maximum aging boost")
	f.Float64("priority-weight-severity", priority.DefaultWeights.Severity,
		"scheduling-priority weight of the scanner severity")
	f.Float64("priority-weight-exploitability", priority.DefaultWeights.Exploitability,
		"scheduling-priority weight of the assessed exploitability")
	f.Float64("priority-weight-likelihood", priority.DefaultWeights.Likelihood,
		"scheduling-priority weight of the assessed likelihood")
	f.Float64("priority-weight-impact", priority.DefaultWeights.Impact,
		"scheduling-priority weight of the assessed impact")
	f.Duration("finding-ttl", rollup.DefaultTTL,
		"how long completed findings are kept before deletion; 0 keeps them forever")

	f.String("agent-namespace", "maul-agents", "namespace the agent Jobs run in")
	f.String("agent-service-account", "maul-agent", "service account for the agent Jobs")
	runnercfg.RegisterFlags(f)
	runnercfg.RegisterRepositoryImageFlags(f)
	runnercfg.RegisterNetworkProfileFlags(f)
	runnercfg.RegisterAgentResourceFlags(f)
	runnercfg.RegisterAgentDNSFlag(f)
	runnercfg.RegisterAgentsPausedFlag(f)
	runnercfg.RegisterAttributionFlag(f)
	runnercfg.RegisterUserCredentialsFlags(f)
	runnercfg.RegisterUsageFlags(f)
	f.Duration("job-deadline", time.Hour, "activeDeadlineSeconds for an agent Job")
	f.Duration("job-ttl", time.Hour, "ttlSecondsAfterFinished for a finished agent Job")

	f.String("model-allowlist", "anthropic/claude-sonnet-5,anthropic/claude-opus-5",
		"canonical model ids remediation may run (the investigation's choice is clamped to this)")
	f.String("remediate-model", "anthropic/claude-sonnet-5",
		"canonical default model when the report's suggestion is missing or unusable (its harness is derived)")
	f.Duration("remediate-timeout", 45*time.Minute, "wall-clock limit for the remediation stage")
	f.Duration("remediate-idle-timeout", 20*time.Minute,
		"end a remediation run that makes no progress (no model turn, no tool result) for this long; 0 disables")
	f.Int("remediate-auto-max-turns", 80,
		"agent turns spent on an unattended fix, and the line past which an estimate needs approval")
	f.Int("remediate-auto-token-budget", 400000,
		"output tokens spent on an unattended fix, and the line past which an estimate needs approval")
	f.Int("remediate-manual-max-turns", 240,
		"most agent turns a human approval can grant")
	f.Int("remediate-manual-token-budget", 1200000,
		"most output tokens a human approval can grant")
	f.Int("changeset-max-entries", changeset.DefaultMaxEntries,
		"most files (upserts plus deletes) a changeset from a repository-image run may touch before it is "+
			"rejected without any forge call")
	return cmd
}

// agentEnv is the MAUL_* configuration every remediation pod receives. The
// per-Job harness and model are carried on the Job spec, not here; the
// allowlist is enforced controller-side (the spawner), so the pod does not
// need it.
func agentEnv(opts *cli.Options) map[string]string {
	return map[string]string{
		"MAUL_REMEDIATE_TIMEOUT":             opts.Duration("remediate-timeout").String(),
		"MAUL_REMEDIATE_IDLE_TIMEOUT":        opts.Duration("remediate-idle-timeout").String(),
		"MAUL_REMEDIATE_AUTO_MAX_TURNS":      fmt.Sprint(opts.Int("remediate-auto-max-turns")),
		"MAUL_REMEDIATE_AUTO_TOKEN_BUDGET":   fmt.Sprint(opts.Int("remediate-auto-token-budget")),
		"MAUL_REMEDIATE_MANUAL_MAX_TURNS":    fmt.Sprint(opts.Int("remediate-manual-max-turns")),
		"MAUL_REMEDIATE_MANUAL_TOKEN_BUDGET": fmt.Sprint(opts.Int("remediate-manual-token-budget")),
	}
}

// modes are the switches ownSettings reads.
type modes struct {
	attribution attribution.Mode
	credentials credgate.Mode
	accountURL  string
}

// ownSettings checks the settings remediation-controller alone reads and
// returns --attribution-mode and --user-credentials, so a bad value stops
// startup.
func ownSettings(opts *cli.Options) (modes, error) {
	if opts.Int("changeset-max-entries") <= 0 {
		return modes{}, errors.New("--changeset-max-entries must be positive")
	}
	if opts.Duration("remediate-idle-timeout") < 0 {
		return modes{}, errors.New("--remediate-idle-timeout must not be negative (0 disables it)")
	}
	attr, err := runnercfg.Attribution(opts)
	if err != nil {
		return modes{}, err
	}
	creds, accountURL, err := runnercfg.UserCredentials(opts, attr)
	if err != nil {
		return modes{}, err
	}
	return modes{attribution: attr, credentials: creds, accountURL: accountURL}, nil
}

// fleet is what serve's startup settles from the flags before any
// reconciler runs: the settings ownSettings reads, the runners and the
// harnesses Resolve enabled.
type fleet struct {
	modes
	runners map[string]jobs.Runner
	enabled []string
}

// startupFleet is serve's one path from the flags to the fleet it runs:
// ownSettings (--attribution-mode, --user-credentials among them), the
// runners and the harnesses enabled. Per-user credentials cover every
// brokered provider, so no --user-credentials mode refuses a harness here
// (the codex broker design's temporary codex guard is gone). The clientset
// is built only once the flag-only checks pass, so a misconfiguration fails
// startup before any cluster config is read.
func startupFleet(ctx context.Context, opts *cli.Options,
	clientset func() (kubernetes.Interface, error)) (fleet, kubernetes.Interface, error) {
	own, err := ownSettings(opts)
	if err != nil {
		return fleet{}, nil, err
	}
	runners, err := runnercfg.Runners(opts)
	if err != nil {
		return fleet{}, nil, err
	}
	cs, err := clientset()
	if err != nil {
		return fleet{}, nil, err
	}
	enabled, err := runnercfg.Resolve(ctx, opts, cs, opts.String("agent-namespace"), runners,
		runnercfg.Restrict(opts), runnercfg.SplitList(opts.String("model-allowlist")), opts.String("remediate-model"))
	if err != nil {
		return fleet{}, nil, err
	}
	return fleet{modes: own, runners: runners, enabled: enabled}, cs, nil
}

func serve(ctx context.Context, opts *cli.Options) error {
	prov, shutdown, err := telemetry.Init(ctx, telemetry.Config{
		Dir:            os.Getenv("MAUL_TELEMETRY_DIR"),
		Level:          opts.LogLevel,
		ServiceName:    "remediation-controller",
		ServiceVersion: version.Version,
	})
	if err != nil {
		prov.Logger.LogAttrs(ctx, slog.LevelWarn, "telemetry disabled", slog.Any("error", err))
	}
	defer func() { _ = shutdown(context.WithoutCancel(ctx)) }()
	log := prov.Logger

	namespace := opts.String("namespace")
	if namespace == "" {
		namespace = os.Getenv("POD_NAMESPACE")
	}
	if namespace == "" {
		return errors.New("namespace is required (--namespace or POD_NAMESPACE)")
	}
	repositoryImages, ephemeralStorage, err := runnercfg.RepositoryImages(opts)
	if err != nil {
		return err
	}
	profilesOn, profiles, proxyBase, err := runnercfg.NetworkProfiles(opts)
	if err != nil {
		return err
	}
	agentResources, err := runnercfg.AgentResources(opts)
	if err != nil {
		return err
	}
	cpuRequest, memoryRequest, cpuLimit, memoryLimit := agentResources.Strings()
	meter, err := runnercfg.UsageMeter(opts, log)
	if err != nil {
		return err
	}
	fl, cs, err := startupFleet(ctx, opts, func() (kubernetes.Interface, error) {
		cfg, err := kube.RestConfig(opts.String("kubeconfig"))
		if err != nil {
			return nil, err
		}
		cs, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			return nil, fmt.Errorf("kubernetes clientset: %w", err)
		}
		return cs, nil
	})
	if err != nil {
		return err
	}
	runners, enabled := fl.runners, fl.enabled
	attributionMode, credMode, accountURL := fl.attribution, fl.credentials, fl.accountURL

	mgr, err := kube.NewManager(kube.Options{
		Kubeconfig:              opts.String("kubeconfig"),
		LeaderElectionID:        "maul-remediation-controller-leader",
		LeaderElectionNamespace: namespace,
		Namespaces:              []string{namespace},
		AgentNamespace:          opts.String("agent-namespace"),
		HealthAddr:              opts.String("health-addr"),
		Log:                     log,
	})
	if err != nil {
		return err
	}

	agentNS := opts.String("agent-namespace")
	remediateModel := opts.String("remediate-model")
	allowlist := runnercfg.SplitList(opts.String("model-allowlist"))
	agentDNS, err := runnercfg.AgentDNS(opts, runners, enabled)
	if err != nil {
		return err
	}
	log.LogAttrs(ctx, slog.LevelInfo, "harnesses enabled",
		slog.Any("enabled", enabled), slog.String("default_remediate_model", remediateModel))

	runner := jobs.New(cs, jobs.Config{
		Namespace:      agentNS,
		ServiceAccount: opts.String("agent-service-account"),
		Deadline:       opts.Duration("job-deadline"),
		TTL:            opts.Duration("job-ttl"),
		Runners:        runners,
		Env:            agentEnv(opts),
		BrokerAudience: opts.String("broker-token-audience"),

		CPURequest:    cpuRequest,
		MemoryRequest: memoryRequest,
		CPULimit:      cpuLimit,
		MemoryLimit:   memoryLimit,

		EphemeralStorage:      ephemeralStorage,
		AllowRepositoryImages: repositoryImages,
		DNS:                   agentDNS,
		NetworkProfiles:       profilesOn,
	}, log)

	forges := forge.NewStore(mgr.GetAPIReader())
	spawner := &remediation.SpawnerReconciler{
		Client:    mgr.GetClient(),
		Namespace: namespace,
		Weights: priority.Weights{
			Severity:       opts.Float("priority-weight-severity"),
			Exploitability: opts.Float("priority-weight-exploitability"),
			Likelihood:     opts.Float("priority-weight-likelihood"),
			Impact:         opts.Float("priority-weight-impact"),
		},
		Enabled:      enabled,
		Allowlist:    allowlist,
		DefaultModel: remediateModel,
		// The same budgets the pods get, so the grant the spawner resolves and
		// the floor the runner enforces agree.
		AutoMaxTurns:      int32(opts.Int("remediate-auto-max-turns")),
		AutoTokenBudget:   int64(opts.Int("remediate-auto-token-budget")),
		ManualMaxTurns:    int32(opts.Int("remediate-manual-max-turns")),
		ManualTokenBudget: int64(opts.Int("remediate-manual-token-budget")),
		Attribution:       attributionMode,
		// Under enforce a person's attempt waits for their credential; the
		// record is read uncached by name (get only, granted with the mode).
		UserCredentials: credMode,
		Credentials:     kubecred.Reader{Client: mgr.GetAPIReader(), Namespace: namespace},
		AccountURL:      accountURL,
		Log:             log,
	}
	if err := spawner.SetupWithManager(mgr); err != nil {
		return err
	}
	rem := &remediation.RemediationReconciler{
		Client:        mgr.GetClient(),
		Runner:        runner,
		Forge:         remediation.NewForgeWriter(forges),
		Namespace:     namespace,
		MaxConcurrent: opts.Int("max-concurrent-remediations"),
		Paused:        opts.Bool(runnercfg.AgentsPausedFlag),
		MaxAttempts:   int32(opts.Int("max-attempts")),
		Aging: schedule.AgingPolicy{
			Interval: opts.Duration("priority-aging-interval"),
			Cap:      int32(opts.Int("priority-aging-cap")),
		},
		Enabled: enabled,
		Images: runnerguard.Guard{
			Enabled:         repositoryImages,
			Breaker:         runnerguard.NewBreaker("remediation-controller", log),
			ProfilesEnabled: profilesOn,
			Profiles:        profiles,
			ProxyBase:       proxyBase,
			// A registry grant or tunnel host may name no Forge's web host
			// (CheckForges; WebHosts lists them through this Store's reader).
			Forges: forges,
		},
		MaxChangesetEntries: opts.Int("changeset-max-entries"),
		Attribution:         attributionMode,
		Meter:               meter,
		Usage:               usagerollup.New(mgr.GetClient(), mgr.GetAPIReader(), namespace, meter.On(), log),
		Log:                 log,
	}
	if err := rem.SetupWithManager(mgr); err != nil {
		return err
	}
	if err := setupRollups(mgr, namespace, opts.Duration("finding-ttl"), attributionMode, log); err != nil {
		return err
	}

	log.LogAttrs(ctx, slog.LevelInfo, "remediation-controller starting",
		slog.String("namespace", namespace),
		slog.Int("max_concurrent", opts.Int("max-concurrent-remediations")),
		slog.Bool("agents_paused", opts.Bool(runnercfg.AgentsPausedFlag)),
		slog.String("attribution_mode", string(attributionMode)),
		slog.String("user_credentials", string(credMode)),
		slog.Bool("usage_measurement", meter.On()),
		slog.Duration("finding_ttl", opts.Duration("finding-ttl")),
		slog.Bool("repository_images", repositoryImages))

	if err := mgr.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// setupRollups registers the rollup/TTL loop and, beside it under
// attribution mode attribute, the UsageRollups' compaction (the run
// controllers aggregate into them).
func setupRollups(mgr ctrl.Manager, namespace string, ttl time.Duration, mode attribution.Mode,
	log *slog.Logger) error {
	roll := &rollup.Reconciler{
		Client:    mgr.GetClient(),
		Namespace: namespace,
		TTL:       ttl,
		Log:       log,
	}
	if err := roll.SetupWithManager(mgr); err != nil {
		return err
	}
	if c := usageCompactor(mgr.GetClient(), mgr.GetAPIReader(), namespace, mode, log); c != nil {
		return mgr.Add(c)
	}
	return nil
}

// usageCompactor is the UsageRollups' compaction loop, or nil while
// attribution is off. Off, nothing is aggregated, and the chart grants no
// access to UsageRollups (the CRD may not even be installed), so a loop
// would only log a refused list every hour on an install that never opted
// in. A mode turned off later leaves the existing rollups as they are until
// it is turned back on.
func usageCompactor(c client.Client, reader client.Reader, namespace string, mode attribution.Mode,
	log *slog.Logger) *rollup.UsageCompactor {
	if !mode.On() {
		return nil
	}
	return &rollup.UsageCompactor{Client: c, Reader: reader, Namespace: namespace, Log: log}
}
