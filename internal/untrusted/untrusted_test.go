// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package untrusted

import (
	"math/rand"
	"strings"
	"testing"
	"testing/quick"
	"unicode/utf8"
)

func quickConfig(seed int64) *quick.Config {
	return &quick.Config{MaxCount: 500, Rand: rand.New(rand.NewSource(seed))}
}

func TestEscapeExamples(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain text", "plain text"},
		{"a\nb\tc", "a\nb\tc"},
		{"a\r\nb", "a\r\nb"},
		{"a\rb", "a<U+000D>b"},
		{"a\x1b[31mb", "a<U+001B>[31mb"},
		{"a\u202eb", "a<U+202E>b"}, // bidi override
		{"a\u200bb", "a<U+200B>b"}, // zero-width space
		{"a\u2028b\u2029", "a<U+2028>b<U+2029>"},
		{"a\xffb", "a<0xFF>b"},     // invalid byte
		{"a\ufe0fb", "a<U+FE0F>b"}, // variation selector
		{"日本語", "日本語"},
	}
	for _, tc := range cases {
		if got := Escape(tc.in); got != tc.want {
			t.Errorf("Escape(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCutExamples(t *testing.T) {
	cases := []struct {
		s    string
		n    int
		want string
	}{
		{"", 0, ""},
		{"abc", 3, "abc"},
		{"abcdef", 3, "abc"},
		{"aé", 2, "a"},
		{"€uro", 3, "€"},
		{"日本語", 7, "日本"},
	}
	for _, tc := range cases {
		if got := Cut(tc.s, tc.n); got != tc.want {
			t.Errorf("Cut(%q, %d) = %q, want %q", tc.s, tc.n, got, tc.want)
		}
	}
}

func TestCapExamples(t *testing.T) {
	if got := Cap("short", 100); got != "short" {
		t.Errorf("Cap short = %q", got)
	}
	got := Cap(strings.Repeat("é", 100), 40)
	if len(got) > 40 || !utf8.ValidString(got) || !strings.HasSuffix(got, "\n[truncated]\n") {
		t.Errorf("Cap(long, 40) = %q (%d bytes)", got, len(got))
	}
}

// TestVisibleProperties: whatever the input, the output is within the bound,
// valid UTF-8, and holds none of the invisible characters Escape names.
func TestVisibleProperties(t *testing.T) {
	const bound = 64
	f := func(s string) bool {
		got := Visible(s, bound)
		if len(got) > bound || !utf8.ValidString(got) {
			return false
		}
		return !strings.ContainsAny(got, "\u202e\u200b\x1b\ufeff")
	}
	if err := quick.Check(f, quickConfig(1)); err != nil {
		t.Error(err)
	}
}

// TestVisibleExpansionBounded: a field of nothing but invisible characters
// expands under escaping, and is still held to the bound.
func TestVisibleExpansionBounded(t *testing.T) {
	got := Visible(strings.Repeat("\u202e", 10000), 256)
	if len(got) > 256 {
		t.Fatalf("len = %d, want <= 256", len(got))
	}
	if !strings.Contains(got, "<U+202E>") {
		t.Errorf("escape not shown: %q", got)
	}
}
