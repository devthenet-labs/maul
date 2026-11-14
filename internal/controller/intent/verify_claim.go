// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/checks"
	"github.com/devthenet-labs/maul/internal/report"
	"github.com/devthenet-labs/maul/internal/templates"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// The evaluator's unconfirmed claim (gate 3 G1, G10: the Shown gap). An
// outcome the evaluator judged not to hold that no check maul re-ran shows
// failing (acceptance.ReasonModelOnlyFail, the doubted outcomes the hand-off
// counts; a red check the reviewer refused lands here) says only that, so
// a person could not see what the evaluator saw. The comments now show its
// note and the calls it cites, read at render time from its report and the
// round's own ledger: what each call sent and what maul's tools recorded
// it returning. Display only: nothing here reaches Decide, the Intent's
// status or any agent's input (a fix round is handed maul's reproduction,
// never this; maul's own comments are not feedback).

// claimVerbs are the calls a claim shows: those that send something to
// the change and observe what it does. A source read, a log or event
// read, a session or a confirmation reproduces nothing.
var claimVerbs = []toolwire.Verb{toolwire.VerbCheckRun, toolwire.VerbExec, toolwire.VerbRun, toolwire.VerbHTTP,
	toolwire.VerbBrowser}

// doubted reports r an outcome the evaluator alone judged not to hold at
// v's heads, as the hand-off counts it (doubtedOutcomes).
func doubted(acc *v1alpha1.IntentAcceptance, v *v1alpha1.IntentVerification, r v1alpha1.OutcomeResult) bool {
	return r.Reason == acceptance.ReasonModelOnlyFail && effectiveResult(acc, v, r) == v1alpha1.OutcomeUnsure
}

// evaluatorClaim is the evaluator's claim on outcome id in pv's evaluate
// run, with the calls it cites that pv's ledger holds; nil when the run's
// report does not judge id to fail (a replay round has none).
func (p *pass) evaluatorClaim(ctx context.Context, pv *v1alpha1.PreviewVerification,
	id string) *templates.VerifyEvaluatorClaim {
	ev := evaluationClaims(p.evaluateRun(pv))
	if ev == nil {
		return nil
	}
	i := slices.IndexFunc(ev.Verdicts, func(v report.OutcomeVerdict) bool { return v.ID == id })
	if i < 0 || ev.Verdicts[i].Verdict != report.VerdictFail {
		return nil
	}
	v := ev.Verdicts[i]
	c := &templates.VerifyEvaluatorClaim{Note: v.Note, Workspace: pv.Spec.IsAgents()}
	entries := ledgerEntries(p.ledgerOf(ctx, pv))
	// Where it saw the outcome fail first, then the rest of its evidence.
	for _, seq := range slices.Concat(v.Contrary, v.Ledger) {
		if len(c.Calls) == templates.VerifyClaimCallsMax {
			break
		}
		if slices.ContainsFunc(c.Calls, func(x templates.VerifyCitedCall) bool { return x.Seq == int64(seq) }) {
			continue
		}
		k := slices.IndexFunc(entries, func(e toolwire.LedgerEntry) bool { return e.Seq == int64(seq) })
		if k < 0 {
			continue
		}
		if call, ok := citedCall(pv, entries, entries[k]); ok {
			c.Calls = append(c.Calls, call)
		}
	}
	return c
}

// citedCall is entry e of pv's ledger as a claim shows it; ok is false for
// a call that reproduces nothing or was refused, and for a relay entry
// other than a check run: maul's own call, not one the evaluator sent. A
// relay check run (a replay or confirmation the evaluator cites from its
// check results) is maul's measurement, shown as such by its mode.
func citedCall(pv *v1alpha1.PreviewVerification, entries []toolwire.LedgerEntry,
	e toolwire.LedgerEntry) (templates.VerifyCitedCall, bool) {
	if e.Refused != "" || !slices.Contains(claimVerbs, e.Verb) ||
		(e.Kind != toolwire.KindAgent && e.Verb != toolwire.VerbCheckRun) {
		return templates.VerifyCitedCall{}, false
	}
	call := templates.VerifyCitedCall{Seq: e.Seq, Verb: string(e.Verb), Base: e.Side == toolwire.SideBase,
		Exit: e.Exit}
	if e.Verb != toolwire.VerbCheckRun {
		call.Input = string(e.Args)
		sum := sha256.Sum256([]byte(e.Output))
		call.Output, call.OutputNotWhole = e.Output, hex.EncodeToString(sum[:]) != e.OutputSHA256
		if call.OutputNotWhole {
			call.Output = ""
		}
		return call, true
	}
	if e.CheckRun == nil {
		return templates.VerifyCitedCall{}, false
	}
	var req toolwire.CheckRunRequest
	if json.Unmarshal(e.Args, &req) == nil {
		if parsed, err := checks.Parse(req.Spec); err == nil {
			call.Check = &parsed
		}
	}
	if call.Check == nil {
		// A body maul cannot word is shown as sent.
		call.Input = string(e.Args)
	}
	call.Mode, call.RunSeq, call.Result = string(e.CheckRun.Mode), e.Seq, string(e.CheckRun.Result)
	run := e.CheckRun
	// maul's own run of the same check at the head, where one failed (a
	// confirmation or a replay), is shown in place of the evaluator's dry
	// run there; a dry run at the base keeps its own.
	if seq := failingSeq(pv, "sha256:"+e.CheckRun.Digest); seq != 0 && !call.Base {
		if own := checkRunAt(entries, seq, "sha256:"+e.CheckRun.Digest); own != nil {
			run, call.RunSeq, call.Result = own, int64(seq), string(own.Result)
		}
	}
	call.Observed = observations(run)
	return call, true
}
