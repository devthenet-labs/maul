// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// reportCutNote ends a report patchy cut short to fit GitHub's size limit.
const reportCutNote = "\n\n_patchy cut this report here to fit GitHub's size limit._"

// defaultRemediationCommit is the commit message when the agent left none:
// GitHub refuses a commit without one.
const defaultRemediationCommit = "fix: remediate security finding (patchy)"

// sanitizeDefanged is Sanitize for markdown that may also be read as plain
// text on the default branch — a pull request body, which a repository may
// have GitHub copy into the merge or squash commit, where inline code
// protects nothing. defang only inserts a space between punctuation and the
// letter or digit after it, which no markdown construct turns on, so the
// result stays inert markdown.
func sanitizeDefanged(s string) string { return defang(Sanitize(s)) }

// fitReport is clean(report) when that fits in budget bytes, and otherwise
// clean of the longest prefix of report whose result, with reportCutNote,
// does — "" when not even the note fits. The report is cut before it is
// cleaned, never after: a cut through cleaned markdown could leave a code
// span unclosed and its content live. GitHub limits a body or comment in
// characters, at most its length in bytes, so a byte budget is safe.
func fitReport(report string, budget int, clean func(string) string) string {
	if out := clean(report); len(out) <= budget {
		return out
	}
	room := budget - len(reportCutNote)
	if room < 0 {
		return ""
	}
	best := ""
	for lo, hi := 0, len(report); lo <= hi; {
		mid := (lo + hi) / 2
		n := mid
		for n > 0 && n < len(report) && !utf8.RuneStart(report[n]) {
			n--
		}
		if out := clean(report[:n]); len(out) <= room {
			best, lo = out, mid+1
		} else {
			hi = mid - 1
		}
	}
	return best + reportCutNote
}

// RemediationCommitMessage is the commit message patchy pushes for a
// remediation changeset: the agent's own message(s), as plain text. A
// commit message lands on the default branch as written when the pull
// request is merged or rebased, and GitHub reads it there as plain text:
// "Fixes #7" in it would close issue 7 — possibly the finding's own
// tracking issue, silently taking it out of the pipeline — and a code span
// protects nothing. So every mention and issue reference is defanged, and
// every character that renders as nothing is shown as its code point, so a
// reviewer of the history reads what the bytes say. A message with nothing
// in it gets patchy's own.
func RemediationCommitMessage(agent string) string {
	msg := defang(visibleText(agent))
	if strings.TrimSpace(msg) == "" {
		return defaultRemediationCommit
	}
	return msg
}

// PRBodyFallback is the pull request body when PRBody cannot render: patchy's
// own words only, never the agent's report.
func PRBodyFallback(issue int) string {
	return fmt.Sprintf("Fixes #%d\n\npatchy opened this pull request automatically; its remediation report is on "+
		"the tracking issue. Review and merge it like any other change.", issue)
}
