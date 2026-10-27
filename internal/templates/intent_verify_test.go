// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"regexp"
	"strings"
	"testing"
)

const hostileText = "@octocat fixes #12 closes owner/repo#3 \u202e<b>x</b> `tick` [l](https://evil.example)"

func testVerifyProgress() VerifyProgress {
	return VerifyProgress{State: VerifyRunning, Round: 1, MaxRounds: 2, Passing: 3, Total: 4}
}

func testVerifySection() VerifySection {
	return VerifySection{
		Progress: VerifyProgress{State: VerifyFailing, Round: 1, MaxRounds: 2, Passing: 2, Total: 4,
			Quarantined: 1, ManualOpen: 1, AutoFix: FixQueued},
		Criteria: []VerifyCriterion{
			{ID: "render", Kind: "http", Statement: "the home page renders", Outcome: VerifyOutcomePass},
			{ID: "battle-leaderboard", Kind: "http", Statement: "a leaderboard ranks patterns by votes",
				Outcome:  VerifyOutcomeFail,
				Expected: "GET /api/battles/b1/leaderboard: status 200, /0/id = p2",
				Observed: "status 500", Body: "{\"error\":\"boom\"}"},
			{ID: "flaky", Kind: "http", Statement: "a flaky one", Outcome: VerifyOutcomeQuarantined},
			{ID: "kick-onsets", Kind: "command", Statement: "kicks land on the beat", Outcome: VerifyOutcomeNotRun},
			{ID: "sounds-good", Kind: "manual", Statement: "it sounds good", Outcome: VerifyOutcomeManual},
		},
	}
}

func TestVerifyGoldens(t *testing.T) {
	withLogs := testVerifySection()
	withLogs.Logs = []VerifyLog{{Part: "beatbattle-api", Tail: "listening on :8080\npanic: boom"}}
	withLogs.ChangedTests = true
	withLogs.ServicesRestarted = true
	withLogs.Impostor = "mallory"
	passed := VerifySection{
		Progress: VerifyProgress{State: VerifyPassed, Round: 2, MaxRounds: 2, Passing: 2, Total: 2},
		Criteria: []VerifyCriterion{
			{ID: "a", Kind: "http", Statement: "a", Outcome: VerifyOutcomePass},
			{ID: "b", Kind: "manual", Statement: "b", Outcome: VerifyOutcomeAccepted, AcceptedAt: testWebHead},
		},
	}
	tests := []struct {
		name   string
		render func() (string, error)
	}{
		{"intent_verify_issue.md", func() (string, error) {
			return RenderVerifyComment(VerifyComment{Namespace: "maul", Intent: "marigold-3", Round: 1, Attempt: 2,
				Section: testVerifySection()})
		}},
		{"intent_verify_issue_logs_flags.md", func() (string, error) {
			return RenderVerifyComment(VerifyComment{Namespace: "maul", Intent: "marigold-3", Round: 1, Attempt: 1,
				Section: withLogs})
		}},
		{"intent_verify_issue_passed.md", func() (string, error) {
			return RenderVerifyComment(VerifyComment{Namespace: "maul", Intent: "marigold-3", Round: 2, Attempt: 1,
				Section: passed})
		}},
		{"intent_preview_live_verify.md", func() (string, error) {
			c := testPreviewComment(PreviewLive, "", "/api")
			s := testVerifySection()
			c.Verify = &s
			return RenderIntentPreviewComment(c)
		}},
		{"intent_preview_live_verify_passed.md", func() (string, error) {
			c := testPreviewComment(PreviewLive, "", "")
			c.Verify = &passed
			return RenderIntentPreviewComment(c)
		}},
		{"intent_status_preview_verifying.md", func() (string, error) {
			p := onePreview(PreviewLive, "")
			v := testVerifyProgress()
			v.AutoFix, v.AutoFixReason = FixHalted, "the fix budget is spent"
			p.Verify = &v
			return RenderIntentStatusComment(testInReview(p))
		}},
		{"intent_plan_acceptance.md", func() (string, error) {
			pc := testPlanComment(testPlan)
			pc.Acceptance = &PlanAcceptance{Automated: 2, NotRun: 1,
				Manual: []PlanManualCriterion{{ID: "sounds-good", Reason: "only a human can judge how it sounds"}}}
			return RenderPlanComment(pc)
		}},
		{"intent_plan_acceptance_off.md", func() (string, error) {
			pc := testPlanComment(testPlan)
			pc.Acceptance = &PlanAcceptance{Off: true}
			return RenderPlanComment(pc)
		}},
		{"prompt_plan_verification.md", func() (string, error) {
			return RenderPlanPrompt(PlanPrompt{
				IssuePath: "/workspace/input/issue.md", ReportPath: "/workspace/reports/plan.md",
				Intent: testPlanRequest, BuildMaxTurns: 150, BuildTokenBudget: 800000, Limits: testPlanLimits,
				Verification: &PlanVerification{Components: []PlanVerifyComponent{
					{Key: "web", Path: "/", Readiness: "/healthz"},
					{Key: "api", Path: "/api", Readiness: "/api/healthz"},
				}},
			})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.render()
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			golden(t, tt.name, got)
		})
	}
}

// TestVerificationAbsentChangesNothing: without verification input every
// surface renders byte for byte as before.
func TestVerificationAbsentChangesNothing(t *testing.T) {
	base, err := RenderPlanComment(testPlanComment(testPlan))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(base, "Acceptance") {
		t.Errorf("plan comment without acceptance mentions it:\n%s", base)
	}
	prompt, err := renderTestPlanPrompt(testPlanRequest, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(prompt, "acceptance") {
		t.Errorf("plan prompt without verification teaches acceptance")
	}
	pre, _ := RenderIntentPreviewComment(testPreviewComment(PreviewLive, "", ""))
	c := testPreviewComment(PreviewLive, "", "")
	c.Verify = &VerifySection{}
	pre2, _ := RenderIntentPreviewComment(c)
	if pre != pre2 {
		t.Errorf("an empty verify section changed the preview comment")
	}
	st, _ := RenderIntentStatusComment(testInReview(onePreview(PreviewLive, "")))
	p := onePreview(PreviewLive, "")
	p.Verify = &VerifyProgress{}
	st2, _ := RenderIntentStatusComment(testInReview(p))
	if st != st2 {
		t.Errorf("a zero progress changed the status comment")
	}
}

// Fenced blocks, code spans and maul's own preview links are inert; what is
// left must hold no live mention, reference, link or HTML.
var (
	liveRe   = regexp.MustCompile(`(^|[^A-Za-z0-9])@[A-Za-z0-9]|#[0-9]|(^|[^\\])<[a-z/]|\]\(|\x{202e}`)
	markerRe = regexp.MustCompile(`(?m)^<!-- maul:notice [^\n]* -->$`)
	linkRe   = regexp.MustCompile(`\[[^\]]*\]\(https://[a-z0-9.-]+\)`)
)

// backtickRun is the length of the run of backticks at s[i:].
func backtickRun(s string, i int) int {
	n := 0
	for i+n < len(s) && s[i+n] == '`' {
		n++
	}
	return n
}

// stripFences drops each fenced block: from a line of three or more
// backticks to the next line of only as many or more.
func stripFences(s string) string {
	var out []string
	closeAt := 0
	for _, line := range strings.Split(s, "\n") {
		switch {
		case closeAt > 0:
			if backtickRun(line, 0) >= closeAt && strings.Trim(line, "`") == "" {
				closeAt = 0
			}
		case backtickRun(line, 0) >= 3:
			closeAt = backtickRun(line, 0)
		default:
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// stripSpans drops each code span: a backtick run to the next run of the
// same length.
func stripSpans(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '`' {
			b.WriteByte(s[i])
			i++
			continue
		}
		n := backtickRun(s, i)
		end := -1
		for j := i + n; j < len(s); {
			if s[j] != '`' {
				j++
				continue
			}
			m := backtickRun(s, j)
			if m == n {
				end = j + m
				break
			}
			j += m
		}
		if end < 0 {
			b.WriteString(s[i : i+n])
			i += n
			continue
		}
		i = end
	}
	return b.String()
}

func assertInert(t *testing.T, name, out string) {
	t.Helper()
	rest := markerRe.ReplaceAllString(out, "")
	rest = stripFences(rest)
	rest = linkRe.ReplaceAllString(rest, "")
	rest = stripSpans(rest)
	if liveRe.MatchString(rest) {
		t.Errorf("%s: agent text is live outside code:\n%s", name, rest)
	}
}

// TestVerifyTextInert: every agent- or app-written field, hostile, renders
// inert on every surface.
func TestVerifyTextInert(t *testing.T) {
	sec := VerifySection{
		Progress: VerifyProgress{State: VerifyFailing, Round: 1, MaxRounds: 2, Passing: 1, Total: 2,
			AutoFix: FixHalted, AutoFixReason: hostileText},
		Criteria: []VerifyCriterion{
			{ID: hostileText, Kind: hostileText, Statement: hostileText, Outcome: VerifyOutcomeFail,
				Expected: hostileText, Observed: hostileText, Body: hostileText + "\n```\n@octocat\n```````"},
			{ID: "m", Kind: "manual", Statement: hostileText, Outcome: VerifyOutcomeAccepted, AcceptedAt: hostileText},
		},
		Logs:     []VerifyLog{{Part: hostileText, Tail: hostileText + "\n````\n#4"}},
		Impostor: hostileText,
	}
	issue, err := RenderVerifyComment(VerifyComment{Namespace: "n", Intent: "i", Round: 1, Attempt: 1, Section: sec})
	if err != nil {
		t.Fatal(err)
	}
	assertInert(t, "issue", issue)
	c := testPreviewComment(PreviewLive, "", "")
	c.Verify = &sec
	pr, err := RenderIntentPreviewComment(c)
	if err != nil {
		t.Fatal(err)
	}
	assertInert(t, "pr", pr)
	p := onePreview(PreviewLive, "")
	p.Verify = &VerifyProgress{State: VerifyFailing, Round: 1, MaxRounds: 2, AutoFix: FixHalted,
		AutoFixReason: hostileText, Total: 1}
	// No pull requests: their links are maul's own, "#N" text included.
	st, err := RenderIntentStatusComment(IntentStatusComment{
		Namespace: "maul", Intent: "target-1", Phase: "InReview", Preview: p})
	if err != nil {
		t.Fatal(err)
	}
	assertInert(t, "status", st)
	pc := testPlanComment(testPlan)
	pc.Acceptance = &PlanAcceptance{Manual: []PlanManualCriterion{{ID: hostileText, Reason: hostileText}}}
	plan, err := RenderPlanComment(pc)
	if err != nil {
		t.Fatal(err)
	}
	head, _, _ := strings.Cut(plan, "### The plan")
	assertInert(t, "plan header", head)
}

// TestVerifyEvidenceBounded: evidence is cut per criterion and the comment
// stays under GitHub's limit, dropping log tails first.
func TestVerifyEvidenceBounded(t *testing.T) {
	big := strings.Repeat("x", 100_000)
	sec := VerifySection{Progress: VerifyProgress{State: VerifyFailing, Round: 1, MaxRounds: 1, Total: 16}}
	for i := range 16 {
		sec.Criteria = append(sec.Criteria, VerifyCriterion{ID: "c" + string(rune('a'+i)), Kind: "http",
			Statement: "s", Outcome: VerifyOutcomeFail, Expected: big, Observed: big, Body: big})
	}
	for i := range 8 {
		sec.Logs = append(sec.Logs, VerifyLog{Part: "p" + string(rune('a'+i)), Tail: big})
	}
	out, err := RenderVerifyComment(VerifyComment{Namespace: "n", Intent: "i", Round: 1, Attempt: 1, Section: sec})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > MaxCommentBytes {
		t.Errorf("comment is %d bytes, over the limit", len(out))
	}
	c := testPreviewComment(PreviewLive, "", "")
	noLogs := sec
	noLogs.Logs = nil
	c.Verify = &noLogs
	pr, err := RenderIntentPreviewComment(c)
	if err != nil {
		t.Fatal(err)
	}
	if len(pr) > MaxCommentBytes {
		t.Errorf("pr comment is %d bytes, over the limit", len(pr))
	}
	if strings.Count(pr, strings.Repeat("x", VerifyEvidenceMaxBytes+1)) != 0 {
		t.Errorf("evidence is not cut at %d bytes", VerifyEvidenceMaxBytes)
	}
}

func TestVerifyKeys(t *testing.T) {
	if VerifyKey(2) == VerifyKey(3) || VerifyKey(1) == PreviewKey {
		t.Error("verify keys collide")
	}
	got, err := RenderVerifyComment(VerifyComment{Namespace: "n", Intent: "i", Round: 2, Attempt: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, NoticeMarker("n", "i", VerifyKey(2))+"\n") {
		t.Errorf("verify comment is not headed by its marker:\n%s", got)
	}
}
