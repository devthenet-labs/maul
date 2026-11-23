// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intentview

import (
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// TestProjectConditionText: a known reason gets its own wording, an
// unknown one the condition's fallback, an empty one the fallback too, and
// a condition the page does not show nothing.
func TestProjectConditionText(t *testing.T) {
	tests := []struct{ condition, reason, want string }{
		{v1alpha1.ConditionApproversResolved, "Resolved", "GitHub answered who each approver is"},
		{v1alpha1.ConditionApproversResolved, "Other", "the approvers are not resolved"},
		{v1alpha1.ConditionApproversResolved, "", "the approvers are not resolved"},
		{v1alpha1.ConditionReady, "Unknown", "the project is not ready"},
		{"IntentNameConflict", "NameHeld", ""},
	}
	for _, tt := range tests {
		if got := ProjectConditionText(tt.condition, tt.reason); got != tt.want {
			t.Errorf("ProjectConditionText(%q, %q) = %q, want %q", tt.condition, tt.reason, got, tt.want)
		}
	}
	for _, c := range ProjectConditions {
		if ProjectConditionText(c, "") == "" {
			t.Errorf("%s has no fallback", c)
		}
	}
}
