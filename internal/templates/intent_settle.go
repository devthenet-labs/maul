// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"

// SettleWords are how every surface names a round that settled without a
// judgement for a cause (v1alpha1.SettleCause): one table, so the hand-off,
// a /maul verify that did not run and the commit status cannot name the
// same cause apart.
type SettleWords struct {
	// Why is the hand-off's reason (a HandoffWhy constant); "" for a cause
	// the verification's state already says (SettleNone and
	// SettlePreviewNotLive: maul could not run its checks).
	Why string
	// NotRun is why an approver's /maul verify did not run (a NotRun
	// constant) for a cause found before a round runs; "" for a cause only
	// a round's attempts settle with.
	NotRun string
	// Short is the cause as the commit status of an Unavailable round says
	// it, maul's words.
	Short string
}

// settleWords are the causes' words: [0] with a preview, [1] on a Project
// with no preview.
var settleWords = map[v1alpha1.SettleCause][2]SettleWords{
	v1alpha1.SettleNone: {
		{Short: "maul could not run the checks"},
		{Short: "maul could not run the checks"},
	},
	v1alpha1.SettlePreviewNotLive: {
		{Short: "the preview did not become live at these commits in time, so the checks could not run"},
		{Short: "maul could not run the checks"},
	},
	v1alpha1.SettleSandboxImage: {
		{Why: HandoffWhySandboxImage, NotRun: NotRunSandboxImage, Short: sandboxImageShort},
		{Why: HandoffWhySandboxImage, NotRun: NotRunSandboxImage, Short: sandboxImageShort},
	},
	v1alpha1.SettleTree: {
		{Why: HandoffWhyTree, NotRun: NotRunTree, Short: treeShort},
		{Why: HandoffWhyTree, NotRun: NotRunTree, Short: treeShort},
	},
	v1alpha1.SettleToolsBuildFailed: {
		{Why: HandoffWhyBuildFailed, Short: buildFailedShort},
		{Why: HandoffWhyBuildFailed, Short: buildFailedShort},
	},
	v1alpha1.SettleSandboxUnenforced: {
		{Why: HandoffWhySandbox, Short: sandboxShort},
		{Why: HandoffWhySandbox, Short: sandboxShort},
	},
	v1alpha1.SettleToolsLost: {
		{Why: HandoffWhyUnavailable, Short: toolsLostShort},
		{Why: HandoffWhyUnavailable, Short: toolsLostShort},
	},
	v1alpha1.SettleReplayIncomplete: {
		{Why: HandoffWhyReplay, Short: replayShort},
		{Why: HandoffWhyReplay, Short: replayShort},
	},
}

// The causes' commit status words that read alike in both placements.
const (
	sandboxImageShort = "the repository has no accepted runner image to build the pull request in, so the checks " +
		"could not run"
	treeShort        = "maul could not fetch the source of the pull request or of its base, so the checks could not run"
	buildFailedShort = "maul's own build of the pull request did not finish, so the checks could not run"
	sandboxShort     = "maul's sandbox was not enforced, so the checks could not run"
	toolsLostShort   = "maul's own checking tools stopped before the checks finished"
	replayShort      = "maul could not re-run every check it saved"
)

// CauseWords are c's words, on a Project with no preview when workspace;
// a cause not listed reads as SettleNone, never as the text it carried.
func CauseWords(c v1alpha1.SettleCause, workspace bool) SettleWords {
	w, ok := settleWords[c]
	if !ok {
		w = settleWords[v1alpha1.SettleNone]
	}
	if workspace {
		return w[1]
	}
	return w[0]
}
