// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"reflect"
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/templates"
)

// verifying sets --intent-verification and --intent-previews-enabled on
// every reconciler, as the binary shares one Settings among them.
func (e *env) verifying(verification, previews bool) {
	for _, s := range []*Settings{&e.project.Settings, &e.intent.Settings, &e.runs.Settings} {
		s.Verification, s.Previews = verification, previews
	}
}

// TestPlanJobIsToldItsVerification: the plan Job of a Project that verifies,
// under a controller that verifies, is handed the components a criterion
// can target (key, path, readiness), so the plan prompt asks for acceptance
// criteria. Live, no plan Job was ever handed them: no plan declared a
// criterion, and verification never ran. A Project that does not verify,
// or a controller with verification or previews off (verification Off),
// hands the plan Job nothing.
func TestPlanJobIsToldItsVerification(t *testing.T) {
	verify := func(p *v1alpha1.Project) *v1alpha1.Project {
		p.Spec.Verification = &v1alpha1.ProjectVerification{Enabled: true}
		return p
	}
	one := func() *v1alpha1.Project {
		p := testProject()
		p.Spec.Preview = &appPreview
		return p
	}
	for _, tt := range []struct {
		name                   string
		project                *v1alpha1.Project
		multi                  bool
		verification, previews bool
		want                   *templates.PlanVerification
	}{
		{"one repository", verify(one()), false, true, true, &templates.PlanVerification{
			Components: []templates.PlanVerifyComponent{{Key: "app", Path: "/", Readiness: "/healthz"}}}},
		{"repositories", verify(previewedProject(true)), true, true, true, &templates.PlanVerification{
			Components: []templates.PlanVerifyComponent{
				{Key: "app", Path: "/api", Readiness: "/healthz"},
				{Key: "web", Path: "/", Readiness: "/healthz"},
			}}},
		{"project does not verify", one(), false, true, true, nil},
		{"controller does not verify", verify(one()), false, false, true, nil},
		{"controller has no previews", verify(one()), false, true, false, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, tt.project)
			if tt.multi {
				e.multiRepo(true)
				e.jobs.output = multiOutput
			}
			e.verifying(tt.verification, tt.previews)
			e.awaiting()
			plans := e.specsOf("plan")
			if len(plans) == 0 {
				t.Fatal("no plan Job launched")
			}
			for _, spec := range plans {
				got, err := templates.DecodePlanVerification(spec.PlanVerification)
				if err != nil || !reflect.DeepEqual(got, tt.want) {
					t.Errorf("plan Job handed %q = %+v (%v), want %+v", spec.PlanVerification, got, err, tt.want)
				}
			}
			for _, phase := range []string{"build", "revise"} {
				for _, spec := range e.specsOf(phase) {
					if spec.PlanVerification != "" {
						t.Errorf("%s Job handed the plan's verification %q", phase, spec.PlanVerification)
					}
				}
			}
		})
	}
}
