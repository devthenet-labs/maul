// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/checks"
	"github.com/devthenet-labs/maul/internal/envelope"
)

// reviewDigest is a well-formed digest for test item i and kind k ("c" or
// "o").
func reviewDigest(k string, i int) string {
	hex := fmt.Sprintf("%064x", i)
	if k == "o" {
		hex = "f" + hex[1:]
	}
	return "sha256:" + hex
}

// reviewCheck is test item i's check: an http check, as the controller
// hands it over (checks.Canonical).
func reviewCheck(i int) string {
	return string(checks.Canonical(mustCheck(fmt.Sprintf(`{"id":"rate-battle.n%d","outcome":"rate-battle",`+
		`"form":"http","http":{"target":"api","steps":[{"name":"rate-it","request":{"method":"POST",`+
		`"path":"/battles/%d","json":{"winner":"{{run}}-a"}},"expect":{"status":201,"bodyContains":"{{run}}-a"}}]}}`,
		i, i))))
}

// reviewHandoff is a checkreview handoff of n items, as the controller
// stages it in input/issue.md.
func reviewHandoff(n int) string {
	h := CheckReviewHandoff{}
	for i := range n {
		check := reviewCheck(i)
		h.Reviews = append(h.Reviews, CheckReviewItem{
			CheckDigest:   checks.Digest(mustCheck(check)),
			OutcomeDigest: reviewDigest("o", i),
			Outcome:       fmt.Sprintf("Outcome %d: a rated battle moves the rating.", i),
			Check:         check,
		})
	}
	raw, err := json.Marshal(h)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func reviewReport(answer string) string {
	return "---\nanswer: " + answer + "\nreason: \"It rates a battle and reads the score.\"\n---\n"
}

// TestCheckReviewEvent: each item is answered in its own fresh session, in
// order, and each answer is keyed by the digests the handoff carried, never
// by anything the session wrote. The stage's spend is every session's.
func TestCheckReviewEvent(t *testing.T) {
	var out bytes.Buffer
	cfg, ws := roleConfig(t, PhaseCheckReview, reviewHandoff(2), &out)
	brokeredClaude(t, &cfg)
	fx := &fakeExec{steps: []step{
		{ws: ws, stdout: streamSuccess, writes: map[string]string{"scratch/review-1.md": reviewReport("proves")}},
		{ws: ws, stdout: streamSuccess, writes: map[string]string{"scratch/review-2.md": reviewReport("unclear")}},
	}}
	sessions := 0
	a := New(cfg, fx)
	a.newSessionID = func() string {
		sessions++
		return fmt.Sprintf("00000000-0000-4000-8000-%012d", sessions)
	}
	if err := a.Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	evs := events(t, out.String())
	if len(evs) != 1 || evs[0].Type != envelope.TypeCheckReview || evs[0].CheckReview == nil {
		t.Fatalf("events = %+v, want one checkreview event", evs)
	}
	c := evs[0].CheckReview
	if c.Outcome != envelope.OutcomeOK {
		t.Fatalf("checkreview = %+v, want ok", c)
	}
	want := []envelope.ReviewAnswer{
		{CheckDigest: checks.Digest(mustCheck(reviewCheck(0))), OutcomeDigest: reviewDigest("o", 0), Answer: "proves",
			Reason: "It rates a battle and reads the score."},
		{CheckDigest: checks.Digest(mustCheck(reviewCheck(1))), OutcomeDigest: reviewDigest("o", 1), Answer: "unclear",
			Reason: "It rates a battle and reads the score."},
	}
	if fmt.Sprint(c.Answers) != fmt.Sprint(want) {
		t.Errorf("answers = %+v\nwant %+v", c.Answers, want)
	}
	if c.Usage.OutputTokens != 60 || c.NumTurns != 14 {
		t.Errorf("stage = %+v, want both sessions' spend", c.Stage)
	}
	if len(fx.specs) != 2 {
		t.Fatalf("commands run = %d, want one per item", len(fx.specs))
	}
	for i, spec := range fx.specs {
		if spec.Dir != filepath.Join(ws, "scratch") {
			t.Errorf("session %d ran in %s, want the scratch directory", i, spec.Dir)
		}
		wantID := fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1)
		if got := argvFlag(t, spec.Argv, "--session-id"); got != wantID {
			t.Errorf("session %d id = %s, want a fresh one, %s", i, got, wantID)
		}
		checkReviewPrompt(t, i, argvFlag(t, spec.Argv, "-p"), ws)
	}
}

// checkReviewPrompt: review i's session is the check review template over
// its own outcome and check, as maul renders the check, naming its own
// report and neither digest.
func checkReviewPrompt(t *testing.T, i int, prompt, ws string) {
	t.Helper()
	if !strings.Contains(prompt, fmt.Sprintf("Outcome %d:", i)) ||
		!strings.Contains(prompt, fmt.Sprintf("/battles/%d", i)) {
		t.Errorf("session %d prompt lacks its own outcome and check:\n%s", i, prompt)
	}
	if other := fmt.Sprintf("Outcome %d:", 1-i); strings.Contains(prompt, other) {
		t.Errorf("session %d prompt carries another item's outcome", i)
	}
	digest := checks.Digest(mustCheck(reviewCheck(i)))
	if strings.Contains(prompt, digest) || strings.Contains(prompt, strings.TrimPrefix(digest, "sha256:")) {
		t.Errorf("session %d prompt carries the check's digest; the session never names it", i)
	}
	// The template's, as maul renders the check: no id, no step name.
	for _, banned := range []string{fmt.Sprintf("rate-battle.n%d", i), "rate-it"} {
		if strings.Contains(prompt, banned) {
			t.Errorf("session %d prompt shows %q; a reviewer never sees a check's names", i, banned)
		}
	}
	if !strings.Contains(prompt, "You are maul's check reviewer.") {
		t.Errorf("session %d prompt is not the check review template:\n%s", i, prompt)
	}
	if !strings.Contains(prompt, filepath.Join(ws, "scratch", fmt.Sprintf("review-%d.md", i+1))) {
		t.Errorf("session %d prompt does not name its report path", i)
	}
}

// TestCheckReviewSessionRefused (S1 bug 8): a session whose report stays
// refused after its repairs makes that one check unclear, unreviewed (an
// answer maul wrote, never the reviewer's), and the stage goes on to the
// next check: one bad review never drops the others.
func TestCheckReviewSessionRefused(t *testing.T) {
	var out bytes.Buffer
	cfg, ws := roleConfig(t, PhaseCheckReview, reviewHandoff(3), &out)
	good := func(n int) step {
		return step{ws: ws, stdout: streamSuccess, writes: map[string]string{
			fmt.Sprintf("scratch/review-%d.md", n): reviewReport("proves")}}
	}
	bad := step{ws: ws, stdout: streamSuccess, writes: map[string]string{
		"scratch/review-2.md": "---\nanswer: proves\nreason: \"It rates a battle.\"\n"}}
	fx := &fakeExec{steps: []step{good(1), bad, bad, bad, good(3)}}
	ev := onlyEvent(t, cfg, fx, &out)
	c := ev.CheckReview
	if c == nil || c.Outcome != envelope.OutcomeOK || len(c.Answers) != 3 {
		t.Fatalf("checkreview = %+v, want ok with three answers", c)
	}
	for i, want := range []string{"proves", "unclear", "proves"} {
		a := c.Answers[i]
		if a.Answer != want || a.Unreviewed != (i == 1) || a.CheckDigest != checks.Digest(mustCheck(reviewCheck(i))) {
			t.Errorf("answer %d = %+v, want %s (unreviewed %v)", i, a, want, i == 1)
		}
	}
	if r := c.Answers[1].Reason; !strings.Contains(r, "maul") || strings.Contains(r, "It rates") {
		t.Errorf("the unreviewed answer's reason %q is not maul's own", r)
	}
	if len(fx.specs) != 3+repairRounds {
		t.Errorf("commands run = %d, want each session and the refused one's repairs", len(fx.specs))
	}
}

// TestCheckReviewRepairsWithinItsTurns (S1 bug 8): with the stage's default
// of 6 turns per check, a review's first run gets 3 and keeps the rest for
// one repair, so an unterminated frontmatter is repaired in the session.
func TestCheckReviewRepairsWithinItsTurns(t *testing.T) {
	var out bytes.Buffer
	cfg, ws := roleConfig(t, PhaseCheckReview, reviewHandoff(1), &out)
	brokeredClaude(t, &cfg)
	cfg.GrantedMaxTurns = 6
	fx := &fakeExec{steps: []step{
		{ws: ws, stdout: streamSuccess, writes: map[string]string{
			"scratch/review-1.md": "---\nanswer: proves\nreason: \"It rates a battle.\"\n"}},
		{ws: ws, stdout: streamSuccess, writes: map[string]string{"scratch/review-1.md": reviewReport("proves")}},
	}}
	c := onlyEvent(t, cfg, fx, &out).CheckReview
	if c == nil || c.Outcome != envelope.OutcomeOK || len(c.Answers) != 1 || c.Answers[0].Answer != "proves" ||
		c.Answers[0].Unreviewed {
		t.Fatalf("checkreview = %+v, want the repaired proves", c)
	}
	if len(fx.specs) != 2 {
		t.Fatalf("commands run = %d, want the session and one repair", len(fx.specs))
	}
	if got := argvFlag(t, fx.specs[0].Argv, "--max-turns"); got != "3" {
		t.Errorf("first run --max-turns %s, want 3 (the rest kept for its repair)", got)
	}
}

// TestCheckReviewRedQuestion: a check proposed to show a defect is asked
// the red question, naming the step maul saw fail.
func TestCheckReviewRedQuestion(t *testing.T) {
	var h CheckReviewHandoff
	if err := json.Unmarshal([]byte(reviewHandoff(1)), &h); err != nil {
		t.Fatal(err)
	}
	h.Reviews[0].Expect, h.Reviews[0].FailedStep, h.Reviews[0].FailedAssertion = "fail", 1, "bodyContains"
	raw, _ := json.Marshal(h)
	var out bytes.Buffer
	cfg, ws := roleConfig(t, PhaseCheckReview, string(raw), &out)
	brokeredClaude(t, &cfg)
	fx := &fakeExec{steps: []step{{ws: ws, stdout: streamSuccess, writes: map[string]string{
		"scratch/review-1.md": reviewReport("proves")}}}}
	c := onlyEvent(t, cfg, fx, &out).CheckReview
	if c == nil || c.Outcome != envelope.OutcomeOK || len(c.Answers) != 1 {
		t.Fatalf("checkreview = %+v", c)
	}
	prompt := argvFlag(t, fx.specs[0].Argv, "-p")
	for _, want := range []string{"This check failed", "step 1", "would pass on a build where the outcome holds"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the red prompt lacks %q:\n%s", want, prompt)
		}
	}
}

// TestCheckReviewHandoffRefused: a handoff that is not the controller's
// encoding is a fatal before any session runs.
func TestCheckReviewHandoffRefused(t *testing.T) {
	item := func(edit func(*CheckReviewItem)) string {
		var h CheckReviewHandoff
		if err := json.Unmarshal([]byte(reviewHandoff(1)), &h); err != nil {
			t.Fatal(err)
		}
		edit(&h.Reviews[0])
		raw, _ := json.Marshal(h)
		return string(raw)
	}
	var h CheckReviewHandoff
	if err := json.Unmarshal([]byte(reviewHandoff(1)), &h); err != nil {
		t.Fatal(err)
	}
	h.Reviews = append(h.Reviews, h.Reviews[0])
	dupRaw, _ := json.Marshal(h)

	tests := []struct{ name, handoff, want string }{
		{"empty", "", "empty"},
		{"not json", "outcome: x", "not valid"},
		{"no items", `{"reviews":[]}`, "no reviews"},
		{"unknown field", `{"reviews":[],"extra":1}`, "not valid"},
		{"too many", reviewHandoff(41), "over 40"},
		{"bad check digest", item(func(i *CheckReviewItem) { i.CheckDigest = "sha256:xyz" }), "check_digest"},
		{"bad outcome digest", item(func(i *CheckReviewItem) { i.OutcomeDigest = "" }), "outcome_digest"},
		{"no outcome text", item(func(i *CheckReviewItem) { i.Outcome = "" }), "outcome is empty"},
		{"no check", item(func(i *CheckReviewItem) { i.Check = "" }), "check is empty"},
		{"check over cap", item(func(i *CheckReviewItem) { i.Check = strings.Repeat("x", 8<<10+1) }), "check is"},
		{"repeated pair", string(dupRaw), "repeats"},
		{"check not json", item(func(i *CheckReviewItem) { i.Check = "goto /battles" }), "check: check is not valid JSON"},
		{"check of another digest", item(func(i *CheckReviewItem) { i.CheckDigest = reviewDigest("c", 9) }),
			"not its check_digest"},
		{"check off the grammar", item(func(i *CheckReviewItem) { i.Check = `{"steps":[]}` }), "check: "},
		{"another expectation", item(func(i *CheckReviewItem) { i.Expect, i.FailedStep = "maybe", 1 }), "expect"},
		{"red with no failed step", item(func(i *CheckReviewItem) { i.Expect, i.FailedAssertion = "fail", "x" }),
			"failed_step"},
		{"red with no failed assertion", item(func(i *CheckReviewItem) { i.Expect, i.FailedStep = "fail", 1 }),
			"failed_assertion"},
		{"a failed step past the check", item(func(i *CheckReviewItem) {
			i.Expect, i.FailedStep, i.FailedAssertion = "fail", 2, "x"
		}),
			"failed_step"},
		{"a failed step of a green check", item(func(i *CheckReviewItem) { i.FailedStep = 1 }), "failed_step"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			cfg, _ := roleConfig(t, PhaseCheckReview, tt.handoff, &out)
			fx := &fakeExec{}
			err := New(cfg, fx).Run(context.Background())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Run() error = %v, want %q", err, tt.want)
			}
			if len(fx.specs) != 0 {
				t.Errorf("commands run = %d, want none", len(fx.specs))
			}
			if evs := events(t, out.String()); len(evs) != 1 || evs[0].Type != envelope.TypeFatal {
				t.Errorf("events = %+v, want one fatal", evs)
			}
		})
	}
}
