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

// rungPropertySeed is the controller-level ladder property's seed, so the
// gate is deterministic.
const rungPropertySeed = 20261111

// genLadderRound is one round's evidence as the Intent and its
// PreviewVerification hold it: the approved outcomes, the store, its
// reviews, narrowing and clearance records, the round's replay runs, an
// agents round's base, and the environment.
func genLadderRound(r *rand.Rand) (*v1alpha1.IntentAcceptance, []v1alpha1.SavedCheck, v1alpha1.PreviewVerification) {
	acc := &v1alpha1.IntentAcceptance{OutcomesDigest: "sha256:" + fmt.Sprintf("%064x", r.Int63())}
	ids := []string{"o-one", "o-two", "o-three"}[:1+r.Intn(3)]
	for _, id := range ids {
		o := v1alpha1.AcceptanceOutcome{ID: id, Outcome: "the app does " + id, Manual: r.Intn(8) == 0}
		if r.Intn(3) == 0 {
			o.Kind = v1alpha1.OutcomeKindKeep
		}
		acc.Outcomes = append(acc.Outcomes, o)
	}
	forms := []v1alpha1.CheckForm{v1alpha1.CheckFormHTTP, v1alpha1.CheckFormBrowser, v1alpha1.CheckFormExec,
		v1alpha1.CheckFormRun}
	stored := make([]v1alpha1.SavedCheck, 0, 5)
	for i := range 1 + r.Intn(5) {
		o := acc.Outcomes[r.Intn(len(acc.Outcomes))]
		c := v1alpha1.SavedCheck{ID: fmt.Sprintf("c%d", i), Outcome: o.ID, Form: forms[r.Intn(len(forms))],
			Body: `{"id":"c"}`, Digest: "sha256:" + fmt.Sprintf("%064x", i+1)}
		if r.Intn(5) == 0 {
			c.Expect, c.FailedStep, c.FailedAssertion = v1alpha1.CheckExpectFail, int32(1+r.Intn(2)), "json /a"
		}
		stored = append(stored, c)
		if r.Intn(5) != 0 {
			answer := v1alpha1.ReviewProves
			if r.Intn(6) == 0 {
				answer = v1alpha1.ReviewUnclear
			}
			acc.Reviews = append(acc.Reviews, v1alpha1.CheckReviewRecord{CheckDigest: c.Digest,
				OutcomeDigest: outcomeDigest(o), Answer: answer})
		}
		if r.Intn(8) == 0 {
			acc.Narrowed = append(acc.Narrowed, v1alpha1.NarrowedCheck{Digest: c.Digest, Verification: "earlier"})
		}
		if c.Expect == v1alpha1.CheckExpectFail && r.Intn(2) == 0 {
			acc.RedCleared = append(acc.RedCleared, v1alpha1.RedClearance{Digest: c.Digest, Verification: "earlier"})
		}
	}
	pv := v1alpha1.PreviewVerification{}
	pv.Name = "target-1-v2-a1"
	pv.Spec.Checks = slices.Clone(stored)
	pv.Status.Ledger = &v1alpha1.LedgerSummary{Count: 30}
	results := []v1alpha1.CheckRunResult{v1alpha1.CheckRunPass, v1alpha1.CheckRunPass, v1alpha1.CheckRunPass,
		v1alpha1.CheckRunFail, v1alpha1.CheckRunError, v1alpha1.CheckRunQuarantined}
	for i, c := range stored {
		if r.Intn(8) == 0 {
			continue // cut short before it (VC-3)
		}
		run := v1alpha1.CheckRun{CheckDigest: c.Digest, Result: results[r.Intn(len(results))], Token: "tok",
			Seq: int32(1 + i)}
		if run.Result == v1alpha1.CheckRunFail && r.Intn(2) == 0 {
			run.AssertionStep, run.AssertionID = c.FailedStep, "json /a"
		}
		pv.Status.Replay = append(pv.Status.Replay, run)
	}
	if r.Intn(3) == 0 {
		pv.Spec.Placement = v1alpha1.PlacementAgents
		pv.Status.Base = &v1alpha1.BaseStatus{Unbuildable: r.Intn(6) == 0}
		for i, c := range stored {
			if r.Intn(4) == 0 {
				continue
			}
			run := v1alpha1.CheckRun{CheckDigest: c.Digest, Result: results[r.Intn(len(results))], Token: "tok-b",
				Seq: int32(20 + i), Tree: v1alpha1.TreeBase}
			if run.Result == v1alpha1.CheckRunFail && r.Intn(3) != 0 {
				run.AssertionStep, run.AssertionID = 1, "json /a"
			}
			pv.Status.Base.Runs = append(pv.Status.Base.Runs, run)
		}
	}
	if r.Intn(8) == 0 {
		pv.Status.Environment = []v1alpha1.PartFact{{Name: "app", Role: v1alpha1.PreviewRoleComponent, Ready: true,
			CrashedDuring: true, Restarts: 1}}
	}
	return acc, stored, pv
}

// quietEvaluation is a full round's evaluator that raises nothing: a pass
// or unsure verdict on most outcomes, citing the ledger or not, and
// sometimes an anomaly (which downgrades only the hand-off).
func quietEvaluation(r *rand.Rand, acc *v1alpha1.IntentAcceptance) *report.Evaluation {
	ev := &report.Evaluation{}
	for _, o := range acc.Outcomes {
		if r.Intn(4) == 0 {
			continue
		}
		v := report.OutcomeVerdict{ID: o.ID, Outcome: o.ID, Verdict: report.VerdictPass}
		if r.Intn(3) == 0 {
			v.Verdict = report.VerdictUnsure
		}
		if r.Intn(3) != 0 {
			v.Ledger = []int32{int32(1 + r.Intn(10))}
		}
		ev.Verdicts = append(ev.Verdicts, v)
	}
	return ev
}

// anyEvaluation is a full round's evaluator that may claim anything: fail
// verdicts, contrary entries, disputes and anomalies.
func anyEvaluation(r *rand.Rand, acc *v1alpha1.IntentAcceptance, stored []v1alpha1.SavedCheck) *report.Evaluation {
	ev := quietEvaluation(r, acc)
	for i := range ev.Verdicts {
		switch r.Intn(4) {
		case 0:
			ev.Verdicts[i].Verdict, ev.Verdicts[i].Ledger = report.VerdictFail, []int32{int32(1 + r.Intn(10))}
		case 1:
			ev.Verdicts[i].Contrary = []int32{int32(1 + r.Intn(10))}
		}
	}
	if len(stored) > 0 && r.Intn(3) == 0 {
		ev.Disputes = append(ev.Disputes, report.Dispute{CheckDigest: stored[r.Intn(len(stored))].Digest,
			Reason: "stale", Ledger: []int32{1}})
	}
	if r.Intn(3) == 0 {
		ev.Anomalies = append(ev.Anomalies, report.Anomaly{Summary: "a console error", Route: "/", Ledger: []int32{2}})
	}
	return ev
}

// TestReplayRoundJudgedAsAFullRoundWithTheSameEvidence is the replay
// rung's safety, through the controller's own judgement (judgeRound,
// decideInput) on generated rounds that StartRung would replay (a clear
// model round at the same heads and outcomes, every outcome covered):
//
//   - zero false passes: each outcome's result is the one a full round
//     with the same evidence gives when its evaluator raises nothing, and
//     a full round whose evaluator claims anything passes nothing the
//     replay round does not;
//   - zero false fails: a replay round never fails an outcome, never asks
//     for a fix, a suspected defect, a retirement or a narrowing, and its
//     judgement routes to Passed or Unconfirmed, never Failing.
func TestReplayRoundJudgedAsAFullRoundWithTheSameEvidence(t *testing.T) {
	r := rand.New(rand.NewSource(rungPropertySeed))
	var tally ladderTally
	for n := range 20000 {
		acc, stored, pv := genLadderRound(r)
		store := storeInput(acc, stored, "")
		rung, _ := acceptance.StartRung(acceptance.RungInput{HeadSet: "h", OutcomesDigest: acc.OutcomesDigest,
			Model:    &acceptance.ModelRound{HeadSet: "h", OutcomesDigest: acc.OutcomesDigest, Clear: true},
			Outcomes: store.Outcomes, Checks: store.Checks, Reviews: store.Reviews})
		if rung != acceptance.RungReplay {
			continue
		}
		tally.replays++
		if err := checkLadderRound(r, acc, stored, pv, &tally); err != nil {
			t.Fatalf("case %d (seed %d): %v", n, rungPropertySeed, err)
		}
	}
	if tally.replays < 500 || tally.passes < 200 || tally.unsure < 200 {
		t.Fatalf("%+v: the property is near vacuous", tally)
	}
}

// ladderTally counts what the property judged, so it is never vacuous.
type ladderTally struct{ replays, passes, unsure int }

// checkLadderRound judges pv three ways (a replay round, a full round
// whose evaluator raises nothing, a full round whose evaluator claims
// anything) and holds the replay round's judgement to the property.
func checkLadderRound(r *rand.Rand, acc *v1alpha1.IntentAcceptance, stored []v1alpha1.SavedCheck,
	pv v1alpha1.PreviewVerification, tally *ladderTally) error {
	replayPV, fullPV := pv, pv
	replayPV.Spec.Rung, fullPV.Spec.Rung = v1alpha1.RungReplay, v1alpha1.RungFull
	replay := judgeRound(acc, &replayPV, stored, nil)
	quiet := judgeRound(acc, &fullPV, stored, quietEvaluation(r, acc))
	anyClaims := judgeRound(acc, &fullPV, stored, anyEvaluation(r, acc, stored))
	d := replay.decision
	if d.Fix != nil || d.Suspected != nil || d.ReplayRound || len(d.Retire) > 0 || len(d.Narrowed) > 0 {
		return fmt.Errorf("a replay round decided %+v", d)
	}
	for i, got := range d.PerOutcome {
		q, a := quiet.decision.PerOutcome[i], anyClaims.decision.PerOutcome[i]
		switch {
		case got.Result == acceptance.ResultFailed:
			return fmt.Errorf("the replay round failed %s (%s)", got.ID, got.Reason)
		case got.Result != q.Result:
			return fmt.Errorf("%s is %s (%s) on replay, %s (%s) in a quiet full round", got.ID, got.Result,
				got.Reason, q.Result, q.Reason)
		case a.Result == acceptance.ResultPassed && got.Result != acceptance.ResultPassed:
			return fmt.Errorf("a full round's claims passed %s, the replay round did not (%s)", got.ID, got.Reason)
		case got.Result == acceptance.ResultPassed:
			tally.passes++
		case got.Result == acceptance.ResultUnsure:
			tally.unsure++
		}
	}
	in := &v1alpha1.Intent{}
	in.Status.Acceptance = acc.DeepCopy()
	in.Status.Verification = &v1alpha1.IntentVerification{Round: 2,
		HeadSetDigest: "sha256:" + fmt.Sprintf("%064x", 7), Rung: v1alpha1.RungReplay}
	applyJudgement(in, replay, pv.CreationTimestamp.Time)
	if s := in.Status.Verification.State; s != v1alpha1.VerificationPassed && s != v1alpha1.VerificationUnconfirmed {
		return fmt.Errorf("a replay round routed to %s", s)
	}
	return nil
}
