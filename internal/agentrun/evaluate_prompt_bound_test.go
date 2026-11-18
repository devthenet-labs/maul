// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"strings"
	"testing"

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
