// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/agentrun"
)

// TestGreenProvenIsDecidesRule pins greenProven, which names a red check to
// the evaluator as not yet proof, to acceptance.Decide: on a replay round
// where the stored red check, cleared a round before, replays green alone,
// its outcome passes exactly when greenProven holds for its reviews.
func TestGreenProvenIsDecidesRule(t *testing.T) {
	red := redCheck(t)
	o := v1alpha1.AcceptanceOutcome{ID: red.Outcome, Outcome: "GET /version answers with the commit it runs"}
	od := outcomeDigest(o)
	redReview := v1alpha1.CheckReviewRecord{CheckDigest: red.Digest, OutcomeDigest: od, Answer: v1alpha1.ReviewProves,
		Run: "r1", Expect: v1alpha1.CheckExpectFail, FailedStep: red.FailedStep, FailedAssertion: red.FailedAssertion}
	green := func(a v1alpha1.ReviewAnswer, outcome string) v1alpha1.CheckReviewRecord {
		return v1alpha1.CheckReviewRecord{CheckDigest: red.Digest, OutcomeDigest: outcome, Answer: a, Run: "r2"}
	}
	for name, reviews := range map[string][]v1alpha1.CheckReviewRecord{
		"red only":            {redReview},
		"green proves":        {redReview, green(v1alpha1.ReviewProves, od)},
		"green refused":       {redReview, green(v1alpha1.ReviewDoesNotProve, od)},
		"green unclear":       {redReview, green(v1alpha1.ReviewUnclear, od)},
		"green, older text":   {redReview, green(v1alpha1.ReviewProves, digest([]byte("older")))},
		"proves then refused": {redReview, green(v1alpha1.ReviewProves, od), green(v1alpha1.ReviewDoesNotProve, od)},
	} {
		t.Run(name, func(t *testing.T) {
			acc := &v1alpha1.IntentAcceptance{Outcomes: []v1alpha1.AcceptanceOutcome{o}, Reviews: reviews,
				RedCleared: []v1alpha1.RedClearance{{Digest: red.Digest, Verification: "x-v4-a1"}}}
			pv := &v1alpha1.PreviewVerification{ObjectMeta: metav1.ObjectMeta{Name: "x-v5-a1"},
				Spec: v1alpha1.PreviewVerificationSpec{Round: 5, Rung: v1alpha1.RungReplay},
				Status: v1alpha1.PreviewVerificationStatus{Replay: []v1alpha1.CheckRun{{CheckDigest: red.Digest,
					Result: v1alpha1.CheckRunPass, Token: "t", Seq: 1}}}}
			d := acceptance.Decide(decideInput(acc, pv, []v1alpha1.SavedCheck{red}, nil))
			passed := d.PerOutcome[0].Result == acceptance.ResultPassed
			if name == "green proves" && !passed {
				t.Fatalf("Decide passes nothing here (%+v): the pin is vacuous", d.PerOutcome[0])
			}
			if want := greenProven(reviews, red.Digest, od); passed != want {
				t.Errorf("Decide passed %v (%+v), greenProven %v", passed, d.PerOutcome[0], want)
			}
		})
	}
}

// TestEvaluateHandoffNamesRedChecksNotYetProof (gate D-B9): the evaluator is
// told which stored checks saved red are not yet proof their outcome holds,
// by the rule a pass is judged by: no review of the green question answered
// proves for the outcome's current text, or one refused. A red check proven
// green, and a check saved green, are not named.
func TestEvaluateHandoffNamesRedChecksNotYetProof(t *testing.T) {
	red, green := redCheck(t), savedCheck(t, healthCheck)
	for _, tt := range []struct {
		name    string
		reviews func(o string) []v1alpha1.CheckReviewRecord
		named   bool
	}{
		{"reviewed red only", func(string) []v1alpha1.CheckReviewRecord { return nil }, true},
		{"refused as green proof", func(o string) []v1alpha1.CheckReviewRecord {
			return []v1alpha1.CheckReviewRecord{{CheckDigest: red.Digest, OutcomeDigest: o,
				Answer: v1alpha1.ReviewDoesNotProve, Run: "r5", Check: red.ID}}
		}, true},
		{"green proof for an older text", func(string) []v1alpha1.CheckReviewRecord {
			return []v1alpha1.CheckReviewRecord{{CheckDigest: red.Digest, OutcomeDigest: digest([]byte("older")),
				Answer: v1alpha1.ReviewProves, Run: "r5", Check: red.ID}}
		}, true},
		{"proven green", func(o string) []v1alpha1.CheckReviewRecord {
			return []v1alpha1.CheckReviewRecord{{CheckDigest: red.Digest, OutcomeDigest: o,
				Answer: v1alpha1.ReviewProves, Run: "r5", Check: red.ID}}
		}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e, name := newEvaluatorEnv(t, 0.4)
			e.storeChecks(name, red, green)
			text := map[string]string{}
			for _, o := range e.get(name).Status.Acceptance.Outcomes {
				text[o.ID] = outcomeDigest(o)
			}
			e.setStatus(name, func(in *v1alpha1.Intent) {
				rv := in.Status.Acceptance.Reviews
				for i := range rv {
					if rv[i].CheckDigest == red.Digest {
						rv[i].Expect, rv[i].FailedStep, rv[i].FailedAssertion = v1alpha1.CheckExpectFail,
							red.FailedStep, red.FailedAssertion
					}
				}
				in.Status.Acceptance.Reviews = append(rv, tt.reviews(text[red.Outcome])...)
			})
			e.review(name, 3)
			pv := e.onlyVerification(name)
			h := decodeEvaluateHandoff(t, mustHandoff(t, e.prVerifyPass(name), &pv))
			want := []agentrun.NotYetProof(nil)
			if tt.named {
				want = []agentrun.NotYetProof{{Outcome: red.Outcome, Check: red.ID}}
			}
			if !slices.Equal(h.NotYetProof, want) {
				t.Errorf("not yet proof = %+v, want %+v", h.NotYetProof, want)
			}
		})
	}
}

// versionStrongCheck is a stronger check of version-shown than versionCheck:
// the one a later round proposes once the check saved red is refused as
// proof of the pass.
const versionStrongCheck = `{"id":"version-shown-strong","outcome":"version-shown","form":"http","http":{
 "target":"app","steps":[
 {"request":{"method":"POST","path":"/notes","json":{"name":"{{run}}-s"}},"expect":{"status":201}},
 {"request":{"path":"/version?q={{run}}"},"expect":{"json":{"/note":"{{run}}-s","/commit":{"$present":true}}}}]}}`

// TestRedCheckWeakAsGreenRePassesOnANewCheck (gate D-B9): a check saved red
// that replays green after the fix, and is refused as proof of the pass
// (the green question answered does_not_prove), leaves its outcome to a new
// check proposed the same round. That outcome passes only on the new check,
// confirmed clean and reviewed proves; refused or absent, it stays unsure.
func TestRedCheckWeakAsGreenRePassesOnANewCheck(t *testing.T) {
	cases := []struct {
		name    string
		propose bool
		strong  v1alpha1.ReviewAnswer
		passed  bool
	}{
		{name: "a new check reviewed proves passes it", propose: true, strong: v1alpha1.ReviewProves, passed: true},
		{name: "a new check refused leaves it unsure", propose: true, strong: v1alpha1.ReviewDoesNotProve},
		{name: "no new check leaves it unsure"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, name := newEvaluatorEnv(t, 0.4)
			red, strong := redCheck(t), savedCheck(t, versionStrongCheck)
			e.storeChecks(name, red)
			e.setStatus(name, func(in *v1alpha1.Intent) {
				rv := in.Status.Acceptance.Reviews
				for i := range rv {
					rv[i].Expect, rv[i].FailedStep, rv[i].FailedAssertion = v1alpha1.CheckExpectFail,
						red.FailedStep, red.FailedAssertion
				}
			})
			e.reviewChecksWith(&fakeReviewer{answers: map[string]v1alpha1.ReviewAnswer{
				red.Digest: v1alpha1.ReviewDoesNotProve, strong.Digest: tc.strong}})
			e.evaluateReporting(`---
verdicts:
  - outcome: version-shown
    verdict: pass
    ledger: [1, 10]
  - outcome: still-healthy
    verdict: unsure
    ledger: []
---
Fixed.
`)
			e.review(name, 3)
			pv := e.onlyVerification(name)
			e.playTools(pv.Name, v1alpha1.VerificationRunning, toolPodIP, true)
			e.review(name, 1)
			e.runRuns()
			e.runRuns()
			e.replayed(pv.Name, v1alpha1.CheckRun{CheckDigest: red.Digest, Result: v1alpha1.CheckRunPass,
				Token: "t1", Seq: 1})
			if tc.propose {
				cur := e.pvNamed(pv.Name)
				cur.Status.Confirmed = []v1alpha1.SavedCheck{strong}
				cur.Status.Confirmations = []v1alpha1.CheckRun{{CheckDigest: strong.Digest,
					Result: v1alpha1.CheckRunPass, Token: "cf-tok", Seq: 10, DryRunSeq: 4}}
				if err := e.c.Update(context.Background(), cur); err != nil {
					t.Fatal(err)
				}
			}
			e.settleReviews(name)
			v := e.verification(name)
			got := v.Outcomes[0]
			switch {
			case tc.passed && (got.Result != v1alpha1.OutcomePassed || got.Basis != strong.Digest):
				t.Errorf("verification %s %+v, want version-shown passed on the new check", v.State, v.Outcomes)
			case !tc.passed && got.Result == v1alpha1.OutcomePassed:
				t.Errorf("verification %s %+v, want version-shown not passed", v.State, v.Outcomes)
			}
		})
	}
}
