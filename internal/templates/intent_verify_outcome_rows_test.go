// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/acceptance"
)

// TestOutcomesShownOnceWithTheirKind (S2 demo run 5): every outcome is
// listed once, in its row, with its reason and maul's check in that row,
// never again as a paragraph of its own; its kind is the outcome's own
// (change or keep, unset as change, anything else as unknown), never the
// word "outcome"; and an app output block names the outcome it is of. The
// same on a previewed pull request's comment, a Project with no preview's
// and the issue's verification comment.
func TestOutcomesShownOnceWithTheirKind(t *testing.T) {
	s := VerifySection{
		Progress: VerifyProgress{State: VerifyFailing, Verification: 1, MaxFixes: 2, Passing: 1, Total: 5,
			Outcomes: true},
		Outcomes: []VerifyOutcomeResult{
			{ID: "o-pass", Kind: "change", Statement: "It prints a total.", Result: OutcomeResultPassed,
				Reason: acceptance.ReasonConfirmed, Check: "exec of {{bin}}: total; exit 0"},
			{ID: "o-unsure", Kind: "keep", Statement: "It still prints the counts.", Result: OutcomeResultUnsure,
				Reason: acceptance.ReasonNotAsserted, Check: "exec of {{bin}}: counts; exit 0"},
			{ID: "o-fail-1", Statement: "It prints a density.", Result: OutcomeResultFailed,
				Check: "exec of {{bin}}: density | a; exit 0", AppOutput: "density 30%"},
			{ID: "o-fail-2", Kind: "keep", Statement: "It reads JSON.", Result: OutcomeResultFailed,
				Check: "exec of {{bin}}: json; exit 0", AppOutput: "parse error"},
			{ID: "o-odd", Kind: "`bogus` @octocat", Statement: "It runs.", Result: OutcomeResultUnsure},
		},
	}
	kinds := map[string]string{"o-pass": "change", "o-unsure": "keep", "o-fail-1": "change", "o-fail-2": "keep",
		"o-odd": "unknown"}
	render := map[string]func() (string, error){
		"preview": func() (string, error) {
			c := testPreviewComment(PreviewLive, "", "")
			c.Handoff, c.Verify = &PreviewHandoff{State: HandoffReady}, &s
			return RenderIntentPreviewComment(c)
		},
		"workspace": func() (string, error) {
			return RenderIntentPreviewComment(workspaceComment(&PreviewHandoff{State: HandoffReady}, &s))
		},
		"issue": func() (string, error) {
			return RenderVerifyComment(VerifyComment{Namespace: "maul", Intent: "target-1", Round: 1, Attempt: 1,
				Section: s})
		},
	}
	for name, r := range render {
		t.Run(name, func(t *testing.T) {
			out, err := r()
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out, "`outcome`") || strings.Contains(out, "bogus") {
				t.Errorf("a kind is not the outcome's own:\n%s", out)
			}
			for _, o := range s.Outcomes {
				id := code(o.ID)
				var row string
				for line := range strings.SplitSeq(out, "\n") {
					if strings.Contains(line, id) && !strings.HasPrefix(line, "App output of ") {
						if row != "" {
							t.Errorf("%s is listed twice:\n%s\n%s", o.ID, row, line)
						}
						row = line
					}
				}
				if row == "" {
					t.Fatalf("%s has no row:\n%s", o.ID, out)
				}
				if kind := code(kinds[o.ID]); !strings.Contains(row, kind) {
					t.Errorf("%s row %q, want kind %s", o.ID, row, kind)
				}
				if o.Check != "" && !strings.Contains(row, "maul's check: ") {
					t.Errorf("%s row %q has no check line", o.ID, row)
				}
				if o.AppOutput != "" && !strings.Contains(out, "App output of "+id+",") {
					t.Errorf("%s's app output does not name it:\n%s", o.ID, out)
				}
			}
			if !strings.Contains(out, "the program failed without asserting anything about the change") {
				t.Errorf("the unsure outcome's reason is not shown:\n%s", out)
			}
		})
	}
}
