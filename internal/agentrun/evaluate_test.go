// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/checks"
	"github.com/devthenet-labs/maul/internal/envelope"
	"github.com/devthenet-labs/maul/internal/harness"
	"github.com/devthenet-labs/maul/internal/runner"
	"github.com/devthenet-labs/maul/internal/templates"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// goodEvaluation is an evaluation report the agent writes under scratch.
const goodEvaluation = `---
verdicts:
  - id: rate-battle
    verdict: pass
    ledger: [12, 14]
    note: "The rating moved by 16 points."
  - id: leaderboard
    verdict: unsure
anomalies:
  - summary: "Console error on /battles"
    route: /battles
    ledger: [20]
---
Rated two battles.
`

// evalHandoff is an evaluator's handoff, as the controller stages it in
// input/issue.md, edited by edit (nil for none). Its request's zero-width
// space must reach the prompt escaped.
func evalHandoff(edit func(*EvaluateHandoff)) string {
	h := EvaluateHandoff{
		Round:    1,
		RunToken: "r1a1x7",
		Request:  "Rate battles with Elo.\u200b",
		Outcomes: []acceptance.PlanOutcome{
			{ID: "rate-battle", Outcome: "Rating a battle moves the winner's rating up.", Kind: acceptance.OutcomeKindChange},
			{ID: "leaderboard", Outcome: "The leaderboard still lists every player.", Kind: acceptance.OutcomeKindKeep},
		},
		Components:   []EvaluateComponent{{Key: "api", Path: "/api"}, {Key: "web", Path: "/"}},
		Tools:        EvaluateTools{Browser: true, MaxToolCalls: 300, LookAroundRoutes: 8},
		ChangedPaths: []ChangedPath{{Path: "api/elo.go", Added: 40, Deleted: 2}},
	}
	if edit != nil {
		edit(&h)
	}
	raw, err := json.Marshal(h)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// evaluateHandoff is the default evaluator handoff.
var evaluateHandoff = evalHandoff(nil)

// roleHandoff is a valid handoff for a role phase.
func roleHandoff(phase Phase) string {
	if phase == PhaseEvaluate {
		return evaluateHandoff
	}
	return reviewHandoff(1)
}

// roleWorkspace builds a role Job's /work as its prepare init leaves it: the
// handoff in input/issue.md and an empty scratch directory, no clone.
func roleWorkspace(t *testing.T, handoff string) string {
	t.Helper()
	ws := t.TempDir()
	for _, dir := range []string{"input", "scratch"} {
		if err := os.MkdirAll(filepath.Join(ws, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(ws, "input", "issue.md"), []byte(handoff), 0o644); err != nil {
		t.Fatal(err)
	}
	return ws
}

// roleConfig is an evaluate or checkreview Job's configuration over a fresh
// role workspace holding handoff, its tool pod a fresh fakeTools.
func roleConfig(t *testing.T, phase Phase, handoff string, out *bytes.Buffer) (Config, string) {
	t.Helper()
	ws := roleWorkspace(t, handoff)
	cfg := newConfig(t, ws, out)
	cfg.Phase = phase
	cfg.Finding = "target-1-ev-r1-a1"
	// A role stage has the wall clock a repair round needs (repairMinWindow).
	cfg.InvestigateTimeout = 30 * time.Minute
	useTools(t, &cfg)
	return cfg, ws
}

// fakeTools is the evaluator's tool pod, as agent-runner reaches it: it
// records every call and answers each with the next ledger seq, refusing
// the verbs in refuse.
type fakeTools struct {
	mu     sync.Mutex
	calls  []toolsCall
	refuse map[toolwire.Verb]string
	seq    int64
}

type toolsCall struct {
	verb toolwire.Verb
	body []byte
}

// useTools points cfg at a new fakeTools and returns it.
func useTools(t *testing.T, cfg *Config) *fakeTools {
	t.Helper()
	f := &fakeTools{refuse: map[toolwire.Verb]string{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	cfg.ToolsURL = srv.URL
	return f
}

func (f *fakeTools) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	verb := toolwire.Verb(strings.TrimPrefix(r.URL.Path, "/v1/"))
	var body bytes.Buffer
	_, _ = body.ReadFrom(r.Body)
	f.mu.Lock()
	f.calls = append(f.calls, toolsCall{verb: verb, body: body.Bytes()})
	f.seq++
	resp := toolwire.Response{Seq: 100 + f.seq, Refused: f.refuse[verb]}
	f.mu.Unlock()
	if resp.Refused != "" {
		resp.Reason = "refused by the test"
		w.WriteHeader(http.StatusConflict)
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// verbs are the verbs called, in order.
func (f *fakeTools) verbs() []toolwire.Verb {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]toolwire.Verb, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, c.verb)
	}
	return out
}

// request decodes the last call of verb into req.
func (f *fakeTools) request(t *testing.T, verb toolwire.Verb, req toolwire.Request) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.calls) - 1; i >= 0; i-- {
		if f.calls[i].verb == verb {
			if err := toolwire.DecodeRequest(bytes.NewReader(f.calls[i].body), req); err != nil {
				t.Fatalf("%s request %s: %v", verb, f.calls[i].body, err)
			}
			return
		}
	}
	t.Fatalf("no %s call", verb)
}

// rateBattleCheck is a check the evaluator saved for rate-battle, and its
// file's JSON.
var rateBattleCheck = mustCheck(`{"id":"rate-battle.api","outcome":"rate-battle","form":"http","http":{` +
	`"target":"api","steps":[{"request":{"method":"POST","path":"/api/battles","json":{"winner":"{{run}}-a"}},` +
	`"expect":{"status":201,"bodyContains":"{{run}}-a"}}]}}`)

func mustCheck(raw string) checks.Check {
	c, err := checks.Parse([]byte(raw))
	if err != nil {
		panic(err)
	}
	return c
}

// proposingEvaluation is goodEvaluation proposing rateBattleCheck under id,
// outcome and digest.
func proposingEvaluation(id, outcome, digest string) string {
	return strings.Replace(goodEvaluation, "anomalies:", "proposed:\n  - id: \""+id+"\"\n    outcome: "+outcome+
		"\n    digest: \""+digest+"\"\nanomalies:", 1)
}

// pngBytes is a screenshot's bytes.
var pngBytes = "\x89PNG\r\n\x1a\nscreenshot"

func TestEvaluateEvent(t *testing.T) {
	var out bytes.Buffer
	cfg, ws := roleConfig(t, PhaseEvaluate, evaluateHandoff, &out)
	brokeredClaude(t, &cfg)
	tools := useTools(t, &cfg)
	digest := checks.Digest(rateBattleCheck)
	report := proposingEvaluation("rate-battle.api", "rate-battle", digest)
	fx := &fakeExec{steps: []step{{ws: ws, stdout: streamSuccess, writes: map[string]string{
		"scratch/report.md":                   report,
		"scratch/checks/rate-battle-api.json": string(checks.Canonical(rateBattleCheck)),
		"scratch/checks/notes.txt":            "not a check",
		"scratch/shots/14.png":                pngBytes,
		"scratch/shots/20.png":                pngBytes + "2",
		"scratch/shots/99.png":                pngBytes + "uncited",
	}}}}
	ev := onlyEvent(t, cfg, fx, &out)

	if ev.Type != envelope.TypeEvaluation || ev.Evaluation == nil {
		t.Fatalf("event = %+v, want an evaluation event", ev)
	}
	if ev.Repo != "acme/shop" || ev.Finding != "target-1-ev-r1-a1" {
		t.Errorf("event keyed %s/%s, want the run it belongs to", ev.Repo, ev.Finding)
	}
	e := ev.Evaluation
	if e.Outcome != envelope.OutcomeOK || e.ReportMarkdown != report {
		t.Fatalf("evaluation = %+v, want ok with the report verbatim", e)
	}
	if e.Harness != cfg.InvestigateHarness || e.Model != cfg.InvestigateModel {
		t.Errorf("stage ran on %s/%s, want the investigate stage's", e.Harness, e.Model)
	}
	if e.Usage.OutputTokens != 30 || e.NumTurns != 7 {
		t.Errorf("stage accounting = %+v, want the run's usage", e.Stage)
	}
	checkEvaluationClaims(t, e, digest)
	checkEvaluateTools(t, tools, digest, e.Cited)
	checkEvaluateCommand(t, fx.specs[0], ws)
}

// checkEvaluationClaims: the event carries the report's claims.
func checkEvaluationClaims(t *testing.T, e *envelope.Evaluation, digest string) {
	t.Helper()
	if len(e.Verdicts) != 2 || e.Verdicts[0].ID != "rate-battle" || e.Verdicts[0].Verdict != "pass" ||
		!slices.Equal(e.Verdicts[0].Ledger, []int32{12, 14}) || e.Verdicts[0].Note != "The rating moved by 16 points." {
		t.Errorf("verdicts = %+v, want the report's", e.Verdicts)
	}
	if len(e.Anomalies) != 1 || e.Anomalies[0].Route != "/battles" || !slices.Equal(e.Anomalies[0].Ledger, []int32{20}) {
		t.Errorf("anomalies = %+v, want the report's", e.Anomalies)
	}
	if len(e.Proposed) != 1 || e.Proposed[0].Digest != digest {
		t.Errorf("proposed = %+v, want the report's", e.Proposed)
	}
}

// checkEvaluateTools: after the agent exits, the proposal is confirmed by
// its bare digest, then the tool pod finished with the cited screenshots as
// evidence, and the event cites what finish named.
func checkEvaluateTools(t *testing.T, tools *fakeTools, digest string, cited []int32) {
	t.Helper()
	if got := tools.verbs(); !slices.Equal(got, []toolwire.Verb{toolwire.VerbConfirm, toolwire.VerbFinish}) {
		t.Fatalf("tool calls = %v, want confirm then finish", got)
	}
	var confirm toolwire.ConfirmRequest
	tools.request(t, toolwire.VerbConfirm, &confirm)
	if want := []string{strings.TrimPrefix(digest, "sha256:")}; !slices.Equal(confirm.Digests, want) {
		t.Errorf("confirm digests = %v, want %v (bare hex, as the tools container keys a check)", confirm.Digests, want)
	}
	var finish toolwire.FinishRequest
	tools.request(t, toolwire.VerbFinish, &finish)
	want := []toolwire.EvidenceRef{{Seq: 14, SHA256: sha256Hex(pngBytes)}, {Seq: 20, SHA256: sha256Hex(pngBytes + "2")}}
	if !slices.Equal(finish.Evidence, want) {
		t.Errorf("finish evidence = %+v, want the cited seqs that have screenshots, %+v", finish.Evidence, want)
	}
	if !slices.Equal(cited, []int32{14, 20}) {
		t.Errorf("cited = %v, want the seqs named in finish", cited)
	}
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// checkEvaluateCommand: the evaluate stage runs in scratch, and its prompt
// is the evaluate template rendered from the handoff: maul's own paths and
// run token stated, the request quoted escaped as untrusted data.
func checkEvaluateCommand(t *testing.T, spec runner.CommandSpec, ws string) {
	t.Helper()
	if spec.Dir != filepath.Join(ws, "scratch") {
		t.Errorf("stage ran in %s, want the scratch directory", spec.Dir)
	}
	prompt := argvFlag(t, spec.Argv, "-p")
	for _, want := range []string{
		"You are maul's evaluator.",
		"This is round 1 of this change's evaluation.",
		filepath.Join(ws, "scratch", "report.md"),
		filepath.Join(ws, "scratch", "checks"),
		filepath.Join(ws, "scratch", "shots"),
		"r1a1x7-alice",
		"Rating a battle moves the winner's rating up.",
		"Rate battles with Elo.<U+200B>",
		"+40 -2 api/elo.go",
		"`api`: served under `/api`",
		"at most 300 tool calls",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, prompt)
		}
	}
}

// TestEvaluateRepairsAMissingVerdict: a report whose frontmatter has no
// verdict is refused and repaired in the agent's own session; the repaired
// report is the one recorded.
func TestEvaluateRepairsAMissingVerdict(t *testing.T) {
	var out bytes.Buffer
	cfg, ws := roleConfig(t, PhaseEvaluate, evaluateHandoff, &out)
	brokeredClaude(t, &cfg)
	missing := "---\nanomalies: []\n---\nForgot the verdicts.\n"
	fx := &fakeExec{steps: []step{
		{ws: ws, stdout: streamSuccess, writes: map[string]string{"scratch/report.md": missing}},
		{ws: ws, stdout: streamSuccess, writes: map[string]string{"scratch/report.md": goodEvaluation}},
	}}
	e := onlyEvent(t, cfg, fx, &out).Evaluation
	if e == nil || e.Outcome != envelope.OutcomeOK || e.ReportMarkdown != goodEvaluation || len(e.Verdicts) != 2 {
		t.Fatalf("evaluation = %+v, want the repaired report", e)
	}
	if len(fx.specs) != 2 || !slices.Contains(fx.specs[1].Argv, "--resume") {
		t.Fatalf("commands = %d, want the stage and one resumed repair", len(fx.specs))
	}
	if repair := argvFlag(t, fx.specs[1].Argv, "-p"); !strings.Contains(repair, "verdicts is required") {
		t.Errorf("repair prompt does not name the missing verdicts:\n%s", repair)
	}
}

// TestEvaluateProposedCheckMustMatchItsFile: a proposal whose digest is no
// check file's, or whose id or outcome is not that file's, refuses the
// report; repair can fix it, and nothing is confirmed until it does.
func TestEvaluateProposedCheckMustMatchItsFile(t *testing.T) {
	digest := checks.Digest(rateBattleCheck)
	other := checks.Digest(mustCheck(strings.Replace(string(checks.Canonical(rateBattleCheck)), "{{run}}-a",
		"{{run}}-b", 2)))
	tests := []struct {
		name, report, want string
	}{
		{"digest of no file", proposingEvaluation("rate-battle.api", "rate-battle", other), "digest of no check file"},
		{"another id", proposingEvaluation("rate-battle.page", "rate-battle", digest), `"rate-battle.page" is not the id`},
		{"another outcome", proposingEvaluation("rate-battle.api", "leaderboard", digest),
			`"leaderboard" is not the outcome`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			cfg, ws := roleConfig(t, PhaseEvaluate, evaluateHandoff, &out)
			brokeredClaude(t, &cfg)
			tools := useTools(t, &cfg)
			s := step{ws: ws, stdout: streamSuccess, writes: map[string]string{
				"scratch/report.md":                   tt.report,
				"scratch/checks/rate-battle-api.json": string(checks.Canonical(rateBattleCheck)),
			}}
			fx := &fakeExec{steps: []step{s, s, s}}
			e := onlyEvent(t, cfg, fx, &out).Evaluation
			if e == nil || e.Outcome != envelope.OutcomeReportInvalid || len(e.Proposed) != 0 {
				t.Fatalf("evaluation = %+v, want report_invalid with no claims", e)
			}
			if !strings.Contains(e.Detail, tt.want) {
				t.Errorf("detail = %q, want %q", e.Detail, tt.want)
			}
			if got := tools.verbs(); !slices.Equal(got, []toolwire.Verb{toolwire.VerbFinish}) {
				t.Errorf("tool calls = %v, want finish alone: nothing is confirmed from a refused report", got)
			}
			var finish toolwire.FinishRequest
			tools.request(t, toolwire.VerbFinish, &finish)
			if len(finish.Evidence) != 0 || len(e.Cited) != 0 {
				t.Errorf("finish evidence = %+v, cited %v; want none", finish.Evidence, e.Cited)
			}
		})
	}
}

// TestEvaluateNoChecksNoConfirm: a report proposing nothing is not
// confirmed, only finished, and a report citing no screenshot names no
// evidence.
func TestEvaluateNoChecksNoConfirm(t *testing.T) {
	var out bytes.Buffer
	cfg, ws := roleConfig(t, PhaseEvaluate, evaluateHandoff, &out)
	tools := useTools(t, &cfg)
	fx := &fakeExec{steps: []step{{ws: ws, stdout: streamSuccess, writes: map[string]string{
		"scratch/report.md": goodEvaluation,
	}}}}
	e := onlyEvent(t, cfg, fx, &out).Evaluation
	if e.Outcome != envelope.OutcomeOK || len(e.Cited) != 0 {
		t.Fatalf("evaluation = %+v, want ok and nothing cited", e)
	}
	if got := tools.verbs(); !slices.Equal(got, []toolwire.Verb{toolwire.VerbFinish}) {
		t.Errorf("tool calls = %v, want finish alone", got)
	}
}

// TestEvaluateToolsRefusalKeepsTheClaims: a confirm or finish the tools
// container refuses changes nothing the stage claims, except that no
// evidence is cited without a finish taken.
func TestEvaluateToolsRefusalKeepsTheClaims(t *testing.T) {
	var out bytes.Buffer
	cfg, ws := roleConfig(t, PhaseEvaluate, evaluateHandoff, &out)
	tools := useTools(t, &cfg)
	tools.refuse[toolwire.VerbConfirm] = toolwire.RefusedNoDryRun
	tools.refuse[toolwire.VerbFinish] = toolwire.RefusedFinished
	report := proposingEvaluation("rate-battle.api", "rate-battle", checks.Digest(rateBattleCheck))
	fx := &fakeExec{steps: []step{{ws: ws, stdout: streamSuccess, writes: map[string]string{
		"scratch/report.md":                   report,
		"scratch/checks/rate-battle-api.json": string(checks.Canonical(rateBattleCheck)),
		"scratch/shots/12.png":                pngBytes,
	}}}}
	e := onlyEvent(t, cfg, fx, &out).Evaluation
	if e.Outcome != envelope.OutcomeOK || len(e.Proposed) != 1 || len(e.Verdicts) != 2 {
		t.Fatalf("evaluation = %+v, want the report's claims", e)
	}
	if len(e.Cited) != 0 {
		t.Errorf("cited = %v, want none: the finish was refused", e.Cited)
	}
	if got := tools.verbs(); !slices.Equal(got, []toolwire.Verb{toolwire.VerbConfirm, toolwire.VerbFinish}) {
		t.Errorf("tool calls = %v, want confirm then finish", got)
	}
}

// TestEvaluateConfirmsEachCheckAlone: two proposals are confirmed in two
// calls, one digest each, in report order, so the tools container's refusal
// of one (it refuses a whole confirm when any digest it names has no green
// dry run) never costs the other its confirmation; a refused confirm still
// leads to the finish.
func TestEvaluateConfirmsEachCheckAlone(t *testing.T) {
	var out bytes.Buffer
	cfg, ws := roleConfig(t, PhaseEvaluate, evaluateHandoff, &out)
	tools := useTools(t, &cfg)
	tools.refuse[toolwire.VerbConfirm] = toolwire.RefusedNoDryRun
	second := mustCheck(strings.Replace(string(checks.Canonical(rateBattleCheck)), `"rate-battle.api"`,
		`"rate-battle.again"`, 1))
	first, again := checks.Digest(rateBattleCheck), checks.Digest(second)
	report := strings.Replace(goodEvaluation, "anomalies:", "proposed:\n"+
		"  - id: \"rate-battle.api\"\n    outcome: rate-battle\n    digest: \""+first+"\"\n"+
		"  - id: \"rate-battle.again\"\n    outcome: rate-battle\n    digest: \""+again+"\"\nanomalies:", 1)
	fx := &fakeExec{steps: []step{{ws: ws, stdout: streamSuccess, writes: map[string]string{
		"scratch/report.md":                     report,
		"scratch/checks/rate-battle-api.json":   string(checks.Canonical(rateBattleCheck)),
		"scratch/checks/rate-battle-again.json": string(checks.Canonical(second)),
	}}}}
	e := onlyEvent(t, cfg, fx, &out).Evaluation
	if e.Outcome != envelope.OutcomeOK || len(e.Proposed) != 2 {
		t.Fatalf("evaluation = %+v, want both proposals claimed", e)
	}
	want := []toolwire.Verb{toolwire.VerbConfirm, toolwire.VerbConfirm, toolwire.VerbFinish}
	if got := tools.verbs(); !slices.Equal(got, want) {
		t.Fatalf("tool calls = %v, want %v", got, want)
	}
	tools.mu.Lock()
	calls := slices.Clone(tools.calls[:2])
	tools.mu.Unlock()
	for i, d := range []string{first, again} {
		var confirm toolwire.ConfirmRequest
		if err := toolwire.DecodeRequest(bytes.NewReader(calls[i].body), &confirm); err != nil {
			t.Fatal(err)
		}
		if want := []string{strings.TrimPrefix(d, "sha256:")}; !slices.Equal(confirm.Digests, want) {
			t.Errorf("confirm %d digests = %v, want %v alone", i, confirm.Digests, want)
		}
	}
}

// TestEvaluateFinishesAFailedStage: a stage that does not end OK still
// finishes its tool pod, naming no evidence and confirming nothing.
func TestEvaluateFinishesAFailedStage(t *testing.T) {
	var out bytes.Buffer
	cfg, ws := roleConfig(t, PhaseEvaluate, evaluateHandoff, &out)
	tools := useTools(t, &cfg)
	fx := &fakeExec{steps: []step{{ws: ws, stdout: streamExecError, result: runner.Result{ExitCode: 1}}}}
	e := onlyEvent(t, cfg, fx, &out).Evaluation
	if e.Outcome == envelope.OutcomeOK {
		t.Fatalf("evaluation = %+v, want the run's failure", e)
	}
	if got := tools.verbs(); !slices.Equal(got, []toolwire.Verb{toolwire.VerbFinish}) {
		t.Errorf("tool calls = %v, want finish alone", got)
	}
}

// TestEvaluateEvidenceBounds: at most toolwire.MaxEvidence screenshots are
// named, the cited ones only, in seq order.
func TestEvaluateEvidenceBounds(t *testing.T) {
	var out bytes.Buffer
	cfg, ws := roleConfig(t, PhaseEvaluate, evaluateHandoff, &out)
	writes := map[string]string{"scratch/report.md": strings.Replace(goodEvaluation, "ledger: [12, 14]",
		"ledger: [1, 2, 3, 4, 5, 6, 7, 8]", 1)}
	for _, n := range []string{"1", "3", "4", "5", "6", "7", "8", "9"} {
		writes["scratch/shots/"+n+".png"] = pngBytes + n
	}
	tools := useTools(t, &cfg)
	fx := &fakeExec{steps: []step{{ws: ws, stdout: streamSuccess, writes: writes}}}
	e := onlyEvent(t, cfg, fx, &out).Evaluation
	if want := []int32{1, 3, 4, 5, 6, 7}; !slices.Equal(e.Cited, want) {
		t.Errorf("cited = %v, want %v", e.Cited, want)
	}
	var finish toolwire.FinishRequest
	tools.request(t, toolwire.VerbFinish, &finish)
	if len(finish.Evidence) != toolwire.MaxEvidence {
		t.Errorf("finish evidence = %+v, want %d", finish.Evidence, toolwire.MaxEvidence)
	}
}

// TestEvaluateReportRefused: a report that does not parse, and is still
// refused after the repair rounds, ends the stage report_invalid with no
// claims.
func TestEvaluateReportRefused(t *testing.T) {
	var out bytes.Buffer
	cfg, ws := roleConfig(t, PhaseEvaluate, evaluateHandoff, &out)
	bad := "---\nverdicts:\n  - id: rate-battle\n    verdict: pass\n---\n" // a pass with no ledger refs
	s := step{ws: ws, stdout: streamSuccess, writes: map[string]string{"scratch/report.md": bad}}
	fx := &fakeExec{steps: []step{s, s, s}}
	ev := onlyEvent(t, cfg, fx, &out)
	e := ev.Evaluation
	if e == nil || e.Outcome != envelope.OutcomeReportInvalid || e.ReportMarkdown != "" || len(e.Verdicts) != 0 {
		t.Fatalf("evaluation = %+v, want report_invalid with no claims", e)
	}
	if len(fx.specs) != 1+repairRounds {
		t.Errorf("commands run = %d, want the stage and %d repair rounds", len(fx.specs), repairRounds)
	}
	for i, spec := range fx.specs {
		if spec.Dir != filepath.Join(ws, "scratch") {
			t.Errorf("command %d ran in %s, want the scratch directory", i, spec.Dir)
		}
	}
}

// TestEvaluateHandoffRefused: a handoff that is not the controller's
// encoding, or an evaluator Job with no tool pod, is a fatal before any run.
func TestEvaluateHandoffRefused(t *testing.T) {
	tests := []struct {
		name, handoff, toolsURL, want string
	}{
		{"empty", "", "", "empty"},
		{"not json", "Outcome rate-battle: it rates.", "", "not valid"},
		{"unknown field", strings.Replace(evaluateHandoff, `{"round"`, `{"extra":1,"round"`, 1), "", "not valid"},
		{"trailing data", evaluateHandoff + "{}", "", "trailing data"},
		{"no round", evalHandoff(func(h *EvaluateHandoff) { h.Round = 0 }), "", "round 0"},
		{"no outcomes", evalHandoff(func(h *EvaluateHandoff) { h.Outcomes = nil }), "", "no outcomes"},
		{"invalid outcome", evalHandoff(func(h *EvaluateHandoff) { h.Outcomes[1].ID = "rate-battle" }), "",
			"used twice"},
		{"no tool calls", evalHandoff(func(h *EvaluateHandoff) { h.Tools.MaxToolCalls = 0 }), "", "max_tool_calls"},
		{"bad run token", evalHandoff(func(h *EvaluateHandoff) { h.RunToken = "Not A Token" }), "", "run token"},
		{"no tool pod", evaluateHandoff, "-", "MAUL_TOOLS_URL is not set"},
		{"tool pod not an origin", evaluateHandoff, "http://maul-tools:8090/v1", "not a bare origin"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			cfg, _ := roleConfig(t, PhaseEvaluate, tt.handoff, &out)
			switch tt.toolsURL {
			case "":
			case "-":
				cfg.ToolsURL = ""
			default:
				cfg.ToolsURL = tt.toolsURL
			}
			fx := &fakeExec{}
			err := New(cfg, fx).Run(context.Background())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Run() error = %v, want %q", err, tt.want)
			}
			if len(fx.specs) != 0 {
				t.Errorf("commands run = %d, want none", len(fx.specs))
			}
			if evs := events(t, out.String()); len(evs) != 1 || evs[0].Type != envelope.TypeFatal {
				t.Errorf("events = %+v, want one fatal", evs)
			}
		})
	}
}

// TestRoleStagesNeedNoClone: a role Job's workspace has no repository
// clone, and that is no fatal; a missing handoff is.
func TestRoleStagesNeedNoClone(t *testing.T) {
	for _, phase := range []Phase{PhaseEvaluate, PhaseCheckReview} {
		t.Run(string(phase), func(t *testing.T) {
			var out bytes.Buffer
			cfg, ws := roleConfig(t, phase, "", &out)
			if err := os.Remove(filepath.Join(ws, "input", "issue.md")); err != nil {
				t.Fatal(err)
			}
			fx := &fakeExec{}
			err := New(cfg, fx).Run(context.Background())
			if err == nil || !strings.Contains(err.Error(), "handoff missing") {
				t.Fatalf("Run() error = %v, want the missing handoff", err)
			}
			evs := events(t, out.String())
			if len(evs) != 1 || evs[0].Type != envelope.TypeFatal {
				t.Errorf("events = %+v, want one fatal", evs)
			}
			if len(fx.specs) != 0 {
				t.Errorf("commands run = %d, want none", len(fx.specs))
			}
		})
	}
}

// TestRoleStagesRefuseUnfitHarnesses: an evaluate or checkreview stage runs
// on brokered claude, brokered codex or the fake harness only, and never on
// one that cannot hold it to the evaluate posture.
func TestRoleStagesRefuseUnfitHarnesses(t *testing.T) {
	tests := []struct {
		name, harness, want string
	}{
		{"copilot", "copilot", "refused"},
		{"unbrokered claude", "claude", "must be brokered"},
	}
	for _, phase := range []Phase{PhaseEvaluate, PhaseCheckReview} {
		for _, tt := range tests {
			t.Run(string(phase)+"/"+tt.name, func(t *testing.T) {
				var out bytes.Buffer
				cfg, _ := roleConfig(t, phase, roleHandoff(phase), &out)
				cfg.InvestigateHarness = tt.harness
				fx := &fakeExec{}
				err := New(cfg, fx).Run(context.Background())
				if err == nil || !strings.Contains(err.Error(), tt.want) {
					t.Fatalf("Run() error = %v, want %q", err, tt.want)
				}
				if len(fx.specs) != 0 {
					t.Errorf("commands run = %d, want none", len(fx.specs))
				}
			})
		}
	}
}

// TestEvaluatePostureChecked: the evaluate posture is checked before a
// stage renders it, so a harness that cannot hold it is refused by name.
func TestEvaluatePostureChecked(t *testing.T) {
	h, _ := harness.ByID("copilot")
	if err := evaluatePosture(h); err == nil || !strings.Contains(err.Error(), "copilot") {
		t.Errorf("evaluatePosture(copilot) = %v, want a refusal naming it", err)
	}
	for _, id := range []string{"claude", "codex", "fake"} {
		h, _ := harness.ByID(id)
		if err := evaluatePosture(h); err != nil {
			t.Errorf("evaluatePosture(%s) = %v, want nil", id, err)
		}
	}
}

// TestDryEvaluateOnFakeHarness runs an evaluate stage end to end through the
// real runner on the fake harness against an httptest tool pod: the fixture
// is replayed through cat, the report the fixture's agent "wrote" is
// settled, its proposal matched to its file and confirmed, the tool pod
// finished, and the evaluation event carries the fixture's usage.
func TestDryEvaluateOnFakeHarness(t *testing.T) {
	var out bytes.Buffer
	cfg, ws := roleConfig(t, PhaseEvaluate, evaluateHandoff, &out)
	tools := useTools(t, &cfg)
	fixture := filepath.Join(t.TempDir(), "fixture.jsonl")
	if err := os.WriteFile(fixture, []byte(streamSuccess+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(harness.FakeFixtureEnv, fixture)
	for name, content := range map[string]string{
		"report.md":                   proposingEvaluation("rate-battle.api", "rate-battle", checks.Digest(rateBattleCheck)),
		"checks/rate-battle-api.json": string(checks.Canonical(rateBattleCheck)),
		"shots/12.png":                pngBytes,
	} {
		path := filepath.Join(ws, "scratch", name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := New(cfg, &runner.Exec{}).Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	evs := events(t, out.String())
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1:\n%s", len(evs), out.String())
	}
	ev := evs[0]
	e := ev.Evaluation
	if ev.Type != envelope.TypeEvaluation || e == nil || e.Outcome != envelope.OutcomeOK {
		t.Fatalf("event = %+v, want an ok evaluation", ev)
	}
	if e.Usage.InputTokens != 100 || e.Usage.OutputTokens != 30 || e.Usage.CostUSD == 0 || e.SessionID != testSessionID {
		t.Errorf("stage = %+v, want the fixture's usage and session", e.Stage)
	}
	if len(e.Verdicts) != 2 || len(e.Proposed) != 1 || !slices.Equal(e.Cited, []int32{12}) {
		t.Errorf("evaluation = %+v, want the report's claims and the one cited screenshot", e)
	}
	if got := tools.verbs(); !slices.Equal(got, []toolwire.Verb{toolwire.VerbConfirm, toolwire.VerbFinish}) {
		t.Errorf("tool calls = %v, want confirm then finish", got)
	}
}

// TestEvaluateHandoffCarriesThePreviousRound (S1-29, SPEC Q15): a round
// after a judged one decodes the previous round's results and the fix diff
// strictly, and hands every one of them to the prompt.
func TestEvaluateHandoffCarriesThePreviousRound(t *testing.T) {
	raw := evalHandoff(func(h *EvaluateHandoff) {
		h.Round, h.PriorRound, h.FixDiff = 2, 1, "+k := 32\n"
		h.PriorVerdicts = []PriorVerdict{{Outcome: "rate-battle", Result: "failed", Reason: "stays put"}}
		h.PriorReplay = []PriorReplay{{Check: "leaderboard", Outcome: "leaderboard", Result: "pass"}}
	})
	for _, key := range []string{`"prior_round":1`, `"prior_verdicts":[`, `"prior_replay":[`, `"fix_diff":`} {
		if !strings.Contains(raw, key) {
			t.Errorf("handoff %s lacks %s", raw, key)
		}
	}
	var h EvaluateHandoff
	if err := decodeHandoff([]byte(raw), "evaluate", &h); err != nil {
		t.Fatal(err)
	}
	if err := validEvaluateHandoff(h); err != nil {
		t.Fatal(err)
	}
	p := h.prompt("", "", "", templates.StageLimits{})
	if p.PriorRound != 1 || p.FixDiff != "+k := 32\n" ||
		len(p.PriorVerdicts) != 1 || p.PriorVerdicts[0] != (templates.PriorVerdict{Outcome: "rate-battle",
		Result: "failed", Reason: "stays put"}) ||
		len(p.PriorReplay) != 1 || p.PriorReplay[0] != (templates.PriorReplay{Check: "leaderboard",
		Outcome: "leaderboard", Result: "pass"}) {
		t.Errorf("prompt = %+v, want the previous round's results", p)
	}
}
