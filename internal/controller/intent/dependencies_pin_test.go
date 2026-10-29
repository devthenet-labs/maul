// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"os"
	"slices"
	"strings"
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

// TestReleaseReasonEnumPinned: the API admits as status.dependencies.
// releaseReason exactly the release reasons this package records.
func TestReleaseReasonEnumPinned(t *testing.T) {
	src, err := os.ReadFile("../../../api/v1alpha1/intent_dependencies_types.go")
	if err != nil {
		t.Fatal(err)
	}
	_, after, ok := strings.Cut(string(src), "ReleaseReason is")
	marker := "+kubebuilder:validation:Enum="
	_, enum, ok2 := strings.Cut(after, marker)
	if !ok || !ok2 {
		t.Fatal("no Enum marker on ReleaseReason")
	}
	enum, _, _ = strings.Cut(enum, "\n")
	got := strings.Split(strings.TrimSpace(enum), ";")
	want := make([]string, 0, len(releaseMessages))
	for r := range releaseMessages {
		want = append(want, r)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("ReleaseReason enum = %v, want the release reasons %v", got, want)
	}
}

// TestDegradedReasonsWorded: every release reason that started an intent
// without its dependencies is worded for the Project condition and the
// status comment, and no other is.
func TestDegradedReasonsWorded(t *testing.T) {
	degraded := map[string]bool{v1alpha1.ReasonDependenciesUnsupported: true,
		v1alpha1.ReasonDependenciesForbidden: true, v1alpha1.ReasonDependenciesUnanswered: true}
	for r := range releaseMessages {
		if got := templates.DependencyReasonWords(r, DefaultDependencyGiveUp) != ""; got != degraded[r] {
			t.Errorf("DependencyReasonWords(%s) worded = %v, want %v", r, got, degraded[r])
		}
	}
}
