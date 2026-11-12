// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"errors"
	"strings"
	"testing"
)

// testOutcomes are a plan's outcomes as the evaluator's plan comment lists
// them: a change answering an issue line, a keep, and a manual one.
func testOutcomes() *PlanOutcomes {
	return &PlanOutcomes{
		Outcomes: []PlanOutcome{
			{ID: "rows-scroll", Outcome: "the two rows scroll together, 40 px a step", From: "Rows scroll together"},
			{ID: "still-loads", Outcome: "the leaderboard still loads", Keep: true},
			{ID: "sounds-good", Outcome: "it sounds good", Manual: true, Reason: "only a human hears it"},
		},
		NotCovered: []string{"Show the pattern's author"},
	}
}

// TestPlanOutcomesGoldens pins the evaluator's plan comment: each outcome
// with the issue line it answers, and the issue lines none answers.
func TestPlanOutcomesGoldens(t *testing.T) {
	tests := []struct {
		name string
		o    *PlanOutcomes
	}{
		{"intent_plan_outcomes.md", testOutcomes()},
		// A Project with no preview (a cli or library surface): the
		// outcomes are checked in the workspace the tool pod builds, never
		// a running preview.
		{"intent_plan_outcomes_workspace.md", func() *PlanOutcomes {
			o := testOutcomes()
			o.Workspace = true
			return o
		}()},
		{"intent_plan_outcomes_workspace_none.md", &PlanOutcomes{Workspace: true,
			NotCovered: []string{"Rows scroll together"}}},
		// No outcome at all: the evaluator checks nothing, and every issue
		// line is listed.
		{"intent_plan_outcomes_none.md", &PlanOutcomes{
			NotCovered: []string{"Rows scroll together", "Show the pattern's author"}, NotCoveredMore: 3}},
		// Agent and issue text inert: mentions, references, markup and
		// characters that render as nothing.
		{"intent_plan_outcomes_hostile.md", &PlanOutcomes{
			Outcomes: []PlanOutcome{{ID: "inert", Outcome: "closes #3 for @octocat <b>now</b>",
				From: "fixes owner/repo#9 `x`\u200b"}},
			NotCovered: []string{"@octocat see #12 <img src=x>", "[link](https://evil.example)"},
		}},
		// Outcomes the advisory lint flags (acceptance.LintOutcomes): under
		// "Before you approve", each by its id and the lint's own word.
		{"intent_plan_outcomes_flags.md", &PlanOutcomes{
			Outcomes: []PlanOutcome{
				{ID: "own-beat", Outcome: "a beat you save at 90 BPM shows 90 bpm", From: "show the BPM"},
				{ID: "every-beat", Outcome: "every beat links to its page", From: "each beat links to its page"},
				{ID: "beat-links", Outcome: "a beat you create links to its page", From: "each beat links to its page"},
				{ID: "still-works", Outcome: "the sequencer still works", Keep: true},
				{ID: "sounds-good", Outcome: "every drum sounds good", Manual: true},
			},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pc := testPlanComment(testPlan)
			pc.Outcomes = tt.o
			got, err := RenderPlanComment(pc)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			golden(t, tt.name, got)
		})
	}
}

// TestPlanOutcomesFlagsWorkspace (G-B-LIB-3): a Project with no preview
// checks outcomes in a workspace the tool pod builds for each round, which
// holds no other run's data, so the plan comment never warns about a preview whose data persists, nor flags an
// outcome for saying every or each. It still flags "still works", which the
// workspace plan prompt forbids as well, and an outcome narrower than its
// issue line.
func TestPlanOutcomesFlagsWorkspace(t *testing.T) {
	pc := testPlanComment(testPlan)
	pc.Outcomes = &PlanOutcomes{Workspace: true, Outcomes: []PlanOutcome{
		{ID: "every-step", Outcome: "`pattern.Shift` moves every step of a pattern one later", From: "shift steps"},
		{ID: "each-err", Outcome: "`pattern.Validate` refuses each pattern of 65 steps", From: "validate patterns"},
		{ID: "still-works", Outcome: "`pattern.Parse` still works", Keep: true},
		{ID: "one-track", Outcome: "`pattern.Shift` of a kick track moves it", From: "each track shifts"},
	}}
	got, err := RenderPlanComment(pc)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, unwanted := range []string{"preview", "persists", "`every-step` says", "`each-err` says"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("a workspace plan comment says %q:\n%s", unwanted, got)
		}
	}
	for _, want := range []string{"`still-works` says \"still works\"", "`one-track` answers an issue line"} {
		if !strings.Contains(got, want) {
			t.Errorf("a workspace plan comment lacks %q:\n%s", want, got)
		}
	}
	golden(t, "intent_plan_outcomes_flags_workspace.md", got)
}

// TestPlanOutcomesAbsentChangesNothing: without outcomes the comment is
// byte for byte the comment before them, whatever else is set.
func TestPlanOutcomesAbsentChangesNothing(t *testing.T) {
	for _, pc := range []PlanComment{testPlanComment(testPlan), func() PlanComment {
		pc := testPlanComment(testPlan)
		pc.Acceptance = &PlanAcceptance{NotAsked: true}
		return pc
	}()} {
		want, err := RenderPlanComment(pc)
		if err != nil {
			t.Fatal(err)
		}
		pc.Outcomes = nil
		got, err := RenderPlanComment(pc)
		if err != nil {
			t.Fatal(err)
		}
		if got != want || strings.Contains(got, "outcome") {
			t.Errorf("a comment without outcomes changed or mentions them:\n%s", got)
		}
	}
}

// TestPlanOutcomesFit: outcomes that would push a plan comment past
// GitHub's limit are not listed but pointed to in the plan; a plan is
// refused only when it would not fit without the list, so the issue lines
// shown never decide a refusal. SamePlanComment agrees with the render.
func TestPlanOutcomesFit(t *testing.T) {
	const front = "---\nsummary: x\n---\n"
	plan := func(size int) PlanComment {
		body := strings.Repeat(strings.Repeat("a", 63)+"\n", size/64+1)
		return testPlanComment(front + body[:size-len(front)-1] + "\n")
	}
	small, err := RenderPlanComment(plan(64))
	if err != nil {
		t.Fatal(err)
	}
	fits := MaxCommentBytes - (len(small) - 64)

	// A plan that only just fits without outcomes.
	pc := plan(fits - 512)
	pc.Outcomes = testOutcomes()
	for range 40 {
		pc.Outcomes.NotCovered = append(pc.Outcomes.NotCovered, strings.Repeat("an issue line ", 20))
	}
	out, err := RenderPlanComment(pc)
	if err != nil {
		t.Fatalf("a plan that fits without its outcome list: %v", err)
	}
	if len(out) > MaxCommentBytes || !strings.Contains(out, outcomesOmitted) || strings.Contains(out, "rows-scroll") {
		t.Errorf("outcome list not replaced by the pointer (%d bytes):\n%s", len(out), out[:2048])
	}
	if same, err := SamePlanComment(pc, out); err != nil || !same {
		t.Errorf("SamePlanComment of its own render = %t, %v", same, err)
	}

	// One that does not fit even without them is refused as before.
	pc = plan(fits + 1)
	pc.Outcomes = testOutcomes()
	if _, err := RenderPlanComment(pc); !errors.Is(err, ErrPlanRefused) {
		t.Errorf("a plan over the limit without outcomes = %v, want ErrPlanRefused", err)
	}

	// A small plan lists them.
	pc = plan(64)
	pc.Outcomes = testOutcomes()
	if out, err := RenderPlanComment(pc); err != nil || !strings.Contains(out, "`rows-scroll`") {
		t.Errorf("a small plan's outcomes are not listed: %v\n%s", err, out)
	}
}

// TestPlanVerificationOutcomes: a plan Job is told the components its
// outcomes are checked on; with no component nothing is handed at all,
// and the slice-1-era "outcomes" switch is no longer part of the wire.
func TestPlanVerificationOutcomes(t *testing.T) {
	in := &PlanVerification{Components: []PlanVerifyComponent{{Key: "web", Path: "/"}}}
	enc := EncodePlanVerification(in)
	if want := `{"components":[{"key":"web","path":"/"}]}`; enc != want {
		t.Errorf("EncodePlanVerification = %s, want %s", enc, want)
	}
	got, err := DecodePlanVerification(enc)
	if err != nil || got == nil || len(got.Components) != 1 {
		t.Errorf("round trip = %+v, %v", got, err)
	}
	if got, err := DecodePlanVerification(`{"components":[{"key":"web","path":"/"}],"outcomes":true}`); err == nil {
		t.Errorf("the removed outcomes switch decoded: %+v", got)
	}
	if enc := EncodePlanVerification(&PlanVerification{}); enc != "" {
		t.Errorf("no component = %q, want none", enc)
	}
}

// TestReplanNoticeGolden pins the one notice an open intent gets whose
// approved plan states slice-1 criteria once the evaluator is on.
func TestReplanNoticeGolden(t *testing.T) {
	got, err := RenderCriteriaReplanNotice(CriteriaReplanNotice{Namespace: "maul", Intent: "beatbattle-44",
		Key: CriteriaReplanKey(3), PlanRevision: 3, TriggerLabel: "maul:intent"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, NoticeMarker("maul", "beatbattle-44", "verify-replan-r3")+"\n") {
		t.Errorf("notice not headed by its marker:\n%s", got)
	}
	golden(t, "intent_notice_verify_criteria_replan.md", got)
}

// TestCriteriaReplanProgress: the status and preview comments say why
// nothing is verified while the approved plan states slice-1 criteria, and
// what an approver can do in review.
func TestCriteriaReplanProgress(t *testing.T) {
	line, whole := verifyRestingLine(VerifyCriteriaReplan)
	// S1-29: a replan is not available in review, so none is offered.
	if !whole || strings.Contains(line, "replan") || !strings.Contains(line, "/maul cancel") ||
		!strings.Contains(line, "acceptance criteria") {
		t.Errorf("verifyRestingLine(%q) = %q, %t", VerifyCriteriaReplan, line, whole)
	}
}

// TestPlanOutcomesFlagsAreAdvice: the lint's flags show under "Before you
// approve" and nowhere else, name each outcome by its id only, and leave a
// comment with no flagged outcome as it was.
func TestPlanOutcomesFlagsAreAdvice(t *testing.T) {
	pc := testPlanComment(testPlan)
	pc.Outcomes = &PlanOutcomes{Outcomes: []PlanOutcome{
		{ID: "every-beat", Outcome: "every beat <b>@octocat</b>", From: "each beat"}}}
	got, err := RenderPlanComment(pc)
	if err != nil {
		t.Fatal(err)
	}
	before, after, ok := strings.Cut(got, "### Before you approve")
	if !ok {
		t.Fatalf("no Before you approve section:\n%s", got)
	}
	advice, _, _ := strings.Cut(after, "### To approve")
	if !strings.Contains(advice, "- `every-beat` says \"every\"") || strings.Contains(before, "says \"every\"") {
		t.Errorf("the flag is not under Before you approve:\n%s", got)
	}
	if strings.Contains(advice, "octocat") {
		t.Errorf("a flag quotes the outcome's text:\n%s", advice)
	}
	if strings.Contains(advice, "check yourself") {
		t.Errorf("a comment with no narrowed outcome has the narrowed lead:\n%s", advice)
	}
	// A narrowed outcome, as the plan prompt asks for, is not one the
	// evaluator cannot show: it gets its own lead, not the shared-data one.
	pc.Outcomes = &PlanOutcomes{Outcomes: []PlanOutcome{
		{ID: "beat-links", Outcome: "a beat you create links to its page", From: "each beat links to its page"}}}
	if got, err = RenderPlanComment(pc); err != nil {
		t.Fatal(err)
	}
	_, advice, _ = strings.Cut(got, "### Before you approve")
	advice, _, _ = strings.Cut(advice, "### To approve")
	if !strings.Contains(advice, "- `beat-links` answers an issue line that says \"each\"") ||
		!strings.Contains(advice, "approve if that is enough") {
		t.Errorf("the narrowed flag is not under its own lead:\n%s", advice)
	}
	if strings.Contains(advice, "may be ones it cannot show") || strings.Contains(advice, "ask for a new plan") {
		t.Errorf("a narrowed outcome is advised as one the evaluator cannot show:\n%s", advice)
	}
	pc.Outcomes = testOutcomes()
	if got, err = RenderPlanComment(pc); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "data persists") {
		t.Errorf("a comment with no flagged outcome is advised:\n%s", got)
	}
}

// TestPlanOutcomeEndsItsSentence: an outcome whose text already ends its
// sentence is not given a second full stop before the issue line it
// answers (S2 run 2: "exits 0.. It answers").
func TestPlanOutcomeEndsItsSentence(t *testing.T) {
	for _, tc := range []struct{ outcome, want string }{
		{"bb count exits 0", "bb count exits 0. It answers"},
		{"bb count exits 0.", "bb count exits 0. It answers"},
		{"does it exit 0?", "does it exit 0? It answers"},
		{"it exits 0!", "it exits 0! It answers"},
	} {
		_, items, _, _, _ := planOutcomesView(&PlanOutcomes{Outcomes: []PlanOutcome{{ID: "exits",
			Outcome: tc.outcome, From: "exit cleanly"}}})
		if len(items) != 1 || !strings.Contains(items[0], tc.want) {
			t.Errorf("outcome %q: items = %q, want %q", tc.outcome, items, tc.want)
		}
	}
}
