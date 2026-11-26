// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/templates"
	"github.com/devthenet-labs/maul/internal/toolwire"
	"github.com/devthenet-labs/maul/internal/untrusted"
)

// The fix-round reproduction ("evaluator observed": the cited-call
// reproduction; DESIGN 2.9 use 2, 4.4). An evaluation's verify-fix round is
// handed, per failure the judgement asks it to fix, a bug report maul
// renders from its own records (templates.RenderCitedReproduction): the
// approved outcome; the calls the evaluator's verdict cites that maul
// counts (acceptance.ValidCite), each with what it sent and what came back,
// from the round's verified ledger; the evaluator's own calls before them,
// what each sent alone (the path that led there); at most 40 log lines
// from the window of the first cited call; and the evaluator's one-line
// note on the outcome, labelled as its reading, not checked by maul (owner
// decision 5). Nothing else of the round reaches it: never the evaluate
// prompt, the report's body, another outcome's note, the anomalies, the
// transcript or a run's name (TestBuildHandoffCarriesOnlyCitedEvidence).
//
// The log lines come from the ledger too: preview-controller's capture
// reaches the release namespace only as the outputs of the evaluator's
// `maul-tool logs` calls, each the component's redacted log since the
// verification started. A failing call's window is what the component
// logged between the evaluator's last read before the call and its first
// read after it; without a read after the call there is no excerpt.

// maxReproLogLines bounds a component's excerpt (the template bounds a
// failure's to the same over all its components).
const maxReproLogLines = 40

// errorLine marks the line an excerpt starts at: the first error, panic or
// exception in the window.
var errorLine = regexp.MustCompile(`(?i)\b(panic|fatal|error|exception|traceback)\b`)

// reproduction renders the failures fix asks a verify-fix round for, of pv
// against the approved outcomes, as j judged them: each failure's valid
// cites (Judge's), the calls before the first of them, its log window and
// the evaluator's note on it.
func (p *pass) reproduction(ctx context.Context, pv *v1alpha1.PreviewVerification, acc *v1alpha1.IntentAcceptance,
	fix *acceptance.FixRequest, j roundJudgement) (string, error) {
	r := templates.CitedReproduction{Verification: pv.Name, Round: pv.Spec.Round, Attempt: pv.Spec.Attempt,
		Workspace: pv.Spec.IsAgents()}
	var components []string
	for _, h := range pv.Spec.Heads {
		r.Heads = append(r.Heads, templates.ReproductionHead{Component: h.Component, Revision: h.Revision})
		if !slices.Contains(components, h.Component) {
			components = append(components, h.Component)
		}
	}
	calls := p.roundCalls(ctx, pv)
	entries := ledgerEntries(p.ledgerOf(ctx, pv))
	for _, f := range fix.Failures {
		i := slices.IndexFunc(acc.Outcomes, func(o v1alpha1.AcceptanceOutcome) bool { return o.ID == f.Outcome })
		d, ok := j.decision(f.Outcome)
		if i < 0 || !ok || len(d.Cites) == 0 {
			continue
		}
		cf := templates.CitedFailure{Outcome: f.Outcome, Statement: acc.Outcomes[i].Outcome, Note: j.note(f.Outcome)}
		for _, seq := range d.Cites {
			cf.Cited = append(cf.Cited, calls.call(seq))
		}
		cf.Before, cf.Refused, cf.Unkept = calls.sentBefore(d.Cites[0])
		for _, c := range components {
			if lines := logWindow(entries, int64(d.Cites[0]), c); len(lines) > 0 {
				cf.Logs = append(cf.Logs, templates.ReproductionLog{Component: c, Lines: lines})
			}
		}
		r.Failures = append(r.Failures, cf)
	}
	out, err := templates.RenderCitedReproduction(r)
	if err != nil {
		return "", fmt.Errorf("render the reproduction of verification %s: %w", pv.Name, err)
	}
	return untrusted.Cap(out, maxVerifyEvidenceBytes), nil
}

// ledgerEntries decodes a full ledger's lines; a line that does not decode
// is left out.
func ledgerEntries(lines []string) []toolwire.LedgerEntry {
	out := make([]toolwire.LedgerEntry, 0, len(lines))
	for _, line := range lines {
		var e toolwire.LedgerEntry
		if strings.TrimSpace(line) == "" || json.Unmarshal([]byte(line), &e) != nil {
			continue
		}
		out = append(out, e)
	}
	return out
}

// logWindow is component's excerpt from the window of the run at seq: what
// the component logged between the last intact `maul-tool logs` read of it
// before seq and the first after; nil without a read after. The excerpt
// starts at the window's first error or panic line, else is its newest
// lines, at most maxReproLogLines.
func logWindow(entries []toolwire.LedgerEntry, seq int64, component string) []string {
	var before, after *toolwire.LedgerEntry
	for i := range entries {
		e := &entries[i]
		if !logsOf(e, component) {
			continue
		}
		switch {
		case e.Seq < seq && (before == nil || e.Seq > before.Seq):
			before = e
		case e.Seq > seq && (after == nil || e.Seq < after.Seq):
			after = e
		}
	}
	if after == nil {
		return nil
	}
	window := after.Output
	if before != nil {
		window = newSince(before.Output, after.Output)
	}
	lines := strings.Split(strings.TrimRight(window, "\n"), "\n")
	if len(lines) == 1 && strings.TrimSpace(lines[0]) == "" {
		return nil
	}
	if i := slices.IndexFunc(lines, errorLine.MatchString); i >= 0 {
		lines = lines[i:]
		return lines[:min(len(lines), maxReproLogLines)]
	}
	return lines[max(0, len(lines)-maxReproLogLines):]
}

// logsOf reports e an intact read of component's log: a logs call that
// ran, its output whole (not dropped to fit the ConfigMap).
func logsOf(e *toolwire.LedgerEntry, component string) bool {
	if e.Verb != toolwire.VerbLogs || e.Refused != "" {
		return false
	}
	var req toolwire.LogsRequest
	if json.Unmarshal(e.Args, &req) != nil || req.Component != component {
		return false
	}
	sum := sha256.Sum256([]byte(e.Output))
	return hex.EncodeToString(sum[:]) == e.OutputSHA256
}

// newSince is what later, a capture read after earlier, adds to it: the
// capture grows from the verification's start, so earlier is later's
// prefix until the capture's tail bound drops its oldest lines; then what
// follows earlier's last lines in later, or all of later when they are gone.
func newSince(earlier, later string) string {
	if strings.HasPrefix(later, earlier) {
		return later[len(earlier):]
	}
	lines := strings.Split(strings.TrimRight(earlier, "\n"), "\n")
	last := strings.Join(lines[max(0, len(lines)-3):], "\n") + "\n"
	if i := strings.Index(later, last); i >= 0 {
		return later[i+len(last):]
	}
	return later
}
