// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/changeset"
	"github.com/devthenet-labs/maul/internal/cli"
	"github.com/devthenet-labs/maul/internal/controller/intent"
	"github.com/devthenet-labs/maul/internal/credgate/kubecred"
	"github.com/devthenet-labs/maul/internal/forge"
	"github.com/devthenet-labs/maul/internal/jobs"
	"github.com/devthenet-labs/maul/internal/kube"
	"github.com/devthenet-labs/maul/internal/model"
	"github.com/devthenet-labs/maul/internal/netprofile"
	"github.com/devthenet-labs/maul/internal/resourceclass"
	"github.com/devthenet-labs/maul/internal/runnercfg"
	"github.com/devthenet-labs/maul/internal/runnerguard"
	"github.com/devthenet-labs/maul/internal/telemetry"
	"github.com/devthenet-labs/maul/internal/usagemeter"
	"github.com/devthenet-labs/maul/internal/usagerollup"
	"github.com/devthenet-labs/maul/internal/version"
)

// The flags this binary alone reads carry an intent- prefix, so the shared
// kustomize ConfigMap's MAUL_* keys for the other controllers can never
// set them by accident.
func newServeCmd(opts *cli.Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the project, intent, run-scheduler and TTL reconcilers",
		RunE:  func(cmd *cobra.Command, _ []string) error { return serve(cmd.Context(), opts) },
	}
	f := cmd.Flags()
	f.String("namespace", "", "namespace the maul resources live in (default: POD_NAMESPACE)")
	f.String("kubeconfig", "", "kubeconfig path (default: in-cluster config)")
	f.String("health-addr", ":8081", "healthz/readyz probe listen address")

	f.Duration("intent-poll-interval", intent.DefaultPollInterval,
		"how often each Project's intent repository and each active intent's issue are polled")
	f.Duration("intent-approval-poll-interval", intent.DefaultApprovalPollInterval,
		"how often an intent awaiting approval polls its issue for an approval")
	f.Duration("intent-pr-poll-interval", intent.DefaultPRPollInterval,
		"how often an intent in review polls its pull requests")
	f.Bool("intent-previews-enabled", false,
		"project opted-in, open intent PRs into Preview resources, beside the default-branch head (read once at "+
			"review start) of each previewed repository an intent did not change; requires a separately enabled "+
			"preview-controller")
	// --intent-verification is REMOVED: the agent evaluator is verification's
	// one switch. It stays registered, hidden, only so a leftover value meets
	// the migration message (verification) rather than an unknown-flag error
	// or, from the environment, silence.
	f.Bool("intent-verification", false, "REMOVED: --intent-evaluator turns verification on")
	_ = f.MarkHidden("intent-verification") // registered just above
	f.Duration("intent-verify-timeout-max", intent.DefaultVerifyTimeoutMax,
		"no effect since slice 1's criteria no longer run (was the most a Project's verification.timeout gave a "+
			"tester Job); still between 1m and 1h until it is removed with the tester's chart keys")
	f.Bool("intent-evaluator", false,
		"verify the approved plan's acceptance outcomes with the agent evaluator, which explores each intent's "+
			"Preview through a slot tool pod and writes its own checks, for Projects with "+
			"spec.verification.enabled (needs --intent-previews-enabled or --intent-evaluator-agents). Off, no "+
			"intent is verified: each verifying Project and its intents' issues say so, and each preview is "+
			"handed to people at once")
	f.String("intent-evaluate-model", "anthropic/claude-sonnet-5-5",
		"canonical model a claude Project's evaluate runs run (--intent-evaluator)")
	f.String("intent-checkreview-model", "anthropic/claude-sonnet-5-5",
		"canonical model a claude Project's checkreview runs run (--intent-evaluator)")
	f.Duration("intent-evaluate-timeout", intent.DefaultEvaluateTimeout,
		"wall-clock limit of an evaluate run, between 1m and 55m (its tool pod lives 5m longer, within the "+
			"hour a verification may take)")
	f.Int("intent-evaluate-max-turns", int(intent.DefaultEvaluateMaxTurns),
		fmt.Sprintf("most agent turns an evaluate run may take, between 1 and %d (a Project may lower it); every "+
			"tool call is a turn, and the run's last few are kept for its report", intent.MaxStageTurns))
	f.Int("intent-checkreview-max-turns", int(intent.DefaultCheckReviewMaxTurns),
		fmt.Sprintf("most agent turns each check-review session may take, between 1 and %d (a Project may lower it)",
			intent.MaxStageTurns))
	f.String("intent-default-handoff", string(v1alpha1.HandoffGated),
		"when a Project that sets no verification.handoff hands its preview to humans under the evaluator: "+
			"gated (once the evaluation passes or gives up) or early (at once)")
	f.Duration("intent-handoff-max", v1alpha1.DefaultHandoffMax,
		"the most a gated hand-off waits, from CI green at the first pull request heads, for a Project that "+
			"sets no verification.handoffMax; between 10m and 4h")
	f.Bool("intent-evaluate-codex", false,
		"also evaluate the intents of codex Projects on codex (needs --intent-codex); off, they are not "+
			"evaluated, their hand-off is early, and each such Project says so (EvaluatorUnverified)")
	f.Int("intent-evaluation-cost-max", int(v1alpha1.DefaultMaxEvaluateCostMicroUSD),
		"the most one intent's evaluate and checkreview runs may spend, in micro-USD, whatever its Project's "+
			"limits.maxEvaluateCostMicroUSD says; counted apart from the per-intent ceiling, so evaluation never "+
			"spends what fix rounds need")
	f.Int("intent-max-tool-calls", int(v1alpha1.DefaultMaxToolCalls),
		"the most tool calls one evaluation may make, between 50 and 300, whatever its Project's "+
			"verification.maxToolCalls says")
	f.Bool("intent-evaluator-agents", false,
		"also evaluate Projects with a cli or library surface and no Preview, through an agents-namespace tool "+
			"pod that builds the pull request's head and its merge base in the repository's accepted runner image "+
			"(needs --intent-evaluator, not --intent-previews-enabled); off, such a "+
			"Project's verification is Off (AgentsToolPodNotEnabled)")
	f.String("intent-tools-image", "",
		"maul's digest-pinned preview-tools image the agents tool pod runs (--intent-evaluator-agents)")
	f.Int("intent-max-tool-pods", 2,
		"the most agents tool pods live at once; at the bound a round waits, never a verdict "+
			"(--intent-evaluator-agents)")
	f.Duration("intent-tool-build-timeout-max", v1alpha1.MaxCLIBuildTimeout,
		"the longest a cli surface's buildTimeout gives each tree's build in the agents tool pod, between 1m "+
			"and 20m (--intent-evaluator-agents)")
	f.String("intent-tool-resources", "",
		`the agents tool pod's requests and limits (equal) as JSON {"tools":{"cpu","memory","ephemeralStorage"},`+
			`"sandbox":{...}}, every value a string quantity; an unset one keeps the default `+
			"(--intent-evaluator-agents)")
	f.Bool("intent-dependencies", true,
		"hold an intent whose issue GitHub records as blocked by other issues in Pending until every blocker is "+
			"closed as completed (an approver's /maul start overrides); off, intents start as soon as their "+
			"trigger is accepted and every Project says DependenciesUnavailable (DependenciesOff)")
	f.Duration("intent-dependency-give-up", intent.DefaultDependencyGiveUp,
		"how long a dependency read GitHub keeps failing holds an intent before it starts without its dependencies")
	_ = f.MarkHidden("intent-dependency-give-up")
	f.Bool("intent-multi-repo", false,
		"run intents of Projects that list more than one repository; off, such a Project is not Ready and its "+
			"intents are held Blocked (UnsupportedRepositories) where they stand: no run is launched and nothing "+
			"is pushed, a Job already running finishes and its push waits, and a wait longer than --job-ttl "+
			"discards that finished work, which is then run again")
	f.Int("intent-max-concurrent-runs", 1, "intent agent Jobs running at once (a pool separate from remediation's)")
	f.Int("intent-rate-limit-floor", intent.DefaultRateLimitFloor,
		"pause intent polling while the installation has fewer core GitHub requests left than this (0 disables)")
	f.Duration("intent-ttl", intent.DefaultTTL, "retention of ended intents after completion; 0 keeps them forever")
	f.Duration("intent-job-deadline", 90*time.Minute,
		"the least activeDeadlineSeconds on an intent agent Job, whatever its stage; a run whose stage timeout "+
			"plus 15m is longer gets that instead, so raising one stage's timeout stretches only that stage's Jobs "+
			"(the broker caller token is minted for the Job's deadline plus 15m)")
	f.Duration("intent-job-deadline-max", 4*time.Hour+30*time.Minute,
		"the most a run's stage timeout plus 15m may stretch its intent Job's deadline to; every stage timeout "+
			"plus 15m must fit under it (the controller refuses to start otherwise)")
	f.String("broker-limit-unit", "tokens",
		"the egress broker's --limit-unit: under cost every intent run is granted a cost budget (the stage's "+
			"limits.<stage>.maxCostMicroUSD, else the Project's limits.maxCostMicroUSD) stamped on its agent Job "+
			"and pod as maul.run/budget-micro-usd; under tokens (the default) none is")
	f.Int("broker-cost-per-hour", 0,
		"the egress broker's --cost-per-hour in micro-USD (cost unit only; 0: none): a Project whose run budget "+
			"is more than half of it is not Ready")
	f.Int("intent-plan-max-turns", 40, "most agent turns a plan run may take (a Project may lower it)")
	f.Int("intent-plan-token-budget", 200000, "most output tokens a plan run may spend (a Project may lower it)")
	f.Duration("intent-plan-timeout", 20*time.Minute, "wall-clock limit of a plan run")
	f.Duration("intent-plan-idle-timeout", 20*time.Minute,
		"end a plan run that makes no progress (no model turn, no tool result) for this long; 0 disables")
	f.Int("intent-build-max-turns", 150, "most agent turns a build run may take (a Project may lower it)")
	f.Int("intent-build-token-budget", 800000, "most output tokens a build run may spend (a Project may lower it)")
	f.Duration("intent-build-timeout", 60*time.Minute, "wall-clock limit of a build run")
	f.Duration("intent-build-idle-timeout", 20*time.Minute,
		"end a build run that makes no progress (no model turn, no tool result) for this long; 0 disables")
	f.Int("intent-revise-max-turns", 80, "most agent turns a revise or check-fix run may take (a Project may lower it)")
	f.Int("intent-revise-token-budget", 400000, "most output tokens a revise or check-fix run may spend")
	f.Duration("intent-revise-timeout", 45*time.Minute, "wall-clock limit of a revise or check-fix run")
	f.Duration("intent-revise-idle-timeout", 20*time.Minute,
		"end a revise or check-fix run that makes no progress (no model turn, no tool result) for this long; 0 disables")
	f.String("intent-plan-model", "anthropic/claude-sonnet-5-5", "canonical model the plan stage runs")
	f.String("intent-build-model", "anthropic/claude-sonnet-5-5", "canonical model the build stage runs")
	f.String("intent-model-allowlist", "",
		"comma-separated canonical models a Project may choose per stage (spec.models) beside the stage flags' "+
			"own, each on the harness it resolves to (claude, or codex under --intent-codex); a Project naming "+
			"another is not Ready (ModelNotAllowed); empty admits only the stage flags' models")
	f.Bool("intent-codex", false,
		"also run intents on brokered codex: a Project with spec.harness codex plans and builds on it (needs the "+
			"codex runner, --codex-agent-image with --broker-url, and codex in --harnesses); off, such a Project "+
			"is not Ready (HarnessNotEnabled) and a codex run waits Pending")
	f.String("intent-codex-plan-model", "openai/gpt-5.3-codex",
		"canonical model a codex Project's plan stage runs (--intent-codex)")
	f.String("intent-codex-build-model", "openai/gpt-5.3-codex",
		"canonical model a codex Project's build, revise and check-fix stages run (--intent-codex)")
	f.String("intent-resource-classes", "",
		"the operator's agent resource classes as JSON, name to {requests: {cpu, memory}, limits: {memory, cpu}} "+
			"(a CPU limit is optional); a Project picks one per repository (agentResourceClass) for that "+
			"repository's build, revise and check-fix runs, and nothing can ask for more than the largest. "+
			"Empty defines none")

	f.String("agent-namespace", "maul-agents", "namespace the agent Jobs run in")
	f.String("agent-service-account", "maul-agent", "service account for the agent Jobs")
	f.Duration("job-ttl", time.Hour, "ttlSecondsAfterFinished for a finished agent Job")
	runnercfg.RegisterFlags(f)
	runnercfg.RegisterRepositoryImageFlags(f)
	runnercfg.RegisterNetworkProfileFlags(f)
	runnercfg.RegisterAgentResourceFlags(f)
	runnercfg.RegisterAgentDNSFlag(f)
	runnercfg.RegisterAgentsPausedFlag(f)
	runnercfg.RegisterAttributionFlag(f)
	runnercfg.RegisterUserCredentialsFlags(f)
	runnercfg.RegisterUsageFlags(f)
	f.Int("changeset-max-entries", changeset.DefaultMaxEntries,
		"most files (upserts plus deletes) a build's changeset may touch before it is rejected without any forge call")
	return cmd
}

// settings reads the shared reconciler settings from the flags.
func settings(opts *cli.Options, namespace, agentNS string) (intent.Settings, error) {
	s := intent.Settings{
		Namespace:            namespace,
		AgentNamespace:       agentNS,
		PollInterval:         opts.Duration("intent-poll-interval"),
		ApprovalPollInterval: opts.Duration("intent-approval-poll-interval"),
		PRPollInterval:       opts.Duration("intent-pr-poll-interval"),
		RateLimitFloor:       opts.Int("intent-rate-limit-floor"),
		MaxAttempts:          intent.DefaultMaxAttempts,
		MultiRepo:            opts.Bool("intent-multi-repo"),
		Previews:             opts.Bool("intent-previews-enabled"),
		IgnoreDependencies:   !opts.Bool("intent-dependencies"),
		DependencyGiveUp:     opts.Duration("intent-dependency-give-up"),
		Plan: intent.StageCeiling{
			MaxTurns:    int32(opts.Int("intent-plan-max-turns")),
			TokenBudget: int64(opts.Int("intent-plan-token-budget")),
			Timeout:     opts.Duration("intent-plan-timeout"),
			IdleTimeout: opts.Duration("intent-plan-idle-timeout"),
		},
		Build: intent.StageCeiling{
			MaxTurns:    int32(opts.Int("intent-build-max-turns")),
			TokenBudget: int64(opts.Int("intent-build-token-budget")),
			Timeout:     opts.Duration("intent-build-timeout"),
			IdleTimeout: opts.Duration("intent-build-idle-timeout"),
		},
		Revise: intent.StageCeiling{
			MaxTurns:    int32(opts.Int("intent-revise-max-turns")),
			TokenBudget: int64(opts.Int("intent-revise-token-budget")),
			Timeout:     opts.Duration("intent-revise-timeout"),
			IdleTimeout: opts.Duration("intent-revise-idle-timeout"),
		},
	}
	for _, c := range []struct {
		name string
		ok   bool
	}{
		{"--intent-poll-interval", s.PollInterval > 0},
		{"--intent-approval-poll-interval", s.ApprovalPollInterval > 0},
		{"--intent-pr-poll-interval", s.PRPollInterval > 0},
		{"--intent-rate-limit-floor", s.RateLimitFloor >= 0},
		{"--intent-plan-max-turns", s.Plan.MaxTurns > 0},
		{"--intent-plan-token-budget", s.Plan.TokenBudget > 0},
		{"--intent-plan-timeout", s.Plan.Timeout > 0},
		{"--intent-build-max-turns", s.Build.MaxTurns > 0},
		{"--intent-build-token-budget", s.Build.TokenBudget > 0},
		{"--intent-build-timeout", s.Build.Timeout > 0},
		{"--intent-revise-max-turns", s.Revise.MaxTurns > 0},
		{"--intent-revise-token-budget", s.Revise.TokenBudget > 0},
		{"--intent-revise-timeout", s.Revise.Timeout > 0},
	} {
		if !c.ok {
			return intent.Settings{}, fmt.Errorf("%s must be positive", c.name)
		}
	}
	for _, c := range []struct {
		name string
		idle time.Duration
	}{
		{"--intent-plan-idle-timeout", s.Plan.IdleTimeout},
		{"--intent-build-idle-timeout", s.Build.IdleTimeout},
		{"--intent-revise-idle-timeout", s.Revise.IdleTimeout},
	} {
		if c.idle < 0 {
			return intent.Settings{}, fmt.Errorf("%s must not be negative (0 disables it)", c.name)
		}
	}
	if err := verification(opts, &s); err != nil {
		return intent.Settings{}, err
	}
	if err := evaluator(opts, &s); err != nil {
		return intent.Settings{}, err
	}
	if err := jobDeadlines(opts, &s); err != nil {
		return intent.Settings{}, err
	}
	if err := brokerBudgets(opts, &s); err != nil {
		return intent.Settings{}, err
	}
	mode, err := runnercfg.Attribution(opts)
	if err != nil {
		return intent.Settings{}, err
	}
	s.Attribution = mode
	if s.UserCredentials, s.AccountURL, err = runnercfg.UserCredentials(opts, mode); err != nil {
		return intent.Settings{}, err
	}
	return s, nil
}

// verificationMigration is where an operator of the removed slice-1
// configuration reads how to move to the evaluator.
const verificationMigration = "see docs/intents/evaluator.md#migrating-from-slice-1"

// verification refuses the removed --intent-verification and reads the
// verification timeout ceiling, held to a PreviewVerification's own timeout
// bounds. Verification itself is the agent evaluator's (evaluator): slice
// 1's acceptance criteria no longer run, so the switch that turned them on
// stops startup rather than leave an operator believing intents are
// verified the old way.
func verification(opts *cli.Options, s *intent.Settings) error {
	if opts.Bool("intent-verification") {
		return errors.New("--intent-verification (MAUL_INTENT_VERIFICATION) is removed: the agent evaluator " +
			"(--intent-evaluator, chart intentController.verification.evaluator) turns verification on, and " +
			"without it no intent is verified; drop the flag, " + verificationMigration)
	}
	s.VerifyTimeoutMax = opts.Duration("intent-verify-timeout-max")
	if s.VerifyTimeoutMax < time.Minute || s.VerifyTimeoutMax > time.Hour {
		return fmt.Errorf("--intent-verify-timeout-max %s must be between 1m and 1h", s.VerifyTimeoutMax)
	}
	return nil
}

// evaluator reads the agent evaluator's switch and, when it is on, its
// settings, each held to its bounds; on, it is what turns verification on
// (Settings.Verification). It is refused without previews unless the
// agents-namespace tool pod is on (Q33): the evaluator judges each intent's
// Preview, or its workspace. Off, nothing of it reaches the settings and no
// intent is verified (each verifying Project says so,
// intent.ReasonEvaluatorOff).
func evaluator(opts *cli.Options, s *intent.Settings) error {
	if !opts.Bool("intent-evaluator") {
		if opts.Bool("intent-evaluator-agents") {
			return errors.New("--intent-evaluator-agents needs --intent-evaluator: the tool pod is the evaluator's")
		}
		return nil
	}
	if !s.Previews && !opts.Bool("intent-evaluator-agents") {
		return errors.New("--intent-evaluator needs --intent-previews-enabled (or --intent-evaluator-agents): " +
			"the evaluator judges each intent's Preview, or its workspace")
	}
	if err := agents(opts, s); err != nil {
		return err
	}
	timeout := opts.Duration("intent-evaluate-timeout")
	if timeout < time.Minute || timeout > intent.MaxEvaluateTimeout {
		return fmt.Errorf("--intent-evaluate-timeout %s must be between 1m and %s", timeout, intent.MaxEvaluateTimeout)
	}
	handoff := v1alpha1.HandoffMode(opts.String("intent-default-handoff"))
	if handoff != v1alpha1.HandoffGated && handoff != v1alpha1.HandoffEarly {
		return fmt.Errorf("--intent-default-handoff %q: want gated or early", handoff)
	}
	handoffMax := opts.Duration("intent-handoff-max")
	if handoffMax < 10*time.Minute || handoffMax > 4*time.Hour {
		return fmt.Errorf("--intent-handoff-max %s must be between 10m and 4h", handoffMax)
	}
	cost := int64(opts.Int("intent-evaluation-cost-max"))
	if cost <= 0 {
		return errors.New("--intent-evaluation-cost-max must be positive")
	}
	calls := opts.Int("intent-max-tool-calls")
	if calls < v1alpha1.MinToolCalls || calls > v1alpha1.MaxToolCallsLimit {
		return fmt.Errorf("--intent-max-tool-calls %d must be between %d and %d", calls, v1alpha1.MinToolCalls,
			v1alpha1.MaxToolCallsLimit)
	}
	evaluateTurns, err := stageTurns(opts, "intent-evaluate-max-turns")
	if err != nil {
		return err
	}
	reviewTurns, err := stageTurns(opts, "intent-checkreview-max-turns")
	if err != nil {
		return err
	}
	s.Verification, s.Evaluator = true, true
	s.EvaluateModel, s.CheckReviewModel = opts.String("intent-evaluate-model"), opts.String("intent-checkreview-model")
	s.DefaultHandoff, s.HandoffMax = handoff, handoffMax
	s.EvaluateCodex = opts.Bool("intent-evaluate-codex")
	s.EvaluationCostMax, s.MaxToolCalls = cost, int32(calls)
	s.Evaluate = intent.StageCeiling{MaxTurns: evaluateTurns,
		TokenBudget: intent.DefaultEvaluateTokenBudget, Timeout: timeout}
	s.CheckReview = intent.StageCeiling{MaxTurns: reviewTurns,
		TokenBudget: intent.DefaultCheckReviewTokenBudget, Timeout: intent.DefaultCheckReviewTimeout}
	return nil
}

// logVerificationOff says, at startup, which verifying Projects set leaves
// Off: every one without the evaluator, or, with the agents tool pod on and
// previews off, every one without a cli or library surface.
func logVerificationOff(ctx context.Context, log *slog.Logger, set intent.Settings) {
	switch {
	case !set.Verification:
		log.LogAttrs(ctx, slog.LevelInfo, "--intent-evaluator is off: no intent is verified; each verifying "+
			"Project says so in its VerificationOff condition, and each preview is handed to people at once")
	case set.VerificationWithoutPreviews():
		log.LogAttrs(ctx, slog.LevelInfo, "--intent-evaluator-agents without --intent-previews-enabled: only "+
			"Projects with a cli or library surface are verified; each other verifying Project says so in its "+
			"VerificationOff condition")
	}
}

// setupPreviewSource registers the preview-source reconciler, which writes
// each eligible Intent's Preview, when --intent-previews-enabled is on.
func setupPreviewSource(mgr ctrl.Manager, opts *cli.Options) error {
	if !opts.Bool("intent-previews-enabled") {
		return nil
	}
	return (&intent.PreviewSourceReconciler{
		Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr)
}

// slice1Census takes the slice-1 census (intent.TakeSlice1Census) through
// r, which can only read, reports it as the maul.intent.slice1_records
// gauge, logs it, and refuses to start while it found any record: each by
// name, with what clears it (Slice1Census.Err). A census that cannot list
// refuses too: what it could not see may be a record the removed criteria
// fields left unverifiable.
func slice1Census(ctx context.Context, log *slog.Logger, r client.Reader, set intent.Settings) error {
	census, err := intent.TakeSlice1Census(ctx, r, set)
	if err != nil {
		return fmt.Errorf("the slice-1 census could not run, so intent-controller does not start: %w", err)
	}
	census.Report()
	attrs := []slog.Attr{slog.Int("count", census.Count()), slog.Any("intents", census.Intents),
		slog.Any("previewverifications", census.Verifications),
		slog.Bool("previewverifications_unlisted", census.VerificationsUnlisted)}
	if err := census.Err(); err != nil {
		return fmt.Errorf("slice-1 census: %w; slice 1's acceptance criteria are removed and these records "+
			"cannot be verified, so intent-controller does not start until they are gone, %s", err,
			verificationMigration)
	}
	msg := "slice-1 census: no open intent or verification records acceptance criteria"
	if census.VerificationsUnlisted {
		msg = "slice-1 census: no open intent records acceptance criteria; PreviewVerifications could not be " +
			"listed (no grant without the evaluator)"
	}
	log.LogAttrs(ctx, slog.LevelInfo, msg, attrs...)
	return nil
}

// agents reads the agents-namespace tool pod's settings when
// --intent-evaluator-agents is on, each held to its bounds; off, none of
// them reaches the settings.
func agents(opts *cli.Options, s *intent.Settings) error {
	if !opts.Bool("intent-evaluator-agents") {
		return nil
	}
	image := opts.String("intent-tools-image")
	if !digestImage.MatchString(image) {
		return fmt.Errorf("--intent-tools-image %q must be an image pinned by digest (name@sha256:<64 hex>)", image)
	}
	pods := opts.Int("intent-max-tool-pods")
	if pods < 1 {
		return fmt.Errorf("--intent-max-tool-pods %d must be at least 1", pods)
	}
	build := opts.Duration("intent-tool-build-timeout-max")
	if build < time.Minute || build > v1alpha1.MaxCLIBuildTimeout {
		return fmt.Errorf("--intent-tool-build-timeout-max %s must be between 1m and %s", build,
			v1alpha1.MaxCLIBuildTimeout)
	}
	res, err := toolResources(opts.String("intent-tool-resources"))
	if err != nil {
		return fmt.Errorf("--intent-tool-resources: %w", err)
	}
	s.EvaluatorAgents, s.ToolsImage, s.MaxToolPods, s.ToolBuildTimeoutMax, s.ToolResources = true, image, pods,
		build, res
	return nil
}

// digestImage is an image reference pinned by digest.
var digestImage = regexp.MustCompile(`^[a-z0-9][^@\s]*@sha256:[0-9a-f]{64}$`)

// toolResources parses --intent-tool-resources: JSON
// {"tools":{"cpu","memory","ephemeralStorage"},"sandbox":{...}}, every value
// a string Kubernetes quantity, unknown keys refused. Empty is the defaults.
func toolResources(raw string) (jobs.ToolPodResources, error) {
	if strings.TrimSpace(raw) == "" {
		return jobs.ToolPodResources{}, nil
	}
	type container struct {
		CPU              string `json:"cpu"`
		Memory           string `json:"memory"`
		EphemeralStorage string `json:"ephemeralStorage"`
	}
	var in struct {
		Tools   container `json:"tools"`
		Sandbox container `json:"sandbox"`
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return jobs.ToolPodResources{}, err
	}
	for name, q := range map[string]string{
		"tools.cpu": in.Tools.CPU, "tools.memory": in.Tools.Memory, "tools.ephemeralStorage": in.Tools.EphemeralStorage,
		"sandbox.cpu": in.Sandbox.CPU, "sandbox.memory": in.Sandbox.Memory,
		"sandbox.ephemeralStorage": in.Sandbox.EphemeralStorage,
	} {
		if q == "" {
			continue
		}
		if v, err := resource.ParseQuantity(q); err != nil || v.Sign() <= 0 {
			return jobs.ToolPodResources{}, fmt.Errorf("%s %q is not a positive quantity", name, q)
		}
	}
	return jobs.ToolPodResources{Tools: jobs.ToolResources(in.Tools), Sandbox: jobs.ToolResources(in.Sandbox)}, nil
}

// stageTurns reads a role stage's turn ceiling flag, held to what a run's
// grant may carry (1 to intent.MaxStageTurns).
func stageTurns(opts *cli.Options, flag string) (int32, error) {
	n := opts.Int(flag)
	if n < 1 || n > int(intent.MaxStageTurns) {
		return 0, fmt.Errorf("--%s %d must be between 1 and %d", flag, n, intent.MaxStageTurns)
	}
	return int32(n), nil
}

// evaluatorProbeDNS is the resolver an evaluator Job's strict probe must
// find closed: under dnsPolicy None the pod names none, so the controller's
// own, read once from resolvConf (SPEC Q14). Without it every evaluator Job
// would be refused, so the evaluator does not start without it. "" while
// the evaluator is off.
func evaluatorProbeDNS(s intent.Settings, resolvConf string) (string, error) {
	if !s.Evaluator {
		return "", nil
	}
	addr, err := runnercfg.ProbeDNS(resolvConf)
	if err != nil {
		return "", fmt.Errorf("--intent-evaluator: %w", err)
	}
	return addr, nil
}

// jobDeadlines reads the intent Job deadline floor and ceiling. Each run's
// Job deadline is derived from its own stage timeout (intent.JobDeadlineMargin
// past it, capped by --intent-job-deadline-max), so the floor is not tied to
// the stage timeouts: raising one stage's timeout stretches that stage's Jobs
// alone. What must hold is that every stage, plus the margin, fits under the
// ceiling, or a Job could end before its stage times out.
func jobDeadlines(opts *cli.Options, s *intent.Settings) error {
	s.JobDeadline, s.JobDeadlineMax = opts.Duration("intent-job-deadline"), opts.Duration("intent-job-deadline-max")
	if s.JobDeadline <= 0 {
		return errors.New("--intent-job-deadline must be positive")
	}
	if s.JobDeadlineMax <= 0 {
		return errors.New("--intent-job-deadline-max must be positive")
	}
	for _, c := range []struct {
		name    string
		timeout time.Duration
	}{
		{"--intent-plan-timeout", s.Plan.Timeout},
		{"--intent-build-timeout", s.Build.Timeout},
		{"--intent-revise-timeout", s.Revise.Timeout},
		// Zero while the evaluator is off.
		{"--intent-evaluate-timeout", s.Evaluate.Timeout},
		{"the checkreview timeout", s.CheckReview.Timeout},
	} {
		if c.timeout+intent.JobDeadlineMargin > s.JobDeadlineMax {
			return fmt.Errorf("%s %s plus %s is longer than --intent-job-deadline-max %s: its Jobs could end "+
				"before the stage times out", c.name, c.timeout, intent.JobDeadlineMargin, s.JobDeadlineMax)
		}
	}
	return nil
}

// brokerBudgets reads the broker's limit unit and hourly cost limit, which
// decide whether runs are granted cost budgets and which budgets a Project
// may set. The hourly limit is refused under the token unit, as the broker
// refuses it, rather than silently ignored.
func brokerBudgets(opts *cli.Options, s *intent.Settings) error {
	perHour := int64(opts.Int("broker-cost-per-hour"))
	switch unit := opts.String("broker-limit-unit"); unit {
	case "tokens":
		if perHour != 0 {
			return errors.New("--broker-cost-per-hour is only meaningful with --broker-limit-unit cost")
		}
	case "cost":
		if perHour < 0 {
			return errors.New("--broker-cost-per-hour must not be negative (0: none)")
		}
		s.CostBudgets, s.BrokerCostPerHour = true, perHour
	default:
		return fmt.Errorf("--broker-limit-unit %q: want tokens or cost", unit)
	}
	return nil
}

// resourceClasses reads --intent-resource-classes: every class validated as
// a whole before the controller starts, so a size the API server would
// refuse, or a typo, is a startup error naming the class rather than a run
// that fails or waits out its deadline.
func resourceClasses(opts *cli.Options) (resourceclass.Set, error) {
	classes, err := resourceclass.Parse(opts.String("intent-resource-classes"))
	if err != nil {
		return nil, fmt.Errorf("--intent-resource-classes: %w", err)
	}
	return classes, nil
}

// jobSizes are what sizes every intent agent Job beyond its runner, each
// read and checked before the controller starts: whether a build may run
// its repository's image and the ephemeral-storage wall on every Job
// (runnercfg.RepositoryImages), the default CPU and memory
// (runnercfg.AgentResources), and the operator's resource classes; and,
// beside them, the operator's network profiles a build on its repository's
// image may carry (runnercfg.NetworkProfiles).
type jobSizes struct {
	repositoryImages bool
	ephemeralStorage string
	defaults         resourceclass.Resources
	classes          resourceclass.Set
	profilesOn       bool
	profiles         netprofile.Set
	proxyBase        *url.URL
}

// readJobSizes reads the jobSizes flags.
func readJobSizes(opts *cli.Options) (jobSizes, error) {
	var (
		s   jobSizes
		err error
	)
	if s.repositoryImages, s.ephemeralStorage, err = runnercfg.RepositoryImages(opts); err != nil {
		return jobSizes{}, err
	}
	if s.defaults, err = runnercfg.AgentResources(opts); err != nil {
		return jobSizes{}, err
	}
	if s.classes, err = resourceClasses(opts); err != nil {
		return jobSizes{}, err
	}
	if s.profilesOn, s.profiles, s.proxyBase, err = runnercfg.NetworkProfiles(opts); err != nil {
		return jobSizes{}, err
	}
	return s, nil
}

// readRunConfig reads what the run reconciler takes from the flags beside
// its settings: the jobSizes and the usage meter (nil while
// --usage-measurement is off), and holds --changeset-max-entries positive.
func readRunConfig(opts *cli.Options, log *slog.Logger) (jobSizes, *usagemeter.Meter, error) {
	if opts.Int("changeset-max-entries") <= 0 {
		return jobSizes{}, nil, errors.New("--changeset-max-entries must be positive")
	}
	sizes, err := readJobSizes(opts)
	if err != nil {
		return jobSizes{}, nil, err
	}
	meter, err := runnercfg.UsageMeter(opts, log)
	if err != nil {
		return jobSizes{}, nil, err
	}
	return sizes, meter, nil
}

// harnesses resolves the harnesses intent stages run on: claude's, the
// default every Project runs on unless it asks for codex (brokered claude,
// or the fake harness in dev and tests), and codex's under --intent-codex,
// the zero RunHarness without it. Each harness runs both stages: its plan
// and build models must resolve to it. Both run brokered, so no intent pod
// ever holds a model credential.
func harnesses(ctx context.Context, opts *cli.Options, cs kubernetes.Interface, agentNS string,
	runners map[string]jobs.Runner) (claude, codex intent.RunHarness, err error) {
	claude = intent.RunHarness{PlanModel: opts.String("intent-plan-model"), BuildModel: opts.String("intent-build-model")}
	models := []string{claude.PlanModel, claude.BuildModel}
	var roleModels []string
	if opts.Bool("intent-evaluator") {
		// The evaluator's and the check reviewer's models run on the claude
		// harness too: allowlisted and resolved with the others.
		roleModels = []string{opts.String("intent-evaluate-model"), opts.String("intent-checkreview-model")}
		models = append(models, roleModels...)
	}
	codexOn := opts.Bool("intent-codex")
	if codexOn {
		codex = intent.RunHarness{ID: model.HarnessCodex, PlanModel: opts.String("intent-codex-plan-model"),
			BuildModel: opts.String("intent-codex-build-model")}
		models = append(models, codex.PlanModel, codex.BuildModel)
	}
	// The models a Project may choose are allowlisted with the stage
	// flags', so each must be runnable and, on brokered claude, covered by
	// the provider's model map at startup, never mid-run.
	allowlist := runnercfg.SplitList(opts.String("intent-model-allowlist"))
	enabled, err := runnercfg.ResolveWithoutBrokerProbe(ctx, opts, cs, agentNS, runners, runnercfg.Restrict(opts),
		append(slices.Clone(models), allowlist...), models...)
	if err != nil {
		return intent.RunHarness{}, intent.RunHarness{}, err
	}
	if claude.ID, err = stageHarness(claude, enabled); err != nil {
		return intent.RunHarness{}, intent.RunHarness{}, err
	}
	if !slices.Contains([]string{model.HarnessClaude, model.HarnessFake}, claude.ID) {
		return intent.RunHarness{}, intent.RunHarness{}, fmt.Errorf("--intent-plan-model and --intent-build-model "+
			"must resolve to brokered claude, and they resolve to %s (a Project runs on codex by its spec.harness, "+
			"under --intent-codex)", claude.ID)
	}
	if r := runners[claude.ID]; claude.ID == model.HarnessClaude && !r.Brokered {
		return intent.RunHarness{}, intent.RunHarness{}, errors.New(
			"intents run on brokered claude only, and the claude runner is not brokered")
	}
	for _, m := range roleModels {
		id, _, err := runnercfg.ResolveHarness(m, enabled)
		if err != nil {
			return intent.RunHarness{}, intent.RunHarness{}, err
		}
		if id != claude.ID {
			return intent.RunHarness{}, intent.RunHarness{}, fmt.Errorf("--intent-evaluate-model and "+
				"--intent-checkreview-model must resolve to the harness intents plan and build on (%s), and %s "+
				"resolves to %s", claude.ID, m, id)
		}
	}
	if claude.Allowed, codex.Allowed, err = allowedModels(allowlist, enabled, claude.ID, codexOn); err != nil {
		return intent.RunHarness{}, intent.RunHarness{}, err
	}
	if !codexOn {
		return claude, intent.RunHarness{}, nil
	}
	if r, ok := runners[model.HarnessCodex]; !ok || !r.Brokered || !slices.Contains(enabled, model.HarnessCodex) {
		return intent.RunHarness{}, intent.RunHarness{}, errors.New("--intent-codex needs the brokered codex " +
			"runner: --codex-agent-image with --broker-url, and codex among --harnesses")
	}
	id, err := stageHarness(codex, enabled)
	if err != nil {
		return intent.RunHarness{}, intent.RunHarness{}, err
	}
	if id != model.HarnessCodex {
		return intent.RunHarness{}, intent.RunHarness{}, fmt.Errorf("--intent-codex-plan-model and "+
			"--intent-codex-build-model must resolve to the codex harness, and they resolve to %s", id)
	}
	return claude, codex, nil
}

// allowedModels splits --intent-model-allowlist by the intent harness each
// entry resolves to: claude's (claudeID: brokered claude, or the fake
// harness) and, under --intent-codex, codex's. An entry that resolves to
// neither is refused: no Project could run on it.
func allowedModels(allowlist, enabled []string, claudeID string, codexOn bool) (claude, codex intent.ModelList,
	err error) {
	var forClaude, forCodex []string
	for _, id := range allowlist {
		h, _, err := runnercfg.ResolveHarness(id, enabled)
		if err != nil {
			return "", "", err
		}
		switch {
		case h == claudeID:
			forClaude = append(forClaude, id)
		case codexOn && h == model.HarnessCodex:
			forCodex = append(forCodex, id)
		default:
			return "", "", fmt.Errorf("--intent-model-allowlist names %s, which runs on %s, and intents run on %s "+
				"(and on codex only with --intent-codex)", id, h, claudeID)
		}
	}
	return intent.NewModelList(forClaude...), intent.NewModelList(forCodex...), nil
}

// stageHarness is the one harness both of h's stage models resolve to.
func stageHarness(h intent.RunHarness, enabled []string) (string, error) {
	plan, _, err := runnercfg.ResolveHarness(h.PlanModel, enabled)
	if err != nil {
		return "", err
	}
	build, _, err := runnercfg.ResolveHarness(h.BuildModel, enabled)
	if err != nil {
		return "", err
	}
	if plan != build {
		return "", fmt.Errorf("the plan model %s runs on %s and the build model %s on %s; an intent runs both "+
			"stages on one harness", h.PlanModel, plan, h.BuildModel, build)
	}
	return plan, nil
}

// harnessesAndDNS is harnesses, then --agent-dns held to the harnesses
// intents run on: the intent Jobs' pods can run without a resolver only on a
// brokered harness (claude or codex) or the fake one, which is all harnesses
// admits. Under --intent-codex the codex runner is also given what it
// injects into a build's repository-declared image (agent-runner and the
// codex CLI): intent builds run on the repository's own image, and codex,
// brokered, carries no credential there.
func harnessesAndDNS(ctx context.Context, opts *cli.Options, cs kubernetes.Interface, agentNS string,
	runners map[string]jobs.Runner) (claude, codex intent.RunHarness, mode jobs.DNSMode, err error) {
	if claude, codex, err = harnesses(ctx, opts, cs, agentNS, runners); err != nil {
		return intent.RunHarness{}, intent.RunHarness{}, "", err
	}
	ids := []string{claude.ID}
	if codex.ID != "" {
		ids = append(ids, codex.ID)
		r := runners[codex.ID]
		r.Inject = runnercfg.Inject(codex.ID)
		runners[codex.ID] = r
	}
	mode, err = runnercfg.AgentDNS(opts, runners, ids)
	return claude, codex, mode, err
}

// releaseNamespace is the namespace the controller runs in: --namespace,
// else the pod's own (POD_NAMESPACE).
func releaseNamespace(opts *cli.Options) (string, error) {
	namespace := opts.String("namespace")
	if namespace == "" {
		namespace = os.Getenv("POD_NAMESPACE")
	}
	if namespace == "" {
		return "", errors.New("namespace is required (--namespace or POD_NAMESPACE)")
	}
	return namespace, nil
}

func serve(ctx context.Context, opts *cli.Options) error {
	prov, shutdown, err := telemetry.Init(ctx, telemetry.Config{
		Dir:            os.Getenv("MAUL_TELEMETRY_DIR"),
		Level:          opts.LogLevel,
		ServiceName:    "intent-controller",
		ServiceVersion: version.Version,
	})
	if err != nil {
		prov.Logger.LogAttrs(ctx, slog.LevelWarn, "telemetry disabled", slog.Any("error", err))
	}
	defer func() { _ = shutdown(context.WithoutCancel(ctx)) }()
	log := prov.Logger

	namespace, err := releaseNamespace(opts)
	if err != nil {
		return err
	}
	agentNS := opts.String("agent-namespace")
	set, err := settings(opts, namespace, agentNS)
	if err != nil {
		return err
	}
	runners, err := runnercfg.Runners(opts)
	if err != nil {
		return err
	}
	sizes, meter, err := readRunConfig(opts, log)
	if err != nil {
		return err
	}
	cpuRequest, memoryRequest, cpuLimit, memoryLimit := sizes.defaults.Strings()

	mgr, err := kube.NewManager(kube.Options{
		Kubeconfig:              opts.String("kubeconfig"),
		LeaderElectionID:        "maul-intent-controller-leader",
		LeaderElectionNamespace: namespace,
		Namespaces:              []string{namespace},
		AgentNamespace:          agentNS,
		HealthAddr:              opts.String("health-addr"),
		Log:                     log,
		// Only intent-labelled ConfigMaps: never the Finding transcripts
		// beside them in the release namespace.
		ConfigMapSelector:  intent.ConfigMapSelector(),
		RepositorySelector: intent.ConfigMapSelector(),
	})
	if err != nil {
		return err
	}
	cfg, err := kube.RestConfig(opts.String("kubeconfig"))
	if err != nil {
		return err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("kubernetes clientset: %w", err)
	}
	claude, codex, agentDNS, err := harnessesAndDNS(ctx, opts, cs, agentNS, runners)
	if err != nil {
		return err
	}
	set.Claude, set.Codex = claude, codex
	harnessID := claude.ID
	probeDNS, err := evaluatorProbeDNS(set, resolvConf(os.Getenv))
	if err != nil {
		return err
	}

	jobsCfg := jobs.Config{
		Namespace:      agentNS,
		ServiceAccount: opts.String("agent-service-account"),
		Deadline:       opts.Duration("intent-job-deadline"),
		TTL:            opts.Duration("job-ttl"),
		Runners:        runners,
		BrokerAudience: opts.String("broker-token-audience"),

		CPURequest:    cpuRequest,
		MemoryRequest: memoryRequest,
		CPULimit:      cpuLimit,
		MemoryLimit:   memoryLimit,

		EphemeralStorage:      sizes.ephemeralStorage,
		AllowRepositoryImages: sizes.repositoryImages,
		DNS:                   agentDNS,
		NetworkProfiles:       sizes.profilesOn,
		SandboxProbeDNS:       probeDNS,
	}
	runner := intent.NewJobRunner(cs, jobsCfg, log)
	// Forges come from the cache; their Secrets are never cached, so the
	// manager's client reads each one live.
	forges := forge.NewStore(mgr.GetClient())
	gh := intent.NewForgeGitHub(forges, namespace)
	images := runnerguard.Guard{
		Enabled:         sizes.repositoryImages,
		Breaker:         runnerguard.NewBreaker("intent-controller", log),
		ProfilesEnabled: sizes.profilesOn,
		Profiles:        sizes.profiles,
		ProxyBase:       sizes.proxyBase,
		// A registry grant or tunnel host may name no Forge's web host
		// (CheckForges; WebHosts lists them through this Store's reader).
		Forges: forges,
	}
	nudger := intent.NewNudger()

	if err := (&intent.ProjectReconciler{
		Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), GitHub: gh, Settings: set, Classes: sizes.classes,
		Nudger: nudger, RepositoryImagesOff: !sizes.repositoryImages, Log: log,
	}).SetupWithManager(mgr); err != nil {
		return err
	}
	if err := (&intent.IntentReconciler{
		Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), GitHub: gh, Settings: set, Images: images,
		Classes: sizes.classes, Nudger: nudger, Log: log,
		// Under --user-credentials enforce, a person's run waits for their
		// credential: the record is read uncached by name (get only).
		Credentials: kubecred.Reader{Client: mgr.GetAPIReader(), Namespace: set.Namespace},
		// The run scheduler's harness: a person's run needs its provider's
		// credential (credgate.ProviderFor).
		Harness: harnessID,
	}).SetupWithManager(mgr); err != nil {
		return err
	}
	if err := (&intent.RunReconciler{
		Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), Jobs: runner, GitHub: gh, Settings: set,
		MaxConcurrent: opts.Int("intent-max-concurrent-runs"), Harness: harnessID,
		Paused: opts.Bool(runnercfg.AgentsPausedFlag),
		Images: images, Classes: sizes.classes, MaxChangesetEntries: opts.Int("changeset-max-entries"),
		Meter: meter, Usage: usagerollup.New(mgr.GetClient(), mgr.GetAPIReader(), set.Namespace, meter.On(), log),
		Log: log,
	}).SetupWithManager(mgr); err != nil {
		return err
	}
	if err := (&intent.TTLReconciler{
		Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), TTL: opts.Duration("intent-ttl"), Log: log,
	}).SetupWithManager(mgr); err != nil {
		return err
	}
	if err := setupAgentsHost(mgr, set, cs, jobs.New(cs, jobsCfg, log), images, harnessID, log); err != nil {
		return err
	}
	if err := setupPreviewSource(mgr, opts); err != nil {
		return err
	}

	logVerificationOff(ctx, log, set)
	// Read-only, uncached, before the manager starts: the API reader.
	if err := slice1Census(ctx, log, mgr.GetAPIReader(), set); err != nil {
		return err
	}
	log.LogAttrs(ctx, slog.LevelInfo, "intent-controller starting",
		slog.String("namespace", namespace),
		slog.String("agent_namespace", agentNS),
		slog.String("harness", harnessID),
		slog.String("codex_harness", codex.ID),
		slog.Int("max_concurrent_runs", opts.Int("intent-max-concurrent-runs")),
		slog.Bool("agents_paused", opts.Bool(runnercfg.AgentsPausedFlag)),
		slog.Bool("multi_repo", set.MultiRepo),
		slog.Bool("dependencies", !set.IgnoreDependencies),
		slog.Bool("verification", set.Verification),
		slog.Bool("evaluator", set.Evaluator),
		slog.Bool("evaluate_codex", set.EvaluateCodex),
		slog.Bool("evaluator_agents", set.EvaluatorAgents),
		slog.Int("max_tool_pods", set.MaxToolPods),
		slog.String("attribution_mode", string(set.Attribution)),
		slog.String("user_credentials", string(set.UserCredentials)),
		slog.Bool("usage_measurement", meter.On()),
		slog.Bool("repository_images", sizes.repositoryImages),
		slog.String("agent_resources", sizes.defaults.String()),
		slog.String("resource_classes", sizes.classes.Describe()))

	if err := mgr.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
