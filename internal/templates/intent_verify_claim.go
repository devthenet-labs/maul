// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"fmt"
	"strings"
)

// An observed round's cited calls as a comment and the fix round's
// reproduction show them ("evaluator observed"): what each call sent and
// what maul's tools recorded it returning, read at render time from the
// round's own ledger. Display only: nothing here is judged. The note is
// SanitizeInline'd and cut at a word with a marker.

// Bounds on what a cited call shows.
const (
	// verifyClaimNoteMaxBytes bounds a note's visible text (the report
	// holds it to report.NoteMaxChars characters).
	verifyClaimNoteMaxBytes = 600
	// VerifyCitedCallsMax bounds the cited calls an observed outcome
	// shows: the most its verdict keeps (v1alpha1.MaxOutcomeCites).
	VerifyCitedCallsMax = 8
	// verifyClaimInputMaxBytes bounds one call's request as shown,
	// verifyClaimOutputMaxBytes its output.
	verifyClaimInputMaxBytes  = 1536
	verifyClaimOutputMaxBytes = 1024
)

// VerifyCitedCall is one tool call the evaluator cites, from the ledger:
// what it sent and what maul's tools observed.
type VerifyCitedCall struct {
	// Seq is its ledger seq, Verb its ledger verb; Base marks a call in the
	// base's sandbox. Input is its request, JSON as received.
	Seq   int64
	Verb  string
	Base  bool
	Input string
	// Exit is the exit status of the process the call ran, shown for a
	// call that runs one (exec, run, browser).
	Exit int
	// Output is the call's output as recorded, whole.
	Output string
	// OutputNotKept marks, on an observed round, a call whose output maul's
	// ledger did not keep (dropped to fit): it still counts, and is said
	// not kept.
	OutputNotKept bool
	// Own marks a call that ran a script or program the evaluator wrote (a
	// browser eval, an exec or run program), labelled as its own.
	Own bool
	// Navigates marks a browser goto: a call that put the page where the
	// calls after it act. A fix round's reproduction keeps the last one
	// before the cited calls however far back it is.
	Navigates bool
}

// verbWords are the ledger verbs a call can be, in maul's words; one not
// listed is shown by its name.
var verbWords = map[string]string{
	"exec":    "a program run",
	"run":     "a Go program run",
	"http":    "an http request",
	"browser": "a browser command",
}

// evaluatorNote is an evaluator's note as a comment shows it: visible
// text, cut at a word to verifyClaimNoteMaxBytes with a marker (else ended
// with a full stop), SanitizeInline'd; "" for a blank one.
func evaluatorNote(raw string) string {
	words, cut := cutWords(visibleText(strings.TrimSpace(raw)), verifyClaimNoteMaxBytes)
	note := SanitizeInline(words)
	switch {
	case note == "":
	case cut:
		note += " " + anomalyCutMarker
	case !strings.HasSuffix(note, "."):
		note += "."
	}
	return note
}

// callText is one cited call as plain text: what it is, what it sent and
// what maul's tools observed, its output cut to verifyClaimOutputMaxBytes.
func callText(c VerifyCitedCall) string {
	return callTextWithin(c, verifyClaimOutputMaxBytes)
}

// callTextWithin is callText with its output cut to outMax bytes.
func callTextWithin(c VerifyCitedCall, outMax int) string {
	return callTextIn(c, verifyClaimInputMaxBytes, outMax)
}

// callTextIn is callText with what it sent cut to inMax bytes and its
// output to outMax.
func callTextIn(c VerifyCitedCall, inMax, outMax int) string {
	lines := []string{callHead(c, true) + ":"}
	lines = append(lines, sentLinesWithin(c.Input, inMax)...)
	switch c.Verb {
	case "exec", "run", "browser":
		lines = append(lines, fmt.Sprintf("Exit status: %d", c.Exit))
	}
	switch out := strings.TrimSpace(cutRunes(visibleText(c.Output), outMax)); {
	case c.OutputNotKept:
		lines = append(lines, "Its output was not kept by maul.")
	case out == "":
		lines = append(lines, "It printed nothing.")
	default:
		lines = append(lines, "Output:", out)
	}
	return strings.Join(lines, "\n")
}

// callHead is how a comment names call c: its seq, what kind of call it
// was, at the base where it was, and the evaluator's own where it ran a
// page script or program of its own (owner decision 1: it counts, and the
// reviewers are told whose it is); returned adds that what it returned
// follows.
func callHead(c VerifyCitedCall, returned bool) string {
	what, ok := verbWords[c.Verb]
	if !ok {
		what = shown(c.Verb)
	}
	head := fmt.Sprintf("Call %d, %s", c.Seq, what)
	if c.Base {
		head += " at the base"
	}
	if !c.Own {
		return head
	}
	if c.Verb == "browser" {
		head += ", the evaluator's own page script"
	} else {
		head += ", the evaluator's own program"
	}
	if returned {
		head += ", and what it returned"
	}
	return head
}

// sentLinesWithin are sentLines cut to inMax bytes.
func sentLinesWithin(input string, inMax int) []string {
	if in := strings.TrimSpace(cutRunes(visibleText(input), inMax)); in != "" {
		return []string{"Sent: " + in}
	}
	return nil
}
