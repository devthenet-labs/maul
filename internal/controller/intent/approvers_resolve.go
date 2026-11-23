// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/ghclient"
	"github.com/devthenet-labs/maul/internal/untrusted"
)

// ApproversResolved reasons (v1alpha1.ConditionApproversResolved).
const (
	// ReasonApproversResolved: GitHub answered for every approver login at
	// status.approvers.resolvedAt.
	ReasonApproversResolved = "Resolved"
	// ReasonApproversUnreadable: GitHub could not be asked for one of them;
	// status.approvers is kept as it was, and ages.
	ReasonApproversUnreadable = "GitHubUnreadable"
)

// approversReresolve is how often the approvers of an unchanged list are
// asked of GitHub again: what bounds how long a lost write access is still
// believed. The settings page refuses identities older than three times
// this (its own ApproversMaxAge).
const approversReresolve = 10 * time.Minute

// maxConditionError bounds the GitHub error an ApproversResolved message
// quotes.
const maxConditionError = 300

// reconcileApprovers resolves p's approvers into cur's status.approvers and
// ApproversResolved condition when a resolution is due (approversDue). On a
// failure the identities and resolvedAt are left exactly as they were and
// the condition is False; the failure is never this pass's error, and is
// retried once per poll interval. It is never part of Ready, and it runs
// whether or not the Project is Ready: a Project held for its settings is
// the one the settings page must still be able to fix.
func (r *ProjectReconciler) reconcileApprovers(ctx context.Context, p, cur *v1alpha1.Project, poll *projectPoll,
	now time.Time, interval time.Duration) {
	logins := approverLogins(p)
	if !approversDue(p.Status.Approvers, logins, now) {
		return
	}
	if !poll.approversFailedAt.IsZero() && now.Sub(poll.approversFailedAt) < interval &&
		slices.Equal(poll.approversFailedFor, logins) {
		// Not asked again yet, but still unresolved: the failed pass may
		// have written no status at all (discovery failing with it).
		setApproversUnreadable(cur, p.Generation, poll.approversFailure)
		return
	}
	ids, err := resolveApprovers(ctx, r.GitHub, p.Spec.IntentRepository, logins)
	if err != nil {
		poll.approversFailedAt, poll.approversFailedFor = now, logins
		poll.approversFailure = untrusted.Visible(err.Error(), maxConditionError)
		r.log().LogAttrs(ctx, slog.LevelWarn, "approvers unresolved; keeping the identities resolved before",
			slog.String("project", p.Name), slog.String("error", err.Error()))
		setApproversUnreadable(cur, p.Generation, poll.approversFailure)
		return
	}
	poll.approversFailedAt, poll.approversFailedFor, poll.approversFailure = time.Time{}, nil, ""
	at := metav1.NewTime(now)
	cur.Status.Approvers = &v1alpha1.ProjectApproversStatus{Identities: ids, ResolvedAt: &at}
	meta.SetStatusCondition(&cur.Status.Conditions, metav1.Condition{Type: v1alpha1.ConditionApproversResolved,
		Status: metav1.ConditionTrue, Reason: ReasonApproversResolved, ObservedGeneration: p.Generation,
		Message: approversMessage(ids)})
}

// setApproversUnreadable sets cur's ApproversResolved False for failure, a
// bounded GitHub error, leaving status.approvers as it was.
func setApproversUnreadable(cur *v1alpha1.Project, generation int64, failure string) {
	meta.SetStatusCondition(&cur.Status.Conditions, metav1.Condition{Type: v1alpha1.ConditionApproversResolved,
		Status: metav1.ConditionFalse, Reason: ReasonApproversUnreadable, ObservedGeneration: generation,
		Message: fmt.Sprintf("GitHub could not be asked who the approvers are (%s); the identities resolved "+
			"before are kept and age, and the status page's project settings refuse edits once they are 30 "+
			"minutes old", failure)})
}

// approverLogins are the logins of p's approver list that can be
// published: in the list's order, each at most 64 characters and none
// empty (the schema's own bounds on both lists), at most
// v1alpha1.MaxProjectApprovers of them.
func approverLogins(p *v1alpha1.Project) []string {
	out := make([]string, 0, len(p.Spec.Approvers.Logins))
	for _, l := range p.Spec.Approvers.Logins {
		if l == "" || len(l) > maxApproverLogin || len(out) == v1alpha1.MaxProjectApprovers {
			continue
		}
		out = append(out, l)
	}
	return out
}

// maxApproverLogin is the longest login an identity may record
// (ProjectApproverIdentity.Login's MaxLength).
const maxApproverLogin = 64

// approversDue reports whether status needs resolving for logins: never
// resolved, the logins differ from the identities', or the resolution is
// approversReresolve old.
func approversDue(st *v1alpha1.ProjectApproversStatus, logins []string, now time.Time) bool {
	if st == nil || st.ResolvedAt == nil {
		return true
	}
	if len(st.Identities) != len(logins) {
		return true
	}
	for i, id := range st.Identities {
		if id.Login != logins[i] {
			return true
		}
	}
	return now.Sub(st.ResolvedAt.Time) >= approversReresolve
}

// resolveApprovers asks GitHub, on the intent repository, who each login is
// and whether it may write there: one identity per login, in order, as the
// list spells it. A login GitHub knows no account for (a 404), or one it
// answers with another account's login, is ID 0 and no write access, which
// matches nobody. Any other failure fails the whole resolution, so nothing
// is published from a partial answer. A login that cannot be published
// (empty, or over 64 characters) is skipped without a call.
func resolveApprovers(ctx context.Context, gh GitHub, intentRepo string, logins []string) (
	[]v1alpha1.ProjectApproverIdentity, error) {
	out := make([]v1alpha1.ProjectApproverIdentity, 0, len(logins))
	for _, login := range logins {
		if login == "" || len(login) > maxApproverLogin || len(out) == v1alpha1.MaxProjectApprovers {
			continue
		}
		c, err := gh.Collaborator(ctx, intentRepo, login)
		switch {
		case errors.Is(err, ghclient.ErrNoSuchUser):
			out = append(out, v1alpha1.ProjectApproverIdentity{Login: login})
			continue
		case err != nil:
			return nil, fmt.Errorf("approver %s: %w", login, err)
		}
		if c.Login != "" && !strings.EqualFold(c.Login, login) {
			out = append(out, v1alpha1.ProjectApproverIdentity{Login: login})
			continue
		}
		out = append(out, v1alpha1.ProjectApproverIdentity{Login: login, ID: max(c.ID, 0),
			Write: c.ID > 0 && ghclient.CanWrite(c.Permission)})
	}
	return out, nil
}

// approversMessage is ApproversResolved's True message: how many approvers
// may act, and which cannot and why.
func approversMessage(ids []v1alpha1.ProjectApproverIdentity) string {
	var write int
	var noWrite, noAccount []string
	for _, id := range ids {
		switch {
		case id.ID == 0:
			noAccount = append(noAccount, id.Login)
		case !id.Write:
			noWrite = append(noWrite, id.Login)
		default:
			write++
		}
	}
	msg := fmt.Sprintf("GitHub answered for every approver: %d with write access to the intent repository", write)
	if len(noWrite) > 0 {
		msg += "; without it: " + strings.Join(noWrite, ", ")
	}
	if len(noAccount) > 0 {
		msg += "; no such GitHub account: " + strings.Join(noAccount, ", ")
	}
	return msg
}
