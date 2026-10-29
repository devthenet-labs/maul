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

// reviewHandoff is a checkreview handoff of n items, as the controller
// stages it in input/issue.md.
func reviewHandoff(n int) string {
	h := checkReviewHandoff{}
	for i := range n {
		h.Reviews = append(h.Reviews, checkReviewItem{
			CheckDigest:   reviewDigest("c", i),
			OutcomeDigest: reviewDigest("o", i),
			Outcome:       fmt.Sprintf("Outcome %d: a rated battle moves the rating.", i),
			Check:         fmt.Sprintf(`{"steps":[{"goto":"/battles/%d"}]}`, i),
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
		{CheckDigest: reviewDigest("c", 0), OutcomeDigest: reviewDigest("o", 0), Answer: "proves",
			Reason: "It rates a battle and reads the score."},
		{CheckDigest: reviewDigest("c", 1), OutcomeDigest: reviewDigest("o", 1), Answer: "unclear",
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
		prompt := argvFlag(t, spec.Argv, "-p")
		if !strings.Contains(prompt, fmt.Sprintf("Outcome %d:", i)) ||
			!strings.Contains(prompt, fmt.Sprintf("/battles/%d", i)) {
			t.Errorf("session %d prompt lacks its own outcome and check:\n%s", i, prompt)
		}
		if other := fmt.Sprintf("Outcome %d:", 1-i); strings.Contains(prompt, other) {
			t.Errorf("session %d prompt carries another item's outcome", i)
		}
		if strings.Contains(prompt, reviewDigest("c", i)) {
			t.Errorf("session %d prompt carries the check's digest; the session never names it", i)
		}
		if !strings.Contains(prompt, filepath.Join(ws, "scratch", fmt.Sprintf("review-%d.md", i+1))) {
			t.Errorf("session %d prompt does not name its report path", i)
		}
	}
}

// TestCheckReviewSessionRefused: a session whose report is refused ends the
// stage with that session's outcome, no answers, and no later session run.
func TestCheckReviewSessionRefused(t *testing.T) {
	var out bytes.Buffer
	cfg, ws := roleConfig(t, PhaseCheckReview, reviewHandoff(2), &out)
	bad := step{ws: ws, stdout: streamSuccess, writes: map[string]string{"scratch/review-1.md": reviewReport("maybe")}}
	fx := &fakeExec{steps: []step{bad, bad, bad}}
	ev := onlyEvent(t, cfg, fx, &out)
	c := ev.CheckReview
	if c == nil || c.Outcome != envelope.OutcomeReportInvalid || len(c.Answers) != 0 {
		t.Fatalf("checkreview = %+v, want report_invalid with no answers", c)
	}
	if !strings.Contains(c.Detail, "review 1 of 2") {
		t.Errorf("detail = %q, want it to name the session", c.Detail)
	}
	if len(fx.specs) != 1+repairRounds {
		t.Errorf("commands run = %d, want the first session and its repairs only", len(fx.specs))
	}
}

// TestCheckReviewHandoffRefused: a handoff that is not the controller's
// encoding is a fatal before any session runs.
func TestCheckReviewHandoffRefused(t *testing.T) {
	item := func(edit func(*checkReviewItem)) string {
		var h checkReviewHandoff
		if err := json.Unmarshal([]byte(reviewHandoff(1)), &h); err != nil {
			t.Fatal(err)
		}
		edit(&h.Reviews[0])
		raw, _ := json.Marshal(h)
		return string(raw)
	}
	var h checkReviewHandoff
	if err := json.Unmarshal([]byte(reviewHandoff(1)), &h); err != nil {
		t.Fatal(err)
	}
	h.Reviews = append(h.Reviews, h.Reviews[0])
	dupRaw, _ := json.Marshal(h)

	tests := []struct{ name, handoff, want string }{
		{"empty", "", "no reviews"},
		{"not json", "outcome: x", "not valid"},
		{"no items", `{"reviews":[]}`, "no reviews"},
		{"unknown field", `{"reviews":[],"extra":1}`, "not valid"},
		{"too many", reviewHandoff(41), "over 40"},
		{"bad check digest", item(func(i *checkReviewItem) { i.CheckDigest = "sha256:xyz" }), "check_digest"},
		{"bad outcome digest", item(func(i *checkReviewItem) { i.OutcomeDigest = "" }), "outcome_digest"},
		{"no outcome text", item(func(i *checkReviewItem) { i.Outcome = "" }), "outcome is empty"},
		{"no check", item(func(i *checkReviewItem) { i.Check = "" }), "check is empty"},
		{"check over cap", item(func(i *checkReviewItem) { i.Check = strings.Repeat("x", 8<<10+1) }), "check is"},
		{"repeated pair", string(dupRaw), "repeats"},
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
