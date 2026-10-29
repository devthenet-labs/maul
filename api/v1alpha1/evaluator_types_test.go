// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestEvaluatorDefaults: every evaluator setting a Project leaves unset reads
// as its documented default (none is a schema default), and a set one reads
// back as set.
func TestEvaluatorDefaults(t *testing.T) {
	var unset ProjectSpec
	if got := unset.HandoffMax(); got != DefaultHandoffMax {
		t.Errorf("HandoffMax() unset = %s, want %s", got, DefaultHandoffMax)
	}
	if got := unset.MaxToolCalls(); got != DefaultMaxToolCalls {
		t.Errorf("MaxToolCalls() unset = %d, want %d", got, DefaultMaxToolCalls)
	}
	if got := unset.Limits.MaxEvaluateCostLimit(); got != DefaultMaxEvaluateCostMicroUSD {
		t.Errorf("MaxEvaluateCostLimit() unset = %d, want %d", got, DefaultMaxEvaluateCostMicroUSD)
	}
	if got := unset.HandoffMode(HandoffGated); got != HandoffGated {
		t.Errorf("HandoffMode(gated) unset = %q, want the controller's default", got)
	}

	calls, cost := int32(120), int64(2500000)
	set := ProjectSpec{
		Verification: &ProjectVerification{Handoff: HandoffEarly,
			HandoffMax: &metav1.Duration{Duration: 30 * time.Minute}, MaxToolCalls: &calls},
		Limits: ProjectLimits{MaxEvaluateCostMicroUSD: &cost},
	}
	if got := set.HandoffMax(); got != 30*time.Minute {
		t.Errorf("HandoffMax() = %s, want 30m", got)
	}
	if got := set.MaxToolCalls(); got != calls {
		t.Errorf("MaxToolCalls() = %d, want %d", got, calls)
	}
	if got := set.Limits.MaxEvaluateCostLimit(); got != cost {
		t.Errorf("MaxEvaluateCostLimit() = %d, want %d", got, cost)
	}
	if got := set.HandoffMode(HandoffGated); got != HandoffEarly {
		t.Errorf("HandoffMode(gated) = %q, want the Project's early", got)
	}
}

// TestOutcomeKindDefault: an outcome with no kind is a change.
func TestOutcomeKindDefault(t *testing.T) {
	for _, tt := range []struct {
		kind OutcomeKind
		want OutcomeKind
	}{
		{"", OutcomeKindChange},
		{OutcomeKindChange, OutcomeKindChange},
		{OutcomeKindKeep, OutcomeKindKeep},
	} {
		if got := (AcceptanceOutcome{Kind: tt.kind}).EffectiveKind(); got != tt.want {
			t.Errorf("EffectiveKind() of %q = %q, want %q", tt.kind, got, tt.want)
		}
	}
}

// TestEvaluatorConditions pins the Project conditions the evaluator adds.
func TestEvaluatorConditions(t *testing.T) {
	if ConditionEvaluatorUnverified != "EvaluatorUnverified" || ConditionEvaluatorUnavailable != "EvaluatorUnavailable" {
		t.Errorf("evaluator conditions = %q, %q", ConditionEvaluatorUnverified, ConditionEvaluatorUnavailable)
	}
}

// TestToolPodPartRole: the tool pod's part role is the slot's tools role
// value, so a fact and a label never disagree.
func TestToolPodPartRole(t *testing.T) {
	if string(PreviewRoleToolPod) != PreviewRoleTools {
		t.Errorf("PreviewRoleToolPod = %q, want %q", PreviewRoleToolPod, PreviewRoleTools)
	}
}
