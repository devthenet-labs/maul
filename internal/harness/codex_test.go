// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package harness

import (
	"math/rand"
	"os"
	"slices"
	"strings"
	"testing"
	"testing/quick"

	"github.com/devthenet-labs/maul/internal/transcript"
)

// codex exec --json emits one JSON event per line: thread.started carries the
// thread id, item.* events the agent's messages and tool items (reported at
// item.started then item.completed), and turn.completed the per-turn usage.
// This fixture mirrors a real capture, including a non-JSON ERROR log line
// interleaved with the stream.
const (
	codexThreadID = "0198c5c4-e1b2-7d3e-9c7a-1d2e3f4a5b6c"

	codexStreamSuccess = `{"type":"thread.started","thread_id":"` + codexThreadID + `"}` + "\n" +
		`{"type":"turn.started"}` + "\n" +
		`2026-07-23T16:20:48.507788Z ERROR codex_core::phase2: transient log line` + "\n" +
		`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"I'll fix the finding."}}` + "\n" +
		`{"type":"item.started","item":{"id":"item_1","type":"command_execution",` +
		`"command":"ls -la","status":"in_progress"}}` + "\n" +
		`{"type":"item.completed","item":{"id":"item_1","type":"command_execution",` +
		`"command":"ls -la","aggregated_output":"total 0\n","exit_code":0,"status":"completed"}}` + "\n" +
		`{"type":"item.completed","item":{"id":"item_2","type":"agent_message","text":"Report written."}}` + "\n" +
		`{"type":"turn.completed","usage":{"input_tokens":61395,"cached_input_tokens":51328,"output_tokens":184}}`

	// A failed turn with no agent output: not usable.
	codexStreamTurnFailed = `{"type":"thread.started","thread_id":"` + codexThreadID + `"}` + "\n" +
		`{"type":"turn.started"}` + "\n" +
		`{"type":"error","message":"stream disconnected"}` + "\n" +
		`{"type":"turn.failed","error":{"message":"429 rate limited"}}`

	// A crash before any terminal turn event: the stream just stops.
	codexStreamTruncated = `{"type":"thread.started","thread_id":"` + codexThreadID + `"}` + "\n" +
		`{"type":"turn.started"}`
)

func TestCodexPromptSpec(t *testing.T) {
	c := NewCodex()
	spec := c.PromptSpec("/work/ws", PromptRequest{
		Prompt: "fix the finding",
		Model:  "gpt-5.2-codex",
		// No codex mapping exists for these; they must not leak into argv.
		SessionID: codexThreadID,
		MaxTurns:  30,
		Sandbox:   SandboxReadOnly,
		AddDirs:   []string{"/scratch"},
		Env:       []string{"OPENAI_API_KEY=k"},
	})
	want := []string{
		"codex", "exec", "fix the finding",
		"--json",
		"--skip-git-repo-check",
		"--sandbox", "danger-full-access",
		"--model", "gpt-5.2-codex",
		"-c", "analytics.enabled=false",
		"-c", `otel.metrics_exporter="none"`,
		"-c", "features.plugins=false",
		"-c", `web_search="disabled"`,
	}
	if !slices.Equal(spec.Argv, want) {
		t.Errorf("Argv =\n%q\nwant\n%q", spec.Argv, want)
	}
	if spec.Dir != "/work/ws" {
		t.Errorf("Dir = %q, want the workspace", spec.Dir)
	}
	if !slices.Equal(spec.Env, []string{"OPENAI_API_KEY=k"}) {
		t.Errorf("Env = %q, want the request env", spec.Env)
	}
}

func TestCodexPromptSpecSystemPromptAppend(t *testing.T) {
	c := NewCodex()
	spec := c.PromptSpec("/ws", PromptRequest{
		Prompt:             "fix it",
		Model:              "m",
		SystemPromptAppend: "never push",
	})
	// codex exec has no system-prompt channel; the append folds into the prompt.
	if got := spec.Argv[2]; got != "never push\n\nfix it" {
		t.Errorf("prompt = %q, want the system append folded in", got)
	}
}

func TestCodexParseResultSuccess(t *testing.T) {
	c := NewCodex()
	res, ok := c.ParseResult([]byte(codexStreamSuccess))
	if !ok {
		t.Fatal("ok = false, want a parsed terminal turn")
	}
	if want := "I'll fix the finding.\nReport written."; res.FinalText != want {
		t.Errorf("FinalText = %q, want %q", res.FinalText, want)
	}
	if res.SessionID != codexThreadID {
		t.Errorf("SessionID = %q, want the thread id %q", res.SessionID, codexThreadID)
	}
	if res.NumTurns != 1 {
		t.Errorf("NumTurns = %d, want 1", res.NumTurns)
	}
	if res.IsError || len(res.Errors) != 0 {
		t.Errorf("envelope = (%v, %q), want a clean success", res.IsError, res.Errors)
	}
	if res.Usage == nil {
		t.Fatal("Usage = nil, want populated")
	}
	// Codex reports the whole prompt as input with a cached subset; the Usage
	// contract wants fresh input split from cache reads: 61395-51328 = 10067.
	if got := derefInt(res.Usage.InputTokens); got != 10067 {
		t.Errorf("InputTokens = %d, want 10067", got)
	}
	if got := derefInt(res.Usage.CacheReadTokens); got != 51328 {
		t.Errorf("CacheReadTokens = %d, want 51328", got)
	}
	if got := derefInt(res.Usage.OutputTokens); got != 184 {
		t.Errorf("OutputTokens = %d, want 184", got)
	}
	// Codex reports tokens but never cost.
	if res.Usage.CostUSD != nil {
		t.Errorf("CostUSD = %v, want nil", *res.Usage.CostUSD)
	}
}

func TestCodexParseResultFallbacks(t *testing.T) {
	c := NewCodex()

	// No terminal turn event: raw stdout comes back as FinalText with ok=false.
	raw := "plain text answer\n"
	if res, ok := c.ParseResult([]byte(raw)); ok || res.FinalText != raw || res.Usage != nil {
		t.Errorf("ParseResult(plain) = (%+v, %v), want raw fallback with ok=false", res, ok)
	}
	if res, ok := c.ParseResult([]byte(codexStreamTruncated)); ok || res.FinalText != codexStreamTruncated {
		t.Errorf("ParseResult(truncated) = (%+v, %v), want raw fallback with ok=false", res, ok)
	}

	// A failed turn is terminal: the error envelope parses.
	res, ok := c.ParseResult([]byte(codexStreamTurnFailed))
	if !ok || !res.IsError {
		t.Fatalf("ParseResult(turn.failed) = (%+v, %v), want the error envelope parsed", res, ok)
	}
	if want := []string{"stream disconnected", "429 rate limited"}; !slices.Equal(res.Errors, want) {
		t.Errorf("Errors = %q, want %q", res.Errors, want)
	}
}

func TestCodexRuntimeError(t *testing.T) {
	c := NewCodex()
	tests := []struct {
		name     string
		stdout   string
		exitCode int
		timedOut bool
		want     string
	}{
		{"usable agent output", codexStreamSuccess, 0, false, ""},
		{"nonzero exit with agent output is usable", codexStreamSuccess, 1, false, ""},
		{"empty", "", 1, false, "empty CLI output"},
		{"plain text clean exit", "hello\n", 0, false, ""},
		{"failed turn", codexStreamTurnFailed, 1, false,
			"codex run error: stream disconnected; 429 rate limited"},
		{"truncated stream crash", codexStreamTruncated, 1, false, "codex produced no agent output"},
		{"timeout with no agent output", codexStreamTruncated, -1, true, "timed out with no agent output"},
	}
	for _, tt := range tests {
		if got := c.RuntimeError([]byte(tt.stdout), tt.exitCode, tt.timedOut); got != tt.want {
			t.Errorf("%s: RuntimeError = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestCodexScanUsage(t *testing.T) {
	c := NewCodex()
	tests := []struct {
		name string
		line string
		want int
		ok   bool
	}{
		{"turn.completed with usage",
			`{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":42}}`, 42, true},
		{"turn.completed without usage", `{"type":"turn.completed"}`, 0, false},
		{"agent message", `{"type":"item.completed","item":{"type":"agent_message","text":"hi"}}`, 0, false},
		{"garbage", "not json", 0, false},
	}
	for _, tt := range tests {
		got, ok := c.ScanUsage([]byte(tt.line))
		if got != tt.want || ok != tt.ok {
			t.Errorf("%s: ScanUsage = (%d, %v), want (%d, %v)", tt.name, got, ok, tt.want, tt.ok)
		}
	}
}

// TestCodexEnvKeys guards the accepted credential channels. runnercfg
// validates --codex-secret-env against this list at controller startup, so a
// name missing here is not a cosmetic omission: it makes that credential
// unconfigurable, and jobs.reservedEnv must reserve each one so it can only
// reach the pod by secretKeyRef.
func TestCodexEnvKeys(t *testing.T) {
	got := NewCodex().EnvKeys()
	want := []string{"OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN"}
	if !slices.Equal(got, want) {
		t.Errorf("EnvKeys() = %v, want %v", got, want)
	}
}

// TestCodexPromptSpecGateway pins the brokered provider config, the argv
// a real codex (rust-v0.153.4) was run with against a recording server to
// capture testdata/codex-0.153.4-exec.jsonl: the broker's route as the
// Responses provider, the placeholder key and the caller-token header named
// by variable only, no websockets, one retry. No token value is in argv.
func TestCodexPromptSpecGateway(t *testing.T) {
	spec := NewCodex().PromptSpec("/ws", PromptRequest{
		Prompt: "p", Model: "gpt-5.3-codex",
		Gateway: &Gateway{
			BaseURL:   "http://broker.maul.svc:8080/openai/v1",
			KeyEnv:    "MAUL_CODEX_PLACEHOLDER_KEY",
			HeaderEnv: map[string]string{"X-Maul-Broker-Token": "MAUL_BROKER_TOKEN"},
		},
		Env: []string{"MAUL_BROKER_TOKEN=secret-token"},
	})
	want := []string{
		"codex", "exec", "p", "--json", "--skip-git-repo-check", "--sandbox", "danger-full-access",
		"--model", "gpt-5.3-codex",
		"-c", "analytics.enabled=false",
		"-c", `otel.metrics_exporter="none"`,
		"-c", "features.plugins=false",
		"-c", `web_search="disabled"`,
		"-c", `model_provider="maul"`,
		"-c", `model_providers.maul.name="maul egress broker"`,
		"-c", `model_providers.maul.base_url="http://broker.maul.svc:8080/openai/v1"`,
		"-c", `model_providers.maul.wire_api="responses"`,
		"-c", `model_providers.maul.env_key="MAUL_CODEX_PLACEHOLDER_KEY"`,
		"-c", `model_providers.maul.env_http_headers={"X-Maul-Broker-Token"="MAUL_BROKER_TOKEN"}`,
		"-c", "model_providers.maul.supports_websockets=false",
		"-c", "model_providers.maul.request_max_retries=1",
		"-c", "model_providers.maul.stream_max_retries=1",
	}
	if !slices.Equal(spec.Argv, want) {
		t.Errorf("Argv =\n%q\nwant\n%q", spec.Argv, want)
	}
	for _, a := range spec.Argv {
		if strings.Contains(a, "secret-token") {
			t.Errorf("argv carries the token: %q", a)
		}
	}
	if !slices.Equal(spec.Env, []string{"MAUL_BROKER_TOKEN=secret-token"}) {
		t.Errorf("Env = %q, want the request env", spec.Env)
	}
}

// TestCodexPromptSpecEvaluate: the evaluate posture is the build posture's
// argv exactly, the argv TestCodexPromptSpecGateway pins from the recorded
// rust-v0.153.4 run: codex renders no posture of its own (the pod is the
// boundary, network included), and there is no tree, only the scratch
// directory the run is started in. AddDirs and WriteDirs stay unrendered.
func TestCodexPromptSpecEvaluate(t *testing.T) {
	gw := &Gateway{
		BaseURL:   "http://broker.maul.svc:8080/openai/v1",
		KeyEnv:    "MAUL_CODEX_PLACEHOLDER_KEY",
		HeaderEnv: map[string]string{"X-Maul-Broker-Token": "MAUL_BROKER_TOKEN"},
	}
	req := PromptRequest{Prompt: "p", Model: "gpt-5.3-codex", Gateway: gw, Env: []string{"E=1"}}
	build := NewCodex().PromptSpec("/ws", withSandbox(req, SandboxWorkspaceWrite))
	eval := withSandbox(req, SandboxEvaluate)
	eval.AddDirs, eval.WriteDirs = []string{"/work"}, []string{"/work"}
	spec := NewCodex().PromptSpec("/work/scratch", eval)
	if !slices.Equal(spec.Argv, build.Argv) {
		t.Errorf("evaluate Argv =\n%q\nwant the build posture's\n%q", spec.Argv, build.Argv)
	}
	if spec.Dir != "/work/scratch" {
		t.Errorf("Dir = %q, want the scratch directory", spec.Dir)
	}
	if !slices.Equal(spec.Env, []string{"E=1"}) {
		t.Errorf("Env = %q, want the request env", spec.Env)
	}
}

func withSandbox(req PromptRequest, s Sandbox) PromptRequest {
	req.Sandbox = s
	return req
}

// TestCodexProviderNameIsNotOpenAI: codex treats a provider named OpenAI as
// OpenAI itself and compresses its request bodies, which the broker's
// inspection could not read.
func TestCodexProviderNameIsNotOpenAI(t *testing.T) {
	if strings.EqualFold(codexProviderName, "openai") || CodexProviderID == "openai" {
		t.Fatalf("provider %q named %q", CodexProviderID, codexProviderName)
	}
}

// TestCodexGatewayConfigQuotesValues: a value is a TOML basic string, so a
// quote or backslash in it cannot end the string and set another key.
func TestCodexGatewayConfigQuotesValues(t *testing.T) {
	got := codexGatewayConfig(Gateway{BaseURL: `http://x/"y\z`, KeyEnv: "K"})
	if want := `model_providers.maul.base_url="http://x/\"y\\z"`; got[2] != want {
		t.Errorf("base_url = %s, want %s", got[2], want)
	}
	for _, kv := range got {
		if strings.Contains(kv, "env_http_headers") {
			t.Errorf("no headers asked for, got %s", kv)
		}
	}
}

// TestCodexRecordedStream parses a stream the real codex CLI wrote
// (rust-v0.153.4, the pinned release, run with the brokered provider
// config of TestCodexPromptSpecGateway against a recording Responses server
// that answered one exec_command call, then a message, each with usage
// input 1200, cached 1024, output 40, reasoning 12). Codex sums the two
// requests into one turn and reports cache writes and reasoning beside the
// rest.
func TestCodexRecordedStream(t *testing.T) {
	stdout, err := os.ReadFile("testdata/codex-0.153.4-exec.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	c := NewCodex()
	res, ok := c.ParseResult(stdout)
	if !ok || res.IsError || res.FinalText != "Done: the report is written." || res.NumTurns != 1 ||
		res.SessionID != "01a152a3-d587-7513-a33c-85ebd73b2853" {
		t.Fatalf("ParseResult = %+v, %v", res, ok)
	}
	if got, want := usageOf(res.Usage), [4]int{2400 - 2048, 2048, 0, 80}; got != want {
		t.Errorf("usage = %v, want %v", got, want)
	}
	if got := c.RuntimeError(stdout, 0, false); got != "" {
		t.Errorf("RuntimeError = %q", got)
	}
	var turns []transcript.Turn
	var budget int
	for line := range strings.SplitSeq(string(stdout), "\n") {
		turns = append(turns, c.ScanTurns([]byte(line))...)
		if n, ok := c.ScanUsage([]byte(line)); ok {
			budget += n
		}
	}
	if budget != 80 {
		t.Errorf("ScanUsage summed %d, want 80", budget)
	}
	kinds := make([]string, 0, len(turns))
	for _, tr := range turns {
		kinds = append(kinds, string(tr.Kind)+":"+tr.Tool)
	}
	want := []string{
		string(transcript.KindNotice) + ":",
		string(transcript.KindToolUse) + ":Bash",
		string(transcript.KindToolResult) + ":",
		string(transcript.KindText) + ":",
	}
	if !slices.Equal(kinds, want) {
		t.Errorf("turns = %q, want %q", kinds, want)
	}
}

// TestCodexRecordedRefusal: a 429 the broker answers (here a spend-limit
// refusal, insufficient_quota) ends the real CLI's turn after one request
// with no agent output, so the run is a runtime error. Codex does not quote
// the refusal's message, only its status.
func TestCodexRecordedRefusal(t *testing.T) {
	stdout, err := os.ReadFile("testdata/codex-0.153.4-exec-429.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	got := NewCodex().RuntimeError(stdout, 1, false)
	if !strings.HasPrefix(got, "codex run error: ") || !strings.Contains(got, "429") {
		t.Errorf("RuntimeError = %q", got)
	}
}

// TestCodexTurnUsageProperty (seeded): whatever a turn reports, fresh input,
// cache reads and cache writes are never negative and sum to the turn's
// input, and output is never below the reasoning figure.
func TestCodexTurnUsageProperty(t *testing.T) {
	prop := func(in, cached, write, out, reasoning int32) bool {
		u := codexTurnUsage(int(in), int(cached), int(write), int(out), int(reasoning))
		return u.fresh >= 0 && u.cacheRead >= 0 && u.cacheWrite >= 0 &&
			u.fresh+u.cacheRead+u.cacheWrite == max(int(in), 0) &&
			u.output >= max(int(reasoning), 0) && u.output >= max(int(out), 0)
	}
	cfg := &quick.Config{MaxCount: 500, Rand: rand.New(rand.NewSource(20261018))}
	if err := quick.Check(prop, cfg); err != nil {
		t.Fatal(err)
	}
}

func TestCodexTurnUsage(t *testing.T) {
	tests := []struct {
		name                          string
		in, cached, write, out, think int
		want                          codexUsage
	}{
		{"split", 1000, 600, 100, 50, 10, codexUsage{fresh: 300, cacheRead: 600, cacheWrite: 100, output: 50}},
		{"cached over input", 100, 150, 0, 1, 0, codexUsage{cacheRead: 100, output: 1}},
		{"writes over the rest", 100, 60, 90, 1, 0, codexUsage{cacheRead: 60, cacheWrite: 40, output: 1}},
		{"negatives", -5, -1, -1, -3, -2, codexUsage{}},
		{"reasoning apart", 10, 0, 0, 4, 9, codexUsage{fresh: 10, output: 9}},
	}
	for _, tt := range tests {
		if got := codexTurnUsage(tt.in, tt.cached, tt.write, tt.out, tt.think); got != tt.want {
			t.Errorf("%s: %+v, want %+v", tt.name, got, tt.want)
		}
	}
}
