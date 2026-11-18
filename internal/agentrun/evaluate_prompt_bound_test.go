// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/envelope"
	"github.com/devthenet-labs/maul/internal/templates"
)

// TestPromptOnArgv: the evaluate prompt's bound follows how the run's
// harness takes its prompt, read from the harness's own command: claude
// reads it on standard input, codex takes it as one argument of `codex
// exec`, which Linux bounds at 128 KiB; the fake replays a fixture and
// takes none. A harness maul does not know gets the command line's bound,
// the smaller.
func TestPromptOnArgv(t *testing.T) {
	for id, want := range map[string]bool{"claude": false, "codex": true, "fake": false, "nope": true} {
		if got := promptOnArgv(id); got != want {
			t.Errorf("promptOnArgv(%q) = %v, want %v", id, got, want)
		}
	}
}

// TestEvaluateHandoffBoundByHarness: a handoff whose prompt fits only the
// standard input's bound is valid for claude and refused for codex before
// any run, never cut.
func TestEvaluateHandoffBoundByHarness(t *testing.T) {
	var h EvaluateHandoff
	if err := decodeHandoff([]byte(evalHandoff(func(h *EvaluateHandoff) {
		h.Request = strings.Repeat("r", templates.RequestQuoteMaxBytes)
		h.FixDiff = strings.Repeat("d", templates.FixDiffMaxBytes)
		h.PriorRound = 1
		for _, o := range h.Outcomes {
			h.PriorVerdicts = append(h.PriorVerdicts, PriorVerdict{Outcome: o.ID, Result: "unsure",
				Reason: strings.Repeat("v", 600)})
		}
		h.Outcomes[0].Outcome = strings.Repeat("o", 500)
	})), "evaluate", &h); err != nil {
		t.Fatal(err)
	}
	for len(h.Outcomes) < 20 {
		o := h.Outcomes[0]
		o.ID = "o-" + strings.Repeat("x", len(h.Outcomes))
		o.From = strings.Repeat("f", 300)
		h.Outcomes = append(h.Outcomes, o)
		h.PriorVerdicts = append(h.PriorVerdicts, PriorVerdict{Outcome: o.ID, Result: "unsure",
			Reason: strings.Repeat("v", 600)})
	}
	p := h.prompt("", "", "", templates.StageLimits{})
	p.OnArgv = true
	if _, err := templates.RenderEvaluatePrompt(p); err == nil {
		t.Fatal("the fixture fits the command line's bound: it cannot tell the two apart")
	}
	if err := validEvaluateHandoff(h, promptOnArgv("claude")); err != nil {
		t.Errorf("claude: %v", err)
	}
	if err := validEvaluateHandoff(h, promptOnArgv("codex")); err == nil {
		t.Error("codex: a prompt over the command line's bound is valid")
	}
}

// TestEvaluateRunPromptBoundByHarness: the prompt the run itself is given
// follows its harness too, not only the handoff's check before it. A
// handoff over the command line's bound only by parts the prompt may leave
// out reaches claude whole on its standard input, and codex as one argument
// under 128 KiB (at or over it Linux refuses to start the command), saying
// what it left out.
func TestEvaluateRunPromptBoundByHarness(t *testing.T) {
	handoff := evalHandoff(func(h *EvaluateHandoff) {
		h.Request = strings.Repeat("r", templates.RequestQuoteMaxBytes)
		h.FixDiff = strings.Repeat("d", templates.FixDiffMaxBytes)
		h.ChangedPaths = nil
		for i := range templates.ChangedPathsMax {
			h.ChangedPaths = append(h.ChangedPaths, ChangedPath{Path: fmt.Sprintf("%03d/", i) + strings.Repeat("p", 290)})
		}
	})
	const lastPath = "199/"
	for _, id := range []string{"claude", "codex"} {
		t.Run(id, func(t *testing.T) {
			var out bytes.Buffer
			cfg, ws := roleConfig(t, PhaseEvaluate, handoff, &out)
			brokeredClaude(t, &cfg)
			stdout := streamSuccess
			if id == "codex" {
				cfg.InvestigateHarness, cfg.InvestigateModel = "codex", "openai/gpt-5.3-codex"
				cfg.BrokerTokenFile, cfg.CodexBaseURL = codexTokenFile(t), "http://broker.maul.svc:8080/openai/v1"
				stdout = codexStream
			}
			fx := &fakeExec{steps: []step{{ws: ws, stdout: stdout,
				writes: map[string]string{"scratch/report.md": goodEvaluation}}}}
			if e := onlyEvent(t, cfg, fx, &out).Evaluation; e == nil || e.Outcome != envelope.OutcomeOK {
				t.Fatalf("evaluation = %+v, want ok", e)
			}
			spec := fx.specs[0]
			for i, a := range spec.Argv {
				if len(a) > templates.EvaluatePromptArgvMaxBytes {
					t.Errorf("argument %d is %d bytes, over %d: Linux refuses to start the command", i, len(a),
						templates.EvaluatePromptArgvMaxBytes)
				}
			}
			if id == "claude" {
				if len(spec.Stdin) <= templates.EvaluatePromptArgvMaxBytes || !strings.Contains(spec.Stdin, lastPath) {
					t.Errorf("claude's prompt is %d bytes on standard input, want the handoff whole, over %d",
						len(spec.Stdin), templates.EvaluatePromptArgvMaxBytes)
				}
				return
			}
			prompt := slices.MaxFunc(spec.Argv, func(a, b string) int { return len(a) - len(b) })
			if !strings.Contains(prompt, "more paths]") || strings.Contains(prompt, lastPath) ||
				!strings.Contains(prompt, strings.Repeat("d", templates.FixDiffMaxBytes)) {
				t.Errorf("codex's prompt (%d bytes) does not say it left changed paths out, or lost the fix diff",
					len(prompt))
			}
		})
	}
}
