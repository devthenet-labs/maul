// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/ghclient"
)

// prunedCriteriaIntent is an intent in review whose approved plan states
// slice-1 criteria, as a release before S4 left it once the API server
// pruned its criteria: recorded by their digest alone, its first round
// judged at the pull request's heads (its http criterion passed, its
// manual one open) and that judgement's pending maul/acceptance posted.
// With evaluator, the agent evaluator then verifies the Project. It
// returns the env, the intent and its pull request's head.
func prunedCriteriaIntent(t *testing.T, evaluator bool) (*env, string, string) {
	t.Helper()
	e := newVerifyEnv(t, acceptancePlan)
	name := e.reviewing()
	e.readyPreview(name)
	e.review(name, 3)
	in := e.get(name)
	hs, ok := previewHeadSet(e.getProject(), in)
	if !ok {
		t.Fatal("no head set")
	}
	head := in.Status.PullRequests[0].HeadSHA
	e.postStatus(head, ghclient.CommitStatus{Context: acceptanceContext, State: statusPending,
		Description: "1/1 automated passed, 1 to accept by hand", Creator: testBot})
	posted := e.latestAcceptance(head)
	e.setStatus(name, func(in *v1alpha1.Intent) {
		since := metav1.NewTime(e.clock.Now())
		in.Status.Verification = &v1alpha1.IntentVerification{State: v1alpha1.VerificationPassed, Round: 1,
			Attempt: 1, HeadSetDigest: hs.digest, Heads: hs.heads, Passed: 1, ManualOpen: 1,
			AutoFix: v1alpha1.AutoFix{State: v1alpha1.AutoFixArmed, Since: &since}}
		pr := &in.Status.PullRequests[0]
		pr.AcceptanceStatusID, pr.AcceptanceHead, pr.AcceptanceDigest = posted.ID, head, "slice-1"
	})
	if evaluator {
		e.evaluatorOn(0.4)
	}
	return e, name, head
}

// TestPrunedCriteriaRecordIsNeverAcceptance is the pruning hazard of the S4
// plan: once IntentAcceptance.Criteria leaves the CRD, the API server prunes
// status.acceptance.criteria, and a criteria-era Intent reads as a record
// with a digest of real criteria (not acceptance.Digest(nil)) and neither
// criteria, outcomes nor an outcomes digest. Read naively that is a plan
// declaring nothing: NotDeclared, and /maul accept all turns maul/acceptance
// green with nothing ever checked, a false pass. Whatever the evaluator
// switch, such a record must never settle NotDeclared, never post success,
// and never be accepted whole: it is a mismatch, for a replan.
func TestPrunedCriteriaRecordIsNeverAcceptance(t *testing.T) {
	for _, tc := range []struct {
		name      string
		evaluator bool
	}{{"evaluator off", false}, {"evaluator on", true}} {
		t.Run(tc.name, func(t *testing.T) {
			// A criteria plan approved, verified, and its http criterion
			// passed, before the upgrade.
			e, name, head := prunedCriteriaIntent(t, tc.evaluator)
			before := len(e.acceptanceStatuses(head))
			if acc := e.get(name).Status.Acceptance; acc.Digest == acceptance.Digest(nil) || acc.OutcomesDigest != "" {
				t.Fatalf("record %+v: not a pruned criteria record", acc)
			}
			e.review(name, 4)
			e.gh.comment(approver, "/maul accept all")
			e.settleActions(name)
			e.review(name, 3)
			in := e.get(name)
			if v := in.Status.Verification; v != nil && v.State == v1alpha1.VerificationNotDeclared {
				t.Errorf("verification = %+v: a pruned criteria record read as a plan declaring nothing", v)
			}
			if acc := in.Status.Acceptance; acc != nil {
				for _, r := range acc.Accepted {
					if r.ID == acceptAll {
						t.Errorf("accepted %+v: a pruned criteria record accepted whole", r)
					}
				}
			}
			all := e.acceptanceStatuses(head) // newest first
			for _, st := range all[:len(all)-before] {
				if st.State == statusSuccess {
					t.Errorf("maul/acceptance %+v on a pruned criteria record", st)
				}
			}
		})
	}
}
