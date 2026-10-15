// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"strings"
	"testing"
)

// The paragraph a profile run is told instead of "no network access", as
// fragments that survive the templates' line wrapping.
var networkRouteFragments = []string{
	"the operator's dependency route `go`",
	"tagged releases of module paths the operator allows",
	"`410 Gone`",
	"modules already in this image still build",
	"branch and commit versions are refused",
	"Nothing else on the network is reachable",
	"never disable checksum verification",
	"Commit `go.mod` and",
	"stop and report it",
}

func renderRemediateProfile(profile string) (string, error) {
	return RenderRemediatePrompt(RemediatePrompt{
		IssuePath:         "/workspace/input/issue.md",
		InvestigationPath: "/workspace/input/investigation.md",
		ReportPath:        "/workspace/reports/remediation.md",
		CommitScriptPath:  "/workspace/commit.sh",
		NetworkProfile:    profile,
	})
}

func renderBuildProfile(profile string) (string, error) {
	return RenderBuildPrompt(BuildPrompt{
		PlanPath:         "/workspace/input/investigation.md",
		ReportPath:       "/workspace/reports/build.md",
		CommitScriptPath: "/workspace/commit.sh",
		Limits:           testBuildLimits,
		NetworkProfile:   profile,
	})
}

// TestNetworkProfilePromptGoldens pins the remediate and build prompts of a
// run whose pod carries a network profile.
func TestNetworkProfilePromptGoldens(t *testing.T) {
	for _, tt := range []struct {
		name   string
		render func(string) (string, error)
	}{
		{"prompt_remediate_network_profile.md", renderRemediateProfile},
		{"prompt_build_network_profile.md", renderBuildProfile},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.render("go")
			if err != nil {
				t.Fatal(err)
			}
			golden(t, tt.name, got)
		})
	}
}

// TestNetworkProfilePromptReplacesOffline: a profile run is told about the
// route and never that it has no network, and the build is not told to stop
// over a dependency the image lacks (the route is how it gets one); a run
// without a profile is told exactly what it was before the field existed
// (the prompt_remediate.md and prompt_build.md goldens).
func TestNetworkProfilePromptReplacesOffline(t *testing.T) {
	for _, tt := range []struct {
		name    string
		render  func(string) (string, error)
		offline string
	}{
		{"remediate", renderRemediateProfile, "no network access"},
		{"build", renderBuildProfile, "**no\n  network access**"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			with, err := tt.render("go")
			if err != nil {
				t.Fatal(err)
			}
			flat := strings.Join(strings.Fields(with), " ")
			for _, want := range networkRouteFragments {
				if !strings.Contains(flat, strings.Join(strings.Fields(want), " ")) {
					t.Errorf("profile prompt lacks %q", want)
				}
			}
			for _, banned := range []string{"no network access", "**no network access**", "the image lacks"} {
				if strings.Contains(flat, banned) {
					t.Errorf("profile prompt still says %q", banned)
				}
			}
			if n := strings.Count(flat, "stop and report it"); n != 1 {
				t.Errorf("profile prompt says stop and report %d times, want once", n)
			}

			without, err := tt.render("")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(without, tt.offline) || strings.Contains(without, "dependency route") {
				t.Errorf("a prompt without a profile lost its offline sentence or gained the route:\n%s", without)
			}
		})
	}
}

// TestNetworkProfilePRBodyGoldens pins the line a Finding's and an
// intent's pull request carry when the run that built them had a network
// profile. Without one each body is what it was (pr_body.md,
// intent_pr_body.md).
func TestNetworkProfilePRBodyGoldens(t *testing.T) {
	for _, tt := range []struct {
		name   string
		render func(profile string) (string, error)
	}{
		{"pr_body_network_profile.md", func(profile string) (string, error) {
			return PRBody(123, "remediation report here", false, nil, profile)
		}},
		{"intent_pr_body_network_profile.md", func(profile string) (string, error) {
			return RenderIntentPRBody(IntentPRBody{
				IntentRepository: "devthenet-labs/intents", IssueNumber: 1,
				Summary:      "Add GET /version returning {sha, built} as JSON",
				PlanRevision: 2, PlanDigest: PlanDigest([]byte(testPlan)), ApprovedBy: "peter",
				NetworkProfile: profile,
			})
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.render("go")
			if err != nil {
				t.Fatal(err)
			}
			golden(t, tt.name, got)
			if !strings.Contains(got, "network profile `go`") {
				t.Errorf("body does not name the profile:\n%s", got)
			}
			without, err := tt.render("")
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(without, "network profile") {
				t.Errorf("a body without a profile mentions one:\n%s", without)
			}
			// Exactly one line more: the profile line and its blank line.
			if strings.Count(got, "\n")-strings.Count(without, "\n") != 2 {
				t.Errorf("the profile added more than its line:\n--- without\n%s\n--- with\n%s", without, got)
			}
		})
	}
}
