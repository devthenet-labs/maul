// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"fmt"
	"slices"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/report"
	"github.com/devthenet-labs/maul/internal/templates"
	"github.com/devthenet-labs/maul/internal/untrusted"
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

// evaluates reports that the agent evaluator verifies Project p's intents
// (--intent-evaluator, beside verifiesProject): its plans are asked for
// acceptance outcomes, a plan stating slice-1 criteria is refused, and an
// approved one's outcomes are what is recorded and verified.
func (s Settings) evaluates(p *v1alpha1.Project) bool {
	return s.Evaluator && s.verifiesProject(p)
}

// planAsk is how a plan of a Project is asked for acceptance criteria, or,
// under the agent evaluator, outcomes.
type planAsk struct {
	// verified: the Project's intents are verified (verifiesProject).
	verified bool
	// outcomes: the agent evaluator verifies them (evaluates), so the plan
	// states outcomes, never criteria.
	outcomes bool
	// targets are the previewed components a criterion can target, as the
	// plan is told of them (templates.PlanVerification.Normalize, which
	// carries outcomes too); nil when there is none, or the Project is not
	// verified.
	targets *templates.PlanVerification
}

// planAsk is the plan-time decision for Project p: verified, and the
// components of every Preview of it (v1alpha1.EffectivePreviews). A
// component named like the tester or the tool pod part gives none, as
// v1alpha1.DesiredPreviewEnvironment fails closed on it.
func (s Settings) planAsk(p *v1alpha1.Project) planAsk {
	if !s.verifiesProject(p) {
		return planAsk{}
	}
	outcomes := s.evaluates(p)
	v := &templates.PlanVerification{Outcomes: outcomes}
	for _, rp := range v1alpha1.EffectivePreviews(p) {
		if rp.Name == v1alpha1.PreviewPartTester || rp.Name == v1alpha1.PreviewRoleTools {
			return planAsk{verified: true, outcomes: outcomes}
		}
		v.Components = append(v.Components, templates.PlanVerifyComponent{
			Key: rp.Name, Path: rp.Path, Readiness: rp.Preview.ReadinessPath})
	}
	return planAsk{verified: true, outcomes: outcomes, targets: v.Normalize()}
}

// asked reports whether the plan is asked for criteria (or outcomes): its
// Project is verified and previews at least one component they can target.
// Only then will an approved plan's criteria be run.
func (a planAsk) asked() bool { return a.verified && a.targets != nil }

// planAcceptance is the plan comment's acceptance line for a plan declaring
// cs, so the approver sees what maul will check and what is theirs before
// approving (design D1): the split when the plan was asked (one with none
// says it declares none); "not asked for", and why, when the Project
// verifies but previews nothing a criterion could target; "not checked"
// when criteria are declared for a Project that is not verified, kept
// rather than refused; nil, no line, when none of these, or when the plan
// was asked for outcomes, which planOutcomes lists instead.
func planAcceptance(a planAsk, cs []acceptance.Criterion) *templates.PlanAcceptance {
	switch {
	case a.verified && !a.asked():
		return &templates.PlanAcceptance{NotAsked: true}
	case a.outcomes:
		return nil
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

// The bounds of the issue lines a plan comment lists as answered by no
// outcome: issue text, which anyone who can edit the issue writes.
const (
	maxNotCoveredLines     = 20
	maxNotCoveredLineBytes = 300
)

// planOutcomes is the plan comment's outcomes section for a plan asked for
// outcomes (a.outcomes and a.asked()): each outcome as the plan states it,
// and the list items of issueBody, the request the plan answers, that no
// outcome quotes in its from (acceptance.NotCovered), at most
// maxNotCoveredLines of them, each bounded and made visible
// (untrusted.Visible), the rest counted. nil for any other plan, and
// without the issue lines when the request cannot be read (issue false).
func planOutcomes(a planAsk, os []acceptance.PlanOutcome, issueBody string, issue bool) *templates.PlanOutcomes {
	if !a.outcomes || !a.asked() {
		return nil
	}
	out := &templates.PlanOutcomes{}
	for _, o := range acceptance.NormalizeOutcomes(os) {
		out.Outcomes = append(out.Outcomes, templates.PlanOutcome{ID: o.ID, Outcome: o.Outcome, From: o.From,
			Keep: o.Kind == acceptance.OutcomeKindKeep, Manual: o.Manual, Reason: o.Reason})
	}
	if !issue {
		return out
	}
	for _, line := range acceptance.NotCovered(issueBody, os) {
		if len(out.NotCovered) == maxNotCoveredLines {
			out.NotCoveredMore++
			continue
		}
		out.NotCovered = append(out.NotCovered, untrusted.Visible(line, maxNotCoveredLineBytes))
	}
	return out
}

// criteriaUnderEvaluator says why a plan cannot be offered under the agent
// evaluator ("" when it can): it states slice-1 criteria, which the
// evaluator never runs (report.Plan.CheckEvaluator). Such a plan is refused
// when its run is collected, and the next attempt is told why.
func criteriaUnderEvaluator(a planAsk, cs []acceptance.Criterion) string {
	if !a.outcomes {
		return ""
	}
	if err := (&report.Plan{Acceptance: cs}).CheckEvaluator(); err != nil {
		return err.Error()
	}
	return ""
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
