// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/api/v1alpha1"
)

// The agent evaluator's switches, commands, conditions and grants are stated
// where operators and people look for them (SPEC S1-26): each page names what
// it must, in the controllers' own words (flags, values, conditions,
// hand-off modes), so a rename in the code fails here before the docs drift.
func TestEvaluatorDocsStateTheSwitchesAndCommands(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	for path, wants := range map[string][]string{
		"docs/intents/evaluator.md": {
			"intentController.verification.evaluator", "acceptance.md",
			"maul-tool http", "maul-tool browser", "maul-tool exec", "maul-tool logs", "maul-tool events",
			"maul-tool check run", "{{run}}", "proves", "dispute", "Look-around",
			string(v1alpha1.HandoffGated), string(v1alpha1.HandoffEarly), "handoffMax", "/maul preview",
			"maul couldn't confirm this", "Ready: maul checked the approved outcomes",
			v1alpha1.ConditionEvaluatorUnverified, "--intent-evaluate-codex", "unverified",
			"maxEvaluateCostMicroUSD", "maxToolCalls", "limits.evaluate", "limits.checkReview",
			"-ledger", "-evidence", "-checks-", "status.ledger",
		},
		"docs/intents/acceptance.md": {
			"evaluator.md", "outcomes", "from:", "kind: keep", "manual: true", "not covered",
			"/maul replan", "/maul preview", "/maul accept all", "intentController.verification.evaluator",
			"maul/acceptance", "/maul verify", "/maul verify fix", "/maul verify --reset", "/maul accept",
			"maxVerifyFixes", "maxVerifications", "manualGatesStatus", "not an attestation",
			"cancelled for your review", "Commit statuses", "{{run}}",
		},
		"docs/configuration/preview-controller.md": {
			"--preview-evaluator", "--preview-tools-image", "--preview-browser-image", "--preview-toolbox-image",
			"--preview-agents-namespace", "--preview-max-evaluations", "preview.verification.tools",
			"preview-tools-isolation", "maul-preview-tool-objects", "maul-preview-ledgers", "`events`",
			"`secrets`", "`configmaps`", "status.tools", "2.5 CPU", "evaluateTimeout",
		},
		"docs/configuration/intent-controller.md": {
			"--intent-evaluator", "--intent-evaluate-model", "--intent-checkreview-model",
			"--intent-evaluate-timeout", "--intent-default-handoff", "--intent-handoff-max",
			"--intent-evaluate-codex", "--intent-evaluation-cost-max", "--intent-max-tool-calls",
		},
		"charts/maul/README.md": {
			"intentController.verification.evaluator", "defaultHandoff", "handoffMax", "codexEvaluator",
			"evaluateModel", "checkReviewModel", "evaluateTimeout", "maxEvaluateCostUSD", "maxToolCalls",
			"preview.verification.tools", "maul-evaluator-labels", "agent.networkPolicy.create",
			"docs/intents/evaluator.md",
		},
		"AGENTS.md": {
			"cmd/preview-tools", "cmd/maul-tool", "cmd/cdp-shim", "cmd/tool-exec", "`checks`",
			"`checks/playwright`", "`toolwire`", "`toolexec`", "`cdpshim`", "`maultool`", "`previewtools`",
			"--intent-evaluator", "docs/intents/evaluator.md",
		},
		"zensical.toml": {"intents/evaluator.md"},
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
