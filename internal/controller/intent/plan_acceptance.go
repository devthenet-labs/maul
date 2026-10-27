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

// A plan's acceptance criteria, before approval: whether its Project's
// intents are verified at all, what the plan comment's header says of the
// criteria (planAcceptance), and the plan-time refusal of a criterion aimed
// at no previewed component (unpreviewedTarget).

// verifies reports whether this controller verifies Project p's intents:
// the Project asks for it (spec.verification.enabled), and the controller
// runs with --intent-verification and --intent-previews-enabled, without
// which every verifying Project's intents are Off
// (VerificationWithoutPreviews). The plan prompt asks for criteria, and the
// plan comment counts them, exactly when it holds.
func (s Settings) verifies(p *v1alpha1.Project) bool {
	return s.Verification && s.Previews && p.Spec.VerificationEnabled()
}

// planAcceptance is the plan comment's acceptance line for a plan declaring
// cs, so the approver sees what maul will check and what is theirs before
// approving (design D1): the split when the Project is verified (a plan with
// none says it declares none); "not checked" when criteria are declared for
// a Project that is not; nil, no line, when neither.
func planAcceptance(verified bool, cs []acceptance.Criterion) *templates.PlanAcceptance {
	if !verified {
		if len(cs) == 0 {
			return nil
		}
		return &templates.PlanAcceptance{Off: true}
	}
	a := &templates.PlanAcceptance{}
	for _, c := range cs {
		switch c.Kind {
		case acceptance.KindHTTP:
			a.Automated++
		case acceptance.KindCommand:
			a.NotRun++
		case acceptance.KindManual:
			a.Manual = append(a.Manual, templates.PlanManualCriterion{ID: c.ID, Reason: c.Reason})
		}
	}
	return a
}

// unpreviewedTarget says which of cs is aimed at nothing ("" when none is):
// an http criterion whose target, or a command criterion whose component,
// is not a previewed repository of p (v1alpha1.EffectivePreviews), or a
// command criterion whose repository is not one of p's. Such a plan is
// refused when its run is collected, whether or not the Project verifies
// now: an approved criterion cannot change, and one aimed at nothing could
// only ever end in NeedsReplan.
func unpreviewedTarget(p *v1alpha1.Project, cs []acceptance.Criterion) string {
	previewed := func(key string) bool {
		return slices.ContainsFunc(v1alpha1.EffectivePreviews(p),
			func(rp v1alpha1.RepositoryPreview) bool { return rp.Name == key })
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
