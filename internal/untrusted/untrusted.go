// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package untrusted

import (
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
//
// Printable ASCII, LF and tab are copied a byte at a time without decoding
// or a Unicode table lookup: every feedback item, check output and status
// detail an intent pass shows goes through here, most of it plain ASCII.
func Escape(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if c := s[i]; c >= ' ' && c < utf8.RuneSelf-1 || c == '\n' || c == '\t' {
			b.WriteByte(c)
			i++
			continue
		}
		r, width := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && width == 1 {
			writeHex(&b, "<0x", uint32(s[i]), 2)
			i++
			continue
		}
		switch {
		case r == '\r' && i+1 < len(s) && s[i+1] == '\n':
			b.WriteRune(r)
		case unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) ||
			unicode.Is(unicode.Variation_Selector, r) || unicode.Is(unicode.Other_Default_Ignorable_Code_Point, r):
			writeHex(&b, "<U+", uint32(r), 4)
		default:
			b.WriteRune(r)
		}
		i += width
	}
	return b.String()
}

// writeHex writes prefix, v in upper-case hex padded to at least digits
// digits, and a closing ">": fmt's "%02X" and "%04X" without fmt.
func writeHex(b *strings.Builder, prefix string, v uint32, digits int) {
	const upper = "0123456789ABCDEF"
	var buf [8]byte
	n := len(buf)
	for v > 0 || len(buf)-n < digits {
		n--
		buf[n] = upper[v&0xF]
		v >>= 4
	}
	b.WriteString(prefix)
	b.Write(buf[n:])
	b.WriteByte('>')
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
