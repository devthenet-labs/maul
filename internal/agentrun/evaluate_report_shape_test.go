// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"bytes"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/checks"
	"github.com/devthenet-labs/maul/internal/envelope"
	"github.com/devthenet-labs/maul/internal/templates"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// liveShapedReport is a report shaped exactly like the ones live evaluators
// wrote on 0.13.7 (S1 bug 5), each verdict naming its outcome as outcome and
// each proposal naming the file it ran as file, proposing the files at
// paths (the scratch path as the agent wrote it, or relative to scratch).
func liveShapedReport(proposals ...[3]string) string {
	var b strings.Builder
	b.WriteString("---\nverdicts:\n  - outcome: \"rate-battle\"\n    verdict: pass\n    ledger: [12, 14]\n" +
		"    note: \"The rating moved by 16 points.\"\n  - outcome: \"leaderboard\"\n    verdict: unsure\n" +
		"    ledger: []\ndisputes: []\nproposed:")
	if len(proposals) == 0 {
		b.WriteString(" []")
	}
	for _, p := range proposals {
		b.WriteString("\n  - id: \"" + p[0] + "\"\n    outcome: \"" + p[1] + "\"\n    file: \"" + p[2] + "\"")
	}
	b.WriteString("\nretire: []\nanomalies: []\n---\nRated a battle.\n")
	return b.String()
}

// TestEvaluateLiveShapedReport (S1 bug 5): the live report is accepted; its
// verdicts are the outcomes' and each proposal is resolved from the file
// it names to that file's digest, which is what agent-runner asks the tools
// container to confirm (the tools container confirms only a digest it saw
// dry-run green), whether the path is the scratch path or relative to it.
func TestEvaluateLiveShapedReport(t *testing.T) {
	for name, path := range map[string]func(ws string) string{
		"absolute": func(ws string) string { return filepath.Join(ws, "scratch", "checks", "rate-battle-api.json") },
		"relative": func(string) string { return "checks/rate-battle-api.json" },
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			cfg, ws := roleConfig(t, PhaseEvaluate, evaluateHandoff, &out)
			brokeredClaude(t, &cfg)
			tools := useTools(t, &cfg)
			report := liveShapedReport([3]string{"rate-battle.api", "rate-battle", path(ws)})
			fx := &fakeExec{steps: []step{{ws: ws, stdout: streamSuccess, writes: map[string]string{
				"scratch/report.md":                   report,
				"scratch/checks/rate-battle-api.json": string(checks.Canonical(rateBattleCheck)),
			}}}}
			e := onlyEvent(t, cfg, fx, &out).Evaluation
			if e == nil || e.Outcome != envelope.OutcomeOK || e.ReportMarkdown != report {
				t.Fatalf("evaluation = %+v, want the live report accepted verbatim", e)
			}
			if len(e.Verdicts) != 2 || e.Verdicts[0].ID != "rate-battle" || e.Verdicts[0].Verdict != "pass" ||
				e.Verdicts[1].ID != "leaderboard" {
				t.Errorf("verdicts = %+v, want the outcomes'", e.Verdicts)
			}
			digest := checks.Digest(rateBattleCheck)
			if len(e.Proposed) != 1 || e.Proposed[0].Digest != digest {
				t.Errorf("proposed = %+v, want the file's digest %s", e.Proposed, digest)
			}
			var confirm toolwire.ConfirmRequest
			tools.request(t, toolwire.VerbConfirm, &confirm)
			if want := []string{strings.TrimPrefix(digest, "sha256:")}; !slices.Equal(confirm.Digests, want) {
				t.Errorf("confirm digests = %v, want the file's %v", confirm.Digests, want)
			}
		})
	}
}

// TestEvaluateUnresolvableFileDropped: a proposal whose file cannot be
// resolved (none there, not a check, another id or outcome, outside
// scratch, a digest it does not have) is dropped with its reason logged,
// never confirmed; the report and every other proposal stand.
func TestEvaluateUnresolvableFileDropped(t *testing.T) {
	good := [3]string{"rate-battle.api", "rate-battle", "checks/rate-battle-api.json"}
	for _, tt := range []struct {
		name string
		bad  [3]string
		want string
	}{
		{"no file", [3]string{"rate-battle.x", "rate-battle", "checks/missing.json"}, "cannot be read"},
		{"not a check", [3]string{"rate-battle.x", "rate-battle", "notes.txt"}, "is not a check"},
		{"another id", [3]string{"rate-battle.x", "rate-battle", "checks/page.json"}, "the id of checks/page.json"},
		{"another outcome", [3]string{"rate-battle.page", "leaderboard", "checks/page.json"},
			"the outcome of checks/page.json"},
		{"outside scratch", [3]string{"rate-battle.x", "rate-battle", "/etc/passwd"}, "not under"},
		{"escaping scratch", [3]string{"rate-battle.x", "rate-battle", "../input/issue.md"}, "cannot be read"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			cfg, ws := roleConfig(t, PhaseEvaluate, evaluateHandoff, &out)
			brokeredClaude(t, &cfg)
			tools := useTools(t, &cfg)
			page := strings.Replace(string(checks.Canonical(rateBattleCheck)), "rate-battle.api", "rate-battle.page", 1)
			report := liveShapedReport(tt.bad, good)
			fx := &fakeExec{steps: []step{{ws: ws, stdout: streamSuccess, writes: map[string]string{
				"scratch/report.md":                   report,
				"scratch/notes.txt":                   "not a check",
				"scratch/checks/page.json":            page,
				"scratch/checks/rate-battle-api.json": string(checks.Canonical(rateBattleCheck)),
			}}}}
			e := onlyEvent(t, cfg, fx, &out).Evaluation
			if e == nil || e.Outcome != envelope.OutcomeOK {
				t.Fatalf("evaluation = %+v, want ok: an unresolvable proposal refuses nothing", e)
			}
			if len(e.Proposed) != 1 || e.Proposed[0].ID != "rate-battle.api" {
				t.Errorf("proposed = %+v, want the resolvable one alone", e.Proposed)
			}
			var confirm toolwire.ConfirmRequest
			tools.request(t, toolwire.VerbConfirm, &confirm)
			want := []string{strings.TrimPrefix(checks.Digest(rateBattleCheck), "sha256:")}
			if !slices.Equal(confirm.Digests, want) || slices.Index(tools.verbs(), toolwire.VerbConfirm) !=
				len(tools.verbs())-2 {
				t.Errorf("tool calls %v, last confirm %v; want one confirm of %v", tools.verbs(), confirm.Digests, want)
			}
			if log := out.String(); !strings.Contains(log, `maul dropped proposed check \"`+tt.bad[0]+`\"`) ||
				!strings.Contains(log, tt.want) {
				t.Errorf("the transcript does not name the dropped proposal and %q:\n%s", tt.want, log)
			}
		})
	}
}

// TestEvaluateFileAndDigestMustAgree: a proposal giving both is dropped
// when its file's digest is not the one it gives.
func TestEvaluateFileAndDigestMustAgree(t *testing.T) {
	var out bytes.Buffer
	cfg, ws := roleConfig(t, PhaseEvaluate, evaluateHandoff, &out)
	brokeredClaude(t, &cfg)
	tools := useTools(t, &cfg)
	report := strings.Replace(liveShapedReport([3]string{"rate-battle.api", "rate-battle",
		"checks/rate-battle-api.json"}), "    file:", "    digest: \"sha256:"+strings.Repeat("0", 64)+"\"\n    file:", 1)
	fx := &fakeExec{steps: []step{{ws: ws, stdout: streamSuccess, writes: map[string]string{
		"scratch/report.md":                   report,
		"scratch/checks/rate-battle-api.json": string(checks.Canonical(rateBattleCheck)),
	}}}}
	e := onlyEvent(t, cfg, fx, &out).Evaluation
	if e == nil || e.Outcome != envelope.OutcomeOK || len(e.Proposed) != 0 {
		t.Fatalf("evaluation = %+v, want ok with the proposal dropped", e)
	}
	if got := tools.verbs(); !slices.Equal(got, []toolwire.Verb{toolwire.VerbFinish}) {
		t.Errorf("tool calls = %v, want finish alone", got)
	}
}

// TestEvaluateRefusedReportNamesItsKeys (diagnosability): a refused
// report's detail names its frontmatter's key paths, never its values, so
// the next failure is visible without the transcript; and the repair
// message restates the exact frontmatter example and the decoder's lines.
func TestEvaluateRefusedReportNamesItsKeys(t *testing.T) {
	var out bytes.Buffer
	cfg, ws := roleConfig(t, PhaseEvaluate, evaluateHandoff, &out)
	brokeredClaude(t, &cfg)
	bad := "---\nverdicts:\n  - outcome: rate-battle\n    verdict: pass\n    evidence: [12]\n" +
		"    note: \"SECRET-NOTE\"\n---\nbody\n"
	s := step{ws: ws, stdout: streamSuccess, writes: map[string]string{"scratch/report.md": bad}}
	fx := &fakeExec{steps: []step{s, s, s}}
	e := onlyEvent(t, cfg, fx, &out).Evaluation
	if e == nil || e.Outcome != envelope.OutcomeReportInvalid {
		t.Fatalf("evaluation = %+v, want report_invalid", e)
	}
	for _, want := range []string{"field evidence not found",
		"frontmatter keys: verdicts, verdicts[].outcome, verdicts[].verdict, verdicts[].evidence, verdicts[].note"} {
		if !strings.Contains(e.Detail, want) {
			t.Errorf("detail = %q, want %q", e.Detail, want)
		}
	}
	if strings.Contains(e.Detail, "SECRET-NOTE") {
		t.Errorf("detail %q carries a value", e.Detail)
	}
	if len(fx.specs) < 2 {
		t.Fatalf("commands = %d, want a repair", len(fx.specs))
	}
	repair := argvFlag(t, fx.specs[1].Argv, "-p")
	for _, want := range []string{strings.TrimSuffix(templates.ExampleEvaluationReport, "\n"),
		"line 5: field evidence not found"} {
		if !strings.Contains(repair, want) {
			t.Errorf("repair prompt lacks %q:\n%s", want, repair)
		}
	}
}
