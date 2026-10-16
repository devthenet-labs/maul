// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

// repoFile reads a file relative to the repository root.
func repoFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", path))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// fenceAfter is the body of the first fenced code block after heading in a
// markdown document.
func fenceAfter(t *testing.T, doc, heading string) string {
	t.Helper()
	_, rest, ok := strings.Cut(doc, heading)
	if !ok {
		t.Fatalf("heading %q not found", heading)
	}
	_, rest, ok = strings.Cut(rest, "```yaml\n")
	if !ok {
		t.Fatalf("no yaml block after %q", heading)
	}
	body, _, ok := strings.Cut(rest, "```")
	if !ok {
		t.Fatalf("unterminated yaml block after %q", heading)
	}
	return body
}

// sizingBrokerLimits is the patchy chart values block of the sizing guide.
type sizingBrokerLimits struct {
	EgressBroker struct {
		Limits struct {
			Unit        string `json:"unit"`
			CostPerPod  int64  `json:"costPerPod"`
			CostPerHour int64  `json:"costPerHour"`
		} `json:"limits"`
	} `json:"egressBroker"`
}

// largePresetLimits are the "large" preset's Project limits as published:
// the sizing guide's Project and the commented preset in patchy-config's
// values.yaml.
func largePresetLimits(t *testing.T) map[string]v1alpha1.ProjectLimits {
	t.Helper()
	out := map[string]v1alpha1.ProjectLimits{}

	var guide struct {
		Projects []struct {
			Spec v1alpha1.ProjectSpec `json:"spec"`
		} `json:"projects"`
	}
	doc := repoFile(t, "docs/intents/sizing.md")
	if err := yaml.Unmarshal([]byte(fenceAfter(t, doc, "### A large Project")), &guide); err != nil {
		t.Fatal(err)
	}
	if len(guide.Projects) != 1 {
		t.Fatalf("the guide shows %d Projects, want 1", len(guide.Projects))
	}
	out["docs/intents/sizing.md"] = guide.Projects[0].Spec.Limits

	// The chart comment: the lines after the preset's heading, up to the
	// next key of the Project, each under the same comment prefix.
	values := repoFile(t, "charts/patchy-config/values.yaml")
	_, rest, ok := strings.Cut(values, `The "large" preset`)
	if !ok {
		t.Fatal("patchy-config values.yaml has no large preset")
	}
	const prefix = "#       #     # "
	var preset []string
	for _, line := range strings.Split(rest, "\n")[2:] {
		after, ok := strings.CutPrefix(line, prefix)
		if !ok {
			break
		}
		preset = append(preset, after)
	}
	var chart v1alpha1.ProjectLimits
	if err := yaml.UnmarshalStrict([]byte(strings.Join(preset, "\n")), &chart); err != nil {
		t.Fatalf("the values.yaml preset %q: %v", preset, err)
	}
	out["charts/patchy-config/values.yaml"] = chart
	return out
}

// TestLargePresetProjectIsReady: a Project carrying the published "large"
// preset is Ready under the broker limits the sizing guide sets beside it,
// and none of its run budgets is above the guide's per-pod cost limit. A
// stage that sets no budget runs on the per-intent ceiling, so the preset
// must budget every stage (the plan once inherited $150 against a $200
// hourly limit and the Project was never Ready).
func TestLargePresetProjectIsReady(t *testing.T) {
	var broker sizingBrokerLimits
	doc := repoFile(t, "docs/intents/sizing.md")
	if err := yaml.Unmarshal([]byte(fenceAfter(t, doc, "### patchy chart values")), &broker); err != nil {
		t.Fatal(err)
	}
	limits := broker.EgressBroker.Limits
	if limits.Unit != "cost" || limits.CostPerHour <= 0 || limits.CostPerPod <= 0 {
		t.Fatalf("the guide's broker limits = %+v, want the cost unit with both cost limits", limits)
	}
	for source, preset := range largePresetLimits(t) {
		t.Run(source, func(t *testing.T) {
			p := testProject()
			p.Spec.Limits = preset
			for _, stage := range []v1alpha1.IntentStage{
				v1alpha1.IntentStagePlan, v1alpha1.IntentStageBuild, v1alpha1.IntentStageRevise,
			} {
				if b := runBudget(p, stage); b > limits.CostPerPod {
					t.Errorf("%s run budget %d is above the guide's costPerPod %d", stage, b, limits.CostPerPod)
				}
			}
			e := newEnv(t, p)
			e.costBudgets(limits.CostPerHour)
			e.reconcileProject()
			c := meta.FindStatusCondition(e.getProject().Status.Conditions, v1alpha1.ConditionReady)
			if c == nil || c.Status != metav1.ConditionTrue {
				t.Errorf("Ready = %+v, want True under costPerHour %d", c, limits.CostPerHour)
			}
		})
	}
}

// mathDollar finds a dollar sign arithmatex would read as inline math
// delimiter: any unescaped one outside code.
var mathDollar = regexp.MustCompile(`(^|[^\\])\$`)

// TestSizingDocsEscapeDollars: the docs site enables pymdownx.arithmatex
// (generic), whose smart-dollar syntax turns "$110 per ... ($5" into a
// math span and drops the prices. Every dollar in the prose of the pages
// that quote prices is escaped; code blocks and spans are left alone.
func TestSizingDocsEscapeDollars(t *testing.T) {
	for _, path := range []string{
		"docs/intents/sizing.md",
		"docs/configuration/intent-controller.md",
	} {
		t.Run(path, func(t *testing.T) {
			fence := false
			for i, line := range strings.Split(repoFile(t, path), "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), "```") {
					fence = !fence
					continue
				}
				if fence {
					continue
				}
				parts := strings.Split(line, "`")
				for j := 0; j < len(parts); j += 2 {
					if mathDollar.MatchString(parts[j]) {
						t.Errorf("line %d has an unescaped $ (arithmatex reads it as math): %s", i+1, line)
						break
					}
				}
			}
		})
	}
}
