// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"fmt"
	"strings"
	"testing"
)

// worstSection is a verification of 16 failing criteria, each with the
// longest statement, the longest expected and observed values, a full body
// prefix and 8 mismatches whose values are the application's own, every one
// fill (256 bytes of it), plus the most log tails.
func worstSection(fill string) VerifySection {
	value := strings.Repeat(fill, 256/len(fill))
	s := VerifySection{Progress: VerifyProgress{State: VerifyFailing, Verification: 3, FixesUsed: 2, MaxFixes: 2,
		Total: 16}, NoDefect: &VerifyNoDefect{Run: "r", Reason: strings.Repeat(fill, 1000/len(fill))},
		Impostor: "mallory", StatusNotPosted: true}
	for i := range 16 {
		c := VerifyCriterion{ID: fmt.Sprintf("c%02d", i), Kind: "http", Outcome: VerifyOutcomeFail,
			Statement: strings.Repeat("s", 200), Expected: strings.Repeat(fill, 512/len(fill)),
			Observed: strings.Repeat(fill, 512/len(fill)), Body: strings.Repeat(fill, 4096/len(fill))}
		for j := range 8 {
			c.Mismatches = append(c.Mismatches, VerifyMismatch{Kind: MismatchJSON, Key: fmt.Sprintf("/%d/%s", j, value),
				Expected: value, Observed: value})
		}
		s.Criteria = append(s.Criteria, c)
	}
	for i := range 8 {
		s.Logs = append(s.Logs, VerifyLog{Part: fmt.Sprintf("part-%d", i), Tail: strings.Repeat(fill, 4096/len(fill))})
	}
	return s
}

// TestVerifyCommentsFitGitHub (review of #286): however much evidence the
// application's answers make, the issue's verification comment and a pull
// request's preview comment stay under GitHub's comment limit. Evidence
// past the comment's one budget is left out deterministically, saying how
// much, and every criterion's verdict is still shown.
func TestVerifyCommentsFitGitHub(t *testing.T) {
	for _, fill := range []string{"v", "`", "a`", "@x|"} {
		t.Run(fmt.Sprintf("%q", fill), func(t *testing.T) {
			s := worstSection(fill)
			issue, err := RenderVerifyComment(VerifyComment{Namespace: "maul", Intent: "i", Round: 3, Attempt: 1,
				Section: s})
			if err != nil {
				t.Fatal(err)
			}
			c := testPreviewComment(PreviewLive, "", "/api")
			c.Verify = &s
			preview, err := RenderIntentPreviewComment(c)
			if err != nil {
				t.Fatal(err)
			}
			for name, body := range map[string]string{"issue": issue, "preview": preview} {
				if len(body) > MaxCommentBytes {
					t.Errorf("%s comment is %d bytes, over %d", name, len(body), MaxCommentBytes)
				}
				if !strings.Contains(body, "more not shown") {
					t.Errorf("%s comment does not say what it left out", name)
				}
				for i := range 16 {
					if id := fmt.Sprintf("`c%02d`", i); !strings.Contains(body, id+" failed") &&
						!strings.Contains(body, "| "+id+" |") {
						t.Errorf("%s comment lost %s's verdict", name, id)
					}
				}
			}
			again, err := RenderVerifyComment(VerifyComment{Namespace: "maul", Intent: "i", Round: 3, Attempt: 1,
				Section: worstSection(fill)})
			if err != nil || again != issue {
				t.Error("the cut is not deterministic")
			}
		})
	}
}
