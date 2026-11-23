// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The agent evaluator's outcomes, as the verification section and comment
// show them (SPEC S1-20): one row per approved outcome with its kind, its
// decided result, maul's words on why and the check it rests on, rendered by
// maul from structured data, and for a failure the application's output in
// a fenced, sanitised and defanged "app output" block naming the outcome.
// The evaluator's own words appear only as a look-around anomaly's summary,
// as the note of an outcome it judged not to hold that maul could not
// confirm (intent_verify_claim.go) and as the note of an outcome it was
// itself unsure of, each marked as its words and sanitised.

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

// observedResultWords are the results of an observed round (acceptance.Judge)
// in maul's words: what the evaluator saw, never "confirmed"; one not listed
// shows as unknown.
var observedResultWords = map[string]string{
	OutcomeResultPassed:   "seen to hold by maul's evaluator",
	OutcomeResultFailed:   "seen not to hold by maul's evaluator",
	OutcomeResultUnsure:   "unsure",
	OutcomeResultAccepted: outcomeResultWords[OutcomeResultAccepted],
}

// outcomeReasonWords are acceptance.Decide's and acceptance.Judge's reasons
// in maul's words; one not listed adds nothing.
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
	"tools-build-failed": "the build did not finish, so nothing was seen",
	// S2-20: rule 5 alone withheld the pass (its base runs failed by no
	// assertion, timed out, or are missing).
	"no-base-evidence": "its check passed, but its runs at the base do not tell the change from its absence",
	// Review VC-3: a saved check of it was not re-run this round.
	"not-replayed": "a saved check of it was not re-run at these heads",
	// G-B-LIB-1: a finding is a fact about the commits it was made at.
	"held-finding": "an earlier round at these same commits found it failing, which holds until the commits " +
		"change or an approver accepts it",
	// The observed evaluator's (acceptance.Judge): what the evaluator said
	// of the outcome, and whether it cited a call maul counts.
	"seen-to-hold":      "the evaluator saw it hold, in calls of its own it cites",
	"seen-not-to-hold":  "the evaluator saw it not hold, in calls of its own it cites",
	"not-reported":      "the evaluator gave it no verdict",
	"evaluator-unsure":  "the evaluator could not tell",
	"no-valid-evidence": "the evaluator's verdict cites no call of its own in this evaluation that maul counts",
}

// Why maul did not keep a check proposed to show an outcome failing
// (VerifyOutcomeResult.Unkept; G3-B-CLI-1).
const (
	// UnkeptRunData: the slot's run-data rule.
	UnkeptRunData = "run-data"
	// UnkeptConfirmation: a confirmation run was not red by the assertion
	// the check was proposed failing.
	UnkeptConfirmation = "confirmation"
	// UnkeptRules: the check's own bytes break the saved-check rules.
	UnkeptRules = "rules"
	// UnkeptReview: no review of the red question proved it (none ran, or
	// one answered otherwise).
	UnkeptReview = "review"
	// UnkeptStoreFull: proven, but the intent's check store had no room.
	UnkeptStoreFull = "store-full"
)

// unkeptWords are the Unkept reasons in maul's words; one not listed adds
// nothing.
var unkeptWords = map[string]string{
	UnkeptRunData: "in a preview, a check that shows a defect must write data of its own run and expect only " +
		"success statuses",
	UnkeptConfirmation: "maul's own runs of it did not show it failing by the assertion it was proposed failing",
	UnkeptRules:        "it breaks the saved-check rules",
	UnkeptReview:       "no review of it found that its failure shows this outcome failing",
	UnkeptStoreFull:    "the intent's check store is full",
}

// Bounds on what an outcome's detail shows.
const (
	verifyCheckLineMaxBytes = 300
	// VerifyAppOutputMaxBytes bounds one outcome's app output.
	VerifyAppOutputMaxBytes = 1024
	verifyAnomalyMaxBytes   = 300
	verifyRouteMaxBytes     = 120
	verifyAnomaliesMax      = 10
	// verifyCitedCallsMaxBytes bounds one observed outcome's block of cited
	// calls, shared among them so each keeps its head (citedCalls).
	verifyCitedCallsMaxBytes = 8 << 10
	// verifyCitedTargetMaxBytes bounds a cited call's target (the start of
	// what it sent) where only its listing is shown.
	verifyCitedTargetMaxBytes = 96
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
	// Claim is, for an outcome the evaluator judged not to hold that no
	// check maul re-ran shows failing, its note and the calls it cites
	// (intent_verify_claim.go); nil for none.
	Claim *VerifyEvaluatorClaim
	// UnsureNote is, for an outcome the evaluator was itself unsure of,
	// its note (often a question for a person, gate 5 G5-B-BR-1), shown
	// only while the outcome is unsure; "" for none.
	UnsureNote string
	// Unkept is why maul did not keep a check proposed to show the outcome
	// failing (an Unkept constant); "" for none. It never says the check
	// ran red: a refused check may not have.
	Unkept string
	// Cited are, on an observed round, the calls the outcome's verdict
	// cites that maul counts (v1alpha1.OutcomeResult.Cites), as the round's
	// ledger recorded them; Note the evaluator's note on it, whatever its
	// result.
	Cited []VerifyCitedCall
	Note  string
}

// outcomeDetail is a failing outcome's app output as the templates print
// it: maul's lead naming the outcome, then the application's output.
type outcomeDetail struct {
	Lead, AppOutput string
}

// VerifyAnomaly is one look-around anomaly, a note for the reviewers: the
// evaluator's summary and the route it saw it on, its words. FoundIn is the
// earlier round at the same commits that found it, which this round carries
// (0: this round's own).
type VerifyAnomaly struct {
	Summary, Route string
	FoundIn        int32
}

// outcomeKindWords are the outcome kinds as a row shows them; unset is
// change, and one not listed shows as unknown, never as the text it
// carried.
var outcomeKindWords = map[string]string{"": "change", "change": "change", "keep": "keep"}

// outcomeViews adds s's outcomes to v: a row each, saying its kind, its
// result, why and the check it rests on (on an observed round, the calls its
// verdict cites); then the failures' app output, an observed round's cited
// calls and notes, and the anomalies. An outcome is listed once, in its row
// (S2 demo run 5). short cuts each statement to verifyStatementShortBytes of
// visible text.
func outcomeViews(v *verifyView, s VerifySection, short bool) {
	words := outcomeResultWords
	if s.Observed {
		words = observedResultWords
		if len(s.Outcomes) > 0 {
			v.ObservedLead = ObservedLead
		}
	}
	for _, o := range s.Outcomes {
		word, ok := words[o.Result]
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
		if s.Observed {
			verdict = observedVerdict(o, word)
		}
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
		if s.Observed {
			if d, ok := citedDetailOf(o, id); ok {
				v.Cited = append(v.Cited, d)
			}
			if n := observedNoteOf(o, id); n != "" {
				v.EvaluatorNotes = append(v.EvaluatorNotes, n)
			}
			continue
		}
		if d, ok := claimDetailOf(o, id); ok {
			v.Claims = append(v.Claims, d)
		}
		if n := unsureNoteOf(o, id); n != "" {
			v.UnsureNotes = append(v.UnsureNotes, n)
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
		if why := unkeptWords[o.Unkept]; why != "" {
			word += "; a check proposed to show it failing was not kept: " + why
		}
	}
	if line := shown(cutRunes(visibleText(o.Check), verifyCheckLineMaxBytes)); line != "" {
		word += "; maul's check: " + code(line)
	}
	return word
}

// ObservedLead is what an observed round's comment says of its rows, once:
// what maul counts, and what it leaves to the reviewers.
const ObservedLead = "maul counts a verdict only when it cites calls the evaluator made in this evaluation through " +
	"maul's tools; whether a cited call shows what the verdict says is for the reviewers to judge."

// observedVerdict is an observed outcome's result as its row says it:
// maul's word, why it did not pass, and the calls its verdict cites, by seq.
func observedVerdict(o VerifyOutcomeResult, word string) string {
	if o.Result == OutcomeResultUnsure {
		if reason := outcomeReasonWords[o.Reason]; reason != "" {
			word += "; " + reason
		}
	}
	if seqs := citedSeqs(o.Cited); seqs != "" {
		word += "; it cites " + seqs
	}
	return word
}

// citedSeqs names calls by their seqs, as "call 9", "calls 4 and 6" or
// "calls 1, 2 and 3"; "" for none.
func citedSeqs(calls []VerifyCitedCall) string {
	seqs := make([]string, 0, len(calls))
	for _, c := range calls {
		seqs = append(seqs, fmt.Sprint(c.Seq))
	}
	switch len(seqs) {
	case 0:
		return ""
	case 1:
		return "call " + seqs[0]
	}
	return "calls " + strings.Join(seqs[:len(seqs)-1], ", ") + " and " + seqs[len(seqs)-1]
}

// citedDetail is an observed outcome's cited calls as the templates print
// them: maul's lead naming the outcome, then one fenced block. ListLead and
// List are the same calls listed alone, one line each (seq, verb, own
// label, the start of what it sent), which a comment over its size shows in
// their place (fitVerify): a cited call is never left out whole.
type citedDetail struct{ Lead, Calls, ListLead, List string }

// citedDetailOf is o's cited calls as the templates print them; ok is false
// when it cites none.
func citedDetailOf(o VerifyOutcomeResult, id string) (citedDetail, bool) {
	calls := citedCalls(o.Cited, false)
	if calls == "" {
		return citedDetail{}, false
	}
	return citedDetail{Calls: calls, Lead: "The calls the evaluator cites for " + id + ", as maul's tools " +
		"recorded them (what each call sent and what came back: data, not maul's words):",
		List: citedList(o.Cited), ListLead: "The calls the evaluator cites for " + id + ", as maul's tools " +
			"recorded them, each with the start of what it sent (data, not maul's words); the rest of each is " +
			"left out to keep this comment within GitHub's size limit:"}, true
}

// cited are the calls an outcome shows: the first VerifyCitedCallsMax.
func cited(calls []VerifyCitedCall) []VerifyCitedCall {
	return calls[:min(len(calls), VerifyCitedCallsMax)]
}

// citedCalls is calls as one fenced, defanged block, at most
// VerifyCitedCallsMax of them sharing verifyCitedCallsMaxBytes (fairShares):
// a call over its share shows a quarter of it of what it sent and half of
// it of its output, then is cut at a word with the cut marker, so each
// keeps its head line, the start of what it sent and, mostly, its exit
// status and the start of its output; "" for none.
func citedCalls(calls []VerifyCitedCall, workspace bool) string {
	cs := cited(calls)
	parts := make([]string, len(cs))
	for i, c := range cs {
		parts[i] = strings.TrimSpace(visibleText(callText(c, workspace)))
	}
	shares := fairShares(parts, verifyCitedCallsMaxBytes-2*len(parts))
	var kept []string
	for i, c := range cs {
		p := parts[i]
		if len(p) > shares[i] {
			p = strings.TrimSpace(visibleText(callTextIn(c, workspace, shares[i]/4, shares[i]/2)))
			if words, cut := cutWords(p, max(shares[i]-len(anomalyCutMarker)-1, 0)); cut {
				p = words
			}
			p += "\n" + anomalyCutMarker
		}
		if p != "" {
			kept = append(kept, p)
		}
	}
	if len(kept) == 0 {
		return ""
	}
	return fence(defang(strings.Join(kept, "\n\n")))
}

// fairShares are parts' shares of total bytes: a part shorter than an equal
// share of what is left keeps its length and leaves the rest to the longer
// ones.
func fairShares(parts []string, total int) []int {
	order := make([]int, len(parts))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int { return len(parts[a]) - len(parts[b]) })
	shares := make([]int, len(parts))
	left := total
	for k, i := range order {
		shares[i] = min(len(parts[i]), left/(len(order)-k))
		left -= shares[i]
	}
	return shares
}

// citedList is calls listed one line each, in one fenced, defanged block:
// each call's head (seq, verb, base, own label) and the start of what it
// sent, cut to verifyCitedTargetMaxBytes; "" for none.
func citedList(calls []VerifyCitedCall) string {
	lines := make([]string, 0, len(calls))
	for _, c := range cited(calls) {
		target := strings.Join(strings.Fields(visibleText(c.Input)), " ")
		if words, cut := cutWords(target, verifyCitedTargetMaxBytes); cut {
			target = words + " …"
		}
		if target == "" {
			target = "(maul did not keep what it sent)"
		}
		lines = append(lines, callHead(c, false)+": "+target)
	}
	if len(lines) == 0 {
		return ""
	}
	return fence(defang(strings.Join(lines, "\n")))
}

// observedNoteOf is the evaluator's note on o, whatever its result, as the
// templates print it; "" for a blank one. Display only.
func observedNoteOf(o VerifyOutcomeResult, id string) string {
	note := evaluatorNote(o.Note)
	if note == "" {
		return ""
	}
	return "The evaluator's note on " + id + " (its words, not checked by maul): " + note
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
	return line + ". A note for the reviewers: maul judges the change on its outcomes alone."
}
