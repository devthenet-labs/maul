// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The agents-namespace Role deliberately copies the agent-jobs grant. Keep
// every operator-facing description explicit about its Secret scope.
func TestAgentSecretGrantIsDocumented(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	for _, path := range []string{
		"AGENTS.md",
		"DESIGN.md",
		"deploy/README.md",
		"docs/deployment/kustomize.md",
		"docs/configuration/intent-controller.md",
		"docs/design/intent-driven-development.md",
		"charts/maul/templates/NOTES.txt",
		"charts/maul/templates/intent-controller.yaml",
		"deploy/kustomize/components/intent-controller/rbac.yaml",
	} {
		t.Run(path, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(root, path))
			if err != nil {
				t.Fatal(err)
			}
			prose := strings.Join(strings.Fields(string(data)), " ")
			if !strings.Contains(prose, "any Secret in the agents namespace") {
				t.Error("the agents-namespace unrestricted Secret grant is not stated")
			}
		})
	}
}

// Acceptance verification widens preview-controller's slot Role (Jobs,
// NetworkPolicies, pods/log) and gives it and intent-controller the
// PreviewVerification grants. The chart's header states every one, and that
// they render only with verification on.
func TestPreviewVerificationGrantsAreDocumented(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	data, err := os.ReadFile(filepath.Join(root, "charts/maul/templates/preview-controller.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var header []string
	for line := range strings.SplitSeq(string(data), "\n") {
		if !strings.HasPrefix(line, "#") {
			break
		}
		header = append(header, strings.TrimPrefix(line, "#"))
	}
	prose := strings.Join(strings.Fields(strings.Join(header, " ")), " ")
	for _, want := range []string{"preview.verification.enabled", "jobs", "networkpolicies", "pods (delete", "pods/log",
		"previewverifications", "previewverifications/status"} {
		if !strings.Contains(prose, want) {
			t.Errorf("the preview-controller header does not state %q", want)
		}
	}
}

// The operator reference states the same grants in prose, beside the chart
// switch that renders them, and the acceptance guide the people's side.
func TestPreviewVerificationDocsStateTheGrantsAndCommands(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	for path, wants := range map[string][]string{
		"docs/configuration/preview-controller.md": {"preview.verification.enabled", "`jobs`", "`networkpolicies`",
			"`pods` (delete", "`pods/log`", "`previewverifications`", "`previewverifications/status`",
			"--preview-tester-image", "--preview-verify-deadline-max", "preview.verification.deadlineMaxSeconds"},
		"docs/configuration/intent-controller.md": {"--intent-verification", "--intent-verify-timeout-max",
			"previewverifications", "maul/acceptance"},
		"docs/intents/acceptance.md": {"maul/acceptance", "/maul verify", "/maul verify fix", "/maul verify --reset",
			"/maul accept", "maxVerifyFixes", "maxVerifications", "manualGatesStatus", "not an attestation",
			"cancelled for your review", "Commit statuses", "{{run}}"},
		"docs/intents/set-up-previews.md": {"acceptance.md", "preview.verification.enabled"},
	} {
		t.Run(path, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(root, path))
			if err != nil {
				t.Fatal(err)
			}
			prose := strings.Join(strings.Fields(string(data)), " ")
			for _, want := range wants {
				if !strings.Contains(prose, want) {
					t.Errorf("%s does not state %q", path, want)
				}
			}
		})
	}
}
