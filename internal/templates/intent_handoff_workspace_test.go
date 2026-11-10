// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"strings"
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
)

// workspacePreview is a Project with no preview (SPEC-S2): the status
// line is the evaluation's. The host and components are set to show that
// nothing of them is ever rendered.
func workspacePreview(h *PreviewHandoff, v *VerifyProgress) *IntentPreview {
	p := onePreview(PreviewLive, "")
	p.Workspace, p.Handoff, p.Verify = true, h, v
	return p
}

// workspaceComment is a pull request's evaluation comment on a Project
// with no preview.
func workspaceComment(h *PreviewHandoff, s *VerifySection) IntentPreviewComment {
	c := testPreviewComment(PreviewLive, "", "/api")
	c.Workspace, c.Handoff, c.Verify = true, h, s
	return c
}

// TestWorkspaceHandoffGoldens pins the link-less hand-off (SPEC-S2 S2-15):
// the status comment's evaluation line and a pull request's evaluation
// comment, held, ready, couldn't confirm for each agents-namespace reason,
// still checking, with no hand-off at all, and once the intent ended; and
// the outcome reasons slice S2 adds.
func TestWorkspaceHandoffGoldens(t *testing.T) {
	status := func(h *PreviewHandoff, v *VerifyProgress) func() (string, error) {
		return func() (string, error) { return RenderIntentStatusComment(testInReview(workspacePreview(h, v))) }
	}
	comment := func(h *PreviewHandoff, s *VerifySection) func() (string, error) {
		return func() (string, error) { return RenderIntentPreviewComment(workspaceComment(h, s)) }
	}
	evaluating := &VerifyProgress{State: VerifyEvaluating, Verification: 1, MaxFixes: 2, Outcomes: true}
	unconfirmed := func(why string) *PreviewHandoff { return &PreviewHandoff{State: HandoffUnconfirmed, Why: why} }
	for _, tt := range []struct {
		name   string
		render func() (string, error)
	}{
		{"intent_status_workspace_held.md", status(&PreviewHandoff{State: HandoffHeld}, evaluating)},
		{"intent_status_workspace_ready.md", status(&PreviewHandoff{State: HandoffReady},
			&VerifyProgress{State: VerifyPassed, Verification: 1, MaxFixes: 2, Passing: 2, Total: 2, Outcomes: true})},
		{"intent_status_workspace_build_failed.md", status(unconfirmed(HandoffWhyBuildFailed),
			&VerifyProgress{State: VerifyUnconfirmed, Verification: 1, MaxFixes: 2, Total: 2, Outcomes: true})},
		{"intent_status_workspace_sandbox_image.md", status(unconfirmed(HandoffWhySandboxImage), nil)},
		{"intent_status_workspace_tree.md", status(unconfirmed(HandoffWhyTree), nil)},
		{"intent_status_workspace_replay.md", status(unconfirmed(HandoffWhyReplay), nil)},
		// Review H2: every outcome confirmed, the round's environment not
		// clean: the line names that, never a preview.
		{"intent_status_workspace_environment.md", status(unconfirmed(HandoffWhyEnvironment),
			&VerifyProgress{State: VerifyUnconfirmed, Verification: 1, MaxFixes: 2, Passing: 2, Total: 2,
				Outcomes: true, Why: HandoffWhyEnvironment, Workspace: true})},
		{"intent_status_workspace_checking.md", status(&PreviewHandoff{State: HandoffChecking}, evaluating)},
		{"intent_status_workspace_early.md", status(nil, evaluating)},
		{"intent_evaluation_held.md", comment(&PreviewHandoff{State: HandoffHeld}, nil)},
		{"intent_evaluation_ready.md", comment(&PreviewHandoff{State: HandoffReady}, testWorkspaceSection())},
		{"intent_evaluation_build_failed.md", comment(unconfirmed(HandoffWhyBuildFailed),
			testBuildFailedSection())},
		{"intent_evaluation_checking.md", comment(&PreviewHandoff{State: HandoffChecking}, nil)},
		{"intent_evaluation_removed.md", func() (string, error) {
			c := workspaceComment(&PreviewHandoff{State: HandoffReady}, testWorkspaceSection())
			c.State = PreviewRemoved
			return RenderIntentPreviewComment(c)
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

// testWorkspaceSection is a cli evaluation: one outcome confirmed, one
// whose program failed without asserting (O4).
func testWorkspaceSection() *VerifySection {
	return &VerifySection{
		Progress: VerifyProgress{State: VerifyUnconfirmed, Verification: 1, MaxFixes: 2, Passing: 1, Total: 2,
			Outcomes: true},
		Outcomes: []VerifyOutcomeResult{
			{ID: "onsets-500ms", Statement: "`bbrender --bpm 120 kick.json` writes kick onsets 500 ms apart.",
				Result: OutcomeResultPassed, Reason: acceptance.ReasonConfirmed,
				Check: "run (go) of {{bin}}: 1 file; exit 0, stdout JSON"},
			{ID: "bpm-flag", Statement: "`bbrender --bpm 90` writes a slower beat.",
				Result: OutcomeResultUnsure, Reason: acceptance.ReasonNotAsserted,
				Check: "exec of {{bin}}: --bpm 90; exit 0, stdout contains"},
		},
	}
}

// testBuildFailedSection is an evaluation whose head did not build in the
// tool pod: every outcome unsure, tools-build-failed.
func testBuildFailedSection() *VerifySection {
	return &VerifySection{
		Progress: VerifyProgress{State: VerifyUnconfirmed, Verification: 1, MaxFixes: 2, Total: 1, Outcomes: true},
		Outcomes: []VerifyOutcomeResult{{ID: "parse-too-long",
			Statement: "`Parse` rejects a pattern longer than 64 steps with `ErrTooLong`.",
			Result:    OutcomeResultUnsure, Reason: acceptance.ReasonBuildFailed}},
	}
}

// TestWorkspaceHandoffShowsNoLink: a Project with no preview is never
// linked, whatever the hand-off and whatever host or state a caller left
// set; nothing says "preview" but the comment's marker.
func TestWorkspaceHandoffShowsNoLink(t *testing.T) {
	states := []*PreviewHandoff{nil, {State: HandoffHeld}, {State: HandoffReady}, {State: HandoffChecking},
		{State: HandoffUnconfirmed, Why: HandoffWhyBuildFailed}, {State: "bogus"}}
	for _, h := range states {
		for _, state := range []string{PreviewLive, PreviewUpdating, PreviewUnavailable, ""} {
			p := workspacePreview(h, &VerifyProgress{State: VerifyEvaluating, Outcomes: true})
			p.State = state
			s, err := RenderIntentStatusComment(testInReview(p))
			if err != nil {
				t.Fatal(err)
			}
			c := workspaceComment(h, testWorkspaceSection())
			c.State = state
			pc, err := RenderIntentPreviewComment(c)
			if err != nil {
				t.Fatal(err)
			}
			for _, out := range []string{s, pc} {
				body := strings.ReplaceAll(out, NoticeMarker("maul", "marigold-3", PreviewKey), "")
				for _, gone := range []string{"preview.maul.example.com", "https://target-1", "https://marigold",
					"**Preview:**", "preview"} {
					if strings.Contains(body, gone) {
						t.Errorf("handoff %+v, state %q shows %q:\n%s", h, state, gone, out)
					}
				}
			}
		}
	}
}

// TestWorkspaceStatusShowsNothingWithoutAHandoffOrProgress: a workspace
// line with neither says nothing, so the status comment is the one with
// no preview at all.
func TestWorkspaceStatusShowsNothingWithoutAHandoffOrProgress(t *testing.T) {
	got, err := RenderIntentStatusComment(testInReview(workspacePreview(nil, nil)))
	if err != nil {
		t.Fatal(err)
	}
	want, err := RenderIntentStatusComment(testInReview(nil))
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("a bare workspace line rendered:\n%s", got)
	}
}

// TestHandoffWhyOfVerification: every agents-namespace PreviewVerification
// reason slice S2 adds has maul's words on an unconfirmed hand-off, or is
// none (a saturated tool pod bound waits, it never gives up).
func TestHandoffWhyOfVerification(t *testing.T) {
	for _, tt := range []struct {
		reason v1alpha1.PreviewVerificationReason
		want   string
	}{
		{v1alpha1.VerificationReasonToolsBuildFailed, HandoffWhyBuildFailed},
		{v1alpha1.VerificationReasonSandboxImageUnavailable, HandoffWhySandboxImage},
		{v1alpha1.VerificationReasonTreeUnavailable, HandoffWhyTree},
		{v1alpha1.VerificationReasonSandboxUnenforced, HandoffWhySandbox},
		{v1alpha1.VerificationReasonToolsEvicted, HandoffWhyUnavailable},
		{v1alpha1.VerificationReasonToolsDeadline, HandoffWhyUnavailable},
		{v1alpha1.VerificationReasonReplayIncomplete, HandoffWhyReplay},
		{v1alpha1.VerificationReasonToolPodsSaturated, ""},
		{v1alpha1.VerificationReasonOK, ""},
		{"@octocat", ""},
	} {
		got := HandoffWhyOfVerification(tt.reason)
		if got != tt.want {
			t.Errorf("%s: %q, want %q", tt.reason, got, tt.want)
		}
		if got != "" && !strings.Contains(handoffLabel(PreviewHandoff{State: HandoffUnconfirmed, Why: got}),
			handoffWhyWords[got]) {
			t.Errorf("%s: why %q has no words of its own", tt.reason, got)
		}
	}
}

// TestOutcomeReasonWordsS2: the outcome reasons slice S2 adds to Decide
// are worded as SPEC-S2 S2-15 states them.
func TestOutcomeReasonWordsS2(t *testing.T) {
	for reason, want := range map[string]string{
		acceptance.ReasonNotAsserted: "the program failed without asserting anything about the change",
		acceptance.ReasonBuildFailed: "couldn't confirm: the build did not finish",
		// S2-20: a check that passed, withheld by its base runs alone.
		acceptance.ReasonNoBaseEvidence: "its check passed, but its runs at the base do not tell the change " +
			"from its absence",
	} {
		got := outcomeVerdict(VerifyOutcomeResult{ID: "a", Result: OutcomeResultUnsure, Reason: reason},
			outcomeResultWords[OutcomeResultUnsure])
		if !strings.Contains(got, want) {
			t.Errorf("%s: %q, want it to say %q", reason, got, want)
		}
	}
}
