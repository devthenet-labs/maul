// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/acceptance/crdconv"
	"github.com/devthenet-labs/maul/internal/httpstep"
	"github.com/devthenet-labs/maul/internal/report"
	"github.com/devthenet-labs/maul/internal/toolledger"
	"github.com/devthenet-labs/maul/internal/untrusted"
)

// Judging an evaluation round ("evaluator observed"). Once a round is
// collected, acceptance.Judge judges it: the claims are the evaluator's
// per-outcome verdicts, each counted only when it cites an entry of the
// round's verified ledger that exercised the application
// (acceptance.ValidCite, checked against the index in the PV's status, never
// against the report); the facts are that index and the round's
// environment. maul checks the citation, never whether the cited call shows
// what the verdict says: that is the evaluator's reading, which every
// comment shows with the calls it cites, for the reviewers to judge. The
// decision is routed with the kept machinery:
//
//   - every outcome seen to hold (or accepted by an approver at the heads)
//     in a clean environment: Passed, and the gated hand-off says "ready";
//   - an outcome seen not to hold, citing a valid call: Failing, and
//     startVerifyFix starts a verify-fix round on Judgement.Fix under the
//     kept bounds (maxVerifyFixes, RepeatedFailure on the outcome-only
//     fingerprint, D3). A failure at heads an acceptance-fix round already
//     found no defect in is a disagreement: Exhausted;
//   - anything else: Unconfirmed, and the hand-off says why.
//
// The judgement is a function of the round alone, but for what earlier
// rounds found at the same heads (G-B-LIB-1): an outcome an earlier round
// saw not to hold there is held (heldOutcomes) until the heads move, an
// approver accepts it, or the fix round it was handed concludes no defect.
//
// The evaluator answers one question, whether the work is correct, and the
// approved outcomes alone answer it: a look-around anomaly (the evaluator's
// side finding) is a note for the human reviewer, recorded, carried through
// later rounds at the same heads (carriedAnomalies) and shown, but it never
// holds a round or downgrades the hand-off.

// Bounds of what a judgement records from the evaluator (the schema's).
const (
	maxAnomalies         = 10
	maxAnomalySummary    = 600
	maxAnomalyRoute      = 300
	maxAnomalyCites      = 32
	maxEvaluationReports = 256 << 10
)

// roundJudgement is acceptance.Judge's answer on one round, with what it
// was judged on.
type roundJudgement struct {
	// in is what the round was judged on, Held as computed then; judgement
	// is Judge's answer on it.
	in        acceptance.JudgeInput
	judgement acceptance.Judgement
	// evaluation is the evaluator's report; nil when it does not parse.
	evaluation *report.Evaluation
	anomalies  []v1alpha1.AnomalyRecord
	// verification names the round's PreviewVerification.
	verification string
}

// decideRound judges pv, a collected round, with run its evaluate run, held
// to what the Intent's verification found earlier at pv's heads.
func (p *pass) decideRound(_ context.Context, pv *v1alpha1.PreviewVerification,
	run *v1alpha1.IntentRun) roundJudgement {
	return judgeRound(p.in.Status.Verification, p.in.Status.Acceptance, pv, evaluationClaims(run))
}

// judgeRound judges pv, a collected round, on acc's approved outcomes, ev,
// its evaluator's claims (nil when its report does not parse: it claims
// nothing), and v, the verification on record, whose findings at pv's heads
// hold outcomes (heldOutcomes). Its anomalies are recorded as notes, and go
// to Judge as nothing.
func judgeRound(v *v1alpha1.IntentVerification, acc *v1alpha1.IntentAcceptance, pv *v1alpha1.PreviewVerification,
	ev *report.Evaluation) roundJudgement {
	in := judgeInput(acc, pv, ev)
	j := roundJudgement{in: in, evaluation: ev, anomalies: anomalyRecords(ev, in), verification: pv.Name}
	return j.heldTo(heldOutcomes(v, pv.Spec.HeadSetDigest, pv.Spec.Round))
}

// heldTo is j judged again with held the outcomes earlier findings hold.
// Judge is pure, so the same round judged with the same holds is judged
// alike.
func (j roundJudgement) heldTo(held []string) roundJudgement {
	j.in.Held = held
	j.judgement = acceptance.Judge(j.in)
	return j
}

// judgeInput is Judge's input for pv: the approved outcomes (each with the
// digest a fix is fingerprinted on), the evaluator's verdicts as written
// (Judge keeps the first per outcome and checks every cite), pv's verified
// ledger index and its environment. The index is in status only once the
// tools log was held to its summary (toolledger.Parse; a log that does not
// hold is Infra ledger-mismatch, and an over-cap status that loses it is
// Infra too), so its presence is what Verified means. One exception: an
// application failure (EnvFailed) whose ledger did not verify is judged on
// its environment alone, as an index of no entry (no cite can count), so
// its outcomes say the change failed (env-failed), never maul's machinery.
func judgeInput(acc *v1alpha1.IntentAcceptance, pv *v1alpha1.PreviewVerification,
	ev *report.Evaluation) acceptance.JudgeInput {
	in := acceptance.JudgeInput{Env: crdconv.PartFacts(pv.Status.Environment)}
	for _, o := range acc.Outcomes {
		in.Outcomes = append(in.Outcomes, acceptance.OutcomeRef{ID: o.ID, Digest: outcomeDigest(o),
			Keep: o.EffectiveKind() == v1alpha1.OutcomeKindKeep, Manual: o.Manual})
	}
	switch l := pv.Status.Ledger; {
	case l != nil:
		in.Verified, in.Count = true, l.Count
		in.Ledger = make([]acceptance.LedgerFact, 0, len(l.Entries))
		for _, e := range l.Entries {
			in.Ledger = append(in.Ledger, acceptance.LedgerFact{Seq: e.Seq, Kind: string(e.Kind), Verb: string(e.Verb),
				Side: string(e.Side), Exit: e.Exit, Refused: e.Refused})
		}
	case httpstep.ClassifyEnvironment(in.Env) == httpstep.EnvAppFailure:
		in.Verified = true
	}
	if ev == nil {
		return in
	}
	for _, v := range ev.Verdicts {
		in.Verdicts = append(in.Verdicts, acceptance.Verdict{Outcome: v.ID, Claim: acceptance.Claim(v.Verdict),
			Cites: slices.Clone(v.Ledger), Note: v.Note})
	}
	return in
}

// anomalyRecords are the evaluator's look-around anomalies as the Intent
// records them: those citing an entry the round's verified ledger holds that
// acceptance.ValidCite admits (an anomaly needs evidence, by the verdicts'
// own rule), bounded to the schema's.
func anomalyRecords(ev *report.Evaluation, in acceptance.JudgeInput) []v1alpha1.AnomalyRecord {
	if ev == nil {
		return nil
	}
	var out []v1alpha1.AnomalyRecord
	for _, a := range ev.Anomalies {
		var cites []int32
		for _, s := range a.Ledger {
			if acceptance.ValidCite(in, s) && !slices.Contains(cites, s) && len(cites) < maxAnomalyCites {
				cites = append(cites, s)
			}
		}
		summary := strings.TrimSpace(untrusted.Cut(a.Summary, maxAnomalySummary))
		if len(cites) == 0 || summary == "" || len(out) == maxAnomalies {
			continue
		}
		out = append(out, v1alpha1.AnomalyRecord{Summary: summary, Route: untrusted.Cut(a.Route, maxAnomalyRoute),
			Ledger: cites})
	}
	return out
}

// heldOutcomes are the outcomes v's latest judgement at headSet saw not to
// hold (acceptance.ReasonSeenNotToHold), when that judgement was of a round
// before round, and those it held already (ReasonHeldFinding)
// (G-B-LIB-1): a failure seen at heads is a finding about them, which a
// later round there does not erase. nil once the heads moved. A fix round
// at headSet that concluded no defect releases only what it was handed (a
// failure seen not to hold), and only while the judgement it was handed is
// the latest (NoDefectRecord.Round). An approver's acceptance is laid over
// the held result (outcomeResults), never read here.
func heldOutcomes(v *v1alpha1.IntentVerification, headSet string, round int32) []string {
	if v == nil || headSet == "" || v.OutcomesHeadSetDigest != headSet {
		return nil
	}
	// A no-defect conclusion speaks for the judgement its fix round was
	// handed alone: a later round there that saw the failure again made a
	// new finding, which it predates.
	noDefect := v.NoDefect != nil && v.NoDefect.HeadSetDigest == headSet && v.NoDefect.Round != 0 &&
		v.NoDefect.Round == v.OutcomesRound
	var out []string
	for _, r := range v.Outcomes {
		switch {
		case r.Reason == acceptance.ReasonSeenNotToHold && noDefect:
			// The fix round handed this failure at these heads concluded,
			// in its recorded reason, that the code has no defect: that is
			// the evidence a later round's pass there rests on.
		case r.Reason == acceptance.ReasonHeldFinding,
			r.Reason == acceptance.ReasonSeenNotToHold && v.OutcomesRound != round:
			out = append(out, r.ID)
		}
	}
	return out
}

// outcomeAccepted reports an approver's /maul accept of outcome id at the
// head set.
func outcomeAccepted(acc *v1alpha1.IntentAcceptance, id, headSet string) bool {
	return acceptedAt(acc, id, headSet) != nil
}

// outcomeResults are the round's results as the Intent records them, in
// plan order: Judge's, each with the valid calls its verdict cites; an
// outcome an earlier finding holds (j.in.Held) and this round did not see
// fail is unsure, reason ReasonHeldFinding, whatever else this round
// decided (Judge holds a pass itself; this keeps the hold on record, so a
// round left unsure does not free it for the next); and an outcome an
// approver accepted at the heads (one not passed) accepted.
func outcomeResults(acc *v1alpha1.IntentAcceptance, headSet string, j roundJudgement) []v1alpha1.OutcomeResult {
	out := make([]v1alpha1.OutcomeResult, 0, len(j.judgement.PerOutcome))
	for _, d := range j.judgement.PerOutcome {
		res := v1alpha1.OutcomeResult{ID: d.ID, Result: v1alpha1.OutcomeResultValue(d.Result), Reason: d.Reason,
			Cites: slices.Clone(d.Cites)}
		if d.Result != acceptance.ResultFailed && slices.Contains(j.in.Held, d.ID) {
			res.Result, res.Reason = v1alpha1.OutcomeUnsure, acceptance.ReasonHeldFinding
		}
		if res.Result != v1alpha1.OutcomePassed && outcomeAccepted(acc, d.ID, headSet) {
			res.Result = v1alpha1.OutcomeAccepted
		}
		out = append(out, res)
	}
	return out
}

// pendingFix is j's fix request less the failures of outcomes an approver
// accepted at the heads (the human closed them), its fingerprint over the
// outcomes left (acceptance.OutcomeFingerprint); nil when none is.
func pendingFix(acc *v1alpha1.IntentAcceptance, headSet string, j roundJudgement) *acceptance.FixRequest {
	fix := j.judgement.Fix
	if fix == nil {
		return nil
	}
	var left []acceptance.FixFailure
	var refs []acceptance.OutcomeRef
	for _, f := range fix.Failures {
		if outcomeAccepted(acc, f.Outcome, headSet) {
			continue
		}
		left = append(left, f)
		if ref, ok := j.outcomeRef(f.Outcome); ok {
			refs = append(refs, ref)
		}
	}
	switch len(left) {
	case len(fix.Failures):
		return fix
	case 0:
		return nil
	}
	return &acceptance.FixRequest{Failures: left, Fingerprint: acceptance.OutcomeFingerprint(refs)}
}

// outcomeRef is the approved outcome id as j was judged on it.
func (j roundJudgement) outcomeRef(id string) (acceptance.OutcomeRef, bool) {
	i := slices.IndexFunc(j.in.Outcomes, func(o acceptance.OutcomeRef) bool { return o.ID == id })
	if i < 0 {
		return acceptance.OutcomeRef{}, false
	}
	return j.in.Outcomes[i], true
}

// decision is Judge's decision on outcome id, ok false for none.
func (j roundJudgement) decision(id string) (acceptance.OutcomeDecision, bool) {
	i := slices.IndexFunc(j.judgement.PerOutcome, func(d acceptance.OutcomeDecision) bool { return d.ID == id })
	if i < 0 {
		return acceptance.OutcomeDecision{}, false
	}
	return j.judgement.PerOutcome[i], true
}

// note is the evaluator's note on outcome id, from the verdict Judge read
// (the first on it); "" for none.
func (j roundJudgement) note(id string) string {
	i := slices.IndexFunc(j.in.Verdicts, func(v acceptance.Verdict) bool { return v.Outcome == id })
	if i < 0 {
		return ""
	}
	return j.in.Verdicts[i].Note
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

// judgedState is the state a judgement routes to (the list above). Its
// anomalies are notes: they hold nothing.
func judgedState(v *v1alpha1.IntentVerification, j roundJudgement, results []v1alpha1.OutcomeResult,
	fix *acceptance.FixRequest) v1alpha1.IntentVerificationState {
	switch {
	case fix != nil && v.NoDefect != nil && v.NoDefect.HeadSetDigest == v.HeadSetDigest && !rearmedSince(v):
		// A fix round concluded no defect at these heads, and the round
		// still fails: maul's evaluator and its fixer disagree, and no fix
		// is left, unless an approver's /maul verify fix since then says to
		// fix it again (the human always wins).
		return v1alpha1.VerificationExhausted
	case fix != nil:
		return v1alpha1.VerificationFailing
	case j.judgement.Env.Clean() && confirmedAll(results):
		return v1alpha1.VerificationPassed
	}
	return v1alpha1.VerificationUnconfirmed
}

// applyJudgement writes j onto cur's verification: the results, the counts,
// the notes (those earlier rounds found at its heads, which it carries,
// then its own) and the state. An outcome an earlier round saw not to hold
// at its heads is held (heldOutcomes, G-B-LIB-1), read from cur as it is
// written, whatever its own evaluator reported: only moved heads, an
// approver's acceptance or a fix round's no-defect conclusion end it. The
// saved-check era's records (the suspected defect, a dispute's replay) are
// never written: a judgement clears them, as one that found none always
// did.
func applyJudgement(cur *v1alpha1.Intent, j roundJudgement) {
	nv, acc := cur.Status.Verification, cur.Status.Acceptance
	carried := carriedAnomalies(nv, nv.HeadSetDigest, nv.Round)
	j = j.heldTo(heldOutcomes(nv, nv.HeadSetDigest, nv.Round))
	clearJudgement(nv)
	nv.Outcomes = outcomeResults(acc, nv.HeadSetDigest, j)
	nv.OutcomesRound, nv.OutcomesHeadSetDigest = nv.Round, nv.HeadSetDigest
	nv.OutcomesEnvironment = string(j.judgement.Env)
	for _, r := range nv.Outcomes {
		switch r.Result {
		case v1alpha1.OutcomePassed:
			nv.Passed++
		case v1alpha1.OutcomeFailed:
			nv.Failed++
		case v1alpha1.OutcomeQuarantined, v1alpha1.OutcomeUnsure, v1alpha1.OutcomeAccepted:
		}
	}
	nv.Anomalies = carryAnomalies(carried, j.anomalies, nv.Round)
	nv.Suspected, nv.Disputed = nil, nil
	nv.State = judgedState(nv, j, nv.Outcomes, pendingFix(acc, nv.HeadSetDigest, j))
	countManual(nv, acc)
}

// judgeEvaluation settles a collected round on its judgement (Judge), its
// claims run's report.
func (p *pass) judgeEvaluation(ctx context.Context, pv *v1alpha1.PreviewVerification, run *v1alpha1.IntentRun) error {
	j := p.decideRound(ctx, pv, run)
	var claims string
	if run != nil {
		claims = run.Name
	}
	p.r.log().LogAttrs(ctx, slog.LevelInfo, "evaluation round judged",
		slog.String("intent", p.in.Name), slog.String("verification", pv.Name), slog.String("run", claims),
		slog.Int("ledger_entries", ledgerCount(pv)), slog.String("handoff", string(j.judgement.Handoff)),
		slog.Bool("fix", j.judgement.Fix != nil), slog.String("environment", string(j.judgement.Env)))
	return p.update(ctx, func(cur *v1alpha1.Intent) error {
		applyJudgement(cur, j)
		p.releaseIfDue(cur)
		return nil
	})
}

// evaluationFailure is the failure an evaluation round's judgement asks a
// verify-fix round for (Judgement.Fix less what approvers accepted): the
// repositories to fix, every head's component in the round's head order,
// its fingerprint (acceptance.OutcomeFingerprint of the failing outcomes),
// and a piece per failure, the fingerprint of its outcome alone
// (acceptance.OutcomeFingerprintPiece): nothing the evaluator chose, so a
// failure of the same outcomes again is a repeat however it was cited. ok
// is false when it asks for none.
func (p *pass) evaluationFailure(ctx context.Context, pv *v1alpha1.PreviewVerification) (verifyFailure,
	*acceptance.FixRequest, roundJudgement, bool) {
	j := p.decideRound(ctx, pv, p.evaluateRun(pv))
	fix := pendingFix(p.in.Status.Acceptance, pv.Spec.HeadSetDigest, j)
	if fix == nil || j.judgement.Env != httpstep.EnvOK {
		return verifyFailure{}, nil, j, false
	}
	f := verifyFailure{signature: fix.Fingerprint}
	for _, ff := range fix.Failures {
		if ref, ok := j.outcomeRef(ff.Outcome); ok {
			f.pieces = append(f.pieces, acceptance.OutcomeFingerprintPiece(ref))
		}
	}
	for _, h := range pv.Spec.Heads {
		if !slices.Contains(f.targets, h.Component) {
			f.targets = append(f.targets, h.Component)
		}
	}
	return f, fix, j, true
}

// suspectedPending reports v's suspected defect at its current heads, not
// yet confirmed by an approver, whatever else the round found (fifth
// review, 5): it holds the gated hand-off and is shown on the issue.
func suspectedPending(v *v1alpha1.IntentVerification) bool {
	return v != nil && v.Suspected != nil && v.Suspected.Confirmed == nil && v.Suspected.HeadSetDigest == v.HeadSetDigest
}

// suspectedConfirmable reports v's suspected defect pending in a judged
// state (a fix round is not running, nor a new round being evaluated) and
// a /maul verify fix created at at confirming it: created after maul
// recorded it, so a person can have seen the evidence (fifth review, 1).
// GitHub and the record both keep whole seconds: a comment in the record's
// own second may predate it, and does not confirm it.
func suspectedConfirmable(v *v1alpha1.IntentVerification, at time.Time) bool {
	if !suspectedPending(v) {
		return false
	}
	switch v.State {
	case v1alpha1.VerificationUnconfirmed, v1alpha1.VerificationFailing, v1alpha1.VerificationExhausted:
	default:
		return false
	}
	return at.Truncate(time.Second).After(v.Suspected.RecordedAt.Truncate(time.Second))
}

// defectConfirmed reports v's suspected defect at its current heads
// confirmed by an approver (fifth review, 6).
func defectConfirmed(v *v1alpha1.IntentVerification) bool {
	return v != nil && v.Suspected != nil && v.Suspected.Confirmed != nil && v.Suspected.HeadSetDigest == v.HeadSetDigest
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

// carriedAnomalies are the look-around notes v records at headSet from
// rounds other than round: a later round at the same heads carries them,
// so the reviewer still sees them. They are those of v's latest judgement
// when it was at headSet, each stamped with the round that found it (a
// record from before rounds were stamped is that judgement's own). A
// record of round itself is that round's, judged again, which its
// judgement writes anew. nil once the heads moved.
func carriedAnomalies(v *v1alpha1.IntentVerification, headSet string, round int32) []v1alpha1.AnomalyRecord {
	if v == nil || headSet == "" || v.OutcomesHeadSetDigest != headSet {
		return nil
	}
	var out []v1alpha1.AnomalyRecord
	for _, a := range v.Anomalies {
		a = *a.DeepCopy()
		if a.Round == 0 {
			a.Round = v.OutcomesRound
		}
		if a.Round != round {
			out = append(out, a)
		}
	}
	return out
}

// carryAnomalies are a judgement's notes as the Intent records them:
// carried, those earlier rounds found at its heads, first, then own, the
// round's own, stamped with round, less any repeating a carried one's
// words, at most maxAnomalies: the oldest carried go first, so a round's
// own (at most maxAnomalies, anomalyRecords) are always kept.
func carryAnomalies(carried, own []v1alpha1.AnomalyRecord, round int32) []v1alpha1.AnomalyRecord {
	out := slices.Clone(carried)
	for _, a := range own {
		if slices.ContainsFunc(carried, func(h v1alpha1.AnomalyRecord) bool {
			return h.Summary == a.Summary && h.Route == a.Route
		}) {
			continue
		}
		a = *a.DeepCopy()
		a.Round = round
		out = append(out, a)
	}
	if len(out) > maxAnomalies {
		out = out[len(out)-maxAnomalies:]
	}
	return out
}

// rearmedSince reports that automatic fixes are armed by an approver's
// /maul verify fix made after v's no-defect conclusion: the approver read
// it and asked maul to fix the failure all the same. A re-arm before the
// conclusion, or an arming that is not an approver's, is answered by it.
// The re-arm is dated by GitHub's clock and the conclusion by maul's, so
// the comparison allows clockSkew, in the approver's favour: the halt the
// conclusion writes leaves no earlier re-arm armed anyway.
func rearmedSince(v *v1alpha1.IntentVerification) bool {
	r := v.AutoFix.Rearm
	return v.AutoFix.State == v1alpha1.AutoFixArmed && r != nil && v.NoDefect != nil &&
		r.At.Add(clockSkew).After(v.NoDefect.At.Time)
}

// outcomesReady reports a judged evaluation every outcome of which is now
// confirmed or accepted by an approver at its heads, with no replay round
// pending: what "ready" means once approvers accepted what
// maul could not confirm (/maul accept, after the judgement).
func outcomesReady(acc *v1alpha1.IntentAcceptance, v *v1alpha1.IntentVerification) bool {
	switch v.State {
	case v1alpha1.VerificationPassed:
		// Written only with its round's judgement at its heads
		// (applyJudgement); never ready over any other record.
		return judgedAtRecord(v)
	case v1alpha1.VerificationUnconfirmed, v1alpha1.VerificationFailing, v1alpha1.VerificationExhausted:
	default:
		return false
	}
	return judgementReady(acc, v)
}

// judgementReady reports v's judgement at its heads, whatever its state:
// every outcome confirmed or accepted, no replay round pending, and a clean
// environment, as Decide's hand-off needs (review H2).
func judgementReady(acc *v1alpha1.IntentAcceptance, v *v1alpha1.IntentVerification) bool {
	if !judgedAtRecord(v) {
		// No judgement of its round at its heads, or the state records a
		// round that could not run (S2-B8): never ready over outcomes on
		// record, whatever they say.
		return false
	}
	results := v.JudgedOutcomes()
	if len(v.Disputed) > 0 || len(results) == 0 ||
		!httpstep.EnvClass(v.OutcomesEnvironment).Clean() {
		return false
	}
	for _, r := range results {
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

// confirmedCount counts v's outcomes confirmed or accepted now, of the
// judgement of its current round at its heads only.
func confirmedCount(acc *v1alpha1.IntentAcceptance, v *v1alpha1.IntentVerification) int32 {
	var n int32
	for _, r := range v.JudgedOutcomes() {
		if res := effectiveResult(acc, v, r); res == v1alpha1.OutcomePassed || res == v1alpha1.OutcomeAccepted {
			n++
		}
	}
	return n
}

// keyLedgerLines is the data key of a verification's full ledger
// (<pv>-ledger, its host's: preview-controller's or the agents host's): one
// JSON line per entry.
const keyLedgerLines = toolledger.Key

// ledgerOf is pv's full ledger, one entry a line, read once a pass per pv
// (uncached) from the ConfigMap its status names, held to pv: nil when it is
// not pv's or cannot be read.
func (p *pass) ledgerOf(ctx context.Context, pv *v1alpha1.PreviewVerification) []string {
	if lines, ok := p.ledgerLines[pv.UID]; ok {
		return lines
	}
	if p.ledgerLines == nil {
		p.ledgerLines = map[types.UID][]string{}
	}
	p.ledgerLines[pv.UID] = nil
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
	p.ledgerLines[pv.UID] = strings.Split(cm.Data[keyLedgerLines], "\n")
	return p.ledgerLines[pv.UID]
}
