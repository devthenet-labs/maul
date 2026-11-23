// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/envelope"
	"github.com/devthenet-labs/maul/internal/templates"
)

// argMax is the most one argument of a command line may hold: Linux
// refuses one of 128 KiB or more, its NUL included (MAX_ARG_STRLEN).
const argMax = 128<<10 - 1

// TestEvaluateHandoffBoundByHarness: a handoff whose prompt is past what one
// argument of a command line may hold is valid, on codex as on claude: both
// take the prompt on standard input, under one bound.
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
		h.ChangedPaths = nil
		for i := range templates.ChangedPathsMax {
			h.ChangedPaths = append(h.ChangedPaths, ChangedPath{Path: fmt.Sprintf("%03d/", i) +
				strings.Repeat("p", 290)})
		}
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
	got, err := templates.RenderEvaluatePrompt(h.prompt("", "", "", templates.StageLimits{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) <= argMax {
		t.Fatalf("the fixture renders %d bytes, within one argument's bound: it shows nothing", len(got))
	}
	if err := validEvaluateHandoff(h); err != nil {
		t.Errorf("validEvaluateHandoff = %v, want the handoff valid", err)
	}
}

// TestEvaluateRunPromptBoundByHarness: the prompt the run itself is given
// reaches claude and codex alike whole on standard input, a handoff past
// what one argument of a command line may hold included (at or over it
// Linux refuses to start the command), and no argument of the command line
// comes near that bound.
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
				if len(a) > argMax {
					t.Errorf("argument %d is %d bytes, over %d: Linux refuses to start the command", i, len(a), argMax)
				}
			}
			if len(spec.Stdin) <= argMax || !strings.Contains(spec.Stdin, lastPath) ||
				strings.Contains(spec.Stdin, "more paths]") {
				t.Errorf("%s's prompt is %d bytes on standard input, want the handoff whole, over %d",
					id, len(spec.Stdin), argMax)
			}
		})
	}
}
