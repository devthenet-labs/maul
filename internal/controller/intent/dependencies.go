// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/action"
	"github.com/devthenet-labs/maul/internal/ghclient"
	"github.com/devthenet-labs/maul/internal/templates"
)

// Intent dependencies. An intent whose issue GitHub records as blocked by
// other issues does not start planning until every blocker is closed as
// completed, so its plan is written against code that holds the dependency.
// The wait is a condition on Pending (WaitingOnDependencies) and
// status.dependencies, never a phase: Pending already means nothing was
// launched. Only the first start is gated; a replan or a revival is an
// approver's explicit action on an intent that already started.

// DefaultDependencyGiveUp is how long a dependency read GitHub keeps
// failing holds an intent before it starts without its dependencies.
const DefaultDependencyGiveUp = 30 * time.Minute

// waitingPollFactor slows a waiting intent's poll of its issue to this many
// poll intervals: an intent that is an intent's blocker releases it at once
// through the end-of-intent nudge (SetupWithManager), so the poll is only for
// blockers closed by hand.
const waitingPollFactor = 5

// lateNoticeKey keys the one notice of a blocker added after the start.
const lateNoticeKey = "dependencies-late"

// depReadKind is how one read of an issue's blockers went.
type depReadKind int

const (
	// depReadOK: GitHub answered with the listing.
	depReadOK depReadKind = iota
	// depReadTransient: no answer that decides anything (a 5xx, a timeout, a
	// secondary rate limit, anything unclassified): the intent waits, up to
	// DependencyGiveUp from the first failure.
	depReadTransient
	// depReadUnsupported: 404 or 410, a GitHub without issue dependencies.
	depReadUnsupported
	// depReadForbidden: a 403 that is no rate limit, the App may not read
	// them.
	depReadForbidden
)

// depRead is one read of the intent issue's blockers.
type depRead struct {
	kind depReadKind
	// blockers are every blocker listed, open and closed.
	blockers []v1alpha1.IntentBlocker
	// unreadable counts blockers GitHub summarises but would not list.
	unreadable int32
}

// depVerdict is what a read decides about the start.
type depVerdict struct {
	// release: the wait ends (reason a False reason); otherwise the intent
	// waits (reason a True reason).
	release bool
	reason  string
	// pending are the blockers not yet closed as completed, open first, at
	// most MaxIntentBlockers; more counts the rest.
	pending    []v1alpha1.IntentBlocker
	more       int32
	unreadable int32
	// unavailableSince is the failing streak's start to record; nil once a
	// read decided something.
	unavailableSince *time.Time
	// failed reports a transient read: what was last recorded still stands.
	failed bool
}

// completedBlocker reports a blocker that no longer holds the start: closed
// as completed, or closed with no reason at all (an issue closed before
// GitHub recorded reasons).
func completedBlocker(b v1alpha1.IntentBlocker) bool {
	return b.State == "closed" && (b.StateReason == "completed" || b.StateReason == "")
}

// pendingBlockers are the blockers not closed as completed, open first and
// then in the listing's order, capped at MaxIntentBlockers with the rest
// counted.
func pendingBlockers(all []v1alpha1.IntentBlocker) ([]v1alpha1.IntentBlocker, int32) {
	var out []v1alpha1.IntentBlocker
	for _, b := range all {
		if !completedBlocker(b) {
			out = append(out, b)
		}
	}
	slices.SortStableFunc(out, func(a, b v1alpha1.IntentBlocker) int {
		return cmp.Compare(boolRank(a.State != "open"), boolRank(b.State != "open"))
	})
	if len(out) > v1alpha1.MaxIntentBlockers {
		return out[:v1alpha1.MaxIntentBlockers], int32(min(len(out)-v1alpha1.MaxIntentBlockers, 999))
	}
	return out, 0
}

func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}

// judgeDependencies decides the start from one read: release once nothing
// holds it, wait otherwise. since is the recorded start of a failing streak,
// giveUp the bound past which a failing read starts the intent anyway, and
// cycle reports that the open blockers lead back to the intent through
// intents of its Project (a wording, which outranks OpenBlockers). Pure.
func judgeDependencies(read depRead, since *time.Time, now time.Time, giveUp time.Duration, cycle bool) depVerdict {
	switch read.kind {
	case depReadUnsupported:
		return depVerdict{release: true, reason: v1alpha1.ReasonDependenciesUnsupported}
	case depReadForbidden:
		return depVerdict{release: true, reason: v1alpha1.ReasonDependenciesForbidden}
	case depReadTransient:
		first := now
		if since != nil && !since.After(now) {
			first = *since
		}
		if now.Sub(first) >= giveUp {
			return depVerdict{release: true, reason: v1alpha1.ReasonDependenciesUnanswered}
		}
		return depVerdict{reason: v1alpha1.ReasonCheckingDependencies, unavailableSince: &first, failed: true}
	}
	pending, more := pendingBlockers(read.blockers)
	v := depVerdict{pending: pending, more: more, unreadable: read.unreadable}
	open, notCompleted := false, false
	for _, b := range pending {
		if b.State == "open" {
			open = true
		} else {
			notCompleted = true
		}
	}
	switch {
	case len(pending) == 0 && read.unreadable == 0:
		v.release, v.reason = true, v1alpha1.ReasonBlockersClosed
		if len(read.blockers) == 0 {
			v.reason = v1alpha1.ReasonNoBlockers
		}
	case open && cycle:
		v.reason = v1alpha1.ReasonDependencyCycle
	case open:
		v.reason = v1alpha1.ReasonOpenBlockers
	case notCompleted:
		v.reason = v1alpha1.ReasonBlockerNotCompleted
	default:
		v.reason = v1alpha1.ReasonBlockerUnreadable
	}
	return v
}

// findCycle is the path from self's blockers back to self through edges (an
// issue to the open issues blocking it, each an intent of the Project): the
// issues the cycle runs through in order, self last; nil when none leads
// back. Neighbours are taken in ascending order, so the path is stable. Pure.
func findCycle(self int64, edges map[int64][]int64) []int64 {
	seen := map[int64]bool{}
	var walk func(n int64, path []int64) []int64
	walk = func(n int64, path []int64) []int64 {
		next := slices.Clone(edges[n])
		slices.Sort(next)
		for _, m := range next {
			if m == self {
				return append(slices.Clone(path), self)
			}
			if seen[m] {
				continue
			}
			seen[m] = true
			if p := walk(m, append(path, m)); p != nil {
				return p
			}
		}
		return nil
	}
	return walk(self, nil)
}

// waitReason is an Intent's WaitingOnDependencies reason while it is True,
// "" otherwise.
func waitReason(in *v1alpha1.Intent) string {
	c := meta.FindStatusCondition(in.Status.Conditions, v1alpha1.ConditionWaitingOnDependencies)
	if c == nil || c.Status != metav1.ConditionTrue {
		return ""
	}
	return c.Reason
}

// holdsActiveSlot reports an Intent counted against its Project's
// maxActiveIntents: not ended, not being deleted, and not waiting on its
// blockers. One released but waiting for a slot (NoActiveSlot) holds one,
// so discovery cannot create new intents ahead of it.
func holdsActiveSlot(in *v1alpha1.Intent) bool {
	if !in.DeletionTimestamp.IsZero() || terminal(in.Status.Phase) {
		return false
	}
	r := waitReason(in)
	return in.Status.Phase != v1alpha1.IntentPending || r == "" || r == v1alpha1.ReasonNoActiveSlot
}

// olderRequest orders two Intents by their trigger, then their name.
func olderRequest(a, b *v1alpha1.Intent) bool {
	if !a.Spec.RequestedBy.At.Equal(&b.Spec.RequestedBy.At) {
		return a.Spec.RequestedBy.At.Before(&b.Spec.RequestedBy.At)
	}
	return a.Name < b.Name
}

// slotFree reports whether self, released, may start now among intents
// (its Project's, read uncached): the others that started and have not ended,
// plus the released waiters older than self, which go first, number fewer
// than limit. An intent still to decide its own start is not counted: it
// counts the others when it does, and decisions are made one at a time. Pure.
func slotFree(self *v1alpha1.Intent, intents []v1alpha1.Intent, limit int32) bool {
	var ahead int32
	for i := range intents {
		in := &intents[i]
		if in.Name == self.Name || in.Spec.Project != self.Spec.Project || !in.DeletionTimestamp.IsZero() ||
			terminal(in.Status.Phase) {
			continue
		}
		switch {
		case in.Status.Phase != v1alpha1.IntentPending:
			ahead++
		case waitReason(in) == v1alpha1.ReasonNoActiveSlot && olderRequest(in, self):
			ahead++
		}
	}
	return ahead < limit
}

// depMemo is what the reconciler keeps in memory of an intent's last
// blocker listing: its tag and blockers, for a conditional read. A restart
// lists once more, which costs a request and nothing else.
type depMemo struct {
	etag     string
	blockers []v1alpha1.IntentBlocker
}

// waitingPoll reports a Pending intent that waits on its dependencies: its
// issue is polled every waitingPollFactor poll intervals.
func (p *pass) waitingPoll() bool {
	return !p.set.IgnoreDependencies && p.in.Waiting()
}

// pendingOnDependencies is pending with dependencies honoured (the default):
// the trigger's authority first, as without them (a refused trigger never
// waits); then, on a poll, an approver's /maul start or the blockers decide,
// and a released intent starts once its Project has a slot for it.
func (p *pass) pendingOnDependencies(ctx context.Context) (bool, error) {
	deps := p.in.Status.Dependencies
	first := deps == nil || deps.Since == nil
	if !p.polled && !first {
		// A waiting intent reads its blockers again on its next poll; only
		// the first decision is made whenever a pass comes, as without
		// dependencies.
		return false, nil
	}
	if first {
		accepted, err := p.decideTrigger(ctx)
		if err != nil || !accepted {
			return true, err
		}
	}
	if p.issue == nil {
		// The summary counts what the listing may not show.
		issue, err := p.r.GitHub.GetIssue(ctx, p.repo(), p.number())
		if err != nil {
			return false, fmt.Errorf("read the issue: %w", err)
		}
		p.issue = issue
	}
	if deps != nil && deps.StartedBy != nil {
		return p.releaseDependencies(ctx, depVerdict{release: true, reason: v1alpha1.ReasonStartedByApprover})
	}
	var since *time.Time
	if deps != nil && deps.UnavailableSince != nil {
		t := deps.UnavailableSince.Time
		since = &t
	}
	read, err := p.readBlockers(ctx)
	if err != nil {
		return false, err
	}
	pending, _ := pendingBlockers(read.blockers)
	cycle := read.kind == depReadOK && len(p.dependencyCycle(ctx, pending)) > 0
	v := judgeDependencies(read, since, p.now, p.set.DependencyGiveUp, cycle)
	if !v.release {
		return p.recordWait(ctx, v)
	}
	return p.releaseDependencies(ctx, v)
}

// readBlockers reads the intent issue's blockers, conditional on the last
// listing's tag. Only a failure of the pass itself (its context) is an
// error; GitHub's answers, failures included, are classified into the read.
func (p *pass) readBlockers(ctx context.Context) (depRead, error) {
	var memo depMemo
	p.r.memo(func() { memo = p.r.deps[p.in.Name] })
	list, err := p.r.GitHub.ListBlockedBy(ctx, p.repo(), p.number(), memo.etag)
	switch {
	case err == nil:
	case ctx.Err() != nil:
		return depRead{}, ctx.Err()
	case ghclient.IsNotFound(err) || ghclient.IsGone(err):
		p.logDependencyRead(ctx, "issue dependencies are not offered; the intent starts without them", err)
		return depRead{kind: depReadUnsupported}, nil
	case ghclient.IsForbidden(err):
		p.logDependencyRead(ctx, "issue dependencies may not be read; the intent starts without them", err)
		return depRead{kind: depReadForbidden}, nil
	default:
		p.logDependencyRead(ctx, "issue dependencies could not be read; the intent waits and reads them again", err)
		return depRead{kind: depReadTransient}, nil
	}
	blockers := memo.blockers
	if !list.NotModified {
		blockers = p.intentBlockers(list.Blockers)
		p.r.memo(func() { p.r.deps[p.in.Name] = depMemo{etag: list.ETag, blockers: blockers} })
	}
	read := depRead{kind: depReadOK, blockers: blockers}
	if is := p.issue; is != nil && is.Dependencies != nil {
		// What GitHub counts but would not list: a blocker in a repository
		// this token may not read.
		read.unreadable = int32(min(max(is.Dependencies.TotalBlockedBy-len(list.Blockers), 0), 999))
		if list.NotModified {
			read.unreadable = int32(min(max(is.Dependencies.TotalBlockedBy-len(blockers), 0), 999))
		}
	}
	return read, nil
}

func (p *pass) logDependencyRead(ctx context.Context, msg string, err error) {
	p.r.log().LogAttrs(ctx, slog.LevelWarn, msg, slog.String("intent", p.in.Name), slog.Any("error", err))
}

// intentBlockers maps GitHub's listing onto recorded blockers: the
// repository as an https URL on the intent repository's host (a token reads
// only its own GitHub), an entry of any other shape left out (and so counted
// unreadable by the summary).
func (p *pass) intentBlockers(listed []ghclient.Blocker) []v1alpha1.IntentBlocker {
	host := "https://github.com"
	if u, err := url.Parse(p.repo()); err == nil && u.Host != "" {
		host = "https://" + u.Host
	}
	out := make([]v1alpha1.IntentBlocker, 0, len(listed))
	for _, b := range listed {
		if b.Number < 1 || !validRepoPart(b.Repo.Owner) || !validRepoPart(b.Repo.Name) {
			continue
		}
		state := "closed"
		if b.State == "open" {
			state = "open"
		}
		reason := b.StateReason
		if len(reason) > 32 || strings.Trim(reason, "abcdefghijklmnopqrstuvwxyz_") != "" {
			reason = "unknown"
		}
		out = append(out, v1alpha1.IntentBlocker{
			Repository: host + "/" + b.Repo.Owner + "/" + b.Repo.Name, Number: int64(b.Number),
			State: state, StateReason: reason,
		})
	}
	return out
}

// validRepoPart reports an owner or repository name the blocker schema
// admits.
func validRepoPart(s string) bool {
	return s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_.-") == ""
}

// dependencyCycle is the cycle pending's open blockers in the intent
// repository close through the Project's other waiting intents, by their
// recorded blockers: the issues in order, this one last; nil when none. It
// reads the Intents from the cache: a wording, never a decision.
func (p *pass) dependencyCycle(ctx context.Context, pending []v1alpha1.IntentBlocker) []int64 {
	edges := map[int64][]int64{p.number(): openIntentRepoBlockers(p.repo(), pending)}
	if len(edges[p.number()]) == 0 {
		return nil
	}
	var intents v1alpha1.IntentList
	if err := p.r.List(ctx, &intents, client.InNamespace(p.in.Namespace)); err != nil {
		return nil
	}
	for i := range intents.Items {
		in := &intents.Items[i]
		if in.Name == p.in.Name || in.Spec.Project != p.in.Spec.Project || !sameRepo(in.Spec.Issue.Repository, p.repo()) ||
			in.Status.Phase != v1alpha1.IntentPending || in.Status.Dependencies == nil {
			continue
		}
		edges[in.Spec.Issue.Number] = openIntentRepoBlockers(p.repo(), in.Status.Dependencies.Blockers)
	}
	return findCycle(p.number(), edges)
}

// openIntentRepoBlockers are the numbers of bs's open blockers in repo.
func openIntentRepoBlockers(repo string, bs []v1alpha1.IntentBlocker) []int64 {
	var out []int64
	for _, b := range bs {
		if b.State == "open" && sameRepo(b.Repository, repo) {
			out = append(out, b.Number)
		}
	}
	return out
}

// recordWait records a wait, in one status write, only when it changed:
// the blockers, their counts, the reason or the failing streak. changed
// reports a write, which ends the pass.
func (p *pass) recordWait(ctx context.Context, v depVerdict) (bool, error) {
	next := v1alpha1.IntentDependencies{}
	if cur := p.in.Status.Dependencies; cur != nil {
		next = *cur.DeepCopy()
	}
	if next.Since == nil {
		at := metav1.NewTime(p.now)
		next.Since = &at
	}
	if !v.failed && v.reason != v1alpha1.ReasonNoActiveSlot {
		next.Blockers, next.More, next.Unreadable = v.pending, v.more, v.unreadable
	}
	next.UnavailableSince = nil
	if v.unavailableSince != nil {
		at := metav1.NewTime(*v.unavailableSince)
		next.UnavailableSince = &at
	}
	msg := p.waitMessage(v.reason, &next)
	cond := meta.FindStatusCondition(p.in.Status.Conditions, v1alpha1.ConditionWaitingOnDependencies)
	if cur := p.in.Status.Dependencies; cur != nil && equalDependencies(cur, &next) && cond != nil &&
		cond.Status == metav1.ConditionTrue && cond.Reason == v.reason && cond.Message == msg {
		return false, nil
	}
	if cond == nil || cond.Reason != v.reason {
		p.r.log().LogAttrs(ctx, slog.LevelInfo, "intent waits on its dependencies",
			slog.String("intent", p.in.Name), slog.String("reason", v.reason))
	}
	return true, p.update(ctx, func(cur *v1alpha1.Intent) error {
		cur.Status.Dependencies = &next
		p.setWaitCondition(cur, metav1.ConditionTrue, v.reason, msg)
		return nil
	})
}

// equalDependencies compares two records as the API server stores them.
func equalDependencies(a, b *v1alpha1.IntentDependencies) bool {
	return equality.Semantic.DeepEqual(a, b)
}

// setWaitCondition sets the WaitingOnDependencies condition, timed by the
// pass's clock: the Project reads the newest decision by it.
func (p *pass) setWaitCondition(cur *v1alpha1.Intent, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&cur.Status.Conditions, metav1.Condition{
		Type: v1alpha1.ConditionWaitingOnDependencies, Status: status, Reason: reason, Message: msg,
		ObservedGeneration: cur.Generation, LastTransitionTime: metav1.NewTime(p.now),
	})
}

// waitMessage is the WaitingOnDependencies message while the intent waits.
func (p *pass) waitMessage(reason string, d *v1alpha1.IntentDependencies) string {
	open := 0
	for _, b := range d.Blockers {
		if b.State == "open" {
			open++
		}
	}
	switch reason {
	case v1alpha1.ReasonOpenBlockers, v1alpha1.ReasonDependencyCycle:
		msg := fmt.Sprintf("waiting for %d open issue(s) this issue is blocked by", open+int(d.More))
		if reason == v1alpha1.ReasonDependencyCycle {
			msg += ", in a dependency cycle through other intents of the Project"
		}
		return msg
	case v1alpha1.ReasonBlockerNotCompleted:
		return "a blocker was closed without being completed; remove the dependency or comment /maul start"
	case v1alpha1.ReasonBlockerUnreadable:
		return fmt.Sprintf("GitHub counts %d blocker(s) maul's App cannot read", d.Unreadable)
	case v1alpha1.ReasonCheckingDependencies:
		return fmt.Sprintf("the blockers could not be read; the intent starts without them at %s if GitHub "+
			"still does not answer", d.UnavailableSince.Add(p.set.DependencyGiveUp).UTC().Format(time.RFC3339))
	case v1alpha1.ReasonNoActiveSlot:
		return fmt.Sprintf("ready to start; waiting for one of the Project's %d active intents to finish",
			maxActiveIntents(p.proj))
	}
	return reason
}

// releaseMessages are the WaitingOnDependencies messages once the start was
// decided.
var releaseMessages = map[string]string{
	v1alpha1.ReasonNoBlockers:              "the issue is blocked by nothing",
	v1alpha1.ReasonBlockersClosed:          "every blocker was closed as completed",
	v1alpha1.ReasonStartedByApprover:       "an approver started the intent with /maul start",
	v1alpha1.ReasonDependenciesUnsupported: "this GitHub does not offer issue dependencies; started without them",
	v1alpha1.ReasonDependenciesForbidden:   "the GitHub App may not read issue dependencies; started without them",
	v1alpha1.ReasonDependenciesUnanswered: "GitHub did not answer the dependency read for the whole give-up " +
		"bound; started without them",
}

// releaseDependencies starts a released intent once the Project has a slot
// for it (slotFree, over an uncached list), and otherwise records it waiting
// for one (NoActiveSlot). The start is one status write with the release.
func (p *pass) releaseDependencies(ctx context.Context, v depVerdict) (bool, error) {
	var intents v1alpha1.IntentList
	if err := p.r.APIReader.List(ctx, &intents, client.InNamespace(p.in.Namespace)); err != nil {
		return false, err
	}
	if !slotFree(p.in, intents.Items, maxActiveIntents(p.proj)) {
		return p.recordWait(ctx, depVerdict{reason: v1alpha1.ReasonNoActiveSlot})
	}
	reason := v.reason
	if waitReason(p.in) == v1alpha1.ReasonNoActiveSlot && reason != v1alpha1.ReasonStartedByApprover {
		// The verdict that put it in the queue stands: the read now is the
		// same release, and a NoBlockers wording would hide the wait.
		if c := p.releasedBy(); c != "" {
			reason = c
		}
	}
	p.r.log().LogAttrs(ctx, slog.LevelInfo, "intent released from its dependencies",
		slog.String("intent", p.in.Name), slog.String("reason", reason))
	return true, p.startPlanning(ctx, p.issue, nil, func(cur *v1alpha1.Intent) {
		next := v1alpha1.IntentDependencies{}
		if d := cur.Status.Dependencies; d != nil {
			next = *d.DeepCopy()
		}
		now := metav1.NewTime(p.now)
		if next.Since == nil {
			next.Since = &now
		}
		next.ReleasedAt, next.UnavailableSince = &now, nil
		cur.Status.Dependencies = &next
		p.setWaitCondition(cur, metav1.ConditionFalse, reason, releaseMessages[reason])
	})
}

// releasedBy is the release reason a queued intent's recorded wait implies:
// BlockersClosed after a wait on blockers, "" otherwise.
func (p *pass) releasedBy() string {
	if d := p.in.Status.Dependencies; d != nil && (len(d.Blockers) > 0 || d.More > 0) {
		return v1alpha1.ReasonBlockersClosed
	}
	return ""
}

// noticeLateBlockers tells the issue, once, that it became blocked after the
// intent started: the start is never undone (design D8), so its plan and
// pull requests will not hold the blocker's work. It runs only for an
// intent whose start was decided with dependencies honoured, only on a poll
// whose issue summary shows an open blocker, and never fails the pass: a
// read GitHub refuses is advice lost, nothing more. changed reports a
// status write.
func (p *pass) noticeLateBlockers(ctx context.Context) (bool, error) {
	deps := p.in.Status.Dependencies
	if p.set.IgnoreDependencies || deps == nil || deps.ReleasedAt == nil || deps.LateNotice != nil ||
		p.issue == nil || p.issue.Dependencies == nil || p.issue.Dependencies.BlockedBy < 1 ||
		p.in.Status.Phase == v1alpha1.IntentPending || terminal(p.in.Status.Phase) {
		return false, nil
	}
	read, err := p.readBlockers(ctx)
	if err != nil || read.kind != depReadOK {
		return false, err
	}
	var late []v1alpha1.IntentBlocker
	for _, b := range read.blockers {
		if b.State == "open" {
			late = append(late, b)
		}
	}
	if len(late) == 0 {
		return false, nil
	}
	if len(late) > v1alpha1.MaxIntentBlockers {
		late = late[:v1alpha1.MaxIntentBlockers]
	}
	body, err := templates.RenderLateDependenciesNotice(templates.LateDependenciesNotice{
		Namespace: p.in.Namespace, Intent: p.in.Name, Key: lateNoticeKey, Blockers: p.blockerRefs(late),
	})
	if err := p.notice(ctx, lateNoticeKey, deps.ReleasedAt.Add(-clockSkew), body, err); err != nil {
		return false, err
	}
	return true, p.update(ctx, func(cur *v1alpha1.Intent) error {
		if cur.Status.Dependencies == nil {
			return nil
		}
		at := metav1.NewTime(p.now)
		cur.Status.Dependencies.LateNotice, cur.Status.Dependencies.Late = &at, late
		return nil
	})
}

// blockerRefs are blockers as a comment may show them: the intent
// repository's as "#N", a Project repository's as "owner/name#N", each
// linked; any other repository's hidden, so its name never reaches the issue.
func (p *pass) blockerRefs(bs []v1alpha1.IntentBlocker) []templates.IntentBlockerRef {
	out := make([]templates.IntentBlockerRef, 0, len(bs))
	for _, b := range bs {
		ref := templates.IntentBlockerRef{Number: b.Number, State: b.State, StateReason: b.StateReason,
			URL: fmt.Sprintf("%s/issues/%d", b.Repository, b.Number)}
		switch {
		case sameRepo(b.Repository, p.repo()):
		case p.isProjectRepository(b.Repository):
			ref.Repository = repoSlug(b.Repository)
		default:
			ref = templates.IntentBlockerRef{Hidden: true, State: b.State, StateReason: b.StateReason}
		}
		out = append(out, ref)
	}
	return out
}

// isProjectRepository reports one of the Project's repositories.
func (p *pass) isProjectRepository(u string) bool {
	for _, r := range p.proj.Spec.Repositories {
		if sameRepo(r.URL, u) {
			return true
		}
	}
	return false
}

// statusDependencies is the status comment's dependency section, nil when
// the intent's start was never decided with dependencies honoured.
func (p *pass) statusDependencies(ctx context.Context) *templates.IntentDependencyStatus {
	d := p.in.Status.Dependencies
	if d == nil || d.Since == nil {
		return nil
	}
	cond := meta.FindStatusCondition(p.in.Status.Conditions, v1alpha1.ConditionWaitingOnDependencies)
	s := &templates.IntentDependencyStatus{
		Blockers: p.blockerRefs(d.Blockers), More: d.More, Unreadable: d.Unreadable,
		MaxActiveIntents: maxActiveIntents(p.proj), Late: p.blockerRefs(d.Late),
	}
	if cond != nil {
		s.Reason = cond.Reason
	}
	s.Waiting = p.in.Status.Phase == v1alpha1.IntentPending && cond != nil && cond.Status == metav1.ConditionTrue
	if d.UnavailableSince != nil {
		s.GiveUpAt = d.UnavailableSince.Add(p.set.DependencyGiveUp)
	}
	if d.ReleasedAt != nil {
		s.ReleasedAt = d.ReleasedAt.Time
	}
	if d.StartedBy != nil {
		s.StartedBy = d.StartedBy.Login
	}
	if s.Waiting && s.Reason == v1alpha1.ReasonDependencyCycle {
		pending := d.Blockers
		cycle := p.dependencyCycle(ctx, pending)
		for _, n := range cycle {
			if n == p.number() {
				continue
			}
			s.Cycle = append(s.Cycle, templates.IntentBlockerRef{Number: n, State: "open",
				URL: fmt.Sprintf("%s/issues/%d", strings.TrimRight(p.repo(), "/"), n)})
		}
	}
	return s
}

// availableVerbs are the commands the intent issue admits now: verbs, with
// start first while the intent waits on its dependencies.
func (p *pass) availableVerbs(phase v1alpha1.IntentPhase) []string {
	out := verbs(phase, p.set.Verification)
	if phase == v1alpha1.IntentPending && p.waitingPoll() {
		out = append([]string{action.VerbStart}, out...)
	}
	return out
}

// setDependenciesUnavailable sets cur's DependenciesUnavailable condition
// from the Project's Intents, which the project reconciler lists every pass
// anyway: True with DependenciesOff while the flag is off; else the newest
// start decided with dependencies honoured says whether they were read (by
// its WaitingOnDependencies condition's transition time). A Project none of
// whose intents decided a start keeps its last word.
func setDependenciesUnavailable(cur *v1alpha1.Project, intents *v1alpha1.IntentList, s Settings) {
	cond := metav1.Condition{Type: v1alpha1.ConditionDependenciesUnavailable, ObservedGeneration: cur.Generation}
	if s.IgnoreDependencies {
		cond.Status, cond.Reason = metav1.ConditionTrue, v1alpha1.ReasonDependenciesOff
		cond.Message = "intent-controller runs with --intent-dependencies=false: intents start without waiting " +
			"for the issues they are blocked by"
		meta.SetStatusCondition(&cur.Status.Conditions, cond)
		return
	}
	var newest *metav1.Condition
	var from string
	for i := range intents.Items {
		in := &intents.Items[i]
		if in.Spec.Project != cur.Name {
			continue
		}
		c := meta.FindStatusCondition(in.Status.Conditions, v1alpha1.ConditionWaitingOnDependencies)
		if c == nil || c.Status != metav1.ConditionFalse {
			continue
		}
		if newest == nil || newest.LastTransitionTime.Before(&c.LastTransitionTime) ||
			newest.LastTransitionTime.Equal(&c.LastTransitionTime) && in.Name > from {
			newest, from = c, in.Name
		}
	}
	if newest == nil {
		return
	}
	if words := templates.DependencyReasonWords(newest.Reason); words != "" {
		cond.Status, cond.Reason = metav1.ConditionTrue, newest.Reason
		cond.Message = fmt.Sprintf("issue dependencies could not be read for intent %s (%s): intents start "+
			"without waiting for them", from, words)
	} else {
		cond.Status, cond.Reason = metav1.ConditionFalse, v1alpha1.ReasonDependenciesRead
		cond.Message = "issue dependencies are read and honoured"
	}
	meta.SetStatusCondition(&cur.Status.Conditions, cond)
}

// nudgeWaiting re-polls, at once, every Pending intent of an ended intent's
// Project: an intent that blocked them may just have closed its issue, and
// it freed a slot either way. Its poll memo is cleared, so the next pass
// reads GitHub, and each is enqueued.
func (r *IntentReconciler) nudgeWaiting(ctx context.Context, c client.Reader, ended client.Object) []ctrl.Request {
	in, ok := ended.(*v1alpha1.Intent)
	if !ok {
		return nil
	}
	var intents v1alpha1.IntentList
	if err := c.List(ctx, &intents, client.InNamespace(in.Namespace),
		client.MatchingFields{projectIndex: in.Spec.Project}); err != nil {
		return nil
	}
	var out []ctrl.Request
	for i := range intents.Items {
		w := &intents.Items[i]
		if w.Name == in.Name || w.Status.Phase != v1alpha1.IntentPending {
			continue
		}
		r.memo(func() { delete(r.polled, w.Name) })
		out = append(out, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(w)})
	}
	return out
}

// endedIntentHandler maps an Intent's ending (a non-terminal phase becoming
// terminal, or a non-terminal Intent deleted) onto nudgeWaiting.
func (r *IntentReconciler) endedIntentHandler(c client.Reader) handler.EventHandler {
	return handler.Funcs{
		UpdateFunc: func(ctx context.Context, e event.UpdateEvent, q workqueue.TypedRateLimitingInterface[ctrl.Request]) {
			o, okOld := e.ObjectOld.(*v1alpha1.Intent)
			n, okNew := e.ObjectNew.(*v1alpha1.Intent)
			if !okOld || !okNew || terminal(o.Status.Phase) || !terminal(n.Status.Phase) {
				return
			}
			for _, req := range r.nudgeWaiting(ctx, c, n) {
				q.Add(req)
			}
		},
		DeleteFunc: func(ctx context.Context, e event.DeleteEvent, q workqueue.TypedRateLimitingInterface[ctrl.Request]) {
			if in, ok := e.Object.(*v1alpha1.Intent); ok && !terminal(in.Status.Phase) {
				for _, req := range r.nudgeWaiting(ctx, c, in) {
					q.Add(req)
				}
			}
		},
	}
}
