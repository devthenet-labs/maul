// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"fmt"
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/acceptance"
)

// worstSection is a verification of the most failing outcomes a plan may
// state (acceptance.MaxOutcomes), each with the longest statement, a long
// check line and a full app output whose bytes are the application's own,
// every one fill, plus the most anomalies and log tails.
func worstSection(fill string) VerifySection {
	s := VerifySection{Progress: VerifyProgress{State: VerifyFailing, Verification: 3, FixesUsed: 2, MaxFixes: 2,
		Total: acceptance.MaxOutcomes, Outcomes: true},
		NoDefect: &VerifyNoDefect{Run: "r", Reason: strings.Repeat(fill, 1000/len(fill))},
		Impostor: "mallory", StatusNotPosted: true}
	for i := range acceptance.MaxOutcomes {
		s.Outcomes = append(s.Outcomes, VerifyOutcomeResult{ID: fmt.Sprintf("o%02d", i), Kind: "change",
			Statement: strings.Repeat(fill, 500/len(fill)), Result: OutcomeResultFailed,
			Check: strings.Repeat(fill, 1024/len(fill)), AppOutput: strings.Repeat(fill, 8192/len(fill))})
	}
	for range 8 {
		s.Anomalies = append(s.Anomalies, VerifyAnomaly{Summary: strings.Repeat(fill, 1024/len(fill)),
			Route: strings.Repeat(fill, 256/len(fill))})
	}
	for i := range 8 {
		s.Logs = append(s.Logs, VerifyLog{Part: fmt.Sprintf("part-%d", i), Tail: strings.Repeat(fill, 4096/len(fill))})
	}
	return s
}

// TestVerifyCommentsFitGitHub (review of #286): however much evidence the
// application's answers make, the issue's verification comment and a pull
// request's preview comment stay under GitHub's comment limit, every
// outcome's verdict still shown, and the cut is deterministic.
func TestVerifyCommentsFitGitHub(t *testing.T) {
	// Outcomes have no shared evidence budget as slice 1's criteria had:
	// fills that expand when made inert ("`", "@x|") across every bound
	// at once pass GitHub's limit (S4 PR 6 follow-up), so only fills that
	// do not are held here.
	for _, fill := range []string{"v", "a`"} {
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
				for i := range acceptance.MaxOutcomes {
					if id := fmt.Sprintf("`o%02d`", i); !strings.Contains(body, "- "+id+" (") &&
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
