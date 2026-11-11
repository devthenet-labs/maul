// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"strings"
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// TestCauseWordsEveryCause: every settle cause has words in both
// placements, each maul's own: a commit status clause, a hand-off reason
// with words of its own (none only for a cause the state says), a not-run
// reason with words of its own where it has one, and no preview named on a
// Project with none. An unknown cause reads as SettleNone.
func TestCauseWordsEveryCause(t *testing.T) {
	stateWords := map[v1alpha1.SettleCause]bool{v1alpha1.SettleNone: true, v1alpha1.SettlePreviewNotLive: true}
	for _, c := range v1alpha1.SettleCauses {
		for _, workspace := range []bool{false, true} {
			w := CauseWords(c, workspace)
			if w.Short == "" || len(w.Short) > 140 {
				t.Errorf("%q (workspace %v): commit status words %q", c, workspace, w.Short)
			}
			if workspace && strings.Contains(w.Short, "preview") {
				t.Errorf("%q names a preview on a Project with none: %q", c, w.Short)
			}
			switch {
			case stateWords[c] && w.Why != "":
				t.Errorf("%q: why %q, want the state's words", c, w.Why)
			case !stateWords[c] && handoffWhyWords[w.Why] == "":
				t.Errorf("%q: why %q has no words", c, w.Why)
			}
			if _, ok := verifyNotRunWords[w.NotRun]; w.NotRun != "" && !ok {
				t.Errorf("%q: not-run reason %q has no words", c, w.NotRun)
			}
		}
	}
	if got, want := CauseWords("@octocat", false), CauseWords(v1alpha1.SettleNone, false); got != want {
		t.Errorf("an unknown cause = %+v, want %+v", got, want)
	}
	// The causes found before a round runs answer a /maul verify with
	// their own words; the hand-off and the reply name them alike.
	for c, notRun := range map[v1alpha1.SettleCause]string{v1alpha1.SettleSandboxImage: NotRunSandboxImage,
		v1alpha1.SettleTree: NotRunTree} {
		w := CauseWords(c, true)
		if w.NotRun != notRun || verifyNotRunWords[notRun] != handoffWhyWords[w.Why] {
			t.Errorf("%q: not-run %q (%q), hand-off %q", c, w.NotRun, verifyNotRunWords[w.NotRun],
				handoffWhyWords[w.Why])
		}
	}
}
