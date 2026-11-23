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
	"github.com/devthenet-labs/maul/internal/templates"
	"github.com/devthenet-labs/maul/internal/toolledger"
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
		if parsed, err := checks.ParseRecorded(req.Spec, "sha256:"+e.CheckRun.Digest); err == nil {
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

// roundCalls are a round's calls as maul shows them ("evaluator observed"):
// what counts comes from pv's verified index in status (the judgement's
// facts), what each call sent and what came back from pv's full ledger,
// <pv>-ledger, read once a pass. A cited call is always shown by its seq,
// verb and exit status; one whose ledger line is gone, is another call's
// (its output digest is not the index's), or whose output was dropped to fit
// is shown as an output maul did not keep: it still counts.
type roundCalls struct {
	index  []v1alpha1.LedgerIndexEntry
	lines  map[int64]toolwire.LedgerEntry
	agents bool
	binary string
}

// roundCalls are pv's calls as maul shows them.
func (p *pass) roundCalls(ctx context.Context, pv *v1alpha1.PreviewVerification) roundCalls {
	rc := roundCalls{lines: map[int64]toolwire.LedgerEntry{}, agents: pv.Spec.IsAgents()}
	if ws := pv.Spec.Workspace; ws != nil && ws.Surface.CLI != nil {
		rc.binary = ws.Surface.CLI.Binary
	}
	if pv.Status.Ledger != nil {
		rc.index = pv.Status.Ledger.Entries
	}
	for _, e := range ledgerEntries(p.ledgerOf(ctx, pv)) {
		if _, dup := rc.lines[e.Seq]; !dup {
			rc.lines[e.Seq] = e
		}
	}
	return rc
}

// indexed is the index entry numbered seq, ok false for none.
func (rc roundCalls) indexed(seq int32) (v1alpha1.LedgerIndexEntry, bool) {
	if seq < 1 || int(seq) > len(rc.index) || rc.index[seq-1].Seq != seq {
		return v1alpha1.LedgerIndexEntry{}, false
	}
	return rc.index[seq-1], true
}

// call is the call numbered seq, with what it sent and what came back.
func (rc roundCalls) call(seq int32) templates.VerifyCitedCall {
	c, e, ok := rc.sent(seq)
	if !ok {
		c.OutputNotKept = true
		return c
	}
	sum := sha256.Sum256([]byte(e.Output))
	if hex.EncodeToString(sum[:]) != e.OutputSHA256 {
		c.OutputNotKept = true
		return c
	}
	c.Output = e.Output
	return c
}

// sent is the call numbered seq with what it sent alone, and its ledger
// line; ok is false when the line is not the index's call.
func (rc roundCalls) sent(seq int32) (templates.VerifyCitedCall, toolwire.LedgerEntry, bool) {
	c := templates.VerifyCitedCall{Seq: int64(seq)}
	ix, indexed := rc.indexed(seq)
	if !indexed {
		return c, toolwire.LedgerEntry{}, false
	}
	c.Verb, c.Base, c.Exit = string(ix.Verb), ix.Side == v1alpha1.TreeBase, int(ix.Exit)
	e, ok := rc.lines[int64(seq)]
	if !ok || e.OutputSHA256 != ix.ResultSHA256 || string(e.Verb) != c.Verb {
		// The label rests on the verified index, and on a line only where
		// the verb needs one (toolledger.OwnCall).
		c.Own = toolledger.OwnCall(ix.Verb, nil, rc.agents, rc.binary)
		return c, toolwire.LedgerEntry{}, false
	}
	c.Input, c.Own = string(e.Args), toolledger.OwnCall(ix.Verb, e.Args, rc.agents, rc.binary)
	return c, e, true
}

// sentBefore are the evaluator's own unrefused calls at the head before
// seq, in seq order, each with what it sent alone: the path that led to the
// first cited call. Its own reads (files, logs, events) are part of that
// path; a call at an agents round's base is exploration of other code, and
// never is.
func (rc roundCalls) sentBefore(seq int32) []templates.VerifyCitedCall {
	var out []templates.VerifyCitedCall
	for _, ix := range rc.index {
		if ix.Seq >= seq || ix.Kind != v1alpha1.LedgerKindAgent || ix.Refused || ix.Side == v1alpha1.TreeBase {
			continue
		}
		if c, _, ok := rc.sent(ix.Seq); ok {
			out = append(out, c)
		}
	}
	return out
}
