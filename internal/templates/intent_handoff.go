// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

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
