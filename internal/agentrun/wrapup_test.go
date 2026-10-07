// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"bytes"
	"context"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bitwise-media-group/patchy/internal/envelope"
	"github.com/bitwise-media-group/patchy/internal/runner"
	"github.com/bitwise-media-group/patchy/internal/templates"
	"github.com/bitwise-media-group/patchy/internal/transcript"
)

// failedBuild is a build report owning up to unfinished work.
const failedBuild = "---\nsuccess: false\nsummary: \"Half built\"\ntests:\n  ran: false\n  passed: false\n" +
	"reason: \"The handler is written; its test is not.\"\n---\nThe handler is in; the test is not.\n"

// The wrap-up tests run on brokered claude with a 30-minute stage (the
// repair tests' setup): a plan's grant of 25 turns runs its first run on 20
// and holds 5 back, a build's of 80 on 74 and 6; streamMaxTurns reports 20
// turns, so a plan's wrap-up gets 5 and a build's 6; and each round's wall
// clock is 15% of the stage's.
const wrapUpRoundWall = 4*time.Minute + 30*time.Second

// salvageRun drives one intent stage whose first run is first and whose
// later runs are after, and returns the executor and the single event's
// stage. cancel cancels the stage's context as the first run ends, as a
// SIGTERM would.
func salvageRun(t *testing.T, c repairCase, tune func(*Config), first step, cancel bool,
	after ...step) (*fakeExec, envelope.Event, *bytes.Buffer) {
	t.Helper()
	var out bytes.Buffer
	cfg, ws := c.setup(t, &out)
	if tune != nil {
		tune(&cfg)
	}
	first.ws = ws
	steps := make([]step, 0, 1+len(after))
	steps = append(steps, first)
	for _, s := range after {
		s.ws = ws
		steps = append(steps, s)
	}
	fx := &fakeExec{steps: steps}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	var e Executor = fx
	if cancel {
		e = cancelAfter{fx, stop}
	}
	a := New(cfg, e)
	a.newSessionID = func() string { return "00000000-0000-4000-8000-000000000001" }
	if err := a.Run(ctx); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	evs := events(t, out.String())
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1", len(evs))
	}
	if len(fx.steps) != 0 {
		t.Errorf("%d scripted runs never ran", len(fx.steps))
	}
	return fx, evs[0], &out
}

// wsOf is the workspace a stage ran in: the parent of the clone its first
// command ran in.
func wsOf(fx *fakeExec) string { return filepath.Dir(fx.specs[0].Dir) }

// checkWrapUpCommand checks a wrap-up continues the first run's session under
// its model, posture, directories and working directory, with the wrap-up
// prompt for its stage, the window's turns and its wall clock.
func checkWrapUpCommand(t *testing.T, c repairCase, fx *fakeExec, turns int, written bool) {
	t.Helper()
	first, wrap := fx.specs[0], fx.specs[1]
	if i := slices.Index(wrap.Argv, "--resume"); i < 0 || wrap.Argv[i+1] != testSessionID ||
		slices.Contains(wrap.Argv, "--session-id") {
		t.Errorf("wrap-up argv %q does not resume the session the first run reported", wrap.Argv)
	}
	if got, want := flagValues(wrap.Argv), flagValues(first.Argv); !maps.EqualFunc(got, want, slices.Equal) {
		t.Errorf("wrap-up flags = %v, want the first run's %v: a wrap-up never widens the posture", got, want)
	}
	if wrap.Dir != first.Dir {
		t.Errorf("wrap-up runs in %s, want the first run's %s", wrap.Dir, first.Dir)
	}
	if got := argvFlag(t, wrap.Argv, "--max-turns"); got != strconv.Itoa(turns) {
		t.Errorf("wrap-up --max-turns = %s, want %d", got, turns)
	}
	ws := wsOf(fx)
	commit := ""
	if c.writable {
		commit = filepath.Join(ws, "commit.sh")
	}
	want, err := templates.RenderWrapUpPrompt(templates.WrapUpPrompt{
		ReportPath: filepath.Join(ws, c.report), CommitScriptPath: commit,
		FirstTurns: argvInt(t, first.Argv, "--max-turns"), Turns: turns, ReportWritten: written,
	})
	if err != nil {
		t.Fatal(err)
	}
	if wrap.Argv[2] != want {
		t.Errorf("wrap-up prompt =\n%s\nwant\n%s", wrap.Argv[2], want)
	}
	if fx.timeouts[1] != wrapUpRoundWall {
		t.Errorf("wrap-up wall clock = %s, want the round's share %s", fx.timeouts[1], wrapUpRoundWall)
	}
	if c.phase == PhasePlan {
		checkPlanRepairScope(t, ws, wrap)
	}
}

// argvInt is the integer value after name in argv.
func argvInt(t *testing.T, argv []string, name string) int {
	t.Helper()
	n, err := strconv.Atoi(argvFlag(t, argv, name))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return n
}

// wrapUpOf is a stage's wrap-up record, or the zero record for none.
func wrapUpOf(st envelope.Stage) (envelope.WrapUp, bool) {
	if st.WrapUp == nil {
		return envelope.WrapUp{}, false
	}
	return *st.WrapUp, true
}

// TestMaxTurnsKeepsTheReport is the regression guard for a plan thrown away
// although its report was written: the run stopped at its turn limit after
// writing a valid plan, and the stage ended budget_exceeded with no plan
// for anyone to read. The plan on disk is now kept, for the approver, marked
// as written by a run that was cut off, without spending another turn.
func TestMaxTurnsKeepsTheReport(t *testing.T) {
	plan := repairCases()[2]
	fx, ev, _ := salvageRun(t, plan, nil, step{stdout: streamMaxTurns,
		writes: map[string]string{plan.report: goodPlan}}, false)
	p := ev.Plan
	if p == nil || p.Outcome != envelope.OutcomeOK || p.ReportMarkdown != goodPlan || len(fx.specs) != 1 {
		t.Fatalf("plan = %+v after %d commands, want the report on disk kept, ok, with no resume", p, len(fx.specs))
	}
	if w, ok := wrapUpOf(p.Stage); !ok || w != (envelope.WrapUp{Reason: envelope.WrapUpMaxTurns, Kept: true}) {
		t.Errorf("wrap-up = %+v, want max_turns, kept, not resumed", p.WrapUp)
	}
}

// TestPlanSalvage is the plan side of the salvage matrix. A plan goes to a
// human approver, who reads it marked as cut off, so a plan report on disk is
// settled whatever cut its run off: a run stopped at its turn limit is
// resumed once to write a missing report, and a report it left or wrote is
// repaired within the window; a run killed (wall clock, kill switch, broker)
// is never resumed, and a report it left is kept only if it parses. A crash
// or a cancelled stage salvages nothing.
func TestPlanSalvage(t *testing.T) {
	plan := repairCases()[2]
	huge := `{"type":"assistant","message":{"usage":{"output_tokens":5000000}}}`
	good := map[string]string{plan.report: goodPlan}
	bad := map[string]string{plan.report: badPlan}
	tests := []struct {
		name    string
		tune    func(*Config)
		first   step
		cancel  bool
		after   []step
		want    envelope.Outcome
		wrap    *envelope.WrapUp
		detail  string
		checkUp bool // the second command is the wrap-up, with 5 turns
	}{
		{name: "turn limit, no report: a wrap-up writes it", first: step{stdout: streamMaxTurns},
			after: []step{{stdout: streamResumed, writes: good}}, want: envelope.OutcomeOK,
			wrap: &envelope.WrapUp{Reason: envelope.WrapUpMaxTurns, Resumed: true, Kept: true}, checkUp: true},
		{name: "turn limit, a bad report on disk is repaired, not wrapped up",
			first: step{stdout: streamMaxTurns, writes: bad},
			after: []step{{stdout: streamResumed, writes: good}}, want: envelope.OutcomeOK,
			wrap: &envelope.WrapUp{Reason: envelope.WrapUpMaxTurns, Kept: true}},
		{name: "turn limit, the wrap-up's bad report is repaired", first: step{stdout: streamMaxTurns},
			after: []step{{stdout: streamResumed, writes: bad}, {stdout: streamResumed, writes: good}},
			want:  envelope.OutcomeOK, wrap: &envelope.WrapUp{Reason: envelope.WrapUpMaxTurns, Resumed: true, Kept: true},
			checkUp: true},
		{name: "turn limit, a wrap-up stopped again keeps the report it wrote", first: step{stdout: streamMaxTurns},
			after: []step{{stdout: streamMaxTurns, writes: good}}, want: envelope.OutcomeOK,
			wrap: &envelope.WrapUp{Reason: envelope.WrapUpMaxTurns, Resumed: true, Kept: true}, checkUp: true},
		{name: "turn limit, the session is gone", first: step{stdout: streamMaxTurns},
			after: []step{{stdout: streamNoSession, result: runner.Result{ExitCode: 1}}},
			want:  envelope.OutcomeBudgetExceeded, wrap: &envelope.WrapUp{Reason: envelope.WrapUpMaxTurns, Resumed: true},
			detail: "(error_max_turns): Reached maximum number of turns (20) (the wrap-up run ended runtime_error: " +
				"claude run error (error_during_execution): No conversation found"},
		{name: "turn limit on a grant too small to hold turns back",
			tune:  func(c *Config) { c.InvestigateMaxTurns = 10 },
			first: step{stdout: streamMaxTurns}, want: envelope.OutcomeBudgetExceeded,
			wrap:   &envelope.WrapUp{Reason: envelope.WrapUpMaxTurns},
			detail: "(no wrap-up: no turns left of the stage's 10)"},
		{name: "wall clock: a report on disk is kept, never resumed",
			first: step{stdout: streamCutOff, result: runner.Result{TimedOut: true, ExitCode: -1}, writes: good},
			want:  envelope.OutcomeOK, wrap: &envelope.WrapUp{Reason: envelope.WrapUpDeadline, Kept: true}},
		{name: "kill switch: a report on disk is kept",
			first: step{stdout: huge, budgetLines: []string{huge}, writes: good},
			want:  envelope.OutcomeOK, wrap: &envelope.WrapUp{Reason: envelope.WrapUpBudget, Kept: true}},
		{name: "broker limit: a report on disk is kept, never resumed",
			first: step{stdout: streamBrokerLimit, result: runner.Result{ExitCode: 1}, writes: good},
			want:  envelope.OutcomeOK, wrap: &envelope.WrapUp{Reason: envelope.WrapUpBudget, Kept: true}},
		{name: "wall clock: a bad report is neither kept nor repaired",
			first: step{stdout: streamCutOff, result: runner.Result{TimedOut: true, ExitCode: -1}, writes: bad},
			want:  envelope.OutcomeTimeout, wrap: &envelope.WrapUp{Reason: envelope.WrapUpDeadline},
			detail: "(the report it left was not kept: " + plan.detailOf(t, badPlan) + ")"},
		{name: "wall clock, no report",
			first: step{stdout: streamCutOff, result: runner.Result{TimedOut: true, ExitCode: -1}},
			want:  envelope.OutcomeTimeout, wrap: &envelope.WrapUp{Reason: envelope.WrapUpDeadline}},
		{name: "a crash salvages nothing", first: step{stdout: streamExecError, writes: good},
			want: envelope.OutcomeRuntimeError},
		{name: "cancelled at the turn limit: no wrap-up", first: step{stdout: streamMaxTurns}, cancel: true,
			want: envelope.OutcomeBudgetExceeded},
		{name: "cancelled with a report on disk: nothing salvaged", first: step{stdout: streamMaxTurns, writes: good},
			cancel: true, want: envelope.OutcomeBudgetExceeded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx, ev, out := salvageRun(t, plan, tt.tune, tt.first, tt.cancel, tt.after...)
			p := ev.Plan
			if p == nil || p.Outcome != tt.want {
				t.Fatalf("plan = %+v, want outcome %q", p, tt.want)
			}
			if !strings.Contains(p.Detail, tt.detail) {
				t.Errorf("detail = %q, want it to hold %q", p.Detail, tt.detail)
			}
			if (p.WrapUp == nil) != (tt.wrap == nil) || (tt.wrap != nil && *p.WrapUp != *tt.wrap) {
				t.Errorf("wrap-up = %+v, want %+v", p.WrapUp, tt.wrap)
			}
			if kept := p.ReportMarkdown == goodPlan; kept != (tt.want == envelope.OutcomeOK) ||
				(!kept && p.ReportMarkdown != "") {
				t.Errorf("report = %q, want the plan exactly when the stage ends ok", p.ReportMarkdown)
			}
			if len(fx.specs) != 1+len(tt.after) {
				t.Errorf("commands = %d, want %d", len(fx.specs), 1+len(tt.after))
			}
			if tt.checkUp {
				checkWrapUpCommand(t, plan, fx, 5, false)
				checkWrapUpTranscript(t, out.String())
			}
		})
	}
}

// TestWrapUpAccounting: a wrap-up's spend is the stage's — tokens, turns
// and time summed, its cumulative cost never counted twice — and the
// stage's runs share one transcript with the wrap-up announced in it.
func TestWrapUpAccounting(t *testing.T) {
	for _, c := range repairCases()[2:] {
		t.Run(string(c.phase), func(t *testing.T) {
			first := step{stdout: streamMaxTurns, writes: maps.Clone(c.outputs), repoWrite: c.repo}
			if first.writes == nil {
				first.writes = map[string]string{}
			}
			_, ev, out := salvageRun(t, c, nil, first, false, step{stdout: streamResumed,
				writes: map[string]string{c.report: c.good}})
			stage := eventStage(t, ev)
			if stage.Outcome != envelope.OutcomeOK {
				t.Fatalf("outcome = %q (%s), want ok", stage.Outcome, stage.Detail)
			}
			if !sameTokens(stage.Usage, plus(maxTurnsUsage, resumedUsage)) || stage.NumTurns != 22 ||
				stage.ElapsedSeconds != 6 || stage.SessionID != testSessionID {
				t.Errorf("accounting = %+v, want both runs summed in the one session", stage)
			}
			if want := 0.05 + priced(t, resumedUsage); stage.Usage.CostUSD < want-1e-9 || stage.Usage.CostUSD > want+1e-9 {
				t.Errorf("cost = %v, want the first run's 0.05 plus the wrap-up priced (%v)", stage.Usage.CostUSD, want)
			}
			checkWrapUpTranscript(t, out.String())
		})
	}
}

// checkWrapUpTranscript checks the stage's runs share one transcript, with
// the wrap-up announced in it.
func checkWrapUpTranscript(t *testing.T, out string) {
	t.Helper()
	got := turns(t, out)
	checkSeqs(t, got)
	if !slices.ContainsFunc(got, func(turn transcript.Turn) bool {
		return turn.Kind == transcript.KindNotice && strings.Contains(turn.Text, "asking the agent to wrap up")
	}) {
		t.Errorf("transcript has no wrap-up notice: %+v", got)
	}
}

// TestBuildSalvage is the build side of the salvage matrix. A build's
// report may have been written early, claiming success for work its run
// then went on changing, so it is never taken as it stands: a run stopped
// at its turn limit always gets a wrap-up turn to make the report match the
// tree — which the turn may not change, and whose commits must be a subset
// of it — and any other cut-off run's report is recorded but never
// packaged, so it can never yield success or a push.
func TestBuildSalvage(t *testing.T) {
	build := repairCases()[3]
	staleSuccess := map[string]string{build.report: goodBuild, "commit.sh": buildCommitScript}
	work := map[string]string{"app.js": "version();\n"}
	huge := `{"type":"assistant","message":{"usage":{"output_tokens":5000000}}}`
	cutOffFirst := func(res runner.Result, stdout string) step {
		return step{stdout: stdout, result: res, writes: maps.Clone(staleSuccess), repoWrite: work}
	}
	tests := []struct {
		name     string
		tune     func(*Config)
		first    step
		cancel   bool
		after    []step
		want     envelope.Outcome
		success  bool
		wrap     *envelope.WrapUp
		detail   string
		recorded string // the report the event carries
		checkUp  int    // 1: the wrap-up checks a written report; 2: writes one
	}{
		{name: "a stale success report takes a wrap-up turn, which owns up",
			first: step{stdout: streamMaxTurns, writes: maps.Clone(staleSuccess), repoWrite: work},
			after: []step{{stdout: streamResumed, writes: map[string]string{build.report: failedBuild}}},
			want:  envelope.OutcomeOK, recorded: failedBuild, checkUp: 1,
			wrap: &envelope.WrapUp{Reason: envelope.WrapUpMaxTurns, Resumed: true, Kept: true}},
		{name: "a wrap-up confirms the build, which is packaged",
			first: step{stdout: streamMaxTurns, writes: maps.Clone(staleSuccess), repoWrite: work},
			after: []step{{stdout: streamResumed, writes: map[string]string{build.report: goodBuild}}},
			want:  envelope.OutcomeOK, success: true, recorded: goodBuild, checkUp: 1,
			wrap: &envelope.WrapUp{Reason: envelope.WrapUpMaxTurns, Resumed: true, Kept: true}},
		{name: "no report: the wrap-up writes it and commit.sh",
			first: step{stdout: streamMaxTurns, repoWrite: work},
			after: []step{{stdout: streamResumed, writes: maps.Clone(staleSuccess)}},
			want:  envelope.OutcomeOK, success: true, recorded: goodBuild, checkUp: 2,
			wrap: &envelope.WrapUp{Reason: envelope.WrapUpMaxTurns, Resumed: true, Kept: true}},
		{name: "the wrap-up's bad report is repaired, keeping the tree",
			first: step{stdout: streamMaxTurns, writes: maps.Clone(staleSuccess), repoWrite: work},
			after: []step{{stdout: streamResumed, writes: map[string]string{build.report: longNoteBuild}},
				{stdout: streamResumed, writes: map[string]string{build.report: goodBuild}}},
			want: envelope.OutcomeOK, success: true, recorded: goodBuild, checkUp: 1,
			wrap: &envelope.WrapUp{Reason: envelope.WrapUpMaxTurns, Resumed: true, Kept: true}},
		{name: "a wrap-up that edits code is refused whole",
			first: step{stdout: streamMaxTurns, writes: maps.Clone(staleSuccess), repoWrite: work},
			after: []step{{stdout: streamResumed, writes: map[string]string{build.report: goodBuild},
				repoWrite: map[string]string{"app.js": "finished();\n"}}},
			want: envelope.OutcomeBudgetExceeded,
			wrap: &envelope.WrapUp{Reason: envelope.WrapUpMaxTurns, Resumed: true},
			detail: "(wrap-up refused: it changed the working tree: app.js, and a wrap-up may change nothing but " +
				"the report and commit.sh)"},
		{name: "a wrap-up's commit.sh that commits a file of its own is refused",
			first: step{stdout: streamMaxTurns, writes: maps.Clone(staleSuccess), repoWrite: work},
			after: []step{{stdout: streamResumed, writes: map[string]string{build.report: goodBuild,
				"commit.sh": "#!/bin/sh\nset -e\necho 'more();' > gen.js\ngit add app.js gen.js\ngit commit -m x\n"}}},
			want: envelope.OutcomeCommitFailed, recorded: goodBuild,
			wrap:   &envelope.WrapUp{Reason: envelope.WrapUpMaxTurns, Resumed: true, Kept: true},
			detail: "commit.sh committed gen.js other than the working tree held when the wrap-up began"},
		{name: "the session is gone: the stale report is recorded, never pushed",
			first: step{stdout: streamMaxTurns, writes: maps.Clone(staleSuccess), repoWrite: work},
			after: []step{{stdout: streamNoSession, result: runner.Result{ExitCode: 1}}},
			want:  envelope.OutcomeBudgetExceeded, recorded: goodBuild,
			wrap:   &envelope.WrapUp{Reason: envelope.WrapUpMaxTurns, Resumed: true},
			detail: "the wrap-up run ended runtime_error: claude run error (error_during_execution)"},
		{name: "a grant too small to hold turns back: recorded, never pushed",
			tune:  func(c *Config) { c.GrantedMaxTurns = 10 },
			first: step{stdout: streamMaxTurns, writes: maps.Clone(staleSuccess), repoWrite: work},
			want:  envelope.OutcomeBudgetExceeded, recorded: goodBuild,
			wrap:   &envelope.WrapUp{Reason: envelope.WrapUpMaxTurns},
			detail: "(no wrap-up: no turns left of the stage's 10; " + unconfirmedBuild + ")"},
		{name: "wall clock: recorded, never resumed or pushed",
			first: cutOffFirst(runner.Result{TimedOut: true, ExitCode: -1}, streamCutOff),
			want:  envelope.OutcomeTimeout, recorded: goodBuild, wrap: &envelope.WrapUp{Reason: envelope.WrapUpDeadline},
			detail: "(" + unconfirmedBuild + ")"},
		{name: "kill switch: recorded, never pushed",
			first: step{stdout: huge, budgetLines: []string{huge}, writes: maps.Clone(staleSuccess), repoWrite: work},
			want:  envelope.OutcomeBudgetExceeded, recorded: goodBuild, wrap: &envelope.WrapUp{Reason: envelope.WrapUpBudget}},
		{name: "broker limit: recorded, never resumed or pushed",
			first: cutOffFirst(runner.Result{ExitCode: 1}, streamBrokerLimit),
			want:  envelope.OutcomeBudgetExceeded, recorded: goodBuild, wrap: &envelope.WrapUp{Reason: envelope.WrapUpBudget}},
		{name: "a crash: recorded, never pushed", first: cutOffFirst(runner.Result{ExitCode: 1}, streamExecError),
			want: envelope.OutcomeRuntimeError, recorded: goodBuild},
		{name: "cancelled at the turn limit: no wrap-up, recorded, never pushed",
			first: step{stdout: streamMaxTurns, writes: maps.Clone(staleSuccess), repoWrite: work}, cancel: true,
			want: envelope.OutcomeBudgetExceeded, recorded: goodBuild},
		{name: "wall clock, no report",
			first: step{stdout: streamCutOff, result: runner.Result{TimedOut: true, ExitCode: -1}, repoWrite: work},
			want:  envelope.OutcomeTimeout, wrap: &envelope.WrapUp{Reason: envelope.WrapUpDeadline}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx, ev, out := salvageRun(t, build, tt.tune, tt.first, tt.cancel, tt.after...)
			rem := ev.Remediation
			if rem == nil || rem.Outcome != tt.want || rem.Success != tt.success {
				t.Fatalf("build = %+v, want outcome %q, success %v", rem, tt.want, tt.success)
			}
			if !strings.Contains(rem.Detail, tt.detail) {
				t.Errorf("detail = %q, want it to hold %q", rem.Detail, tt.detail)
			}
			if (rem.WrapUp == nil) != (tt.wrap == nil) || (tt.wrap != nil && *rem.WrapUp != *tt.wrap) {
				t.Errorf("wrap-up = %+v, want %+v", rem.WrapUp, tt.wrap)
			}
			if rem.ReportMarkdown != tt.recorded {
				t.Errorf("report = %q, want %q", rem.ReportMarkdown, tt.recorded)
			}
			if rem.Success != (rem.Changeset != nil) {
				t.Errorf("success %v with changeset %v: only a confirmed success is packaged", rem.Success,
					rem.Changeset != nil)
			}
			if len(fx.specs) != 1+len(tt.after) {
				t.Errorf("commands = %d, want %d", len(fx.specs), 1+len(tt.after))
			}
			if tt.checkUp > 0 {
				checkWrapUpCommand(t, build, fx, 6, tt.checkUp == 1)
				checkWrapUpTranscript(t, out.String())
			}
		})
	}
}

// TestIdleSalvage: a run the idle watchdog ended is never resumed — it was
// killed mid-stream — but a plan it left on disk is kept, and a build's
// report is recorded, never pushed.
func TestIdleSalvage(t *testing.T) {
	for _, tt := range []struct {
		phase    Phase
		files    map[string]string
		want     envelope.Outcome
		wrap     envelope.WrapUp
		recorded string
	}{
		{PhasePlan, map[string]string{"reports/plan.md": goodPlan}, envelope.OutcomeOK,
			envelope.WrapUp{Reason: envelope.WrapUpIdle, Kept: true}, goodPlan},
		{PhaseBuild, map[string]string{"reports/build.md": goodBuild, "commit.sh": buildCommitScript},
			envelope.OutcomeTimeout, envelope.WrapUp{Reason: envelope.WrapUpIdle}, goodBuild},
	} {
		t.Run(string(tt.phase), func(t *testing.T) {
			var out bytes.Buffer
			cfg, ws := intentConfig(t, tt.phase, &out)
			cfg = watched(cfg, testIdle)
			layDown(t, ws, tt.files)
			fx := &streamExec{ws: ws, tick: testTick, lines: []string{
				`{"type":"system","subtype":"init","session_id":"` + testSessionID + `"}`, lineHungCall,
			}}
			stage := stageOf(t, runOne(t, cfg, fx, &out))
			if stage.Outcome != tt.want || stage.WrapUp == nil || *stage.WrapUp != tt.wrap {
				t.Fatalf("stage = %+v (wrap-up %+v), want %q, %+v", stage, stage.WrapUp, tt.want, tt.wrap)
			}
			ev := events(t, out.String())[0]
			got := ""
			switch {
			case ev.Plan != nil:
				got = ev.Plan.ReportMarkdown
			case ev.Remediation != nil:
				got = ev.Remediation.ReportMarkdown
				if ev.Remediation.Success || ev.Remediation.Changeset != nil {
					t.Error("an idle build was packaged")
				}
			}
			if got != tt.recorded {
				t.Errorf("report = %q, want %q", got, tt.recorded)
			}
		})
	}
}
