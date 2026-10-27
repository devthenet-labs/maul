// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package untrusted

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"testing/quick"
	"unicode"
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

// referenceEscape is Escape as first written, rune by rune through fmt: the
// fast path must print exactly what it printed.
func referenceEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, width := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && width == 1 {
			fmt.Fprintf(&b, "<0x%02X>", s[i])
			i++
			continue
		}
		switch {
		case r == '\n' || r == '\t', r == '\r' && i+1 < len(s) && s[i+1] == '\n':
			b.WriteRune(r)
		case unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) ||
			unicode.Is(unicode.Variation_Selector, r) || unicode.Is(unicode.Other_Default_Ignorable_Code_Point, r):
			fmt.Fprintf(&b, "<U+%04X>", r)
		default:
			b.WriteRune(r)
		}
		i += width
	}
	return b.String()
}

// TestEscapeMatchesReferenceSeeded: every byte, the rune classes Escape
// names, and random runes across the whole range escape as the reference
// does.
func TestEscapeMatchesReferenceSeeded(t *testing.T) {
	var all strings.Builder
	for c := range 256 {
		all.WriteByte(byte(c))
		all.WriteString("\r")
	}
	if got, want := Escape(all.String()), referenceEscape(all.String()); got != want {
		t.Fatalf("every byte: Escape = %q, want %q", got, want)
	}
	// Each rune spelled as its UTF-8 bytes: é, U+0085, U+00AD, U+200B, U+202E,
	// U+2028, U+2029, U+FE0F, U+FEFF, U+4E16, U+E0041, U+E0100, U+10FFFF.
	alphabet := []string{"a", " ", "~", "\x7f", "\n", "\r", "\r\n", "\t", "\x00", "\x1b", "\xff", "\xc3", "\xc3\xa9",
		"\xc2\x85", "\xc2\xad", "\xe2\x80\x8b", "\xe2\x80\xae", "\xe2\x80\xa8", "\xe2\x80\xa9", "\xef\xb8\x8f",
		"\xef\xbb\xbf", "\xe4\xb8\x96", "\xf3\xa0\x81\x81", "\xf3\xa0\x84\x80", "\xf4\x8f\xbf\xbf"}
	rng := rand.New(rand.NewSource(20261027))
	for i := range 2000 {
		var b strings.Builder
		for range rng.Intn(40) {
			if rng.Intn(4) == 0 {
				b.WriteRune(rune(rng.Intn(utf8.MaxRune + 1)))
				continue
			}
			b.WriteString(alphabet[rng.Intn(len(alphabet))])
		}
		if got, want := Escape(b.String()), referenceEscape(b.String()); got != want {
			t.Fatalf("seeded case %d: Escape(%q) = %q, want %q", i, b.String(), got, want)
		}
	}
}
