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
	"github.com/devthenet-labs/maul/internal/checks"
	"github.com/devthenet-labs/maul/internal/templates"
	"github.com/devthenet-labs/maul/internal/toolwire"
	"github.com/devthenet-labs/maul/internal/untrusted"
)

// The fix-round reproduction (DESIGN 2.2, 2.9 use 2, 4.4, 7.7; SPEC S1-21).
// An evaluation's verify-fix round is handed, per failure the judgement
// asks it to fix, a bug report maul renders from its own records: the
// approved outcome, the failing check's steps (templates, in maul's words)
// with what each expects, what the check runner observed on the failing
// run (that run's tools ledger entry), and at most 40 log lines from the
// run's window. Nothing else of the round reaches it: never the evaluate
// prompt, the evaluator's notes, report or transcript, another check, a
// check's digest or the check store (TestBuildHandoffNeverReferencesEvaluator).
//
// The log lines come from the ledger too: preview-controller's capture
// reaches the release namespace only as the outputs of the evaluator's
// `maul-tool logs` calls, each the component's redacted log since the
// verification started. A failing run's window is what the component
// logged between the evaluator's last read before the run and its first
// read after it; without a read after the run there is no excerpt.

// maxReproLogLines bounds a component's excerpt (the template bounds a
// failure's to the same over all its components).
const maxReproLogLines = 40

// errorLine marks the line an excerpt starts at: the first error, panic or
// exception in the window.
var errorLine = regexp.MustCompile(`(?i)\b(panic|fatal|error|exception|traceback)\b`)

// reproduction renders the failures fix asks a verify-fix round for, of pv
// against the approved outcomes, with cs the checks the round was judged
// on.
func (p *pass) reproduction(ctx context.Context, pv *v1alpha1.PreviewVerification, acc *v1alpha1.IntentAcceptance,
	fix *acceptance.FixRequest, cs []v1alpha1.SavedCheck) (string, error) {
	r := templates.VerifyReproduction{Verification: pv.Name, Round: pv.Spec.Round, Attempt: pv.Spec.Attempt}
	var components []string
	for _, h := range pv.Spec.Heads {
		r.Heads = append(r.Heads, templates.ReproductionHead{Component: h.Component, Revision: h.Revision})
		if !slices.Contains(components, h.Component) {
			components = append(components, h.Component)
		}
	}
	entries := ledgerEntries(p.ledgerOf(ctx, pv))
	for _, f := range fix.Failures {
		i := slices.IndexFunc(acc.Outcomes, func(o v1alpha1.AcceptanceOutcome) bool { return o.ID == f.Outcome })
		if i < 0 {
			continue
		}
		rf := templates.ReproductionFailure{Outcome: f.Outcome, Statement: acc.Outcomes[i].Outcome}
		logged := components
		if k := slices.IndexFunc(cs, func(c v1alpha1.SavedCheck) bool { return c.Digest == f.CheckDigest }); k >= 0 {
			if parsed, err := checks.Parse([]byte(cs[k].Body)); err == nil {
				rf.Check = &parsed
				if parsed.HTTP != nil {
					logged = []string{parsed.HTTP.Target}
				}
			}
		}
		seq := failingSeq(pv, f.CheckDigest)
		if run := checkRunAt(entries, seq, f.CheckDigest); run != nil {
			rf.Observed = observations(run)
		}
		for _, c := range logged {
			if lines := logWindow(entries, int64(seq), c); len(lines) > 0 {
				rf.Logs = append(rf.Logs, templates.ReproductionLog{Component: c, Lines: lines})
			}
		}
		r.Failures = append(r.Failures, rf)
	}
	out, err := templates.RenderVerifyReproduction(r)
	if err != nil {
		return "", fmt.Errorf("render the reproduction of verification %s: %w", pv.Name, err)
	}
	return untrusted.Cap(out, maxVerifyEvidenceBytes), nil
}

// failingSeq is the ledger seq of check digest's first failing run in pv
// (a replay or a confirmation), 0 for none.
func failingSeq(pv *v1alpha1.PreviewVerification, digest string) int32 {
	var seq int32
	for _, r := range slices.Concat(pv.Status.Replay, pv.Status.Confirmations) {
		if r.CheckDigest == digest && r.Result == v1alpha1.CheckRunFail && (seq == 0 || r.Seq < seq) {
			seq = r.Seq
		}
	}
	return seq
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

// checkRunAt is the check run of digest recorded at seq, nil for none.
func checkRunAt(entries []toolwire.LedgerEntry, seq int32, digest string) *toolwire.CheckRunRecord {
	if seq == 0 {
		return nil
	}
	for _, e := range entries {
		if e.Seq == int64(seq) && e.CheckRun != nil && e.CheckRun.Digest == digest {
			return e.CheckRun
		}
	}
	return nil
}

// observations are a check run's steps that did not pass, attempt by
// attempt; an attempt that failed with no step recorded is one observation
// of its own.
func observations(r *toolwire.CheckRunRecord) []templates.ReproductionObservation {
	var out []templates.ReproductionObservation
	for i, a := range r.Attempts {
		n := len(out)
		for _, s := range a.Steps {
			if s.Result != toolwire.ResultPass {
				out = append(out, templates.ReproductionObservation{Attempt: i + 1, Step: s.Index + 1,
					Result: string(s.Result), Detail: s.Detail})
			}
		}
		if len(out) == n && a.Result != toolwire.ResultPass {
			out = append(out, templates.ReproductionObservation{Attempt: i + 1, Result: string(a.Result)})
		}
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
