// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package report

import (
	"strings"
	"testing"
)

func TestParseCheckReview(t *testing.T) {
	tests := []struct {
		name, doc  string
		wantAnswer ReviewAnswer
		wantReason string
	}{
		{"proves", "---\nanswer: proves\nreason: \"  It rates a battle the run created and reads its score.  \"\n---\n",
			AnswerProves, "It rates a battle the run created and reads its score."},
		{"does not prove", "---\nanswer: does_not_prove\nreason: \"It only loads /.\"\n---\nThe body.\n",
			AnswerDoesNotProve, "It only loads /."},
		{"unclear", "---\nanswer: unclear\nreason: \"The outcome names no rating rule.\"\n---\n",
			AnswerUnclear, "The outcome names no rating rule."},
		// A reason written as plain prose with a colon is repaired, as the
		// plan's free text is.
		{"unquoted reason", "---\nanswer: proves\nreason: Asserts the score: 1016.\n---\n",
			AnswerProves, "Asserts the score: 1016."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := ParseCheckReview([]byte(tt.doc))
			if err != nil {
				t.Fatalf("ParseCheckReview() error = %v", err)
			}
			if c.Answer != tt.wantAnswer || c.Reason != tt.wantReason {
				t.Errorf("ParseCheckReview() = %+v, want %s %q", c, tt.wantAnswer, tt.wantReason)
			}
		})
	}
}

func TestParseCheckReviewRefusals(t *testing.T) {
	tests := []struct {
		name, doc, want string
	}{
		{"no frontmatter", "proves\n", "missing frontmatter"},
		{"no answer", "---\nreason: r\n---\n", `answer "" is not one of`},
		{"bad answer", "---\nanswer: yes\nreason: r\n---\n", `answer "yes" is not one of`},
		{"no reason", "---\nanswer: proves\n---\n", "reason is required"},
		{"long reason", "---\nanswer: proves\nreason: \"" + strings.Repeat("r", NoteMaxChars+1) + "\"\n---\n", "over 600"},
		{"unknown key", "---\nanswer: proves\nreason: r\ncheck_digest: x\n---\n", "check_digest"},
		{"hidden reason", "---\nanswer: proves\nreason: \"ok\\u200b\"\n---\n", "U+200B"},
		{"hidden body", "---\nanswer: proves\nreason: r\n---\nok" + string(rune(0x202e)) + "\n", "U+202E"},
		{"tagged", "---\nanswer: !!str proves\nreason: r\n---\n", "tagged"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseCheckReview([]byte(tt.doc))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("ParseCheckReview() error = %v, want %q", err, tt.want)
			}
		})
	}
}

// TestCheckReviewValidate: the answer rules hold on their own, for an
// answer that reaches a controller on the envelope rather than in a report.
func TestCheckReviewValidate(t *testing.T) {
	ok := CheckReview{Answer: AnswerDoesNotProve, Reason: "  vacuous  "}
	if err := ok.Validate(); err != nil || ok.Reason != "vacuous" {
		t.Errorf("Validate() = %v, reason %q; want valid and trimmed", err, ok.Reason)
	}
	for _, c := range []CheckReview{
		{Answer: "proves ", Reason: "r"},
		{Answer: AnswerProves},
		{Answer: AnswerProves, Reason: "two\nlines"},
	} {
		if err := c.Validate(); err == nil {
			t.Errorf("Validate(%+v) = nil, want an error", c)
		}
	}
}
