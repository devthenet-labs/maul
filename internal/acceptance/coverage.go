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
)

// NotCovered returns the issue's list items that no outcome quotes in its
// from, in issue order: the lines the plan may have left out, shown to the
// approver beside the outcomes. An outcome covers an item when its from,
// compared without regard to case or runs of whitespace (and with a list
// marker it quoted cut), occurs in the item's text; an empty from covers
// nothing.
//
// A list item is a line whose first text is a bullet (-, *, +) or ordered
// (1. or 1)) marker followed by text, indented less than four columns or
// continuing a list; a task box is cut from its text. Lines in fenced or
// indented code blocks and HTML comments are not items, nor are rules or
// blockquotes. The issue body is untrusted: the returned lines are its
// text, for the caller to bound and render as such.
func NotCovered(issueBody string, outcomes []PlanOutcome) []string {
	froms := make([]string, 0, len(outcomes))
	for _, o := range outcomes {
		f := foldText(fromMarker.ReplaceAllString(strings.TrimSpace(o.From), ""))
		if strings.ContainsFunc(f, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) {
			froms = append(froms, f)
		}
	}
	var out []string
	for _, item := range listItems(issueBody) {
		text := foldText(item)
		covered := false
		for _, f := range froms {
			if strings.Contains(text, f) {
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

// foldText is the comparison form: lowercased, whitespace runs one space,
// trimmed.
func foldText(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }

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
