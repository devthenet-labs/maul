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

// TestPlanOutcomesAbsentChangesNothing: without outcomes the comment is
// byte for byte the comment before them, whatever else is set.
func TestPlanOutcomesAbsentChangesNothing(t *testing.T) {
	for _, pc := range []PlanComment{testPlanComment(testPlan), func() PlanComment {
		pc := testPlanComment(testPlan)
		pc.Acceptance = &PlanAcceptance{Automated: 1}
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

// TestPlanVerificationOutcomes: a plan Job is told the evaluator asks for
// outcomes beside the components; without it the handoff is byte for byte
// what it was, and with no component nothing is handed at all.
func TestPlanVerificationOutcomes(t *testing.T) {
	in := &PlanVerification{Components: []PlanVerifyComponent{{Key: "web", Path: "/"}}, Outcomes: true}
	enc := EncodePlanVerification(in)
	if want := `{"components":[{"key":"web","path":"/"}],"outcomes":true}`; enc != want {
		t.Errorf("EncodePlanVerification = %s, want %s", enc, want)
	}
	got, err := DecodePlanVerification(enc)
	if err != nil || got == nil || !got.Outcomes || len(got.Components) != 1 {
		t.Errorf("round trip = %+v, %v", got, err)
	}
	in.Outcomes = false
	if enc := EncodePlanVerification(in); strings.Contains(enc, "outcomes") {
		t.Errorf("without the evaluator the handoff names outcomes: %s", enc)
	}
	if enc := EncodePlanVerification(&PlanVerification{Outcomes: true}); enc != "" {
		t.Errorf("outcomes with no component = %q, want none", enc)
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
// nothing is verified while the approved plan states slice-1 criteria.
func TestCriteriaReplanProgress(t *testing.T) {
	line, whole := verifyRestingLine(VerifyCriteriaReplan)
	if !whole || !strings.Contains(line, "/maul replan") || !strings.Contains(line, "outcomes") {
		t.Errorf("verifyRestingLine(%q) = %q, %t", VerifyCriteriaReplan, line, whole)
	}
}
