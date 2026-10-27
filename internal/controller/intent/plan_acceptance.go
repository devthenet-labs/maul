// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"fmt"
	"slices"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/templates"
)

// A plan's acceptance criteria, before approval. One decision, planAsk,
// says whether a plan of a Project is asked for criteria and against which
// components; the plan Job's handoff (planVerification), the plan comment's
// acceptance line (planAcceptance) and the plan-time refusal of a criterion
// aimed at nothing (unpreviewedTarget) all read it, so the three cannot
// disagree. Acceptance verification is optional: nothing here ever refuses
// a plan of a Project whose intents are not verified.

// verifiesProject is the Project-level half of whether this controller
// verifies Project p's intents: the Project asks for it
// (spec.verification.enabled), and the controller runs with
// --intent-verification and --intent-previews-enabled, without which every
// verifying Project's intents are Off (VerificationWithoutPreviews). Both
// the plan-time decision (planAsk) and the review-time one (pass.verifies,
// which adds the Intent's own Preview) are derived from it.
func (s Settings) verifiesProject(p *v1alpha1.Project) bool {
	return s.Verification && s.Previews && p.Spec.VerificationEnabled()
}

// planAsk is how a plan of a Project is asked for acceptance criteria.
type planAsk struct {
	// verified: the Project's intents are verified (verifiesProject).
	verified bool
	// targets are the previewed components a criterion can target, as the
	// plan is told of them (templates.PlanVerification.Normalize); nil when
	// there is none, or the Project is not verified.
	targets *templates.PlanVerification
}

// planAsk is the plan-time decision for Project p: verified, and the
// components of every Preview of it (v1alpha1.EffectivePreviews). A
// component named like the tester part gives none, as
// v1alpha1.DesiredPreviewEnvironment fails closed on it.
func (s Settings) planAsk(p *v1alpha1.Project) planAsk {
	if !s.verifiesProject(p) {
		return planAsk{}
	}
	v := &templates.PlanVerification{}
	for _, rp := range v1alpha1.EffectivePreviews(p) {
		if rp.Name == v1alpha1.PreviewPartTester {
			return planAsk{verified: true}
		}
		v.Components = append(v.Components, templates.PlanVerifyComponent{
			Key: rp.Name, Path: rp.Path, Readiness: rp.Preview.ReadinessPath})
	}
	return planAsk{verified: true, targets: v.Normalize()}
}

// asked reports whether the plan is asked for criteria: its Project is
// verified and previews at least one component they can target. Only then
// will an approved plan's criteria be run.
func (a planAsk) asked() bool { return a.verified && a.targets != nil }

// planAcceptance is the plan comment's acceptance line for a plan declaring
// cs, so the approver sees what maul will check and what is theirs before
// approving (design D1): the split when the plan was asked (one with none
// says it declares none); "not asked for", and why, when the Project
// verifies but previews nothing a criterion could target; "not checked"
// when criteria are declared for a Project that is not verified, kept
// rather than refused; nil, no line, when none of these.
func planAcceptance(a planAsk, cs []acceptance.Criterion) *templates.PlanAcceptance {
	switch {
	case a.verified && !a.asked():
		return &templates.PlanAcceptance{NotAsked: true}
	case !a.verified && len(cs) == 0:
		return nil
	case !a.verified:
		return &templates.PlanAcceptance{Off: true}
	}
	out := &templates.PlanAcceptance{}
	for _, c := range cs {
		switch c.Kind {
		case acceptance.KindHTTP:
			out.Automated++
		case acceptance.KindCommand:
			out.NotRun++
		case acceptance.KindManual:
			out.Manual = append(out.Manual, templates.PlanManualCriterion{ID: c.ID, Reason: c.Reason})
		}
	}
	return out
}

// unpreviewedTarget says which of cs is aimed at nothing ("" when none is),
// only for a plan that was asked (a.asked()): an http criterion whose
// target, or a command criterion whose component, is not one of the
// components the plan was told of, or a command criterion whose repository
// is not one of p's. Such a plan is refused when its run is collected: an
// approved criterion cannot change, and one aimed at nothing could only end
// in NeedsReplan. A plan of a Project that is not verified keeps its
// criteria, shown as not checked (planAcceptance).
func unpreviewedTarget(a planAsk, p *v1alpha1.Project, cs []acceptance.Criterion) string {
	if !a.asked() {
		return ""
	}
	previewed := func(key string) bool {
		return slices.ContainsFunc(a.targets.Components,
			func(c templates.PlanVerifyComponent) bool { return c.Key == key })
	}
	repository := func(key string) bool {
		return slices.ContainsFunc(p.Spec.Repositories,
			func(r v1alpha1.ProjectRepository) bool { return r.Name == key })
	}
	for _, c := range cs {
		switch {
		case c.Kind == acceptance.KindHTTP && !previewed(c.Target):
			return fmt.Sprintf("criterion %s targets %s, which is not a previewed repository of the project",
				c.ID, c.Target)
		case c.Kind == acceptance.KindCommand && c.In != nil && c.In.Component != "" && !previewed(c.In.Component):
			return fmt.Sprintf("criterion %s runs in component %s, which is not a previewed repository of the "+
				"project", c.ID, c.In.Component)
		case c.Kind == acceptance.KindCommand && c.In != nil && c.In.Repository != "" && !repository(c.In.Repository):
			return fmt.Sprintf("criterion %s runs in repository %s, which is not a repository of the project",
				c.ID, c.In.Repository)
		}
	}
	return ""
}
