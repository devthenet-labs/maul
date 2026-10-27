// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package untrusted

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Visible is one untrusted field made safe to show: cut before escaping, and
// again after it, so a malicious or malformed provider response cannot
// expand without bound.
func Visible(s string, maxBytes int) string {
	return Cap(Escape(Cut(s, maxBytes)), maxBytes)
}

// Escape makes untrusted text legible to both a human and the model. CRLF,
// LF and tabs keep their ordinary layout; every other invisible, invalid
// byte or non-ASCII separator becomes a printed escape.
func Escape(s string) string {
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

// Cap cuts s to at most maxBytes on a rune boundary, marking the cut.
func Cap(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	const note = "\n[truncated]\n"
	end := maxBytes - len(note)
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end] + note
}

// Cut cuts s to at most n bytes on a rune boundary, without a mark.
func Cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
