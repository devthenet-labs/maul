// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/acceptance/crdconv"
	"github.com/devthenet-labs/maul/internal/checks"
	"github.com/devthenet-labs/maul/internal/report"
	"github.com/devthenet-labs/maul/internal/toolwire"
	"github.com/devthenet-labs/maul/internal/untrusted"
)

// Judging an evaluation round (DESIGN 2.2 step 11, 4.3; SPEC S1-20). Once a
// round is collected and its new checks reviewed and stored
// (checkreview_round.go), acceptance.Decide judges it: the facts are the
// tools ledger's (pv's replays and confirmations) and the store's, the
// claims the evaluator's report. The decision is routed with the kept
// machinery:
//
//   - every outcome confirmed (or accepted by an approver at the heads), no
//     anomaly, a clean environment: Passed, and the gated hand-off says
//     "ready";
//   - a failure the evaluator agrees with: Failing, and startVerifyFix starts
//     a verify-fix round on Decision.Fix under the kept bounds
//     (maxVerifyFixes, RepeatedFailure on Decide's fingerprint, D3). A
//     failure at heads an acceptance-fix round already found no defect in is
//     a disagreement: Exhausted;
//   - a disputed failing check whose replacement is confirmed and reviewed
//     proves: the disputed check is retired from the store (retireChecks,
//     its own pass, before the judgement is written) and one model-free
//     replay round at the same heads is due (status.verification.disputed);
//   - anything else: Unconfirmed, and the hand-off says "maul couldn't
//     confirm this".
//
// The judgement is a function of the round alone: the checks it judges are
// the ones the round replayed (pv.spec.checks) and the ones it confirmed and
// the store kept, so retiring a check after the round changes nothing about
// how that round is judged, and startVerifyFix and the fix round's input
// decide exactly what the judgement did.

// Bounds of what a judgement records from the evaluator (the schema's).
const (
	maxAnomalies         = 10
	maxAnomalySummary    = 600
	maxAnomalyRoute      = 300
	maxAnomalyCites      = 32
	maxCheckLineBytes    = 1000
	maxCheckLineSteps    = 12
	maxEvaluationReports = 256 << 10
)

// roundJudgement is acceptance.Decide's answer on one round, with what it
// was judged on.
type roundJudgement struct {
	decision acceptance.Decision
	// checks are the saved checks the round was judged on; stored the
	// store as it was read (nil when it could not be).
	checks, stored []v1alpha1.SavedCheck
	// evaluation is the evaluator's report; nil on a replay round, or one
	// that does not parse.
	evaluation *report.Evaluation
	anomalies  []v1alpha1.AnomalyRecord
	env        []acceptance.PartFact
	// replay: the round was a replay round.
	replay bool
}

// decideRound judges pv, a collected round, with run its evaluate run (nil
// on a replay round).
func (p *pass) decideRound(ctx context.Context, pv *v1alpha1.PreviewVerification,
	run *v1alpha1.IntentRun) roundJudgement {
	stored, _, err := p.storedChecks(ctx)
	if err != nil {
		p.r.log().LogAttrs(ctx, slog.LevelWarn, "read the intent's check store; the round is judged on its own checks",
			slog.String("intent", p.in.Name), slog.String("verification", pv.Name), slog.Any("error", err))
		stored = nil
	}
	ev := evaluationClaims(run)
	cs := judgedChecks(pv, stored)
	in := decideInput(p.in.Status.Acceptance, pv, cs, ev)
	return roundJudgement{decision: acceptance.Decide(in), checks: cs, stored: stored, evaluation: ev,
		anomalies: anomalyRecords(ev, pv), env: in.Env, replay: pv.Spec.Rung == v1alpha1.RungReplay}
}

// evaluationClaims is run's evaluator report, nil for none (a replay round)
// or one that does not parse: an unreadable report claims nothing.
func evaluationClaims(run *v1alpha1.IntentRun) *report.Evaluation {
	if run == nil || run.Status.Phase != v1alpha1.RunComplete || run.Status.Report == "" ||
		len(run.Status.Report) > maxEvaluationReports {
		return nil
	}
	ev, err := report.ParseEvaluation([]byte(run.Status.Report))
	if err != nil {
		return nil
	}
	return ev
}

// judgedChecks are the saved checks a round is judged on: those it replayed
// (pv's copy of the store when it was created), then those it confirmed
// that the store kept (reviewed proves), each once, by digest.
func judgedChecks(pv *v1alpha1.PreviewVerification, stored []v1alpha1.SavedCheck) []v1alpha1.SavedCheck {
	out := slices.Clone(pv.Spec.Checks)
	has := func(d string) bool {
		return slices.ContainsFunc(out, func(c v1alpha1.SavedCheck) bool { return c.Digest == d })
	}
	for _, c := range stored {
		confirmed := slices.ContainsFunc(pv.Status.Confirmed, func(x v1alpha1.SavedCheck) bool {
			return x.Digest == c.Digest
		})
		if confirmed && !has(c.Digest) {
			out = append(out, c)
		}
	}
	return out
}

// decideInput is Decide's input for a round: the approved outcomes (each
// with the digest its reviews are keyed by), the round's checks (valid: only
// valid checks are ever stored), the tools ledger's runs, the cached
// reviews, and the evaluator's claims with the ledger entries it cites that
// the round's ledger holds. Base runs: none in S1.
func decideInput(acc *v1alpha1.IntentAcceptance, pv *v1alpha1.PreviewVerification, cs []v1alpha1.SavedCheck,
	ev *report.Evaluation) acceptance.Input {
	var in acceptance.Input
	for _, o := range acc.Outcomes {
		in.Outcomes = append(in.Outcomes, acceptance.OutcomeRef{ID: o.ID, Digest: outcomeDigest(o),
			Keep: o.EffectiveKind() == v1alpha1.OutcomeKindKeep, Manual: o.Manual})
	}
	for _, c := range cs {
		in.Checks = append(in.Checks, acceptance.StoredCheck{Digest: c.Digest, Outcome: c.Outcome,
			Form: acceptance.CheckForm(c.Form), Valid: true})
	}
	in.Replay, in.Confirmations = checkRuns(pv.Status.Replay), checkRuns(pv.Status.Confirmations)
	for _, r := range acc.Reviews {
		in.Reviews = append(in.Reviews, acceptance.CheckReview{CheckDigest: r.CheckDigest,
			OutcomeDigest: r.OutcomeDigest, Answer: acceptance.ReviewAnswer(r.Answer)})
	}
	in.Env = crdconv.PartFacts(pv.Status.Environment)
	if ev == nil {
		return in
	}
	for _, v := range ev.Verdicts {
		in.Verdicts = append(in.Verdicts, acceptance.EvaluatorVerdict{Outcome: v.ID, Claim: acceptance.Claim(v.Verdict),
			Ledger: citedOnly(v.Ledger, pv)})
	}
	for _, d := range resolvedDisputes(ev) {
		in.Disputes = append(in.Disputes, acceptance.CheckDispute{CheckDigest: d.CheckDigest, Reason: d.Reason,
			Replacement: d.Replacement, Ledger: citedOnly(d.Ledger, pv)})
	}
	for _, a := range ev.Anomalies {
		in.Anomalies = append(in.Anomalies, acceptance.AnomalyRef{Summary: a.Summary, Route: a.Route,
			Ledger: citedOnly(a.Ledger, pv)})
	}
	return in
}

// resolvedDisputes are ev's disputes with each replacement, which the
// report names by its proposed check's id, resolved to that proposal's
// digest (the store, the reviews and Decide key checks by digest); a
// replacement naming no proposal resolves to none.
func resolvedDisputes(ev *report.Evaluation) []report.Dispute {
	out := make([]report.Dispute, 0, len(ev.Disputes))
	for _, d := range ev.Disputes {
		id := d.Replacement
		d.Replacement = ""
		if i := slices.IndexFunc(ev.Proposed, func(pc report.ProposedCheck) bool { return pc.ID == id }); id != "" &&
			i >= 0 {
			d.Replacement = ev.Proposed[i].Digest
		}
		out = append(out, d)
	}
	return out
}

// checkRuns are a verification's check runs as Decide reads them.
func checkRuns(rs []v1alpha1.CheckRun) []acceptance.CheckRun {
	out := make([]acceptance.CheckRun, 0, len(rs))
	for _, r := range rs {
		out = append(out, acceptance.CheckRun{CheckDigest: r.CheckDigest, Result: acceptance.RunResult(r.Result),
			Token: r.Token, Seq: r.Seq})
	}
	return out
}

// citedOnly are the seqs of cited that pv's verified ledger holds: a claim
// citing an entry the ledger does not have cites nothing.
func citedOnly(cited []int32, pv *v1alpha1.PreviewVerification) []int32 {
	var out []int32
	for _, s := range cited {
		if pv.Status.Ledger != nil && s >= 1 && s <= pv.Status.Ledger.Count && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// anomalyRecords are the evaluator's look-around anomalies as the Intent
// records them: those citing an entry pv's ledger holds (an anomaly needs
// evidence), bounded to the schema's.
func anomalyRecords(ev *report.Evaluation, pv *v1alpha1.PreviewVerification) []v1alpha1.AnomalyRecord {
	if ev == nil {
		return nil
	}
	var out []v1alpha1.AnomalyRecord
	for _, a := range ev.Anomalies {
		cites := citedOnly(a.Ledger, pv)
		summary := strings.TrimSpace(untrusted.Cut(a.Summary, maxAnomalySummary))
		if len(cites) == 0 || summary == "" || len(out) == maxAnomalies {
			continue
		}
		if len(cites) > maxAnomalyCites {
			cites = cites[:maxAnomalyCites]
		}
		out = append(out, v1alpha1.AnomalyRecord{Summary: summary, Route: untrusted.Cut(a.Route, maxAnomalyRoute),
			Ledger: cites})
	}
	return out
}

// outcomeAccepted reports an approver's /maul accept of outcome id at the
// head set.
func outcomeAccepted(acc *v1alpha1.IntentAcceptance, id, headSet string) bool {
	return acceptedAt(acc, id, headSet) != nil
}

// outcomeResults are the round's results as the Intent records them, in
// plan order: Decide's, each over its basis check's line, and an outcome an
// approver accepted at the heads (one Decide did not pass) accepted.
func outcomeResults(acc *v1alpha1.IntentAcceptance, headSet string, j roundJudgement) []v1alpha1.OutcomeResult {
	out := make([]v1alpha1.OutcomeResult, 0, len(j.decision.PerOutcome))
	for _, r := range j.decision.PerOutcome {
		res := v1alpha1.OutcomeResult{ID: r.ID, Result: v1alpha1.OutcomeResultValue(r.Result), Basis: r.Basis,
			Reason: r.Reason}
		if i := slices.IndexFunc(j.checks, func(c v1alpha1.SavedCheck) bool { return c.Digest == r.Basis }); i >= 0 {
			res.Line = checkLine(j.checks[i])
		}
		if r.Result != acceptance.ResultPassed && outcomeAccepted(acc, r.ID, headSet) {
			res.Result = v1alpha1.OutcomeAccepted
		}
		out = append(out, res)
	}
	return out
}

// pendingFix is j's fix request less the failures of outcomes an approver
// accepted at the heads (the human closed them), its fingerprint over what
// is left; nil when none is.
func pendingFix(acc *v1alpha1.IntentAcceptance, headSet string, j roundJudgement) *acceptance.FixRequest {
	fix := j.decision.Fix
	if fix == nil {
		return nil
	}
	var left []acceptance.FixFailure
	for _, f := range fix.Failures {
		if !outcomeAccepted(acc, f.Outcome, headSet) {
			left = append(left, f)
		}
	}
	if len(left) == len(fix.Failures) {
		return fix
	}
	if len(left) == 0 {
		return nil
	}
	outcomes, digests := make([]string, 0, len(left)), make([]string, 0, len(left))
	for _, f := range left {
		outcomes, digests = append(outcomes, f.Outcome), append(digests, f.CheckDigest)
	}
	return &acceptance.FixRequest{Failures: left, Fingerprint: acceptance.EvaluationFingerprint(outcomes, digests,
		j.env)}
}

// confirmedAll reports results every one of which passed or an approver
// accepted.
func confirmedAll(rs []v1alpha1.OutcomeResult) bool {
	for _, r := range rs {
		if r.Result != v1alpha1.OutcomePassed && r.Result != v1alpha1.OutcomeAccepted {
			return false
		}
	}
	return len(rs) > 0
}

// judgedState is the state a judgement routes to (the list above), and
// whether a replay round follows it.
func judgedState(v *v1alpha1.IntentVerification, j roundJudgement, results []v1alpha1.OutcomeResult,
	fix *acceptance.FixRequest) (v1alpha1.IntentVerificationState, bool) {
	replayNext := j.decision.ReplayRound && !j.replay
	switch {
	case fix != nil && v.NoDefect != nil && v.NoDefect.HeadSetDigest == v.HeadSetDigest:
		// A fix round concluded no defect at these heads, and the round
		// still fails: maul and its fixer disagree, and no fix is left.
		return v1alpha1.VerificationExhausted, false
	case fix != nil:
		return v1alpha1.VerificationFailing, false
	case !replayNext && len(j.anomalies) == 0 && j.decision.Env == acceptance.EnvOK && confirmedAll(results):
		return v1alpha1.VerificationPassed, false
	}
	return v1alpha1.VerificationUnconfirmed, replayNext
}

// applyJudgement writes j onto cur's verification: the results, the counts,
// the anomalies, the disputed checks a replay round awaits, and the state.
func applyJudgement(cur *v1alpha1.Intent, j roundJudgement) {
	nv, acc := cur.Status.Verification, cur.Status.Acceptance
	clearJudgement(nv)
	nv.Outcomes = outcomeResults(acc, nv.HeadSetDigest, j)
	for _, r := range nv.Outcomes {
		switch r.Result {
		case v1alpha1.OutcomePassed:
			nv.Passed++
		case v1alpha1.OutcomeFailed:
			nv.Failed++
		case v1alpha1.OutcomeQuarantined:
			nv.Quarantined++
		case v1alpha1.OutcomeUnsure, v1alpha1.OutcomeAccepted:
		}
	}
	nv.Anomalies = j.anomalies
	state, replayNext := judgedState(nv, j, nv.Outcomes, pendingFix(acc, nv.HeadSetDigest, j))
	nv.State, nv.Disputed = state, nil
	if replayNext {
		nv.Disputed = slices.Clone(j.decision.Retire)
	}
	countManual(nv, acc)
}

// judgeEvaluation settles a collected round on its judgement (Decide), its
// claims run's report (nil on a replay round, which has no evaluator).
func (p *pass) judgeEvaluation(ctx context.Context, pv *v1alpha1.PreviewVerification, run *v1alpha1.IntentRun) error {
	j := p.decideRound(ctx, pv, run)
	return p.writeJudgement(ctx, pv, run, j)
}

// writeJudgement writes j, pv's judgement, onto the Intent.
func (p *pass) writeJudgement(ctx context.Context, pv *v1alpha1.PreviewVerification, run *v1alpha1.IntentRun,
	j roundJudgement) error {
	var claims string
	if run != nil {
		claims = run.Name
	}
	p.r.log().LogAttrs(ctx, slog.LevelInfo, "evaluation round judged",
		slog.String("intent", p.in.Name), slog.String("verification", pv.Name), slog.String("run", claims),
		slog.Int("ledger_entries", ledgerCount(pv)), slog.String("handoff", string(j.decision.Handoff)),
		slog.Bool("fix", j.decision.Fix != nil), slog.Bool("replay", j.decision.ReplayRound),
		slog.String("environment", string(j.decision.Env)))
	return p.update(ctx, func(cur *v1alpha1.Intent) error {
		applyJudgement(cur, j)
		p.releaseIfDue(cur)
		return nil
	})
}

// retireDisputed retires, from the store, the disputed checks j's decision
// retires that the store still holds (retireChecks: only with a stored,
// proven replacement), before the round is judged. It reports whether it
// wrote. A store that could not be read retires nothing, and the round is
// judged all the same; a failed write is returned, and tried again.
func (p *pass) retireDisputed(ctx context.Context, j roundJudgement) (bool, error) {
	if j.evaluation == nil || j.stored == nil {
		return false, nil
	}
	var still []string
	for _, d := range j.decision.Retire {
		if slices.ContainsFunc(j.stored, func(c v1alpha1.SavedCheck) bool { return c.Digest == d }) {
			still = append(still, d)
		}
	}
	if len(still) == 0 {
		return false, nil
	}
	wrote, err := p.retireChecks(ctx, still, resolvedDisputes(j.evaluation))
	if err != nil {
		return false, fmt.Errorf("retire the disputed checks: %w", err)
	}
	return wrote, nil
}

// evaluationFailure is the failure an evaluation round's judgement asks a
// verify-fix round for (Decision.Fix less what approvers accepted): the
// repositories to fix, in order (each failing http check's target, then
// every head's component), its fingerprint, and a piece per failure, each
// a fingerprint of its outcome and check (never a check's digest: nothing
// of the store reaches a revise round). ok is false when it asks for none.
func (p *pass) evaluationFailure(ctx context.Context, pv *v1alpha1.PreviewVerification) (verifyFailure,
	*acceptance.FixRequest, roundJudgement, bool) {
	j := p.decideRound(ctx, pv, p.evaluateRun(pv))
	fix := pendingFix(p.in.Status.Acceptance, pv.Spec.HeadSetDigest, j)
	if fix == nil || j.decision.Env != acceptance.EnvOK {
		return verifyFailure{}, nil, j, false
	}
	f := verifyFailure{signature: fix.Fingerprint}
	for _, ff := range fix.Failures {
		f.pieces = append(f.pieces, "o:"+ff.Outcome+" "+
			acceptance.EvaluationFingerprint([]string{ff.Outcome}, []string{ff.CheckDigest}, nil))
		if i := slices.IndexFunc(j.checks, func(c v1alpha1.SavedCheck) bool { return c.Digest == ff.CheckDigest }); i >= 0 {
			if t := httpTarget(j.checks[i]); t != "" && !slices.Contains(f.targets, t) {
				f.targets = append(f.targets, t)
			}
		}
	}
	for _, h := range pv.Spec.Heads {
		if !slices.Contains(f.targets, h.Component) {
			f.targets = append(f.targets, h.Component)
		}
	}
	return f, fix, j, true
}

// httpTarget is an http check's component, "" for another form.
func httpTarget(c v1alpha1.SavedCheck) string {
	parsed, err := checks.Parse([]byte(c.Body))
	if err != nil || parsed.HTTP == nil {
		return ""
	}
	return parsed.HTTP.Target
}

// checkLine is maul's one-line description of a saved check, from its
// structured body: its form, what it asks of the preview, and how much it
// asserts. "" for a body that does not parse. The paths and names in it are
// the evaluator's, shown by the templates as inert text.
func checkLine(c v1alpha1.SavedCheck) string {
	parsed, err := checks.Parse([]byte(c.Body))
	if err != nil {
		return ""
	}
	var line string
	switch {
	case parsed.HTTP != nil:
		line = httpLine(parsed.HTTP)
	case parsed.Browser != nil:
		line = browserLine(parsed.Browser)
	case parsed.Exec != nil:
		line = execLine(parsed.Exec)
	default:
		return ""
	}
	line = strings.Join(strings.Fields(untrusted.Escape(line)), " ")
	return untrusted.Cut(line, maxCheckLineBytes)
}

// stepsLine joins a check's step descriptions, at most maxCheckLineSteps.
func stepsLine(steps []string) string {
	if len(steps) > maxCheckLineSteps {
		steps = append(steps[:maxCheckLineSteps:maxCheckLineSteps], fmt.Sprintf("%d more",
			len(steps)-maxCheckLineSteps))
	}
	return strings.Join(steps, ", ")
}

// httpLine: "http to <component>: GET /a, POST /b; <n> expectations".
func httpLine(h *checks.HTTPCheck) string {
	steps, expects := make([]string, 0, len(h.Steps)), 0
	for _, s := range h.Steps {
		method := s.Request.Method
		if method == "" {
			method = "GET"
		}
		steps = append(steps, method+" "+s.Request.Path)
		expects += len(s.Expect.Status) + len(s.Expect.JSON) + len(s.Expect.Headers)
		if s.Expect.BodyContains != "" {
			expects++
		}
	}
	return fmt.Sprintf("http to %s: %s; %d %s", h.Target, stepsLine(steps), expects,
		pluralWord(expects, "expectation", "expectations"))
}

// browserLine: "browser: goto /a, click, expect visible".
func browserLine(b *checks.BrowserCheck) string {
	steps := make([]string, 0, len(b.Steps))
	for _, s := range b.Steps {
		switch {
		case s.Goto != "":
			steps = append(steps, "goto "+s.Goto)
		case s.WaitForURL != "":
			steps = append(steps, "wait for "+s.WaitForURL)
		case s.Click != nil:
			steps = append(steps, "click")
		case s.Fill != nil:
			steps = append(steps, "fill")
		case s.Select != nil:
			steps = append(steps, "select")
		case s.Press != nil:
			steps = append(steps, "press")
		case s.Scroll != nil:
			steps = append(steps, "scroll")
		case s.Expect != nil:
			steps = append(steps, "expect "+string(s.Expect.Assert))
		}
	}
	return "browser: " + stepsLine(steps)
}

// execLine: "exec: <program>; expects exit 0, stdout".
func execLine(e *checks.ExecCheck) string {
	program := ""
	if len(e.Argv) > 0 {
		program = e.Argv[0]
	}
	var expects []string
	if e.Expect.Exit != nil {
		expects = append(expects, fmt.Sprintf("exit %d", *e.Expect.Exit))
	}
	if e.Expect.StdoutContains != "" || len(e.Expect.StdoutJSON) > 0 {
		expects = append(expects, "stdout")
	}
	if len(expects) == 0 {
		return "exec: " + program
	}
	return "exec: " + program + "; expects " + strings.Join(expects, ", ")
}

// pluralWord is one when n is 1, else many.
func pluralWord(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// outcomesReady reports a judged evaluation every outcome of which is now
// confirmed or accepted by an approver at its heads, with no anomaly and no
// replay round pending: what "ready" means once approvers accepted what
// maul could not confirm (/maul accept, after the judgement).
func outcomesReady(acc *v1alpha1.IntentAcceptance, v *v1alpha1.IntentVerification) bool {
	switch v.State {
	case v1alpha1.VerificationPassed:
		return true
	case v1alpha1.VerificationUnconfirmed, v1alpha1.VerificationFailing, v1alpha1.VerificationExhausted:
	default:
		return false
	}
	if len(v.Anomalies) > 0 || len(v.Disputed) > 0 || len(v.Outcomes) == 0 {
		return false
	}
	for _, r := range v.Outcomes {
		if r.Result != v1alpha1.OutcomePassed && !outcomeAccepted(acc, r.ID, v.HeadSetDigest) {
			return false
		}
	}
	return true
}

// effectiveResult is an outcome's result as reported now: accepted once an
// approver accepted it at the heads (and maul did not confirm it).
func effectiveResult(acc *v1alpha1.IntentAcceptance, v *v1alpha1.IntentVerification,
	r v1alpha1.OutcomeResult) v1alpha1.OutcomeResultValue {
	if r.Result != v1alpha1.OutcomePassed && outcomeAccepted(acc, r.ID, v.HeadSetDigest) {
		return v1alpha1.OutcomeAccepted
	}
	return r.Result
}

// confirmedCount counts v's outcomes confirmed or accepted now.
func confirmedCount(acc *v1alpha1.IntentAcceptance, v *v1alpha1.IntentVerification) int32 {
	var n int32
	for _, r := range v.Outcomes {
		if res := effectiveResult(acc, v, r); res == v1alpha1.OutcomePassed || res == v1alpha1.OutcomeAccepted {
			n++
		}
	}
	return n
}

// keyLedgerLines is the data key of a verification's full ledger
// (<pv>-ledger, preview-controller's): one JSON line per entry.
const keyLedgerLines = "ledger.jsonl"

// maxAppOutputBytes bounds the application output a failure shows.
const maxAppOutputBytes = 2 << 10

// appOutput is what the application gave the failing run of check digest in
// pv: the failing steps' runner messages from the run's own ledger entry,
// read from the verification's full ledger (owned by pv); "" when there is
// no failing run, or its entry or ledger cannot be read. It is the
// application's text, untrusted: the templates show it only fenced,
// sanitised and defanged.
func (p *pass) appOutput(ctx context.Context, pv *v1alpha1.PreviewVerification, digest string) string {
	seq := failingSeq(pv, digest)
	if seq == 0 {
		return ""
	}
	if run := checkRunAt(ledgerEntries(p.ledgerOf(ctx, pv)), seq, digest); run != nil {
		return failedSteps(run)
	}
	return ""
}

// failedSteps are a check run's failing steps' messages, attempt by
// attempt, bounded.
func failedSteps(r *toolwire.CheckRunRecord) string {
	var lines []string
	for i, a := range r.Attempts {
		for _, s := range a.Steps {
			if s.Result != toolwire.ResultPass && strings.TrimSpace(s.Detail) != "" {
				lines = append(lines, fmt.Sprintf("attempt %d, step %d: %s", i+1, s.Index+1, s.Detail))
			}
		}
	}
	return untrusted.Cut(strings.Join(lines, "\n"), maxAppOutputBytes)
}

// ledgerOf is pv's full ledger, one entry a line, read once a pass
// (uncached) from the ConfigMap its status names, held to pv: nil when it is
// not pv's or cannot be read.
func (p *pass) ledgerOf(ctx context.Context, pv *v1alpha1.PreviewVerification) []string {
	if p.ledgerRead {
		return p.ledgerLines
	}
	p.ledgerRead = true
	if pv.Status.Ledger == nil || pv.Status.Ledger.ConfigMap == "" {
		return nil
	}
	var cm corev1.ConfigMap
	if err := p.r.APIReader.Get(ctx, types.NamespacedName{Namespace: pv.Namespace, Name: pv.Status.Ledger.ConfigMap},
		&cm); err != nil {
		p.r.log().LogAttrs(ctx, slog.LevelWarn, "read the verification's ledger; shown without the app output",
			slog.String("intent", p.in.Name), slog.String("verification", pv.Name), slog.Any("error", err))
		return nil
	}
	if !controlledBy(cm.OwnerReferences, pv.UID) {
		return nil
	}
	p.ledgerLines = strings.Split(cm.Data[keyLedgerLines], "\n")
	return p.ledgerLines
}
