// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"strings"

	"github.com/devthenet-labs/maul/internal/templates"
)

// changeFocused reports whether h's round is a change-focused one: a round
// after a judged one that is handed the whole story of what changed. It is
// derived from the handoff alone, so no controller flag or handoff key
// exists and no controller and runner can disagree about it. It holds when
//   - the handoff carries a previous round and a diff since it,
//   - it carries that round's result for every outcome of this one, and
//   - the prompt quotes the diff whole (templates.FixDiffWhole): no
//     repository's compare unreadable or cut by the forge, no file left
//     out, and nothing cut by the controller's or the prompt's own bound.
//     The light look at an untouched outcome that passed rests on the
//     evaluator having read all of the fix, so a cut diff is never
//     focused: every outcome is then judged as a first round judges it.
//
// A change-focused round only changes the prompt's words: its limits, its
// ledger and the pass rules are a first round's. The controller sends a
// diff and previous results only when they describe the same judged round
// (previousRound); that is what makes this derivation safe.
func (h EvaluateHandoff) changeFocused() bool {
	if h.PriorRound < 1 || strings.TrimSpace(h.FixDiff) == "" {
		return false
	}
	if !templates.FixDiffWhole(h.FixDiff, h.fixDiffCut()) {
		return false
	}
	had := make(map[string]bool, len(h.PriorVerdicts))
	for _, v := range h.PriorVerdicts {
		had[v.Outcome] = true
	}
	for _, o := range h.Outcomes {
		if !had[o.ID] {
			return false
		}
	}
	return true
}

// fixDiffCut is h's FixDiffCut as the prompt's data; the zero value when
// the diff is the whole fix.
func (h EvaluateHandoff) fixDiffCut() templates.FixDiffCut {
	c := h.FixDiffCut
	if c == nil {
		return templates.FixDiffCut{}
	}
	return templates.FixDiffCut{ShownBytes: c.ShownBytes, ReadBytes: c.ReadBytes, ForgeCut: c.ForgeCut,
		Files: c.Files, MoreFiles: c.MoreFiles, Unavailable: c.Unavailable}
}
