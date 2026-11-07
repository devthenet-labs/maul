// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"fmt"
	"strings"

	"github.com/devthenet-labs/maul/internal/action"
	"github.com/devthenet-labs/maul/internal/command"
)

// VerifyOutcome is what maul made of a /maul verify or /maul accept.
type VerifyOutcome string

// Verify command outcomes.
const (
	// VerifyRequested: the verification request is recorded.
	VerifyRequested VerifyOutcome = "requested"
	// VerifyResetTooSoon: a reset was refused, the last being too recent.
	VerifyResetTooSoon VerifyOutcome = "reset-too-soon"
	// VerifyAccepted: the criterion is accepted at the current commits.
	VerifyAccepted VerifyOutcome = "accepted"
	// VerifyAcceptUnknown: the approved plan has no criterion of that id.
	VerifyAcceptUnknown VerifyOutcome = "accept-unknown"
	// VerifyAcceptAutomatic: the criterion is checked automatically and
	// passed or failed outright, so no human acceptance stands in for it.
	VerifyAcceptAutomatic VerifyOutcome = "accept-automatic"
	// VerifyAcceptNotYet: maul has not recorded the commits under
	// verification yet, so there is nothing to bind an acceptance to.
	VerifyAcceptNotYet VerifyOutcome = "accept-not-yet"
	// VerifyAcceptMoved: the preview was not live at the commits under
	// verification when the acceptance was given (or is not now), so maul
	// cannot tell which commits the approver checked.
	VerifyAcceptMoved VerifyOutcome = "accept-moved"
	// VerifyAcceptNotChecked: VerifyAcceptMoved for a Project with no
	// preview (SPEC-S2): maul was not checking the commits under
	// verification when the approver commented.
	VerifyAcceptNotChecked VerifyOutcome = "accept-not-checked"
	// VerifyApplied: the command was applied before; this is its reply
	// again, its effect since consumed or superseded.
	VerifyApplied VerifyOutcome = "applied"
	// VerifyUsage: the command's arguments are not its grammar.
	VerifyUsage VerifyOutcome = "usage"
	// VerifyCapReached: the approved plan revision has used every
	// verification its Project allows (spec.limits.maxVerifications), so a
	// request would never run.
	VerifyCapReached VerifyOutcome = "cap-reached"
	// VerifyStopped: /maul verify stop halted automatic fixes.
	VerifyStopped VerifyOutcome = "stopped"
	// VerifyDefectConfirmed: /maul verify fix confirmed the defect maul
	// suspected, which starts a fix round on it.
	VerifyDefectConfirmed VerifyOutcome = "defect-confirmed"
	// VerifyPreviewReleased: /maul preview released the held preview link.
	VerifyPreviewReleased VerifyOutcome = "preview-released"
	// VerifyPreviewNotHeld: nothing holds the preview link (a plan with
	// criteria, or an early hand-off), so /maul preview has nothing to
	// release.
	VerifyPreviewNotHeld VerifyOutcome = "preview-not-held"
	// VerifyPreviewAlready: the preview link was released before.
	VerifyPreviewAlready VerifyOutcome = "preview-already"
	// VerifyPreviewNone: the Project has no preview (an agents-namespace
	// evaluation, SPEC-S2), so /maul preview has no link to show.
	VerifyPreviewNone VerifyOutcome = "preview-none"
	// VerifyNotRun: the request cannot be acted on, or was consumed
	// without a run (S2-B8): NotRunWhy says why, and Stands names the
	// evaluation whose result at the same commits stands.
	VerifyNotRun VerifyOutcome = "not-run"
)

// Why a /maul verify did not run (VerifyNotRun), in maul's words
// (verifyNotRunWords); a reason not listed is said as the generic one.
const (
	// NotRunNotEnabled: the Project does not verify its intents.
	NotRunNotEnabled = "not-enabled"
	// NotRunToolPodOff: the agents tool pod (--intent-evaluator-agents)
	// is off, and the Project has a cli or library surface.
	NotRunToolPodOff = "tool-pod-off"
	// NotRunSurfaceCombination: the Project sets a preview beside its cli
	// or library surface.
	NotRunSurfaceCombination = "surface-combination"
	// NotRunPreviewsOff: previews are off, and the Project is verified in
	// its preview.
	NotRunPreviewsOff = "previews-off"
	// NotRunHarnessUnverified: the Project's harness has no evaluator maul
	// verified.
	NotRunHarnessUnverified = "harness-unverified"
	// NotRunNothingToCheck: the approved plan has nothing maul checks.
	NotRunNothingToCheck = "nothing-to-check"
	// NotRunSandboxImage, NotRunTree: an agents round cannot run: no
	// accepted runner image, or a tree that cannot be fetched.
	NotRunSandboxImage = "sandbox-image"
	NotRunTree         = "tree"
	// NotRunBudget: the intent's evaluations spent their ceiling.
	NotRunBudget = "budget"
	// NotRunCap: the approved plan revision used every verification its
	// Project allows (spec.limits.maxVerifications) before it ran.
	NotRunCap = "cap"
)

// verifyNotRunWords are the reasons a /maul verify did not run, in maul's
// words; "" is the generic one.
var verifyNotRunWords = map[string]string{
	"":               "maul could not run its checks",
	NotRunNotEnabled: "verification is not enabled for this project",
	NotRunToolPodOff: "the tool pod that checks a project with no preview is not enabled in this installation",
	NotRunSurfaceCombination: "this project sets a preview beside its cli or library surface, and maul " +
		"verifies only one of them",
	NotRunPreviewsOff:       "previews are not enabled in this installation, and this project is checked in its preview",
	NotRunHarnessUnverified: "maul does not evaluate intents built on this project's harness",
	NotRunNothingToCheck:    "the approved plan has nothing maul checks",
	NotRunSandboxImage:      handoffWhyWords[HandoffWhySandboxImage],
	NotRunTree:              handoffWhyWords[HandoffWhyTree],
	NotRunBudget:            "this intent's evaluations spent their budget",
	NotRunCap:               "this approved plan has used every verification its project allows",
}

// VerifyCommandNotice answers a /maul verify or /maul accept on the intent
// issue or one of its pull requests.
type VerifyCommandNotice struct {
	// Namespace, Intent and Key make the notice's marker (NoticeMarker).
	Namespace string
	Intent    string
	Key       string
	// Surface is where the command was made; zero means the intent issue.
	Surface command.Surface
	// Actor is the login that made the command.
	Actor string
	// Verb is action.VerbVerify, action.VerbAccept or action.VerbPreview.
	Verb string
	// Outcome is what maul made of it.
	Outcome VerifyOutcome
	// Fix and Reset are the verify modifiers that were applied.
	Fix   bool
	Reset bool
	// Joined: the request joined one already waiting.
	Joined bool
	// AfterRound: a round is running; the verification follows it.
	AfterRound bool
	// Criterion is the id an accept named, already checked against the
	// criterion id grammar (command.ParseAccept); empty for a verify.
	Criterion string
	// Heads are the short commits an acceptance was given at.
	Heads []string
	// NoCriteria: the accepted plan declares no criteria (/maul accept all).
	NoCriteria bool
	// Outcomes: the approved plan declares outcomes (the evaluator's), so
	// an accept names outcomes, not criteria.
	Outcomes bool
	// Accepted are the outcomes an accept closed, in plan order: the one
	// it named, or every one /maul accept all closed.
	Accepted []string
	// Acceptable are the criteria or outcomes an approver can accept now.
	Acceptable []string
	// ResetWait is how long resets are spaced, for VerifyResetTooSoon.
	ResetWait string
	// MaxVerifications is the Project's limit, for VerifyCapReached.
	MaxVerifications int32
	// Cancelled: the stop cancelled an automatic fix not yet started;
	// Running: one already running finishes. For VerifyStopped.
	Cancelled, Running bool
	// NotRunWhy is why the request did not run (a NotRun constant), and
	// Stands the evaluation whose result at the same commits stands (0:
	// none), for VerifyNotRun.
	NotRunWhy string
	Stands    int32
}

// RenderVerifyCommandNotice renders a VerifyCommandNotice.
func RenderVerifyCommandNotice(n VerifyCommandNotice) (string, error) {
	surface := n.Surface
	if surface == "" {
		surface = command.IntentIssue
	}
	text, help, err := verifyNoticeText(n, surface)
	if err != nil {
		return "", err
	}
	return render("intent_notice_verify.md.tmpl", struct {
		Marker string
		Text   string
		Help   string
	}{
		Marker: NoticeMarker(n.Namespace, n.Intent, n.Key),
		Text:   text,
		Help:   help,
	})
}

// verifyNoticeText is the notice's sentence and, where it asks for one, the
// help.
// ownNoticeText is the reply with words of its own: a request that did not
// run (notRunText), an acceptance of an outcome (outcomeAcceptText), or a
// Project with no preview's (workspaceNoticeText); ok is false for any
// other.
func ownNoticeText(n VerifyCommandNotice, cmd, actor string) (string, bool) {
	if n.Outcome == VerifyNotRun {
		return notRunText(n, actor), true
	}
	if text, ok := outcomeAcceptText(n, cmd, actor); ok {
		return text, true
	}
	return workspaceNoticeText(n, cmd)
}

// workspaceNoticeText is the reply of a Project with no preview (an agents
// evaluation, SPEC-S2) where it differs from a preview's: ok is false for
// any other outcome.
func workspaceNoticeText(n VerifyCommandNotice, cmd string) (string, bool) {
	switch n.Outcome {
	case VerifyPreviewNone:
		return cmd + " does nothing: this project has no preview, so there is no link to show. maul checks the " +
			"approved outcomes " + workspaceBuild + ", and hands the result to the reviewers once it has.", true
	case VerifyAcceptNotChecked:
		return fmt.Sprintf("%s does nothing: maul was not checking %s, the commits it verifies, when you "+
			"commented, so it cannot tell which commits you checked. Accept again once maul checks them.", cmd,
			joinCode(n.Heads)), true
	}
	return "", false
}

func verifyNoticeText(n VerifyCommandNotice, surface command.Surface) (text, help string, err error) {
	cmd := slashCommand(n.Verb)
	if n.Verb == action.VerbAccept && n.Criterion != "" {
		cmd = slashCommand(n.Verb + " " + n.Criterion)
	}
	actor := code(oneLine(n.Actor))
	if text, ok := ownNoticeText(n, cmd, actor); ok {
		return text, "", nil
	}
	switch n.Outcome {
	case VerifyPreviewReleased:
		return fmt.Sprintf("%s from %s is recorded: the preview link is shown now, before maul has finished "+
			"checking the change. maul goes on checking it, and its results follow.", cmd, actor), "", nil
	case VerifyPreviewNotHeld:
		return cmd + " does nothing: the preview link is not held for this intent, so it is shown as soon as the " +
			"preview runs the current commits.", "", nil
	case VerifyPreviewAlready:
		return cmd + " does nothing: the preview link was released already.", "", nil

	case VerifyRequested:
		return requestedText(n, actor), "", nil
	case VerifyResetTooSoon:
		cmd = slashCommand(n.Verb + " " + command.VerifyReset)
		return fmt.Sprintf("%s does nothing: the preview was reset less than %s ago. Use %s to verify without a "+
			"reset, or try again later.", cmd, n.ResetWait, slashCommand(action.VerbVerify)), "", nil
	case VerifyAccepted:
		if n.NoCriteria {
			return fmt.Sprintf("%s from %s is done: the change, whose approved plan declares no acceptance criteria, "+
				"is accepted at %s. If the commits change, it must be accepted again.", cmd, actor,
				joinCode(n.Heads)), "", nil
		}
		return fmt.Sprintf("%s from %s is done: the criterion is accepted at %s. If the commits change, it is shown "+
			"as accepted earlier and not re-checked.", cmd, actor, joinCode(n.Heads)), "", nil
	case VerifyAcceptUnknown:
		return cmd + " names no criterion of the approved plan." + acceptable("criterion", n.Acceptable), "", nil
	case VerifyAcceptAutomatic:
		return cmd + " does nothing: that criterion is checked automatically, and only a manual one, one maul does " +
			"not run yet or one that passed only on retry is accepted by hand." +
			acceptable("criterion", n.Acceptable), "", nil
	case VerifyAcceptNotYet:
		return cmd + " must wait: maul has not yet recorded the commits it verifies, and an acceptance holds only " +
			"at those. Try again once verification has started.", "", nil
	case VerifyAcceptMoved:
		return fmt.Sprintf("%s does nothing: the preview was not running %s, the commits maul verifies, when you "+
			"commented, so maul cannot tell which commits you checked. Check the preview once it runs them and "+
			"accept again.", cmd, joinCode(n.Heads)), "", nil
	case VerifyStopped:
		return stoppedText(n, actor), "", nil
	case VerifyDefectConfirmed:
		return fmt.Sprintf("%s from %s confirms the defect maul suspected: maul starts a fix round on it, handed "+
			"maul's reproduction, and checks the outcomes again once the fix is pushed.",
			slashCommand(n.Verb+" "+command.VerifyFix), actor), "", nil
	case VerifyApplied:
		return fmt.Sprintf("%s from %s was applied already, and is not applied again.", cmd, actor), "", nil
	case VerifyUsage:
		return cmd + " was not understood.", command.HelpFor(surface, []string{n.Verb}), nil
	case VerifyCapReached:
		return fmt.Sprintf("%s does nothing: this approved plan has used all %d verifications its Project allows, "+
			"so no verification can run. Raise the Project's `spec.limits.maxVerifications` to allow more.", cmd,
			n.MaxVerifications), "", nil
	}
	return "", "", fmt.Errorf("render verify notice: no text for outcome %q", n.Outcome)
}

// requestedText is the reply to a recorded /maul verify from actor.
func requestedText(n VerifyCommandNotice, actor string) string {
	cmd := slashCommand(n.Verb)
	if n.Fix {
		cmd = slashCommand(n.Verb + " " + command.VerifyFix)
	}
	if n.Reset {
		cmd = slashCommand(n.Verb + " " + command.VerifyReset)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s from %s is recorded: maul checks the combined change against the approved acceptance "+
		"criteria ", cmd, actor)
	if n.AfterRound {
		b.WriteString("after the current round.")
	} else {
		b.WriteString("once its checks are green and the preview runs the current commits.")
	}
	if n.Joined {
		b.WriteString(" It joins a verification already waiting.")
	}
	if n.Reset {
		b.WriteString(" The preview restarts with fresh data first.")
	}
	if n.Fix {
		b.WriteString(" maul may fix failing criteria on its own again.")
	}
	return b.String()
}

// notRunText is the reply to a /maul verify from actor that did not run:
// why, in maul's words, what stands instead, and when to ask again.
func notRunText(n VerifyCommandNotice, actor string) string {
	cmd := slashCommand(n.Verb)
	if n.Fix {
		cmd = slashCommand(n.Verb + " " + command.VerifyFix)
	}
	if n.Reset {
		cmd = slashCommand(n.Verb + " " + command.VerifyReset)
	}
	words, ok := verifyNotRunWords[n.NotRunWhy]
	if !ok {
		words = verifyNotRunWords[""]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s from %s did not run: %s.", cmd, actor, words)
	if n.Stands > 0 {
		fmt.Fprintf(&b, " Evaluation %d's result at these commits stands.", n.Stands)
	}
	if n.NotRunWhy != NotRunNothingToCheck {
		fmt.Fprintf(&b, " Nothing was checked; send %s again once that changes.", cmd)
	}
	return b.String()
}

// stoppedText is the reply to /maul verify stop from actor.
func stoppedText(n VerifyCommandNotice, actor string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s from %s is recorded: maul no longer fixes failing acceptance criteria on its own.",
		slashCommand(n.Verb+" "+command.VerifyStop), actor)
	if n.Cancelled {
		b.WriteString(" The automatic fix that had not started yet is cancelled.")
	}
	if n.Running {
		b.WriteString(" The automatic fix already running finishes and pushes; no other follows it.")
	}
	fmt.Fprintf(&b, " Verification still runs and reports, and nothing else starts. %s lets maul fix failures "+
		"again.", slashCommand(n.Verb+" "+command.VerifyFix))
	return b.String()
}

// outcomeAcceptText is the reply to a /maul accept on a plan that declares
// outcomes, for the replies whose words differ from a criterion's; ok is
// false for any other notice.
func outcomeAcceptText(n VerifyCommandNotice, cmd, actor string) (string, bool) {
	if !n.Outcomes || n.Verb != action.VerbAccept {
		return "", false
	}
	const byHand = "only a manual outcome, or one maul could not confirm or found failing, is accepted by hand."
	switch n.Outcome {
	case VerifyAccepted:
		if n.Criterion == command.AcceptAll {
			return fmt.Sprintf("%s from %s is done: the outcomes %s are accepted at %s. If the commits change, they "+
				"are shown as accepted earlier and not re-checked.", cmd, actor, joinCode(n.Accepted),
				joinCode(n.Heads)), true
		}
		return fmt.Sprintf("%s from %s is done: the outcome is accepted at %s. If the commits change, it is shown "+
			"as accepted earlier and not re-checked.", cmd, actor, joinCode(n.Heads)), true
	case VerifyAcceptUnknown:
		return cmd + " names no outcome of the approved plan." + acceptable("outcome", n.Acceptable), true
	case VerifyAcceptAutomatic:
		if n.Criterion == command.AcceptAll {
			return cmd + " does nothing: " + byHand + acceptable("outcome", n.Acceptable), true
		}
		return cmd + " does nothing: that outcome passed, or maul has not decided it yet, and " + byHand +
			acceptable("outcome", n.Acceptable), true
	}
	return "", false
}

// acceptable is the sentence naming the criteria or outcomes (noun) an
// approver can accept now.
func acceptable(noun string, ids []string) string {
	if len(ids) == 0 {
		return " No " + noun + " can be accepted now."
	}
	return " You can accept: " + joinCode(ids) + "."
}

// joinCode joins each of items as code.
func joinCode(items []string) string {
	out := make([]string, 0, len(items))
	for _, s := range items {
		if c := code(oneLine(s)); c != "" {
			out = append(out, c)
		}
	}
	return strings.Join(out, ", ")
}
