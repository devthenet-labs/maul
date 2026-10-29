// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"slices"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"

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
	f.Bool("intent-verification", false,
		"run the approved plan's acceptance criteria against each intent's Preview for Projects with "+
			"spec.verification.enabled (PreviewVerification requests a separately enabled preview-controller "+
			"answers); needs --intent-previews-enabled, without which verification is Off and each verifying "+
			"Project says why. Off, no acceptance or verification state is written")
	f.Duration("intent-verify-timeout-max", intent.DefaultVerifyTimeoutMax,
		"the most a Project's verification.timeout gives a tester Job, between 1m and 1h; keep it at or under "+
			"preview-controller's tester deadline maximum (chart preview.verification.deadlineMaxSeconds)")
	f.Bool("intent-evaluator", false,
		"verify the approved plan's acceptance outcomes with the agent evaluator, which explores each intent's "+
			"Preview through a slot tool pod and writes its own checks (needs --intent-verification and "+
			"--intent-previews-enabled). Off, slice 1's acceptance criteria run unchanged")
	f.String("intent-evaluate-model", "anthropic/claude-sonnet-5",
		"canonical model a claude Project's evaluate runs run (--intent-evaluator)")
	f.String("intent-checkreview-model", "anthropic/claude-sonnet-5",
		"canonical model a claude Project's checkreview runs run (--intent-evaluator)")
	f.Duration("intent-evaluate-timeout", intent.DefaultEvaluateTimeout,
		"wall-clock limit of an evaluate run, between 1m and 55m (its tool pod lives 5m longer, within the "+
			"hour a verification may take)")
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
	f.String("intent-plan-model", "anthropic/claude-sonnet-5", "canonical model the plan stage runs")
	f.String("intent-build-model", "anthropic/claude-sonnet-5", "canonical model the build stage runs")
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

// verification reads the acceptance-verification switch and the tester
// timeout ceiling, held to a PreviewVerification's own timeout bounds.
// Verification runs against the Preview the preview projection writes;
// without it the controller still starts, with verification Off
// (Settings.VerificationWithoutPreviews: serve warns, and the Projects and
// issues say why), rather than taking intents down with it.
func verification(opts *cli.Options, s *intent.Settings) error {
	s.Verification = opts.Bool("intent-verification")
	s.VerifyTimeoutMax = opts.Duration("intent-verify-timeout-max")
	if s.VerifyTimeoutMax < time.Minute || s.VerifyTimeoutMax > time.Hour {
		return fmt.Errorf("--intent-verify-timeout-max %s must be between 1m and 1h", s.VerifyTimeoutMax)
	}
	return nil
}

// evaluator reads the agent evaluator's switch and, when it is on, its
// settings, each held to its bounds. It is refused without verification and
// previews: the evaluator judges each intent's Preview. Off, nothing of it
// reaches the settings, so slice 1 runs exactly as before.
func evaluator(opts *cli.Options, s *intent.Settings) error {
	if !opts.Bool("intent-evaluator") {
		return nil
	}
	if !s.Verification || !s.Previews {
		return errors.New("--intent-evaluator needs --intent-verification and --intent-previews-enabled: the " +
			"evaluator judges each intent's Preview")
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
	s.Evaluator = true
	s.EvaluateModel, s.CheckReviewModel = opts.String("intent-evaluate-model"), opts.String("intent-checkreview-model")
	s.DefaultHandoff, s.HandoffMax = handoff, handoffMax
	s.EvaluateCodex = opts.Bool("intent-evaluate-codex")
	s.EvaluationCostMax, s.MaxToolCalls = cost, int32(calls)
	s.Evaluate = intent.StageCeiling{MaxTurns: intent.DefaultEvaluateMaxTurns,
		TokenBudget: intent.DefaultEvaluateTokenBudget, Timeout: timeout}
	s.CheckReview = intent.StageCeiling{MaxTurns: intent.DefaultCheckReviewMaxTurns,
		TokenBudget: intent.DefaultCheckReviewTokenBudget, Timeout: intent.DefaultCheckReviewTimeout}
	return nil
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
	enabled, err := runnercfg.ResolveWithoutBrokerProbe(ctx, opts, cs, agentNS, runners, runnercfg.Restrict(opts),
		models, models...)
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
	set.Codex = codex
	harnessID := claude.ID
	probeDNS, err := evaluatorProbeDNS(set, resolvConf(os.Getenv))
	if err != nil {
		return err
	}

	runner := intent.NewJobRunner(cs, jobs.Config{
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
	}, log)
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
		Nudger: nudger, Log: log,
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
		Paused:    opts.Bool(runnercfg.AgentsPausedFlag),
		PlanModel: claude.PlanModel, BuildModel: claude.BuildModel,
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
	if opts.Bool("intent-previews-enabled") {
		if err := (&intent.PreviewSourceReconciler{
			Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), Scheme: mgr.GetScheme(),
		}).SetupWithManager(mgr); err != nil {
			return err
		}
	}

	if set.VerificationWithoutPreviews() {
		log.LogAttrs(ctx, slog.LevelWarn, "--intent-verification without --intent-previews-enabled: verification "+
			"runs against the intent's Preview, so it is off; each verifying Project says so in its VerificationOff "+
			"condition")
	}
	log.LogAttrs(ctx, slog.LevelInfo, "intent-controller starting",
		slog.String("namespace", namespace),
		slog.String("agent_namespace", agentNS),
		slog.String("harness", harnessID),
		slog.String("codex_harness", codex.ID),
		slog.Int("max_concurrent_runs", opts.Int("intent-max-concurrent-runs")),
		slog.Bool("agents_paused", opts.Bool(runnercfg.AgentsPausedFlag)),
		slog.Bool("multi_repo", set.MultiRepo),
		slog.Bool("verification", set.Verification),
		slog.Bool("evaluator", set.Evaluator),
		slog.Bool("evaluate_codex", set.EvaluateCodex),
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
