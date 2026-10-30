// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"strings"
	"testing"
)

// TestHandoffGoldens pins the gated hand-off's texts: the status
// comment's preview line and a pull request's preview comment, held (no
// link anywhere), ready, couldn't confirm (with maul's reason) and still
// checking; and the evaluator's verification section and comment, with an
// outcome per result, a check line, app output and an anomaly.
func TestHandoffGoldens(t *testing.T) {
	status := func(h *PreviewHandoff, v *VerifyProgress) func() (string, error) {
		return func() (string, error) {
			p := onePreview(PreviewLive, "")
			p.Handoff, p.Verify = h, v
			return RenderIntentStatusComment(testInReview(p))
		}
	}
	comment := func(h *PreviewHandoff, s *VerifySection) func() (string, error) {
		return func() (string, error) {
			c := testPreviewComment(PreviewLive, "", "")
			c.Handoff, c.Verify = h, s
			return RenderIntentPreviewComment(c)
		}
	}
	evaluating := &VerifyProgress{State: VerifyEvaluating, Verification: 1, MaxFixes: 2, Outcomes: true}
	for _, tt := range []struct {
		name   string
		render func() (string, error)
	}{
		{"intent_status_handoff_held.md", status(&PreviewHandoff{State: HandoffHeld}, evaluating)},
		{"intent_status_handoff_ready.md", status(&PreviewHandoff{State: HandoffReady},
			&VerifyProgress{State: VerifyPassed, Verification: 1, MaxFixes: 2, Passing: 3, Total: 3, Outcomes: true})},
		{"intent_status_handoff_unconfirmed.md", status(&PreviewHandoff{State: HandoffUnconfirmed,
			Why: HandoffWhyAnomaly}, &VerifyProgress{State: VerifyUnconfirmed, Verification: 2, FixesUsed: 1,
			MaxFixes: 2, Passing: 2, Total: 3, Outcomes: true})},
		{"intent_status_handoff_out_of_time.md", status(&PreviewHandoff{State: HandoffUnconfirmed,
			Why: HandoffWhyTime}, evaluating)},
		{"intent_status_handoff_checking.md", status(&PreviewHandoff{State: HandoffChecking}, evaluating)},
		{"intent_preview_handoff_held.md", comment(&PreviewHandoff{State: HandoffHeld}, nil)},
		{"intent_preview_handoff_ready.md", comment(&PreviewHandoff{State: HandoffReady}, testOutcomeSection())},
		{"intent_preview_handoff_unconfirmed.md", comment(&PreviewHandoff{State: HandoffUnconfirmed,
			Why: HandoffWhySandbox}, nil)},
		{"intent_verify_outcomes_issue.md", func() (string, error) {
			return RenderVerifyComment(VerifyComment{Namespace: "maul", Intent: "target-1", Round: 2, Attempt: 1,
				Section: *testOutcomeSection()})
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.render()
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			golden(t, tt.name, got)
		})
	}
}

// testOutcomeSection is an evaluation with one outcome of each result: a
// confirmed one and its check, a failing one with hostile app output, an
// unsure one, and one an approver accepted; and a hostile anomaly.
func testOutcomeSection() *VerifySection {
	return &VerifySection{
		Progress: VerifyProgress{State: VerifyFailing, Verification: 2, FixesUsed: 1, MaxFixes: 2, Passing: 2,
			Total: 4, Outcomes: true, AutoFix: FixQueued},
		Outcomes: []VerifyOutcomeResult{
			{ID: "winner-rises", Statement: "Rating a battle moves the winner above the loser.",
				Result: OutcomeResultPassed, Reason: "confirmed",
				Check: "http to api: POST /api/players, POST /api/battles, GET /api/leaderboard; 3 expectations"},
			{ID: "upset-gains-more", Statement: "Beating a higher-rated player gains more points.",
				Result: OutcomeResultFailed, Reason: "reproduced",
				Check:     "http to api: POST /api/battles, GET /api/players/{{run}}-a; 2 expectations",
				AppOutput: "step 2: expected /delta 24, got 16\n@octocat see #12 fixes #3\n```\n</details>"},
			{ID: "leaderboard-loads", Statement: "The leaderboard still lists every player.",
				Result: OutcomeResultUnsure, Reason: "model-only-pass"},
			{ID: "looks-right", Statement: "The board looks right.", Result: OutcomeResultAccepted,
				Reason: "manual", AcceptedAt: testWebHead},
		},
		Anomalies: []VerifyAnomaly{{Summary: "console error on /battle @octocat #7", Route: "/battle"}},
	}
}

// TestHandoffHeldShowsNoLink: a held hand-off never shows the preview's
// host, on the status line or a pull request's comment, whatever the
// host; an unknown state is held too.
func TestHandoffHeldShowsNoLink(t *testing.T) {
	for _, state := range []string{HandoffHeld, "", "bogus"} {
		p := onePreview(PreviewLive, "")
		p.Handoff = &PreviewHandoff{State: state}
		s, err := RenderIntentStatusComment(testInReview(p))
		if err != nil {
			t.Fatal(err)
		}
		c := testPreviewComment(PreviewLive, "", "/api")
		c.Handoff = &PreviewHandoff{State: state}
		pc, err := RenderIntentPreviewComment(c)
		if err != nil {
			t.Fatal(err)
		}
		for _, out := range []string{s, pc} {
			if strings.Contains(out, "preview.maul.example.com") || strings.Contains(out, "https://target-1") {
				t.Errorf("state %q shows the link:\n%s", state, out)
			}
		}
	}
}

// TestHandoffWhyIsMaulsWords: an unconfirmed hand-off's reason is one of
// maul's own sentences; an unknown reason is the generic one, never the
// text it carried.
func TestHandoffWhyIsMaulsWords(t *testing.T) {
	for why := range handoffWhyWords {
		if got := handoffLabel(PreviewHandoff{State: HandoffUnconfirmed, Why: why}); !strings.Contains(got,
			handoffWhyWords[why]) {
			t.Errorf("why %q: %q", why, got)
		}
	}
	got := handoffLabel(PreviewHandoff{State: HandoffUnconfirmed, Why: "@octocat #3"})
	if strings.Contains(got, "octocat") || !strings.Contains(got, handoffWhyWords[HandoffWhyUnsure]) {
		t.Errorf("unknown why = %q", got)
	}
}

// TestAppOutputInert: the application's output is shown only fenced, in a
// fence no line of it closes, defanged, and bounded; never for an outcome
// that did not fail.
func TestAppOutputInert(t *testing.T) {
	hostile := strings.Repeat("`", 9) + "\n@octocat fixes #3\n" + strings.Repeat("x", 4<<10)
	s := VerifySection{Progress: VerifyProgress{State: VerifyFailing, Outcomes: true}, Outcomes: []VerifyOutcomeResult{
		{ID: "a", Result: OutcomeResultFailed, AppOutput: hostile},
		{ID: "b", Result: OutcomeResultPassed, AppOutput: "SECRET-PASSED-OUTPUT"},
	}}
	out, err := renderVerifySection(&s)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "SECRET-PASSED-OUTPUT") {
		t.Error("app output shown for an outcome that did not fail")
	}
	if strings.Contains(out, "@octocat") || strings.Contains(out, " #3") {
		t.Errorf("app output not defanged:\n%s", out)
	}
	if !strings.Contains(out, strings.Repeat("`", 10)+"text\n") {
		t.Errorf("app output not in a fence its own backticks cannot close:\n%s", out)
	}
	if strings.Count(out, "x") > VerifyAppOutputMaxBytes {
		t.Errorf("app output not bounded: %d bytes of it", strings.Count(out, "x"))
	}
}
