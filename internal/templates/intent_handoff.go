// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"

// The gated hand-off (agent evaluator, DESIGN 7.1, 7.2): how a live
// preview's link is shown while maul checks the approved outcomes. Held, the
// link is not shown at all; once released it is labelled: ready when maul
// confirmed every outcome, "maul couldn't confirm this" (with maul's reason)
// when it gave up, and "still checking" when a human asked for it first. The
// early hand-off passes no PreviewHandoff, and its text is slice 1's.

// Hand-off states.
const (
	// HandoffHeld: the link is held until maul has checked the outcomes.
	HandoffHeld = "held"
	// HandoffReady: maul confirmed every approved outcome.
	HandoffReady = "ready"
	// HandoffUnconfirmed: maul gave up without confirming every outcome.
	HandoffUnconfirmed = "unconfirmed"
	// HandoffChecking: the link was released (an approver's review or
	// command round, a push maul did not make, /maul preview) while maul is
	// still checking.
	HandoffChecking = "checking"
)

// Why maul could not confirm, as an unconfirmed hand-off says it.
const (
	HandoffWhyUnsure      = "unsure"
	HandoffWhyFailing     = "failing"
	HandoffWhyAnomaly     = "anomaly"
	HandoffWhyRounds      = "rounds"
	HandoffWhyBudget      = "budget"
	HandoffWhyTime        = "time"
	HandoffWhySandbox     = "sandbox"
	HandoffWhyEnvironment = "environment"
	HandoffWhyUnavailable = "unavailable"

	// The agents-namespace tool pod's (SPEC-S2): its build of the head did
	// not finish, the repository has no accepted image to build in, or a
	// tree could not be fetched.
	HandoffWhyBuildFailed  = "build-failed"
	HandoffWhySandboxImage = "sandbox-image"
	HandoffWhyTree         = "tree"

	// HandoffWhyReplay: a replay round's tool pod finished before the end
	// of its replay, so a saved check may never have run (review VC-3).
	HandoffWhyReplay = "replay"
)

// handoffWhyWords are the reasons in maul's words; one not listed shows as
// the generic reason, never as the text it carried.
var handoffWhyWords = map[string]string{
	HandoffWhyUnsure:      "some outcomes are unsure",
	HandoffWhyFailing:     "an outcome fails and maul stopped fixing it",
	HandoffWhyAnomaly:     "it noticed something odd while it looked around",
	HandoffWhyRounds:      "it used every round it may run",
	HandoffWhyBudget:      "it spent its evaluation budget",
	HandoffWhyTime:        "it ran out of time",
	HandoffWhySandbox:     "its sandbox was not enforced",
	HandoffWhyEnvironment: "the preview failed while it was checked",
	HandoffWhyUnavailable: "it could not run its checks",
	// The agents-namespace tool pod's (SPEC-S2).
	HandoffWhyBuildFailed:  "its own build of the pull request did not finish",
	HandoffWhySandboxImage: "the repository has no accepted runner image to build the pull request in",
	HandoffWhyTree:         "it could not fetch the source of the pull request or of its base",
	// Review VC-3.
	HandoffWhyReplay: "it could not re-run every check it saved",
}

// workspaceEnvironmentWords is HandoffWhyEnvironment on a Project with no
// preview: what failed is maul's own tool pod, never a preview.
const workspaceEnvironmentWords = "its own checking tools failed while it checked"

// UnconfirmedCause is why a round whose every outcome was confirmed (or
// accepted) is still not confirmed, in maul's words, for the two causes
// that withhold it whatever the outcomes show (review H2): an anomaly and
// an environment that was not clean; workspace is a Project with no
// preview. "" for any other reason, which the counts already explain.
func UnconfirmedCause(why string, workspace bool) string {
	switch {
	case why == HandoffWhyEnvironment && workspace:
		return workspaceEnvironmentWords
	case why == HandoffWhyAnomaly, why == HandoffWhyEnvironment:
		return handoffWhyWords[why]
	}
	return ""
}

// HandoffWhyOfVerification is why maul could not confirm, as an
// unconfirmed hand-off says it, for a PreviewVerification that settled for
// a reason of the agents-namespace tool pod (SPEC-S2) or a replay cut short
// (review VC-3); "" for any other
// reason, and for a saturated tool pod bound, which waits and never gives
// up.
func HandoffWhyOfVerification(reason v1alpha1.PreviewVerificationReason) string {
	switch reason {
	case v1alpha1.VerificationReasonToolsBuildFailed:
		return HandoffWhyBuildFailed
	case v1alpha1.VerificationReasonSandboxImageUnavailable:
		return HandoffWhySandboxImage
	case v1alpha1.VerificationReasonTreeUnavailable:
		return HandoffWhyTree
	case v1alpha1.VerificationReasonSandboxUnenforced:
		return HandoffWhySandbox
	case v1alpha1.VerificationReasonToolsEvicted, v1alpha1.VerificationReasonToolsDeadline,
		v1alpha1.VerificationReasonKeptCopyFailed:
		return HandoffWhyUnavailable
	case v1alpha1.VerificationReasonReplayIncomplete:
		return HandoffWhyReplay
	}
	return ""
}

// PreviewHandoff is a gated hand-off's state: one of the Handoff
// constants (an unknown one is shown as held: the link stays hidden), and
// for HandoffUnconfirmed one of the HandoffWhy constants.
type PreviewHandoff struct {
	State, Why string
}

// handoffWhy is why maul could not confirm, in its words.
func handoffWhy(why string) string {
	if w, ok := handoffWhyWords[why]; ok {
		return w
	}
	return handoffWhyWords[HandoffWhyUnsure]
}

// handoffLabel is the label a released link carries, ending in a full
// stop; "" for a held one.
func handoffLabel(h PreviewHandoff) string {
	switch h.State {
	case HandoffReady:
		return "Ready: maul checked the approved outcomes."
	case HandoffUnconfirmed:
		return "maul couldn't confirm this: " + handoffWhy(h.Why) + "."
	case HandoffChecking:
		return "Shown while maul is still checking the approved outcomes."
	}
	return ""
}

// handoffHeld is the sentence of a held link: what maul is doing, and how
// to see it now. It names what intent-controller releases a held link on
// (haltHuman, releaseForPush, and /maul preview): an approver's round on a
// pull request, not any comment and not /maul verify.
func handoffHeld(subject string) string {
	return subject + " is live, and maul holds its link until it has checked the approved outcomes. An approver's " +
		"review asking for changes or " + slashCommand("revise") + " on a pull request, a push maul did not make " +
		"or " + slashCommand("preview") + " shows it now."
}

// handoffStatusSentence is the status comment's preview sentence under h,
// with linked, the sentence as it is with the link.
func handoffStatusSentence(h PreviewHandoff, linked string) string {
	if label := handoffLabel(h); label != "" {
		return label + " " + linked
	}
	return handoffHeld("The preview")
}

// handoffCommentSentence is a pull request's preview comment sentence
// under h: head names the pull request's head, linked the sentence with
// the link.
func handoffCommentSentence(h PreviewHandoff, head, linked string) string {
	if label := handoffLabel(h); label != "" {
		return label + " " + linked
	}
	return handoffHeld(head)
}

// The link-less hand-off (SPEC-S2): a Project with no preview is evaluated
// in maul's own build of the pull request, so there is no link to hold;
// what is held is the hand-off itself, and once released the line says
// where maul stands, as the linked one does.

// workspaceBuild is where a workspace evaluation checks the change.
const workspaceBuild = "in its own build of the pull request"

// workspaceRelease is what hands a held workspace evaluation over now:
// what intent-controller releases a held hand-off on, as handoffHeld names
// it, less /maul preview, which shows a link there is none of.
func workspaceRelease() string {
	return "An approver's review asking for changes or " + slashCommand("revise") + " on a pull request, or a " +
		"push maul did not make, hands it over now."
}

// workspaceLabel is a released workspace hand-off's label, ending in a
// full stop: the linked label, except that nothing is shown while maul is
// still checking and a failing environment names no preview; "" for a
// held one.
func workspaceLabel(h PreviewHandoff) string {
	switch {
	case h.State == HandoffChecking:
		return "Handed to its reviewers while maul is still checking the approved outcomes " + workspaceBuild + "."
	case h.State == HandoffUnconfirmed && h.Why == HandoffWhyEnvironment:
		return "maul couldn't confirm this: " + workspaceEnvironmentWords + "."
	}
	return handoffLabel(h)
}

// workspaceStatusSentence is the status comment's evaluation sentence
// under h; nil (the early hand-off) says only what maul does.
func workspaceStatusSentence(h *PreviewHandoff) string {
	if h == nil {
		return "maul checks the approved outcomes " + workspaceBuild + "."
	}
	if label := workspaceLabel(*h); label != "" {
		return label
	}
	return "maul is checking the approved outcomes " + workspaceBuild + ", and holds its hand-off to the " +
		"reviewers until it has. " + workspaceRelease()
}

// workspaceCommentSentence is a pull request's evaluation sentence under h:
// rev is the pull request's head, as shown.
func workspaceCommentSentence(h *PreviewHandoff, rev string) string {
	if h == nil {
		return workspaceStatusSentence(nil) + " This pull request's head is " + rev + "."
	}
	if label := workspaceLabel(*h); label != "" {
		return label + " This pull request's head is " + rev + "."
	}
	return "This pull request's head, " + rev + ", is being checked: maul holds its hand-off to the reviewers " +
		"until it has checked the approved outcomes " + workspaceBuild + ". " + workspaceRelease()
}
