// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// The reviewer round is retired ("evaluator observed", W4): no saved check
// is proposed, confirmed, reviewed or stored, so no checkreview IntentRun
// is ever launched. What stays here is what a checkreview run an earlier
// release created still meets (checkReviewCurrent, read when such a run is
// scheduled); the stage is settled and its leftovers removed by the steps
// after the switch.

// checkReviewCurrent re-reads, uncached, what a checkreview run is launched
// for: its round and attempt are still the Intent's verification in flight,
// evaluating. A refusal says why and launches nothing: a stale round's
// checks are never reviewed.
func (r *RunReconciler) checkReviewCurrent(ctx context.Context, run *v1alpha1.IntentRun) (*result, error) {
	var in v1alpha1.Intent
	if err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: run.Namespace, Name: run.Spec.IntentRef.Name},
		&in); err != nil {
		return nil, err
	}
	v := in.Status.Verification
	want := v1alpha1.PreviewVerificationName(in.Name, run.Spec.Round, run.Spec.Attempt)
	if in.UID != run.Spec.IntentRef.UID || v == nil || v.Ref == nil || v.Ref.Name != want ||
		v.State != v1alpha1.VerificationEvaluating {
		return &result{outcome: OutcomeAborted, detail: fmt.Sprintf("the evaluation round is no longer current: "+
			"the intent no longer evaluates %s; nothing was launched", want)}, nil
	}
	return nil, nil
}
