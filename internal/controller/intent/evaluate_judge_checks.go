// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"fmt"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/acceptance/crdconv"
	"github.com/devthenet-labs/maul/internal/checks"
	"github.com/devthenet-labs/maul/internal/httpstep"
	"github.com/devthenet-labs/maul/internal/report"
	"github.com/devthenet-labs/maul/internal/toolwire"
	"github.com/devthenet-labs/maul/internal/untrusted"
)

// The saved-check judgement, retired ("evaluator observed", W4). Until
// release N's switch an evaluation round was judged here: acceptance.Decide
// over the saved checks the round replayed and confirmed, the check store,
// the reviewer's answers and the base runs, with the rung ladder, disputes,
// red checks and the suspected defect built on it. Nothing reachable calls
// it any more: every round is judged by acceptance.Judge
// (evaluate_judge.go). It stays compiled, with the tests that pin it, only
// until the deletions after the switch (W7) remove it with acceptance.Decide.

// Bounds of a saved check's one-line description.
const (
	maxCheckLineBytes = 1000
	maxCheckLineSteps = 12
	// maxAppOutputBytes bounds the application output a failure shows.
	maxAppOutputBytes = 2 << 10
)

// checksJudgement is acceptance.Decide's answer on one round, with what it
// was judged on.
type checksJudgement struct {
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

// judgeChecksRound judges pv, a collected round, on acc, the store as read (nil
// when it could not be) and ev, its evaluator's claims (nil on a replay
// round). Its anomalies are recorded as notes and go to Decide as nothing.
func judgeChecksRound(acc *v1alpha1.IntentAcceptance, pv *v1alpha1.PreviewVerification, stored []v1alpha1.SavedCheck,
	ev *report.Evaluation) checksJudgement {
	cs := judgedChecks(pv, stored)
	in := decideInput(acc, pv, cs, ev)
	j := checksJudgement{decision: acceptance.Decide(in), checks: cs, stored: stored, evaluation: ev,
		anomalies: checksAnomalyRecords(ev, pv), env: in.Env, replay: pv.Spec.Rung == v1alpha1.RungReplay,
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
func suspectedRecord(j checksJudgement, headSet string, now time.Time,
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
// on a slot round, S1's). Its anomalies are no input: they are notes.
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
	return in
}

// storeInput is Decide's view of the store for the round named
// verification: the approved outcomes (each with the digest its reviews are
// keyed by), the checks cs (valid while they hold to the substance rules as
// maul reads them now: storedValid) with
// their clearance and narrowing records from rounds before it, and the
// cached reviews. decideInput and, before the switch, the rung shared it,
// so the rung read the store exactly as the round's judgement would.
func storeInput(acc *v1alpha1.IntentAcceptance, cs []v1alpha1.SavedCheck, verification string) acceptance.Input {
	var in acceptance.Input
	for _, o := range acc.Outcomes {
		in.Outcomes = append(in.Outcomes, acceptance.OutcomeRef{ID: o.ID, Digest: outcomeDigest(o),
			Keep: o.EffectiveKind() == v1alpha1.OutcomeKindKeep, Manual: o.Manual})
	}
	for _, c := range cs {
		in.Checks = append(in.Checks, acceptance.StoredCheck{Digest: c.Digest, Outcome: c.Outcome,
			Form: acceptance.CheckForm(c.Form), Valid: storedValid(c), ExpectFail: c.Expect == v1alpha1.CheckExpectFail,
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

// storedValid reports a stored check that still holds to the saved-check
// substance rules (checks.Substantive; code review 2026-11-18, checks 6). A
// check stored under earlier rules that they now refuse keeps its body and
// digest and replays as stored, but supports nothing: Decide never passes
// or fails an outcome on it nor asks a fix of it, and the rung is full, so
// a round can save a check that asserts something. A body that does not
// parse counts as refused: the safe side.
func storedValid(c v1alpha1.SavedCheck) bool {
	parsed, err := checks.Parse([]byte(c.Body))
	return err == nil && checks.Substantive(parsed) == nil
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

// checksAnomalyRecords are the evaluator's look-around anomalies as the Intent
// records them: those citing an entry pv's ledger holds (an anomaly needs
// evidence), bounded to the schema's.
func checksAnomalyRecords(ev *report.Evaluation, pv *v1alpha1.PreviewVerification) []v1alpha1.AnomalyRecord {
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

// checksHeldOutcomes are the outcomes v's latest judgement at headSet found
// failing (foundFailing: a check failed and the evaluator agreed, a check
// saved red failed, or the evaluator saw it fail, whatever reason Decide
// recorded), when that judgement was of a round before round, and those it
// held already (ReasonHeldFinding), each by the check it rested on ("" for
// the evaluator's word alone) (G-B-LIB-1): a failure seen at heads is a
// finding about them, which a later round there does not erase. nil once
// the heads moved. A fix round at headSet that concluded no defect releases
// only what it was handed (handedToFixer), and only while the judgement it
// was handed is the latest (NoDefectRecord.Round).
func checksHeldOutcomes(v *v1alpha1.IntentVerification, headSet string, round int32) map[string]string {
	if v == nil || headSet == "" || v.OutcomesHeadSetDigest != headSet {
		return nil
	}
	// A no-defect conclusion speaks for the judgement its fix round was
	// handed alone: a later round there that found the failure again made a
	// new finding, which it predates.
	noDefect := v.NoDefect != nil && v.NoDefect.HeadSetDigest == headSet && v.NoDefect.Round != 0 &&
		v.NoDefect.Round == v.OutcomesRound
	out := map[string]string{}
	for _, r := range v.Outcomes {
		if noDefect && handedToFixer(v, r, headSet) {
			// A fix round handed this failure's reproduction at these heads
			// concluded, in its recorded reason, that the code has no
			// defect: that is the evidence a later round's pass there
			// rests on.
			continue
		}
		switch {
		case r.Reason == acceptance.ReasonHeldFinding, findsFailing(r.Reason) && v.OutcomesRound != round:
			out[r.ID] = r.Basis
		case r.Objected && v.OutcomesRound != round:
			// The evaluator saw it fail under a reason that rests on no
			// check of maul's: its word alone, which no retirement answers.
			out[r.ID] = ""
		}
	}
	return out
}

// foundFailing reports a result of a round that found its outcome failing
// at the round's heads: a reason that says so (findsFailing), or the
// evaluator's objection recorded beside any other reason (Objected; code
// review 2026-11-18, judge 1).
func foundFailing(r v1alpha1.OutcomeResult) bool {
	return findsFailing(r.Reason) || r.Objected
}

// findsFailing reports a judged reason that is a finding of the outcome
// failing at the round's heads: a fix-worthy check failed and the evaluator
// agreed (reproduced), a check saved red failed (suspected-defect), or the
// evaluator saw the outcome fail with no check maul could confirm
// (model-only-fail, evaluator-saw-failure).
func findsFailing(reason string) bool {
	switch reason {
	case acceptance.ReasonReproduced, acceptance.ReasonSuspectedDefect, acceptance.ReasonModelOnlyFail,
		acceptance.ReasonEvaluatorSawFailure:
		return true
	}
	return false
}

// handedToFixer reports r, a result of v's latest judgement at headSet, as
// one the acceptance-fix round was handed: a reproduced failure, or the
// suspected defect an approver confirmed there (/maul verify fix). An
// outcome only suspected, or seen failing by the evaluator alone, was
// never handed to it, so its no-defect conclusion says nothing of it.
func handedToFixer(v *v1alpha1.IntentVerification, r v1alpha1.OutcomeResult, headSet string) bool {
	switch r.Reason {
	case acceptance.ReasonReproduced:
		return true
	case acceptance.ReasonSuspectedDefect:
		s := v.Suspected
		return s != nil && s.Confirmed != nil && s.HeadSetDigest == headSet && slices.Contains(s.Outcomes, r.ID)
	}
	return false
}

// checksHoldOutcomes holds rs, a round's results at headSet, to the findings
// earlier rounds made there (checksHeldOutcomes, by outcome id and check): an
// outcome this round did not find failing again is unsure, reason
// ReasonHeldFinding, on the check it failed on, whatever else this round
// decided; accepted when an approver accepted it at the heads. Only a
// check gone from the store as read, retired through a dispute with a
// proven replacement, releases its outcome: that is the evidence the check
// was wrong. A retirement this round's Decision asks for releases nothing
// yet, since the store may refuse it (retirable), and the round's record
// must then keep the hold. A store that could not be read releases nothing.
func checksHoldOutcomes(rs []v1alpha1.OutcomeResult, failing map[string]string, acc *v1alpha1.IntentAcceptance,
	headSet string, j checksJudgement) []v1alpha1.OutcomeResult {
	for i := range rs {
		basis, ok := failing[rs[i].ID]
		if !ok || foundFailing(rs[i]) {
			continue // not held, or found failing again this round
		}
		if basis != "" && j.stored != nil &&
			!slices.ContainsFunc(j.stored, func(c v1alpha1.SavedCheck) bool { return c.Digest == basis }) {
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

// checksOutcomeResults are the round's results as the Intent records them, in
// plan order: Decide's, each over its basis check's line, and an outcome an
// approver accepted at the heads (one Decide did not pass) accepted.
func checksOutcomeResults(acc *v1alpha1.IntentAcceptance, headSet string, j checksJudgement) []v1alpha1.OutcomeResult {
	out := make([]v1alpha1.OutcomeResult, 0, len(j.decision.PerOutcome))
	for _, r := range j.decision.PerOutcome {
		res := v1alpha1.OutcomeResult{ID: r.ID, Result: v1alpha1.OutcomeResultValue(r.Result), Basis: r.Basis,
			Reason: r.Reason, Objected: slices.Contains(j.decision.Objected, r.ID)}
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

// checksPendingFix is j's fix request less the failures of outcomes an approver
// accepted at the heads (the human closed them), its fingerprint over what
// is left; nil when none is.
func checksPendingFix(acc *v1alpha1.IntentAcceptance, headSet string, j checksJudgement) *acceptance.FixRequest {
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

// checksJudgedState is the state a judgement routes to (the list above), and
// whether a replay round follows it. Its anomalies are notes: they hold
// nothing.
func checksJudgedState(v *v1alpha1.IntentVerification, j checksJudgement, results []v1alpha1.OutcomeResult,
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
	case !replayNext && j.decision.Env.Clean() && confirmedAll(results):
		return v1alpha1.VerificationPassed, false
	}
	return v1alpha1.VerificationUnconfirmed, replayNext
}

// applyChecksJudgement writes j onto cur's verification: the results, the counts,
// the notes (those earlier rounds found at its heads, which it carries,
// then its own), the disputed checks a replay round awaits, and the state.
// An outcome an earlier round found failing or suspected at its heads is
// held (checksHeldOutcomes, G-B-LIB-1) whatever its own evaluator reported: only
// moved heads, an approver's acceptance, a retirement through a dispute or
// a fix round's no-defect conclusion end it.
func applyChecksJudgement(cur *v1alpha1.Intent, j checksJudgement, now time.Time) {
	nv, acc := cur.Status.Verification, cur.Status.Acceptance
	carried := carriedAnomalies(nv, nv.HeadSetDigest, nv.Round)
	failing := checksHeldOutcomes(nv, nv.HeadSetDigest, nv.Round)
	clearJudgement(nv)
	nv.Outcomes = checksHoldOutcomes(checksOutcomeResults(acc, nv.HeadSetDigest, j), failing, acc, nv.HeadSetDigest, j)
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
	nv.Anomalies = carryAnomalies(carried, j.anomalies, nv.Round)
	addRedCleared(acc, j.cleared, j.verification)
	addNarrowed(acc, j.decision.Narrowed, j.verification, j.stored)
	nv.Suspected = suspectedRecord(j, nv.HeadSetDigest, now, nv.Suspected)
	state, replayNext := checksJudgedState(nv, j, nv.Outcomes, checksPendingFix(acc, nv.HeadSetDigest, j))
	nv.State, nv.Disputed = state, nil
	if replayNext {
		nv.Disputed = slices.Clone(j.decision.Retire)
	}
	recordLadder(nv, acc, j)
	countManual(nv, acc)
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
