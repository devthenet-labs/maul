// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"bytes"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/envelope"
	"github.com/devthenet-labs/maul/internal/provider"
)

// codexIntentConfig is an intent stage's config on brokered codex: the
// caller token file, the broker's openai route, and the codex models, with
// the run's log captured.
func codexIntentConfig(t *testing.T, phase Phase, out, log *bytes.Buffer) (Config, string) {
	t.Helper()
	cfg, ws := intentConfig(t, phase, out)
	cfg.InvestigateHarness, cfg.RemediateHarness = "codex", "codex"
	cfg.InvestigateModel, cfg.RemediateModel = "openai/gpt-5.3-codex", "openai/gpt-5.3-codex"
	cfg.BrokerTokenFile, cfg.CodexBaseURL = codexTokenFile(t), "http://broker.maul.svc:8080/openai/v1"
	cfg.Log = slog.New(slog.NewTextHandler(log, nil))
	return cfg, ws
}

// TestIntentStagesOnBrokeredCodex: an intent's plan and build run on
// brokered codex as they do on claude: codex exec with the broker as its
// provider, the caller token in the environment only, the codex model id,
// the stage's report read and, for a build, the changeset packaged; the
// plan's prompt keeps the read-only rule as the planner's own, since codex
// does not enforce it.
func TestIntentStagesOnBrokeredCodex(t *testing.T) {
	for _, tt := range []struct {
		phase   Phase
		writes  map[string]string
		repo    map[string]string
		outcome func(envelope.Event) (envelope.Stage, bool)
	}{
		{PhasePlan, map[string]string{"reports/plan.md": goodPlan}, nil,
			func(ev envelope.Event) (envelope.Stage, bool) {
				if ev.Plan == nil {
					return envelope.Stage{}, false
				}
				return ev.Plan.Stage, ev.Plan.ReportMarkdown == goodPlan
			}},
		{PhaseBuild, map[string]string{"reports/build.md": goodBuild, "commit.sh": buildCommitScript},
			map[string]string{"app.js": "version();\n"},
			func(ev envelope.Event) (envelope.Stage, bool) {
				if ev.Remediation == nil {
					return envelope.Stage{}, false
				}
				return ev.Remediation.Stage, ev.Remediation.Success && ev.Remediation.Changeset != nil
			}},
	} {
		t.Run(string(tt.phase), func(t *testing.T) {
			var out, log bytes.Buffer
			cfg, ws := codexIntentConfig(t, tt.phase, &out, &log)
			fx := &fakeExec{steps: []step{{ws: ws, stdout: codexStream, writes: tt.writes, repoWrite: tt.repo}}}
			ev := onlyEvent(t, cfg, fx, &out)
			stage, done := tt.outcome(ev)
			if stage.Outcome != envelope.OutcomeOK || !done || stage.Harness != "codex" ||
				stage.Model != "openai/gpt-5.3-codex" {
				t.Fatalf("event = %+v, want an ok %s on codex", ev, tt.phase)
			}
			if len(fx.specs) != 1 {
				t.Fatalf("commands run = %d, want 1", len(fx.specs))
			}
			spec := fx.specs[0]
			argv := strings.Join(spec.Argv, "\n")
			if spec.Argv[0] != "codex" || spec.Argv[1] != "exec" || !slices.Contains(spec.Argv, "gpt-5.3-codex") {
				t.Errorf("argv = %q, want codex exec on gpt-5.3-codex", spec.Argv)
			}
			if !strings.Contains(argv, `model_providers.maul.base_url="http://broker.maul.svc:8080/openai/v1"`) {
				t.Errorf("argv = %q, want the broker's openai route as codex's provider", spec.Argv)
			}
			if !slices.Contains(spec.Env, provider.CodexTokenEnv+"=codex-token") || strings.Contains(argv, "codex-token") {
				t.Errorf("the caller token is not in the env alone: env %v", spec.Env)
			}
			if tt.phase == PhasePlan && (!strings.Contains(argv, "This stage is read-only, and that is yours to keep") ||
				strings.Contains(argv, "the sandbox holds you to it")) {
				t.Error("the codex plan prompt claims a sandbox codex does not enforce")
			}
		})
	}
}

// TestCodexIntentStagesDegradeCleanly: codex can neither continue a session
// nor follow a running command, so on an intent stage a refused report ends
// the stage as it did before repair existed (one run, report_invalid), and
// the run says so in its log, as it says the command output is shown only
// with its result.
func TestCodexIntentStagesDegradeCleanly(t *testing.T) {
	for _, phase := range []Phase{PhasePlan, PhaseBuild} {
		t.Run(string(phase), func(t *testing.T) {
			var out, log bytes.Buffer
			cfg, ws := codexIntentConfig(t, phase, &out, &log)
			writes := map[string]string{"reports/plan.md": badPlan}
			if phase == PhaseBuild {
				writes = map[string]string{"reports/build.md": scalarNotesBuild, "commit.sh": buildCommitScript}
			}
			fx := &fakeExec{steps: []step{{ws: ws, stdout: codexStream, writes: writes}}}
			stage := eventStage(t, onlyEvent(t, cfg, fx, &out))
			if stage.Outcome != envelope.OutcomeReportInvalid || len(fx.specs) != 1 {
				t.Errorf("outcome %q after %d commands, want report_invalid after 1", stage.Outcome, len(fx.specs))
			}
			for _, want := range []string{"cannot continue a session", "cannot follow a running command"} {
				if !strings.Contains(log.String(), want) {
					t.Errorf("log lacks %q:\n%s", want, log.String())
				}
			}
		})
	}
}
