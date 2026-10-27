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
	// VerifyUsage: the command's arguments are not its grammar.
	VerifyUsage VerifyOutcome = "usage"
	// VerifyCapReached: the approved plan revision has used every
	// verification its Project allows (spec.limits.maxVerifications), so a
	// request would never run.
	VerifyCapReached VerifyOutcome = "cap-reached"
)

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
	// Verb is action.VerbVerify or action.VerbAccept.
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
	// Acceptable are the criteria an approver can accept now.
	Acceptable []string
	// ResetWait is how long resets are spaced, for VerifyResetTooSoon.
	ResetWait string
	// MaxVerifications is the Project's limit, for VerifyCapReached.
	MaxVerifications int32
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
func verifyNoticeText(n VerifyCommandNotice, surface command.Surface) (text, help string, err error) {
	cmd := slashCommand(n.Verb)
	if n.Verb == action.VerbAccept && n.Criterion != "" {
		cmd = slashCommand(n.Verb + " " + n.Criterion)
	}
	actor := code(oneLine(n.Actor))
	switch n.Outcome {
	case VerifyRequested:
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
		return b.String(), "", nil
	case VerifyResetTooSoon:
		cmd = slashCommand(n.Verb + " " + command.VerifyReset)
		return fmt.Sprintf("%s does nothing: the preview was reset less than %s ago. Use %s to verify without a "+
			"reset, or try again later.", cmd, n.ResetWait, slashCommand(action.VerbVerify)), "", nil
	case VerifyAccepted:
		return fmt.Sprintf("%s from %s is done: the criterion is accepted at %s. If the commits change, it is shown "+
			"as accepted earlier and not re-checked.", cmd, actor, joinCode(n.Heads)), "", nil
	case VerifyAcceptUnknown:
		return cmd + " names no criterion of the approved plan." + acceptable(n.Acceptable), "", nil
	case VerifyAcceptAutomatic:
		return cmd + " does nothing: that criterion is checked automatically, and only a manual one or one that " +
			"passed only on retry is accepted by hand." + acceptable(n.Acceptable), "", nil
	case VerifyAcceptNotYet:
		return cmd + " must wait: maul has not yet recorded the commits it verifies, and an acceptance holds only " +
			"at those. Try again once verification has started.", "", nil
	case VerifyUsage:
		return cmd + " was not understood.", command.HelpFor(surface, []string{n.Verb}), nil
	case VerifyCapReached:
		return fmt.Sprintf("%s does nothing: this approved plan has used all %d verifications its Project allows, "+
			"so no verification can run. Raise the Project's `spec.limits.maxVerifications` to allow more, or %s "+
			"for a new plan, whose count starts afresh.", cmd, n.MaxVerifications, slashCommand("replan")), "", nil
	}
	return "", "", fmt.Errorf("render verify notice: no text for outcome %q", n.Outcome)
}

// acceptable is the sentence naming the criteria an approver can accept now.
func acceptable(ids []string) string {
	if len(ids) == 0 {
		return " No criterion can be accepted now."
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
