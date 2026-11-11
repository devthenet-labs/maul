// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/acceptance/crdconv"
	"github.com/devthenet-labs/maul/internal/checks"
	"github.com/devthenet-labs/maul/internal/httpstep"
	"github.com/devthenet-labs/maul/internal/report"
	"github.com/devthenet-labs/maul/internal/toolledger"
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
	// disputes are the evaluation's disputes, each replacement resolved to
	// a digest against the round (resolvedDisputes); nil without one.
	disputes  []report.Dispute
	anomalies []v1alpha1.AnomalyRecord
	env       []httpstep.PartFact
	// replay: the round was a replay round.
	replay bool
	// clear: the evaluator raised nothing a replay could not see again
	// (acceptance.ModelRoundClear); meaningful only with an evaluation.
	clear bool
	// cleared are the checks saved red whose replay this round passed
	// cleanly (clearedRed), recorded on status.acceptance.redCleared as
	// cleared by verification, the round's.
	cleared      []string
	verification string
	// base is the round's base runs as Decide read them (nil for a slot
	// round), for the check lines (baseClause).
	base *acceptance.BaseRuns
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
	return judgeHolding(p.in.Status.Acceptance, pv, stored, evaluationClaims(run),
		heldAnomalies(p.in.Status.Verification, pv.Spec.HeadSetDigest, pv.Spec.Round))
}

// judgeRound judges pv, a collected round, on acc, the store as read (nil
// when it could not be) and ev, its evaluator's claims (nil on a replay
// round), with no anomaly held at its heads.
func judgeRound(acc *v1alpha1.IntentAcceptance, pv *v1alpha1.PreviewVerification, stored []v1alpha1.SavedCheck,
	ev *report.Evaluation) roundJudgement {
	return judgeHolding(acc, pv, stored, ev, nil)
}

// judgeHolding is judgeRound with held, the anomalies earlier rounds found
// at pv's heads (heldAnomalies): those no approver accepted go to Decide
// beside the round's own (G-B-LIB-1), so the round is neither ready nor a
// clear model round while one stands. Their records are carried by
// applyJudgement, which reads them afresh in its own write.
func judgeHolding(acc *v1alpha1.IntentAcceptance, pv *v1alpha1.PreviewVerification, stored []v1alpha1.SavedCheck,
	ev *report.Evaluation, held []v1alpha1.AnomalyRecord) roundJudgement {
	cs := judgedChecks(pv, stored)
	in := decideInput(acc, pv, cs, ev)
	for _, a := range openAnomalies(held) {
		// Cited in an earlier round's ledger, not this one's: no seq.
		in.Anomalies = append(in.Anomalies, acceptance.AnomalyRef{Summary: a.Summary, Route: a.Route})
	}
	j := roundJudgement{decision: acceptance.Decide(in), checks: cs, stored: stored, evaluation: ev,
		anomalies: anomalyRecords(ev, pv), env: in.Env, replay: pv.Spec.Rung == v1alpha1.RungReplay,
		clear: acceptance.ModelRoundClear(in), cleared: clearedRed(cs, pv.Status.Replay), verification: pv.Name,
		base: in.Base}
	if ev != nil {
		j.disputes = resolvedDisputes(ev, pv)
	}
	return j
}

// suspectedRecord is the suspected defect j's decision found (fourth
// review: failed only on checks saved red), recorded for the verification
// j judged at headSet; nil for none.
func suspectedRecord(j roundJudgement, headSet string, now time.Time,
	prev *v1alpha1.SuspectedDefect) *v1alpha1.SuspectedDefect {
	s := j.decision.Suspected
	if s == nil || j.verification == "" || headSet == "" {
		return nil
	}
	if prev != nil && prev.Verification == j.verification && prev.HeadSetDigest == headSet {
		// The same round judged again: the record, its time and any
		// confirmation stand.
		return prev
	}
	rec := &v1alpha1.SuspectedDefect{Verification: j.verification, HeadSetDigest: headSet,
		RecordedAt: metav1.NewTime(now)}
	for _, f := range s.Failures {
		if !slices.Contains(rec.Outcomes, f.Outcome) && len(rec.Outcomes) < acceptance.MaxOutcomes {
			rec.Outcomes = append(rec.Outcomes, f.Outcome)
		}
	}
	return rec
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

// suspectedConfirmed reports v's suspected defect confirmed by an approver,
// found by pv's judgement at pv's heads: its fix request is then pv's.
func suspectedConfirmed(v *v1alpha1.IntentVerification, pv *v1alpha1.PreviewVerification) bool {
	return v != nil && v.Suspected != nil && v.Suspected.Confirmed != nil && v.Suspected.Verification == pv.Name &&
		v.Suspected.HeadSetDigest == pv.Spec.HeadSetDigest
}

// noRunData reports a saved check, green or red, that writes no data of its
// own run (checks.WritesRunData): its failure never starts an automatic
// fix, only a suspected defect a person confirms (#309 review). A body that
// does not parse counts as writing none: the safe side.
func noRunData(c v1alpha1.SavedCheck) bool {
	parsed, err := checks.Parse([]byte(c.Body))
	return err != nil || !checks.WritesRunData(parsed)
}

// clearedBefore reports a check saved red that replayed green in a round
// before verification's (third review, F1): a clearance never counts in
// the round that earned it, so the judgement and the fix path that
// re-decides after it decide alike.
func clearedBefore(acc *v1alpha1.IntentAcceptance, digest, verification string) bool {
	return slices.ContainsFunc(acc.RedCleared, func(c v1alpha1.RedClearance) bool {
		return c.Digest == digest && c.Verification != verification
	})
}

// addRedCleared records digests as cleared by verification, each once (the
// first clearance kept), the newest MaxSavedChecks.
func addRedCleared(acc *v1alpha1.IntentAcceptance, digests []string, verification string) {
	for _, d := range digests {
		if !slices.ContainsFunc(acc.RedCleared, func(c v1alpha1.RedClearance) bool { return c.Digest == d }) {
			acc.RedCleared = append(acc.RedCleared, v1alpha1.RedClearance{Digest: d, Verification: verification})
		}
	}
	if n := len(acc.RedCleared); n > v1alpha1.MaxSavedChecks {
		acc.RedCleared = slices.Clone(acc.RedCleared[n-v1alpha1.MaxSavedChecks:])
	}
}

// narrowedBefore reports a check narrowed in a round before verification's
// (Decision.Narrowed, bug 13 review): like a clearance, a narrowing counts
// from the round after the one that found it, which withheld the pass on
// the evaluator's own words, so that round is judged alike every time.
func narrowedBefore(acc *v1alpha1.IntentAcceptance, digest, verification string) bool {
	return slices.ContainsFunc(acc.Narrowed, func(n v1alpha1.NarrowedCheck) bool {
		return n.Digest == digest && n.Verification != verification
	})
}

// addNarrowed records digests as narrowed by verification, each once (the
// first record kept), drops the records of checks the store, when it was
// read (stored non-nil), no longer holds, and keeps the newest
// MaxSavedChecks.
func addNarrowed(acc *v1alpha1.IntentAcceptance, digests []string, verification string,
	stored []v1alpha1.SavedCheck) {
	for _, d := range digests {
		if !slices.ContainsFunc(acc.Narrowed, func(n v1alpha1.NarrowedCheck) bool { return n.Digest == d }) {
			acc.Narrowed = append(acc.Narrowed, v1alpha1.NarrowedCheck{Digest: d, Verification: verification})
		}
	}
	if stored != nil {
		acc.Narrowed = slices.DeleteFunc(acc.Narrowed, func(n v1alpha1.NarrowedCheck) bool {
			return !slices.ContainsFunc(stored, func(c v1alpha1.SavedCheck) bool { return c.Digest == n.Digest })
		})
	}
	if n := len(acc.Narrowed); n > v1alpha1.MaxSavedChecks {
		acc.Narrowed = slices.Clone(acc.Narrowed[n-v1alpha1.MaxSavedChecks:])
	}
	if len(acc.Narrowed) == 0 {
		acc.Narrowed = nil
	}
}

// clearedRed are the digests of the checks of cs saved red whose replays
// in runs all passed, with a run token, at least one (review finding 2):
// from then on their later steps have run, and their replays are judged as
// any saved check's; once also reviewed proves with the green question,
// each is proven against a working build and is a saved green check, whose
// later regression fixes automatically (fifth review, 4).
func clearedRed(cs []v1alpha1.SavedCheck, runs []v1alpha1.CheckRun) []string {
	var out []string
	for _, c := range cs {
		if c.Expect != v1alpha1.CheckExpectFail {
			continue
		}
		passed, clean := false, true
		for _, r := range runs {
			if r.CheckDigest != c.Digest {
				continue
			}
			passed = passed || (r.Result == v1alpha1.CheckRunPass && r.Token != "")
			clean = clean && r.Result == v1alpha1.CheckRunPass && r.Token != ""
		}
		if passed && clean {
			out = append(out, c.Digest)
		}
	}
	return out
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
// reviews, the checks narrowed in an earlier round, and the evaluator's
// claims with the ledger entries it cites that the round's ledger holds (its
// contrary entries as written), and an agents round's base (baseRuns; none
// on a slot round, S1's).
func decideInput(acc *v1alpha1.IntentAcceptance, pv *v1alpha1.PreviewVerification, cs []v1alpha1.SavedCheck,
	ev *report.Evaluation) acceptance.Input {
	in := storeInput(acc, cs, pv.Name)
	in.Replay, in.Confirmations = checkRuns(pv.Status.Replay), checkRuns(pv.Status.Confirmations)
	for _, c := range pv.Spec.Checks {
		// Every check of the round's copy of the store was to replay
		// (review VC-3): one with no replay run passes nothing.
		in.ReplayRequired = append(in.ReplayRequired, c.Digest)
	}
	in.Base = baseRuns(pv)
	in.Env = crdconv.PartFacts(pv.Status.Environment)
	if ev == nil {
		return in
	}
	for _, v := range ev.Verdicts {
		// Contrary entries go in as written (S1 demo bug 13): they only ever
		// withhold a pass, so the evaluator's word that it saw the outcome
		// fail is enough, whether or not the ledger holds the seq.
		in.Verdicts = append(in.Verdicts, acceptance.EvaluatorVerdict{Outcome: v.ID, Claim: acceptance.Claim(v.Verdict),
			Ledger: citedOnly(v.Ledger, pv), Contrary: slices.Clone(v.Contrary)})
	}
	for _, d := range resolvedDisputes(ev, pv) {
		in.Disputes = append(in.Disputes, acceptance.CheckDispute{CheckDigest: d.CheckDigest, Reason: d.Reason,
			Replacement: d.Replacement, Ledger: citedOnly(d.Ledger, pv)})
	}
	for _, a := range ev.Anomalies {
		in.Anomalies = append(in.Anomalies, acceptance.AnomalyRef{Summary: a.Summary, Route: a.Route,
			Ledger: citedOnly(a.Ledger, pv)})
	}
	return in
}

// storeInput is Decide's view of the store for the round named
// verification: the approved outcomes (each with the digest its reviews are
// keyed by), the checks cs (valid: only valid checks are ever stored) with
// their clearance and narrowing records from rounds before it, and the
// cached reviews. decideInput and the rung (rungFor) share it, so the rung
// reads the store exactly as the round's judgement will.
func storeInput(acc *v1alpha1.IntentAcceptance, cs []v1alpha1.SavedCheck, verification string) acceptance.Input {
	var in acceptance.Input
	for _, o := range acc.Outcomes {
		in.Outcomes = append(in.Outcomes, acceptance.OutcomeRef{ID: o.ID, Digest: outcomeDigest(o),
			Keep: o.EffectiveKind() == v1alpha1.OutcomeKindKeep, Manual: o.Manual})
	}
	for _, c := range cs {
		in.Checks = append(in.Checks, acceptance.StoredCheck{Digest: c.Digest, Outcome: c.Outcome,
			Form: acceptance.CheckForm(c.Form), Valid: true, ExpectFail: c.Expect == v1alpha1.CheckExpectFail,
			FailedStep: c.FailedStep, FailedAssertion: c.FailedAssertion,
			Cleared: clearedBefore(acc, c.Digest, verification), NoRunData: noRunData(c),
			Narrowed: narrowedBefore(acc, c.Digest, verification)})
	}
	for _, r := range acc.Reviews {
		in.Reviews = append(in.Reviews, acceptance.CheckReview{CheckDigest: r.CheckDigest,
			OutcomeDigest: r.OutcomeDigest, Answer: acceptance.ReviewAnswer(r.Answer),
			ExpectFail: r.Expect == v1alpha1.CheckExpectFail, FailedStep: r.FailedStep,
			FailedAssertion: r.FailedAssertion})
	}
	return in
}

// resolvedDisputes are ev's disputes with each replacement, which the
// report names by its proposed check's id, resolved to that proposal's
// digest (the store, the reviews and Decide key checks by digest); a
// replacement naming no proposal resolves to none. A proposal that names
// only its file carries no digest in the report (agent-runner read it from
// the file, in the pod): it resolves to the digest of the check of its id
// and outcome that the round's ledger confirmed (pv.status.confirmed, read
// from the ledger's own green dry run), and to none when there is none.
func resolvedDisputes(ev *report.Evaluation, pv *v1alpha1.PreviewVerification) []report.Dispute {
	out := make([]report.Dispute, 0, len(ev.Disputes))
	for _, d := range ev.Disputes {
		id := d.Replacement
		d.Replacement = ""
		if i := slices.IndexFunc(ev.Proposed, func(pc report.ProposedCheck) bool { return pc.ID == id }); id != "" &&
			i >= 0 {
			d.Replacement = proposedDigest(ev.Proposed[i], pv)
		}
		out = append(out, d)
	}
	return out
}

// proposedDigest is the digest a proposal names: its own, or for one naming
// only its file, that of the check of its id and outcome pv confirmed.
func proposedDigest(p report.ProposedCheck, pv *v1alpha1.PreviewVerification) string {
	if p.Digest != "" {
		return p.Digest
	}
	i := slices.IndexFunc(pv.Status.Confirmed, func(c v1alpha1.SavedCheck) bool {
		return c.ID == p.ID && c.Outcome == p.Outcome
	})
	if i < 0 {
		return ""
	}
	return pv.Status.Confirmed[i].Digest
}

// baseRuns is an agents round's base as Decide reads it (rule 5, SPEC-S2
// S2-20): always required, since every check of the round was to run at
// the base too; unbuildable as the agents host recorded it (only a base
// build that ran and failed); and the runs at the base tree, each with its
// cause. A round whose base was never written has a required base with no
// run, which passes nothing. nil for a slot round, which has no base.
func baseRuns(pv *v1alpha1.PreviewVerification) *acceptance.BaseRuns {
	if !pv.Spec.IsAgents() {
		return nil
	}
	out := &acceptance.BaseRuns{Required: true}
	b := pv.Status.Base
	if b == nil {
		return out
	}
	out.Unbuildable = b.Unbuildable
	var at []v1alpha1.CheckRun
	for _, r := range b.Runs {
		if r.Side() == v1alpha1.TreeBase {
			at = append(at, r)
		}
	}
	out.Runs = checkRuns(at)
	return out
}

// checkRuns are a verification's check runs as Decide reads them: only
// runs carrying the run token tools generated (the schema requires one; a
// run without it is no run of tools').
func checkRuns(rs []v1alpha1.CheckRun) []acceptance.CheckRun {
	out := make([]acceptance.CheckRun, 0, len(rs))
	for _, r := range rs {
		if r.Token == "" {
			continue
		}
		run := acceptance.CheckRun{CheckDigest: r.CheckDigest, Result: acceptance.RunResult(r.Result),
			Token: r.Token, Seq: r.Seq, ExpectFail: r.Expect == v1alpha1.CheckExpectFail,
			Cause: httpstep.RunCause(r.Cause)}
		if r.RedByAssertion() {
			run.AssertionStep, run.AssertionID = r.AssertionStep, r.AssertionID
		}
		out = append(out, run)
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

// heldAnomalies are the look-around anomalies v records at headSet from
// rounds other than round (G-B-LIB-1): an anomaly is a fact about the
// commits it was found at. They are those of v's latest judgement when it
// was at headSet, each stamped with the round that found it (a record from
// before rounds were stamped is that judgement's own). A record of round
// itself is that round's, judged again, which its judgement writes anew.
// nil once the heads moved.
func heldAnomalies(v *v1alpha1.IntentVerification, headSet string, round int32) []v1alpha1.AnomalyRecord {
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

// heldOutcomes are the outcomes v's latest judgement at headSet found
// failing (reproduced) or suspected a defect in, when that judgement was of
// a round before round, and those it held already (ReasonHeldFinding), each
// by the check it rested on (G-B-LIB-1): a check failing at heads is a
// finding about them, which a later round there does not erase. nil once
// the heads moved.
func heldOutcomes(v *v1alpha1.IntentVerification, headSet string, round int32) map[string]string {
	if v == nil || headSet == "" || v.OutcomesHeadSetDigest != headSet {
		return nil
	}
	out := map[string]string{}
	for _, r := range v.Outcomes {
		found := r.Reason == acceptance.ReasonReproduced || r.Reason == acceptance.ReasonSuspectedDefect
		if r.Reason == acceptance.ReasonHeldFinding || found && v.OutcomesRound != round {
			out[r.ID] = r.Basis
		}
	}
	return out
}

// holdOutcomes holds rs, a round's results at headSet, to the findings
// earlier rounds made there (heldOutcomes, by outcome id and check): an
// outcome this round did not find failing again is unsure, reason
// ReasonHeldFinding, on the check it failed on, whatever else this round
// decided; accepted when an approver accepted it at the heads. Only a
// check retired, through a dispute with a proven replacement (this round's
// Decision.Retire, or gone from the store as read), releases its outcome:
// that is the evidence the check was wrong. A store that could not be read
// releases nothing.
func holdOutcomes(rs []v1alpha1.OutcomeResult, failing map[string]string, acc *v1alpha1.IntentAcceptance,
	headSet string, j roundJudgement) []v1alpha1.OutcomeResult {
	for i := range rs {
		basis, ok := failing[rs[i].ID]
		if !ok || rs[i].Reason == acceptance.ReasonReproduced || rs[i].Reason == acceptance.ReasonSuspectedDefect {
			continue // not held, or found again this round
		}
		if basis != "" && (slices.Contains(j.decision.Retire, basis) ||
			j.stored != nil && !slices.ContainsFunc(j.stored, func(c v1alpha1.SavedCheck) bool { return c.Digest == basis })) {
			continue
		}
		r := v1alpha1.OutcomeResult{ID: rs[i].ID, Result: v1alpha1.OutcomeUnsure, Basis: basis,
			Reason: acceptance.ReasonHeldFinding}
		for _, cs := range [][]v1alpha1.SavedCheck{j.checks, j.stored} {
			if k := slices.IndexFunc(cs, func(c v1alpha1.SavedCheck) bool { return c.Digest == basis }); k >= 0 &&
				basis != "" {
				r.Line = checkLine(cs[k])
				break
			}
		}
		if outcomeAccepted(acc, r.ID, headSet) {
			r.Result = v1alpha1.OutcomeAccepted
		}
		rs[i] = r
	}
	return rs
}

// openAnomalies are the anomalies of as no approver accepted.
func openAnomalies(as []v1alpha1.AnomalyRecord) []v1alpha1.AnomalyRecord {
	var out []v1alpha1.AnomalyRecord
	for _, a := range as {
		if a.Accepted == nil {
			out = append(out, a)
		}
	}
	return out
}

// carryAnomalies are a judgement's anomalies as the Intent records them:
// held, those earlier rounds found at its heads, first, then own, the
// round's own, stamped with round, less any repeating a held one's words
// (the held record, and any acceptance of it, stands), at most
// maxAnomalies.
func carryAnomalies(held, own []v1alpha1.AnomalyRecord, round int32) []v1alpha1.AnomalyRecord {
	out := slices.Clone(held)
	for _, a := range own {
		if slices.ContainsFunc(held, func(h v1alpha1.AnomalyRecord) bool {
			return h.Summary == a.Summary && h.Route == a.Route
		}) {
			continue
		}
		a = *a.DeepCopy()
		a.Round = round
		out = append(out, a)
	}
	if len(out) > maxAnomalies {
		out = out[:maxAnomalies]
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
			if r.Result == acceptance.ResultPassed && res.Line != "" && changeOutcome(acc, r.ID) {
				res.Line = withBaseClause(res.Line, j.base, r.Basis)
			}
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
// whether a replay round follows it. v carries the judgement's anomalies
// with those held at its heads (applyJudgement): any no approver accepted
// keeps it from Passed.
func judgedState(v *v1alpha1.IntentVerification, j roundJudgement, results []v1alpha1.OutcomeResult,
	fix *acceptance.FixRequest) (v1alpha1.IntentVerificationState, bool) {
	replayNext := j.decision.ReplayRound && !j.replay
	switch {
	case fix != nil && v.NoDefect != nil && v.NoDefect.HeadSetDigest == v.HeadSetDigest && !rearmedSince(v):
		// A fix round concluded no defect at these heads, and the round
		// still fails: maul and its fixer disagree, and no fix is left,
		// unless an approver's /maul verify fix since then says to fix
		// it again (the human always wins).
		return v1alpha1.VerificationExhausted, false
	case fix != nil:
		return v1alpha1.VerificationFailing, false
	case !replayNext && len(openAnomalies(v.Anomalies)) == 0 && j.decision.Env.Clean() && confirmedAll(results):
		return v1alpha1.VerificationPassed, false
	}
	return v1alpha1.VerificationUnconfirmed, replayNext
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

// applyJudgement writes j onto cur's verification: the results, the counts,
// the anomalies (those earlier rounds found at its heads, which it carries,
// then its own: G-B-LIB-1), the disputed checks a replay round awaits, and
// the state. A held anomaly no approver accepted keeps the round from
// Passed and from being a clear model round, whatever its own evaluator
// reported: only moved heads or an approver's acceptance end it.
func applyJudgement(cur *v1alpha1.Intent, j roundJudgement, now time.Time) {
	nv, acc := cur.Status.Verification, cur.Status.Acceptance
	held := heldAnomalies(nv, nv.HeadSetDigest, nv.Round)
	if len(openAnomalies(held)) > 0 {
		j.clear = false
	}
	failing := heldOutcomes(nv, nv.HeadSetDigest, nv.Round)
	clearJudgement(nv)
	nv.Outcomes = holdOutcomes(outcomeResults(acc, nv.HeadSetDigest, j), failing, acc, nv.HeadSetDigest, j)
	nv.OutcomesRound, nv.OutcomesHeadSetDigest = nv.Round, nv.HeadSetDigest
	nv.OutcomesEnvironment = string(j.decision.Env)
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
	nv.Anomalies = carryAnomalies(held, j.anomalies, nv.Round)
	addRedCleared(acc, j.cleared, j.verification)
	addNarrowed(acc, j.decision.Narrowed, j.verification, j.stored)
	nv.Suspected = suspectedRecord(j, nv.HeadSetDigest, now, nv.Suspected)
	state, replayNext := judgedState(nv, j, nv.Outcomes, pendingFix(acc, nv.HeadSetDigest, j))
	nv.State, nv.Disputed = state, nil
	if replayNext {
		nv.Disputed = slices.Clone(j.decision.Retire)
	}
	recordLadder(nv, acc, j)
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
		applyJudgement(cur, j, p.now)
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
	wrote, err := p.retireChecks(ctx, still, j.disputes)
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
	if s := j.decision.Suspected; s != nil && suspectedConfirmed(p.in.Status.Verification, pv) {
		// An approver confirmed the suspected defect (/maul verify fix): its
		// failures are the fix round's, as an automatic fix's would be.
		failures := s.Failures
		if f := j.decision.Fix; f != nil {
			failures = append(slices.Clone(f.Failures), failures...)
		}
		outcomes, digests := make([]string, 0, len(failures)), make([]string, 0, len(failures))
		for _, f := range failures {
			outcomes, digests = append(outcomes, f.Outcome), append(digests, f.CheckDigest)
		}
		j.decision.Fix = &acceptance.FixRequest{Failures: failures,
			Fingerprint: acceptance.EvaluationFingerprint(outcomes, digests, j.env)}
	}
	fix := pendingFix(p.in.Status.Acceptance, pv.Spec.HeadSetDigest, j)
	if fix == nil || j.decision.Env != httpstep.EnvOK {
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
	case parsed.Run != nil:
		line = runLine(parsed.Run)
	default:
		return ""
	}
	line = strings.Join(strings.Fields(untrusted.Escape(line)), " ")
	return untrusted.Cut(line, maxCheckLineBytes)
}

// baseCauseWords say, for a check line, why a run at base leaves no base
// comparison (httpstep.RunCause.NoComparison).
var baseCauseWords = map[httpstep.RunCause]string{
	httpstep.CauseCompile:            "does not compile at base",
	httpstep.CauseVendoredTree:       "the base tree is vendored",
	httpstep.CauseReplaceOutsideTree: "the base replaces a module outside its tree",
	httpstep.CauseNoModule:           "the base has no go.mod",
	httpstep.CauseBaseUnbuildable:    "the base did not build",
}

// changeOutcome reports that id is a change outcome of acc: the kind whose
// check must discriminate at base (rule 5).
func changeOutcome(acc *v1alpha1.IntentAcceptance, id string) bool {
	if acc == nil {
		return false
	}
	i := slices.IndexFunc(acc.Outcomes, func(o v1alpha1.AcceptanceOutcome) bool { return o.ID == id })
	return i >= 0 && acc.Outcomes[i].EffectiveKind() == v1alpha1.OutcomeKindChange
}

// withBaseClause is line, a passed change outcome's check line, with what
// its runs at base say (SPEC-S2 Q29, section 7 (c), (d)): that the check
// discriminates (red by assertion at base) or has no base comparison, and
// why; line as it is for a round with no base (the slot placement) or a
// base that says neither. The line is cut first, so the clause is never
// cut off.
func withBaseClause(line string, base *acceptance.BaseRuns, digest string) string {
	var clause string
	switch cmp, cause := base.Comparison(digest); cmp {
	case acceptance.BaseDiscriminates:
		clause = "; discriminates: fails at base by assertion"
	case acceptance.BaseNoComparison:
		clause = "; no base comparison"
		if w := baseCauseWords[cause]; w != "" {
			clause += ": " + w
		}
	default:
		return line
	}
	return untrusted.Cut(line, maxCheckLineBytes-len(clause)) + clause
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
	return expectsLine("exec: "+program, e.Expect)
}

// runLine: "run: a go program of 2 files; expects exit 0, stdout".
func runLine(r *checks.RunCheck) string {
	return expectsLine(fmt.Sprintf("run: a %s program of %d %s", r.Language, len(r.Files),
		pluralWord(len(r.Files), "file", "files")), r.Expect)
}

// expectsLine is head and what x asserts.
func expectsLine(head string, x checks.ExecExpect) string {
	var expects []string
	if x.Exit != nil {
		expects = append(expects, fmt.Sprintf("exit %d", *x.Exit))
	}
	if x.StdoutContains != "" || len(x.StdoutJSON) > 0 {
		expects = append(expects, "stdout")
	}
	if len(expects) == 0 {
		return head
	}
	return head + "; expects " + strings.Join(expects, ", ")
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
// every outcome confirmed or accepted, no anomaly an approver did not accept,
// no replay round pending, and a clean environment, as Decide's hand-off
// needs (review H2).
func judgementReady(acc *v1alpha1.IntentAcceptance, v *v1alpha1.IntentVerification) bool {
	if !judgedAtRecord(v) {
		// No judgement of its round at its heads, or the state records a
		// round that could not run (S2-B8): never ready over outcomes on
		// record, whatever they say.
		return false
	}
	results := v.JudgedOutcomes()
	if len(openAnomalies(v.Anomalies)) > 0 || len(v.Disputed) > 0 || len(results) == 0 ||
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
// attempt, each with what its element showed when the runner received it
// (gate D-B10, as the reproduction shows it), bounded.
func failedSteps(r *toolwire.CheckRunRecord) string {
	var lines []string
	for i, a := range r.Attempts {
		for _, s := range a.Steps {
			if s.Result != toolwire.ResultPass && strings.TrimSpace(s.Detail) != "" {
				line := fmt.Sprintf("attempt %d, step %d: %s", i+1, s.Index+1, s.Detail)
				if got := received(s.Received, a.Run); got != "" {
					line += "; the element showed: " + got
				}
				lines = append(lines, line)
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
