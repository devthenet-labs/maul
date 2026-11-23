// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package toolledger

import (
	"encoding/json"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/checks"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// OwnCall reports a cited call that ran a page script or program the
// evaluator wrote, which every surface labels as its own ("evaluator
// observed", owner decision 1: it counts, and the reviewers are told whose
// it is). verb is the call's verb in the round's verified index; args is
// its request as its ledger line recorded it (as the evaluator sent it,
// before any placeholder was filled), nil when maul kept no line that is
// the index's call; agents is an agents (cli or library) round, binary its
// built cli binary ("" for a library).
//
// A Go program (run) and a slot exec (the toolbox runs only what it is
// sent) are the evaluator's own from the verb alone. An agents exec runs
// any argv with any files the evaluator supplies (workspaceExec), so it is
// its own unless its line shows exactly the built binary
// (checks.BinPlaceholder or binary, as argv[0]) with no files: unlabelled
// needs that proof, and an exec with no readable line is labelled. A
// browser call is a page script only when its line shows an eval; with no
// line nothing shows it was one.
func OwnCall(verb v1alpha1.LedgerVerb, args json.RawMessage, agents bool, binary string) bool {
	switch verb {
	case v1alpha1.LedgerVerbRun:
		return true
	case v1alpha1.LedgerVerbExec:
		return !agents || !builtBinaryOnly(args, binary)
	case v1alpha1.LedgerVerbBrowser:
		var req toolwire.BrowserRequest
		return args != nil && json.Unmarshal(args, &req) == nil && req.Verb == "eval"
	}
	return false
}

// builtBinaryOnly reports an exec request, as recorded, of exactly a cli's
// built binary with no files of the evaluator's.
func builtBinaryOnly(args json.RawMessage, binary string) bool {
	var req toolwire.ExecRequest
	if binary == "" || args == nil || json.Unmarshal(args, &req) != nil || len(req.Argv) == 0 ||
		len(req.Files) > 0 {
		return false
	}
	return req.Argv[0] == checks.BinPlaceholder || req.Argv[0] == binary
}
