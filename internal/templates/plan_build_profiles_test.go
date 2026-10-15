// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"reflect"
	"strings"
	"testing"
)

// TestPlanPromptBuildProfilesGoldens pins what a plan prompt says when a
// build of the plan gets an operator network profile. A live planner, told
// only that a human must add each dependency the build cannot fetch, listed
// a new Go module as needing the image updated, when the build's go profile
// could fetch it (intents#29). Without a profile the prompt is exactly what
// it was (prompt_plan.md, prompt_plan_trees.md).
func TestPlanPromptBuildProfilesGoldens(t *testing.T) {
	for _, tt := range []struct {
		name   string
		render func([]BuildProfile) (string, error)
		golden string
		want   string
	}{
		{"one repository", func(b []BuildProfile) (string, error) {
			return RenderPlanPrompt(PlanPrompt{
				IssuePath: "/workspace/input/issue.md", ReportPath: "/workspace/reports/plan.md",
				Intent: testPlanRequest, BuildMaxTurns: 150, BuildTokenBudget: 800000, Limits: testPlanLimits,
				BuildProfiles: b,
			})
		}, "prompt_plan.md", "This repository's build runs with the operator's network profile `go`"},
		{"repositories", func(b []BuildProfile) (string, error) {
			p, err := renderTestTreesPlanPromptWith(b)
			return p, err
		}, "prompt_plan_trees.md",
			"The build of each repository listed here runs with an operator network profile: `" + testAPIURL +
				"` (profile `go`)."},
	} {
		t.Run(tt.name, func(t *testing.T) {
			without, err := tt.render(nil)
			if err != nil {
				t.Fatal(err)
			}
			golden(t, tt.golden, without)
			got, err := tt.render([]BuildProfile{{URL: testAPIURL, Profile: "go"}})
			if err != nil {
				t.Fatal(err)
			}
			golden(t, strings.TrimSuffix(tt.golden, ".md")+"_network_profile.md", got)
			if !strings.Contains(got, tt.want) {
				t.Errorf("prompt lacks %q:\n%s", tt.want, got)
			}
			if !strings.Contains(got, "only tagged releases of module paths the operator allows") ||
				!strings.Contains(got, "must already be in the image") {
				t.Errorf("prompt does not say what the build can fetch and what it cannot:\n%s", got)
			}
		})
	}
}

func renderTestTreesPlanPromptWith(b []BuildProfile) (string, error) {
	return RenderPlanPrompt(PlanPrompt{
		IssuePath: "/workspace/input/issue.md", ReportPath: "/workspace/reports/plan.md",
		Intent: testPlanRequest, BuildMaxTurns: 150, BuildTokenBudget: 800000, Limits: testPlanLimits,
		Trees: []PlanTree{
			{URL: testWebURL, Path: "/workspace/repo"},
			{URL: testAPIURL, Path: "/workspace/repos/api"},
		},
		BuildProfiles: b,
	})
}

func TestBuildProfilesRoundTrip(t *testing.T) {
	in := []BuildProfile{{URL: "https://github.com/acme/app", Profile: "go"}, {URL: "", Profile: "x"},
		{URL: "https://github.com/acme/web\n", Profile: "go\u202e"}}
	got, err := DecodeBuildProfiles(EncodeBuildProfiles(in))
	if err != nil {
		t.Fatal(err)
	}
	want := []BuildProfile{{URL: "https://github.com/acme/app", Profile: "go"},
		{URL: "https://github.com/acme/web", Profile: "go"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
	if got, err := DecodeBuildProfiles(""); err != nil || got != nil {
		t.Errorf("none decodes to %+v, %v", got, err)
	}
	if _, err := DecodeBuildProfiles("{"); err == nil {
		t.Error("broken JSON decodes")
	}
}
