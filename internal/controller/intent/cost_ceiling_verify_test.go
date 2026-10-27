// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// TestCostCeilingEndsVerifyRetry: an acceptance-fix round whose attempt
// failed retryably creates no further attempt once the spend is at the
// ceiling (lease), but unlike a human's round (TestCostCeilingGatesReviseRetry)
// it never blocks the intent (D2): the round ends, automatic fixes halt on
// CostCeiling with VerifyFailing, and the intent is back in review.
func TestCostCeilingEndsVerifyRetry(t *testing.T) {
	e := newVerifyEnv(t, acceptancePlan)
	name, _ := e.failingVerification()
	e.jobs.output = notBuilt(notBuiltReport("")) // the attempt fails, retryably
	// Lease the round below the ceiling, then reach the ceiling before its
	// attempt fails.
	for range 20 {
		if len(e.verifyRuns(name)) == 1 {
			break
		}
		e.review(name, 1)
		e.readyRepositories(repoImage)
	}
	in := e.get(name)
	if n := len(e.verifyRuns(name)); n != 1 || in.Status.Phase != v1alpha1.IntentRevising {
		t.Fatalf("verify runs = %d, phase %s; want the acceptance-fix round entered", n, in.Status.Phase)
	}
	if in.Status.Usage.CostMicroUSD <= 0 {
		t.Fatalf("spend %d: the ceiling cannot be reached", in.Status.Usage.CostMicroUSD)
	}
	e.setCeiling(in.Status.Usage.CostMicroUSD)

	in = e.drive(name, v1alpha1.IntentInReview, repoImage)
	if meta.IsStatusConditionTrue(in.Status.Conditions, v1alpha1.ConditionBudgetExhausted) {
		t.Fatalf("conditions = %+v: an acceptance fix blocked the intent at the ceiling", in.Status.Conditions)
	}
	runs := e.verifyRuns(name)
	if len(runs) != 1 || runs[0].Status.Phase != v1alpha1.RunFailed {
		t.Fatalf("verify runs = %+v, want the one failed attempt and no retry", runs)
	}
	v := in.Status.Verification
	if v.AutoFix.State != v1alpha1.AutoFixHalted || v.AutoFix.Reason != v1alpha1.AutoFixCostCeiling ||
		v.State != v1alpha1.VerificationFailing || in.Status.ActiveRun != nil {
		t.Fatalf("verification %+v, active run %+v; want fixes halted on CostCeiling, Failing",
			v, in.Status.ActiveRun)
	}
	if c := meta.FindStatusCondition(in.Status.Conditions, v1alpha1.ConditionVerifyFailing); c == nil ||
		c.Status != metav1.ConditionTrue || c.Reason != string(v1alpha1.AutoFixCostCeiling) {
		t.Fatalf("VerifyFailing = %+v, want True CostCeiling", c)
	}
	// Review stays open: the intent is still InReview a while later.
	in = e.drive(name, v1alpha1.IntentInReview, repoImage)
	if in.Status.VerifyFixes != 0 {
		t.Errorf("verifyFixes = %d, want the unpushed round uncounted", in.Status.VerifyFixes)
	}
}
