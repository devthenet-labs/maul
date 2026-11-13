// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package command

import (
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Misplaced reports a comment that carries no command but has a later line
// that reads as one of the verbs surface offers, and that verb: what its
// author most likely meant as a command, written below other text, where
// the grammar never reads one. It is for a reply saying that commands go on
// a comment's first line, never for acting: a misplaced command is text.
//
// It is false whenever Parse is true, and its verb is always one Available
// returns for surface. It reads only plain Markdown lines that stand alone
// (split at CRLF, CR and LF, as GitHub splits them), and skips, as the
// grammar treats them as text, a line that is quoted or continues a quote,
// that is inside a fenced code block, an HTML comment or a code span opened
// on an earlier line, that is indented as code, that holds a code span
// itself, or that holds any other Unicode line break. So it can miss a
// command, never find one in text no reader would take for one.
func Misplaced(body string, surface Surface) (verb string, ok bool) {
	offered := Available(surface)
	if len(offered) == 0 {
		return "", false
	}
	if _, ok := parseGrammar(body); ok {
		return "", false
	}
	var (
		first   = true
		quote   bool   // the line before continues a quote
		fence   string // the open fence, or ""
		backs   int    // backticks outside fences so far
		comment bool   // inside an HTML comment
	)
	for _, line := range markdownLines(body) {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			quote = false
			continue
		}
		wasFirst := first
		first = false
		if fence != "" {
			if closesFence(trimmed, fence) {
				fence = ""
			}
			continue
		}
		if f := opensFence(line); f != "" {
			fence = f
			continue
		}
		inSpan, inComment := backs%2 == 1, comment
		backs += strings.Count(line, "`")
		comment = commentOpen(comment, line)
		isQuote := strings.HasPrefix(trimmed, ">")
		quoted := quote || isQuote
		quote = quoted
		if wasFirst || inSpan || inComment || quoted || indentedCode(line) || strings.Contains(line, "`") ||
			strings.ContainsAny(line, "\v\f\u0085  ") {
			continue
		}
		if v := lineVerb(trimmed); v != "" && slices.Contains(offered, v) {
			return v, true
		}
	}
	return "", false
}

// markdownLines splits body at CRLF, CR and LF, and at no other line break.
func markdownLines(body string) []string {
	return strings.Split(strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\r", "\n"), "\n")
}

// lineVerb is the verb of trimmed when it reads as "/maul <verb>": the
// prefix as a whole word, then the next word as verb makes it; "" for
// anything else.
func lineVerb(trimmed string) string {
	if len(trimmed) < len(Prefix) || !strings.EqualFold(trimmed[:len(Prefix)], Prefix) {
		return ""
	}
	rest := trimmed[len(Prefix):]
	if r, _ := utf8.DecodeRuneInString(rest); rest == "" || !unicode.IsSpace(r) {
		return ""
	}
	word, _ := cutWord(strings.TrimLeftFunc(rest, unicode.IsSpace))
	return verb(word)
}

// opensFence is the fence line opens, as CommonMark reads one: at most three
// spaces, then three or more backticks or tildes; "" when it opens none.
func opensFence(line string) string {
	indent := len(line) - len(strings.TrimLeft(line, " "))
	if indent > 3 {
		return ""
	}
	rest := line[indent:]
	for _, c := range []string{"`", "~"} {
		n := len(rest) - len(strings.TrimLeft(rest, c))
		if n >= 3 {
			return strings.Repeat(c, n)
		}
	}
	return ""
}

// closesFence reports whether trimmed closes fence: that character, at least
// as many times, and nothing else.
func closesFence(trimmed, fence string) bool {
	return strings.HasPrefix(trimmed, fence) && strings.Trim(trimmed, fence[:1]) == ""
}

// commentOpen reports whether an HTML comment is open after line, given
// whether one was open before it.
func commentOpen(open bool, line string) bool {
	for {
		if open {
			i := strings.Index(line, "-->")
			if i < 0 {
				return true
			}
			line, open = line[i+3:], false
			continue
		}
		i := strings.Index(line, "<!--")
		if i < 0 {
			return false
		}
		line, open = line[i+4:], true
	}
}
