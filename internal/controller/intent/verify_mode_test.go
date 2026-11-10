// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
)

// TestEmptyCriteriaDigest pins the literal to the digest of the empty
// criteria list, however the list is spelled, while that grammar exists.
func TestEmptyCriteriaDigest(t *testing.T) {
	for name, got := range map[string]string{"nil": acceptance.Digest(nil),
		"empty": acceptance.Digest([]acceptance.Criterion{})} {
		if got != emptyCriteriaDigest {
			t.Errorf("acceptance.Digest(%s) = %s, emptyCriteriaDigest %s", name, got, emptyCriteriaDigest)
		}
	}
}

// TestRecordOf: what a record holds, by the data that survives the API
// server pruning status.acceptance.criteria.
func TestRecordOf(t *testing.T) {
	criteria := []v1alpha1.AcceptanceCriterion{{ID: "c", Kind: v1alpha1.AcceptanceManual, Statement: "s"}}
	outcomes := []v1alpha1.AcceptanceOutcome{{ID: "o", Outcome: "x"}}
	other := "sha256:" + strings.Repeat("c", 64)
	for _, tc := range []struct {
		name string
		acc  *v1alpha1.IntentAcceptance
		want recordKind
	}{
		{"none", nil, recordNone},
		{"plan declaring nothing", &v1alpha1.IntentAcceptance{Digest: emptyCriteriaDigest}, recordEmpty},
		{"criteria", &v1alpha1.IntentAcceptance{Digest: other, Criteria: criteria}, recordCriteria},
		{"pruned criteria", &v1alpha1.IntentAcceptance{Digest: other}, recordPrunedCriteria},
		{"outcomes", &v1alpha1.IntentAcceptance{Digest: emptyCriteriaDigest, OutcomesDigest: other,
			Outcomes: outcomes}, recordOutcomes},
		{"no outcomes, outcomes digest", &v1alpha1.IntentAcceptance{Digest: emptyCriteriaDigest,
			OutcomesDigest: other}, recordOutcomes},
		{"both", &v1alpha1.IntentAcceptance{Digest: other, Criteria: criteria, OutcomesDigest: other},
			recordMixed},
	} {
		if got := recordOf(tc.acc); got != tc.want {
			t.Errorf("%s: recordOf = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestVerifyModeHoldsOneEra: over every input, slice 1's mode holds no
// outcomes and the evaluator's no criteria, so a fork inside either on the
// record's own lists (len(acc.Outcomes) > 0) cannot cross eras, and a
// record pruned of its criteria or holding both is never verified.
func TestVerifyModeHoldsOneEra(t *testing.T) {
	for _, verifies := range []bool{true, false} {
		for _, evaluates := range []bool{true, false} {
			for rec := recordNone; rec <= recordMixed; rec++ {
				m := verifyModeOf(verifies, evaluates, rec)
				switch {
				case m == modeCriteria && rec != recordCriteria && rec != recordEmpty,
					m == modeOutcomes && rec != recordOutcomes && rec != recordEmpty,
					(m == modeCriteria || m == modeOutcomes) && !verifies,
					m == modeCriteria && evaluates, m == modeOutcomes && !evaluates:
					t.Errorf("verifyModeOf(%v, %v, %d) = %d", verifies, evaluates, rec, m)
				}
			}
		}
	}
}

// TestNoOutcomesPlanSwitchedOffIsNoSuccess: a plan approved under the
// evaluator that states no outcomes, accepted whole, then the evaluator
// switched off: its record (an outcomes digest, no outcomes) is nothing
// slice 1 verifies, so the intent is Off and its maul/acceptance the
// switched-off error, never the green the acceptance gave it.
func TestNoOutcomesPlanSwitchedOffIsNoSuccess(t *testing.T) {
	e, name := evaluatorReviewing(t, validPlan)
	e.readyPreview(name)
	e.review(name, 3)
	e.gh.comment(approver, "/maul accept all")
	e.settleActions(name)
	e.review(name, 2)
	head := e.get(name).Status.PullRequests[0].HeadSHA
	if st := e.latestAcceptance(head); st.State != statusSuccess {
		t.Fatalf("maul/acceptance accepted whole under the evaluator = %+v, want success", st)
	}
	e.evaluating(false)
	e.review(name, 3)
	if v := e.verification(name); v.State != v1alpha1.VerificationOff {
		t.Errorf("verification = %s, want Off", v.State)
	}
	if st := e.latestAcceptance(head); st.State != statusError || st.Description != switchedOff.description {
		t.Errorf("maul/acceptance with the evaluator off = %+v, want %+v", st, switchedOff)
	}
}

// modeRecord is one record the property puts on the approved revision.
type modeRecord int

const (
	propNone modeRecord = iota
	propCriteria
	propPrunedCriteria
	propOutcomes
)

// wantMode is the property's own truth table (S4 plan, PR 4), written apart
// from verifyModeOf: a record of another revision, or a criteria-era one
// under the evaluator, is read again; a record the evaluator switch
// disagrees with otherwise, or one pruned of its criteria, is a mismatch;
// a matching record of an intent not verified is Off.
func wantMode(verifies, evaluates, current bool, rec modeRecord) verifyMode {
	if !current || rec == propNone {
		return modeNone
	}
	criteriaEra := rec == propCriteria || rec == propPrunedCriteria
	switch {
	case evaluates && criteriaEra:
		return modeNone
	case !evaluates && rec != propCriteria:
		return modeMismatch
	case !verifies:
		return modeOff
	case evaluates:
		return modeOutcomes
	}
	return modeCriteria
}

// TestVerifyModeSeeded is a seeded property over every combination of
// {verifies, evaluates, revision current, record: none, criteria,
// pruned-criteria, outcomes}, each crossed with every verification state,
// the recorded head set at the pull requests' heads or not, and random
// judgements and acceptances: the pass's mode is the truth table's; a
// record nothing verifies (none, Off, mismatch) never gets a success or a
// "maul is checking this" maul/acceptance; and success on the evaluator's
// outcomes needs a judgement (or every outcome accepted by hand) standing
// at the current heads. Slice 1's criteria may still post success until
// they are deleted (S4 PR 6), and only at the current heads.
func TestVerifyModeSeeded(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.review(name, 3)
	base := e.prVerifyPass(name)
	if recordOf(base.in.Status.Acceptance) != recordOutcomes || len(base.in.Status.Acceptance.Outcomes) == 0 {
		t.Fatalf("base record = %+v, want the approved outcomes", base.in.Status.Acceptance)
	}
	states := []v1alpha1.IntentVerificationState{"", v1alpha1.VerificationNotDeclared, v1alpha1.VerificationOff,
		v1alpha1.VerificationWaitingForCI, v1alpha1.VerificationWaitingForPreview, v1alpha1.VerificationVerifying,
		v1alpha1.VerificationPassed, v1alpha1.VerificationFailing, v1alpha1.VerificationFixing,
		v1alpha1.VerificationExhausted, v1alpha1.VerificationNeedsReplan, v1alpha1.VerificationUnavailable,
		v1alpha1.VerificationEvaluating, v1alpha1.VerificationUnconfirmed}
	results := []v1alpha1.OutcomeResultValue{v1alpha1.OutcomePassed, v1alpha1.OutcomeFailed, v1alpha1.OutcomeUnsure,
		v1alpha1.OutcomeQuarantined, v1alpha1.OutcomeAccepted}
	for seed := range int64(8) {
		rng := rand.New(rand.NewSource(seed))
		for _, verifies := range []bool{true, false} {
			for _, evaluates := range []bool{true, false} {
				for _, current := range []bool{true, false} {
					for _, rec := range []modeRecord{propNone, propCriteria, propPrunedCriteria, propOutcomes} {
						for _, state := range states {
							for _, atHeads := range []bool{true, false} {
								c := modeCase{verifies: verifies, evaluates: evaluates, current: current, rec: rec,
									state: state, atHeads: atHeads}
								c.check(t, base, rng, results, fmt.Sprintf("seed %d %+v", seed, c))
							}
						}
					}
				}
			}
		}
	}
}

// modeCase is one combination of TestVerifyModeSeeded.
type modeCase struct {
	verifies, evaluates, current bool
	rec                          modeRecord
	state                        v1alpha1.IntentVerificationState
	atHeads                      bool
}

// check builds c's pass from base, with rng's judgements and acceptances,
// and holds its mode and maul/acceptance to the property.
func (c modeCase) check(t *testing.T, base *pass, rng *rand.Rand, results []v1alpha1.OutcomeResultValue,
	label string) {
	t.Helper()
	p := *base
	p.in, p.proj = base.in.DeepCopy(), base.proj.DeepCopy()
	p.set.Evaluator = c.evaluates
	if !c.verifies {
		// Verification on, but no Preview to verify against: the slot
		// host cannot verify it.
		p.proj.Spec.Preview = nil
		for i := range p.proj.Spec.Repositories {
			p.proj.Spec.Repositories[i].Preview = nil
		}
	}
	if p.verifies() != c.verifies || p.set.evaluates(p.proj) != c.evaluates {
		t.Fatalf("%s: verifies %v, evaluates %v: the setup did not take", label, p.verifies(),
			p.set.evaluates(p.proj))
	}
	acc := c.record(p.in.Status.Acceptance, p.in.Status.Approval.PlanRevision)
	p.in.Status.Acceptance = acc
	heads := "sha256:" + strings.Repeat("d", 64)
	if hs, ok := previewHeadSet(base.proj, base.in); ok && c.atHeads {
		heads = hs.digest
	}
	v := c.verification(rng, acc, heads, results)
	p.in.Status.Verification = v

	want := wantMode(c.verifies, c.evaluates, c.current, c.rec)
	if got := p.mode(); got != want {
		t.Fatalf("%s: mode = %d, want %d", label, got, want)
	}
	st, _, ok := p.acceptanceStatus()
	if !ok {
		return
	}
	switch want {
	case modeNone, modeOff, modeMismatch:
		// Never success, never "maul is checking this", never a pending
		// that waits on a round nothing will run: only the error saying
		// nothing is verified.
		if st.state != statusError {
			t.Fatalf("%s: mode %d posts %+v", label, want, st)
		}
		return
	}
	if st.state != statusSuccess {
		return
	}
	if !p.atHeads(v) {
		t.Fatalf("%s: success %+v at other heads than the pull requests'", label, st)
	}
	if want == modeOutcomes && !outcomesStand(acc, v) {
		t.Fatalf("%s: success %+v with no judgement standing at the heads: %+v", label, st, v)
	}
}

// outcomesStand is a standing judgement of acc's outcomes at v's heads: a
// round's, ready to hand off, or, with nothing to evaluate, every outcome
// manual and accepted by an approver at those heads.
func outcomesStand(acc *v1alpha1.IntentAcceptance, v *v1alpha1.IntentVerification) bool {
	if outcomesReady(acc, v) {
		return true
	}
	return v.State == v1alpha1.VerificationNotDeclared && len(acc.Outcomes) > 0 &&
		!slices.ContainsFunc(acc.Outcomes, func(o v1alpha1.AcceptanceOutcome) bool {
			return !o.Manual || acceptedAt(acc, o.ID, v.HeadSetDigest) == nil
		})
}

// record is c's record: the approved outcomes (as base holds them), a
// criteria record or one pruned of its criteria, each of the approved
// revision or, not current, of the one before.
func (c modeCase) record(outcomes *v1alpha1.IntentAcceptance, revision int32) *v1alpha1.IntentAcceptance {
	var acc *v1alpha1.IntentAcceptance
	criteriaDigest := "sha256:" + strings.Repeat("c", 64)
	switch c.rec {
	case propNone:
		return nil
	case propOutcomes:
		acc = outcomes.DeepCopy()
	case propCriteria:
		acc = &v1alpha1.IntentAcceptance{Digest: criteriaDigest, Criteria: []v1alpha1.AcceptanceCriterion{
			{ID: "sounds-good", Kind: v1alpha1.AcceptanceManual, Statement: "the beat sounds good"},
			{ID: "renders", Kind: v1alpha1.AcceptanceHTTP, Statement: "a render returns audio", Target: "app"}}}
	case propPrunedCriteria:
		acc = &v1alpha1.IntentAcceptance{Digest: criteriaDigest}
	}
	acc.PlanRevision = revision
	if !c.current {
		acc.PlanRevision = revision + 1
	}
	return acc
}

// verification is a verification in c's state at heads, with rng's
// outcome results and counts, and rng's acceptances (all, and of each
// outcome) put on acc at heads or elsewhere.
func (c modeCase) verification(rng *rand.Rand, acc *v1alpha1.IntentAcceptance, heads string,
	results []v1alpha1.OutcomeResultValue) *v1alpha1.IntentVerification {
	v := &v1alpha1.IntentVerification{State: c.state, Round: 1 + rng.Int31n(3), HeadSetDigest: heads,
		Passed: rng.Int31n(3), Failed: rng.Int31n(2), AutoFix: v1alpha1.AutoFix{State: v1alpha1.AutoFixArmed}}
	if acc == nil {
		return v
	}
	elsewhere := "sha256:" + strings.Repeat("e", 64)
	at := func() string {
		if rng.Intn(2) == 0 {
			return heads
		}
		return elsewhere
	}
	now := metav1.Now()
	ids := []string{acceptAll}
	for i := range acc.Outcomes {
		acc.Outcomes[i].Manual = rng.Intn(3) == 0
		ids = append(ids, acc.Outcomes[i].ID)
		v.Outcomes = append(v.Outcomes, v1alpha1.OutcomeResult{ID: acc.Outcomes[i].ID,
			Result: results[rng.Intn(len(results))]})
	}
	for _, c := range acc.Criteria {
		ids = append(ids, c.ID)
	}
	for _, id := range ids {
		if rng.Intn(2) == 0 {
			acc.Accepted = append(acc.Accepted, v1alpha1.AcceptanceRecord{ID: id, By: approver, At: now,
				HeadSetDigest: at()})
		}
	}
	if len(v.Outcomes) > 0 && rng.Intn(4) != 0 {
		v.OutcomesRound, v.OutcomesHeadSetDigest = v.Round, heads
		if rng.Intn(4) != 0 {
			v.OutcomesEnvironment = string(acceptance.EnvOK)
		}
	}
	countManual(v, acc)
	return v
}

// TestPrunedCriteriaRecordSettlesOnce: the pruned criteria record
// TestPrunedCriteriaRecordIsNeverAcceptance settles is a fixed point:
// further passes change neither the verification nor the record, and post
// no maul/acceptance status, with the evaluator off (Off, the switched-off
// error) or on (read again: stopped for a replan, an error).
func TestPrunedCriteriaRecordSettlesOnce(t *testing.T) {
	for _, tc := range []struct {
		name      string
		evaluator bool
		state     v1alpha1.IntentVerificationState
	}{{"evaluator off", false, v1alpha1.VerificationOff}, {"evaluator on", true, v1alpha1.VerificationUnavailable}} {
		t.Run(tc.name, func(t *testing.T) {
			e := newVerifyEnv(t, acceptancePlan)
			name := e.reviewing()
			e.readyPreview(name)
			e.review(name, 3)
			e.observe(e.onlyVerification(name).Name, v1alpha1.VerificationObserved, answering(200))
			e.review(name, 2)
			if tc.evaluator {
				e.evaluatorOn(0.4)
			}
			e.setStatus(name, func(in *v1alpha1.Intent) { in.Status.Acceptance.Criteria = nil })
			e.review(name, 4)
			settled := e.get(name)
			if v := settled.Status.Verification; v == nil || v.State != tc.state {
				t.Fatalf("verification = %+v, want %s", v, tc.state)
			}
			head := settled.Status.PullRequests[0].HeadSHA
			before := e.acceptanceStatuses(head) // newest first
			if len(before) == 0 || before[0].State != statusError {
				t.Fatalf("maul/acceptance = %+v, want an error", before)
			}
			e.review(name, 3)
			in := e.get(name)
			if !equality.Semantic.DeepEqual(in.Status.Verification, settled.Status.Verification) ||
				!equality.Semantic.DeepEqual(in.Status.Acceptance, settled.Status.Acceptance) {
				t.Errorf("record moved after settling:\nverification %+v\nwas %+v\nacceptance %+v\nwas %+v",
					in.Status.Verification, settled.Status.Verification, in.Status.Acceptance,
					settled.Status.Acceptance)
			}
			if after := e.acceptanceStatuses(head); len(after) != len(before) {
				t.Errorf("maul/acceptance posted again after settling: %+v", after[:len(after)-len(before)])
			}
		})
	}
}
