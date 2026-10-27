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

	"github.com/devthenet-labs/maul/internal/cli"
	"github.com/devthenet-labs/maul/internal/controller/preview"
	"github.com/devthenet-labs/maul/internal/kube"
	"github.com/devthenet-labs/maul/internal/telemetry"
	"github.com/devthenet-labs/maul/internal/version"
)

func newServeCmd(opts *cli.Options) *cobra.Command {
	cmd := &cobra.Command{
		Use: "serve", Short: "Run the Preview slot controller and orphan sweep",
		RunE: func(cmd *cobra.Command, _ []string) error { return serve(cmd.Context(), opts) },
	}
	f := cmd.Flags()
	f.String("namespace", "", "release namespace (default: POD_NAMESPACE)")
	f.String("kubeconfig", "", "kubeconfig path (default: in-cluster config)")
	f.String("health-addr", ":8081", "healthz/readyz probe listen address")
	f.Int("preview-slot-count", 2, "number of chart-created preview slots")
	f.String("preview-image-prefix", "", "exact <registry>/<path>/ prefix every preview image repository sits "+
		"directly under, one DNS-label leaf deep (the chart's <preview.imageRegistry>/<preview.imagePathPrefix>/)")
	f.String("preview-host-suffix", "", "DNS suffix after <project>-<issue>")
	f.String("preview-node-selector", "", "JSON object of node label to value every slot Pod selects: the "+
		"dedicated preview nodes (merged with --preview-node-pool and --preview-node-class)")
	f.String("preview-node-pool", "", "dedicated DefaultDeny EKS Auto Mode NodePool (fills the node "+
		"selector's karpenter.sh/nodepool)")
	f.String("preview-node-class", "", "dedicated DefaultDeny EKS Auto Mode NodeClass (fills the node "+
		"selector's eks.amazonaws.com/nodeclass)")
	f.String("preview-taint-key", "", "dedicated NoExecute preview-only taint key (value \"true\"), every "+
		"slot Pod tolerates it; required with a node selector")
	f.String("preview-node-isolation", preview.NodeIsolationRequired, "required: every slot Pod selects the "+
		"preview nodes and tolerates their taint, and both must be set; none: neither, an explicit opt-out for "+
		"a cluster without dedicated preview nodes")
	f.Duration("preview-rollout-timeout", 10*time.Minute, "one runtime rollout attempt deadline")
	f.Duration("preview-poll-interval", 15*time.Second, "readiness/queue poll and orphan-sweep interval")
	f.Int("preview-max-retries", 3, "max timed-out or rejected rollout attempts per PR head")
	f.Bool("preview-target-health", false, "mark a Preview Ready only once each component's load balancer "+
		"target is healthy (its Pods' readiness gates, injected in slot namespaces labelled for it): the Ingress "+
		"precedes the Pods and stays across redeploys")
	f.Bool("preview-legacy-ingress", true, "render each Preview's slot Ingress (the load balancer path, "+
		"exactly as before the preview gateway); false renders none, marks a Preview Ready once every component "+
		"has a Ready Pod of its current spec (--preview-target-health is ignored) and deletes the slot Ingresses "+
		"the controller rendered before, for the preview gateway to route from status.serving")
	f.Bool("preview-auth-required", false, "put every preview Ingress behind the sign-in relay: render each "+
		"slot's pinned auth annotations (--preview-auth-annotations) and have the sweeper report, then after a "+
		"grace period delete, a slot Ingress without them")
	f.String("preview-auth-annotations", "", "JSON object of slot namespace to its pinned auth annotations, "+
		"the current key generation's, exactly as the chart renders it for the slot admission policy")
	f.String("preview-tester-image", "", "digest-pinned preview-tester image (<repository>@sha256:<64 hex>, "+
		"never under the preview image prefix); set, it turns acceptance verification on: per-part "+
		"NetworkPolicies for verification-capable Previews and the PreviewVerification tester Jobs")
	f.Duration("preview-verify-deadline-max", 30*time.Minute, "the most a tester Job may run (its "+
		"activeDeadlineSeconds), the bound the slot Job admission policy holds it to (chart "+
		"preview.verification.deadlineMaxSeconds); a verification asking more is clamped. 1m to 1h")
	f.String("preview-auth-previous-annotations", "", "the previous key generation's pinned auth annotations "+
		"during a rotation's overlap (same JSON shape); an Ingress still on them is not unauthenticated")
	return cmd
}

func serve(ctx context.Context, opts *cli.Options) error {
	prov, shutdown, err := telemetry.Init(ctx, telemetry.Config{
		Dir: os.Getenv("MAUL_TELEMETRY_DIR"), Level: opts.LogLevel,
		ServiceName: "preview-controller", ServiceVersion: version.Version,
	})
	if err != nil {
		prov.Logger.LogAttrs(ctx, slog.LevelWarn, "telemetry disabled", slog.Any("error", err))
	}
	defer func() { _ = shutdown(context.WithoutCancel(ctx)) }()
	namespace := opts.String("namespace")
	if namespace == "" {
		namespace = os.Getenv("POD_NAMESPACE")
	}
	settings := preview.Settings{
		Namespace: namespace, SlotCount: opts.Int("preview-slot-count"),
		ImagePrefix: opts.String("preview-image-prefix"), HostSuffix: opts.String("preview-host-suffix"),
		NodePool: opts.String("preview-node-pool"), NodeClass: opts.String("preview-node-class"),
		TaintKey: opts.String("preview-taint-key"), NodeIsolation: opts.String("preview-node-isolation"),
		RolloutTimeout: opts.Duration("preview-rollout-timeout"), PollInterval: opts.Duration("preview-poll-interval"),
		MaxRetries:      int32(opts.Int("preview-max-retries")),
		TargetHealth:    opts.Bool("preview-target-health"),
		NoLegacyIngress: !opts.Bool("preview-legacy-ingress"),
		TesterImage:     opts.String("preview-tester-image"),
	}
	selector, err := preview.ParseNodeSelector(opts.String("preview-node-selector"))
	if err != nil {
		return err
	}
	settings.NodeSelector = selector
	auth, err := authSettings(opts)
	if err != nil {
		return err
	}
	settings.Auth = auth
	if err := settings.Validate(); err != nil {
		return err
	}
	deadlineMax := opts.Duration("preview-verify-deadline-max")
	if deadlineMax < time.Minute || deadlineMax > time.Hour {
		return fmt.Errorf("--preview-verify-deadline-max %s is outside [1m, 1h]", deadlineMax)
	}
	mgr, err := kube.NewManager(kube.Options{
		Kubeconfig: opts.String("kubeconfig"), LeaderElectionID: "maul-preview-controller-leader",
		LeaderElectionNamespace: namespace, Namespaces: []string{namespace},
		HealthAddr: opts.String("health-addr"), Log: prov.Logger,
	})
	if err != nil {
		return err
	}
	// The manager's cache is release-namespace-only. Slot reads use a direct
	// client, so the lease and cleanup see the current API server state and
	// no cluster-scoped informer or slot-wide cache is needed.
	direct, err := client.New(mgr.GetConfig(), client.Options{Scheme: mgr.GetScheme()})
	if err != nil {
		return fmt.Errorf("preview direct client: %w", err)
	}
	recorder := mgr.GetEventRecorder("maul-preview-controller")
	reconciler := &preview.Reconciler{Client: direct, Settings: settings, Log: prov.Logger, Events: recorder}
	if settings.TesterImage != "" {
		verifier, err := newVerifier(mgr, direct, settings, deadlineMax, prov.Logger)
		if err != nil {
			return err
		}
		// A Preview released while a verification runs records what the
		// slot showed first.
		reconciler.BeforeRelease = verifier.BeforeRelease
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		return err
	}
	if err := mgr.Add(&preview.Sweeper{Client: direct, Settings: settings, Log: prov.Logger,
		Events: recorder}); err != nil {
		return err
	}
	if settings.NoLegacyIngress && settings.TargetHealth {
		prov.Logger.LogAttrs(ctx, slog.LevelWarn, "preview target health ignored: without the legacy slot "+
			"Ingress no load balancer target is waited for")
	}
	prov.Logger.LogAttrs(ctx, slog.LevelInfo, "preview-controller starting",
		slog.Int("slot_count", settings.SlotCount), slog.String("namespace", namespace),
		slog.Bool("auth_required", settings.Auth.Required), slog.Bool("legacy_ingress", !settings.NoLegacyIngress),
		slog.Bool("verification", settings.TesterImage != ""))
	if err := mgr.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// newVerifier sets up the PreviewVerification reconciler: it watches the
// requests in the release namespace through the manager's cache, reads and
// writes through the direct client, and reads the tester's output and the
// components' log tails through pods/log.
func newVerifier(mgr ctrl.Manager, direct client.Client, settings preview.Settings, deadlineMax time.Duration,
	log *slog.Logger) (*preview.VerificationReconciler, error) {
	cs, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		return nil, fmt.Errorf("preview log client: %w", err)
	}
	verifier := &preview.VerificationReconciler{Client: direct, Settings: settings,
		Logs: preview.KubeLogReader{Pods: cs.CoreV1()}, DeadlineMax: deadlineMax, Log: log}
	if err := verifier.SetupWithManager(mgr); err != nil {
		return nil, err
	}
	return verifier, nil
}

// authSettings reads the sign-in relay's pinned annotations, which the chart
// renders once for both this controller and the slot admission policy.
func authSettings(opts *cli.Options) (preview.AuthSettings, error) {
	current, err := preview.ParseAuthAnnotations(opts.String("preview-auth-annotations"))
	if err != nil {
		return preview.AuthSettings{}, err
	}
	previous, err := preview.ParseAuthAnnotations(opts.String("preview-auth-previous-annotations"))
	if err != nil {
		return preview.AuthSettings{}, fmt.Errorf("previous generation: %w", err)
	}
	return preview.AuthSettings{Required: opts.Bool("preview-auth-required"), Annotations: current,
		Previous: previous}, nil
}
