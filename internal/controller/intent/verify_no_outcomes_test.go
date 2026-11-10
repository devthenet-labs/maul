// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/action"
	"github.com/devthenet-labs/maul/internal/command"
	"github.com/devthenet-labs/maul/internal/templates"
)

// The edges S4 PR 6 left to the outcomes era once slice 1's criteria went:
// a plan that declares no outcome, accepted whole or not at all, and a
// criteria round in flight when the evaluator stops it for a replan.

// TestAcceptOnAPlanDeclaringNoOutcome: under the evaluator a plan that
// declares no outcome takes only /maul accept all, the approver's word that
// the change needs none, bound to the recorded heads: any other id is
// unknown, nothing is accepted before the heads are recorded or while the
// evaluator does not verify the Project, and the reply says the plan
// declares nothing.
func TestAcceptOnAPlanDeclaringNoOutcome(t *testing.T) {
	for _, tc := range []struct {
		name  string
		id    string
		setup func(e *env, p *pass)
		want  templates.VerifyOutcome
	}{
		{"another id", "health", nil, templates.VerifyAcceptUnknown},
		{"before the heads are recorded", command.AcceptAll, func(_ *env, p *pass) {
			p.in.Status.Verification.HeadSetDigest = ""
		}, templates.VerifyAcceptNotYet},
		{"the evaluator does not verify the Project", command.AcceptAll, func(_ *env, p *pass) {
			p.set.Evaluator = false
		}, templates.VerifyAcceptNotYet},
		{"all", command.AcceptAll, nil, templates.VerifyAccepted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, name := evaluatorReviewing(t, validPlan)
			e.readyPreview(name)
			e.review(name, 3)
			if v := e.verification(name); v.State != v1alpha1.VerificationNotDeclared || v.HeadSetDigest == "" {
				t.Fatalf("verification = %+v, want NotDeclared at recorded heads", v)
			}
			p := e.prVerifyPass(name)
			if tc.setup != nil {
				tc.setup(e, p)
			}
			c := verifyCommand{surface: command.IntentIssue, key: "verify-1", id: 1, at: e.clock.Now(),
				actor: actorOf(approver), verb: action.VerbAccept, body: "/maul accept " + tc.id}
			n, err := p.acceptCriterion(context.Background(), c, tc.id, templates.VerifyCommandNotice{})
			if err != nil {
				t.Fatal(err)
			}
			if n.Outcome != tc.want || n.NoCriteria != (tc.want == templates.VerifyAccepted) {
				t.Fatalf("outcome %q (no criteria %v), want %q", n.Outcome, n.NoCriteria, tc.want)
			}
			acc := e.get(name).Status.Acceptance
			if accepted := len(acc.Accepted) > 0; accepted != (tc.want == templates.VerifyAccepted) {
				t.Errorf("accepted = %+v", acc.Accepted)
			}
		})
	}
}

// TestCountManualWithNothingOnRecord: with no acceptance on record nothing
// is counted, whatever an earlier count said.
func TestCountManualWithNothingOnRecord(t *testing.T) {
	v := &v1alpha1.IntentVerification{ManualOpen: 2, ManualAccepted: 1, NotRun: 3}
	countManual(v, nil)
	if v.ManualOpen != 0 || v.ManualAccepted != 0 || v.NotRun != 0 {
		t.Errorf("counts = open %d accepted %d not run %d, want none", v.ManualOpen, v.ManualAccepted, v.NotRun)
	}
}

// TestReplanDropsTheRoundInFlight: a criteria round a release before S4
// left in flight when the evaluator comes to verify the Project is deleted
// as the intent stops for a replan: nothing reads its result.
func TestReplanDropsTheRoundInFlight(t *testing.T) {
	e := newVerifyEnv(t, acceptancePlan)
	name := e.reviewing()
	e.readyPreview(name)
	e.review(name, 2)
	in := e.get(name)
	pv := &v1alpha1.PreviewVerification{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.PreviewVerificationName(name, 1, 1), Namespace: testNS,
			Labels:          map[string]string{v1alpha1.LabelIntent: name},
			OwnerReferences: []metav1.OwnerReference{intentOwner(in)}},
		Spec: v1alpha1.PreviewVerificationSpec{IntentRef: v1alpha1.ObjectReference{Name: name, UID: in.UID},
			HeadSetDigest: in.Status.Verification.HeadSetDigest, Round: 1, Attempt: 1, Run: "abc123",
			CriteriaDigest: in.Status.Acceptance.Digest},
	}
	if err := e.c.Create(context.Background(), pv); err != nil {
		t.Fatal(err)
	}
	e.setStatus(name, func(in *v1alpha1.Intent) {
		in.Status.Verification.State = v1alpha1.VerificationVerifying
		in.Status.Verification.Ref = &v1alpha1.ObjectReference{Name: pv.Name, UID: pv.UID}
	})
	e.evaluating(true)
	e.review(name, 4)
	e.checkStoppedForReplan(name)
	if pvs := e.verifications(name); len(pvs) != 0 {
		t.Errorf("verifications = %d, want the in-flight round deleted", len(pvs))
	}
	if !strings.Contains(e.gh.withMarker("maul:intent")[0].Body, "acceptance criteria") {
		t.Errorf("status comment does not say the plan states criteria")
	}
}

// TestVerifyRoundInputRefusesWhatIsNotTheApprovedFailure: an acceptance-fix
// round is handed evidence only of its own verification of the approved
// outcomes that failed: with no verification named, another plan
// revision's record, the verification gone or of other outcomes, or one
// that leaves no failure to fix, its input is unavailable and nothing is
// rendered; its own failing verification is rendered.
func TestVerifyRoundInputRefusesWhatIsNotTheApprovedFailure(t *testing.T) {
	e, name, pvName := failingEvaluation(t)
	pv := e.pvNamed(pvName)
	ref := &v1alpha1.ObjectReference{Name: pv.Name, UID: pv.UID}
	for _, tc := range []struct {
		name  string
		setup func(p *pass, run *v1alpha1.IntentRun)
		ok    bool
	}{
		{"its own failing verification", func(*pass, *v1alpha1.IntentRun) {}, true},
		{"no verification named", func(_ *pass, run *v1alpha1.IntentRun) { run.Spec.Inputs.VerificationRef = nil },
			false},
		{"another plan revision", func(_ *pass, run *v1alpha1.IntentRun) { run.Spec.Inputs.PlanRevision++ }, false},
		{"the verification gone", func(_ *pass, run *v1alpha1.IntentRun) {
			run.Spec.Inputs.VerificationRef = &v1alpha1.ObjectReference{Name: "gone", UID: "gone"}
		}, false},
		{"other outcomes", func(p *pass, _ *v1alpha1.IntentRun) {
			p.in.Status.Acceptance.OutcomesDigest = "sha256:" + strings.Repeat("f", 64)
		}, false},
		{"no outcomes digest", func(p *pass, _ *v1alpha1.IntentRun) { p.in.Status.Acceptance.OutcomesDigest = "" },
			false},
		{"the failure accepted by hand", func(p *pass, _ *v1alpha1.IntentRun) {
			p.in.Status.Acceptance.Accepted = append(p.in.Status.Acceptance.Accepted, v1alpha1.AcceptanceRecord{
				ID: oVersion, By: approver, At: metav1.NewTime(e.clock.Now()), HeadSetDigest: pv.Spec.HeadSetDigest})
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := e.prVerifyPass(name)
			run := &v1alpha1.IntentRun{Spec: v1alpha1.IntentRunSpec{Stage: v1alpha1.IntentStageRevise,
				Trigger: v1alpha1.IntentRunTriggerVerify, Repository: v1alpha1.IntentRunRepository{URL: appRepoURL},
				Inputs: v1alpha1.IntentRunInputs{PlanRevision: p.in.Status.Approval.PlanRevision,
					VerificationRef: ref.DeepCopy()}}}
			tc.setup(p, run)
			feedback, _, err := p.verifyRoundInput(context.Background(), run)
			if tc.ok {
				if err != nil || feedback == "" {
					t.Fatalf("input = %q, %v; want the reproduction", feedback, err)
				}
				return
			}
			if !errors.Is(err, errInputUnavailable) || feedback != "" {
				t.Fatalf("input = %q, %v; want unavailable", feedback, err)
			}
		})
	}
}

// TestVerifiesApprovedNeedsAnOutcomesDigest: a verification verified the
// approved record only by the record's own outcomes digest: a criteria-era
// request (no outcomes digest) never matches a record without one, as two
// empty digests would.
func TestVerifiesApprovedNeedsAnOutcomesDigest(t *testing.T) {
	digestOf := "sha256:" + strings.Repeat("a", 64)
	for _, tc := range []struct {
		name     string
		pv, acc  string
		approved bool
	}{
		{"the record's outcomes", digestOf, digestOf, true},
		{"other outcomes", "sha256:" + strings.Repeat("b", 64), digestOf, false},
		{"a criteria request, a record of none", "", "", false},
		{"a criteria request", "", digestOf, false},
	} {
		pv := &v1alpha1.PreviewVerification{Spec: v1alpha1.PreviewVerificationSpec{OutcomesDigest: tc.pv,
			CriteriaDigest: emptyCriteriaDigest}}
		acc := &v1alpha1.IntentAcceptance{Digest: emptyCriteriaDigest, OutcomesDigest: tc.acc}
		if got := verifiesApproved(pv, acc); got != tc.approved {
			t.Errorf("%s: verifiesApproved = %v, want %v", tc.name, got, tc.approved)
		}
	}
}

// TestPlanAskWithAComponentNamedLikeAPart: a Project previewing a component
// named like the tester or the tool pod's part is verified, but its plan is
// asked nothing to target, as v1alpha1.DesiredPreviewEnvironment fails
// closed on it.
func TestPlanAskWithAComponentNamedLikeAPart(t *testing.T) {
	for _, part := range []string{v1alpha1.PreviewPartTester, v1alpha1.PreviewRoleTools} {
		p := previewedProject(true)
		p.Spec.Verification = &v1alpha1.ProjectVerification{Enabled: true}
		p.Spec.Repositories[0].Name = part
		set := Settings{Verification: true, Previews: true, Evaluator: true}
		if a := set.planAsk(p); !a.verified || a.targets != nil || a.asked() {
			t.Errorf("%s: planAsk = %+v, want verified and not asked", part, a)
		}
	}
}
