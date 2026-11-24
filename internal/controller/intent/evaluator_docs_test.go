// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/templates"
)

// docsRoot is the repository root, seen from this package.
var docsRoot = filepath.Join("..", "..", "..")

// The agent evaluator's switches, commands, conditions and grants are stated
// where operators and people look for them (SPEC S1-26): each page names what
// it must, in the controllers' own words (flags, values, conditions,
// hand-off modes, the comments' sentences), so a rename in the code fails
// here before the docs drift.
func TestEvaluatorDocsStateTheSwitchesAndCommands(t *testing.T) {
	for path, wants := range map[string][]string{
		"docs/intents/evaluator.md": {
			"intentController.verification.evaluator", "acceptance.md",
			"maul-tool http", "maul-tool browser", "maul-tool exec", "maul-tool logs", "maul-tool events",
			"maul-tool files", "maul-tool run", "--base", "Look-around",
			string(v1alpha1.HandoffGated), string(v1alpha1.HandoffEarly), "handoffMax", "/maul preview",
			strings.TrimSuffix(templates.NotReadyLead, ": "), templates.ReadyLabel, templates.ObservedLead,
			templates.WithdrawnSentence, templates.OutcomeCount(3, 3),
			acceptance.ReasonSeenToHold, acceptance.ReasonSeenNotToHold, acceptance.ReasonNotReported,
			acceptance.ReasonEvaluatorUnsure, acceptance.ReasonNoValidEvidence, acceptance.ReasonHeldFinding,
			acceptance.ReasonContraryNote, "anomalies[].outcome",
			// The two residuals a reviewer is told to look at (design.json
			// residual_risks): relevance and a misread.
			"A cited call can be about something else.", "A correct answer can be misread.",
			"the evaluator's own page script", "the evaluator's own program", "Its output was not kept by maul.",
			"The evaluator's reading, not checked by maul",
			v1alpha1.ConditionEvaluatorUnverified, "--intent-evaluate-codex", "unverified",
			"maxEvaluateCostMicroUSD", "maxToolCalls", "limits.evaluate", "limits.checkReview",
			"-ledger", "-evidence", "status.ledger", "/maul verify explore",
			// Migrating from saved checks: the supersede, the retired
			// settings, the leftover stores and the rollback.
			"status.verification.judge", string(v1alpha1.JudgeObserved), v1alpha1.ConditionVerificationSuperseded,
			"maul.intent.verification_superseded", "--intent-checkreview-model", "--intent-checkreview-max-turns",
			"checkReviewModel", "checkReviewMaxTurns", "spec.models.checkReview", "-checks-", v1alpha1.LabelIntent,
			"helm rollback", "agent.paused",
		},
		"docs/intents/acceptance.md": {
			"evaluator.md", "outcomes", "from:", "kind: keep", "manual: true", "not covered",
			"/maul replan", "/maul preview", "/maul accept all", "intentController.verification.evaluator",
			"maul/acceptance", "/maul verify", "/maul verify fix", "/maul verify --reset", "/maul accept",
			"maxVerifyFixes", "maxVerifications", "manualGatesStatus", "not an attestation", "/maul verify explore",
			"cancelled for your review", "Commit statuses", templates.OutcomeCount(3, 3),
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
			"evaluateModel", "evaluateTimeout", "maxEvaluateCostUSD", "maxToolCalls",
			"checkReviewModel", "checkReviewMaxTurns", "docs/intents/evaluator.md#migrating-from-saved-checks",
			"preview.verification.tools", "maul-evaluator-labels", "agent.networkPolicy.create",
			"docs/intents/evaluator.md",
		},
		"AGENTS.md": {
			"cmd/preview-tools", "cmd/maul-tool", "cmd/cdp-shim", "cmd/tool-exec", "`toolwire`", "`toolexec`",
			"`cdpshim`", "`maultool`", "`previewtools`",
			"--intent-evaluator", "docs/intents/evaluator.md", "acceptance.Judge", "acceptance.ValidCite",
			"verify_migrate.go", "/maul verify explore",
		},
		"zensical.toml": {"intents/evaluator.md"},
	} {
		t.Run(path, func(t *testing.T) {
			prose := docsProse(t, path)
			for _, want := range wants {
				if !strings.Contains(prose, want) {
					t.Errorf("%s does not state %q", path, want)
				}
			}
		})
	}
}

// What the evaluator no longer has (saved checks, their reviewer, the cost
// ladder, disputes, suspected defects, the quarantine retry and the base
// as proof) is described nowhere as how it works: the evaluator's page
// names it only in its migration section, and the acceptance page and the
// orientation map not at all.
func TestEvaluatorDocsDropTheSavedCheckMachinery(t *testing.T) {
	retired := []string{
		`maul-tool check`, `check results`, `\bcheck ?review`, `\breplay`, `\bdisput`, `\brung\b`, `escalat`,
		`suspected`, `quarantin`, `\{\{run\}\}`, "`proves`", `proves? (it|its|the) outcome`, `saved check`,
		`red by assertion`,
		`base comparison`, `no-base-evidence`, `\bconfirm(s|ed|ing)?\b`, `cost ladder`,
	}
	agents := []string{
		`acceptance\.Decide`, "`Decide`", `StartRung`, `checkstore\.go`, `rung\.go`, `BaseRuns`, `ReplayRuns`,
		`ConfirmedChecks`, `ConfirmationRuns`, `report\.CheckReview`, `maul-tool check`, `check results`,
		`evaluate_cover`, `checkreview\.go`, `the check reviewer`, `saved checks? (replay|is|are)`,
	}
	for path, terms := range map[string][]string{
		"docs/intents/evaluator.md":  retired,
		"docs/intents/acceptance.md": retired,
		"AGENTS.md":                  agents,
	} {
		t.Run(path, func(t *testing.T) {
			prose := docsProse(t, path)
			if path == "docs/intents/evaluator.md" {
				prose = withoutSection(t, prose, "## Migrating from saved checks")
			}
			// A link to the migration section names what it is about.
			prose = strings.ReplaceAll(prose, "[Migrating from saved checks]", "[the migration]")
			for _, term := range terms {
				if m := regexp.MustCompile(`(?i).{0,60}` + term + `.{0,60}`).FindString(prose); m != "" {
					t.Errorf("%s still describes %q: ...%s...", path, term, m)
				}
			}
		})
	}
}

// TestAcceptanceLeadSaysWhoLooks (FC 5): the acceptance page's lead, the
// first thing a reader takes from it, says that an agent evaluator uses the
// change and reports what it saw, and that what a cited call shows is the
// reviewers' to judge: never that maul checks the work, nor any form of
// check, confirm, verify or prove as what maul does to it.
func TestAcceptanceLeadSaysWhoLooks(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(docsRoot, "docs/intents/acceptance.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, after, _ := strings.Cut(string(data), "# Acceptance verification\n")
	lead, _, _ := strings.Cut(strings.TrimLeft(after, "\n"), "\n\n")
	if lead == "" {
		t.Fatal("docs/intents/acceptance.md has no lead paragraph")
	}
	if m := regexp.MustCompile(`(?i)\b(check\w*|confirm\w*|verif(y|ies|ied)|proves?|proof)\b`).FindString(
		lead); m != "" {
		t.Errorf("the acceptance page's lead says %q: %s", m, lead)
	}
	flat := strings.Join(strings.Fields(lead), " ")
	for _, want := range []string{"agent evaluator", "reviewers"} {
		if !strings.Contains(flat, want) {
			t.Errorf("the acceptance page's lead does not say %q: %s", want, lead)
		}
	}
}

// Every link into the evaluator's and the acceptance page, from the docs,
// the chart (its fail-render messages included), the controllers' startup
// messages and the pages themselves, lands on a heading those pages have:
// a section deleted or renamed fails here, not in a reader's browser.
func TestEvaluatorDocAnchorsResolve(t *testing.T) {
	pages := map[string]string{"evaluator.md": "docs/intents/evaluator.md", "acceptance.md": "docs/intents/acceptance.md"}
	anchors := map[string]map[string]bool{}
	for name, path := range pages {
		anchors[name] = headingSlugs(t, path)
	}
	link := regexp.MustCompile(`\b(evaluator|acceptance)\.md#([A-Za-z0-9_-]+)`)
	inPage := regexp.MustCompile(`\]\(#([A-Za-z0-9_-]+)\)`)
	seen := 0
	err := filepath.WalkDir(docsRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".mise", "node_modules", "testdata", "bin", "dist", "site", ".venv":
				return filepath.SkipDir
			}
			return nil
		}
		switch filepath.Ext(path) {
		case ".md", ".go", ".yaml", ".yml", ".tpl", ".tmpl", ".ts", ".json", ".toml":
		default:
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(docsRoot, path)
		for _, m := range link.FindAllStringSubmatch(string(data), -1) {
			seen++
			if !anchors[m[1]+".md"][m[2]] {
				t.Errorf("%s links to %s.md#%s, which has no such heading", rel, m[1], m[2])
			}
		}
		for name, page := range pages {
			if filepath.ToSlash(rel) != page {
				continue
			}
			for _, m := range inPage.FindAllStringSubmatch(string(data), -1) {
				seen++
				if !anchors[name][m[1]] {
					t.Errorf("%s links to #%s, which has no such heading", rel, m[1])
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen < 20 {
		t.Fatalf("found only %d anchor links: the walk did not reach the tree", seen)
	}
	// The fail-render guard and intent-controller's startup warning name
	// this section; the walk above checks the link, this its heading.
	if !anchors["evaluator.md"]["migrating-from-saved-checks"] {
		t.Error("docs/intents/evaluator.md has no heading Migrating from saved checks")
	}
}

// docsProse is the file at path (from the repository root) with its
// whitespace folded to single spaces.
func docsProse(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(docsRoot, path))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(strings.Fields(string(data)), " ")
}

// withoutSection is prose less the level-2 section headed heading, up to
// the next level-2 heading or the end.
func withoutSection(t *testing.T, prose, heading string) string {
	t.Helper()
	before, rest, ok := strings.Cut(prose, heading+" ")
	if !ok {
		t.Fatalf("no section %q", heading)
	}
	if _, after, more := strings.Cut(rest, " ## "); more {
		return before + " ## " + after
	}
	return before
}

// headingSlugs are the anchors of path's headings, as Python-Markdown's
// toc extension (the docs site's) makes them: punctuation dropped, lower
// case, runs of spaces and hyphens one hyphen.
func headingSlugs(t *testing.T, path string) map[string]bool {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(docsRoot, path))
	if err != nil {
		t.Fatal(err)
	}
	drop, join := regexp.MustCompile(`[^\w\s-]`), regexp.MustCompile(`[-\s]+`)
	out := map[string]bool{}
	fenced := false
	for line := range strings.Lines(string(data)) {
		if strings.HasPrefix(line, "```") {
			fenced = !fenced
		}
		title, ok := strings.CutPrefix(strings.TrimLeft(line, "#"), " ")
		if fenced || !ok || !strings.HasPrefix(line, "#") {
			continue
		}
		slug := strings.ToLower(strings.TrimSpace(drop.ReplaceAllString(title, "")))
		out[join.ReplaceAllString(slug, "-")] = true
	}
	return out
}
