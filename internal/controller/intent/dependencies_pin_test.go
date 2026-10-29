// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/intentview"
	"github.com/devthenet-labs/maul/internal/templates"
)

// waitReasons are every reason this package waits on dependencies with.
var waitReasons = []string{v1alpha1.ReasonOpenBlockers, v1alpha1.ReasonBlockerNotCompleted,
	v1alpha1.ReasonBlockerUnreadable, v1alpha1.ReasonDependencyCycle, v1alpha1.ReasonCheckingDependencies,
	v1alpha1.ReasonNoActiveSlot}

// TestIntentviewWaitingTextPinned: the board words every reason this
// package waits with, and only those.
func TestIntentviewWaitingTextPinned(t *testing.T) {
	if len(intentview.WaitingText) != len(waitReasons) {
		t.Errorf("intentview.WaitingText words %d reasons, the controller waits with %d",
			len(intentview.WaitingText), len(waitReasons))
	}
	for _, r := range waitReasons {
		if intentview.WaitingText[r] == "" {
			t.Errorf("intentview.WaitingText has no wording for %s", r)
		}
		if _, ok := releaseMessages[r]; ok {
			t.Errorf("%s is both a wait and a release reason", r)
		}
	}
}

// TestDegradedReasonsWorded: every release reason that started an intent
// without its dependencies is worded for the Project condition and the
// status comment, and no other is.
func TestDegradedReasonsWorded(t *testing.T) {
	degraded := map[string]bool{v1alpha1.ReasonDependenciesUnsupported: true,
		v1alpha1.ReasonDependenciesForbidden: true, v1alpha1.ReasonDependenciesUnanswered: true}
	for r := range releaseMessages {
		if got := templates.DependencyReasonWords(r) != ""; got != degraded[r] {
			t.Errorf("DependencyReasonWords(%s) worded = %v, want %v", r, got, degraded[r])
		}
	}
}
