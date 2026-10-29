// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/attribution"
	"github.com/devthenet-labs/maul/internal/credgate"
	"github.com/devthenet-labs/maul/internal/forge"
)

// Names derived from an Intent or an IntentRun. Each is created under its
// name once and adopted on AlreadyExists only when its controller owner is
// the one creating it.
func inputConfigMapName(intent string, revision int32) string {
	return fmt.Sprintf("%s-input-r%d", intent, revision)
}

func planConfigMapName(intent string, revision int32) string {
	return fmt.Sprintf("%s-plan-r%d", intent, revision)
}

func runInputName(run string) string { return run + "-input" }

func runRepositoryName(run string) string { return run + "-src" }

// branchName is the branch every pull request of the intent is opened from.
func branchName(intent string) string { return v1alpha1.IntentBranchPrefix + intent }

// ConfigMap data keys.
const (
	// keyIssue is the handoff every run's input ConfigMap carries as the
	// Job's issue.md, and the input snapshot's rendered request.
	keyIssue = "issue.md"
	// keyInvestigation is a build run's approved plan, the Job's
	// investigation.md; a replan's or revival's plan run carries its
	// context file under it.
	keyInvestigation = "investigation.md"
	// keyContext is the input snapshot's context file on a replan or
	// revival (planContext): beside the request, never part of it or its
	// digest, and absent on the first plan.
	keyContext = "context.md"
	// keyApprovedPlan keeps the exact approved prefix of a revise handoff.
	keyApprovedPlan   = "approved-plan.md"
	keyCheckSignature = "check-signature"
	// keyCheckNames are the names of the failed checks a check-fix round
	// fixes, one per line: what its pull request notice names. Never part of
	// the Job's input.
	keyCheckNames   = "check-names"
	keyInputRefusal = "input-refusal"
	keyNoFeedback   = "no-usable-feedback"
	// keyOpenPullRequests is a plan run's list of the other intents' open
	// pull requests (templates.EncodeOpenPullRequests), absent for none:
	// the plan Job's MAUL_OPEN_PULL_REQUESTS, never part of its request or
	// its digest.
	keyOpenPullRequests = "open-pull-requests.json"
	// keyPlan is the plan report exactly as the planner wrote it.
	keyPlan = "plan.md"
	// The input snapshot's parts, kept beside the rendered request so an
	// approval can re-render it from the issue as it is now.
	keyTitle        = "title"
	keyBody         = "body"
	keyRepositories = "repositories"
	keyComments     = "comments"
)

// ConfigMapSelector selects the ConfigMaps intent-controller reads and
// watches through its cache: every one it creates (the input snapshot, the
// plan, a run's input, a run's transcript) carries LabelIntent. Its manager
// confines the ConfigMap informer to them (kube.Options.ConfigMapSelector),
// so the Finding flow's transcripts beside them never fill its memory. One
// it reads that may lack the label (an object met under a derived name) is
// read through the API reader.
func ConfigMapSelector() labels.Selector {
	req, err := labels.NewRequirement(v1alpha1.LabelIntent, selection.Exists, nil)
	if err != nil {
		// LabelIntent is a constant, valid key: never. Fail closed.
		return labels.Nothing()
	}
	return labels.NewSelector().Add(*req)
}

// digest is the sha256 digest of b, as every intent digest is written.
func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// normalizeRepoURL compares repository URLs the way forges do:
// case-insensitively, with a trailing slash and any .git suffix dropped.
func normalizeRepoURL(u string) string {
	u = strings.ToLower(strings.TrimRight(strings.TrimSpace(u), "/"))
	return strings.TrimSuffix(u, ".git")
}

func sameRepo(a, b string) bool { return normalizeRepoURL(a) == normalizeRepoURL(b) }

// repoSlug is "owner/name" of a repository URL, or the URL itself when it
// does not parse.
func repoSlug(u string) string {
	if _, repo, err := forge.ParseRepoURL(u); err == nil {
		return repo.String()
	}
	return u
}

// terminal reports the phases an Intent ends in.
func terminal(p v1alpha1.IntentPhase) bool { return v1alpha1.IntentTerminal(p) }

// microUSD parses a UsageSummary cost ("1.234567") into micro-USD, the unit
// intent usage is summed in; "" and anything unparseable are zero.
func microUSD(cost string) int64 {
	if cost == "" {
		return 0
	}
	whole, frac, _ := strings.Cut(cost, ".")
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || w < 0 || w > math.MaxInt64/1_000_000 {
		return 0
	}
	frac = (frac + "000000")[:6]
	f, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0
	}
	return w*1_000_000 + f
}

// Settings are the controller-wide limits the reconcilers share.
type Settings struct {
	// Namespace is where Projects, Intents, IntentRuns, Repositories and
	// ConfigMaps live.
	Namespace string
	// AgentNamespace is where the agent Jobs run, recorded on JobRef.
	AgentNamespace string
	// PollInterval paces discovery and every non-terminal intent's poll of
	// its issue; ApprovalPollInterval an intent awaiting approval's;
	// PRPollInterval an intent in review's poll of its pull requests.
	PollInterval         time.Duration
	ApprovalPollInterval time.Duration
	PRPollInterval       time.Duration
	// RateLimitFloor pauses polling while the installation has fewer core
	// requests left than this.
	RateLimitFloor int
	// MaxAttempts bounds the counted attempts of one stage's round.
	MaxAttempts int32
	// Plan, Build and Revise are the per-stage ceilings: a Project's limits are
	// clamped to them, and the Job's stage configuration carries them.
	Plan, Build, Revise StageCeiling
	// Evaluate and CheckReview are the evaluator's and the check reviewer's
	// ceilings, set under --intent-evaluator (the evaluate timeout from
	// --intent-evaluate-timeout, the rest maul's defaults); no run of either
	// stage is created or launched while the evaluator is off
	// (errStageNotEnabled, stageEnabled).
	Evaluate, CheckReview StageCeiling
	// MultiRepo runs intents of Projects that list more than one repository
	// (--intent-multi-repo). Off, such a Project is not Ready, and every
	// Intent of one is held Blocked (UnsupportedRepositories): nothing is
	// planned, built, pushed or revised for it until the flag is on again or
	// the Project lists one repository, so turning the flag off is a real
	// rollback. A one-repository Project behaves the same either way.
	MultiRepo bool
	// Previews is --intent-previews-enabled: the preview projection
	// (PreviewSourceReconciler) runs. The intent reconciler then records, at
	// review start, the default-branch head each previewed repository the
	// intent has no pull request in runs in its preview
	// (status.previewBases); off, it reads nothing for previews.
	Previews bool
	// Attribution is --attribution-mode: under attribute every new
	// IntentRun is stamped with whom it is for (attribute.go) and every
	// agent Job carries its run's attributed-to annotation; under off (the
	// zero value) neither is.
	Attribution attribution.Mode
	// UserCredentials is --user-credentials: under enforce a plan, build
	// or revise run attributed to a person is not created while that
	// person has no Valid ModelCredential, and the Intent is Blocked
	// (CredentialRequired) until it is (credential_hold.go); off and
	// attribute (the zero value is off) create every run as before.
	UserCredentials credgate.Mode
	// AccountURL is the status page's Account page (--account-url), named
	// in a credential block's message; may be empty.
	AccountURL string
	// CostBudgets is --broker-limit-unit cost: the egress broker counts
	// spend in micro-USD, so every run is granted a cost budget
	// (IntentRunGrant.BudgetMicroUSD) and its agent Job and pod carry it as
	// provider.BudgetAnnotation. Off (the token unit, the default), no run
	// records a budget and no Job carries one.
	CostBudgets bool
	// BrokerCostPerHour is the broker's hourly cost limit in micro-USD
	// (--broker-cost-per-hour), zero when it has none or counts tokens. A
	// Project whose run budget is more than half of it is not Ready
	// (ReasonRunBudgetOverHourlyLimit): one such run could spend the
	// cluster's hour alone.
	BrokerCostPerHour int64
	// JobDeadline is --intent-job-deadline, the least activeDeadlineSeconds
	// an intent Job gets, independent of the stage timeouts; JobDeadlineMax
	// is --intent-job-deadline-max, the most a run's stage timeout can
	// stretch it to (jobDeadline), which every stage timeout plus
	// JobDeadlineMargin fits under.
	JobDeadline    time.Duration
	JobDeadlineMax time.Duration
	// Verification is --intent-verification: an intent in review whose
	// Project verifies (spec.verification.enabled) has its approved plan's
	// acceptance criteria recorded and run against its Preview
	// (verify_loop.go), which needs Previews: without them the intent's
	// verification is Off (VerificationWithoutPreviews). Off (the default)
	// the intent reconciler writes no acceptance or verification state and
	// creates no PreviewVerification, whatever the Project says.
	Verification bool
	// Evaluator is --intent-evaluator (SPEC Q11): a verifying Project's
	// plans are asked for acceptance outcomes, which the agent evaluator
	// checks, and an approved plan's outcomes are recorded
	// (status.acceptance.outcomes) and evaluated against the Preview, one
	// round at a time (evaluate_round.go); an open intent whose approved
	// plan states slice-1 criteria gets one replan notice and is not
	// verified until a new plan is approved (DESIGN 7.6). Off (the default)
	// slice 1 runs unchanged. serve refuses it without Verification and
	// Previews.
	Evaluator bool
	// EvaluateModel and CheckReviewModel are the canonical models a claude
	// Project's evaluate and checkreview runs run on
	// (--intent-evaluate-model, --intent-checkreview-model); a codex
	// Project's run on Codex.BuildModel.
	EvaluateModel, CheckReviewModel string
	// DefaultHandoff is --intent-default-handoff, the hand-off mode of a
	// Project that sets none (spec.verification.handoff), and HandoffMax
	// --intent-handoff-max, the gated hand-off's bound when it sets none.
	DefaultHandoff v1alpha1.HandoffMode
	HandoffMax     time.Duration
	// EvaluateCodex is --intent-evaluate-codex (SPEC Q16): a codex Project's
	// intents are evaluated on codex. Off (the default), they are not
	// evaluated at all: their hand-off is early, and the Project says so
	// (ConditionEvaluatorUnverified).
	EvaluateCodex bool
	// EvaluationCostMax is --intent-evaluation-cost-max in micro-USD: the
	// most one intent's evaluate and checkreview runs may spend, whatever
	// its Project's limits.maxEvaluateCostMicroUSD says; zero leaves the
	// Project's alone (evaluationCeiling).
	EvaluationCostMax int64
	// MaxToolCalls is --intent-max-tool-calls: the most tool calls an
	// evaluation's tool pod allows, whatever its Project's
	// verification.maxToolCalls says; zero leaves the Project's alone.
	MaxToolCalls int32
	// VerifyTimeoutMax is --intent-verify-timeout-max: the most a Project's
	// verification.timeout gives a tester Job (verifyTimeout). Zero is
	// DefaultVerifyTimeoutMax.
	VerifyTimeoutMax time.Duration
	// Codex is the codex harness intents may run on (--intent-codex): the
	// harness id and the canonical model of each stage. The zero value, the
	// default, runs no intent on codex: a Project asking for it is not Ready
	// (ReasonHarnessNotEnabled), and a codex run of its intents is not
	// launched but waits Pending (harnessOff).
	Codex RunHarness
}

// RunHarness is a harness intent runs launch on: its harness id (claude,
// codex, or fake in dev and tests) and the canonical model each stage runs.
// A revise or check-fix run runs the build model.
type RunHarness struct {
	ID         string
	PlanModel  string
	BuildModel string
}

// harnessOff reports whether h is a harness intents of this controller do
// not run on: codex while --intent-codex is off. Claude, the default, is
// always on.
func (s Settings) harnessOff(h v1alpha1.ProjectHarness) bool {
	return h.IsCodex() && s.Codex.ID == ""
}

// JobDeadlineMargin is what an intent Job's deadline allows past its stage
// timeout: the prepare init, report repair rounds and collection. The binary
// refuses to start unless every stage timeout plus it fits under
// JobDeadlineMax.
const JobDeadlineMargin = 15 * time.Minute

// jobDeadline is the activeDeadlineSeconds of a run whose stage timeout is
// timeout: the timeout plus JobDeadlineMargin, capped by JobDeadlineMax, and
// never less than JobDeadline, so a short stage keeps the deadline every
// intent Job had before deadlines were derived, while a longer stage
// stretches only its own Jobs. Zero keeps the jobs Config's.
func (s Settings) jobDeadline(timeout time.Duration) time.Duration {
	derived := time.Duration(0)
	if timeout > 0 {
		derived = timeout + JobDeadlineMargin
		if s.JobDeadlineMax > 0 && derived > s.JobDeadlineMax {
			derived = s.JobDeadlineMax
		}
	}
	return max(s.JobDeadline, derived)
}

// ceiling is stage's controller ceiling; an unknown stage's is zero, never
// another stage's.
func (s Settings) ceiling(stage v1alpha1.IntentStage) StageCeiling {
	switch stage {
	case v1alpha1.IntentStagePlan:
		return s.Plan
	case v1alpha1.IntentStageBuild:
		return s.Build
	case v1alpha1.IntentStageRevise:
		return s.Revise
	case v1alpha1.IntentStageEvaluate:
		return s.Evaluate
	case v1alpha1.IntentStageCheckReview:
		return s.CheckReview
	}
	return StageCeiling{}
}

// VerificationWithoutPreviews reports --intent-verification without
// --intent-previews-enabled: verification runs against the Preview only the
// preview projection writes, so every verifying Project's intents are Off,
// and the Project (ConditionVerificationOff) and the issue's status comment
// say why. serve warns of it rather than refusing to start.
func (s Settings) VerificationWithoutPreviews() bool {
	return s.Verification && !s.Previews
}

// verifyTimeout is a verification's tester wall clock for Project p: its
// verification.timeout (or the default), never more than VerifyTimeoutMax.
func (s Settings) verifyTimeout(p *v1alpha1.Project) time.Duration {
	limit := s.VerifyTimeoutMax
	if limit <= 0 {
		limit = DefaultVerifyTimeoutMax
	}
	return min(p.Spec.VerificationTimeout(), limit)
}

// stageLimits are the Project's limits for stage; an unknown stage's are
// zero, never another stage's.
func stageLimits(p *v1alpha1.Project, stage v1alpha1.IntentStage) v1alpha1.StageLimits {
	switch stage {
	case v1alpha1.IntentStagePlan:
		return p.Spec.Limits.Plan
	case v1alpha1.IntentStageBuild:
		return p.Spec.Limits.Build
	case v1alpha1.IntentStageRevise:
		return p.Spec.Limits.Revise
	case v1alpha1.IntentStageEvaluate:
		return p.Spec.Limits.Evaluate
	case v1alpha1.IntentStageCheckReview:
		return p.Spec.Limits.CheckReview
	}
	return v1alpha1.StageLimits{}
}

// runBudget is the cost budget of one run of stage in micro-USD: the
// stage's maxCostMicroUSD, or the Project's per-intent ceiling when that is
// unset, and never more than that ceiling (the schema holds it there too).
func runBudget(p *v1alpha1.Project, stage v1alpha1.IntentStage) int64 {
	ceiling := maxCostMicroUSD(p)
	if b := stageLimits(p, stage).MaxCostMicroUSD; b > 0 && b < ceiling {
		return b
	}
	return ceiling
}

// multiRepoOff reports a Project these settings run no intent of: one that
// lists more than one repository while MultiRepo is off.
func (s Settings) multiRepoOff(p *v1alpha1.Project) bool {
	return !s.MultiRepo && len(p.Spec.Repositories) > 1
}

// StageCeiling bounds one agent stage. IdleTimeout ends a run of the stage
// that makes no progress (no model turn, no tool result) for that long; zero
// disables the watchdog.
type StageCeiling struct {
	MaxTurns    int32
	TokenBudget int64
	Timeout     time.Duration
	IdleTimeout time.Duration
}

// Defaults the design sets.
const (
	DefaultPollInterval         = 60 * time.Second
	DefaultApprovalPollInterval = 30 * time.Second
	DefaultPRPollInterval       = 60 * time.Second
	DefaultRateLimitFloor       = 1000
	DefaultMaxAttempts          = 2
	DefaultTTL                  = 14 * 24 * time.Hour
	// DefaultVerifyTimeoutMax is --intent-verify-timeout-max's default: the
	// chart's preview.verification.deadlineMaxSeconds, which preview-controller
	// holds a tester Job's deadline to.
	DefaultVerifyTimeoutMax = 30 * time.Minute
	// DefaultEvaluateTimeout is --intent-evaluate-timeout's default, an
	// evaluate run's wall clock (DESIGN 5); MaxEvaluateTimeout its most, so
	// the round's tool pod (the evaluate timeout plus
	// EvaluationToolsMargin) fits a PreviewVerification's one-hour bound.
	DefaultEvaluateTimeout = 20 * time.Minute
	MaxEvaluateTimeout     = time.Hour - EvaluationToolsMargin
	// EvaluationToolsMargin is what a round's tool pod lives past its
	// evaluate run's timeout (DESIGN 5: the evaluator's start-up and the
	// confirmation runs after it).
	EvaluationToolsMargin = 5 * time.Minute
	// DefaultEvaluateMaxTurns and DefaultEvaluateTokenBudget bound an
	// evaluate run (DESIGN 5: 60 turns); a Project's limits.evaluate may
	// lower them.
	DefaultEvaluateMaxTurns    int32 = 60
	DefaultEvaluateTokenBudget int64 = 400000
	// DefaultCheckReviewMaxTurns, DefaultCheckReviewTokenBudget and
	// DefaultCheckReviewTimeout bound a checkreview run: its turns bound
	// each review session (DESIGN 5: 3), the rest the whole run.
	DefaultCheckReviewMaxTurns    int32 = 3
	DefaultCheckReviewTokenBudget int64 = 100000
	DefaultCheckReviewTimeout           = 15 * time.Minute
)

// withDefaults fills unset settings.
func (s Settings) withDefaults() Settings {
	if s.PollInterval <= 0 {
		s.PollInterval = DefaultPollInterval
	}
	if s.ApprovalPollInterval <= 0 {
		s.ApprovalPollInterval = DefaultApprovalPollInterval
	}
	if s.PRPollInterval <= 0 {
		s.PRPollInterval = DefaultPRPollInterval
	}
	if s.MaxAttempts <= 0 {
		s.MaxAttempts = DefaultMaxAttempts
	}
	return s
}

// grant is what one run of stage is granted: the Project's limit for the
// stage clamped by the controller's ceiling (a zero limit takes the
// ceiling), and the ceiling's wall clock.
func (s Settings) grant(p *v1alpha1.Project, stage v1alpha1.IntentStage) v1alpha1.IntentRunGrant {
	ceiling, limits := s.ceiling(stage), stageLimits(p, stage)
	turns := ceiling.MaxTurns
	if limits.MaxTurns > 0 && limits.MaxTurns < turns {
		turns = limits.MaxTurns
	}
	tokens := ceiling.TokenBudget
	if limits.TokenBudget > 0 && limits.TokenBudget < tokens {
		tokens = limits.TokenBudget
	}
	timeout := ceiling.Timeout
	if limits.Timeout != nil && limits.Timeout.Duration > 0 && limits.Timeout.Duration < timeout {
		timeout = limits.Timeout.Duration
	}
	g := v1alpha1.IntentRunGrant{
		MaxTurns:            turns,
		TokenBudget:         tokens,
		TimeoutMilliseconds: timeout.Milliseconds(),
	}
	if s.CostBudgets {
		g.BudgetMicroUSD = runBudget(p, stage)
	}
	return g
}

// budgetOverHourly reports the first stage whose run budget is more than
// half the broker's hourly cost limit, with that budget: the guard that one
// run cannot starve every other (design "limits that fit large features"
// §1.4). Nothing is reported unless the broker counts cost under an hourly
// limit.
func (s Settings) budgetOverHourly(p *v1alpha1.Project) (v1alpha1.IntentStage, int64) {
	if !s.CostBudgets || s.BrokerCostPerHour <= 0 {
		return "", 0
	}
	for _, stage := range []v1alpha1.IntentStage{
		v1alpha1.IntentStagePlan, v1alpha1.IntentStageBuild, v1alpha1.IntentStageRevise,
	} {
		// A budget is at most the schema's $1000 cap, so doubling it cannot
		// overflow; exactly half the limit is allowed.
		if b := runBudget(p, stage); 2*b > s.BrokerCostPerHour {
			return stage, b
		}
	}
	return "", 0
}

// runTimeout is a granted run's stage timeout: its grant's, or, for a run
// granted with none, the controller's stage timeout.
func (s Settings) runTimeout(run *v1alpha1.IntentRun) time.Duration {
	if ms := run.Spec.Grant.TimeoutMilliseconds; ms > 0 {
		return time.Duration(ms) * time.Millisecond
	}
	return s.ceiling(run.Spec.Stage).Timeout
}

// runBudgetFor is the cost budget a run's Job carries: none unless the
// broker counts cost; then its grant's, or, for a run granted before the
// unit was cost, the Project's budget for its stage now, so no intent Job
// launches without one.
func (s Settings) runBudgetFor(run *v1alpha1.IntentRun, p *v1alpha1.Project) int64 {
	if !s.CostBudgets {
		return 0
	}
	if b := run.Spec.Grant.BudgetMicroUSD; b > 0 {
		return b
	}
	return runBudget(p, run.Spec.Stage)
}

// Project settings with the schema defaults applied, for a Project the API
// server never defaulted (a fake client's).
func approveLabel(p *v1alpha1.Project) string {
	if p.Spec.Labels.Approve != "" {
		return p.Spec.Labels.Approve
	}
	return v1alpha1.DefaultApproveLabel
}

func maxActiveIntents(p *v1alpha1.Project) int32 {
	if p.Spec.Limits.MaxActiveIntents > 0 {
		return p.Spec.Limits.MaxActiveIntents
	}
	return v1alpha1.DefaultMaxActiveIntents
}

func maxCostMicroUSD(p *v1alpha1.Project) int64 {
	if p.Spec.Limits.MaxCostMicroUSD > 0 {
		return p.Spec.Limits.MaxCostMicroUSD
	}
	return v1alpha1.DefaultMaxCostMicroUSD
}

func requireRepositoryImage(p *v1alpha1.Project) bool {
	return p.Spec.RequireRepositoryImage == nil || *p.Spec.RequireRepositoryImage
}

// isApprover reports whether login is one of the Project's approvers,
// compared case-insensitively as GitHub compares logins.
func isApprover(p *v1alpha1.Project, login string) bool {
	for _, a := range p.Spec.Approvers.Logins {
		if strings.EqualFold(a, login) {
			return true
		}
	}
	return false
}
