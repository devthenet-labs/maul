// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"strings"
	"testing"
)

func renderGoRequirementsBody(reqs []GoRequirement) (string, error) {
	return RenderIntentPRBody(IntentPRBody{
		IntentRepository: "devthenet-labs/intents", IssueNumber: 1,
		Summary:      "Add GET /version returning {sha, built} as JSON",
		PlanRevision: 2, PlanDigest: PlanDigest([]byte(testPlan)), ApprovedBy: "peter",
		GoRequirements: reqs,
	})
}

// TestIntentPRBodyGoRequirementsGoldens pins the account an intent pull
// request gives of the Go module requirements its build added to or changed
// in go.mod, and each one that is not as the approved plan's
// new_dependencies named it: a live build shipped golang.org/x/mod v0.42.0
// where the plan approved v0.28.0, and nothing said so. With none, the body
// is exactly what it was (intent_pr_body.md).
func TestIntentPRBodyGoRequirementsGoldens(t *testing.T) {
	for _, tt := range []struct {
		name string
		reqs []GoRequirement
		want []string
	}{
		{"intent_pr_body_go_planned.md", []GoRequirement{
			{Path: "golang.org/x/mod", Version: "v0.28.0", InPlan: true, PlanVersion: "v0.28.0"},
			{Path: "golang.org/x/tools", Version: "v0.30.0", Indirect: true},
		}, []string{"`golang.org/x/mod` `v0.28.0`: added, as planned"}},
		{"intent_pr_body_go_version_differs.md", []GoRequirement{
			{Path: "golang.org/x/mod", Version: "v0.42.0", InPlan: true, PlanVersion: "v0.28.0"},
			{Path: "github.com/google/uuid", Version: "v1.6.0", Was: "v1.5.0", InPlan: true},
		}, []string{"1 of them differs from the approved plan",
			"`golang.org/x/mod` `v0.42.0`: added; **the plan named `v0.28.0`**"}},
		{"intent_pr_body_go_not_planned.md", []GoRequirement{
			{Path: "github.com/spf13/cobra", Version: "v1.9.0"},
			{Path: "golang.org/x/mod", Version: "v0.42.0", Was: "v0.41.0"},
		}, []string{"2 of them differ from the approved plan",
			"`github.com/spf13/cobra` `v1.9.0`: added; **not in the plan**",
			"`golang.org/x/mod` `v0.42.0`: changed from `v0.41.0`; **not in the plan**"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := renderGoRequirementsBody(tt.reqs)
			if err != nil {
				t.Fatal(err)
			}
			golden(t, tt.name, got)
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("body lacks %q:\n%s", w, got)
				}
			}
		})
	}
	without, err := renderGoRequirementsBody(nil)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "intent_pr_body.md", without)
}

// TestIntentPRBodyGoRequirementsAreInert: a go.mod is the build agent's
// own writing, so a module path or version that reads as a mention or an
// issue reference is broken apart like every other agent text in the body,
// and the list is bounded.
func TestIntentPRBodyGoRequirementsAreInert(t *testing.T) {
	reqs := make([]GoRequirement, 0, 1+2*GoRequirementsMax)
	reqs = append(reqs, GoRequirement{Path: "example.com/@octocat/closes#12", Version: "v1.0.0-#3"})
	for i := range 2 * GoRequirementsMax {
		reqs = append(reqs, GoRequirement{Path: "example.com/m" + strings.Repeat("x", i), Version: "v1.0.0"})
	}
	got, err := renderGoRequirementsBody(reqs)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"@octocat", "#12", "#3"} {
		if strings.Contains(got, bad) {
			t.Errorf("body holds %q:\n%s", bad, got)
		}
	}
	if n := strings.Count(got, "\n- `"); n != GoRequirementsMax {
		t.Errorf("body lists %d requirements, want at most %d", n, GoRequirementsMax)
	}
	if !strings.Contains(got, "and 21 more") {
		t.Errorf("body does not count the requirements it leaves out:\n%s", got)
	}
}
