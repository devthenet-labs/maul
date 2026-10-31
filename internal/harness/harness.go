// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package harness

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/devthenet-labs/maul/internal/runner"
	"github.com/devthenet-labs/maul/internal/transcript"
)

// Usage is the harness-reported consumption of one agent session. Fields are
// nil where the CLI does not report them. InputTokens is the fresh (uncached)
// input only; cache reads and writes are reported separately so a multi-turn
// session's cheap cache traffic does not inflate the headline input figure.
type Usage struct {
	InputTokens         *int // fresh (uncached) input only
	CacheReadTokens     *int
	CacheCreationTokens *int
	OutputTokens        *int
	CostUSD             *float64
}

// Sandbox is the filesystem/execution posture a run requires, declared
// harness-neutrally so agentrun states intent once and each harness renders it
// in its own vocabulary: claude as an allow/deny tool grammar, codex (once the
// pod's kernel sandbox is confirmed) as a --sandbox mode backed by mounted
// execpolicy rules. A harness that has no way to express a read-only or
// writable posture renders it as its default — the request is advisory, not
// a guarantee every CLI can keep. SandboxEvaluate is the exception: a harness
// that cannot keep it refuses it (CheckPosture).
type Sandbox int

const (
	// SandboxDefault imposes no tool policy: the CLI's own defaults apply. It
	// is the zero value, so a bare PromptRequest stays unrestricted.
	SandboxDefault Sandbox = iota
	// SandboxReadOnly is the investigation posture: read the tree and write
	// only the report — no source edits, no shell, no network tools.
	SandboxReadOnly
	// SandboxWorkspaceWrite is the remediation posture: edit the workspace
	// freely; network tools stay denied.
	SandboxWorkspaceWrite
	// SandboxEvaluate is the agent evaluator's posture: no tree, only the
	// scratch directory the run is started in (its workspace), which it may
	// read and write, and a shell that runs maul-tool alone. A harness that
	// cannot hold a run to it refuses it (CheckPosture) rather than render
	// its default.
	SandboxEvaluate
)

// ErrPostureUnsupported is CheckPosture's refusal: the harness cannot render
// the posture, and a run under it must not start.
var ErrPostureUnsupported = errors.New("harness: sandbox posture unsupported")

// PostureChecker is the optional capability of refusing a posture the
// harness renders only as something weaker. A harness without it renders
// every known posture.
type PostureChecker interface {
	// CheckPosture returns an error wrapping ErrPostureUnsupported when the
	// harness cannot render s.
	CheckPosture(s Sandbox) error
}

// CheckPosture reports whether h can run a request under s: nil, or an error
// wrapping ErrPostureUnsupported. A posture no harness knows is refused by
// all of them. A caller asks before rendering a posture its harness may not
// hold (agentrun, for SandboxEvaluate); PromptSpec itself cannot refuse.
func CheckPosture(h Harness, s Sandbox) error {
	if s < SandboxDefault || s > SandboxEvaluate {
		return fmt.Errorf("%w: %s has no posture %d", ErrPostureUnsupported, h.ID(), s)
	}
	if pc, ok := h.(PostureChecker); ok {
		return pc.CheckPosture(s)
	}
	return nil
}

// PromptRequest describes one prompted agent run, harness-agnostically; the
// harness maps it onto its CLI's flags.
type PromptRequest struct {
	Prompt             string
	Model              string   // harness-specific model id
	SystemPromptAppend string   // appended to the CLI's system prompt when set
	SessionID          string   // pre-assigned UUID → claude --session-id
	MaxTurns           int      // agent-turn ceiling; 0 leaves the CLI default
	Sandbox            Sandbox  // filesystem/exec posture; each harness renders it natively
	AddDirs            []string // extra directories the agent may access
	Env                []string // extras appended to os.Environ() by the runner
	// WriteDirs scopes a SandboxReadOnly run's writes: the directories (absolute,
	// or relative to the run's workspace) it may create and change files in,
	// and nowhere else. Empty keeps the posture's unscoped write, so a request
	// that does not set it renders exactly as before; other postures ignore it.
	// claude renders it as path-scoped edit rules; a harness with no path
	// grammar ignores it, and the pod remains its boundary.
	WriteDirs []string
	// MaxOutputTokens bounds what one model request of the run may write
	// (thinking included): the max_tokens every request asks for, which the
	// egress broker reserves at the model's output price before it forwards
	// the request. Zero leaves the CLI's default (64000 on the pinned
	// claude for the 5-series models). claude renders it as
	// ClaudeMaxOutputTokensEnv; a harness with no such setting ignores it.
	MaxOutputTokens int
	// Gateway points a harness configured on its command line (codex) at the
	// egress broker rather than its vendor's API; nil leaves the CLI's own
	// provider. claude reads its gateway from the pod's environment and
	// ignores it.
	Gateway *Gateway
}

// Gateway is the egress broker as a harness CLI's model provider: the route
// it is based at, and the names of the environment variables (set in the
// run's Env, never on the command line) carrying the placeholder key the
// CLI's auth gate demands and the headers it must send. Values never appear
// here, only names, so argv carries no token.
type Gateway struct {
	// BaseURL is the broker route the CLI's provider is based at, such as
	// http://broker:8080/openai/v1.
	BaseURL string
	// KeyEnv names the variable holding the non-secret placeholder key.
	KeyEnv string
	// HeaderEnv maps a header name to the variable whose value the CLI
	// sends as it on every request (the broker's caller token).
	HeaderEnv map[string]string
}

// AgentResult is the parsed terminal result of one agent run.
type AgentResult struct {
	FinalText string
	SessionID string // the session id the CLI reports on its result event
	NumTurns  int
	Usage     *Usage // includes CostUSD from total_cost_usd; nil when unreported
	IsError   bool
	Subtype   string
	Errors    []string
}

// Harness is the required surface every agent CLI implements. A harness
// drives a model — it does not own one; the model a run targets is supplied
// as a harness-specific CLI id on the PromptRequest.
type Harness interface {
	ID() string        // registry key, e.g. "claude"
	Name() string      // human name, e.g. "Claude Code"
	CLI() []string     // runner binary candidates, in preference order
	EnvKeys() []string // credential env vars, in preference order
	// PromptSpec builds the headless command for one prompted run in workspace ws.
	PromptSpec(ws string, req PromptRequest) runner.CommandSpec
	// ParseResult extracts the terminal result from the CLI's full stdout.
	// ok is false when the output carried no terminal result event (plain
	// text, crash mid-stream); the AgentResult then carries the raw stdout as
	// FinalText and nothing else.
	ParseResult(stdout []byte) (res AgentResult, ok bool)
	// RuntimeError returns a short reason when the agent run did not complete
	// usably (auth blocked, crash, budget exhausted, error envelope), or ""
	// when it did. A run that reports an error is reported as one even if it
	// also produced text: a partial answer from a run that died is not a
	// result, and treating it as one hides the cause behind whatever the
	// stage checks next.
	RuntimeError(stdout []byte, exitCode int, timedOut bool) string
}

// BudgetReporter is the optional capability of distinguishing a run that
// exhausted its budget from one that malfunctioned. Both are runtime errors;
// only the first is the agent doing as it was told until the limit stopped
// it, and callers report them differently.
type BudgetReporter interface {
	// Exhausted reports whether stdout describes a run that hit its turn or
	// token limit rather than failing.
	Exhausted(stdout []byte) bool
}

// TerminalErrorReporter is the optional capability of reading the error a run
// ended on off the CLI's own terminal event. It reports only what the CLI
// itself said ended the run — never the model's text or a tool's output,
// which carry whatever the repository holds — so a caller matching a
// contract message against it (the egress broker's limit prefix) cannot
// have the outcome chosen by a quote of that message in the workspace, or
// by an error the run recovered from before dying of something else.
type TerminalErrorReporter interface {
	// TerminalError returns the error text the CLI's terminal event
	// reported, or "" when the run did not end in an error.
	TerminalError(stdout []byte) string
}

// UsageScanner is the optional capability of reading token usage off the live
// output stream; it powers the caller's output-token budget kill switch.
type UsageScanner interface {
	// ScanUsage returns the output-token count reported by one stream line
	// (claude: assistant events' message.usage.output_tokens), ok=false
	// otherwise.
	ScanUsage(line []byte) (outputTokens int, ok bool)
}

// StreamUsageReporter is the optional capability of reading what a run spent
// off its stream when it ended without the terminal event that totals it: a
// timeout, the budget kill switch, a crash, a cancelled pod. Every model call
// is billed whether or not the run finishes, so a stage records this rather
// than nothing; a cost ceiling summed from what stages record cannot see a
// run that records zero.
type StreamUsageReporter interface {
	// StreamUsage returns the usage the stream reported per model call,
	// summed, or nil when it reported none. CostUSD stays nil: no CLI
	// reports a cost before its terminal event, so the controller prices the
	// tokens at the model's rates (agentresult.stageCost).
	StreamUsage(stdout []byte) *Usage
}

// FirstEditReporter is the optional capability of reading off a run's stream
// the agent turn on which it first changed a file: how much of a run went on
// learning the code before changing it, which is what carrying context from
// earlier runs forward should shrink. A harness without it reports nothing,
// and the stage records 0 (unknown).
type FirstEditReporter interface {
	// FirstEditTurn returns the 1-based agent turn (one model API message)
	// on which the stream first shows a file-editing tool call, or 0 when it
	// shows none.
	FirstEditTurn(stdout []byte) int
}

// TurnScanner is the optional capability of projecting the live output stream
// onto the harness-neutral conversation vocabulary; it powers the transcript
// the status page replays. A harness without it simply produces no transcript.
//
// Each CLI's stream is shaped differently — claude nests content blocks under
// an assistant message, codex flattens them into typed items, copilot emits one
// event per message — so normalising here is the only place the difference can
// be absorbed once.
type TurnScanner interface {
	// ScanTurns returns the turns one stream line carries, in order, or nil
	// for a line that carries none. One line can yield several: a single
	// claude assistant event may hold text, thinking, and tool-use blocks.
	//
	// It reports raw text; bounding, redaction, and sequencing belong to the
	// caller's transcript.Recorder.
	ScanTurns(line []byte) []transcript.Turn
}

// TaskWatcher is the optional capability of following a command the agent
// runs while it runs: reading the stream's command events, and knowing where
// the CLI keeps a running command's output. It powers the live command output
// agentrun shows beside the transcript. A harness without it shows a command's
// output only when its tool result arrives.
type TaskWatcher interface {
	// ScanTasks returns the command events one stream line carries, in
	// order, or nil for a line that carries none. Only foreground shell
	// commands are reported started: a backgrounded one outlives the call
	// that ran it, and other task types write no output file. answers says
	// whether the caller has a command whose tool call may still be
	// answered; without one, a tool result is passed over undecoded, as is
	// every line that cannot carry a command event: the caller runs this on
	// every line of the stream.
	ScanTasks(line []byte, answers bool) []TaskEvent
	// TaskOutputGlob returns the filepath.Glob pattern matching the file a
	// running command's output is written to, for command task of session,
	// run by a CLI with environment env (its effective one: the last
	// assignment of a name wins) as user uid. "" when there is none to look
	// for. The file exists only while the command runs.
	TaskOutputGlob(env []string, uid int, session, task string) string
}

// TaskEventKind says what a TaskEvent reports.
type TaskEventKind int

// The command events.
const (
	// TaskSession: the run's session started; Session names it.
	TaskSession TaskEventKind = iota + 1
	// TaskStarted: a foreground shell command started. Task names it,
	// ToolUse the tool call that runs it, and Session its session when the
	// line names one.
	TaskStarted
	// TaskEnded: a command ended. Task names it, ToolUse its tool call.
	TaskEnded
	// TaskAnswered: a tool call was answered; ToolUse names it. A command's
	// call is answered once the command has ended, so this ends a command
	// whose own end was never reported.
	TaskAnswered
)

// TaskEvent is one command event off a CLI's stream.
type TaskEvent struct {
	Kind    TaskEventKind
	Session string
	Task    string
	ToolUse string
}

// Resumer is the optional capability of continuing a session an earlier run
// left behind, with one more prompt. agentrun uses it to have the agent that
// wrote a report maul refused repair it in the same conversation, which
// still holds everything it did. A harness without it has no repair.
type Resumer interface {
	// ResumeSpec builds the headless command that continues session
	// sessionID in workspace ws with req.Prompt as the next message, under
	// the rest of req exactly as PromptSpec renders it: model, turn cap,
	// sandbox posture, directories, environment. req.SessionID is ignored;
	// sessionID names the session.
	ResumeSpec(ws, sessionID string, req PromptRequest) runner.CommandSpec
}

// All returns the builtin harness set.
func All() []Harness {
	return []Harness{NewClaude(), NewCodex(), NewCopilot(), NewFake()}
}

// ByID returns the builtin harness with the given id, if any.
func ByID(id string) (Harness, bool) {
	for _, h := range All() {
		if h.ID() == id {
			return h, true
		}
	}
	return nil, false
}

// Available finds the first of the harness's runner binaries on PATH.
func Available(h Harness) (path string, ok bool) {
	for _, name := range h.CLI() {
		if p, err := exec.LookPath(name); err == nil {
			return p, true
		}
	}
	return "", false
}

// AvailableIn finds the harness's runner binary under dir alone, never
// falling back to PATH: an injected CLI that is missing must be reported,
// not quietly replaced by the image's own. It is how a pod running a
// repository-declared image resolves the CLI maul injected (agentrun's
// preflight, whose result the stage then runs by absolute path).
func AvailableIn(h Harness, dir string) (path string, ok bool) {
	for _, name := range h.CLI() {
		p := filepath.Join(dir, name)
		if info, err := os.Stat(p); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return p, true
		}
	}
	return "", false
}

// base carries the descriptive fields shared by all harnesses.
type base struct {
	id      string
	name    string
	clis    []string
	envKeys []string
}

func (b base) ID() string        { return b.id }
func (b base) Name() string      { return b.name }
func (b base) CLI() []string     { return b.clis }
func (b base) EnvKeys() []string { return b.envKeys }
