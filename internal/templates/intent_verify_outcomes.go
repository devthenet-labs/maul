// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/devthenet-labs/maul/internal/command"
)

// The agent evaluator's outcomes, as the verification section and comment
// show them (SPEC S1-20): one row per approved outcome with its kind, its
// decided result, maul's words on why and the check it rests on, rendered by
// maul from structured data, and for a failure the application's output in
// a fenced, sanitised and defanged "app output" block naming the outcome.
// The evaluator's own words appear only as a look-around anomaly's summary,
// marked as its words and sanitised.

// Outcome results as a comment shows them (status.verification.outcomes).
const (
	OutcomeResultPassed      = "passed"
	OutcomeResultFailed      = "failed"
	OutcomeResultUnsure      = "unsure"
	OutcomeResultQuarantined = "quarantined"
	OutcomeResultAccepted    = "accepted"
)

// outcomeResultWords are the results in maul's words; one not listed shows
// as unknown, never as the text it carried.
var outcomeResultWords = map[string]string{
	OutcomeResultPassed:      "confirmed",
	OutcomeResultFailed:      "failed",
	OutcomeResultUnsure:      "unsure: maul couldn't confirm it",
	OutcomeResultQuarantined: "quarantined: its check passed only on retry",
	OutcomeResultAccepted:    "accepted by a human, not re-checked",
}

// outcomeReasonWords are acceptance.Decide's reasons in maul's words; one
// not listed adds nothing.
var outcomeReasonWords = map[string]string{
	"manual":              "only a human can close it",
	"retired":             "its check was retired this round",
	"disputed":            "the evaluator disputes its failing check, so a replacement check is replayed",
	"env-failed":          "the preview failed while it was checked",
	"infra":               "maul's own machinery failed while it was checked",
	"evaluator-disagrees": "a check failed, but the evaluator did not agree that it is a defect",
	"model-only-pass":     "the evaluator says it holds, but no check maul re-ran proves it",
	"model-only-fail":     "the evaluator says it does not hold, but no check maul re-ran shows it",
	"no-confirmed-check":  "no check maul re-ran covers it",
	"evaluator-saw-failure": "a check passed, but the evaluator saw the outcome as worded fail, " +
		"so the check tests less than the outcome",
	// Slice S2's (SPEC-S2 S2-15): a program check that failed without
	// asserting (O4), and the tool pod's own build of the head that did not
	// finish, which is maul's, never the change's.
	"not-asserted":       "the program failed without asserting anything about the change",
	"tools-build-failed": "couldn't confirm: the build did not finish",
	// S2-20: rule 5 alone withheld the pass (its base runs failed by no
	// assertion, timed out, or are missing).
	"no-base-evidence": "its check passed, but its runs at the base do not tell the change from its absence",
	// Review VC-3: a saved check of it was not re-run this round.
	"not-replayed": "a saved check of it was not re-run at these heads",
	// G-B-LIB-1: a finding is a fact about the commits it was made at.
	"held-finding": "an earlier round at these same commits found it failing, which holds until the commits " +
		"change or an approver accepts it",
}

// Bounds on what an outcome's detail shows.
const (
	verifyCheckLineMaxBytes = 300
	// VerifyAppOutputMaxBytes bounds one outcome's app output.
	VerifyAppOutputMaxBytes = 1024
	verifyAnomalyMaxBytes   = 300
	verifyRouteMaxBytes     = 120
	verifyAnomaliesMax      = 10
)

// VerifyOutcomeResult is one approved outcome and its decided result.
type VerifyOutcomeResult struct {
	// ID and Statement are the approved outcome's.
	ID, Statement string
	// Kind is the outcome's kind, change or keep; unset is change.
	Kind string
	// Result is one of the OutcomeResult constants, Reason Decide's
	// reason for it.
	Result, Reason string
	// Check is maul's one-line description of the check the result rests
	// on; "" for none.
	Check string
	// AppOutput is what the application gave the failing check, shown
	// fenced, sanitised and defanged; "" for none.
	AppOutput string
	// AcceptedAt is the head an approver accepted the outcome at.
	AcceptedAt string
}

// outcomeDetail is a failing outcome's app output as the templates print
// it: maul's lead naming the outcome, then the application's output.
type outcomeDetail struct {
	Lead, AppOutput string
}

// VerifyAnomaly is one look-around anomaly: the evaluator's summary and the
// route it saw it on, its words. FoundIn is the earlier round at the same
// commits that found it, which this round carries (G-B-LIB-1; 0: this
// round's own), and AcceptedBy the approver who accepted it ("" while it
// holds the commits).
type VerifyAnomaly struct {
	Summary, Route string
	FoundIn        int32
	AcceptedBy     string
}

// outcomeKindWords are the outcome kinds as a row shows them; unset is
// change, and one not listed shows as unknown, never as the text it
// carried.
var outcomeKindWords = map[string]string{"": "change", "change": "change", "keep": "keep"}

// outcomeViews adds s's outcomes to v: a row each, saying its kind, its
// result, why and the check it rests on; then the failures' app output and
// the anomalies. An outcome is listed once, in its row (S2 demo run 5).
// short cuts each statement to verifyStatementShortBytes of visible text.
func outcomeViews(v *verifyView, s VerifySection, short bool) {
	for _, o := range s.Outcomes {
		word, ok := outcomeResultWords[o.Result]
		if !ok {
			word = "unknown"
		}
		if o.Result == OutcomeResultAccepted && shown(o.AcceptedAt) != "" {
			word = "accepted by a human at " + shortRevision(o.AcceptedAt) + ", not re-checked"
		}
		kind, ok := outcomeKindWords[o.Kind]
		if !ok {
			kind = "unknown"
		}
		id, verdict, statement := code(shown(o.ID)), outcomeVerdict(o, word), o.Statement
		if short {
			if cut := cutRunes(visibleText(statement), verifyStatementShortBytes); cut != visibleText(statement) {
				statement = cut + "…"
			}
		}
		v.Rows = append(v.Rows, verifyRow{ID: id, Kind: code(kind), Outcome: verdict,
			Statement: SanitizeInline(statement), IDCell: cell(id), KindCell: cell(code(kind)),
			OutcomeCell: cell(verdict)})
		if d, ok := outcomeDetailOf(o, id); ok {
			v.Outcomes = append(v.Outcomes, d)
		}
	}
	for i, a := range s.Anomalies {
		if i == verifyAnomaliesMax {
			break
		}
		if line := anomalyLine(a); line != "" {
			v.Anomalies = append(v.Anomalies, line)
		}
	}
}

// outcomeVerdict is one outcome's result as its row says it: maul's word,
// why it is not confirmed and the check it rests on.
func outcomeVerdict(o VerifyOutcomeResult, word string) string {
	if o.Result != OutcomeResultPassed && o.Result != OutcomeResultAccepted {
		if reason := outcomeReasonWords[o.Reason]; reason != "" {
			word += "; " + reason
		}
	}
	if line := shown(cutRunes(visibleText(o.Check), verifyCheckLineMaxBytes)); line != "" {
		word += "; maul's check: " + code(line)
	}
	return word
}

// outcomeDetailOf is a failing outcome's app output, led by the outcome it
// is of; ok is false when there is none.
func outcomeDetailOf(o VerifyOutcomeResult, id string) (outcomeDetail, bool) {
	if o.Result != OutcomeResultFailed {
		return outcomeDetail{}, false
	}
	raw := strings.TrimSpace(cutRunes(visibleText(o.AppOutput), VerifyAppOutputMaxBytes))
	if raw == "" {
		return outcomeDetail{}, false
	}
	return outcomeDetail{AppOutput: fence(defang(raw)),
		Lead: "App output of " + id + ", as the application wrote it (data, not maul's words):"}, true
}

// cutWords caps valid UTF-8 s at limit bytes at the last whitespace that
// fits in its latter half, its trailing whitespace trimmed, or on a rune
// boundary when none does (a word that long is cut, rather than most of
// what fits dropped); cut reports whether anything was dropped.
func cutWords(s string, limit int) (string, bool) {
	if len(s) <= limit {
		return s, false
	}
	out := cutRunes(s, limit)
	if r, _ := utf8.DecodeRuneInString(s[len(out):]); !unicode.IsSpace(r) {
		if i := strings.LastIndexFunc(out, unicode.IsSpace); i > len(out)/2 {
			out = out[:i]
		}
	}
	return strings.TrimRightFunc(out, unicode.IsSpace), true
}

// anomalyCutMarker ends an anomaly's words where maul cut them (D-B13), so
// the cut text is never taken for the evaluator's own.
const anomalyCutMarker = "[cut by maul]"

// anomalyLine is one anomaly as a comment says it: the evaluator's words,
// sanitised, cut at a word with a marker, and marked as its own.
func anomalyLine(a VerifyAnomaly) string {
	words, cut := cutWords(visibleText(a.Summary), verifyAnomalyMaxBytes)
	summary := SanitizeInline(words)
	if summary == "" {
		return ""
	}
	if cut {
		summary += " " + anomalyCutMarker
	}
	who := "maul's evaluator"
	if a.FoundIn > 0 {
		who = fmt.Sprintf("round %d's evaluator, at these same commits,", a.FoundIn)
	}
	line := "Look-around: " + who + " noticed (its words, not checked by maul): " + summary
	if route := shown(cutRunes(visibleText(a.Route), verifyRouteMaxBytes)); route != "" {
		line += ", on " + code(route)
	}
	line += "."
	switch {
	case a.AcceptedBy != "":
		line += " Accepted at these commits by " + code(oneLine(a.AcceptedBy)) + "."
	case a.FoundIn > 0:
		line += " It holds these commits until they change or an approver accepts it with " +
			code("/maul accept "+command.AcceptAnomalies) + "."
	}
	return line
}
