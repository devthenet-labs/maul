// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"fmt"
	"slices"
	"time"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/templates"
)

// Judgement currency (S4 PR 9): whether the verification on record is about
// the pull requests as they are now, and whether its judgement is one the
// reporters may present. Every reporter (maul/acceptance, the hand-off, the
// comments, the not-run answer and /maul accept) and every round driver
// reads the heads through headsNow and nothing else, so no two of them can
// disagree about which commits are current (review SM-1). A seeded
// property holds the reporters to one answer
// (TestReportersAgreeOnCurrencySeeded).

// headsNow is the head set the Intent's records derive now: its tool
// host's (toolHost.heads, as its rounds derive them), and with the agents
// host switched off still the agents host's own, which reads only the
// Project and the Intent (settledAtHeads keeps a round settled at them).
// ok is false while there is none.
func (p *pass) headsNow() (headSet, bool) {
	if p.set.agentsHostOff(p.proj) {
		return agentsHost{}.heads(p)
	}
	return p.toolHost().heads(p)
}

// atHeads reports that v is about headsNow: only then is v's judgement
// reported (review SM-1). A pull request's head is recorded on the poll
// that reads it, and the head set follows only on a later pass's
// syncHeadSet, so in between a reporter would otherwise show a judgement
// of the old heads on the new.
func (p *pass) atHeads(v *v1alpha1.IntentVerification) bool {
	hs, ok := p.headsNow()
	return ok && v != nil && hs.digest == v.HeadSetDigest
}

// judgedAtRecord reports that v's outcomes were written by the judgement of
// its own round at its own head set, by a round that ran (settledWhy: a
// round that recorded it could not run is no judgement, whatever outcomes
// it kept). It reads the record alone; judgedNow adds the heads now.
func judgedAtRecord(v *v1alpha1.IntentVerification) bool {
	return v.OutcomesJudgedAt(v.Round, v.HeadSetDigest) && settledWhy(v) == ""
}

// judgedNow reports that v holds a judgement of the pull requests as they
// are now: at headsNow, and judged there by its own round (judgedAtRecord).
func (p *pass) judgedNow(v *v1alpha1.IntentVerification) bool {
	return p.atHeads(v) && judgedAtRecord(v)
}

// standingJudgement is the round whose judgement of the current heads
// stands on record (0 when none does): a settled round's reference whose
// judgement is current (judgedNow). A state that records its round could
// not run is no judgement: every surface says maul couldn't confirm it.
func (p *pass) standingJudgement(v *v1alpha1.IntentVerification) int32 {
	if v.Ref == nil || !p.judgedNow(v) {
		return 0
	}
	switch v.State {
	case v1alpha1.VerificationPassed, v1alpha1.VerificationUnconfirmed, v1alpha1.VerificationFailing,
		v1alpha1.VerificationExhausted:
		return v.Round
	}
	return 0
}

// acceptedNow reports that an acceptance given at at binds to v's head
// set: v is at headsNow (atHeads), and what checks those heads had them
// before at, and still has. On a Project the agents tool pod evaluates, a
// round of the Intent's at that head set was created before at; on a
// previewed one, the Preview is live at exactly those heads and went live at
// them (status.lastDeployedAt, cleared by every new deployment) before at.
// An acceptance is bound to the commits the approver could have checked,
// never to ones recorded, deployed or checked after they commented. With
// the agents host switched off nothing checks them, so none binds. A
// Preview not found is not live; any other failed read is returned,
// deciding nothing.
func (p *pass) acceptedNow(ctx context.Context, v *v1alpha1.IntentVerification, at time.Time) (bool, error) {
	if !p.atHeads(v) {
		return false, nil
	}
	switch {
	case p.set.agentsProject(p.proj):
		pvs, err := p.ownVerifications(ctx)
		if err != nil {
			return false, fmt.Errorf("read the intent's verifications: %w", err)
		}
		return slices.ContainsFunc(pvs, func(pv v1alpha1.PreviewVerification) bool {
			return pv.Spec.IsAgents() && pv.Spec.HeadSetDigest == v.HeadSetDigest &&
				at.After(pv.CreationTimestamp.Time)
		}), nil
	case p.set.agentsHostOff(p.proj):
		return false, nil
	}
	var pv v1alpha1.Preview
	err := p.r.APIReader.Get(ctx, types.NamespacedName{Namespace: p.in.Namespace, Name: p.in.Name}, &pv)
	switch {
	case kerrors.IsNotFound(err):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("read the intent's preview: %w", err)
	}
	deployed := pv.Status.LastDeployedAt
	return viewPreview(p.proj, p.in, &pv).state == templates.PreviewLive && deployed != nil &&
		at.After(deployed.Time), nil
}
