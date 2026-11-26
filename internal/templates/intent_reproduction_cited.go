// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/devthenet-labs/maul/internal/untrusted"
)

// The fix-round reproduction of an observed round (design "evaluator
// observed": the cited-call reproduction): what a verify-fix round started
// by an evaluation whose evaluator saw outcomes not hold is given. Per
// failing outcome, maul renders a bug report from its own records: the
// approved outcome, the calls the verdict cites (what each sent and what
// came back, from the round's verified ledger), at most
// CitedReproductionBeforeMax of the evaluator's own calls before them by
// what each sent alone (the path that led there), and the last page it went
// to before them however far back, never cut silently: what is left out,
// refused or not kept is said by number (W13), at most
// verifyReproLogLines redacted log lines from the round's window, and the
// evaluator's one-line note, labelled as its reading, not checked by maul
// (owner decision 5). Never the evaluator's prompt, its report's body,
// another outcome's note, its anomalies or its transcript: the values carry
// none of them.
//
// Every value but maul's own sentences is the evaluator's or the
// application's, so each is made legible (untrusted.Visible) and shown only
// in a code span or a fence no line of it can close, cut to a fixed size.

// Bounds on what a cited reproduction shows.
const (
	// CitedReproductionBeforeMax bounds the calls before the cited ones a
	// failure shows: the nearest to them are kept, beside the last goto
	// before them, and the rest are said left out.
	CitedReproductionBeforeMax = 8
	// citedReproNoteBytes bounds the evaluator's note (the report holds it
	// to 600 characters).
	citedReproNoteBytes = 2400
	// citedReproSentBytes bounds what one earlier call sent, as shown.
	citedReproSentBytes = 512
	// citedReproOutputBytes bounds one cited call's output, and
	// citedReproCallsBytes a failure's cited calls in all: larger than a
	// comment's (a fix agent is handed no comment's size limit), since the
	// line that shows a failure is often far into a snapshot or a program's
	// output.
	citedReproOutputBytes = 8 << 10
	citedReproCallsBytes  = 48 << 10
)

// Bounds on what a reproduction shows of the values it quotes.
const (
	// verifyReproLogLines bounds the log lines one failure shows, over all
	// its components (DESIGN 4.1: 40 lines).
	verifyReproLogLines = 40
	// verifyReproLogLineBytes bounds one log line.
	verifyReproLogLineBytes = 400
	// verifyReproValueBytes bounds one value shown in a code span (a
	// component, a commit).
	verifyReproValueBytes = 256
	// verifyReproStatementBytes bounds the approved outcome's text.
	verifyReproStatementBytes = 1 << 10
)

// ReproductionHead is one previewed component at its commit.
type ReproductionHead struct{ Component, Revision string }

// ReproductionLog is one component's log lines from the window.
type ReproductionLog struct {
	Component string
	Lines     []string
}

// reproLogView is one component's log excerpt as the template reads it.
type reproLogView struct{ Component, Block string }

// reproCode is untrusted s, escaped and bounded, in a code span.
func reproCode(s string) string {
	if s == "" {
		return "(none)"
	}
	return code(untrusted.Visible(s, verifyReproValueBytes))
}

// CitedReproduction is an observed evaluation's verify-fix round's input:
// the verification that found outcomes failing, at its heads, and its
// failures.
type CitedReproduction struct {
	// Verification names the PreviewVerification, Round and Attempt its
	// numbering.
	Verification   string
	Round, Attempt int32
	Heads          []ReproductionHead
	// Failures are the failing outcomes, in plan order: at least one.
	Failures []CitedFailure
	// Workspace is an agents round's: the calls ran against maul's builds
	// of the commits, never a preview.
	Workspace bool
}

// CitedFailure is one failing outcome's reproduction.
type CitedFailure struct {
	// Outcome is the approved outcome's id, Statement its approved text.
	Outcome, Statement string
	// Note is the evaluator's one-line note on its verdict: its reading,
	// shown labelled; "" for none.
	Note string
	// Cited are the calls the verdict cites that maul counts, in seq order:
	// at least one. Before are the evaluator's own calls before the first of
	// them, in seq order; only what each sent is shown, the last
	// CitedReproductionBeforeMax and the last that navigated (Navigates),
	// the rest said left out by number.
	Cited, Before []VerifyCitedCall
	// Refused are the evaluator's calls before the first cited one that
	// maul refused, and Unkept those whose request maul did not keep, by
	// seq: neither is shown, each is said by number.
	Refused, Unkept []int64
	// Logs are the excerpts from the round's window, per component.
	Logs []ReproductionLog
}

// citedReproView is the template's view.
type citedReproView struct {
	Verification, Round, Attempt string
	Heads                        []string
	Failures                     []citedFailureView
	Workspace                    bool
}

type citedFailureView struct {
	Outcome, Statement, Note, Before, Cited string
	// BeforeNote and CitedNote are maul's sentences on what each list
	// leaves out; "" when it leaves out nothing.
	BeforeNote, CitedNote string
	Logs                  []reproLogView
}

// RenderCitedReproduction renders r as a fix round's feedback; a
// reproduction of no failure, or of a failure citing no call, is refused.
func RenderCitedReproduction(r CitedReproduction) (string, error) {
	if len(r.Failures) == 0 {
		return "", errors.New("render cited reproduction: no failure")
	}
	v := citedReproView{Verification: reproCode(r.Verification), Round: strconv.Itoa(int(r.Round)),
		Attempt: strconv.Itoa(int(r.Attempt)), Workspace: r.Workspace}
	for _, h := range r.Heads {
		v.Heads = append(v.Heads, reproCode(h.Component)+" at "+reproCode(h.Revision))
	}
	for _, f := range r.Failures {
		if len(f.Cited) == 0 {
			return "", fmt.Errorf("render cited reproduction: outcome %q cites no call", f.Outcome)
		}
		v.Failures = append(v.Failures, citedFailure(f))
	}
	out, err := render("intent_reproduction_cited.md.tmpl", v)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(out, "\n"), nil
}

// citedFailure is one failure's view.
func citedFailure(f CitedFailure) citedFailureView {
	fv := citedFailureView{Outcome: reproCode(f.Outcome),
		Statement: chomp(fence(untrusted.Visible(f.Statement, verifyReproStatementBytes)))}
	if note := strings.TrimSpace(f.Note); note != "" {
		fv.Note = chomp(fence(untrusted.Visible(note, citedReproNoteBytes)))
	}
	before, left, nav := pathShown(f.Before)
	lines := make([]string, 0, len(before))
	for _, c := range before {
		lines = append(lines, sentLine(c))
	}
	if len(lines) > 0 {
		fv.Before = chomp(fence(strings.Join(lines, "\n")))
	}
	fv.BeforeNote = pathNote(left, nav, f.Refused, f.Unkept)
	shownCited := f.Cited[:min(len(f.Cited), VerifyCitedCallsMax)]
	cited := make([]string, 0, len(shownCited))
	for _, c := range shownCited {
		cited = append(cited, callTextWithin(c, citedReproOutputBytes))
	}
	text, cut := cutWords(visibleText(strings.Join(cited, "\n\n")), citedReproCallsBytes)
	if cut {
		text += "\n" + anomalyCutMarker
	}
	fv.Cited = chomp(fence(text))
	if rest := f.Cited[len(shownCited):]; len(rest) > 0 {
		fv.CitedNote = fmt.Sprintf("maul shows %d of the %d calls the evaluator cites here: %s %s left out.",
			len(shownCited), len(f.Cited), callNumbers(seqsOf(rest)), plural(int32(len(rest)), "is", "are"))
	}
	fv.Logs = reproLogs(f.Logs)
	return fv
}

// pathShown is which of the calls before the cited ones a reproduction
// shows: the last CitedReproductionBeforeMax, and before them the last
// that navigated when it falls outside them (nav, nil otherwise), since a
// reproduction starts from the page it put the browser on; left are the
// calls not shown.
func pathShown(path []VerifyCitedCall) (shown, left []VerifyCitedCall, nav *VerifyCitedCall) {
	start := max(0, len(path)-CitedReproductionBeforeMax)
	window := path[start:]
	last := -1
	for i, c := range path {
		if c.Navigates {
			last = i
		}
	}
	if last < 0 || last >= start {
		return window, path[:start], nil
	}
	shown = append(append(shown, path[last]), window...)
	left = append(append(left, path[:last]...), path[last+1:start]...)
	return shown, left, &path[last]
}

// sentLine is one call before the cited ones as a reproduction shows it:
// how a comment names it, the evaluator's own labelled, and what it sent,
// on one line, marked where it was cut to fit.
func sentLine(c VerifyCitedCall) string {
	line := callHead(c, false)
	sent := strings.TrimSpace(c.Input)
	if sent == "" {
		return line
	}
	shown := untrusted.Visible(sent, citedReproSentBytes)
	line += ": " + strings.Join(strings.Fields(shown), " ")
	if len(sent) > citedReproSentBytes || len(untrusted.Escape(sent)) > citedReproSentBytes {
		line += " " + anomalyCutMarker
	}
	return line
}

// pathNote is maul's sentences on what the calls before the cited ones
// leave out: those left out for length (with the page kept from before
// them, nav), those maul refused and those whose request it did not keep;
// "" when nothing is.
func pathNote(left []VerifyCitedCall, nav *VerifyCitedCall, refused, unkept []int64) string {
	var out []string
	if len(left) > 0 {
		s := fmt.Sprintf("maul left out %d of the evaluator's earlier calls here to keep this short: %s.", len(left),
			callNumbers(seqsOf(left)))
		if nav != nil {
			s += fmt.Sprintf(" Call %d is kept: it is the last page the evaluator went to before the cited "+
				"calls.", nav.Seq)
		}
		out = append(out, s)
	}
	if len(refused) > 0 {
		out = append(out, fmt.Sprintf("maul refused %d of the evaluator's calls before the ones it cites, not "+
			"shown: %s.", len(refused), callNumbers(refused)))
	}
	if len(unkept) > 0 {
		out = append(out, fmt.Sprintf("maul did not keep what %d of the evaluator's calls before the ones it "+
			"cites sent, not shown: %s.", len(unkept), callNumbers(unkept)))
	}
	return strings.Join(out, " ")
}

// seqsOf are calls' seqs.
func seqsOf(calls []VerifyCitedCall) []int64 {
	out := make([]int64, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.Seq)
	}
	return out
}

// callNumbers names calls by seq in a sentence, ascending as given: "call
// 4", "calls 4 and 5", a run of three or more as "1 to 3".
func callNumbers(seqs []int64) string {
	var items []string
	for i := 0; i < len(seqs); {
		j := i
		for j+1 < len(seqs) && seqs[j+1] == seqs[j]+1 {
			j++
		}
		if j-i >= 2 {
			items = append(items, fmt.Sprintf("%d to %d", seqs[i], seqs[j]))
		} else {
			for k := i; k <= j; k++ {
				items = append(items, strconv.FormatInt(seqs[k], 10))
			}
		}
		i = j + 1
	}
	word := "calls"
	if len(seqs) == 1 {
		word = "call"
	}
	switch len(items) {
	case 0:
		return ""
	case 1:
		return word + " " + items[0]
	}
	return word + " " + strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

// reproLogs are the log excerpts a failure shows, at most
// verifyReproLogLines lines over all its components, each line bounded.
func reproLogs(logs []ReproductionLog) []reproLogView {
	var out []reproLogView
	budget := verifyReproLogLines
	for _, l := range logs {
		if budget == 0 {
			break
		}
		lines := l.Lines[:min(len(l.Lines), budget)]
		budget -= len(lines)
		shown := make([]string, 0, len(lines))
		for _, line := range lines {
			shown = append(shown, untrusted.Visible(line, verifyReproLogLineBytes))
		}
		if len(shown) > 0 {
			out = append(out, reproLogView{Component: reproCode(l.Component),
				Block: chomp(fence(strings.Join(shown, "\n")))})
		}
	}
	return out
}
