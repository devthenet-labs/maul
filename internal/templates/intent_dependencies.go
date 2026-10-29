// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"fmt"
	"strings"
	"time"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/action"
)

// IntentBlockerRef is one issue blocking an intent's issue, as the comment
// names it. Every field is maul's own: the controller derives them from
// GitHub's listing, never from text anyone wrote.
type IntentBlockerRef struct {
	// Repository is "" for the intent repository itself, "owner/name" for
	// one of the Project's repositories. A blocker in any other repository
	// is Hidden: counted, never named, so a private repository's name never
	// reaches a public issue.
	Repository string
	Number     int64
	// URL links the blocker; "" shows the reference as code.
	URL    string
	Hidden bool
	// State is "open" or "closed"; StateReason GitHub's state_reason.
	State       string
	StateReason string
}

// IntentDependencyStatus is the dependency section of an intent's status
// comment (intent dependencies): while it waits, what it waits for and how
// to end the wait; once it started, one line of history.
type IntentDependencyStatus struct {
	// Waiting reports the intent still waits; Reason is the
	// WaitingOnDependencies reason (v1alpha1.Reason*), True ones while it
	// waits and the release reason once it started.
	Waiting bool
	Reason  string
	// Blockers are the blockers not yet closed as completed, as last read
	// while waiting; More counts those past them, Unreadable those GitHub
	// counts but would not show.
	Blockers   []IntentBlockerRef
	More       int32
	Unreadable int32
	// Cycle are the intent repository's issues the cycle runs through, from
	// this issue's blocker back to this issue (exclusive), in order.
	Cycle []IntentBlockerRef
	// GiveUpAt is when a read GitHub keeps failing gives up and the intent
	// starts without its dependencies (CheckingDependencies).
	GiveUpAt time.Time
	// MaxActiveIntents is the Project's limit (NoActiveSlot).
	MaxActiveIntents int32
	// ReleasedAt is when the wait ended; StartedBy the approver whose
	// /maul start ended it.
	ReleasedAt time.Time
	StartedBy  string
	// Late are the open blockers added after the intent started, as the
	// one notice named them.
	Late []IntentBlockerRef
}

// dependencyReasonWords say, in a clause, why a read was degraded.
var dependencyReasonWords = map[string]string{
	v1alpha1.ReasonDependenciesUnsupported: "this GitHub does not offer issue dependencies",
	v1alpha1.ReasonDependenciesForbidden:   "the GitHub App may not read them",
	v1alpha1.ReasonDependenciesUnanswered:  "GitHub did not answer for 30 minutes",
}

// DependencyReasonWords is the clause naming why a degraded read could not
// honour the dependencies, "" for any other reason.
func DependencyReasonWords(reason string) string { return dependencyReasonWords[reason] }

// waitingSentence is the Pending phase's sentence while the intent waits,
// slotSentence while it waits only for an active slot.
const (
	waitingSentence = "waiting for the issues this one is blocked by."
	slotSentence    = "ready to start, and waiting for an active slot."
)

// blockerRef is a named blocker as Markdown: a link maul built, or code.
func blockerRef(b IntentBlockerRef) string {
	label := fmt.Sprintf("#%d", b.Number)
	if b.Repository != "" {
		label = oneLine(b.Repository) + label
	}
	if u := oneLine(b.URL); u != "" && !strings.ContainsAny(u, "()[]<> ") {
		return "[" + label + "](" + u + ")"
	}
	return code(label)
}

// blockerState says where a blocker stands.
func blockerState(b IntentBlockerRef) string {
	switch {
	case b.State == "open":
		return "open"
	case b.StateReason == "not_planned":
		return "closed as not planned, so what it was for never landed"
	case b.StateReason == "duplicate":
		return "closed as a duplicate, so what it was for never landed"
	}
	return "closed without being completed, so what it was for never landed"
}

// refList joins refs as prose: "#1", "#1 and #2", "#1, #2 and #3".
func refList(refs []string) string {
	switch len(refs) {
	case 0:
		return ""
	case 1:
		return refs[0]
	}
	return strings.Join(refs[:len(refs)-1], ", ") + " and " + refs[len(refs)-1]
}

// named are the refs of the blockers the comment may name.
func named(bs []IntentBlockerRef) []string {
	var out []string
	for _, b := range bs {
		if !b.Hidden {
			out = append(out, blockerRef(b))
		}
	}
	return out
}

// renderDependencies is the status comment's dependency section, "" when
// there is nothing to say: no blocker was ever waited for, overridden or
// left unread, and none was added late.
func renderDependencies(d *IntentDependencyStatus) string {
	if d == nil {
		return ""
	}
	if d.Waiting {
		return renderWaiting(d)
	}
	var lines []string
	if l := renderStarted(d); l != "" {
		lines = append(lines, l)
	}
	if late := named(d.Late); len(late) > 0 || hiddenCount(d.Late) > 0 {
		refs := late
		if n := hiddenCount(d.Late); n > 0 {
			refs = append(refs, issues(n)+" in another repository")
		}
		lines = append(lines, "**Blocked by, added after planning started:** "+refList(refs)+".")
	}
	return strings.Join(lines, "\n\n")
}

// renderStarted is the one history line once the intent started.
func renderStarted(d *IntentDependencyStatus) string {
	at := ""
	if !d.ReleasedAt.IsZero() {
		at = " (released " + d.ReleasedAt.UTC().Format("2006-01-02 15:04 UTC") + ")"
	}
	refs := named(d.Blockers)
	if n := hiddenCount(d.Blockers); n > 0 {
		refs = append(refs, issues(n)+" in another repository")
	}
	switch {
	case d.Reason == v1alpha1.ReasonStartedByApprover:
		line := "**Dependencies:** started by " + code(oneLine(d.StartedBy)) + " with " +
			slashCommand(action.VerbStart)
		if len(refs) > 0 {
			line += " while " + refList(refs) + " " + wasWere(len(d.Blockers)) + " still not closed as completed"
		}
		return line + "."
	case dependencyReasonWords[d.Reason] != "":
		return "**Dependencies:** maul could not read this issue's dependencies (" + dependencyReasonWords[d.Reason] +
			"), so it started without waiting for them."
	case d.Reason == v1alpha1.ReasonBlockersClosed && len(refs) > 0:
		return "**Dependencies:** waited for " + refList(refs) + at + "."
	}
	return ""
}

// renderWaiting is the section while the intent waits: the blockers, then
// what ends the wait.
func renderWaiting(d *IntentDependencyStatus) string {
	list := blockerList(d)
	advice := waitAdvice(d)
	if list == "" {
		return advice
	}
	return list + "\n\n" + advice
}

// blockerList is the "Blocked by" list, "" when there is nothing to list.
func blockerList(d *IntentDependencyStatus) string {
	if len(d.Blockers) == 0 && d.More == 0 && d.Unreadable == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("**Blocked by:**\n")
	for _, bl := range d.Blockers {
		if !bl.Hidden {
			b.WriteString("\n- " + blockerRef(bl) + ": " + blockerState(bl))
		}
	}
	open, other := hiddenSplit(d.Blockers)
	if open > 0 {
		b.WriteString("\n- " + issues(open) + " in another repository: open")
	}
	if other > 0 {
		b.WriteString("\n- " + issues(other) + " in another repository: closed without being completed")
	}
	if d.More > 0 {
		b.WriteString("\n- " + issues(int(d.More)) + " more")
	}
	if d.Unreadable > 0 {
		b.WriteString("\n- " + issues(int(d.Unreadable)) + " maul cannot read")
	}
	return b.String()
}

// waitAdvice says what ends the wait, by its reason.
func waitAdvice(d *IntentDependencyStatus) string {
	start := slashCommand(action.VerbStart)
	switch d.Reason {
	case v1alpha1.ReasonDependencyCycle:
		return cycleAdvice(d.Cycle, start)
	case v1alpha1.ReasonBlockerNotCompleted:
		var refs []string
		for _, bl := range d.Blockers {
			if bl.State != "open" && !bl.Hidden {
				refs = append(refs, blockerRef(bl))
			}
		}
		who := refList(refs)
		if who == "" {
			who = "A blocker"
		}
		return who + " " + wasWere(max(len(refs), 1)) + " closed without being completed. Remove it from " +
			"this issue's dependencies on GitHub if this intent no longer needs it, or comment " + start +
			" to start without it."
	case v1alpha1.ReasonBlockerUnreadable:
		return "GitHub counts blockers that maul's GitHub App cannot read, so maul cannot tell whether they " +
			"are closed. Install the App on their repositories, remove the dependencies, or comment " + start + "."
	case v1alpha1.ReasonCheckingDependencies:
		return "maul could not read this issue's dependencies from GitHub just now and will try again; if " +
			"GitHub still does not answer by " + d.GiveUpAt.UTC().Format("2006-01-02 15:04 UTC") +
			", it starts without them."
	case v1alpha1.ReasonNoActiveSlot:
		return fmt.Sprintf("Nothing holds this intent back any more but the Project's limit of %d active "+
			"intents: it starts when one of them finishes, oldest waiting intent first.", d.MaxActiveIntents)
	}
	return "maul starts planning once every one of them is closed as completed. To start now anyway, " +
		"an approver comments " + start + "."
}

// cycleAdvice names a dependency cycle: the issues it runs through, this
// one last, and how to break it.
func cycleAdvice(cycle []IntentBlockerRef, start string) string {
	refs := named(cycle)
	switch {
	case len(cycle) == 0:
		return "**Dependency cycle:** this issue is listed as blocking itself, so it cannot start. " +
			"Remove that dependency on GitHub, or comment " + start + "."
	case len(refs) <= 1:
		who := "an issue"
		if len(refs) == 1 {
			who = refs[0]
		}
		return "**Dependency cycle:** " + who + " is itself waiting for this issue, so neither can start. " +
			"Remove one of the dependencies on GitHub, or comment " + start + " on one of the two issues."
	}
	return "**Dependency cycle:** " + strings.Join(append(refs, "this issue"), " → ") +
		", so none of them can start. Remove one of the dependencies on GitHub, or comment " + start +
		" on one of the issues."
}

func hiddenCount(bs []IntentBlockerRef) int {
	open, other := hiddenSplit(bs)
	return open + other
}

func hiddenSplit(bs []IntentBlockerRef) (open, other int) {
	for _, b := range bs {
		switch {
		case !b.Hidden:
		case b.State == "open":
			open++
		default:
			other++
		}
	}
	return open, other
}

// issues counts issues in words: "1 issue", "3 issues".
func issues(n int) string {
	if n == 1 {
		return "1 issue"
	}
	return fmt.Sprintf("%d issues", n)
}

func wasWere(n int) string {
	if n == 1 {
		return "was"
	}
	return "were"
}

// LateDependenciesNotice tells the issue, once, that it became blocked after
// maul started work on it.
type LateDependenciesNotice struct {
	// Namespace, Intent and Key make the notice's marker (NoticeMarker).
	Namespace string
	Intent    string
	Key       string
	// Blockers are the open blockers added after the start.
	Blockers []IntentBlockerRef
}

// RenderLateDependenciesNotice renders a LateDependenciesNotice.
func RenderLateDependenciesNotice(n LateDependenciesNotice) (string, error) {
	refs := named(n.Blockers)
	if h := hiddenCount(n.Blockers); h > 0 {
		refs = append(refs, issues(h)+" in another repository")
	}
	if len(refs) == 0 {
		return "", fmt.Errorf("render late dependencies notice: no blocker")
	}
	return render("intent_notice_dependencies_late.md.tmpl", struct {
		Marker   string
		Blockers string
		Cancel   string
		Replan   string
	}{
		Marker:   NoticeMarker(n.Namespace, n.Intent, n.Key),
		Blockers: refList(refs),
		Cancel:   slashCommand(action.VerbCancel),
		Replan:   slashCommand(action.VerbReplan),
	})
}
