// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/checks"
	"github.com/devthenet-labs/maul/internal/envelope"
	"github.com/devthenet-labs/maul/internal/harness"
	"github.com/devthenet-labs/maul/internal/report"
	"github.com/devthenet-labs/maul/internal/runner"
	"github.com/devthenet-labs/maul/internal/templates"
)

// The evaluator report probe's environment. TestEvaluatorReportProbe skips
// unless evalProbeCLIEnv names the real claude binary and a model credential
// (evalProbeCredentialEnvs) is set: `mise run evaluator-report-probe`
// fetches the CLI version Dockerfile.claude-agent-runner pins and sets the
// first, the caller the second.
const (
	evalProbeCLIEnv   = "MAUL_EVALUATOR_PROBE_CLI"
	evalProbeModelEnv = "MAUL_EVALUATOR_PROBE_MODEL"
	// evalProbeModel is the model the probe asks by default: the CLI id of
	// intent-controller's --intent-evaluate-model default.
	evalProbeModel = "claude-sonnet-5"
	// evalProbeToolEnv turns the test binary into the stand-in maul-tool
	// (TestEvaluatorReportProbeTool); its value is the tool's state
	// directory.
	evalProbeToolEnv = "MAUL_EVALUATOR_PROBE_TOOL_STATE"
	// evalProbeRunBudgetUSD caps each CLI run's spend (--max-budget-usd);
	// a run is expected to cost well under it.
	evalProbeRunBudgetUSD = "0.40"
)

// evalProbeCredentialEnvs are the credentials the claude CLI takes from its
// environment, either of which the probe passes through: an API key, or a
// `claude setup-token` subscription token.
var evalProbeCredentialEnvs = []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"}

// The probe's synthetic round: three outcomes of a tiny API, the first of
// which the stand-in preview satisfies, the second of which it breaks, and
// a manual third.
var evalProbeOutcomes = []acceptance.PlanOutcome{
	{
		ID:      "greets-by-name",
		Outcome: `GET /api/greet?name=<name> answers 200 with the JSON {"greeting": "Hello, <name>!"}.`,
		From:    "greet people by name",
		Kind:    acceptance.OutcomeKindChange,
	},
	{
		ID:      "counts-visits",
		Outcome: `POST /api/visits records a visit, and GET /api/visits then answers {"count": n} with n visits.`,
		From:    "count the visits",
		Kind:    acceptance.OutcomeKindChange,
	},
	{
		ID:      "reads-warmly",
		Outcome: "The greeting reads as warm and friendly.",
		From:    "make it friendly",
		Manual:  true,
		Reason:  "only a person can judge the tone",
	},
}

// TestEvaluatorReportProbe asks the REAL model, through the real claude CLI
// under the evaluate posture, to write an evaluation report for a tiny
// synthetic round, and holds what it writes to the same acceptance the
// evaluate stage applies (report.ParseEvaluation, then checkProposed). The
// e2e suite's scripted claude always writes the format exactly, so only a
// real model shows whether the prompt actually teaches it.
//
// Both prompts that ask for the report are probed, each in a session of its
// own: the evaluate prompt (RenderEvaluatePrompt) on a run with room to
// finish, and the out-of-turns repair (RenderRepairPrompt, OutOfTurns) on a
// run cut off after two turns, as settleReport resumes one. The preview is a
// stand-in maul-tool (this test binary, TestEvaluatorReportProbeTool) that
// numbers its calls as the ledger does and answers from a fixed fake
// application, so the model has real ledger seqs and digests to cite.
//
// It costs real money: two short sessions on the evaluate model, in the
// order of $0.20 in all, each CLI run capped by turns and by
// evalProbeRunBudgetUSD (each run's cost is logged). It never runs in
// `go test ./...`: it skips without evalProbeCLIEnv and a credential.
func TestEvaluatorReportProbe(t *testing.T) {
	cli := os.Getenv(evalProbeCLIEnv)
	if cli == "" {
		t.Skipf("%s is not set; run `mise run evaluator-report-probe`", evalProbeCLIEnv)
	}
	cred := evalProbeCredential()
	if cred == "" {
		t.Skipf("no model credential: set one of %s", strings.Join(evalProbeCredentialEnvs, ", "))
	}
	model := os.Getenv(evalProbeModelEnv)
	if model == "" {
		model = evalProbeModel
	}
	var total float64

	t.Run("evaluate prompt", func(t *testing.T) {
		p := newEvalReportProbe(t, cli, cred, model)
		// A small stage as the controller hands it out: the first run keeps
		// reportReserve back, as explore does.
		const stageTurns = 14
		first := stageTurns - reportReserve(stageTurns)
		res := p.run(t, p.firstSpec(t, first, first))
		total += res.cost
		outcome, detail := p.settle()
		if outcome == envelope.OutcomeReportMissing && res.capped {
			// What the evaluate prompt teaches is still judged: only a
			// report it never got to write is asked for, as settleReport
			// asks, and a refused one fails here and now.
			t.Logf("the run reached its %d turns without a report; asking for it as settleReport does", first)
			res = p.run(t, p.repairSpec(t, res, detail, min(repairMaxTurns, stageTurns-res.turns)))
			total += res.cost
			outcome, detail = p.settle()
		}
		p.judge(t, outcome, detail)
	})

	t.Run("out-of-turns repair prompt", func(t *testing.T) {
		p := newEvalReportProbe(t, cli, cred, model)
		// The prompt states a stage's real budget, but the CLI is cut off
		// after two turns: well before any report, so the repair prompt is
		// the one that asks for it.
		const stageTurns, cut = 100, 2
		first := stageTurns - reportReserve(stageTurns)
		res := p.run(t, p.firstSpec(t, first, cut))
		total += res.cost
		outcome, detail := p.settle()
		if outcome != envelope.OutcomeReportMissing {
			t.Fatalf("the run wrote a report within %d turns; the repair prompt was not reached", cut)
		}
		if !res.capped {
			t.Fatalf("the run ended without a report and without reaching its %d turns (%s)", cut, res.summary)
		}
		res = p.run(t, p.repairSpec(t, res, detail, repairMaxTurns))
		total += res.cost
		outcome, detail = p.settle()
		p.judge(t, outcome, detail)
	})

	t.Logf("evaluator report probe: %s, total cost $%.4f", model, total)
}

// evalReportProbe is one session's layout, as the evaluator pod lays it
// out: HOME at work, the run in work/scratch with its checks and shots
// directories, and the stand-in maul-tool first on PATH.
type evalReportProbe struct {
	cli, cred, model      string
	work, scratch, binDir string
	session               string
	// accepted is the last report settle accepted.
	accepted *report.Evaluation
}

func newEvalReportProbe(t *testing.T, cli, cred, model string) *evalReportProbe {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := &evalReportProbe{
		cli: cli, cred: cred, model: model,
		work:    filepath.Join(root, "work"),
		binDir:  filepath.Join(root, "bin"),
		session: newProbeSessionID(t),
	}
	p.scratch = filepath.Join(p.work, "scratch")
	state := filepath.Join(root, "tool-state")
	for _, dir := range []string{p.checksDir(), filepath.Join(p.scratch, "shots"), p.binDir, state} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// The stand-in maul-tool re-enters this test binary; its environment is
	// set here, inside the script, so the model's command stays a bare
	// `maul-tool ...` as the posture requires.
	script := fmt.Sprintf("#!/bin/sh\n%s=%s exec %s -test.run='^TestEvaluatorReportProbeTool$' -- \"$@\"\n",
		evalProbeToolEnv, shellQuote(state), shellQuote(self))
	tool := filepath.Join(p.binDir, "maul-tool")
	if err := os.WriteFile(tool, []byte(script), 0o755); err != nil { //nolint:gosec // an executable stand-in
		t.Fatal(err)
	}
	return p
}

func (p *evalReportProbe) reportPath() string { return filepath.Join(p.scratch, "report.md") }
func (p *evalReportProbe) checksDir() string  { return filepath.Join(p.scratch, "checks") }

// handoff is the synthetic round as the controller would hand it over.
func (p *evalReportProbe) handoff() EvaluateHandoff {
	return EvaluateHandoff{
		Round:    1,
		RunToken: "probe7",
		Request:  "Add a greeting endpoint that greets people by name, count the visits, and make it friendly.",
		Outcomes: evalProbeOutcomes,
		Components: []EvaluateComponent{{
			Key: "api", Path: "/",
		}},
		Tools:        EvaluateTools{MaxToolCalls: 20},
		ChangedPaths: []ChangedPath{{Path: "server/greet.go", Added: 40}, {Path: "server/visits.go", Added: 25}},
	}
}

// request is the evaluate stage's request for a run of turns turns.
func (p *evalReportProbe) request(prompt string, turns int) harness.PromptRequest {
	return harness.PromptRequest{
		Prompt: prompt, Model: p.model, MaxTurns: turns, Sandbox: harness.SandboxEvaluate,
	}
}

// firstSpec is the stage's first run: the evaluate prompt rendered for
// stated turns, the CLI capped at turns.
func (p *evalReportProbe) firstSpec(t *testing.T, stated, turns int) runner.CommandSpec {
	t.Helper()
	h := p.handoff()
	if err := validEvaluateHandoff(h); err != nil {
		t.Fatalf("the probe's handoff: %v", err)
	}
	prompt, err := templates.RenderEvaluatePrompt(h.prompt(p.reportPath(), p.checksDir(),
		filepath.Join(p.scratch, "shots"), templates.StageLimits{
			MaxTurns: stated, TokenBudget: 200000, Timeout: 20 * time.Minute,
		}))
	if err != nil {
		t.Fatal(err)
	}
	req := p.request(prompt, turns)
	req.SessionID = p.session
	return harness.NewClaude().PromptSpec(p.scratch, req)
}

// repairSpec resumes the session with the out-of-turns repair prompt for
// a run that ended with no report, as repairRound does: detail is
// readReportAs's.
func (p *evalReportProbe) repairSpec(t *testing.T, prev probeRun, detail string, turns int) runner.CommandSpec {
	t.Helper()
	prompt, err := templates.RenderRepairPrompt(templates.RepairPrompt{
		ReportPath: p.reportPath(), Missing: true, OutOfTurns: true, Error: detail,
		Round: 1, Rounds: repairRounds,
	})
	if err != nil {
		t.Fatal(err)
	}
	session := prev.session
	if session == "" {
		session = p.session
	}
	return harness.NewClaude().ResumeSpec(p.scratch, session, p.request(prompt, max(turns, 2)))
}

// probeRun is what one CLI run ended with.
type probeRun struct {
	session, summary string
	turns            int
	capped           bool
	cost             float64
}

// run executes spec with the real CLI: a clean environment holding only the
// credential, HOME at work and the stand-in maul-tool on PATH.
func (p *evalReportProbe) run(t *testing.T, spec runner.CommandSpec) probeRun {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	argv := slices.Clone(spec.Argv)
	argv[0] = p.cli
	// The probe's own spending cap, beside the posture's turn cap: a run
	// that reaches it fails the probe rather than the bill growing.
	argv = append(argv, "--max-budget-usd", evalProbeRunBudgetUSD)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = spec.Dir
	cmd.Env = append([]string{
		"HOME=" + p.work,
		"PATH=" + p.binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		p.cred,
		"DISABLE_AUTOUPDATER=1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
	}, spec.Env...)
	cmd.Stdin = strings.NewReader("")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	c := harness.NewClaude()
	res, ok := c.ParseResult(stdout.Bytes())
	if !ok {
		t.Fatalf("no result event from the CLI (exit %v)\nstderr:\n%s\nstdout:\n%s", runErr, stderr.String(),
			stdout.String())
	}
	out := probeRun{session: res.SessionID, turns: res.NumTurns, capped: c.Exhausted(stdout.Bytes())}
	if res.Usage != nil && res.Usage.CostUSD != nil {
		out.cost = *res.Usage.CostUSD
	}
	out.summary = fmt.Sprintf("subtype %q, %d turns, $%.4f", res.Subtype, res.NumTurns, out.cost)
	t.Logf("run: %s", out.summary)
	if res.IsError && !out.capped {
		t.Fatalf("the run failed: %s\n%s\nstderr:\n%s", out.summary, c.TerminalError(stdout.Bytes()), stderr.String())
	}
	return out
}

// settle reads the report and holds it to the evaluate stage's acceptance,
// as settleReport does: readReportAs over readReport and parseEvaluation's
// two steps (report.ParseEvaluation, then the proposals against the check
// files).
func (p *evalReportProbe) settle() (envelope.Outcome, string) {
	e, _, outcome, detail := readReportAs(p.reportPath(), readReport, func(raw []byte) (*report.Evaluation, error) {
		e, err := report.ParseEvaluation(raw)
		if err != nil {
			return nil, err
		}
		if err := checkProposed(p.checksDir(), e.Proposed); err != nil {
			return nil, fmt.Errorf("report: evaluation: %w", err)
		}
		return e, nil
	})
	p.accepted = e
	return outcome, detail
}

// judge fails the probe unless the report was accepted, with the
// decoder's errors and the report as the model wrote it.
func (p *evalReportProbe) judge(t *testing.T, outcome envelope.Outcome, detail string) {
	t.Helper()
	switch outcome {
	case envelope.OutcomeOK:
	case envelope.OutcomeReportMissing:
		t.Fatalf("the model wrote no report: %s", detail)
	default:
		raw, _ := os.ReadFile(p.reportPath())
		t.Fatalf("maul refused the report the model wrote (%s):\n%s\n\nthe report:\n%s", outcome, detail, raw)
	}
	ids := make([]string, 0, len(p.accepted.Verdicts))
	for _, v := range p.accepted.Verdicts {
		ids = append(ids, v.ID+"="+string(v.Verdict))
	}
	t.Logf("the report was accepted: verdicts %v, %d proposed", ids, len(p.accepted.Proposed))
	for _, o := range evalProbeOutcomes {
		if !slices.ContainsFunc(p.accepted.Verdicts, func(v report.OutcomeVerdict) bool { return v.ID == o.ID }) {
			t.Errorf("the report gives outcome %q no verdict", o.ID)
		}
	}
}

// evalProbeCredential is the first credential set in the environment, as
// NAME=value, or "".
func evalProbeCredential() string {
	for _, name := range evalProbeCredentialEnvs {
		if v := os.Getenv(name); v != "" {
			return name + "=" + v
		}
	}
	return ""
}

// newProbeSessionID is a random UUIDv4, the --session-id grammar.
func newProbeSessionID(t *testing.T) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// TestEvaluatorReportProbeTool is the stand-in maul-tool, not a test: the
// probe's maul-tool script runs this binary with evalProbeToolEnv set, and
// it answers one command and exits. Without the variable it skips.
func TestEvaluatorReportProbeTool(t *testing.T) {
	state := os.Getenv(evalProbeToolEnv)
	if state == "" {
		t.Skip("the evaluator report probe's stand-in maul-tool; run by TestEvaluatorReportProbe only")
	}
	args := os.Args
	if i := slices.Index(args, "--"); i >= 0 {
		args = args[i+1:]
	}
	out, code := probeTool(state, args)
	_, _ = os.Stdout.WriteString(out)
	os.Exit(code) //nolint:revive // the helper process ends here, before the test framework prints
}

// probeTool answers one maul-tool command from the fake application: a
// greeting that works and a visit counter that never counts. It numbers
// every call it accepts as the ledger does, and runs a check by parsing and
// validating it with the real grammar, then passing it if it is bound to the
// working outcome and failing it otherwise.
func probeTool(state string, args []string) (string, int) {
	refuse := func(why string) (string, int) { return "maul-tool: " + why + "\n", 3 }
	if len(args) == 0 {
		return refuse("no command")
	}
	var body string
	switch {
	case len(args) == 2 && args[0] == "check" && args[1] == "results":
		body = "[observed] no saved checks: this is the change's first round\n"
	case len(args) == 3 && args[0] == "check" && args[1] == "run":
		b, err := probeCheckRun(args[2])
		if err != nil {
			return refuse(err.Error())
		}
		body = b
	case len(args) >= 3 && args[0] == "http":
		body = probeHTTP(args[1], args[2])
	case len(args) == 2 && args[0] == "logs":
		body = "[untrusted output]\nlistening on :8080\n"
	case len(args) == 1 && args[0] == "events":
		body = "[untrusted output]\nno events\n"
	default:
		return refuse("unknown command " + strconv.Quote(strings.Join(args, " ")))
	}
	seq, err := nextSeq(state)
	if err != nil {
		return "maul-tool: " + err.Error() + "\n", 4
	}
	return fmt.Sprintf("[maul-tool ledger seq %d]\n%s", seq, body), 0
}

// probeHTTP is the fake application's answer to one request.
func probeHTTP(method, target string) string {
	u, err := url.Parse(target)
	if err != nil {
		return "[observed] http status 400 in 2 ms\n"
	}
	status, resp := 404, `{"error": "not found"}`
	switch {
	case method == "GET" && u.Path == "/api/greet":
		status, resp = 200, fmt.Sprintf(`{"greeting": %q}`, "Hello, "+u.Query().Get("name")+"!")
	case method == "POST" && u.Path == "/api/visits":
		status, resp = 201, `{"recorded": true}`
	case method == "GET" && u.Path == "/api/visits":
		status, resp = 200, `{"count": 0}`
	}
	return fmt.Sprintf("[observed] http status %d in 3 ms\n[untrusted output]\n%s\n", status, resp)
}

// probeCheckRun dry-runs a check file: refused as maul-tool refuses one the
// grammar does not admit, else pass for the working outcome and fail for
// any other, printed as maul-tool prints a check run.
func probeCheckRun(path string) (string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // a path the model names, under the probe's scratch
	if err != nil {
		return "", err
	}
	c, err := checks.Parse(data)
	if err != nil {
		return "", err
	}
	if err := checks.Validate(c, checks.Preview{Components: []string{"api"}}); err != nil {
		return "", err
	}
	digest := strings.TrimPrefix(checks.Digest(c), "sha256:")
	if c.Outcome == "greets-by-name" {
		return fmt.Sprintf("[observed] check %s digest sha256:%s dry-run: pass (attempts: pass, pass)\n", c.ID, digest), nil
	}
	return fmt.Sprintf("[observed] check %s digest sha256:%s dry-run: fail (attempts: fail, fail)\n"+
		"[observed]   attempt 1 step 2 fail: %q\n", c.ID, digest, `expect json /count: got 0, want 1`), nil
}

// nextSeq takes the next ledger seq from the state directory.
func nextSeq(state string) (int, error) {
	path := filepath.Join(state, "seq")
	n := 0
	if b, err := os.ReadFile(path); err == nil { //nolint:gosec // the probe's own state file
		n, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	}
	n++
	return n, os.WriteFile(path, []byte(strconv.Itoa(n)), 0o600)
}

// TestEvaluatorReportProbeStandIn runs the probe's stand-in maul-tool the
// way the model does, through its script on PATH, so the free part of the
// probe is checked on every `go test`: the ledger numbering, the fake
// application, a check run's digest (the one checkProposed then matches)
// and the refusals.
func TestEvaluatorReportProbeStandIn(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh is not on PATH")
	}
	p := newEvalReportProbe(t, "", "", "")
	tool := func(args ...string) (string, int) {
		t.Helper()
		cmd := exec.Command(filepath.Join(p.binDir, "maul-tool"), args...)
		cmd.Dir = p.scratch
		out, err := cmd.Output()
		var exit *exec.ExitError
		if err != nil && !errors.As(err, &exit) {
			t.Fatalf("maul-tool %q: %v", args, err)
		}
		return string(out), cmd.ProcessState.ExitCode()
	}
	check := `{"id": "greets-by-name", "outcome": "greets-by-name", "form": "http", "http": {"target": "api",
  "steps": [{"request": {"method": "GET", "path": "/api/greet?name={{run}}-al"},
             "expect": {"status": 200, "json": {"/greeting": "Hello, {{run}}-al!"}}}]}}`
	if err := os.WriteFile(filepath.Join(p.checksDir(), "greets.json"), []byte(check), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := checks.Parse([]byte(check))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"check", "results"}, 0, "[maul-tool ledger seq 1]\n[observed] no saved checks"},
		{[]string{"http", "GET", "/api/greet?name=probe7-al"}, 0,
			"[maul-tool ledger seq 2]\n[observed] http status 200 in 3 ms\n[untrusted output]\n" +
				`{"greeting": "Hello, probe7-al!"}`},
		{[]string{"http", "GET", "/api/visits"}, 0, `{"count": 0}`},
		{[]string{"check", "run", "checks/greets.json"}, 0, "[maul-tool ledger seq 4]\n[observed] check " +
			"greets-by-name digest " + checks.Digest(c) + " dry-run: pass"},
		{[]string{"check", "run", "checks/absent.json"}, 3, "maul-tool: open checks/absent.json"},
		{[]string{"browser", "snapshot"}, 3, "maul-tool: unknown command"},
		{[]string{"events"}, 0, "[maul-tool ledger seq 5]\n"},
	} {
		got, code := tool(tc.args...)
		if code != tc.code || !strings.Contains(got, tc.want) {
			t.Errorf("maul-tool %q = %d %q, want %d and %q", tc.args, code, got, tc.code, tc.want)
		}
	}
	if err := checkProposed(p.checksDir(), []report.ProposedCheck{
		{ID: "greets-by-name", Outcome: "greets-by-name", Digest: checks.Digest(c)},
	}); err != nil {
		t.Errorf("a proposal of the stand-in's digest is refused: %v", err)
	}
}
