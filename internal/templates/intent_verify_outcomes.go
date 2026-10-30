// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import "strings"

// The agent evaluator's outcomes, as the verification section and comment
// show them (SPEC S1-20): one row per approved outcome with its decided
// result, then per outcome maul's sentence on why and the check it rests
// on, rendered by maul from structured data, and for a failure the
// application's output in a fenced, sanitised and defanged "app output"
// block. The evaluator's own words appear only as a look-around anomaly's
// summary, marked as its words and sanitised.

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

// outcomeDetail is one outcome's detail as the templates print it: maul's
// sentence on its result and its check, and the application's output.
type outcomeDetail struct {
	Sentence, AppOutput string
}

// VerifyAnomaly is one look-around anomaly: the evaluator's summary and the
// route it saw it on, its words.
type VerifyAnomaly struct {
	Summary, Route string
}

// outcomeViews adds s's outcomes to v: a row each, then their details and
// the anomalies.
func outcomeViews(v *verifyView, s VerifySection) {
	for _, o := range s.Outcomes {
		word, ok := outcomeResultWords[o.Result]
		if !ok {
			word = "unknown"
		}
		if o.Result == OutcomeResultAccepted && shown(o.AcceptedAt) != "" {
			word = "accepted by a human at " + shortRevision(o.AcceptedAt) + ", not re-checked"
		}
		id := code(shown(o.ID))
		v.Rows = append(v.Rows, verifyRow{ID: id, Kind: code("outcome"), Outcome: word,
			Statement: SanitizeInline(o.Statement), IDCell: cell(id), KindCell: cell(code("outcome"))})
		if d, ok := outcomeDetailOf(o, id, word); ok {
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

// outcomeDetailOf is one outcome's detail: why it is not confirmed, the
// check it rests on and the application's output; ok is false when there
// is nothing to add to its row.
func outcomeDetailOf(o VerifyOutcomeResult, id, word string) (outcomeDetail, bool) {
	head := id + ": " + word
	reason := outcomeReasonWords[o.Reason]
	if o.Result == OutcomeResultPassed || o.Result == OutcomeResultAccepted {
		reason = ""
	}
	line := shown(cutRunes(visibleText(o.Check), verifyCheckLineMaxBytes))
	var out outcomeDetail
	if o.Result == OutcomeResultFailed {
		if raw := strings.TrimSpace(cutRunes(visibleText(o.AppOutput), VerifyAppOutputMaxBytes)); raw != "" {
			out.AppOutput = fence(defang(raw))
		}
	}
	if reason == "" && line == "" && out.AppOutput == "" {
		return outcomeDetail{}, false
	}
	if reason != "" {
		head += "; " + reason
	}
	out.Sentence = head + "."
	if line != "" {
		out.Sentence += " maul's check: " + code(line) + "."
	}
	return out, true
}

// anomalyLine is one anomaly as a comment says it: the evaluator's words,
// sanitised, cut, and marked as its own.
func anomalyLine(a VerifyAnomaly) string {
	summary := SanitizeInline(cutRunes(visibleText(a.Summary), verifyAnomalyMaxBytes))
	if summary == "" {
		return ""
	}
	line := "Look-around: maul's evaluator noticed (its words, not checked by maul): " + summary
	if route := shown(cutRunes(visibleText(a.Route), verifyRouteMaxBytes)); route != "" {
		line += ", on " + code(route)
	}
	return line + "."
}
