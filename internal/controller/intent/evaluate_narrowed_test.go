// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"slices"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/report"
)

// narrowedRound is bug 13's review shape: in a full round the evaluator saw
// outcome x fail as worded (contrary seqs) while its new, narrowed check
// passed and was reviewed proves; outcome y's stored check failed and is
// disputed with a confirmed, proven replacement, so a replay round follows
// at the same heads.
type narrowedRound struct {
	acc                *v1alpha1.IntentAcceptance
	narrow, old, repl  v1alpha1.SavedCheck
	stored             []v1alpha1.SavedCheck
	full               *v1alpha1.PreviewVerification
	ev                 *report.Evaluation
	headSet            string
	outcomeX, outcomeY v1alpha1.AcceptanceOutcome
}

func newNarrowedRound() narrowedRound {
	check := func(id, outcome, digit string) v1alpha1.SavedCheck {
		return v1alpha1.SavedCheck{ID: id, Outcome: outcome, Form: v1alpha1.CheckFormHTTP, Body: `{}`,
			Digest: "sha256:" + strings.Repeat(digit, 64)}
	}
	r := narrowedRound{
		outcomeX: v1alpha1.AcceptanceOutcome{ID: "round-trip", Outcome: "Posting back the exact body GET returned gives 201",
			Kind: v1alpha1.OutcomeKindKeep},
		outcomeY: v1alpha1.AcceptanceOutcome{ID: "version-shown", Outcome: "The version is shown"},
		headSet:  "sha256:" + strings.Repeat("9", 64),
	}
	r.narrow, r.old, r.repl = check("rt-narrow", r.outcomeX.ID, "a"), check("v-old", r.outcomeY.ID, "b"),
		check("v-new", r.outcomeY.ID, "c")
	r.stored = []v1alpha1.SavedCheck{r.old, r.narrow, r.repl}
	r.acc = &v1alpha1.IntentAcceptance{Outcomes: []v1alpha1.AcceptanceOutcome{r.outcomeX, r.outcomeY}}
	for _, c := range r.stored {
		o := r.outcomeX
		if c.Outcome == r.outcomeY.ID {
			o = r.outcomeY
		}
		r.acc.Reviews = append(r.acc.Reviews, v1alpha1.CheckReviewRecord{CheckDigest: c.Digest,
			OutcomeDigest: outcomeDigest(o), Answer: v1alpha1.ReviewProves, Run: "x-cr-r0-a1"})
	}
	r.full = &v1alpha1.PreviewVerification{
		Spec: v1alpha1.PreviewVerificationSpec{Rung: v1alpha1.RungFull, HeadSetDigest: r.headSet,
			Checks: []v1alpha1.SavedCheck{r.old}},
		Status: v1alpha1.PreviewVerificationStatus{
			Ledger:    &v1alpha1.LedgerSummary{Count: 20},
			Confirmed: []v1alpha1.SavedCheck{r.narrow, r.repl},
			Replay:    []v1alpha1.CheckRun{{CheckDigest: r.old.Digest, Result: v1alpha1.CheckRunFail, Token: "t1", Seq: 2}},
			Confirmations: []v1alpha1.CheckRun{
				{CheckDigest: r.narrow.Digest, Result: v1alpha1.CheckRunPass, Token: "t2", Seq: 5},
				{CheckDigest: r.repl.Digest, Result: v1alpha1.CheckRunPass, Token: "t3", Seq: 6},
			},
		},
	}
	r.full.Name = "x-pv-1"
	r.ev = &report.Evaluation{
		Verdicts: []report.OutcomeVerdict{
			{ID: r.outcomeX.ID, Verdict: report.VerdictPass, Ledger: []int32{5}, Contrary: []int32{3, 4}},
			{ID: r.outcomeY.ID, Verdict: report.VerdictPass, Ledger: []int32{6}},
		},
		Proposed: []report.ProposedCheck{{ID: r.narrow.ID, Outcome: r.outcomeX.ID, Digest: r.narrow.Digest},
			{ID: r.repl.ID, Outcome: r.outcomeY.ID, Digest: r.repl.Digest}},
		Disputes: []report.Dispute{{CheckDigest: r.old.Digest, Reason: "the old check read a stale field",
			Replacement: r.repl.ID, Ledger: []int32{2}}},
	}
	return r
}

// replayOf is the replay round after r's full round: the store less the
// retired disputed check, every check replayed clean, no evaluator.
func (r narrowedRound) replayOf(name string) *v1alpha1.PreviewVerification {
	pv := &v1alpha1.PreviewVerification{
		Spec: v1alpha1.PreviewVerificationSpec{Rung: v1alpha1.RungReplay, HeadSetDigest: r.headSet,
			Checks: []v1alpha1.SavedCheck{r.narrow, r.repl}},
		Status: v1alpha1.PreviewVerificationStatus{Ledger: &v1alpha1.LedgerSummary{Count: 2},
			Replay: []v1alpha1.CheckRun{
				{CheckDigest: r.narrow.Digest, Result: v1alpha1.CheckRunPass, Token: "t4", Seq: 1},
				{CheckDigest: r.repl.Digest, Result: v1alpha1.CheckRunPass, Token: "t5", Seq: 2},
			}},
	}
	pv.Name = name
	return pv
}

func (r narrowedRound) intent() *v1alpha1.Intent {
	in := &v1alpha1.Intent{}
	in.Name = "x"
	in.Status.Acceptance = r.acc
	in.Status.Verification = &v1alpha1.IntentVerification{HeadSetDigest: r.headSet}
	return in
}

// TestNarrowedCheckNeverPassesItsOutcomeLater (bug 13 review, findings 1
// and 2): the full round withholds x (the evaluator saw it fail) and records
// its passing check as narrowed; the evaluator-free replay round at the same
// heads, and a later full round whose evaluator says nothing contrary, then
// never pass x on that check, while y passes on its replacement.
func TestNarrowedCheckNeverPassesItsOutcomeLater(t *testing.T) {
	r := newNarrowedRound()
	in := r.intent()
	now := time.Now()

	j := judgeRound(in.Status.Acceptance, r.full, r.stored, r.ev)
	applyJudgement(in, j, now)
	v := in.Status.Verification
	if v.State != v1alpha1.VerificationUnconfirmed || !slices.Equal(v.Disputed, []string{r.old.Digest}) {
		t.Fatalf("full round: state %s, disputed %v; want Unconfirmed with a replay due for %s", v.State,
			v.Disputed, r.old.Digest)
	}
	if got := in.Status.Acceptance.Narrowed; len(got) != 1 || got[0].Digest != r.narrow.Digest ||
		got[0].Verification != r.full.Name {
		t.Fatalf("narrowed = %+v, want %s recorded by %s", got, r.narrow.Digest, r.full.Name)
	}

	stored := []v1alpha1.SavedCheck{r.narrow, r.repl}
	j = judgeRound(in.Status.Acceptance, r.replayOf("x-pv-2"), stored, nil)
	applyJudgement(in, j, now)
	checkNarrowedOutcomes(t, "replay round", in)

	later := r.replayOf("x-pv-3")
	later.Spec.Rung, later.Spec.HeadSetDigest = v1alpha1.RungFull, "sha256:"+strings.Repeat("8", 64)
	in.Status.Verification.HeadSetDigest = later.Spec.HeadSetDigest
	ev := &report.Evaluation{Verdicts: []report.OutcomeVerdict{
		{ID: r.outcomeX.ID, Verdict: report.VerdictPass, Ledger: []int32{1}},
		{ID: r.outcomeY.ID, Verdict: report.VerdictPass, Ledger: []int32{2}},
	}}
	j = judgeRound(in.Status.Acceptance, later, stored, ev)
	applyJudgement(in, j, now)
	checkNarrowedOutcomes(t, "later round", in)
}

func checkNarrowedOutcomes(t *testing.T, round string, in *v1alpha1.Intent) {
	t.Helper()
	v := in.Status.Verification
	if v.State == v1alpha1.VerificationPassed || len(v.Outcomes) != 2 {
		t.Fatalf("%s: state %s, outcomes %+v; want x left unconfirmed", round, v.State, v.Outcomes)
	}
	if x := v.Outcomes[0]; x.Result != v1alpha1.OutcomeUnsure || x.Reason != acceptance.ReasonEvaluatorSawFailure {
		t.Errorf("%s: x = %+v, want unsure, %s", round, x, acceptance.ReasonEvaluatorSawFailure)
	}
	if y := v.Outcomes[1]; y.Result != v1alpha1.OutcomePassed {
		t.Errorf("%s: y = %+v, want passed on its replacement", round, y)
	}
}

// TestAddNarrowed: each digest once (the first record kept), records of
// checks the store no longer holds dropped, the newest MaxSavedChecks.
func TestAddNarrowed(t *testing.T) {
	d := func(i int) string {
		return "sha256:" + strings.Repeat(string(rune('a'+i%6)), 63) + string(rune('0'+i/6))
	}
	acc := &v1alpha1.IntentAcceptance{}
	addNarrowed(acc, []string{d(0), d(1)}, "pv-1", nil)
	addNarrowed(acc, []string{d(1), d(2)}, "pv-2", nil)
	if len(acc.Narrowed) != 3 || acc.Narrowed[1].Verification != "pv-1" || acc.Narrowed[2].Verification != "pv-2" {
		t.Fatalf("narrowed = %+v, want three, the first record of each kept", acc.Narrowed)
	}
	addNarrowed(acc, nil, "pv-3", []v1alpha1.SavedCheck{{Digest: d(2)}, {Digest: d(0)}})
	if len(acc.Narrowed) != 2 || acc.Narrowed[0].Digest != d(0) || acc.Narrowed[1].Digest != d(2) {
		t.Fatalf("narrowed = %+v, want those the store still holds", acc.Narrowed)
	}
	many := make([]string, 0, v1alpha1.MaxSavedChecks+5)
	for i := range v1alpha1.MaxSavedChecks + 5 {
		many = append(many, d(i+10))
	}
	addNarrowed(acc, many, "pv-4", nil)
	if n := len(acc.Narrowed); n != v1alpha1.MaxSavedChecks || acc.Narrowed[n-1].Digest != many[len(many)-1] {
		t.Errorf("narrowed = %d records, want the newest %d", n, v1alpha1.MaxSavedChecks)
	}
}
