// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestCutWords: text over the limit is cut at the last whitespace that fits,
// never mid-word, and reported cut; text that fits is left as it is. A
// single word longer than the limit is cut on a rune boundary.
func TestCutWords(t *testing.T) {
	tests := []struct {
		name, in string
		limit    int
		want     string
		cut      bool
	}{
		{"fits", "Neither comes from this change.", 40, "Neither comes from this change.", false},
		{"exactly fits", "abc def", 7, "abc def", false},
		{"at a space", "Neither comes from this change.", 24, "Neither comes from this", true},
		{"ends on a space", "Neither comes from this change.", 23, "Neither comes from this", true},
		{"mid-word", "one, two three", 13, "one, two", true},
		{"multibyte", "日本 日本 日本", 16, "日本 日本", true},
		{"one long word", strings.Repeat("é", 10), 9, "éééé", true},
		// A space only in the first half: backing up there would drop most
		// of what fits, so the cut stays on a rune boundary.
		{"early space only", "a " + strings.Repeat("x", 40), 20, "a " + strings.Repeat("x", 18), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, cut := cutWords(tt.in, tt.limit)
			if got != tt.want || cut != tt.cut {
				t.Errorf("cutWords(%q, %d) = %q, %v; want %q, %v", tt.in, tt.limit, got, cut, tt.want, tt.cut)
			}
			if len(got) > tt.limit || !utf8.ValidString(got) {
				t.Errorf("cutWords(%q, %d) = %q: %d bytes or invalid UTF-8", tt.in, tt.limit, got, len(got))
			}
		})
	}
}

// TestAnomalyLineCutMarked (D-B13): a look-around anomaly longer than the
// comment shows is cut at a word and marked as cut by maul, so the cut text
// is never taken for the evaluator's own words; the route and the rest of
// the line follow as before. One that fits carries no marker.
func TestAnomalyLineCutMarked(t *testing.T) {
	// 37.5 words fit: the cut falls in the 38th.
	summary := strings.Repeat("abcdefg ", 45) + "Neither comes from this change."
	line := anomalyLine(VerifyAnomaly{Summary: summary, Route: "/"})
	if want := strings.Repeat("abcdefg ", 37) + anomalyCutMarker + ", on `/`."; !strings.Contains(line, want) {
		t.Errorf("anomaly line = %q, want the cut at a word and marked before the route", line)
	}
	if strings.Contains(line, "abcd "+anomalyCutMarker) || strings.Contains(line, "Neither") {
		t.Errorf("anomaly line = %q, want no word cut and nothing past the limit", line)
	}
	if line := anomalyLine(VerifyAnomaly{Summary: "Neither comes from this change.", Route: "/"}); strings.Contains(line,
		anomalyCutMarker) {
		t.Errorf("anomaly line = %q, want no marker on a summary that fits", line)
	}
}
