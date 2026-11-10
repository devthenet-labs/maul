// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/command"
)

const hostileText = "@octocat fixes #12 closes owner/repo#3 \u202e<b>x</b> `tick` [l](https://evil.example)"

func testVerifyProgress() VerifyProgress {
	return VerifyProgress{State: VerifyRunning, Verification: 1, FixesUsed: 0, MaxFixes: 2, Passing: 3, Total: 4}
}

// testVerifySection is an outcome round failing, with its notes: an
// impostor's status, a no-defect conclusion (its mention and markup made
// inert) and a component's log tail.
func testVerifySection() VerifySection {
	return VerifySection{
		Progress: VerifyProgress{State: VerifyFailing, Verification: 1, FixesUsed: 1, MaxFixes: 2, Passing: 1,
			Total: 2, AutoFix: FixHalted, AutoFixReason: "FixFoundNoDefect", Outcomes: true},
		Outcomes: []VerifyOutcomeResult{
			{ID: "home-renders", Kind: "keep", Statement: "the home page still renders", Result: OutcomeResultPassed,
				Check: "http to web: GET /; 1 expectation"},
			{ID: "leaderboard-ranks", Kind: "change", Statement: "a leaderboard ranks patterns by votes",
				Result: OutcomeResultFailed, Check: "http to api: GET /api/battles/{{run}}/leaderboard; 2 expectations",
				AppOutput: "{\"error\":\"boom\"}"},
		},
		Logs:     []VerifyLog{{Part: "beatbattle-api", Tail: "listening on :8080\npanic: boom"}},
		Impostor: "mallory",
		NoDefect: &VerifyNoDefect{Run: "marigold-3-rev2-api-a1",
			Reason: "each outcome holds when replayed by hand; @octocat the <b>network policy</b> was late"},
	}
}

func TestVerifyGoldens(t *testing.T) {
	tests := []struct {
		name   string
		render func() (string, error)
	}{
		// The shared notes of a verification comment, on an outcome round.
		{"intent_verify_outcomes_issue_notes.md", func() (string, error) {
			return RenderVerifyComment(VerifyComment{Namespace: "maul", Intent: "marigold-3", Round: 1, Attempt: 2,
				Section: testVerifySection()})
		}},
		{"intent_preview_live_verify_outcomes_notes.md", func() (string, error) {
			c := testPreviewComment(PreviewLive, "", "/api")
			s := testVerifySection()
			c.Verify = &s
			return RenderIntentPreviewComment(c)
		}},
		{"intent_status_preview_verifying.md", func() (string, error) {
			p := onePreview(PreviewLive, "")
			v := testVerifyProgress()
			v.AutoFix, v.AutoFixReason = FixHalted, "the fix budget is spent"
			p.Verify = &v
			return RenderIntentStatusComment(testInReview(p))
		}},
		{"intent_plan_acceptance_not_asked.md", func() (string, error) {
			pc := testPlanComment(testPlan)
			pc.Acceptance = &PlanAcceptance{NotAsked: true}
			return RenderPlanComment(pc)
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
		Progress: VerifyProgress{State: VerifyFailing, Verification: 1, FixesUsed: 0, MaxFixes: 2, Passing: 1, Total: 2,
			AutoFix: FixHalted, AutoFixReason: hostileText, Outcomes: true},
		Outcomes: []VerifyOutcomeResult{
			{ID: hostileText, Kind: hostileText, Statement: hostileText, Result: OutcomeResultFailed,
				Reason: hostileText, Check: hostileText, AppOutput: hostileText + "\n```\n@octocat\n```````"},
			{ID: "m", Kind: "keep", Statement: hostileText, Result: OutcomeResultAccepted, AcceptedAt: hostileText},
		},
		Anomalies: []VerifyAnomaly{{Summary: hostileText, Route: hostileText}},
		Logs:      []VerifyLog{{Part: hostileText, Tail: hostileText + "\n````\n#4"}},
		Impostor:  hostileText,
		NoDefect:  &VerifyNoDefect{Run: hostileText, Reason: hostileText},
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
	p.Verify = &VerifyProgress{State: VerifyFailing, Verification: 1, FixesUsed: 0, MaxFixes: 2, AutoFix: FixHalted,
		AutoFixReason: hostileText, Total: 1}
	// No pull requests: their links are maul's own, "#N" text included.
	st, err := RenderIntentStatusComment(IntentStatusComment{
		Namespace: "maul", Intent: "target-1", Phase: "InReview", Preview: p})
	if err != nil {
		t.Fatal(err)
	}
	assertInert(t, "status", st)
}

// TestVerifyEvidenceBounded: evidence is cut per outcome and the comment
// stays under GitHub's limit, dropping log tails first.
func TestVerifyEvidenceBounded(t *testing.T) {
	big := strings.Repeat("x", 100_000)
	sec := VerifySection{Progress: VerifyProgress{State: VerifyFailing, Verification: 1, FixesUsed: 0, MaxFixes: 1,
		Total: 16, Outcomes: true}}
	for i := range 16 {
		sec.Outcomes = append(sec.Outcomes, VerifyOutcomeResult{ID: "o" + string(rune('a'+i)), Kind: "change",
			Statement: "s", Result: OutcomeResultFailed, Check: big, AppOutput: big})
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
	if strings.Count(pr, strings.Repeat("x", VerifyAppOutputMaxBytes+1)) != 0 {
		t.Errorf("evidence is not cut at %d bytes", VerifyAppOutputMaxBytes)
	}
}

// TestVerifyLogsBounded: a comment carries at most verifyLogsMax log
// tails, in order, and none for a part whose tail is empty once made
// visible.
func TestVerifyLogsBounded(t *testing.T) {
	sec := testVerifySection()
	sec.Logs = []VerifyLog{{Part: "blank", Tail: " \n\t"}}
	for i := range verifyLogsMax + 1 {
		sec.Logs = append(sec.Logs, VerifyLog{Part: fmt.Sprintf("part-%d", i), Tail: fmt.Sprintf("tail %d", i)})
	}
	out, err := RenderVerifyComment(VerifyComment{Namespace: "n", Intent: "i", Round: 1, Attempt: 1, Section: sec})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "`blank`") {
		t.Errorf("a blank tail is shown:\n%s", out)
	}
	for i := range verifyLogsMax + 1 {
		shown := strings.Contains(out, fmt.Sprintf("tail %d", i))
		if want := i < verifyLogsMax-1; shown != want {
			t.Errorf("tail %d shown %v, want %v:\n%s", i, shown, want, out)
		}
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

// TestVerifyLineStates: the states verification can rest in besides
// running and judged each read as maul's own sentence, and none claims a
// check ran.
func TestVerifyLineStates(t *testing.T) {
	for _, tc := range []struct {
		p    VerifyProgress
		want string
	}{
		{VerifyProgress{State: VerifyWaitingForCI}, "Verification waits for the pull requests' checks to pass."},
		{VerifyProgress{State: VerifyWaiting}, "Verification waits for the preview to be ready."},
		{VerifyProgress{State: VerifyNeedsReplan, AutoFix: FixHalted, AutoFixReason: "CriterionError"},
			"Verification cannot run: an approved check's target is not previewed. " + startOverText},
		{VerifyProgress{State: VerifyNotDeclared}, "No acceptance criteria were declared."},
		{VerifyProgress{State: VerifyNotChecked, ManualOpen: 2}, "Nothing is checked automatically, 2 for a human."},
		{VerifyProgress{State: VerifyUnreadable}, "Verification stopped: maul cannot read the approved plan's " +
			"acceptance criteria, so nothing is verified. " + startOverText},
		{VerifyProgress{State: VerifyCriteriaReplan}, "Nothing is verified: the approved plan states acceptance " +
			"criteria, which maul's evaluator does not run. " + startOverText},
		{VerifyProgress{State: VerifyPreviewNotLive, Verification: 1, FixesUsed: 0, MaxFixes: 3, ManualOpen: 1},
			"Verification unavailable: the preview did not become live at the pull requests' heads in time, so the " +
				"checks could not run. Verification resumes once the preview is live; `kubectl get preview` says " +
				"why it is not."},
		{VerifyProgress{State: VerifyHeadsMoved, Outcomes: true}, "The pull requests moved to new commits: maul " +
			"is checking the new commits, and the last result, about earlier commits, is not shown."},
		{VerifyProgress{State: VerifyExhausted, Verification: 3, FixesUsed: 2, MaxFixes: 2, Passing: 1, Total: 3,
			Outcomes: true}, "Evaluation 3 failing: 1/3 outcomes confirmed, no automatic fix left."},
		{VerifyProgress{State: VerifyUnavailable, Verification: 2, MaxFixes: 2, Outcomes: true},
			"Verification unavailable: maul could not run the checks."},
		{VerifyProgress{State: VerifyFailing, Verification: 1, MaxFixes: 2, Passing: 1, Total: 3, Outcomes: true,
			DefectConfirmed: true, AutoFix: FixQueued},
			"Evaluation 1: a person confirmed the defect maul found: 1/3 outcomes confirmed; a fix is queued."},
	} {
		if got := verifyLine(tc.p); got != tc.want {
			t.Errorf("%+v:\n got %q\nwant %q", tc.p, got, tc.want)
		}
	}
}

// startOverText is what an intent in review is told instead of a replan
// (S1-29): a replan is not available once its pull requests are open.
const startOverText = "The approved plan cannot change while its pull requests are open: to start over with a " +
	"new plan, an approver comments `/maul cancel` on the intent's issue (the pull requests are left to you) and " +
	"opens a new issue."

// TestNoReplanOfferedInReview (S1-29): nothing maul tells an intent in
// review offers a replan, which is not available there.
func TestNoReplanOfferedInReview(t *testing.T) {
	said := make([]string, 0, 5)
	for _, state := range []string{VerifyNeedsReplan, VerifyUnreadable, VerifyCriteriaReplan} {
		line, _ := verifyRestingLine(state)
		said = append(said, line)
	}
	notice, err := RenderCriteriaReplanNotice(CriteriaReplanNotice{Namespace: "maul", Intent: "x-1",
		Key: CriteriaReplanKey(2), PlanRevision: 2, TriggerLabel: "maul:intent"})
	if err != nil {
		t.Fatal(err)
	}
	said = append(said, notice)
	text, _, err := verifyNoticeText(VerifyCommandNotice{Verb: "verify", Actor: "octocat",
		Outcome: VerifyCapReached, MaxVerifications: 5}, command.IntentIssue)
	if err != nil {
		t.Fatal(err)
	}
	said = append(said, text)
	for _, s := range said {
		if strings.Contains(s, "/maul replan") || strings.Contains(s, "replan needed") ||
			strings.Contains(s, "re-apply") {
			t.Errorf("offers a replan in review: %q", s)
		}
	}
	for _, want := range []string{"`/maul cancel`", "new issue with the `maul:intent` label"} {
		if !strings.Contains(strings.Join(strings.Fields(notice), " "), want) {
			t.Errorf("the criteria notice does not say %q:\n%s", want, notice)
		}
	}
}

// TestStartOverNamesTheIssue: the start-over sentence is also shown in a
// pull request's preview comment, where `/maul cancel` is not a command
// (command.IntentPR), so it says where the approver comments it.
func TestStartOverNamesTheIssue(t *testing.T) {
	if slices.Contains(command.Available(command.IntentPR), "cancel") {
		t.Skip("cancel is a pull request command now")
	}
	c := testPreviewComment(PreviewLive, "", "")
	c.Verify = &VerifySection{Progress: VerifyProgress{State: VerifyCriteriaReplan}}
	got, err := RenderIntentPreviewComment(c)
	if err != nil {
		t.Fatal(err)
	}
	if want := "`/maul cancel` on the intent's issue"; !strings.Contains(strings.Join(strings.Fields(got), " "), want) {
		t.Errorf("the pull request's comment does not say where to cancel (%q):\n%s", want, got)
	}
}
