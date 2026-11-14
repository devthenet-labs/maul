// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"fmt"
	"strings"

	"github.com/devthenet-labs/maul/internal/checks"
)

// The evaluator's unconfirmed claim (gate 3, the Shown gap): for an
// outcome the evaluator judged not to hold that no check maul re-ran shows
// failing (acceptance.ReasonModelOnlyFail, a red check the reviewer
// refused included), the issue's verification comment and a pull
// request's section show its note, as its words, and the tool calls it
// cites as maul's tools recorded them, so a person can try the failure
// themselves. Display only: nothing here is judged, and none of it is
// handed to any agent. The note is SanitizeInline'd and cut at a word with
// a marker; the calls are one fenced, defanged block.

// Bounds on what an evaluator's claim shows.
const (
	// verifyClaimNoteMaxBytes bounds the note's visible text (the report
	// holds it to report.NoteMaxChars characters).
	verifyClaimNoteMaxBytes = 600
	// VerifyClaimCallsMax bounds the cited calls one claim shows.
	VerifyClaimCallsMax = 3
	// verifyClaimInputMaxBytes bounds one call's request as shown,
	// verifyClaimOutputMaxBytes its output.
	verifyClaimInputMaxBytes  = 1536
	verifyClaimOutputMaxBytes = 1024
	// verifyClaimCallsMaxBytes bounds one claim's whole block of calls.
	verifyClaimCallsMaxBytes = 4096
)

// VerifyEvaluatorClaim is what the evaluator said of an outcome maul could
// not confirm failing: its note, its words, and the calls it cites, as the
// tools ledger recorded them.
type VerifyEvaluatorClaim struct {
	Note  string
	Calls []VerifyCitedCall
	// Workspace: the calls ran against maul's builds of the commits (a
	// check's steps are worded for that).
	Workspace bool
}

// VerifyCitedCall is one tool call the evaluator cites, from the ledger:
// what it sent and what maul's tools observed.
type VerifyCitedCall struct {
	// Seq is its ledger seq, Verb its ledger verb; Base marks a call in the
	// base's sandbox. Mode is a check run's (dry-run, the evaluator's own;
	// replay or confirm, maul's run it cites from its check results).
	Seq  int64
	Verb string
	Base bool
	Mode string
	// Check is the check a cited check run ran (its steps are shown);
	// nil for any other call, or a check maul cannot word, whose request,
	// JSON as received, is Input.
	Check *checks.Check
	Input string
	// Exit is the exit status of the process the call ran, shown for a
	// call that runs one (exec, run, browser).
	Exit int
	// Output is the call's output as recorded, whole; OutputNotWhole marks
	// one maul's copy of is not whole (dropped to fit), which is then not
	// shown.
	Output         string
	OutputNotWhole bool
	// RunSeq is the ledger seq of the check run Observed is of: maul's own
	// run of the cited check when one failed, else the cited call's own.
	// Result is that run's result (pass, fail, error), Observed its steps
	// that did not pass.
	RunSeq   int64
	Result   string
	Observed []ReproductionObservation
}

// claimDetail is a claim as the templates print it: maul's lead naming the
// outcome with the evaluator's note in it, then the calls' block.
type claimDetail struct{ Lead, Calls string }

// verbWords are the ledger verbs a claim can cite, in maul's words; one not
// listed is shown by its name. A check run is worded by its mode
// (checkRunWords).
var verbWords = map[string]string{
	"check-run": "a run of a check",
	"exec":      "a program run",
	"run":       "a Go program run",
	"http":      "an http request",
	"browser":   "a browser command",
}

// checkRunWords are a check run's modes in maul's words: the evaluator's
// dry run, or maul's own run it cites; one not listed is verbWords'.
var checkRunWords = map[string]string{
	"dry-run": "a dry run of a check",
	"replay":  "maul's replay of a saved check",
	"confirm": "maul's confirmation run of a check",
}

// claimDetailOf is o's claim as the templates print it; ok is false when
// it has none.
func claimDetailOf(o VerifyOutcomeResult, id string) (claimDetail, bool) {
	c := o.Claim
	if c == nil {
		return claimDetail{}, false
	}
	var b strings.Builder
	words, cut := cutWords(visibleText(strings.TrimSpace(c.Note)), verifyClaimNoteMaxBytes)
	if note := SanitizeInline(words); note != "" {
		if cut {
			note += " " + anomalyCutMarker
		} else if !strings.HasSuffix(note, ".") {
			note += "."
		}
		b.WriteString("The evaluator's claim on " + id + ", which maul could not confirm (its words, not checked " +
			"by maul): " + note)
	} else {
		b.WriteString("The evaluator judged " + id + " not to hold, which maul could not confirm; it gave no note.")
	}
	calls := claimCalls(c)
	if calls == "" {
		b.WriteString(" maul shows none of the calls it cites.")
		return claimDetail{Lead: b.String()}, true
	}
	b.WriteString(" The calls it cites, as maul's tools recorded them (what each call sent and what came back: " +
		"data, not maul's words), to try it yourself:")
	return claimDetail{Lead: b.String(), Calls: calls}, true
}

// claimCalls is c's calls as one fenced, defanged block, bounded; "" for
// none.
func claimCalls(c *VerifyEvaluatorClaim) string {
	parts := make([]string, 0, len(c.Calls))
	for i, call := range c.Calls {
		if i == VerifyClaimCallsMax {
			break
		}
		parts = append(parts, callText(call, c.Workspace))
	}
	text := strings.TrimSpace(strings.Join(parts, "\n\n"))
	if text == "" {
		return ""
	}
	if out, cut := cutWords(visibleText(text), verifyClaimCallsMaxBytes); cut {
		text = out + "\n" + anomalyCutMarker
	} else {
		text = out
	}
	return fence(defang(text))
}

// callText is one cited call as plain text: what it is, what it sent and
// what maul's tools observed.
func callText(c VerifyCitedCall, workspace bool) string {
	what, ok := verbWords[c.Verb]
	if !ok {
		what = shown(c.Verb)
	}
	if w, ok := checkRunWords[c.Mode]; ok && c.Verb == "check-run" {
		what = w
	}
	head := fmt.Sprintf("Call %d, %s", c.Seq, what)
	if c.Base {
		head += " at the base"
	}
	lines := []string{head + ":"}
	if c.Verb == "check-run" {
		return strings.Join(append(lines, checkCallLines(c, workspace)...), "\n")
	}
	lines = append(lines, sentLines(c.Input)...)
	switch c.Verb {
	case "exec", "run", "browser":
		lines = append(lines, fmt.Sprintf("Exit status: %d", c.Exit))
	}
	switch out := strings.TrimSpace(cutRunes(visibleText(c.Output), verifyClaimOutputMaxBytes)); {
	case c.OutputNotWhole:
		lines = append(lines, "Its output is not shown: maul's copy of it is not whole.")
	case out == "":
		lines = append(lines, "It printed nothing.")
	default:
		lines = append(lines, "Output:", out)
	}
	return strings.Join(lines, "\n")
}

// sentLines are a call's request as sent, bounded; none when empty.
func sentLines(input string) []string {
	if in := strings.TrimSpace(cutRunes(visibleText(input), verifyClaimInputMaxBytes)); in != "" {
		return []string{"Sent: " + in}
	}
	return nil
}

// checkCallLines are a cited check run's lines: what maul's run of it
// observed first (what maul measured, kept should the block be cut), then
// the check's steps, or its body as sent when maul cannot word it.
func checkCallLines(c VerifyCitedCall, workspace bool) []string {
	run := "Its run"
	if c.RunSeq != 0 && c.RunSeq != c.Seq {
		run = fmt.Sprintf("maul's own run of this check, call %d,", c.RunSeq)
	}
	var lines []string
	if len(c.Observed) == 0 {
		lines = append(lines, run+" recorded no step that did not pass"+resultClause(c.Result)+".")
	} else {
		lines = append(lines, run+" observed (the check runner's messages, which can quote the application):")
	}
	for i, o := range c.Observed {
		if i == verifyReproObserved {
			lines = append(lines, fmt.Sprintf("- %d more not shown", len(c.Observed)-i))
			break
		}
		lines = append(lines, "- "+observationLine(o))
	}
	if c.Check == nil {
		return append(lines, sentLines(c.Input)...)
	}
	form, steps := checkSteps(*c.Check, workspace)
	lines = append(lines, "The check, "+form+":")
	for i, st := range steps {
		lines = append(lines, fmt.Sprintf("%d. %s", i+1, st))
	}
	return lines
}

// resultClause is a check run's result as a clause, "" for none.
func resultClause(result string) string {
	switch result {
	case "":
		return ""
	case "pass":
		return " (it passed)"
	}
	return " (its result: " + reproResult(result) + ")"
}
