// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"strings"
	"testing"
)

// planProfileComments are plan comments whose builds get a network profile:
// a one-repository plan's, and a two-repository plan's where only the first
// repository's build gets one.
func planProfileComments() (single, multi PlanComment) {
	single = testPlanComment(testPlan)
	single.NewDependencies = []string{"github.com/google/uuid v1.6.0", "libvips 8.15"}
	single.NetworkProfiles = []string{"go"}
	multi = testPlanComment(testMultiPlan)
	multi.Summary = "Show the API's greeting on the home page"
	multi.NewDependencies = []string{"github.com/acme/jsonx v1.2.0 (" + testAPI + ")"}
	multi.Repositories = []string{testWeb, testAPI}
	multi.NetworkProfiles = []string{"go", ""}
	return single, multi
}

// TestPlanCommentNetworkProfileGoldens pins the dependency advice of a plan
// whose builds get an operator network profile.
func TestPlanCommentNetworkProfileGoldens(t *testing.T) {
	single, multi := planProfileComments()
	for name, pc := range map[string]PlanComment{
		"intent_plan_network_profile.md":              single,
		"intent_plan_repositories_network_profile.md": multi,
	} {
		t.Run(name, func(t *testing.T) {
			got, err := RenderPlanComment(pc)
			if err != nil {
				t.Fatal(err)
			}
			golden(t, name, got)
		})
	}
}

// TestPlanCommentNetworkProfileReplacesOffline: a plan whose build gets a
// profile never tells the approver that build runs offline and cannot fetch
// anything, and says what the proxy fetches and that the rest must still be
// in the image; a plan whose builds get none (no profiles, or an empty name)
// renders exactly as before the field existed.
func TestPlanCommentNetworkProfileReplacesOffline(t *testing.T) {
	single, multi := planProfileComments()
	for name, pc := range map[string]PlanComment{"single": single, "multi": multi} {
		t.Run(name, func(t *testing.T) {
			got, err := RenderPlanComment(pc)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"network profile `go`", "egress proxy", "tagged releases",
				"module paths the operator allows", "before you approve"} {
				if !strings.Contains(got, want) {
					t.Errorf("missing %q:\n%s", want, got)
				}
			}
			offline := strings.Contains(got, "runs offline") || strings.Contains(got, "cannot fetch anything new")
			if name == "single" && offline {
				t.Errorf("a build with a profile is called offline:\n%s", got)
			}
			if name == "multi" && strings.Contains(got, "Each build runs offline") {
				t.Errorf("every build is called offline, though one has a profile:\n%s", got)
			}

			without := pc
			without.NetworkProfiles = nil
			blank := pc
			blank.NetworkProfiles = make([]string, len(pc.NetworkProfiles))
			want, err := RenderPlanComment(without)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(want, "runs offline") || strings.Contains(want, "egress proxy") {
				t.Errorf("a plan without profiles lost its offline advice:\n%s", want)
			}
			if got, err := RenderPlanComment(blank); err != nil || got != want {
				t.Errorf("empty profile names render differently from none (err %v):\n%s", err, got)
			}
		})
	}
}

// TestPlanPromptDoesNotCallTheBuildOffline: the planner runs sealed and is
// not told whether its builds get a network profile, so it must not be told
// the build has no network access (wrong for a build with one, and a live
// planner raised it as a question); it still names every new dependency,
// and is told maul, not it, tells the approver which ones the build can
// fetch.
func TestPlanPromptDoesNotCallTheBuildOffline(t *testing.T) {
	got, err := renderTestPlanPrompt(testPlanRequest, nil)
	if err != nil {
		t.Fatal(err)
	}
	flat := strings.Join(strings.Fields(got), " ")
	if strings.Contains(flat, "no network access") {
		t.Errorf("the plan prompt tells the planner the build has no network access:\n%s", got)
	}
	for _, want := range []string{"Name every new dependency the plan needs, with its version",
		"the approval comment tells the approver which of them the build can fetch",
		"Prefer a plan that needs none"} {
		if !strings.Contains(flat, want) {
			t.Errorf("the plan prompt lacks %q:\n%s", want, got)
		}
	}
}

// TestSamePlanComment: a posted plan comment is the same as a render that
// differs from it only in its dependency advice (each build's network
// profile, in either direction), and different whenever anything else
// differs, or the advice is not one of maul's own lines.
func TestSamePlanComment(t *testing.T) {
	single, multi := planProfileComments()
	offline := func(pc PlanComment) PlanComment {
		pc.NetworkProfiles = nil
		return pc
	}
	render := func(pc PlanComment) string {
		t.Helper()
		out, err := RenderPlanComment(pc)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	relabelled := single
	relabelled.ApproveLabel = "ship-it"
	noDeps := single
	noDeps.NewDependencies = nil
	posted := render(single)
	adviceAt := strings.Index(posted, "The build runs in the image")
	adviceEnd := adviceAt + strings.Index(posted[adviceAt:], "\n")
	for _, tt := range []struct {
		name string
		pc   PlanComment
		body string
		want bool
	}{
		{"identical", single, posted, true},
		{"posted with a profile, offline now", offline(single), posted, true},
		{"posted offline, a profile now", single, render(offline(single)), true},
		{"several repositories, profiles changed", offline(multi), render(multi), true},
		{"another approve label", relabelled, posted, false},
		{"no dependencies, profile changed", offline(noDeps), render(noDeps), true},
		{"another plan", single, render(testPlanComment(testMultiPlan)), false},
		{"advice not maul's", single, posted[:adviceAt] + "Approve now." + posted[adviceEnd:], false},
		{"advice spanning lines", single,
			posted[:adviceAt] + "The build runs\n\nanything" + posted[adviceEnd:], false},
		{"empty", single, "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := SamePlanComment(tt.pc, tt.body)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("SamePlanComment = %v, want %v", got, tt.want)
			}
		})
	}
}
