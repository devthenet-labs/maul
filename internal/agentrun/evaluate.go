// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/envelope"
	"github.com/devthenet-labs/maul/internal/harness"
	"github.com/devthenet-labs/maul/internal/maultool"
	"github.com/devthenet-labs/maul/internal/report"
	"github.com/devthenet-labs/maul/internal/runner"
	"github.com/devthenet-labs/maul/internal/templates"
)

// evaluateHandoffMaxBytes bounds the evaluate stage's handoff
// (input/issue.md): the approved outcomes and what the controller frames
// beside them, the request, the changed paths and a fix diff of up to
// templates.FixDiffMaxBytes among them. A larger one is refused, never cut,
// so the evaluator never judges against half an outcome.
const evaluateHandoffMaxBytes = 256 << 10

// EvaluateHandoff is the evaluate stage's handoff (input/issue.md), which
// the intent controller encodes as strict JSON (encoding/json of this
// type). agent-runner renders the evaluate prompt from it
// (templates.RenderEvaluatePrompt), which quotes every untrusted field
// (Request, Outcomes, ChangedPaths, FixDiff, PriorVerdicts, PriorReplay) as
// fenced data.
type EvaluateHandoff struct {
	// Round is the verification round, 1-based.
	Round int `json:"round"`
	// RunToken is the exploration token the agent names its data with
	// (the PreviewVerification's run token): templates' run token grammar.
	RunToken string `json:"run_token"`
	// Request is the intent's issue, quoted as context.
	Request string `json:"request,omitempty"`
	// Outcomes are the approved outcomes, in plan order: 1 to
	// acceptance.MaxOutcomes, valid by acceptance.ValidateOutcomes.
	Outcomes []acceptance.PlanOutcome `json:"outcomes"`
	// Components are the previewed components an http check can target.
	Components []EvaluateComponent `json:"components,omitempty"`
	// Tools are what the round's tool pod offers.
	Tools EvaluateTools `json:"tools"`
	// ChangedPaths are the files the pull requests change, with their line
	// counts (SPEC Q15).
	ChangedPaths []ChangedPath `json:"changed_paths,omitempty"`
	// FixDiff is the fix's diff since the last evaluated heads, on a round
	// after a fix; empty on round 1.
	FixDiff string `json:"fix_diff,omitempty"`
	// PriorRound is the last judged round before this one, 0 for none;
	// PriorVerdicts are maul's results of its outcomes, and PriorReplay how
	// its saved checks replayed (SPEC Q15).
	PriorRound    int            `json:"prior_round,omitempty"`
	PriorVerdicts []PriorVerdict `json:"prior_verdicts,omitempty"`
	PriorReplay   []PriorReplay  `json:"prior_replay,omitempty"`
}

// PriorVerdict is maul's result of one outcome in a previous round, with
// why it did not pass (which may quote that round's evaluator).
type PriorVerdict struct {
	Outcome string `json:"outcome"`
	Result  string `json:"result"`
	Reason  string `json:"reason,omitempty"`
}

// PriorReplay is how one saved check, bound to Outcome, replayed in a
// previous round.
type PriorReplay struct {
	Check   string `json:"check"`
	Outcome string `json:"outcome"`
	Result  string `json:"result"`
}

// EvaluateComponent is one previewed component: the key an http check's
// target names, and the path it is served under.
type EvaluateComponent struct {
	Key  string `json:"key"`
	Path string `json:"path"`
}

// EvaluateTools are what a round's tool pod offers beside http, logs and
// events.
type EvaluateTools struct {
	Browser          bool `json:"browser,omitempty"`
	Sandbox          bool `json:"sandbox,omitempty"`
	MaxToolCalls     int  `json:"max_tool_calls"`
	LookAroundRoutes int  `json:"look_around_routes,omitempty"`
}

// ChangedPath is one file a pull request changes, with its added and
// deleted line counts.
type ChangedPath struct {
	Path    string `json:"path"`
	Added   int    `json:"added"`
	Deleted int    `json:"deleted"`
}

// evaluateInput is what the evaluate stage is handed: the decoded handoff
// and a client of its tool pod.
type evaluateInput struct {
	handoff EvaluateHandoff
	tools   *maultool.Client
}

// roleHarness refuses a role stage's harness: one intents may not run on
// (intentHarness), or one that cannot hold a run to the evaluate posture
// (evaluatePosture). Both role stages run on the investigate stage's
// harness.
func (a *Agent) roleHarness() error {
	if err := a.intentHarness(a.cfg.InvestigateHarness); err != nil {
		return err
	}
	h, ok := harness.ByID(a.cfg.InvestigateHarness)
	if !ok {
		return fmt.Errorf("intent %s stage: unknown harness %q", a.cfg.Phase, a.cfg.InvestigateHarness)
	}
	return evaluatePosture(h)
}

// evaluatePosture asks h, before any request renders it, whether it can
// hold a run to harness.SandboxEvaluate: a harness that would render a
// weaker posture is refused rather than run.
func evaluatePosture(h harness.Harness) error {
	if err := harness.CheckPosture(h, harness.SandboxEvaluate); err != nil {
		return fmt.Errorf("harness %q cannot run an evaluator-side stage: %w", h.ID(), err)
	}
	return nil
}

// readHandoff reads a role stage's handoff, at most limit bytes: a larger
// one is an error, as is an empty one.
func (a *Agent) readHandoff(limit int) ([]byte, error) {
	f, err := os.Open(a.cfg.issuePath())
	if err != nil {
		return nil, fmt.Errorf("input handoff: %w", err)
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil {
		return nil, fmt.Errorf("input handoff: %w", err)
	}
	if len(raw) > limit {
		return nil, fmt.Errorf("input handoff: %s is over %d bytes", a.cfg.issuePath(), limit)
	}
	return raw, nil
}

// decodeHandoff decodes a role stage's JSON handoff into v strictly:
// unknown fields and trailing data are refused.
func decodeHandoff(raw []byte, what string, v any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return fmt.Errorf("input handoff: the %s handoff is empty", what)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("input handoff: the %s handoff is not valid: %w", what, err)
	}
	if dec.More() {
		return fmt.Errorf("input handoff: the %s handoff is not valid: trailing data", what)
	}
	return nil
}

// evaluateInput reads and checks the evaluate stage's handoff, and the
// tool pod it reaches (MAUL_TOOLS_URL): either missing or malformed is a
// contradiction in the controller, refused before any run.
func (a *Agent) evaluateInput() (evaluateInput, error) {
	raw, err := a.readHandoff(evaluateHandoffMaxBytes)
	if err != nil {
		return evaluateInput{}, err
	}
	var h EvaluateHandoff
	if err := decodeHandoff(raw, "evaluate", &h); err != nil {
		return evaluateInput{}, err
	}
	if err := validEvaluateHandoff(h); err != nil {
		return evaluateInput{}, fmt.Errorf("input handoff: %w", err)
	}
	tools, err := maultool.NewClient(a.cfg.ToolsURL)
	if err != nil {
		return evaluateInput{}, fmt.Errorf("the evaluator's tool pod: %w", err)
	}
	return evaluateInput{handoff: h, tools: tools}, nil
}

// validEvaluateHandoff holds a handoff to what the prompt needs: a round,
// at least one valid outcome, and a tool-call bound. The run token's
// grammar is the prompt's own (RenderEvaluatePrompt), checked here first so
// a bad one is refused before the run.
func validEvaluateHandoff(h EvaluateHandoff) error {
	var errs []error
	if h.Round < 1 {
		errs = append(errs, fmt.Errorf("round %d is not 1 or more", h.Round))
	}
	if len(h.Outcomes) == 0 {
		errs = append(errs, errors.New("the evaluate handoff has no outcomes"))
	} else if err := acceptance.ValidateOutcomes(h.Outcomes); err != nil {
		errs = append(errs, err)
	}
	if h.Tools.MaxToolCalls < 1 {
		errs = append(errs, fmt.Errorf("max_tool_calls %d is not 1 or more", h.Tools.MaxToolCalls))
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	if _, err := templates.RenderEvaluatePrompt(h.prompt("", "", "", templates.StageLimits{})); err != nil {
		return err
	}
	return nil
}

// prompt is the handoff as the evaluate prompt's data, with maul's own
// paths and the run's limits.
func (h EvaluateHandoff) prompt(reportPath, checksDir, shotsDir string,
	limits templates.StageLimits) templates.EvaluatePrompt {
	p := templates.EvaluatePrompt{
		ReportPath: reportPath, ChecksDir: checksDir, ShotsDir: shotsDir,
		Limits: limits, Round: h.Round, RunToken: h.RunToken,
		Tools: templates.EvaluateTools{
			Browser: h.Tools.Browser, Sandbox: h.Tools.Sandbox,
			MaxToolCalls: h.Tools.MaxToolCalls, LookAroundRoutes: h.Tools.LookAroundRoutes,
		},
		Request: h.Request, Outcomes: h.Outcomes, FixDiff: h.FixDiff, PriorRound: h.PriorRound,
	}
	for _, v := range h.PriorVerdicts {
		p.PriorVerdicts = append(p.PriorVerdicts, templates.PriorVerdict{Outcome: v.Outcome, Result: v.Result,
			Reason: v.Reason})
	}
	for _, r := range h.PriorReplay {
		p.PriorReplay = append(p.PriorReplay, templates.PriorReplay{Check: r.Check, Outcome: r.Outcome,
			Result: r.Result})
	}
	for _, c := range h.Components {
		p.Components = append(p.Components, templates.EvaluateComponent{Key: c.Key, Path: c.Path})
	}
	for _, c := range h.ChangedPaths {
		p.ChangedPaths = append(p.ChangedPaths, templates.ChangedPath{Path: c.Path, Added: c.Added, Deleted: c.Deleted})
	}
	return p
}

// evaluate runs the evaluate stage, then closes the round's tool pod: it
// asks the tools container to confirm the checks the accepted report
// proposes, then to finish, naming the screenshots the report cites as
// evidence (closeTools). Finish is sent whatever the stage's outcome, so the
// tool pod's ledger completes as soon as the evaluator is done with it.
func (a *Agent) evaluate(ctx context.Context, in evaluateInput) *envelope.Evaluation {
	ev, accepted := a.explore(ctx, in.handoff)
	ev.Cited = a.closeTools(ctx, in.tools, accepted)
	return ev
}

// explore runs the evaluate stage under the evaluate posture, in the
// scratch directory, and folds the result into the event payload: the
// report exactly as written and its parsed frontmatter, every value of it
// the model's claim (agentresult.FromEvaluation re-derives them from the
// report). It returns the accepted report, nil when the stage did not end
// OK.
func (a *Agent) explore(ctx context.Context, in EvaluateHandoff) (*envelope.Evaluation, *evaluationReport) {
	ev := &envelope.Evaluation{Stage: envelope.Stage{
		Harness: a.cfg.InvestigateHarness,
		Model:   a.cfg.InvestigateModel,
	}}
	fail := func(outcome envelope.Outcome, detail string) (*envelope.Evaluation, *evaluationReport) {
		ev.Outcome, ev.Detail = outcome, detail
		return ev, nil
	}
	h, ok := harness.ByID(a.cfg.InvestigateHarness)
	if !ok {
		return fail(envelope.OutcomeRuntimeError, fmt.Sprintf("unknown harness %q", a.cfg.InvestigateHarness))
	}
	maxTurns, budget, err := a.stageLimits()
	if err != nil {
		return fail(envelope.OutcomeRuntimeError, err.Error())
	}
	env, gateway, err := a.brokerEnv(h)
	if err != nil {
		return fail(envelope.OutcomeRuntimeError, err.Error())
	}
	prompt, err := templates.RenderEvaluatePrompt(in.prompt(a.cfg.evaluationPath(), a.cfg.checksDir(),
		a.cfg.shotsDir(), templates.StageLimits{MaxTurns: maxTurns, TokenBudget: budget,
			Timeout: a.cfg.InvestigateTimeout}))
	if err != nil {
		return fail(envelope.OutcomeRuntimeError, err.Error())
	}
	sr := a.newStage(h, "", harness.PromptRequest{
		Prompt:    prompt,
		Model:     a.cliModel(a.cfg.InvestigateModel, a.cfg.InvestigateHarness),
		MaxTurns:  maxTurns,
		Sandbox:   harness.SandboxEvaluate,
		SessionID: a.newSessionID(),
		Env:       env,
		Gateway:   gateway,
	}, a.cfg.InvestigateTimeout, a.cfg.InvestigateIdleTimeout, budget)
	res, idle, runErr := a.start(ctx, sr)
	a.fillStage(&ev.Stage, h, res)
	if outcome, detail := a.runOutcome(h, res, idle, runErr); outcome != envelope.OutcomeOK {
		return fail(outcome, detail)
	}

	e, raw, outcome, detail := settleReport(ctx, a, sr, &ev.Stage, a.cfg.evaluationPath(), readReport,
		a.parseEvaluation)
	if outcome != envelope.OutcomeOK {
		return fail(outcome, detail)
	}
	ev.Outcome = envelope.OutcomeOK
	ev.ReportMarkdown = string(raw)
	fillEvaluation(ev, e.Evaluation)
	return ev, e
}

// runOutcome is how a role stage's run ended: the idle watchdog, the budget
// kill switch, or the harness's own verdict (stageOutcome).
func (a *Agent) runOutcome(h harness.Harness, res runner.Result, idle string, runErr error) (envelope.Outcome, string) {
	if idle != "" {
		return envelope.OutcomeTimeout, idle
	}
	if res.Aborted {
		return envelope.OutcomeBudgetExceeded, res.AbortReason
	}
	return stageOutcome(h, res, runErr, credentialValues(h, a.scrub...))
}

// fillEvaluation copies a parsed report's claims onto the event.
func fillEvaluation(ev *envelope.Evaluation, e *report.Evaluation) {
	for _, v := range e.Verdicts {
		ev.Verdicts = append(ev.Verdicts, envelope.OutcomeVerdict{
			ID: v.ID, Verdict: string(v.Verdict), Ledger: v.Ledger, Note: v.Note,
		})
	}
	for _, d := range e.Disputes {
		ev.Disputes = append(ev.Disputes, envelope.Dispute{
			CheckDigest: d.CheckDigest, Reason: d.Reason, Replacement: d.Replacement, Ledger: d.Ledger,
		})
	}
	for _, p := range e.Proposed {
		ev.Proposed = append(ev.Proposed, envelope.ProposedCheck{ID: p.ID, Outcome: p.Outcome, Digest: p.Digest})
	}
	for _, r := range e.Retire {
		ev.Retire = append(ev.Retire, envelope.Retire{CheckDigest: r.CheckDigest, Reason: r.Reason})
	}
	for _, an := range e.Anomalies {
		ev.Anomalies = append(ev.Anomalies, envelope.Anomaly{Summary: an.Summary, Route: an.Route, Ledger: an.Ledger})
	}
}
