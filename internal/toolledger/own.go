// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package toolledger

import (
	"encoding/json"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// OwnCalls labels each entry of a round's verified index that ran a page
// script or program the evaluator wrote, which every surface labels as its
// own ("evaluator observed", owner decision 1: it counts, and the reviewers
// are told whose it is). It returns one label per index entry, in index
// order. args(seq) is entry seq's request as its kept ledger line recorded
// it (as the evaluator sent it, before any placeholder was filled), nil
// when maul kept no line that is the index's call; agents is an agents (cli
// or library) round, binary its built cli binary ("" for a library).
//
// Every label fails closed: a call is unlabelled only on proof.
//
//   - A Go program (run) and a slot exec (the toolbox runs only what it is
//     sent) are the evaluator's own from the verb alone.
//   - An agents exec runs any argv with any files the evaluator supplies
//     (workspaceExec), so it is its own unless its line shows exactly the
//     built binary (toolwire.BinPlaceholder or binary, as argv[0]) with no
//     files, and no earlier call at the same side ran anything of the
//     evaluator's there: /work is writable, so an earlier exec or run of its
//     own could have replaced the binary or planted what it reads. An
//     earlier call that maul refused ran nothing; one with no readable line
//     is taken as its own.
//   - A browser call is a page script unless its line shows a verb other
//     than eval: with no line, or its args dropped to fit, nothing shows it
//     was not one.
func OwnCalls(index []v1alpha1.LedgerIndexEntry, args func(seq int32) json.RawMessage, agents bool,
	binary string) []bool {
	out := make([]bool, len(index))
	tainted := map[v1alpha1.TreeSide]bool{}
	for i, ix := range index {
		a := keptArgs(args(ix.Seq))
		switch ix.Verb {
		case v1alpha1.LedgerVerbRun:
			out[i] = true
		case v1alpha1.LedgerVerbExec:
			out[i] = !agents || tainted[ix.Side] || !builtBinaryOnly(a, binary)
		case v1alpha1.LedgerVerbBrowser:
			out[i] = !browserNotEval(a)
		}
		if agents && out[i] && ix.Kind == v1alpha1.LedgerKindAgent && !ix.Refused &&
			(ix.Verb == v1alpha1.LedgerVerbExec || ix.Verb == v1alpha1.LedgerVerbRun) {
			tainted[ix.Side] = true
		}
	}
	return out
}

// keptArgs is args as maul kept them, nil when they were dropped to fit.
func keptArgs(args json.RawMessage) json.RawMessage {
	if string(args) == droppedArgs {
		return nil
	}
	return args
}

// builtBinaryOnly reports an exec request, as recorded, of exactly a cli's
// built binary with no files of the evaluator's.
func builtBinaryOnly(args json.RawMessage, binary string) bool {
	var req toolwire.ExecRequest
	if binary == "" || args == nil || json.Unmarshal(args, &req) != nil || len(req.Argv) == 0 ||
		len(req.Files) > 0 {
		return false
	}
	return req.Argv[0] == toolwire.BinPlaceholder || req.Argv[0] == binary
}

// browserNotEval reports a browser request, as recorded, of a verb other
// than eval.
func browserNotEval(args json.RawMessage) bool {
	var req toolwire.BrowserRequest
	return args != nil && json.Unmarshal(args, &req) == nil && req.Verb != "" && req.Verb != "eval"
}
