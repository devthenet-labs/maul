// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"fmt"
	"log/slog"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/credgate"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
	"github.com/bitwise-media-group/patchy/internal/model"
	"github.com/bitwise-media-group/patchy/internal/templates"
)

// Per-user model credentials (design L3, section 4 "Fallback" and revision
// 1's re-attribution rule), under --user-credentials enforce only. A run
// attributed to a person runs on that person's own credential or not at
// all: the broker refuses it otherwise. So before a plan, build or revise
// run attributed to a person is created, the person's ModelCredential must
// be Valid; while it is not, the Intent is Blocked (CredentialRequired),
// status.credentialHold names the person, and the status comment says who
// must add a credential and links the Account page. Nothing is replanned:
// the block lifts on its own once the credential is Valid, and a block from
// Building also once an approver who holds a Valid credential approves the
// plan again (settleCredentialReapproval), which makes the build theirs.
// Off and attribute, and runs attributed to the org, never block.

// credentialProvider is the provider whose credential pays for the
// Intent's runs: their harness's (credgate.ProviderFor, the one source of
// it, so codex intents need only choose their harness). ok is false for a
// harness no person's credential pays for, whose runs are never held.
func (r *IntentReconciler) credentialProvider() (v1alpha1.ModelCredentialProvider, bool) {
	h := r.Harness
	if h == "" {
		h = model.HarnessClaude
	}
	return credgate.ProviderFor(h)
}

// checkCredential is credgate.Check for a run of the Intent's harness: no
// hold for a harness no person's credential pays for.
func (p *pass) checkCredential(ctx context.Context, attributed *v1alpha1.Attribution) (credgate.Verdict, error) {
	provider, ok := p.r.credentialProvider()
	if !ok {
		return credgate.Verdict{}, nil
	}
	return credgate.Check(ctx, p.set.UserCredentials, provider, attributed, p.r.Credentials)
}

// holdForCredential blocks the Intent, before a run attributed by
// attributed is created, while that person has no Valid credential.
// reapprove says a later approval would lift it (a build). held reports
// the block (a status write).
func (p *pass) holdForCredential(ctx context.Context, attributed *v1alpha1.Attribution, reapprove bool) (
	held bool, err error) {
	v, err := p.checkCredential(ctx, attributed)
	if err != nil || !v.Hold {
		return false, err
	}
	p.r.log().LogAttrs(ctx, slog.LevelInfo, "the intent's next run waits for a person's model credential",
		slog.String("intent", p.in.Name), slog.String("user", v.User.Login), slog.String("reason", v.Reason))
	err = p.setPhase(ctx, v1alpha1.IntentBlocked, func(cur *v1alpha1.Intent) {
		setCondition(cur, v1alpha1.ConditionCredentialRequired, metav1.ConditionTrue, v.Reason,
			credgate.Message(v, p.set.AccountURL, reapprove))
		cur.Status.CredentialHold = v.User.DeepCopy()
		cur.Status.ActiveRun = nil
	})
	return true, err
}

// credentialBlockHolds reports whether a CredentialRequired block still
// holds: the person the next run would be attributed to (for a block from
// Building, the current approval's, so a re-approval moves it; otherwise
// status.credentialHold) still has no Valid credential under enforce. A
// block that holds with a changed reason or person is rewritten, so the
// status comment says what holds it now. A mode that is no longer enforce
// holds nothing.
func (p *pass) credentialBlockHolds(ctx context.Context) (bool, error) {
	c := meta.FindStatusCondition(p.in.Status.Conditions, v1alpha1.ConditionCredentialRequired)
	if c == nil || c.Status != metav1.ConditionTrue {
		return false, nil
	}
	from := v1alpha1.IntentBlockedFrom(p.in)
	var attributed *v1alpha1.Attribution
	switch {
	case from == v1alpha1.IntentBuilding && p.in.Status.Approval != nil:
		attributed = p.attributeRun(v1alpha1.IntentStageBuild)
	case p.in.Status.CredentialHold != nil:
		attributed = &v1alpha1.Attribution{User: p.in.Status.CredentialHold, Reason: v1alpha1.AttributionApprover}
	}
	v, err := p.checkCredential(ctx, p.set.Attribution.Stamp(attributed))
	if err != nil || !v.Hold {
		return false, err
	}
	msg := credgate.Message(v, p.set.AccountURL, from == v1alpha1.IntentBuilding)
	if c.Reason == v.Reason && c.Message == msg && sameUser(p.in.Status.CredentialHold, v.User) {
		return true, nil
	}
	return true, p.update(ctx, func(cur *v1alpha1.Intent) error {
		setCondition(cur, v1alpha1.ConditionCredentialRequired, metav1.ConditionTrue, v.Reason, msg)
		cur.Status.CredentialHold = v.User.DeepCopy()
		return nil
	})
}

// credentialReapproval reports an Intent Blocked from Building on its
// approver's credential, the plan it approved still the current one: an
// approval now (settleCredentialReapproval) may replace whom the build is
// for.
func (p *pass) credentialReapproval() bool {
	st := p.in.Status
	return st.Phase == v1alpha1.IntentBlocked && v1alpha1.IntentBlockedFrom(p.in) == v1alpha1.IntentBuilding &&
		meta.IsStatusConditionTrue(st.Conditions, v1alpha1.ConditionCredentialRequired) &&
		st.Approval != nil && st.Plan != nil && st.Plan.PostedAt != nil && st.Plan.Revision == st.Approval.PlanRevision
}

func sameUser(a, b *v1alpha1.UserRef) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// settleCredentialReapproval answers an approval made while the Intent is
// Blocked from Building on its approver's credential (credentialReapproval):
// from an approver, made after the approval it would replace, of the plan
// and issue as they were approved, and by someone whose own credential is
// Valid, it replaces the approval (the same plan revision and digests), so
// the build is theirs and the block lifts on the next pass. An approver
// without a valid credential is told so and nothing changes; anything else
// is answered as an approval is in that state.
func (p *pass) settleCredentialReapproval(ctx context.Context, a humanAction, issue *ghclient.Issue) error {
	prev := p.in.Status.Approval
	if !after(a.at, a.source, a.id, v1alpha1.IntentAction{Source: prev.Source, EventID: prev.EventID, At: prev.At}) {
		return p.notAvailable(ctx, a, p.in.Status.Phase, false)
	}
	isLabel := a.source == v1alpha1.IntentActionLabel
	approve := approveLabel(p.proj)
	removeLabel := func() error {
		if !isLabel || !hasLabel(issue, approve) {
			return nil
		}
		if err := p.r.GitHub.RemoveLabel(ctx, p.repo(), p.number(), approve); err != nil {
			return fmt.Errorf("remove the approve label: %w", err)
		}
		return nil
	}
	ok, bot, err := p.authorize(ctx, a.actor)
	if err != nil {
		return err
	}
	if !ok {
		if err := p.notAllowed(ctx, a, bot, isLabel, false); err != nil {
			return err
		}
		return removeLabel()
	}
	planChanged, issueChanged, err := p.approvalChanged(ctx, issue)
	if err != nil {
		return err
	}
	if planChanged || issueChanged {
		return p.refuseApproval(ctx, a, issue, planChanged, issueChanged)
	}
	holds := false
	provider, judged := p.r.credentialProvider()
	if judged {
		if holds, err = credgate.Holds(ctx, p.set.UserCredentials, provider, a.actor.Login, a.actor.ID,
			p.r.Credentials); err != nil {
			return err
		}
	}
	if holds {
		n := templates.CredentialApprovalNotice{
			Namespace: p.in.Namespace, Intent: p.in.Name, Key: a.key(), Actor: a.actor.Login,
			LabelRemoved: isLabel && hasLabel(issue, approve), AccountURL: p.set.AccountURL,
			Provider: credgate.ProviderName(provider), Kinds: credgate.CredentialKinds(provider),
		}
		if isLabel {
			n.Label = approve
		} else {
			n.Verb = a.verb
		}
		body, err := templates.RenderCredentialApprovalNotice(n)
		if err := p.notice(ctx, a.key(), a.at, body, err); err != nil {
			return err
		}
		return removeLabel()
	}
	approval := v1alpha1.IntentApproval{
		By: a.actor.Login, ByID: max(a.actor.ID, 0), Source: a.source, EventID: a.id, At: metav1.NewTime(a.at),
		PlanRevision: prev.PlanRevision, PlanDigest: prev.PlanDigest, InputDigest: prev.InputDigest,
	}
	p.r.log().LogAttrs(ctx, slog.LevelInfo, "a re-approval replaces whom the held build is for",
		slog.String("intent", p.in.Name), slog.String("from", prev.By), slog.String("to", a.actor.Login))
	if err := p.update(ctx, func(cur *v1alpha1.Intent) error {
		cur.Status.Approval = &approval
		setCondition(cur, v1alpha1.ConditionApprovalRejected, metav1.ConditionFalse, "Approved",
			fmt.Sprintf("plan r%d approved by %s", approval.PlanRevision, a.actor.Login))
		return nil
	}); err != nil {
		return err
	}
	return p.replyDone(ctx, a)
}
