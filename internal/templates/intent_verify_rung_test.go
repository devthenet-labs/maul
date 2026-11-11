// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"strings"
	"testing"
)

// TestVerifyCommentNamesTheReplayRung: a replay round's comment says, in
// its heading, that maul replayed its saved checks with no model; a full
// round's heading is as it was.
func TestVerifyCommentNamesTheReplayRung(t *testing.T) {
	s := VerifySection{Progress: VerifyProgress{State: VerifyPassed, Verification: 2, Passing: 1, Total: 1,
		Outcomes: true},
		Outcomes: []VerifyOutcomeResult{{ID: "version-served", Kind: "change", Statement: "It serves its version.",
			Result: OutcomeResultPassed, Check: "http to shop: GET /version"}}}
	for _, tc := range []struct {
		name   string
		replay bool
		want   string
	}{
		{name: "replay", replay: true,
			want: "### maul: verification 2, attempt 1 (replayed saved checks, no model)\n"},
		{name: "full", want: "### maul: verification 2, attempt 1\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := RenderVerifyComment(VerifyComment{Namespace: "maul", Intent: "target-1", Round: 2,
				Attempt: 1, Replay: tc.replay, Section: s})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("comment heading, want %q:\n%s", tc.want, out)
			}
			if !tc.replay && strings.Contains(out, "no model") {
				t.Errorf("a full round's comment says no model:\n%s", out)
			}
		})
	}
}
