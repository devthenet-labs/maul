// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// TestAcceptanceStatusWhileEvaluating (S1-29): an intent the evaluator
// verifies has its maul/acceptance posted through the digest-deduped path
// while it is Evaluating, pending in maul's words ("maul is checking this"),
// and once its evaluations spent their ceiling with nothing confirmed
// (Unconfirmed), a final error ("maul couldn't confirm this"), never a
// pending status that waits for nothing.
func TestAcceptanceStatusWhileEvaluating(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.review(name, 3)
	if v := e.verification(name); v.State != v1alpha1.VerificationEvaluating {
		t.Fatalf("verification = %+v, want Evaluating", v)
	}
	head := e.get(name).Status.PullRequests[0].HeadSHA
	e.review(name, 1)
	st := e.latestAcceptance(head)
	if st.State != statusPending || !strings.HasPrefix(st.Description, "maul is checking this") ||
		len(st.Description) > 140 {
		t.Fatalf("maul/acceptance while evaluating = %+v, want pending, maul is checking this", st)
	}
	n := len(e.acceptanceStatuses(head))
	e.review(name, 2)
	if got := len(e.acceptanceStatuses(head)); got != n {
		t.Fatalf("an unchanged status was posted again: %d statuses, had %d", got, n)
	}

	// The round runs and is judged; then the evaluation ceiling, lowered
	// under what was spent, starts no more: Unconfirmed for good.
	pv := e.onlyVerification(name)
	e.playTools(pv.Name, v1alpha1.VerificationRunning, toolPodIP, true)
	e.review(name, 1)
	e.runRuns()
	e.runRuns()
	e.playTools(pv.Name, v1alpha1.VerificationObserved, toolPodIP, true)
	e.review(name, 3)
	proj := e.project0()
	proj.Spec.Limits.MaxEvaluateCostMicroUSD = new(int64(100000))
	if err := e.c.Update(context.Background(), proj); err != nil {
		t.Fatal(err)
	}
	e.setStatus(name, func(in *v1alpha1.Intent) {
		in.Status.Verification.Requested = &v1alpha1.VerifyRequest{By: approver, ByID: 7,
			At: metav1.NewTime(e.clock.Now()), CommentID: 99}
	})
	e.clock.Advance(10 * time.Minute)
	e.review(name, 4)
	if v := e.verification(name); v.State != v1alpha1.VerificationUnconfirmed || v.Requested != nil {
		t.Fatalf("verification = %+v, want Unconfirmed past the ceiling", v)
	}
	st = e.latestAcceptance(head)
	if st.State != statusError || !strings.HasPrefix(st.Description, "maul couldn't confirm this") ||
		len(st.Description) > 140 {
		t.Errorf("maul/acceptance once unconfirmed = %+v, want a final error, maul couldn't confirm this", st)
	}
}
