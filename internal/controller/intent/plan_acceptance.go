// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"slices"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/report"
	"github.com/devthenet-labs/maul/internal/templates"
	"github.com/devthenet-labs/maul/internal/untrusted"
)

// A plan's acceptance outcomes, before approval. One decision, planAsk,
// says whether a plan of a Project is asked for outcomes and against which
// components or workspace; the plan Job's handoff (planVerification), the
// plan comment's acceptance lines (planAcceptance, planOutcomes) and the
// plan-time refusal of slice-1 criteria (criteriaUnderEvaluator) all read
// it, so they cannot disagree. Acceptance verification is optional: nothing
// here ever refuses a plan of a Project whose intents are not verified.

// verifiesProject is the Project-level half of whether this controller
// verifies Project p's intents: the Project asks for it
// (spec.verification.enabled), and the controller runs with
// the evaluator (Verification) and --intent-previews-enabled, without which every
// verifying Project's intents are Off (VerificationWithoutPreviews). A
// Project with a cli or library surface needs no previews (SPEC-S2 Q33)
// but the agents tool pod (agentsProject), and none of its own beside its
// surface (O6). It is exactly "no VerificationOff" (verificationOff), so the
// Project's condition never disagrees with what is verified. Both the
// plan-time decision (planAsk) and the review-time one (pass.verifies,
// which adds the Intent's tool host) are derived from it.
func (s Settings) verifiesProject(p *v1alpha1.Project) bool {
	if !s.Verification || !p.Spec.VerificationEnabled() {
		return false
	}
	off, _ := s.verificationOff(p)
	return off == ""
}

// evaluates reports that the agent evaluator verifies Project p's intents
// (--intent-evaluator, beside verifiesProject): its plans are asked for
// acceptance outcomes, a plan stating slice-1 criteria is refused, and an
// approved one's outcomes are what is recorded and verified.
func (s Settings) evaluates(p *v1alpha1.Project) bool {
	return s.Evaluator && s.verifiesProject(p)
}

// planAsk is how a plan of a Project is asked for acceptance outcomes.
type planAsk struct {
	// verified: the agent evaluator verifies the Project's intents
	// (evaluates), so the plan states outcomes, never criteria.
	verified bool
	// targets are the previewed components (or, with no preview, the
	// workspace) the outcomes are checked on, as the plan is told of them
	// (templates.PlanVerification.Normalize); nil when there is none, or
	// the Project is not verified.
	targets *templates.PlanVerification
}

// planAsk is the plan-time decision for Project p: verified by the agent
// evaluator, and the components of every Preview of it
// (v1alpha1.EffectivePreviews). A component named like the tester or the
// tool pod part gives none, as v1alpha1.DesiredPreviewEnvironment fails
// closed on it.
func (s Settings) planAsk(p *v1alpha1.Project) planAsk {
	if !s.evaluates(p) {
		return planAsk{}
	}
	previews := v1alpha1.EffectivePreviews(p)
	if len(previews) == 0 && p.Spec.Surfaces.Agents() {
		return planAsk{verified: true, targets: planWorkspace(p)}
	}
	v := &templates.PlanVerification{}
	for _, rp := range previews {
		if rp.Name == v1alpha1.PreviewPartToolPod || rp.Name == v1alpha1.PreviewRoleTools {
			return planAsk{verified: true}
		}
		v.Components = append(v.Components, templates.PlanVerifyComponent{
			Key: rp.Name, Path: rp.Path, Readiness: rp.Preview.ReadinessPath})
	}
	return planAsk{verified: true, targets: v.Normalize()}
}

// planWorkspace is the plan-time ask of a Project with a cli or library
// surface and no preview (SPEC-S2): outcomes with the surface named and no
// components, as the tool pod will build it. A Project that also previews
// a repository (O6) never reaches it: it is not verified at all
// (SurfaceCombinationUnsupported), at plan time as at review time. Nor is
// one under a controller without the agents tool pod
// (AgentsToolPodNotEnabled). nil, so the plan is not asked, when the
// surface cannot be stated: its repository is not one of the Project's
// (or is unnamed in a Project of several), or its binary or language is one
// the plan prompt cannot state (templates.PlanVerification.Normalize).
func planWorkspace(p *v1alpha1.Project) *templates.PlanVerification {
	s := p.Spec.Surfaces
	w := &templates.PlanWorkspace{}
	var key string
	switch {
	case s.CLI != nil:
		w.Surface, w.Binary, key = templates.SurfaceCLI, s.CLI.Binary, s.CLI.Repository
	case s.Library != nil:
		w.Surface, w.Language, key = templates.SurfaceLibrary, string(s.Library.Language), s.Library.Repository
	}
	repos := p.Spec.Repositories
	switch {
	case len(repos) == 1 && (key == "" || key == repos[0].Name):
		// The Project's one repository: the prompt does not name it.
	case key == "":
		return nil
	default:
		i := slices.IndexFunc(repos, func(r v1alpha1.ProjectRepository) bool { return r.Name == key })
		if i < 0 {
			return nil
		}
		w.Repository = repos[i].URL
	}
	return (&templates.PlanVerification{Workspace: w}).Normalize()
}

// asked reports whether the plan is asked for outcomes: its Project is
// verified and previews at least one component (or states a workspace)
// they are checked on. Only then will an approved plan's outcomes be run.
func (a planAsk) asked() bool { return a.verified && a.targets != nil }

// planAcceptance is the plan comment's acceptance line: "not asked for",
// and why, when the Project is verified but previews nothing its outcomes
// could be checked on; nil, no line, otherwise (an asked plan's outcomes
// are planOutcomes' to list).
func planAcceptance(a planAsk) *templates.PlanAcceptance {
	if a.verified && !a.asked() {
		return &templates.PlanAcceptance{NotAsked: true}
	}
	return nil
}

// The bounds of the issue lines a plan comment lists as answered by no
// outcome: issue text, which anyone who can edit the issue writes.
const (
	maxNotCoveredLines     = 20
	maxNotCoveredLineBytes = 300
)

// planOutcomes is the plan comment's outcomes section for a plan asked for
// outcomes (a.asked()): each outcome as the plan states it,
// and the list items of issueBody, the request the plan answers, that no
// outcome quotes in its from (acceptance.NotCovered), at most
// maxNotCoveredLines of them, each bounded and made visible
// (untrusted.Visible), the rest counted; checked in the workspace for a
// Project with no preview (planWorkspace). nil for any other plan, and
// without the issue lines when the request cannot be read (issue false).
func planOutcomes(a planAsk, os []acceptance.PlanOutcome, issueBody string, issue bool) *templates.PlanOutcomes {
	if !a.asked() {
		return nil
	}
	out := &templates.PlanOutcomes{Workspace: a.targets != nil && a.targets.Workspace != nil}
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
	if !a.verified {
		return ""
	}
	if err := (&report.Plan{Acceptance: cs}).CheckEvaluator(); err != nil {
		return err.Error()
	}
	return ""
}
