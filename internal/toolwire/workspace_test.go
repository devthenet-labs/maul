// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package toolwire

import "testing"

// TestWorkspaceSpellings pins the spellings an evaluator writes in its calls
// and the prompts teach: the placeholders the relay substitutes, the one
// language the run verb takes, and the fixed window a browser session opens
// at. The placeholders and the viewport moved here from the saved-check
// grammar unchanged, so a prompt and a relay of the same release agree, and
// a ledger an earlier release wrote reads the same.
func TestWorkspaceSpellings(t *testing.T) {
	for _, tc := range []struct{ name, got, want string }{
		{"bin placeholder", BinPlaceholder, "{{bin}}"},
		{"tree placeholder", TreePlaceholder, "{{tree}}"},
		{"library language", LanguageGo, "go"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
	if DefaultViewport != (Viewport{Width: 1280, Height: 800}) {
		t.Errorf("DefaultViewport = %+v, want 1280x800", DefaultViewport)
	}
}
