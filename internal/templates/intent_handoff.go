// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"fmt"
	"strings"
)

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

	// HandoffWhyEvaluatorDoubts: the evaluator judged outcomes not to
	// hold that no check maul re-ran shows failing (gate D-B6); the
	// hand-off counts them (PreviewHandoff.Doubted) and never calls them
	// failing.
	HandoffWhyEvaluatorDoubts = "evaluator-doubts"

	// The agents-namespace tool pod's (SPEC-S2): its build of the head did
	// not finish, the repository has no accepted image to build in, or a
	// tree could not be fetched.
	HandoffWhyBuildFailed  = "build-failed"
	HandoffWhySandboxImage = "sandbox-image"
	HandoffWhyTree         = "tree"

	// HandoffWhyReplay: a replay round's tool pod finished before the end
	// of its replay, so a saved check may never have run (review VC-3).
	HandoffWhyReplay = "replay"

	// HandoffWhyInfra: the environment failed for maul's own reasons (its
	// tools, the cluster), never the change's (review H3); the change's
	// own failure is HandoffWhyEnvironment.
	HandoffWhyInfra = "infra"

	// HandoffWhySuspected: maul suspects a defect in outcomes
	// (PreviewHandoff.Suspected) and waits for a person to confirm it
	// (gate D-B11): it leads, before outcomes in doubt or an anomaly.
	HandoffWhySuspected = "suspected"
)

// handoffWhyWords are the reasons in maul's words; one not listed shows as
// the generic reason, never as the text it carried. A Project with no
// preview reads workspaceWhyWords first.
var handoffWhyWords = map[string]string{
	HandoffWhyUnsure:      "some outcomes are unsure",
	HandoffWhyFailing:     "an outcome fails and maul stopped fixing it",
	HandoffWhyAnomaly:     "it noticed something odd while it looked around",
	HandoffWhyRounds:      "it used every round it may run",
	HandoffWhyBudget:      "it spent its evaluation budget",
	HandoffWhyTime:        "it ran out of time",
	HandoffWhySandbox:     "its sandbox was not enforced",
	HandoffWhyEnvironment: "the preview failed while it was checked",
	HandoffWhyInfra:       "its own checking environment failed while it checked",
	HandoffWhyUnavailable: "it could not run its checks",
	// Gate D-B6; the words without a count, which doubtedWords gives.
	HandoffWhyEvaluatorDoubts: "the evaluator judged some outcomes not to hold, but no check maul re-ran shows it",
	// Gate D-B11; the words without the outcomes, which suspectedWhy gives.
	HandoffWhySuspected: "it suspects a defect, which a person should confirm",
	// The agents-namespace tool pod's (SPEC-S2).
	HandoffWhyBuildFailed:  "its own build of the pull request did not finish",
	HandoffWhySandboxImage: "the repository has no accepted runner image to build the pull request in",
	HandoffWhyTree:         "it could not fetch the source of the pull request or of its base",
	// Review VC-3.
	HandoffWhyReplay: "it could not re-run every check it saved",
}

// workspaceWhyWords are the reasons whose words differ on a Project with
// no preview: what fails there is maul's own tool pod, never a preview.
var workspaceWhyWords = map[string]string{
	HandoffWhyEnvironment: "its own checking tools failed while it checked",
	HandoffWhyInfra:       "its own checking tools failed while it checked",
}

// whyWords are why's words, on a Project with no preview when workspace;
// ok is false for a reason not listed.
func whyWords(why string, workspace bool) (words string, ok bool) {
	if w, found := workspaceWhyWords[why]; found && workspace {
		return w, true
	}
	w, found := handoffWhyWords[why]
	return w, found
}

// UnconfirmedCause is why a round whose every outcome was confirmed (or
// accepted) is still not confirmed, in maul's words, for the causes that
// withhold it whatever the outcomes show (review H2): an anomaly, and an
// environment that was not clean, the change's or maul's own (review H3);
// workspace is a Project with no preview. "" for any other reason, which
// the counts already explain.
func UnconfirmedCause(why string, workspace bool) string {
	switch why {
	case HandoffWhyAnomaly, HandoffWhyEnvironment, HandoffWhyInfra:
		w, _ := whyWords(why, workspace)
		return w
	}
	return ""
}

// PreviewHandoff is a gated hand-off's state: one of the Handoff
// constants (an unknown one is shown as held: the link stays hidden), and
// for HandoffUnconfirmed one of the HandoffWhy constants.
type PreviewHandoff struct {
	State, Why string
	// Doubted counts the outcomes the evaluator judged not to hold (gate
	// D-B6): the reason itself for HandoffWhyEvaluatorDoubts, named after
	// the outcomes maul suspects for HandoffWhySuspected.
	Doubted int32
	// Suspected are, for HandoffWhySuspected, the outcomes maul suspects a
	// defect in (gate D-B11).
	Suspected []string
	// Anomaly: for HandoffUnconfirmed, a look-around anomaly is named after
	// the reason, which leads with the outcomes in doubt (gate D-B6).
	Anomaly bool
}

// handoffWhy is why maul could not confirm, in its words (on a Project
// with no preview when workspace): the reason, then the outcomes in doubt
// after a suspected defect, then an anomaly, if any.
func handoffWhy(h PreviewHandoff, workspace bool) string {
	w, ok := whyWords(h.Why, workspace)
	switch {
	case !ok:
		w = handoffWhyWords[HandoffWhyUnsure]
	case h.Why == HandoffWhyEvaluatorDoubts && h.Doubted > 0:
		w = doubtedWords(h.Doubted)
	case h.Why == HandoffWhySuspected:
		w = suspectedWhy(h.Suspected)
		if h.Doubted > 0 {
			w += ", and " + doubtedWords(h.Doubted)
		}
	}
	if h.Anomaly && h.Why != HandoffWhyAnomaly {
		w += ", and " + handoffWhyWords[HandoffWhyAnomaly]
	}
	return w
}

// doubtedWords are HandoffWhyEvaluatorDoubts' words for n outcomes: the
// evaluator's judgement, counted, and that maul did not prove it.
func doubtedWords(n int32) string {
	return fmt.Sprintf("the evaluator judged %d %s not to hold, but no check maul re-ran shows it", n,
		plural(n, "outcome", "outcomes"))
}

// suspectedWhy are HandoffWhySuspected's words naming the outcomes, each
// shown as code; the words without them when none can be shown.
func suspectedWhy(outcomes []string) string {
	ids := make([]string, 0, len(outcomes))
	for _, id := range outcomes {
		if c := shown(id); c != "" {
			ids = append(ids, code(c))
		}
	}
	if len(ids) == 0 {
		return handoffWhyWords[HandoffWhySuspected]
	}
	return "it suspects a defect in " + strings.Join(ids, ", ") + ", which a person should confirm"
}

// handoffLabel is the label a released link carries, ending in a full
// stop; "" for a held one.
func handoffLabel(h PreviewHandoff) string { return labelIn(h, false) }

// labelIn is handoffLabel, on a Project with no preview when workspace.
func labelIn(h PreviewHandoff, workspace bool) string {
	switch h.State {
	case HandoffReady:
		return "Ready: maul checked the approved outcomes."
	case HandoffUnconfirmed:
		return "maul couldn't confirm this: " + handoffWhy(h, workspace) + "."
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
// still checking and a failing environment names no preview
// (workspaceWhyWords); "" for a held one.
func workspaceLabel(h PreviewHandoff) string {
	if h.State == HandoffChecking {
		return "Handed to its reviewers while maul is still checking the approved outcomes " + workspaceBuild + "."
	}
	return labelIn(h, true)
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
