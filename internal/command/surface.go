// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package command

import (
	"slices"
	"strings"

	"github.com/devthenet-labs/maul/internal/action"
)

// Surface is where on GitHub a command was made. Each surface offers its own
// verbs, and a verb shared between surfaces means what that surface's object
// makes of it.
type Surface string

// The surfaces commands arrive on.
const (
	// FindingIssue is a Finding's tracking issue. Its verbs mean exactly what
	// they mean on the status page and in the CLI.
	FindingIssue Surface = "finding-issue"
	// IntentIssue is the issue an intent was written in.
	IntentIssue Surface = "intent-issue"
	// IntentPR is a pull request maul opened for an intent.
	IntentPR Surface = "intent-pr"
)

// usage is one verb as a surface's help lists it.
type usage struct {
	verb string
	// note says whether the verb uses its note, which the help then shows.
	note bool
	// args is the verb's argument grammar as the help shows it, such as
	// "<id>"; a verb with arguments takes no note.
	args    string
	summary string
}

// The acceptance commands, which the intent issue and every intent pull
// request offer alike: an intent has one combined change and one Preview, so
// any of its surfaces names them.
var (
	verifyUsage = usage{verb: action.VerbVerify, args: "[" + VerifyFix + "|" + VerifyStop + "|" + VerifyReset + "]",
		summary: "check the change against the plan's acceptance criteria now; `" + VerifyFix +
			"` also lets maul fix failures again, `" + VerifyStop + "` only stops maul fixing failures on its own, `" +
			VerifyReset + "` first restarts the preview with fresh data"}
	acceptUsage = usage{verb: action.VerbAccept, args: "<id>",
		summary: "accept a manual criterion, or one that passed only on retry, at the current commits"}
)

// surfaces is the design's table of which verbs apply where, in the order the
// help lists them. A Finding issue offers action.ActionVerbs, in that order.
var surfaces = map[Surface][]usage{
	FindingIssue: {
		{verb: action.VerbApprove, note: true,
			summary: "release the hold on this finding, or revive it after it was handed off"},
		{verb: action.VerbRetry, summary: "retry this finding from the state it failed in"},
		{verb: action.VerbExpedite, summary: "skip the accumulation window, the minimum age and the queue"},
		{verb: action.VerbSuspend, summary: "pause this finding's progress through the pipeline"},
		{verb: action.VerbResume, summary: "resume this finding after a suspend"},
	},
	IntentIssue: {
		{verb: action.VerbApprove, summary: "approve the posted plan and start the build"},
		{verb: action.VerbReplan, note: true,
			summary: "plan again, taking the note and approvers' comments since the last plan into account"},
		{verb: action.VerbStart, summary: "start this intent now, without waiting for the issues it is blocked by"},
		verifyUsage,
		acceptUsage,
		{verb: action.VerbCancel,
			summary: "stop work on this intent and close it; open pull requests are left to you"},
	},
	IntentPR: {
		{verb: action.VerbRevise, note: true, summary: "start a revision round from your review feedback and the note"},
		{verb: action.VerbRetry, summary: "retry the round that failed"},
		verifyUsage,
		acceptUsage,
	},
}

// Available returns the verbs surface offers, in the order its help lists
// them, or nil for an unknown surface. Whether one of them means anything in
// the object's current phase, and whether the commenter may use it, is the
// caller's to decide.
func Available(surface Surface) []string {
	usages := surfaces[surface]
	if usages == nil {
		return nil
	}
	verbs := make([]string, len(usages))
	for i, u := range usages {
		verbs[i] = u.verb
	}
	return verbs
}

// Help is the Markdown reply to a command whose verb surface does not offer:
// every verb it does offer, one per line, or "" for an unknown surface.
func Help(surface Surface) string {
	return HelpFor(surface, Available(surface))
}

// HelpFor is Help limited to verbs, for the reply to a command whose verb the
// surface offers but the object's current phase does not: the caller passes
// the verbs the phase admits (for a Finding, action.Available). It lists
// each of verbs that surface offers, in the surface's order and with the
// same usage line Help gives it, and ignores the rest. It is "" when surface
// offers none of verbs, so the caller can say that nothing is available now
// instead of sending an empty list.
func HelpFor(surface Surface, verbs []string) string {
	var b strings.Builder
	for _, u := range surfaces[surface] {
		if !slices.Contains(verbs, u.verb) {
			continue
		}
		if b.Len() == 0 {
			b.WriteString("Commands go on the first line of a comment. Here you can use:\n")
		}
		b.WriteString("\n- `" + Prefix + " " + u.verb)
		switch {
		case u.args != "":
			b.WriteString(" " + u.args)
		case u.note:
			b.WriteString(" [note]")
		}
		b.WriteString("`: " + u.summary)
	}
	return b.String()
}
