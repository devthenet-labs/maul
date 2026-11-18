// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"fmt"
	"math/rand"
	"slices"
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/report"
)

// postedCheck writes data of its own run and asserts only that the call
// worked: the substance rules refuse it, and a failure of a check that
// writes run data would otherwise start an automatic fix.
const postedCheck = `{"id":"version-shown.posted","outcome":"version-shown","form":"http","http":{"target":"app",
 "steps":[{"request":{"method":"POST","path":"/notes","json":{"name":"{{run}}-v"}},"expect":{"status":201}}]}}`

// TestStoredCheckTheSubstanceRulesRefuseSupportsNothing (code review
// 2026-11-18, checks 6): a check stored under earlier rules that the
// saved-check substance rules now refuse (checks.Substantive) is replayed
// as it was stored, by its digest, but supports nothing: green it passes
// no outcome, red it fails none and asks for no fix, even with the
// evaluator's cited fail, and the next round is full, so an evaluator can
// save a check that asserts something.
func TestStoredCheckTheSubstanceRulesRefuseSupportsNothing(t *testing.T) {
	o := v1alpha1.AcceptanceOutcome{ID: oVersion, Outcome: "The build page shows the version"}
	vacuous := savedCheck(t, postedCheck)
	acc := &v1alpha1.IntentAcceptance{Outcomes: []v1alpha1.AcceptanceOutcome{o}, OutcomesDigest: "sha256:o",
		Reviews: []v1alpha1.CheckReviewRecord{{CheckDigest: vacuous.Digest, OutcomeDigest: outcomeDigest(o),
			Answer: v1alpha1.ReviewProves, Run: "r1", Check: vacuous.ID, Reason: "yes"}}}
	stored := []v1alpha1.SavedCheck{vacuous}
	if store := storeInput(acc, stored, ""); store.Checks[0].Valid {
		t.Fatal("a stored check the substance rules refuse is valid to Decide")
	}
	fail := &report.Evaluation{Verdicts: []report.OutcomeVerdict{{ID: oVersion, Verdict: report.VerdictFail,
		Ledger: []int32{1}}}}
	for _, tc := range []struct {
		name   string
		result v1alpha1.CheckRunResult
		ev     *report.Evaluation
		rung   v1alpha1.VerificationRung
	}{
		{name: "green on replay", result: v1alpha1.CheckRunPass, rung: v1alpha1.RungReplay},
		{name: "green in a full round", result: v1alpha1.CheckRunPass, rung: v1alpha1.RungFull},
		{name: "red in a full round the evaluator fails", result: v1alpha1.CheckRunFail, ev: fail,
			rung: v1alpha1.RungFull},
		{name: "red on replay", result: v1alpha1.CheckRunFail, rung: v1alpha1.RungReplay},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := v1alpha1.CheckRun{CheckDigest: vacuous.Digest, Result: tc.result, Token: "t1", Seq: 1}
			if tc.result == v1alpha1.CheckRunFail {
				run.AssertionStep = 1
			}
			pv := &v1alpha1.PreviewVerification{
				Spec: v1alpha1.PreviewVerificationSpec{Rung: tc.rung, Checks: stored},
				Status: v1alpha1.PreviewVerificationStatus{Replay: []v1alpha1.CheckRun{run},
					Ledger: &v1alpha1.LedgerSummary{Count: 5}},
			}
			d := judgeRound(acc, pv, stored, tc.ev).decision
			if got := d.PerOutcome[0]; got.Result != acceptance.ResultUnsure {
				t.Fatalf("outcome = %+v, want unsure", got)
			}
			if d.Fix != nil || d.Suspected != nil || d.Handoff == acceptance.HandoffReady {
				t.Fatalf("decision = %+v: no fix, no suspected defect, not ready", d)
			}
		})
	}
	store := storeInput(acc, stored, "")
	rung, _ := acceptance.StartRung(acceptance.RungInput{HeadSet: "h", OutcomesDigest: acc.OutcomesDigest,
		Model:    &acceptance.ModelRound{HeadSet: "h", OutcomesDigest: acc.OutcomesDigest, Clear: true},
		Outcomes: store.Outcomes, Checks: store.Checks, Reviews: store.Reviews})
	if rung != acceptance.RungFull {
		t.Fatalf("rung = %s, want full: the outcome has no stored check a replay could pass it on", rung)
	}
}

// TestRefusedStoredCheckBesideAValidOne: a refused stored check never
// takes a valid sibling's pass away when both are green (a false fail),
// and red it holds the outcome unsure, never failed and never a fix, as
// any failing check of an outcome withholds its pass (rule 6).
func TestRefusedStoredCheckBesideAValidOne(t *testing.T) {
	o := v1alpha1.AcceptanceOutcome{ID: oVersion, Outcome: "The build page shows the version"}
	vacuous, valid := savedCheck(t, postedCheck), savedCheck(t, versionCheck)
	stored := []v1alpha1.SavedCheck{vacuous, valid}
	acc := &v1alpha1.IntentAcceptance{Outcomes: []v1alpha1.AcceptanceOutcome{o}}
	for _, c := range stored {
		acc.Reviews = append(acc.Reviews, v1alpha1.CheckReviewRecord{CheckDigest: c.Digest,
			OutcomeDigest: outcomeDigest(o), Answer: v1alpha1.ReviewProves, Run: "r1", Check: c.ID, Reason: "yes"})
	}
	fail := &report.Evaluation{Verdicts: []report.OutcomeVerdict{{ID: oVersion, Verdict: report.VerdictFail,
		Ledger: []int32{1}}}}
	for _, tc := range []struct {
		name   string
		vacant v1alpha1.CheckRunResult
		ev     *report.Evaluation
		result acceptance.Result
		basis  string
	}{
		{name: "both green", vacant: v1alpha1.CheckRunPass, result: acceptance.ResultPassed, basis: valid.Digest},
		{name: "the refused one red", vacant: v1alpha1.CheckRunFail, ev: fail, result: acceptance.ResultUnsure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			replay := []v1alpha1.CheckRun{
				{CheckDigest: vacuous.Digest, Result: tc.vacant, Token: "t1", Seq: 1},
				{CheckDigest: valid.Digest, Result: v1alpha1.CheckRunPass, Token: "t2", Seq: 2},
			}
			if tc.vacant == v1alpha1.CheckRunFail {
				replay[0].AssertionStep = 1
			}
			pv := &v1alpha1.PreviewVerification{
				Spec: v1alpha1.PreviewVerificationSpec{Rung: v1alpha1.RungFull, Checks: stored},
				Status: v1alpha1.PreviewVerificationStatus{Replay: replay,
					Ledger: &v1alpha1.LedgerSummary{Count: 5}},
			}
			d := judgeRound(acc, pv, stored, tc.ev).decision
			if got := d.PerOutcome[0]; got.Result != tc.result || tc.basis != "" && got.Basis != tc.basis {
				t.Fatalf("outcome = %+v, want %s on %s", got, tc.result, tc.basis)
			}
			if d.Fix != nil || d.Suspected != nil {
				t.Fatalf("decision = %+v: no fix, no suspected defect", d)
			}
		})
	}
}

// TestRefusedStoredChecksNeverDecide is the compatibility rule as a seeded
// property over the ladder's generated rounds (genLadderRound), some of
// whose stored checks are ones the substance rules now refuse: in a replay
// round and in a full round whose evaluator claims anything, no outcome is
// passed or failed on a refused check, none asks a fix or a suspected
// defect of one, an outcome bound only to refused checks is neither passed
// nor failed, and a round that would start with only refused checks for a
// non-manual outcome is full.
func TestRefusedStoredChecksNeverDecide(t *testing.T) {
	r := rand.New(rand.NewSource(rungPropertySeed))
	refusedSeen := 0
	for n := range 5000 {
		acc, stored, pv := genLadderRound(r)
		refused := map[string]bool{}
		for i := range stored {
			if r.Intn(3) == 0 {
				stored[i].Body, pv.Spec.Checks[i].Body = postedCheck, postedCheck
				refused[stored[i].Digest] = true
			}
		}
		if len(refused) == 0 {
			continue
		}
		refusedSeen++
		if err := refusedDecideNothing(r, acc, stored, pv, refused); err != nil {
			t.Fatalf("case %d (seed %d): %v", n, rungPropertySeed, err)
		}
	}
	if refusedSeen < 1000 {
		t.Fatalf("%d rounds with a refused check: the property is near vacuous", refusedSeen)
	}
}

// refusedDecideNothing holds one generated round to
// TestRefusedStoredChecksNeverDecide.
func refusedDecideNothing(r *rand.Rand, acc *v1alpha1.IntentAcceptance, stored []v1alpha1.SavedCheck,
	pv v1alpha1.PreviewVerification, refused map[string]bool) error {
	onlyRefused := map[string]bool{}
	for _, o := range acc.Outcomes {
		bound := slices.DeleteFunc(slices.Clone(stored), func(c v1alpha1.SavedCheck) bool { return c.Outcome != o.ID })
		onlyRefused[o.ID] = len(bound) > 0 && !slices.ContainsFunc(bound, func(c v1alpha1.SavedCheck) bool {
			return !refused[c.Digest]
		})
	}
	replayPV, fullPV := pv, pv
	replayPV.Spec.Rung, fullPV.Spec.Rung = v1alpha1.RungReplay, v1alpha1.RungFull
	for name, d := range map[string]acceptance.Decision{
		"replay": judgeRound(acc, &replayPV, stored, nil).decision,
		"full":   judgeRound(acc, &fullPV, stored, anyEvaluation(r, acc, stored)).decision,
	} {
		for _, got := range d.PerOutcome {
			decided := got.Result == acceptance.ResultPassed || got.Result == acceptance.ResultFailed
			if decided && (refused[got.Basis] || onlyRefused[got.ID]) {
				return fmt.Errorf("%s round: %s is %s on %s, a refused check", name, got.ID, got.Result, got.Basis)
			}
		}
		for _, f := range []*acceptance.FixRequest{d.Fix, d.Suspected} {
			if f != nil && slices.ContainsFunc(f.Failures, func(x acceptance.FixFailure) bool {
				return refused[x.CheckDigest]
			}) {
				return fmt.Errorf("%s round asks a fix of a refused check: %+v", name, f)
			}
		}
	}
	store := storeInput(acc, stored, "")
	rung, _ := acceptance.StartRung(acceptance.RungInput{HeadSet: "h", OutcomesDigest: acc.OutcomesDigest,
		Model:    &acceptance.ModelRound{HeadSet: "h", OutcomesDigest: acc.OutcomesDigest, Clear: true},
		Outcomes: store.Outcomes, Checks: store.Checks, Reviews: store.Reviews})
	for _, o := range acc.Outcomes {
		if !o.Manual && onlyRefused[o.ID] && rung != acceptance.RungFull {
			return fmt.Errorf("rung %s with %s bound only to refused checks", rung, o.ID)
		}
	}
	return nil
}
