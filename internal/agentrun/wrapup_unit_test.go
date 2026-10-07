// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/quick"
	"time"

	"github.com/bitwise-media-group/patchy/internal/envelope"
	"github.com/bitwise-media-group/patchy/internal/harness"
	"github.com/bitwise-media-group/patchy/internal/runner"
)

func TestSplitGrant(t *testing.T) {
	tests := []struct {
		turns          int
		timeout        time.Duration
		first, reserve int
		wall           time.Duration
		why            string
	}{
		{0, 20 * time.Minute, 0, 0, 3 * time.Minute, "no turn cap holds nothing back"},
		{10, 20 * time.Minute, 10, 0, 3 * time.Minute, "a grant below 15 holds nothing back"},
		{14, 20 * time.Minute, 14, 0, 3 * time.Minute, "nor does 14"},
		{15, 20 * time.Minute, 12, 3, 3 * time.Minute, "15 holds back the floor of 3"},
		{25, 20 * time.Minute, 20, 5, 3 * time.Minute, "a fifth between the bounds"},
		{40, 20 * time.Minute, 34, 6, 3 * time.Minute, "the plan default: at most 6"},
		{150, 60 * time.Minute, 144, 6, 9 * time.Minute, "the build default: 15% of an hour"},
		{80, 45 * time.Minute, 74, 6, 6*time.Minute + 45*time.Second, "revise: 15% of 45 minutes"},
		{240, 2 * time.Hour, 234, 6, 10 * time.Minute, "the wall share is capped at 10 minutes"},
		{40, time.Minute, 34, 6, time.Minute, "never more wall clock than the stage's own"},
		{40, 0, 34, 6, 0, "no wall clock, no share of it"},
	}
	for _, tt := range tests {
		t.Run(tt.why, func(t *testing.T) {
			got := splitGrant(tt.turns, tt.timeout)
			want := grantSplit{grant: tt.turns, first: tt.first, reserve: tt.reserve, wall: tt.wall}
			if got != want {
				t.Errorf("splitGrant(%d, %s) = %+v, want %+v", tt.turns, tt.timeout, got, want)
			}
		})
	}
}

// TestSplitGrantProperty: for any grant and wall clock, the first run's
// turns and the reserve add up to the grant exactly, so a wrap-up never
// spends past it; the reserve is none below wrapUpMinGrant and within
// [wrapUpMinTurns, wrapUpMaxTurns] from it, never growing smaller as the
// grant grows; a first run always has a turn when the grant does; and the
// per-round wall clock is within its bounds and the stage's own.
func TestSplitGrantProperty(t *testing.T) {
	cfg := &quick.Config{Rand: rand.New(rand.NewSource(20261007)), MaxCount: 2000}
	prop := func(turns uint16, seconds uint32) bool {
		n, timeout := int(turns), time.Duration(seconds%(4*3600))*time.Second
		s := splitGrant(n, timeout)
		next := splitGrant(n+1, timeout)
		reserveOK := (n < wrapUpMinGrant && s.reserve == 0) ||
			(n >= wrapUpMinGrant && s.reserve >= wrapUpMinTurns && s.reserve <= wrapUpMaxTurns)
		wallOK := s.wall <= timeout && s.wall >= 0 &&
			(timeout < wrapUpMinWall || (s.wall >= wrapUpMinWall && s.wall <= wrapUpMaxWall))
		return s.grant == n && s.first+s.reserve == n && reserveOK && (n < 1 || s.first >= 1) &&
			next.reserve >= s.reserve && wallOK
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}

// legacyRepairLimits is the repair window a Finding stage had before the
// intent window existed, copied verbatim from repairRound and repairWindow
// as they were, so the pin below compares against the old code itself.
func legacyRepairLimits(ctx context.Context, sr *stageRun, st *envelope.Stage, elapsed time.Duration) (roundLimits,
	string) {
	if ctx.Err() != nil {
		return roundLimits{}, "the stage was cancelled"
	}
	if left := sr.timeout - elapsed; left < repairMinWindow {
		return roundLimits{}, fmt.Sprintf("less than %s of the stage's %s wall clock left",
			shortDuration(repairMinWindow), shortDuration(sr.timeout))
	}
	if sr.req.MaxTurns > 0 && sr.req.MaxTurns-st.NumTurns < 1 {
		return roundLimits{}, fmt.Sprintf("no turns left of the stage's %d", sr.req.MaxTurns)
	}
	if sr.budget > 0 && sr.budget-st.Usage.OutputTokens < 1 {
		return roundLimits{}, fmt.Sprintf("no output tokens left of the stage's %d", sr.budget)
	}
	maxTurns := repairMaxTurns
	if sr.req.MaxTurns > 0 {
		maxTurns = min(repairMaxTurns, sr.req.MaxTurns-st.NumTurns)
	}
	budget := 0
	if sr.budget > 0 {
		budget = sr.budget - st.Usage.OutputTokens
	}
	return roundLimits{maxTurns: maxTurns, budget: budget, timeout: min(repairTimeout, sr.timeout-elapsed)}, ""
}

// TestFindingRepairWindowUnchanged pins a Finding stage's repair window —
// the turns, output tokens and wall clock a repair round of an investigation
// or a remediation gets, and when none may start — to exactly what it was
// before intents gained their own window, beside
// TestFindingStageInvocationsUnchanged: by example, and for any stage
// against the old code.
func TestFindingRepairWindowUnchanged(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name     string
		ctx      context.Context
		maxTurns int
		used     int
		budget   int
		spent    int
		elapsed  time.Duration
		want     roundLimits
		why      string
	}{
		{"a fresh stage", context.Background(), 25, 7, 150000, 30, time.Minute,
			roundLimits{maxTurns: 6, budget: 149970, timeout: 10 * time.Minute}, ""},
		{"few turns left", context.Background(), 10, 7, 0, 0, time.Minute,
			roundLimits{maxTurns: 3, timeout: 10 * time.Minute}, ""},
		{"no cap", context.Background(), 0, 7, 0, 0, 25 * time.Minute,
			roundLimits{maxTurns: 6, timeout: 5 * time.Minute}, ""},
		{"cancelled", cancelled, 25, 7, 0, 0, 0, roundLimits{}, "the stage was cancelled"},
		{"no wall clock", context.Background(), 25, 7, 0, 0, 29 * time.Minute, roundLimits{},
			"less than 2m of the stage's 30m wall clock left"},
		{"no turns", context.Background(), 7, 7, 0, 0, 0, roundLimits{}, "no turns left of the stage's 7"},
		{"no tokens", context.Background(), 25, 7, 30, 30, 0, roundLimits{},
			"no output tokens left of the stage's 30"},
	}
	a := New(Config{}, &fakeExec{})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sr := &stageRun{req: harness.PromptRequest{MaxTurns: tt.maxTurns}, timeout: 30 * time.Minute,
				budget: tt.budget}
			st := &envelope.Stage{NumTurns: tt.used, Usage: envelope.Usage{OutputTokens: tt.spent}}
			got, why := a.roundWindow(tt.ctx, sr, st, tt.elapsed)
			if got != tt.want || why != tt.why {
				t.Errorf("roundWindow = %+v, %q; want %+v, %q", got, why, tt.want, tt.why)
			}
		})
	}

	prop := func(maxTurns, used uint8, budget, spent uint32, timeoutS, elapsedS uint16) bool {
		sr := &stageRun{req: harness.PromptRequest{MaxTurns: int(maxTurns)},
			timeout: time.Duration(timeoutS) * time.Second, budget: int(budget % 1000000)}
		st := &envelope.Stage{NumTurns: int(used), Usage: envelope.Usage{OutputTokens: int(spent % 1000000)}}
		elapsed := time.Duration(elapsedS) * time.Second
		got, why := a.roundWindow(context.Background(), sr, st, elapsed)
		want, wantWhy := legacyRepairLimits(context.Background(), sr, st, elapsed)
		return got == want && why == wantWhy
	}
	if err := quick.Check(prop, &quick.Config{Rand: rand.New(rand.NewSource(20261008)), MaxCount: 2000}); err != nil {
		t.Error(err)
	}
}

// TestIntentWindow: every round after an intent stage's first run — a
// wrap-up or a repair — gets what the whole grant has left, not what its
// first run's share has left, at most repairMaxTurns; the output tokens the
// result events left of the budget; and the stage's wall clock left, at
// most the split's per-round share. None starts when the stage is
// cancelled, or has too little of any of them left.
func TestIntentWindow(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name    string
		ctx     context.Context
		grant   int
		used    int
		budget  int
		spent   int
		elapsed time.Duration
		want    roundLimits
		why     string
	}{
		{"stopped at the first run's share: the reserve", context.Background(), 40, 34, 200000, 150000, time.Minute,
			roundLimits{maxTurns: 6, budget: 50000, timeout: 3 * time.Minute}, ""},
		{"a run that ended early keeps at most repairMaxTurns", context.Background(), 40, 7, 0, 0, time.Minute,
			roundLimits{maxTurns: 6, timeout: 3 * time.Minute}, ""},
		{"a result reporting a turn past the cap leaves less", context.Background(), 40, 35, 0, 0, time.Minute,
			roundLimits{maxTurns: 5, timeout: 3 * time.Minute}, ""},
		{"less wall clock left than the share", context.Background(), 40, 34, 0, 0, 17*time.Minute + 30*time.Second,
			roundLimits{maxTurns: 6, timeout: 2*time.Minute + 30*time.Second}, ""},
		{"no cap", context.Background(), 0, 50, 0, 0, time.Minute, roundLimits{maxTurns: 6, timeout: 3 * time.Minute},
			""},
		{"cancelled", cancelled, 40, 34, 0, 0, 0, roundLimits{}, "the stage was cancelled"},
		{"no wall clock", context.Background(), 40, 34, 0, 0, 19 * time.Minute, roundLimits{},
			"less than 2m of the stage's 20m wall clock left"},
		{"the whole grant spent", context.Background(), 40, 40, 0, 0, 0, roundLimits{},
			"no turns left of the stage's 40"},
		{"the budget spent, by the result events", context.Background(), 40, 34, 200000, 200000, 0, roundLimits{},
			"no output tokens left of the stage's 200000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			split := splitGrant(tt.grant, 20*time.Minute)
			sr := &stageRun{req: harness.PromptRequest{MaxTurns: split.first}, timeout: 20 * time.Minute,
				budget: tt.budget, split: &split}
			st := &envelope.Stage{NumTurns: tt.used, Usage: envelope.Usage{OutputTokens: tt.spent}}
			got, why := New(Config{}, &fakeExec{}).roundWindow(tt.ctx, sr, st, tt.elapsed)
			if got != tt.want || why != tt.why {
				t.Errorf("roundWindow = %+v, %q; want %+v, %q", got, why, tt.want, tt.why)
			}
		})
	}
}

// streamMaxTurns is a claude run stopped at its turn limit: a clean turn
// boundary, with a result event reporting the session, its turns and usage.
const streamMaxTurns = `{"type":"system","subtype":"init","session_id":"` + testSessionID + `"}` + "\n" +
	`{"type":"assistant","message":{"id":"msg_m1","usage":{"output_tokens":12},` +
	`"content":[{"type":"text","text":"Still reading."}]}}` + "\n" +
	`{"type":"result","subtype":"error_max_turns","is_error":true,"result":"",` +
	`"session_id":"` + testSessionID + `","num_turns":20,"total_cost_usd":0.05,` +
	`"usage":{"input_tokens":200,"cache_creation_input_tokens":40,"cache_read_input_tokens":100,"output_tokens":60},` +
	`"errors":["Reached maximum number of turns (20)"]}`

// maxTurnsUsage is streamMaxTurns' usage.
var maxTurnsUsage = envelope.Usage{InputTokens: 200, CacheCreationTokens: 40, CacheReadTokens: 100, OutputTokens: 60}

func TestClassifyCutOff(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	bg := context.Background()
	tests := []struct {
		name   string
		ctx    context.Context
		res    runner.Result
		idle   string
		runErr error
		want   cutOff
	}{
		{"ok", bg, runner.Result{Stdout: []byte(streamSuccess)}, "", nil, cutNone},
		{"turn limit", bg, runner.Result{Stdout: []byte(streamMaxTurns), ExitCode: 1}, "", nil, cutMaxTurns},
		{"kill switch", bg, runner.Result{Aborted: true, Stdout: []byte(streamCutOff)}, "", nil, cutKillSwitch},
		{"wall clock", bg, runner.Result{TimedOut: true, Stdout: []byte(streamCutOff)}, "", nil, cutDeadline},
		{"idle", bg, runner.Result{Stdout: []byte(streamCutOff)}, "no progress for 10m", context.Canceled, cutIdle},
		{"broker limit", bg, runner.Result{Stdout: []byte(streamBrokerLimit), ExitCode: 1}, "", nil, cutBrokerLimit},
		{"crash", bg, runner.Result{Stdout: []byte(streamExecError), ExitCode: 1}, "", nil, cutOther},
		{"run error", bg, runner.Result{}, "", errors.New("exec: not found"), cutOther},
		// A SIGTERM ends the stage: nothing may start after it, whatever the
		// run ended on.
		{"cancelled at the turn limit", cancelled, runner.Result{Stdout: []byte(streamMaxTurns)}, "", nil, cutOther},
		{"cancelled while idle", cancelled, runner.Result{}, "no progress", context.Canceled, cutOther},
	}
	h := harness.NewClaude()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyCutOff(tt.ctx, h, tt.res, tt.idle, tt.runErr); got != tt.want {
				t.Errorf("classifyCutOff = %d, want %d", got, tt.want)
			}
		})
	}
	for cut, want := range map[cutOff]envelope.WrapUpReason{
		cutNone: "", cutMaxTurns: envelope.WrapUpMaxTurns, cutKillSwitch: envelope.WrapUpBudget,
		cutBrokerLimit: envelope.WrapUpBudget, cutDeadline: envelope.WrapUpDeadline, cutIdle: envelope.WrapUpIdle,
		cutOther: "",
	} {
		if got := cut.reason(); got != want {
			t.Errorf("cutOff(%d).reason() = %q, want %q", cut, got, want)
		}
	}
}

// TestKillSwitchNearInertOnClaude21 pins why no output tokens are held back
// for a wrap-up: CLI 2.1 stamps each message's usage with its output count
// at the message's START, so the kill switch, which sums those, sees a few
// tokens a message. A run that wrote thousands trips a budget of ten only
// past ten messages' worth of starts. Nothing may rely on it to bound a
// round; the turn cap and the result events' totals do.
func TestKillSwitchNearInertOnClaude21(t *testing.T) {
	watch := budgetWatcher(harness.NewClaude(), 10)
	for line := range strings.SplitSeq(streamCutOff, "\n") {
		if abort, reason := watch([]byte(line)); abort {
			t.Fatalf("the kill switch tripped on a 2.1-shaped stream: %s", reason)
		}
	}
	if u := harness.NewClaude().StreamUsage([]byte(streamCutOff)); u == nil || *u.OutputTokens != 4 {
		t.Errorf("stream output tokens = %v, want the floor of 4 the starts report", u)
	}
}

// TestWrapUpNeedsAResumer: a harness that cannot continue a session gets no
// wrap-up, and none is recorded as resumed.
func TestWrapUpNeedsAResumer(t *testing.T) {
	split := splitGrant(40, 20*time.Minute)
	sr := &stageRun{h: harness.NewCodex(), req: harness.PromptRequest{MaxTurns: split.first},
		timeout: 20 * time.Minute, split: &split, started: time.Now()}
	st := &envelope.Stage{NumTurns: 34, WrapUp: &envelope.WrapUp{Reason: envelope.WrapUpMaxTurns}}
	fx := &fakeExec{}
	r := New(Config{}, fx).wrapUpRound(context.Background(), sr, st, "/workspace/reports/plan.md", false)
	if r.ran || r.why != "the codex harness cannot resume a session" || st.WrapUp.Resumed || len(fx.specs) != 0 {
		t.Errorf("wrap-up on codex = %+v, resumed %v, %d commands; want none", r, st.WrapUp.Resumed, len(fx.specs))
	}
}

// TestIntentRecordsCutOffRuns: an intent run cut off before its result event
// still records the session patchy assigned it and the turns its stream
// shows, so the controller can say how far it got; a Finding stage records
// exactly what it always has.
func TestIntentRecordsCutOffRuns(t *testing.T) {
	const session = "00000000-0000-4000-8000-0000000000aa"
	for _, st := range everyStage {
		t.Run(string(st.phase), func(t *testing.T) {
			var out bytes.Buffer
			cfg, ws := st.config(t, &out)
			fx := &fakeExec{steps: []step{{ws: ws, stdout: streamCutOff,
				result: runner.Result{TimedOut: true, ExitCode: -1}}}}
			a := New(cfg, fx)
			a.newSessionID = func() string { return session }
			if err := a.Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			stage := eventStage(t, events(t, out.String())[0])
			intent := st.phase == PhasePlan || st.phase == PhaseBuild
			wantSession, wantTurns := "", 0
			if intent {
				wantSession, wantTurns = session, 2
			}
			if stage.SessionID != wantSession || stage.NumTurns != wantTurns {
				t.Errorf("session %q, turns %d; want %q, %d", stage.SessionID, stage.NumTurns, wantSession, wantTurns)
			}
			if intent != (stage.WrapUp != nil) {
				t.Errorf("wrap-up = %+v, want one exactly on an intent stage", stage.WrapUp)
			}
		})
	}
}

// TestOutsideBaselineProperty: commits that only write paths with exactly
// the mode and content the baseline holds, and only delete paths it does not
// hold, are a subset of it; change any one of them — a different blob, a
// different mode, a path the baseline lacks, a deletion of one it holds —
// and that path, and only it, is named.
func TestOutsideBaselineProperty(t *testing.T) {
	prop := func(seed int64, n, picks uint8) bool {
		r := rand.New(rand.NewSource(seed))
		baseline := map[string]treeEntry{}
		var paths []string
		for i := range int(n%40) + 1 {
			p := fmt.Sprintf("dir%d/file%d.go", r.Intn(4), i)
			baseline[p] = treeEntry{mode: []string{"100644", "100755"}[r.Intn(2)], object: fmt.Sprintf("%040x", r.Int63())}
			paths = append(paths, p)
		}
		slices.Sort(paths)
		paths = slices.Compact(paths)
		changes := make([]treeChange, 0, int(picks%10)+1)
		for range int(picks % 10) {
			p := paths[r.Intn(len(paths))]
			changes = append(changes, treeChange{path: p, entry: baseline[p]})
		}
		changes = append(changes, treeChange{path: "gone/old.go", deleted: true})
		if len(outsideBaseline(changes, baseline)) != 0 {
			return false
		}
		victim := paths[r.Intn(len(paths))]
		bad := map[int]treeChange{
			0: {path: victim, entry: treeEntry{mode: baseline[victim].mode, object: strings.Repeat("f", 40)}},
			1: {path: victim, entry: treeEntry{mode: "120000", object: baseline[victim].object}},
			2: {path: "new/generated.go", entry: treeEntry{mode: "100644", object: strings.Repeat("e", 40)}},
			3: {path: victim, deleted: true},
		}[r.Intn(4)]
		got := outsideBaseline(append(slices.Clone(changes), bad), baseline)
		return slices.Equal(got, []string{bad.path})
	}
	if err := quick.Check(prop, &quick.Config{Rand: rand.New(rand.NewSource(20261009)), MaxCount: 500}); err != nil {
		t.Error(err)
	}
}

// TestCommittedSubset checks the guard against a real clone: commit.sh may
// commit part of what the tree held when the wrap-up began, leaving a
// scratch file out, but a file it creates and force-adds itself — ignored,
// so not in the baseline — is refused, as is a file it rewrites or one it
// deletes that the tree still held.
func TestCommittedSubset(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		name   string
		script string
		want   string
	}{
		{"a subset: the change, without the scratch file", "git add app.js\ngit commit -qm change\n", ""},
		{"a force-added ignored file of its own", "git add app.js\nmkdir -p build\necho x > build/gen.js\n" +
			"git add -f build/gen.js\ngit commit -qm change\n", "commit.sh committed build/gen.js other than"},
		{"a file rewritten", "echo other > app.js\ngit add app.js\ngit commit -qm change\n",
			"commit.sh committed app.js other than"},
		{"a file deleted that the tree held", "git add app.js\ngit rm -q keep.txt\ngit commit -qm change\n",
			"commit.sh committed keep.txt other than"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ws := newWorkspace(t)
			repo := filepath.Join(ws, "repo")
			write := func(name, content string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			mustGit := func(args ...string) {
				t.Helper()
				if _, err := git(ctx, repo, args...); err != nil {
					t.Fatal(err)
				}
			}
			write(".gitignore", "build/\nscratch.txt\n")
			write("keep.txt", "keep\n")
			mustGit("add", ".")
			mustGit("commit", "-qm", "base")
			base, err := headSHA(ctx, repo)
			if err != nil {
				t.Fatal(err)
			}
			mustGit("checkout", "-qB", "patchy/run")
			write("app.js", "version();\n")
			write("scratch.txt", "notes\n")
			before, err := treeFingerprint(ctx, repo)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := runScript(ctx, repo, writeScript(t, ws, tt.script)); err != nil {
				t.Fatal(err)
			}
			got := committedSubset(ctx, repo, base, "patchy/run", before.worktree)
			if (tt.want == "") != (got == "") || !strings.HasPrefix(got, tt.want) {
				t.Errorf("committedSubset = %q, want %q", got, tt.want)
			}
		})
	}
}

// writeScript writes an executable shell script under dir.
func writeScript(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "commit.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -e\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
