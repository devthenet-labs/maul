// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package web

import (
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/intentview"
)

// TestCardCarriesTheWait: a Pending intent waiting on its blockers shows
// the wait on its card in public wording, naming only the intent
// repository's issues; one that started shows none.
func TestCardCarriesTheWait(t *testing.T) {
	const repo = "https://github.com/acme/intents"
	in := &v1alpha1.Intent{
		ObjectMeta: metav1.ObjectMeta{Name: "target-3"},
		Spec:       v1alpha1.IntentSpec{Project: "target", Issue: v1alpha1.IntentIssue{Repository: repo, Number: 3}},
		Status: v1alpha1.IntentStatus{
			Phase: v1alpha1.IntentPending,
			Conditions: []metav1.Condition{{Type: v1alpha1.ConditionWaitingOnDependencies,
				Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonOpenBlockers, Message: "secret detail"}},
			Dependencies: &v1alpha1.IntentDependencies{Blockers: []v1alpha1.IntentBlocker{
				{Repository: repo, Number: 2, State: "open"},
				{Repository: "https://github.com/private/thing", Number: 9, State: "open"},
			}},
		},
	}
	c := projectCard(in, nil, nil, intentview.Limits{})
	if c.Waiting == nil || c.Waiting.Reason != "waiting for the issues it is blocked by" ||
		!slices.Equal(c.Waiting.Refs, []string{"#2"}) || c.Waiting.Others != 1 {
		t.Fatalf("card waiting = %+v", c.Waiting)
	}
	in.Status.Phase = v1alpha1.IntentPlanning
	if c := projectCard(in, nil, nil, intentview.Limits{}); c.Waiting != nil {
		t.Fatalf("a started intent's card shows a wait: %+v", c.Waiting)
	}
}
