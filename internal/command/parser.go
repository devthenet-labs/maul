// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package command

import (
	"strings"
	"unicode"

	"github.com/devthenet-labs/maul/internal/action"
)

// The modifiers /maul verify takes.
const (
	// VerifyFix also re-arms automatic fixes.
	VerifyFix = "fix"
	// VerifyStop halts automatic fixes and verifies nothing: it only
	// stops maul fixing failures on its own.
	VerifyStop = "stop"
	// VerifyReset first resets the preview's data. It is a modifier of
	// verify, never a verb: action.VerbReset is the integration-scoped
	// delete-everything, which no intent surface offers.
	VerifyReset = "--reset"
)

// maxCriterionIDBytes bounds a criterion id, as the plan grammar does.
const maxCriterionIDBytes = 32

// VerifyArgs is what a /maul verify asks for beyond a verification.
type VerifyArgs struct {
	// Fix: "/maul verify fix".
	Fix bool
	// Stop: "/maul verify stop".
	Stop bool
	// Reset: "/maul verify --reset".
	Reset bool
}

// Args returns the arguments of the command body carries: the
// whitespace-separated words after its verb on the command's own line. Text
// on the lines below is the note and never an argument, so a command line
// with nothing after its verb has none, whatever follows it. ok is false when
// body carries no /maul command (Parse's rule, code blocks and quotes
// included).
func Args(body string) (args []string, ok bool) {
	if _, ok := parseGrammar(body); !ok {
		return nil, false
	}
	line, _ := firstLine(lineBreaks.Replace(body))
	rest := strings.TrimLeftFunc(line, unicode.IsSpace)[len(Prefix):]
	_, after := cutWord(strings.TrimLeftFunc(rest, unicode.IsSpace))
	return strings.Fields(after), true
}

// ParseVerify reads body as /maul verify: no argument, VerifyFix,
// VerifyStop or VerifyReset, at most one of them, matched without regard to
// ASCII case. ok is false for any other verb and for any other argument,
// which asks for the help instead: nothing is ever guessed from a verify
// command a human mistyped.
func ParseVerify(body string) (VerifyArgs, bool) {
	c, ok := parseGrammar(body)
	if !ok || c.Verb != action.VerbVerify {
		return VerifyArgs{}, false
	}
	args, _ := Args(body)
	switch {
	case len(args) == 0:
		return VerifyArgs{}, true
	case len(args) > 1:
		return VerifyArgs{}, false
	case strings.EqualFold(args[0], VerifyFix):
		return VerifyArgs{Fix: true}, true
	case strings.EqualFold(args[0], VerifyStop):
		return VerifyArgs{Stop: true}, true
	case strings.EqualFold(args[0], VerifyReset):
		return VerifyArgs{Reset: true}, true
	}
	return VerifyArgs{}, false
}

// AcceptAll is the argument of /maul accept that names every criterion or
// outcome an approver may accept now (for a plan with no criteria, the
// change as a whole). It matches the id grammar, so ParseAccept returns it
// like an id; what it means is the caller's.
const AcceptAll = "all"

// ParseAccept reads body as /maul accept <id>: exactly one argument, a
// criterion or outcome id as the plan grammar writes one (a lower-case ASCII
// letter, then up to 31 lower-case letters, digits and dashes), or
// AcceptAll. The note, if any, is on the lines below. ok is false for any
// other verb and anything else, so an id returned is always safe to echo.
// Whether the intent has such a criterion or outcome is the caller's to
// decide.
func ParseAccept(body string) (id string, ok bool) {
	c, ok := parseGrammar(body)
	if !ok || c.Verb != action.VerbAccept {
		return "", false
	}
	args, _ := Args(body)
	if len(args) != 1 || !criterionID(args[0]) {
		return "", false
	}
	return args[0], true
}

// ParsePreview reports that body is /maul preview with no argument (a note
// may follow on the lines below). Any argument asks for the help instead.
func ParsePreview(body string) bool {
	c, ok := parseGrammar(body)
	if !ok || c.Verb != action.VerbPreview {
		return false
	}
	args, _ := Args(body)
	return len(args) == 0
}

// criterionID reports whether s matches ^[a-z][a-z0-9-]{0,31}$, the plan
// grammar's id (internal/acceptance, which this pure package does not
// import).
func criterionID(s string) bool {
	if s == "" || len(s) > maxCriterionIDBytes || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for i := 1; i < len(s); i++ {
		if c := s[i]; (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}
