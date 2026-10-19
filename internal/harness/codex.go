// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package harness

import (
	"bytes"
	"encoding/json"
	"maps"
	"slices"
	"strings"

	"github.com/bitwise-media-group/patchy/internal/runner"
	"github.com/bitwise-media-group/patchy/internal/transcript"
)

// Codex drives the `codex` CLI (OpenAI Codex), the alternative harness for
// running the agent stages on OpenAI models.
type Codex struct {
	base
}

// NewCodex returns the builtin Codex harness.
func NewCodex() *Codex {
	return &Codex{base: base{
		id:   "codex",
		name: "OpenAI Codex",
		clis: []string{"codex"},
		// The credentials the codex CLI authenticates with in headless mode,
		// in the CLI's own resolution order (it reads stored auth.json first,
		// which an agent pod never has). OPENAI_API_KEY is a platform API key
		// billed at usage rates; the CODEX_* pair carries a workspace token
		// billed to a ChatGPT plan, which the vendor documents as the channel
		// for schedulers and private CI runners.
		envKeys: []string{"OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN"},
	}}
}

// PromptSpec builds the headless codex invocation for one prompted run.
// `codex exec --json` emits one JSON event per line, which is what
// ParseResult and ScanUsage parse. Codex's own Landlock/Seatbelt sandbox is
// always disabled: patchy confines the agent at the pod layer (no network
// egress beyond the egress broker, no credentials, locked-down
// securityContext), where codex's kernel sandbox is redundant and
// unavailable anyway.
//
// codex exec has no equivalents for MaxTurns or a pre-assigned SessionID, so
// those request fields do not map; budget and timeout enforcement stay with
// the runner, and the session id is read back from the stream's thread.started
// event instead. The neutral Sandbox posture is intentionally not rendered:
// codex's read-only/workspace-write modes are enforced by bubblewrap, which
// this image does not ship, and enabling it would mean relaxing the pod's
// RuntimeDefault seccomp profile — it gates the user-namespace and mount
// syscalls bwrap needs (verified on-cluster: userns clone is EPERM under
// RuntimeDefault, works under Unconfined). That is not a trade worth making to
// prevent writes in a pod that already has no egress, no credentials, a
// read-only rootfs, and a workspace discarded after the run — so --sandbox
// stays danger-full-access for every posture. AddDirs is likewise unmapped
// (codex 0.145 gained --add-dir; wiring it is a follow-up). SystemPromptAppend
// has no system-prompt channel either and is folded into the prompt so its
// instructions still reach the agent.
//
// Every run switches off what would dial anything but the model provider
// (codexQuietConfig). With a Gateway the run's provider is the egress broker
// (codexGatewayConfig); patchy always sets one, since codex runs brokered
// only.
func (c *Codex) PromptSpec(ws string, req PromptRequest) runner.CommandSpec {
	prompt := req.Prompt
	if req.SystemPromptAppend != "" {
		prompt = req.SystemPromptAppend + "\n\n" + prompt
	}
	argv := []string{
		"codex", "exec", prompt,
		"--json",
		"--skip-git-repo-check",
		"--sandbox", "danger-full-access",
		"--model", req.Model,
	}
	for _, kv := range codexQuietConfig {
		argv = append(argv, "-c", kv)
	}
	if req.Gateway != nil {
		for _, kv := range codexGatewayConfig(*req.Gateway) {
			argv = append(argv, "-c", kv)
		}
	}
	return runner.CommandSpec{Argv: argv, Dir: ws, Env: req.Env}
}

// codexQuietConfig are the -c overrides every codex run carries: no
// analytics and no OpenTelemetry metrics (each would dial an OpenAI host
// the pod cannot reach, and their failures only add noise), no plugins (a
// plugin brings its own tools and endpoints) and no web search (a hosted
// tool the broker refuses anyway, so offering it would fail the request).
var codexQuietConfig = []string{
	`analytics.enabled=false`,
	`otel.metrics_exporter="none"`,
	`features.plugins=false`,
	`web_search="disabled"`,
}

// CodexProviderID is the id of the model provider codexGatewayConfig
// defines, and codexProviderName its display name. The name must not be
// "OpenAI": codex compares provider names to decide it is talking to
// OpenAI itself, and then compresses request bodies (zstd), which the
// broker's body inspection cannot read.
const (
	CodexProviderID   = "patchy"
	codexProviderName = "patchy egress broker"
)

// codexRetries is how often codex retries a request or a dropped stream on
// the broker's provider: once. The broker's limits, not codex's retry
// loop, decide when a run has spent enough, and a refusal the broker
// answers is not worth repeating.
const codexRetries = "1"

// codexGatewayConfig are the -c overrides that make the egress broker the
// run's model provider: the Responses API at the broker's openai route,
// authenticated by the placeholder key (which the broker strips) and the
// headers carrying the caller token, both read by codex from the run's
// environment by name, so no token is ever on the command line. Websockets
// are off: the broker refuses a protocol upgrade. A repository's own
// .codex/config.toml cannot override any of this: model_provider and
// model_providers are not settable from a project config.
func codexGatewayConfig(g Gateway) []string {
	p := "model_providers." + CodexProviderID + "."
	out := []string{
		`model_provider=` + tomlString(CodexProviderID),
		p + `name=` + tomlString(codexProviderName),
		p + `base_url=` + tomlString(g.BaseURL),
		p + `wire_api="responses"`,
		p + `env_key=` + tomlString(g.KeyEnv),
	}
	if len(g.HeaderEnv) > 0 {
		pairs := make([]string, 0, len(g.HeaderEnv))
		for _, h := range slices.Sorted(maps.Keys(g.HeaderEnv)) {
			pairs = append(pairs, tomlString(h)+"="+tomlString(g.HeaderEnv[h]))
		}
		out = append(out, p+`env_http_headers={`+strings.Join(pairs, ",")+`}`)
	}
	return append(out,
		p+`supports_websockets=false`,
		p+`request_max_retries=`+codexRetries,
		p+`stream_max_retries=`+codexRetries,
	)
}

// tomlString renders s as a TOML basic string, the form codex parses a -c
// value in. JSON's string escapes (\", \\, \n, \uXXXX, ...) are all valid
// TOML basic-string escapes, so a JSON encoding is a TOML one.
func tomlString(s string) string {
	b, _ := json.Marshal(s) // a string always encodes
	return string(b)
}

// codexEvent is one line of `codex exec --json` output. Each event type
// populates only its own fields: thread.started carries the thread id,
// item.completed the agent messages, turn.completed the per-turn usage,
// turn.failed an error object, and type:"error" a bare message.
type codexEvent struct {
	Type     string    `json:"type"`
	ThreadID string    `json:"thread_id"`
	Item     codexItem `json:"item"`
	Usage    *struct {
		InputTokens           *int `json:"input_tokens"`
		CachedInputTokens     *int `json:"cached_input_tokens"`
		CacheWriteInputTokens *int `json:"cache_write_input_tokens"`
		OutputTokens          *int `json:"output_tokens"`
		ReasoningOutputTokens *int `json:"reasoning_output_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
	Message string `json:"message"`
}

// codexItem is one item of a codex turn. Codex flattens what claude nests
// under an assistant message into typed items, so the fields below are the
// union across item types and only those of Type are populated.
//
// Beyond type and text these names are best-effort: codex's item schema is not
// versioned in a way this repo can pin, so ScanTurns degrades to naming the
// item type rather than guessing wrong when a field is absent.
type codexItem struct {
	Type             string `json:"type"`
	Text             string `json:"text"`
	Command          string `json:"command"`
	AggregatedOutput string `json:"aggregated_output"`
	ExitCode         *int   `json:"exit_code"`
	Server           string `json:"server"`
	Tool             string `json:"tool"`
	Query            string `json:"query"`
}

// codexToolItems maps a codex item type to the tool name a transcript shows.
var codexToolItems = map[string]string{
	"command_execution": "Bash",
	"file_change":       "Edit",
	"mcp_tool_call":     "MCP",
	"web_search":        "WebSearch",
}

// ScanTurns projects one `codex exec --json` line onto the transcript
// vocabulary. Codex reports items only on completion, so a tool call and its
// output arrive together and are split back into the call and result turns the
// rest of the pipeline expects.
func (c *Codex) ScanTurns(line []byte) []transcript.Turn {
	var ev codexEvent
	if json.Unmarshal(line, &ev) != nil {
		return nil
	}
	switch ev.Type {
	case "thread.started":
		if ev.ThreadID == "" {
			return nil
		}
		return []transcript.Turn{{
			Role: transcript.RoleSystem, Kind: transcript.KindNotice,
			Text: "thread " + ev.ThreadID + " started",
		}}
	case "item.completed":
		return codexItemTurns(ev.Item)
	case "turn.failed":
		if ev.Error == nil || strings.TrimSpace(ev.Error.Message) == "" {
			return nil
		}
		return []transcript.Turn{{
			Role: transcript.RoleSystem, Kind: transcript.KindNotice,
			Text: "turn failed: " + strings.TrimSpace(ev.Error.Message),
		}}
	}
	return nil
}

// codexItemTurns maps one completed item.
func codexItemTurns(item codexItem) []transcript.Turn {
	switch item.Type {
	case "agent_message":
		if strings.TrimSpace(item.Text) == "" {
			return nil
		}
		return []transcript.Turn{{
			Role: transcript.RoleAssistant, Kind: transcript.KindText, Text: item.Text,
		}}
	case "reasoning":
		if strings.TrimSpace(item.Text) == "" {
			return nil
		}
		return []transcript.Turn{{
			Role: transcript.RoleAssistant, Kind: transcript.KindThinking, Text: item.Text,
		}}
	case "todo_list":
		if strings.TrimSpace(item.Text) == "" {
			return nil
		}
		return []transcript.Turn{{
			Role: transcript.RoleAssistant, Kind: transcript.KindNotice, Text: item.Text,
		}}
	}

	tool, ok := codexToolItems[item.Type]
	if !ok {
		return nil
	}
	if item.Type == "mcp_tool_call" && item.Tool != "" {
		tool = strings.TrimSpace(item.Server + " " + item.Tool)
	}
	turns := []transcript.Turn{{
		Role: transcript.RoleAssistant, Kind: transcript.KindToolUse,
		Tool: tool, Text: codexItemInput(item),
	}}
	if out := strings.TrimSpace(item.AggregatedOutput); out != "" {
		if item.ExitCode != nil && *item.ExitCode != 0 {
			out = "[error] " + out
		}
		turns = append(turns, transcript.Turn{
			Role: transcript.RoleUser, Kind: transcript.KindToolResult, Text: out,
		})
	}
	return turns
}

// codexItemInput picks the argument identifying what a tool item did, falling
// back to the item type so an unmapped shape still reads as an action.
func codexItemInput(item codexItem) string {
	for _, s := range []string{item.Command, item.Query, item.Text} {
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return item.Type
}

// codexScan is the digest of one event stream; ParseResult and RuntimeError
// each project from it.
type codexScan struct {
	threadID string
	texts    []string
	turns    int // completed turns
	errors   []string
	terminal bool // saw turn.completed or turn.failed
	usage    *Usage
}

// scanCodexEvents walks the event stream once. Usage is summed across
// turn.completed events (codexTurnUsage); codex reports tokens but never
// cost, so CostUSD stays nil.
func scanCodexEvents(stdout []byte) codexScan {
	var s codexScan
	var sum codexUsage
	for line := range bytes.SplitSeq(stdout, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var ev codexEvent
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		switch ev.Type {
		case "thread.started":
			s.threadID = ev.ThreadID
		case "item.completed":
			if ev.Item.Type == "agent_message" {
				s.texts = append(s.texts, ev.Item.Text)
			}
		case "turn.completed":
			s.turns++
			s.terminal = true
			if u := ev.Usage; u != nil {
				sum.add(codexTurnUsage(deref(u.InputTokens), deref(u.CachedInputTokens),
					deref(u.CacheWriteInputTokens), deref(u.OutputTokens), deref(u.ReasoningOutputTokens)))
				sum.writes = sum.writes || u.CacheWriteInputTokens != nil
				s.usage = sum.usage()
			}
		case "turn.failed":
			s.terminal = true
			if ev.Error != nil && strings.TrimSpace(ev.Error.Message) != "" {
				s.errors = append(s.errors, strings.TrimSpace(ev.Error.Message))
			}
		case "error":
			if strings.TrimSpace(ev.Message) != "" {
				s.errors = append(s.errors, strings.TrimSpace(ev.Message))
			}
		}
	}
	return s
}

// codexUsage is the running sum of a stream's turns. writes records
// whether any turn reported cache writes at all, so a stream from a CLI
// that never does keeps CacheCreationTokens unreported (nil) rather than
// claiming zero.
type codexUsage struct {
	fresh, cacheRead, cacheWrite, output int
	writes                               bool
}

func (u *codexUsage) add(t codexUsage) {
	u.fresh += t.fresh
	u.cacheRead += t.cacheRead
	u.cacheWrite += t.cacheWrite
	u.output += t.output
}

func (u codexUsage) usage() *Usage {
	out := &Usage{InputTokens: new(u.fresh), CacheReadTokens: new(u.cacheRead), OutputTokens: new(u.output)}
	if u.writes {
		out.CacheCreationTokens = new(u.cacheWrite)
	}
	return out
}

// codexTurnUsage maps one turn.completed usage onto the Usage contract.
// Codex reports input_tokens as the whole prompt, with cached_input_tokens
// and cache_write_input_tokens subsets of it; the contract wants fresh
// (uncached, unwritten) input on InputTokens and the cache reads and writes
// apart, so both subsets are split off (each clamped to what is left, so a
// count larger than the whole cannot drive another negative) and the three
// always sum to input_tokens. output_tokens already includes the reasoning
// tokens (reasoning_output_tokens is its subset, as in the Responses API's
// output_tokens_details, and as the recorded stream shows); the reasoning
// figure is read only to hold that: an output figure below it is raised to
// it, so a stream that ever reported them apart never under-reports. These
// are the egress broker's figures for the same requests: it folds OpenAI's
// cache writes into fresh input, which prices the same, since cache writes
// bill at the input rate (model.UsageCostUSD).
func codexTurnUsage(input, cached, cacheWrite, output, reasoning int) codexUsage {
	input = max(input, 0)
	read := min(max(cached, 0), input)
	write := min(max(cacheWrite, 0), input-read)
	return codexUsage{
		fresh: input - read - write, cacheRead: read, cacheWrite: write,
		output: max(output, reasoning, 0),
	}
}

func deref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// ParseResult digests the codex event stream: the final answer is the
// concatenated agent messages, the session id is the thread id, and the turn
// count and usage accumulate over completed turns. Output with no terminal
// turn event (plain text, crash mid-stream) returns the raw stdout as
// FinalText with ok=false, matching the interface contract.
func (c *Codex) ParseResult(stdout []byte) (AgentResult, bool) {
	s := scanCodexEvents(stdout)
	if !s.terminal {
		return AgentResult{FinalText: string(stdout)}, false
	}
	return AgentResult{
		FinalText: strings.Join(s.texts, "\n"),
		SessionID: s.threadID,
		NumTurns:  s.turns,
		Usage:     s.usage,
		IsError:   len(s.errors) > 0,
		Errors:    s.errors,
	}, true
}

// RuntimeError detects a codex run that produced no agent output (auth
// blocked, crash, failed turn) so it is reported distinctly from a run whose
// report merely needs judging. A run that emitted any agent message is
// usable regardless of exit code — a partial answer, not an error.
func (c *Codex) RuntimeError(stdout []byte, exitCode int, timedOut bool) string {
	if len(bytes.TrimSpace(stdout)) == 0 {
		return "empty CLI output"
	}
	s := scanCodexEvents(stdout)
	if len(s.texts) > 0 {
		return "" // produced agent output — usable
	}
	if len(s.errors) > 0 {
		return "codex run error: " + strings.Join(s.errors, "; ")
	}
	switch {
	case timedOut:
		return "timed out with no agent output"
	case exitCode != 0:
		return "codex produced no agent output"
	}
	return ""
}

// StreamUsage sums the turn.completed usage the stream carried. Codex reports
// usage only when a turn completes, so a run cut off inside its one turn
// reports nil: it spent tokens, but its stream never said how many.
func (c *Codex) StreamUsage(stdout []byte) *Usage {
	return scanCodexEvents(stdout).usage
}

// ScanUsage reads the output-token count off one live stream line. Codex
// reports usage only on turn.completed events, so the budget accumulator
// sums per-turn totals; within a turn the budget cannot fire early, but a
// multi-turn session over budget is still killed between turns.
func (c *Codex) ScanUsage(line []byte) (int, bool) {
	var ev codexEvent
	if json.Unmarshal(line, &ev) != nil {
		return 0, false
	}
	if ev.Type != "turn.completed" || ev.Usage == nil || ev.Usage.OutputTokens == nil {
		return 0, false
	}
	return max(*ev.Usage.OutputTokens, deref(ev.Usage.ReasoningOutputTokens)), true
}
