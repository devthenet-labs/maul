// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import "strings"

// changeFocused reports whether h's round is a change-focused one: a round
// after a judged one that is handed the whole story of what changed. It is
// derived from the handoff alone, so no controller flag or handoff key
// exists and no controller and runner can disagree about it. It holds when
//   - the handoff carries a previous round and a diff since it,
//   - it carries that round's result for every outcome of this one, and
//   - none of the diff is a repository's whose compare maul could not
//     read, or one the forge had cut (a diff the prompt's own bound cut
//     leaves the round focused: the prompt then says every outcome is
//     affected, and the files it leaves out are named).
//
// A change-focused round only changes the prompt's words: its limits, its
// ledger and the pass rules are a first round's. The controller sends a
// diff and previous results only when they describe the same judged round
// (previousRound); that is what makes this derivation safe.
func (h EvaluateHandoff) changeFocused() bool {
	if h.PriorRound < 1 || strings.TrimSpace(h.FixDiff) == "" {
		return false
	}
	if c := h.FixDiffCut; c != nil && (len(c.Unavailable) > 0 || len(c.ForgeCut) > 0) {
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
