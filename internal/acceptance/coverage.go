// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import (
	"regexp"
	"strings"
	"unicode"
)

var (
	// listItem is a bullet or ordered list item's marker and text.
	listItem = regexp.MustCompile(`^(?:[-*+]|[0-9]{1,9}[.)])(?:[ \t]+(.*))?$`)
	// taskBox is a task list item's checkbox, cut from its text.
	taskBox = regexp.MustCompile(`^\[[ xX]\][ \t]+`)
	// thematicBreak is a line of three or more -, * or _ (spaces allowed):
	// a rule, not a list.
	thematicBreak = regexp.MustCompile(`^(?:(?:-[ \t]*){3,}|(?:\*[ \t]*){3,}|(?:_[ \t]*){3,})$`)
	// fromMarker is a list marker a from may quote along with the line.
	fromMarker = regexp.MustCompile(`^(?:[-*+]|[0-9]{1,9}[.)])(?:[ \t]+|$)`)
	// inlineLink is an inline link or image (its target allowing one level
	// of balanced parentheses) or a full or collapsed reference link, its
	// text kept, its target or label cut.
	inlineLink = regexp.MustCompile(`!?\[([^\]]*)\](?:\((?:[^()]|\([^()]*\))*\)|\[[^\]]*\])`)
	// inlineMarkup cuts the characters of inline code, emphasis,
	// strikethrough and backslash escapes.
	inlineMarkup = strings.NewReplacer("`", "", "*", "", "_", "", "~", "", "\\", "")
	// ellipsis is where a from was shortened: three or more dots, or "…".
	ellipsis = regexp.MustCompile(`\.{3,}|…`)
)

// minAbbreviated is the fewest letters and digits the pieces of a shortened
// from must hold together to cover an item, so a few short words found far
// apart never count as a quote.
const minAbbreviated = 12

// NotCovered returns the issue's list items that no outcome quotes in its
// from, in issue order: the lines the plan may have left out, shown to the
// approver beside the outcomes. An outcome covers an item when its from
// (with a list marker it quoted cut) occurs in the item's text, both
// compared in the same plain form: inline markup cut (inline links and
// images, and full or collapsed reference links, to their text; the
// characters of code spans, emphasis, strikethrough and escapes removed),
// case and runs of whitespace ignored, trailing punctuation cut. Inline
// HTML, entities and shortcut reference links are compared as written. A
// from with no letter or digit covers nothing.
//
// A from the planner shortened with an ellipsis ("..." or more dots, or
// "…") at its start or inside it is cut there into pieces, each trimmed of
// spaces and punctuation, the empty ones dropped. It covers an item when
// its pieces occur in the item's plain text in order, without overlapping,
// provided each piece holds a letter or digit and the pieces together hold
// at least 12 (minAbbreviated); otherwise it covers nothing. A trailing
// ellipsis is cut with the trailing punctuation, leaving a plain from.
//
// A list item is a line whose first text is a bullet (-, *, +) or ordered
// (1. or 1)) marker followed by text, indented less than four columns or
// continuing a list; a task box is cut from its text. Lines in fenced or
// indented code blocks and HTML comments are not items, nor are rules or
// blockquotes. The issue body is untrusted: the returned lines are its
// text, for the caller to bound and render as such.
func NotCovered(issueBody string, outcomes []PlanOutcome) []string {
	froms := make([][]string, 0, len(outcomes))
	for _, o := range outcomes {
		if pieces := fromPieces(o.From); pieces != nil {
			froms = append(froms, pieces)
		}
	}
	var out []string
	for _, item := range listItems(issueBody) {
		text := plainText(item)
		covered := false
		for _, pieces := range froms {
			if containsInOrder(text, pieces) {
				covered = true
				break
			}
		}
		if !covered {
			out = append(out, item)
		}
	}
	return out
}

// fromPieces returns a from's plain pieces as NotCovered defines them: the
// whole from when it has no ellipsis, nil when it covers nothing.
func fromPieces(from string) []string {
	f := plainText(fromMarker.ReplaceAllString(strings.TrimSpace(from), ""))
	if !ellipsis.MatchString(f) {
		if !strings.ContainsFunc(f, isAlnum) {
			return nil
		}
		return []string{f}
	}
	var pieces []string
	alnum := 0
	for _, p := range ellipsis.Split(f, -1) {
		p = strings.Trim(p, ".,;:!? ")
		if p == "" {
			continue
		}
		if !strings.ContainsFunc(p, isAlnum) {
			return nil
		}
		for _, r := range p {
			if isAlnum(r) {
				alnum++
			}
		}
		pieces = append(pieces, p)
	}
	if alnum < minAbbreviated {
		return nil
	}
	return pieces
}

// containsInOrder reports whether every piece occurs in text after the one
// before it, without overlapping it.
func containsInOrder(text string, pieces []string) bool {
	for _, p := range pieces {
		i := strings.Index(text, p)
		if i < 0 {
			return false
		}
		text = text[i+len(p):]
	}
	return true
}

func isAlnum(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// plainText is the comparison form: inline markup cut, lowercased,
// whitespace runs one space, trailing punctuation and space trimmed. The
// planner may quote an issue line with or without its markup, so both
// sides are compared without it.
func plainText(s string) string {
	s = inlineMarkup.Replace(inlineLink.ReplaceAllString(s, "$1"))
	s = strings.ToLower(strings.Join(strings.Fields(s), " "))
	return strings.TrimRight(s, ".,;:!? ")
}

// listItems returns the text of every list item of a markdown body, in
// order, as NotCovered defines them.
func listItems(body string) []string {
	var (
		items     []string
		fence     string // the open fence's run of ` or ~, or ""
		inComment bool
		inList    bool
	)
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSuffix(line, "\r")
		indent, rest := indentOf(line)
		if fence != "" {
			if indent < 4 && closesFence(rest, fence) {
				fence = ""
			}
			continue
		}
		if !inComment && indent < 4 {
			if f := openFence(rest); f != "" {
				fence = f
				continue
			}
		}
		line, inComment = stripComments(line, inComment)
		indent, rest = indentOf(line)
		rest = strings.TrimRight(rest, " \t")
		switch {
		case rest == "":
			continue
		case indent >= 4 && !inList:
			continue // an indented code block
		case thematicBreak.MatchString(rest):
			inList = false
			continue
		}
		m := listItem.FindStringSubmatch(rest)
		if m == nil {
			if indent == 0 {
				inList = false
			}
			continue
		}
		inList = true
		text := strings.TrimSpace(taskBox.ReplaceAllString(m[1], ""))
		if text != "" {
			items = append(items, text)
		}
	}
	return items
}

// indentOf returns a line's indentation in columns (a tab to the next
// multiple of four) and the text after it.
func indentOf(line string) (int, string) {
	cols := 0
	for i, r := range line {
		switch r {
		case ' ':
			cols++
		case '\t':
			cols += 4 - cols%4
		default:
			return cols, line[i:]
		}
	}
	return cols, ""
}

// openFence returns the fence run a line opens (three or more backticks or
// tildes), or "".
func openFence(s string) string {
	for _, c := range []string{"`", "~"} {
		n := len(s) - len(strings.TrimLeft(s, c))
		if n >= 3 {
			return s[:n]
		}
	}
	return ""
}

// closesFence reports whether a line closes the open fence: a run of the
// same character at least as long, and nothing after it.
func closesFence(s, fence string) bool {
	run := strings.TrimLeft(s, fence[:1])
	return len(s)-len(run) >= len(fence) && strings.TrimSpace(run) == ""
}

// stripComments removes the HTML comments of a line, given whether one is
// open at its start, and reports whether one is open at its end.
func stripComments(line string, open bool) (string, bool) {
	var b strings.Builder
	for {
		if open {
			end := strings.Index(line, "-->")
			if end < 0 {
				return b.String(), true
			}
			line, open = line[end+3:], false
			continue
		}
		start := strings.Index(line, "<!--")
		if start < 0 {
			b.WriteString(line)
			return b.String(), false
		}
		b.WriteString(line[:start])
		line, open = line[start+4:], true
	}
}
