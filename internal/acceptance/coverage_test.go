// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import (
	"fmt"
	"math/rand"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode"
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
	const (
		issue59First = "`GET /api/patterns?minBpm=<n>&maxBpm=<n>` returns only the patterns whose `bpm` is " +
			"between the two values, inclusive, newest first, with the same `limit` rules as today. Either bound " +
			"may be given alone."
		issue59Second = "A bound that is not a whole number from 60 to 200, or a `minBpm` above `maxBpm`, gets 400."
		issue59       = "Let people find beats in a tempo range.\n\n- " + issue59First + "\n- " + issue59Second + "\n"
	)
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
		{"link url with parentheses", "- see [docs](https://e/a_(b)) now\n", from("see docs now"), nil},
		{"reference link", "- see [docs][1] and [more][] now\n", from("see docs and more now"), nil},
		{"link url is not the text", "- see [the leaderboard](https://example.com/lb)\n",
			from("example.com"), []string{"see [the leaderboard](https://example.com/lb)"}},
		{"trailing punctuation", "- ratings update after every battle.\n- use Elo\n",
			from("ratings update after every battle", "use Elo."), nil},
		{"markup does not cover a different line", "- `POST /render/wav` answers with a header\n",
			from("POST /render/mp3 answers with a header"), []string{"`POST /render/wav` answers with a header"}},
		{"a from of markup only covers nothing", "- `x`\n", from("``", "**", "[](u)"), []string{"`x`"}},
		// #59: the planner shortened its quote of the 400 line with "...".
		{"abbreviated with three dots", issue59, from("A bound that is not a whole number from 60 to 200 ... gets 400."),
			[]string{issue59First}},
		{"abbreviated with an ellipsis character", issue59, from("A bound that is not a whole number… gets 400"),
			[]string{issue59First}},
		{"abbreviated at both ends", issue59,
			from("…returns only the patterns whose `bpm` is between...", "... above maxBpm, gets 400"), nil},
		{"abbreviated with its marker", issue59, from("- A bound ... not a whole number ... gets 400"),
			[]string{issue59First}},
		{"pieces out of order", issue59, from("gets 400 ... A bound that is not a whole number"),
			[]string{issue59First, issue59Second}},
		{"pieces overlapping", "- whole number from 60\n", from("whole number ... number from 60"),
			[]string{"whole number from 60"}},
		{"pieces too short", issue59, from("a ... gets 400", "a bound ... 400"), []string{issue59First, issue59Second}},
		{"one piece too short", issue59, from("A bound that is not a whole number ... 400"),
			[]string{issue59First, issue59Second}},
		{"a line with an ellipsis quoted exactly", "- wait... retry\n- wait… retry later\n- go on...\n",
			from("wait... retry", "Wait…  retry later.", "go on..."), nil},
		{"a trailing ellipsis character is trailing punctuation", "- it gets 400\n- it gets 500\n",
			from("gets 400…", "gets 500..."), nil},
		{"a piece of punctuation only", issue59, from("A bound that is not a whole number ... - ... gets 400"),
			[]string{issue59First, issue59Second}},
		{"ellipsis only covers nothing", issue59, from("...", "…", "... , ...", "- …"),
			[]string{issue59First, issue59Second}},
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

// TestNotCoveredAbbreviatedProperty: a from that quotes a bullet in
// pieces joined by "..." or "…" covers it exactly when the pieces are
// substantive (each at least four letters or digits, twelve together) and
// occur in the bullet in that order; a piece the bullet lacks never
// covers it; and a bullet that itself holds an ellipsis is covered by an
// exact quote.
func TestNotCoveredAbbreviatedProperty(t *testing.T) {
	r := rand.New(rand.NewSource(seed))
	marks := []string{"...", " ... ", "…", " … ", "....", ", ... "}
	join := func(parts []string) string {
		var b strings.Builder
		for i, p := range parts {
			if i > 0 {
				b.WriteString(marks[r.Intn(len(marks))])
			}
			b.WriteString(p)
		}
		return b.String()
	}
	for n := range 2000 {
		ws := strings.Fields(sentence(r) + " " + sentence(r))
		line := strings.Join(ws, " ")
		var pieces []string
		for i := 0; i < len(ws); {
			j := min(i+1+r.Intn(3), len(ws))
			if r.Intn(3) > 0 {
				pieces = append(pieces, strings.Join(ws[i:j], " "))
			}
			i = j + r.Intn(2)
		}
		if len(pieces) < 2 {
			continue
		}
		var want bool
		mode := r.Intn(4)
		switch mode {
		case 0: // in order
			want = substantive(pieces)
		case 1: // out of order
			slices.Reverse(pieces)
			want = substantive(pieces) && inOrder(line, pieces)
		case 2: // a piece the line lacks
			pieces = slices.Insert(pieces, r.Intn(len(pieces)+1), "zebra quartz")
			want = false
		case 3: // the line itself holds an ellipsis, quoted exactly
			line = join(ws)
			pieces = []string{line}
			want = true
		}
		from := mangle(r, join(pieces))
		got := NotCovered("- "+line+"\n", []PlanOutcome{{ID: "o", Outcome: "x", From: from}})
		if covered := len(got) == 0; covered != want {
			t.Fatalf("case %d mode %d (seed %d): from %q of %q: covered %v, want %v", n, mode, seed, from, line,
				covered, want)
		}
	}
}

// substantive restates the abbreviated from's guard: each piece at least
// four letters or digits, twelve together.
func substantive(pieces []string) bool {
	total := 0
	for _, p := range pieces {
		k := len(regexp.MustCompile(`[\pL\pN]`).FindAllString(p, -1))
		if k < 4 {
			return false
		}
		total += k
	}
	return total >= 12
}

// inOrder restates in-order, non-overlapping occurrence as a regexp.
func inOrder(line string, pieces []string) bool {
	quoted := make([]string, len(pieces))
	for i, p := range pieces {
		quoted[i] = regexp.QuoteMeta(p)
	}
	return regexp.MustCompile(strings.Join(quoted, ".*")).MatchString(line)
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
// It assumes decorate's shapes: sentence never emits brackets or
// parentheses, so every "[" starts a link and every "](" ends its text.
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
		if strings.ContainsFunc(f, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) &&
			strings.Contains(l, f) {
			return true
		}
	}
	return false
}
