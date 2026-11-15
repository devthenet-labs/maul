// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/acceptance"
)

// The evaluator's note on an outcome it was itself unsure of (gate 5
// G5-B-BR-1): the issue and the pull request show it, as its words, inert
// and bounded, so a person can see what it asked them to judge.

// presetsNote is gate 5 G11's note (beatbattle-109 r1), as the evaluator
// wrote it.
const presetsNote = "The presets wrap onto the line below the Swing slider at 1280px; whether a wrapped row still " +
	"counts as right after the slider is a layout reading for a person"

// unsureSection is G11's first round: the evaluator unsure of the presets'
// placement, with its question, and the swing outcome confirmed.
func unsureSection() VerifySection {
	return VerifySection{
		Progress: VerifyProgress{State: VerifyUnconfirmed, Verification: 1, MaxFixes: 2, Passing: 1, Total: 2,
			Outcomes: true, Why: HandoffWhyUnsure},
		Outcomes: []VerifyOutcomeResult{
			{ID: "light-sets-swing", Kind: "change", Statement: "Light sets the Swing slider to 20.",
				Result: OutcomeResultPassed, Reason: acceptance.ReasonConfirmed,
				Check: "browser: goto /, click button Light; 1 expectation"},
			{ID: "presets-visible-clickable", Kind: "change",
				Statement: "The presets sit right after the Swing slider, visible and clickable.",
				Result:    OutcomeResultUnsure, Reason: acceptance.ReasonNoCheck, UnsureNote: presetsNote},
		},
	}
}

func TestVerifyUnsureNoteGoldens(t *testing.T) {
	for _, tt := range []struct {
		name   string
		render func() (string, error)
	}{
		{"intent_verify_unsure_issue.md", func() (string, error) {
			return RenderVerifyComment(VerifyComment{Namespace: "maul", Intent: "beatbattle-109", Round: 1,
				Attempt: 1, Section: unsureSection()})
		}},
		{"intent_verify_unsure_pr.md", func() (string, error) {
			c := testPreviewComment(PreviewLive, "", "/api")
			s := unsureSection()
			c.Handoff, c.Verify = &PreviewHandoff{State: HandoffUnconfirmed, Why: HandoffWhyUnsure}, &s
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

// TestVerifyUnsureNoteShown: the note is shown, labelled as the
// evaluator's unconfirmed words, inert on both surfaces and bounded; a
// blank note, and a note on a row that is not unsure, show nothing.
func TestVerifyUnsureNoteShown(t *testing.T) {
	const lead = "The evaluator was unsure of `o` too; its note (its words, not checked by maul): "
	for _, tt := range []struct {
		name   string
		result string
		note   string
		want   string // "" for no note shown
	}{
		{"question", OutcomeResultUnsure, presetsNote, lead + presetsNote + "."},
		{"hostile", OutcomeResultUnsure, hostileNote, lead + "The plain path never resets its counter"},
		{"long", OutcomeResultUnsure, strings.Repeat("word ", 400), anomalyCutMarker},
		{"blank", OutcomeResultUnsure, " \t ", ""},
		{"passed", OutcomeResultPassed, presetsNote, ""},
		{"accepted", OutcomeResultAccepted, presetsNote, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := VerifySection{Progress: VerifyProgress{State: VerifyUnconfirmed, Verification: 1, Total: 1,
				Outcomes: true}, Outcomes: []VerifyOutcomeResult{{ID: "o", Result: tt.result,
				Reason: acceptance.ReasonNoCheck, UnsureNote: tt.note}}}
			issue, err := RenderVerifyComment(VerifyComment{Namespace: "maul", Intent: "i", Round: 1, Attempt: 1,
				Section: s})
			if err != nil {
				t.Fatal(err)
			}
			pr, err := RenderIntentPreviewComment(workspaceComment(nil, &s))
			if err != nil {
				t.Fatal(err)
			}
			for name, out := range map[string]string{"issue": issue, "pr": pr} {
				assertInert(t, name, out)
				if why := checkInert(parse(markerRe.ReplaceAllString(out, ""))); why != "" {
					t.Errorf("%s: %s:\n%s", name, why, out)
				}
				shown := strings.Contains(out, "The evaluator was unsure of")
				if tt.want == "" {
					if shown {
						t.Errorf("%s shows a note:\n%s", name, out)
					}
					continue
				}
				if !shown || !strings.Contains(out, tt.want) {
					t.Errorf("%s lacks %q:\n%s", name, tt.want, out)
				}
				if strings.Count(out, "word ") > verifyClaimNoteMaxBytes/len("word ") {
					t.Errorf("%s: the note is not bounded:\n%s", name, out)
				}
			}
		})
	}
}

// TestVerifyUnsureNoteCutNamed: a note left out to fit GitHub's limit is
// named as the evaluator's note, never as a claim.
func TestVerifyUnsureNoteCutNamed(t *testing.T) {
	var left [pieceLog + 1]int
	left[pieceUnsureNote] = 1
	if got := cutNote(left, false); !strings.Contains(got, "1 evaluator's note on an outcome it was unsure of") {
		t.Errorf("cut note = %q", got)
	}
}
