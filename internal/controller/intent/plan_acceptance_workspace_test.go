// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"reflect"
	"slices"
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/templates"
)

func workspaceProject(repos []v1alpha1.ProjectRepository, s *v1alpha1.ProjectSurfaces) *v1alpha1.Project {
	return &v1alpha1.Project{Spec: v1alpha1.ProjectSpec{
		Repositories: repos,
		Verification: &v1alpha1.ProjectVerification{Enabled: true},
		Surfaces:     s,
	}}
}

var evaluating = Settings{Verification: true, Previews: true, Evaluator: true}

// TestPlanAskNamesTheWorkspaceSurface: a Project with no preview and a cli or
// library surface is asked for outcomes with the surface named, no
// components (SPEC-S2 S2-17); anything that cannot be stated as one valid
// workspace is not asked, never asked wrongly.
func TestPlanAskNamesTheWorkspaceSurface(t *testing.T) {
	one := []v1alpha1.ProjectRepository{{Name: "app", URL: "https://github.com/o/app"}}
	two := append(slices.Clone(one), v1alpha1.ProjectRepository{Name: "lib", URL: "https://github.com/o/lib"})
	cli := func(repo, bin string) *v1alpha1.ProjectSurfaces {
		return &v1alpha1.ProjectSurfaces{CLI: &v1alpha1.CLISurface{
			Repository: repo, Build: []string{"go", "build", "-o", "/work/bin/", "."}, Binary: bin}}
	}
	lib := func(repo string) *v1alpha1.ProjectSurfaces {
		return &v1alpha1.ProjectSurfaces{Library: &v1alpha1.LibrarySurface{
			Repository: repo, Language: v1alpha1.LibraryLanguageGo}}
	}
	previewed := slices.Clone(one)
	previewed[0].Preview = &v1alpha1.ProjectRepositoryPreview{}

	for _, tt := range []struct {
		name string
		p    *v1alpha1.Project
		set  Settings
		want *templates.PlanVerification // nil: not asked
	}{
		{"cli, one repository", workspaceProject(one, cli("", "/work/bin/bbcount")), evaluating,
			&templates.PlanVerification{Outcomes: true, Workspace: &templates.PlanWorkspace{
				Surface: templates.SurfaceCLI, Binary: "/work/bin/bbcount"}}},
		{"cli, named repository of two", workspaceProject(two, cli("lib", "/work/bin/x")), evaluating,
			&templates.PlanVerification{Outcomes: true, Workspace: &templates.PlanWorkspace{
				Surface: templates.SurfaceCLI, Repository: "https://github.com/o/lib", Binary: "/work/bin/x"}}},
		{"library, one repository", workspaceProject(one, lib("")), evaluating,
			&templates.PlanVerification{Outcomes: true, Workspace: &templates.PlanWorkspace{
				Surface: templates.SurfaceLibrary, Language: "go"}}},
		{"library, named repository of two", workspaceProject(two, lib("app")), evaluating,
			&templates.PlanVerification{Outcomes: true, Workspace: &templates.PlanWorkspace{
				Surface: templates.SurfaceLibrary, Repository: "https://github.com/o/app", Language: "go"}}},
		{"unnamed repository of two", workspaceProject(two, lib("")), evaluating, nil},
		{"unknown repository", workspaceProject(one, lib("nope")), evaluating, nil},
		{"invalid binary", workspaceProject(one, cli("", "/usr/bin/x")), evaluating, nil},
		{"beside a preview", workspaceProject(previewed, lib("")), evaluating, nil},
		{"slice 1 criteria, not outcomes", workspaceProject(one, lib("")),
			Settings{Verification: true, Previews: true}, nil},
		{"no surface", workspaceProject(one, nil), evaluating, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a := tt.set.planAsk(tt.p)
			if got := a.asked(); got != (tt.want != nil) {
				t.Fatalf("asked = %v, want %v", got, tt.want != nil)
			}
			if tt.want == nil {
				return
			}
			if !a.outcomes || !reflect.DeepEqual(a.targets, tt.want) {
				t.Errorf("targets = %+v (outcomes %v), want %+v", a.targets, a.outcomes, tt.want)
			}
			enc := templates.EncodePlanVerification(a.targets)
			if enc == "" {
				t.Fatal("the handoff encodes to nothing")
			}
			if _, err := templates.DecodePlanVerification(enc); err != nil {
				t.Errorf("the handoff does not decode: %v", err)
			}
		})
	}
}

// TestPlanJobIsHandedTheWorkspace: the plan Job's MAUL_PLAN_VERIFICATION
// carries the workspace and no components.
func TestPlanJobIsHandedTheWorkspace(t *testing.T) {
	p := workspaceProject([]v1alpha1.ProjectRepository{{Name: "app", URL: "https://github.com/o/app"}},
		&v1alpha1.ProjectSurfaces{Library: &v1alpha1.LibrarySurface{Language: v1alpha1.LibraryLanguageGo}})
	r := &RunReconciler{Settings: evaluating}
	got, err := templates.DecodePlanVerification(r.planVerification(p))
	if err != nil || got == nil || !got.Outcomes || len(got.Components) != 0 || got.Workspace == nil ||
		got.Workspace.Surface != templates.SurfaceLibrary {
		t.Errorf("MAUL_PLAN_VERIFICATION decodes to %+v, %v", got, err)
	}
	r.Settings.Evaluator = false
	if s := r.planVerification(p); s != "" {
		t.Errorf("without the evaluator the plan is handed %s, want nothing", s)
	}
}
