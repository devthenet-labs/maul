// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
)

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
	t.Skip("S4 PR 4: mode() must key the criteria-era mismatch on Digest != acceptance.Digest(nil) with no " +
		"OutcomesDigest; until then a pruned record reads as NotDeclared and /maul accept all posts success")
	for _, tc := range []struct {
		name      string
		evaluator bool
	}{{"evaluator off", false}, {"evaluator on", true}} {
		t.Run(tc.name, func(t *testing.T) {
			// A criteria plan approved, verified, and its http criterion
			// passed, before the upgrade.
			e := newVerifyEnv(t, acceptancePlan)
			name := e.reviewing()
			e.readyPreview(name)
			e.review(name, 3)
			e.observe(e.onlyVerification(name).Name, v1alpha1.VerificationObserved, answering(200))
			e.review(name, 2)
			if tc.evaluator {
				e.evaluatorOn(0.4)
			}
			e.setStatus(name, func(in *v1alpha1.Intent) {
				// What the API server leaves of the record once the field
				// is gone: the digest, no criteria, no outcomes.
				in.Status.Acceptance.Criteria = nil
			})
			head := e.get(name).Status.PullRequests[0].HeadSHA
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
