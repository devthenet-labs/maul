// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/templates"
	"github.com/devthenet-labs/maul/internal/toolledger"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

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
	// own are the index's own-call labels (toolledger.OwnCalls), nil until
	// labelled: ownAt labels them then.
	own []bool
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
	rc.own = rc.labels()
	return rc
}

// labels are the index's own-call labels.
func (rc roundCalls) labels() []bool {
	return toolledger.OwnCalls(rc.index, rc.args, rc.agents, rc.binary)
}

// ownAt is the own-call label of the index's entry at seq (an indexed seq).
func (rc roundCalls) ownAt(seq int32) bool {
	own := rc.own
	if len(own) != len(rc.index) {
		own = rc.labels()
	}
	return own[seq-1]
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
	// The label rests on the verified index, and on the lines only where a
	// verb needs one (toolledger.OwnCalls), the calls before seq included.
	c.Own = rc.ownAt(seq)
	e, ok := rc.line(ix)
	if !ok {
		return c, toolwire.LedgerEntry{}, false
	}
	c.Input = string(e.Args)
	c.Navigates = navigates(e)
	return c, e, true
}

// navigates reports e a browser goto: the call that put the page where the
// calls after it act.
func navigates(e toolwire.LedgerEntry) bool {
	var req toolwire.BrowserRequest
	return e.Verb == toolwire.VerbBrowser && json.Unmarshal(e.Args, &req) == nil && req.Verb == "goto"
}

// line is the kept ledger line of index entry ix, ok false when none kept
// is the index's call (its output digest and verb).
func (rc roundCalls) line(ix v1alpha1.LedgerIndexEntry) (toolwire.LedgerEntry, bool) {
	e, ok := rc.lines[int64(ix.Seq)]
	if !ok || e.OutputSHA256 != ix.ResultSHA256 || e.Verb != toolwire.Verb(ix.Verb) {
		return toolwire.LedgerEntry{}, false
	}
	return e, true
}

// args is the request entry seq's kept line recorded, nil when none kept is
// the index's call.
func (rc roundCalls) args(seq int32) json.RawMessage {
	ix, ok := rc.indexed(seq)
	if !ok {
		return nil
	}
	e, ok := rc.line(ix)
	if !ok {
		return nil
	}
	return e.Args
}

// sentBefore are the evaluator's own unrefused calls at the head before
// seq, in seq order, each with what it sent alone: the path that led to the
// first cited call. Its own reads (files, logs, events) are part of that
// path; a call at an agents round's base is exploration of other code, and
// never is. refused are the path's calls maul refused, unkept those whose
// request maul did not keep, by seq: never shown, so the reproduction says
// them by number.
func (rc roundCalls) sentBefore(seq int32) (out []templates.VerifyCitedCall, refused, unkept []int64) {
	for _, ix := range rc.index {
		if ix.Seq >= seq || ix.Kind != v1alpha1.LedgerKindAgent || ix.Side == v1alpha1.TreeBase {
			continue
		}
		if ix.Refused {
			refused = append(refused, int64(ix.Seq))
			continue
		}
		c, _, ok := rc.sent(ix.Seq)
		if !ok {
			unkept = append(unkept, int64(ix.Seq))
			continue
		}
		out = append(out, c)
	}
	return out, refused, unkept
}
