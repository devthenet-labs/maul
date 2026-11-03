// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import (
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"
)

func TestNotCovered(t *testing.T) {
	from := func(fs ...string) []PlanOutcome {
		os := make([]PlanOutcome, len(fs))
		for i, f := range fs {
			os[i] = PlanOutcome{ID: fmt.Sprintf("o%d", i), Outcome: "x", From: f}
		}
		return os
	}
	const issue = `Rate battles with Elo.

- ratings should update after every battle
- use Elo, not +1/-1
* the leaderboard still lists everyone
`
	const issue57 = "Tell people how long a rendered WAV is without them having to download it.\n\n" +
		"- `POST /render/wav` answers with an `X-Duration-Ms` header: the length of the returned audio in " +
		"whole milliseconds.\n" +
		"- `POST /render/analyze` and `POST /render/wav` agree: for the same pattern, `X-Duration-Ms` equals " +
		"the analysis's `durationMs`.\n"
	tests := []struct {
		name     string
		body     string
		outcomes []PlanOutcome
		want     []string
	}{
		{"no outcomes", issue, nil, []string{
			"ratings should update after every battle", "use Elo, not +1/-1", "the leaderboard still lists everyone",
		}},
		{"all quoted", issue, from("ratings should update after every battle", "use Elo, not +1/-1",
			"the leaderboard still lists everyone"), nil},
		{"partially quoted", issue, from("update after every", "use elo"), []string{"the leaderboard still lists everyone"}},
		{"case and whitespace", issue, from("  RATINGS   should\tupdate after EVERY battle ", "Use  Elo,  not +1/-1",
			"The Leaderboard still lists everyone"), nil},
		{"quoted with its marker", issue, from("- use Elo, not +1/-1"), []string{
			"ratings should update after every battle", "the leaderboard still lists everyone",
		}},
		{"a from longer than the line", issue, from("ratings should update after every battle, always"), []string{
			"ratings should update after every battle", "use Elo, not +1/-1", "the leaderboard still lists everyone",
		}},
		{"empty from covers nothing", issue, from("", "   ", "-"), []string{
			"ratings should update after every battle", "use Elo, not +1/-1", "the leaderboard still lists everyone",
		}},
		{"no bullets", "Just prose.\nMore prose - with a dash.\n", nil, nil},
		{"ordered and task lists", "1. first thing\n2) second thing\n- [ ] a task\n- [x] done task\n", from("second"),
			[]string{"first thing", "a task", "done task"}},
		{"plus marker and nested", "+ top\n  - nested one\n    - deeper\n", nil, []string{"top", "nested one", "deeper"}},
		{"fenced code block", "- real\n```\n- in code\n```\n~~~yaml\n- also code\n~~~\n- after\n", nil,
			[]string{"real", "after"}},
		{"fence closed only by its own kind", "```\n- code\n~~~\n- still code\n```\n- out\n", nil, []string{"out"}},
		{"longer fence", "````\n```\n- code\n````\n- out\n", nil, []string{"out"}},
		{"unclosed fence", "- before\n```\n- code to the end\n", nil, []string{"before"}},
		{"indented code block", "Prose.\n\n    - in code\n\n- item\n", nil, []string{"item"}},
		{"indented continuation of a list", "- item\n\n    - nested after a blank\n", nil,
			[]string{"item", "nested after a blank"}},
		{"html comment", "<!-- template:\n- fill this in\n-->\n- kept\n<!-- - inline --> \n", nil, []string{"kept"}},
		{"crlf", "- one\r\n- two\r\n", from("one"), []string{"two"}},
		{"marker without text", "-\n- \n-   \n- x\n", nil, []string{"x"}},
		{"not a marker", "-no space\n--- \n***\n- ok\n", nil, []string{"ok"}},
		{"thematic break", "- - -\n* * *\n", nil, nil},
		{"blockquote is not a list", "> - quoted\n- plain\n", nil, []string{"plain"}},
		// #57: the planner quoted the lines without their inline code.
		{"inline code dropped by the from", issue57, from(
			"POST /render/wav answers with an X-Duration-Ms header: the length of the returned audio in whole "+
				"milliseconds.",
			"POST /render/analyze and POST /render/wav agree: for the same pattern, X-Duration-Ms equals the "+
				"analysis's durationMs."), nil},
		{"inline code kept by the from", issue57, from("`POST /render/wav` answers with an `X-Duration-Ms` header",
			"`POST /render/analyze` and `POST /render/wav` agree"), nil},
		{"only one of two lines quoted", issue57, from("POST /render/wav answers with an X-Duration-Ms header"),
			[]string{"`POST /render/analyze` and `POST /render/wav` agree: for the same pattern, `X-Duration-Ms` " +
				"equals the analysis's `durationMs`."}},
		{"emphasis and strikethrough", "- ratings **must** update _after_ every ~~round~~ battle\n",
			from("ratings must update after every round battle"), nil},
		{"link text", "- see [the leaderboard](https://example.com/lb) and ![a badge](b.png)\n",
			from("see the leaderboard and a badge"), nil},
		{"link url is not the text", "- see [the leaderboard](https://example.com/lb)\n",
			from("example.com"), []string{"see [the leaderboard](https://example.com/lb)"}},
		{"trailing punctuation", "- ratings update after every battle.\n- use Elo\n",
			from("ratings update after every battle", "use Elo."), nil},
		{"markup does not cover a different line", "- `POST /render/wav` answers with a header\n",
			from("POST /render/mp3 answers with a header"), []string{"`POST /render/wav` answers with a header"}},
		{"a from of markup only covers nothing", "- `x`\n", from("``", "**", "[](u)"), []string{"`x`"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := NotCovered(tc.body, tc.outcomes)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("NotCovered() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestNotCoveredProperty: whatever the issue, a from that quotes a bullet
// exactly (in any case, with any whitespace, with or without its inline
// markup) covers it, and every line NotCovered reports is a bullet of the
// issue.
func TestNotCoveredProperty(t *testing.T) {
	r := rand.New(rand.NewSource(seed))
	markers := []string{"- ", "* ", "+ ", "1. ", "12) ", "- [ ] ", "  - "}
	for n := range 500 {
		var b strings.Builder
		var bullets, plains []string
		for range r.Intn(12) {
			if r.Intn(4) == 0 {
				b.WriteString(sentence(r) + "\n")
				continue
			}
			plain := sentence(r)
			line := decorate(r, plain)
			bullets = append(bullets, line)
			plains = append(plains, plain)
			b.WriteString(markers[r.Intn(len(markers))] + line + "\n")
		}
		var outcomes []PlanOutcome
		quoted := map[string]bool{}
		for i, line := range bullets {
			if r.Intn(2) == 0 {
				continue
			}
			quoted[line] = true
			if r.Intn(2) == 0 {
				line = plains[i]
			}
			outcomes = append(outcomes, PlanOutcome{ID: fmt.Sprintf("o%d", i), Outcome: "x", From: mangle(r, line)})
		}
		got := NotCovered(b.String(), outcomes)
		for _, line := range got {
			if quoted[line] {
				t.Fatalf("case %d (seed %d): %q is quoted exactly and still not covered\n%s", n, seed, line, b.String())
			}
			if !slices.Contains(bullets, line) {
				t.Fatalf("case %d (seed %d): %q is no bullet of the issue\n%s", n, seed, line, b.String())
			}
		}
		for _, line := range bullets {
			if !quoted[line] && !coveredBySome(line, outcomes) && !slices.Contains(got, line) {
				t.Fatalf("case %d (seed %d): %q is quoted by no outcome and not reported", n, seed, line)
			}
		}
	}
}

func sentence(r *rand.Rand) string {
	words := make([]string, 1+r.Intn(6))
	for i := range words {
		words[i] = word(r)
	}
	return strings.Join(words, " ")
}

// mangle changes a line's case and whitespace only.
func mangle(r *rand.Rand, s string) string {
	var b strings.Builder
	b.WriteString(strings.Repeat(" ", r.Intn(3)))
	for _, f := range strings.Fields(s) {
		if r.Intn(2) == 0 {
			f = strings.ToUpper(f)
		}
		b.WriteString(f + strings.Repeat(" \t", 1+r.Intn(2)))
	}
	return b.String()
}

// decorate wraps some of a sentence's words in inline markup: code spans,
// emphasis, strikethrough or a link.
func decorate(r *rand.Rand, s string) string {
	fields := strings.Fields(s)
	for i, f := range fields {
		switch r.Intn(6) {
		case 0:
			fields[i] = "`" + f + "`"
		case 1:
			fields[i] = "**" + f + "**"
		case 2:
			fields[i] = "_" + f + "_"
		case 3:
			fields[i] = "~~" + f + "~~"
		case 4:
			fields[i] = "[" + f + "](https://example.com/" + word(r) + ")"
		}
	}
	return strings.Join(fields, " ")
}

// coveredBySome is the rule restated, for the property's other direction.
func coveredBySome(line string, os []PlanOutcome) bool {
	plain := func(s string) string {
		for {
			open := strings.Index(s, "](")
			end := strings.Index(s, ")")
			if open < 0 || end < open {
				break
			}
			s = s[:open] + s[end+1:]
		}
		s = strings.NewReplacer("[", "", "`", "", "*", "", "_", "", "~", "").Replace(s)
		return strings.TrimRight(strings.ToLower(strings.Join(strings.Fields(s), " ")), ".,;:!? ")
	}
	l := plain(line)
	for _, o := range os {
		f := plain(o.From)
		if f != "" && strings.Contains(l, f) {
			return true
		}
	}
	return false
}
