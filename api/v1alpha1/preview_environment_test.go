// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1

import (
	"math/rand"
	"os"
	"reflect"
	"slices"
	"testing"

	"sigs.k8s.io/yaml"
)

func verifying(p *Project) *Project {
	p.Spec.Verification = &ProjectVerification{Enabled: true}
	return p
}

func TestDesiredPreviewEnvironment(t *testing.T) {
	webPR := openPR("https://github.com/acme/Acme.Web_App", headA)
	apiPR := openPR("https://github.com/acme/api", headB)
	tests := []struct {
		name    string
		project *Project
		intent  *Intent
		want    *PreviewEnvironment
	}{
		{
			name:    "the tester reaches each component on its own port, in component order",
			project: verifying(twoAppProject()), intent: reviewing(apiPR, webPR),
			want: &PreviewEnvironment{Verification: true, Edges: []PreviewEdge{
				{From: PreviewPartTester, To: "web", Port: 8080},
				{From: PreviewPartTester, To: "api", Port: 9090},
			}},
		},
		{name: "verification unset", project: twoAppProject(), intent: reviewing(apiPR, webPR)},
		{
			name: "verification configured but off",
			project: func() *Project {
				p := twoAppProject()
				p.Spec.Verification = &ProjectVerification{PostLogs: true}
				return p
			}(),
			intent: reviewing(apiPR, webPR),
		},
		// No Preview, no environment: the environment never outlives the
		// components it connects.
		{name: "an intent with no preview", project: verifying(twoAppProject()),
			intent: &Intent{Status: IntentStatus{Phase: IntentBuilding}}},
		{name: "a component without a revision", project: verifying(twoAppProject()), intent: reviewing(apiPR)},
		{
			name: "a component keyed like the tester fails closed",
			project: verifying(&Project{Spec: ProjectSpec{
				Repositories: []ProjectRepository{{Name: PreviewPartTester, URL: "https://github.com/acme/t"}},
				Preview:      &ProjectPreview{ImageRepository: previewImageWeb, Port: 8080, ReadinessPath: "/healthz"},
			}}),
			intent: reviewing(openPR("https://github.com/acme/t", headA)),
		},
		{
			name: "a component keyed like the tool pod fails closed",
			project: verifying(&Project{Spec: ProjectSpec{
				Repositories: []ProjectRepository{{Name: PreviewRoleTools, URL: "https://github.com/acme/t"}},
				Preview:      &ProjectPreview{ImageRepository: previewImageWeb, Port: 8080, ReadinessPath: "/healthz"},
			}}),
			intent: reviewing(openPR("https://github.com/acme/t", headA)),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := DesiredPreviewEnvironment(tt.project, tt.intent)
			if ok != (tt.want != nil) || !reflect.DeepEqual(got, tt.want) {
				t.Errorf("DesiredPreviewEnvironment = %+v, %v; want %+v", got, ok, tt.want)
			}
		})
	}
}

// TestDesiredPreviewEnvironmentProperties: over random Projects and Intents,
// the environment exists exactly when verification is on and the Preview
// exists; it is deterministic; its edges are one per component, from the
// tester, on the component's port; none ends at the tester; and a PR head
// moving never changes it (the Preview's environment is set once).
func TestDesiredPreviewEnvironmentProperties(t *testing.T) {
	rng := rand.New(rand.NewSource(20261026))
	for i := range 2000 {
		p, in := randomEnvironmentCase(rng)
		env, ok := DesiredPreviewEnvironment(p, in)
		components, previewed := DesiredPreviewComponents(p, in)
		if ok != (previewed && p.Spec.VerificationEnabled()) {
			t.Fatalf("case %d: environment %v, preview %v, verification %v", i, ok, previewed, p.Spec.VerificationEnabled())
		}
		again, _ := DesiredPreviewEnvironment(p, in)
		if !reflect.DeepEqual(env, again) {
			t.Fatalf("case %d: not deterministic: %+v then %+v", i, env, again)
		}
		if !ok {
			if env != nil {
				t.Fatalf("case %d: no environment but %+v", i, env)
			}
			continue
		}
		if !testerEdgesOnly(env, components) {
			t.Fatalf("case %d: environment %+v for components %+v", i, env, components)
		}
		moved := in.DeepCopy()
		for k := range moved.Status.PullRequests {
			moved.Status.PullRequests[k].HeadSHA = headB
		}
		if env2, ok2 := DesiredPreviewEnvironment(p, moved); ok2 && !reflect.DeepEqual(env, env2) {
			t.Fatalf("case %d: a moved head changed the environment: %+v then %+v", i, env, env2)
		}
	}
}

// randomEnvironmentCase is a two-component Project, verifying or not, and an
// Intent in review (or not) with any of its pull requests, at heads that may
// or may not be previewable.
func randomEnvironmentCase(rng *rand.Rand) (*Project, *Intent) {
	heads := []string{headA, headB, baseC, "", "abc"}
	p := twoAppProject()
	if rng.Intn(4) > 0 {
		p.Spec.Verification = &ProjectVerification{Enabled: rng.Intn(5) > 0}
	}
	var prs []IntentPullRequest
	for _, repo := range []string{"https://github.com/acme/Acme.Web_App", "https://github.com/acme/api"} {
		if rng.Intn(3) > 0 {
			prs = append(prs, openPR(repo, heads[rng.Intn(len(heads))]))
		}
	}
	in := reviewing(prs...)
	if rng.Intn(6) == 0 {
		in.Status.Phase = IntentPlanning
	}
	return p, in
}

// testerEdgesOnly reports whether env is verification-capable with exactly
// one edge per component, in order, from the tester on the component's port.
func testerEdgesOnly(env *PreviewEnvironment, components []PreviewComponent) bool {
	if !env.Verification || len(env.Edges) != len(components) {
		return false
	}
	for k, e := range env.Edges {
		c := components[k]
		if e.From != PreviewPartTester || e.To != c.Name || e.Port != c.Port || e.To == PreviewPartTester {
			return false
		}
	}
	return true
}

// TestIntentRunTriggersMatchSchema pins IntentRunTriggers, which exhaustive
// switches are tested against, to the trigger enum the generated CRD admits.
func TestIntentRunTriggersMatchSchema(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/kustomize/base/crds/maul.run_intentruns.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd struct {
		Spec struct {
			Versions []struct {
				Schema struct {
					OpenAPIV3Schema struct {
						Properties struct {
							Spec struct {
								Properties struct {
									Trigger struct {
										Enum []string `json:"enum"`
									} `json:"trigger"`
								} `json:"properties"`
							} `json:"spec"`
						} `json:"properties"`
					} `json:"openAPIV3Schema"`
				} `json:"schema"`
			} `json:"versions"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatal(err)
	}
	if len(crd.Spec.Versions) != 1 {
		t.Fatalf("%d versions, want 1", len(crd.Spec.Versions))
	}
	enum := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties.Spec.Properties.Trigger.Enum
	got := make([]string, 0, len(IntentRunTriggers))
	for _, tr := range IntentRunTriggers {
		got = append(got, string(tr))
	}
	if !slices.Equal(got, enum) {
		t.Errorf("IntentRunTriggers = %v, the schema admits %v", got, enum)
	}
}
