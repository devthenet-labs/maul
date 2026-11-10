// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package broker

import (
	"bytes"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
)

// claudeRequestFixture is a Messages request the real claude CLI sent
// (2.1.322, the release Dockerfile.claude-agent-runner pins) with
// --model claude-sonnet-5-5 in the read-only posture, to the posture
// probe's loopback model (TestClaudePostureProbe): the body with every text,
// tool result and tool description elided and the probe's paths shortened,
// and beside it the anthropic-beta header it carried.
const (
	claudeRequestFixture = "testdata/claude-2.1.322-messages-request.json"
	claudeBetasFixture   = "testdata/claude-2.1.322-messages-betas.txt"
)

// TestInspectClaudeCodeRequest runs that request through the Anthropic
// inspection. On the 5.5 models the CLI sends adaptive thinking with
// display "updates", an output_config effort, context management, a
// mid-conversation system message and no tool_choice: none of them may be
// refused or rewritten, or every brokered claude run on a 5.5 model would
// fail at its first request. The beta header loses nothing under the
// default deny-list either.
func TestInspectClaudeCodeRequest(t *testing.T) {
	body, err := os.ReadFile(claudeRequestFixture)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(claudeBetasFixture)
	if err != nil {
		t.Fatal(err)
	}
	betas := bytes.TrimSpace(raw)
	for _, want := range []string{`"thinking":`, `"output_config":`, `"context_management":`, `"role": "system"`} {
		if !bytes.Contains(body, []byte(want)) {
			t.Fatalf("fixture lost %s: re-record it", want)
		}
	}
	insp, err := inspectBody(body, DefaultBetaDenylist)
	if err != nil {
		t.Fatalf("a recorded claude request is refused: %v", err)
	}
	if insp.model != "claude-sonnet-5-5" || insp.maxTokens != 128000 || insp.rewritten != nil {
		t.Fatalf("inspection = model %q, max_tokens %d, rewritten %v", insp.model, insp.maxTokens, insp.rewritten != nil)
	}
	h := http.Header{}
	h.Set(betaHeader, string(betas))
	filterBetas(h, DefaultBetaDenylist)
	if got, want := strings.Split(h.Get(betaHeader), ","), strings.Split(string(betas), ","); !slices.Equal(got, want) {
		t.Errorf("betas after the deny-list = %q, want all of %q", got, want)
	}
}
