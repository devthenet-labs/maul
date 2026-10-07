// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package e2e

import (
	"strconv"
	"strings"
	"testing"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/templates"
)

// The in-pod wrap-up of an intent stage cut off at its turn limit, end to
// end: the kubelet runs the real agent-runner with hack/fake-agent/claude in
// the model's place, the scripted CLI stopping its first run at the
// --max-turns agent-runner gave it, as the CLI does (error_max_turns), and
// intent-controller collects, records and marks what the pod salvaged.

// scriptedCut is the scripted claude's spend on a first run it stops at its
// turn limit (the same as an uncut first run's).
var scriptedCut = scriptedFirst

// Wrap-up bounds, as agentrun holds them: a grant of at least
// wrapUpMinGrant turns holds back between wrapUpMinTurns and wrapUpMaxTurns
// of them for the rounds after its first run.
const (
	wrapUpMinGrant = 15
	wrapUpMinTurns = 3
	wrapUpMaxTurns = 6
)

// startWrapUpIntent runs the intent stack with the real agent-runner for
// the given phase, the scripted claude ending its first run as first says
// and resuming it as repair says, and files an intent.
func startWrapUpIntent(t *testing.T, phase, first, repair string) (*intentEnv, int, string) {
	t.Helper()
	e := startIntents(t)
	e.kubelet.useAgentRunner(t, phase)
	e.kubelet.setAgentEnv("PATCHY_FAKE_CLAUDE_FIRST", first)
	e.kubelet.setAgentEnv("PATCHY_FAKE_CLAUDE_REPAIR", repair)
	number, name := e.fileIntent(t)
	return e, number, name
}

// approvePlan approves the Intent's posted plan.
func (e *intentEnv) approvePlan(t *testing.T, number int, name string) {
	t.Helper()
	in := e.waitPhase(t, name, v1alpha1.IntentAwaitingApproval)
	afterSecond(in.Status.Plan.PostedAt.Time)
	e.gh.LabelIssue(number, approveLabel, approver)
}

// checkWrapUpCalls checks the scripted claude ran the stage's first run,
// then one wrap-up resuming its session under its tools: the first run
// held to its share of the grant, which held back the turns the wrap-up was
// given, within agentrun's bounds. It returns the first run's turn limit.
func checkWrapUpCalls(t *testing.T, calls []scriptedCall) int {
	t.Helper()
	if len(calls) != 2 || calls[0].mode != "first" || calls[1].mode != "resume" ||
		calls[1].session != calls[0].session || calls[1].tools != calls[0].tools {
		t.Fatalf("claude calls = %+v, want the first run, then one resume of its session under its tools", calls)
	}
	first, err := strconv.Atoi(calls[0].maxTurns)
	if err != nil {
		t.Fatalf("first run's --max-turns = %q: %v", calls[0].maxTurns, err)
	}
	reserve, err := strconv.Atoi(calls[1].maxTurns)
	if err != nil {
		t.Fatalf("wrap-up's --max-turns = %q: %v", calls[1].maxTurns, err)
	}
	grant := first + reserve
	if grant < wrapUpMinGrant || reserve < wrapUpMinTurns || reserve > wrapUpMaxTurns ||
		reserve != min(wrapUpMaxTurns, max(wrapUpMinTurns, grant/5)) {
		t.Errorf("first run --max-turns %d, wrap-up --max-turns %d: want the wrap-up given exactly the turns "+
			"held back from a grant of %d", first, reserve, grant)
	}
	return first
}

// checkWrapUp checks a run records its cut-off as want, and the turns and
// session its pod reported.
func checkWrapUp(t *testing.T, run *v1alpha1.IntentRun, want v1alpha1.IntentRunWrapUp, turns int, session string) {
	t.Helper()
	if w := run.Status.WrapUp; w == nil || *w != want {
		t.Errorf("run %s wrap-up = %+v, want %+v", run.Name, w, want)
	}
	if run.Status.NumTurns != int32(turns) {
		t.Errorf("run %s turns = %d, want %d", run.Name, run.Status.NumTurns, turns)
	}
	if run.Status.SessionID != session {
		t.Errorf("run %s session = %q, want %q", run.Name, run.Status.SessionID, session)
	}
}

// TestIntentBuildSalvagedAfterMaxTurns: a build stopped at its turn limit
// before writing its report is resumed once, in its own session, to write
// it; the report it writes is settled, the tree it found is committed and
// pushed, and the pull request says the build was cut off. The run records
// the wrap-up, both runs' turns and spend, and one transcript.
func TestIntentBuildSalvagedAfterMaxTurns(t *testing.T) {
	e, number, name := startWrapUpIntent(t, "build", "max-turns", "fix")
	e.approvePlan(t, number, name)
	job, run := e.buildAttempt(t, name, 1)
	if run.Status.Outcome != "ok" {
		t.Fatalf("build run = %s %q, want ok after the wrap-up", run.Status.Outcome, run.Status.Detail)
	}
	calls := scriptedCalls(t, job)
	first := checkWrapUpCalls(t, calls)

	in := e.waitPhase(t, name, v1alpha1.IntentInReview)
	run = e.run(t, run.Name)
	checkWrapUp(t, run, v1alpha1.IntentRunWrapUp{Reason: v1alpha1.WrapUpMaxTurns, Resumed: true, Kept: true},
		first+1, calls[0].session)
	u := run.Status.Usage
	if u.InputTokens != scriptedCut.InputTokens+scriptedRepair.InputTokens ||
		u.OutputTokens != scriptedCut.OutputTokens+scriptedRepair.OutputTokens ||
		u.CacheReadTokens != scriptedCut.CacheReadTokens+scriptedRepair.CacheReadTokens ||
		u.CacheCreationTokens != scriptedCut.CacheCreationTokens+scriptedRepair.CacheCreationTokens {
		t.Errorf("usage = %+v, want the cut-off run's and the wrap-up's tokens summed", u)
	}
	checkOneTranscript(t, job)

	commit, ok := e.gh.CommitOf(run.Status.PushedCommit)
	if !ok || len(commit.Files) != 1 || string(commit.Files["VERSION"]) != "0.1.0\n" {
		t.Errorf("pushed commit = %+v, want the VERSION the cut-off run wrote", commit)
	}
	if len(in.Status.PullRequests) != 1 {
		t.Fatalf("pull requests = %+v, want the build's one", in.Status.PullRequests)
	}
	pr, ok := e.gh.Pull(int(in.Status.PullRequests[0].Number))
	if !ok || !strings.Contains(pr.Body, "The build was cut off before it finished: it reached its turn limit.") ||
		!strings.Contains(pr.Body, "In one short final turn, changing no code") {
		t.Errorf("pull request body does not say the build was cut off and wrapped up:\n%s", pr.Body)
	}
}

// TestIntentPlanSalvagedAfterMaxTurns: a plan stopped at its turn limit is
// kept and posted for approval marked as cut off, its run recording the
// wrap-up. Without a report on disk its session is resumed once to write
// one; with one already written, that report is settled as it stands and
// no further turn is spent.
func TestIntentPlanSalvagedAfterMaxTurns(t *testing.T) {
	for _, tt := range []struct {
		name, first string
		resumed     bool
		says        string
	}{
		{"no report: one wrap-up turn writes it", "max-turns", true,
			"patchy gave it one short final turn, in which it wrote this plan from what it had found by then."},
		{"report on disk: settled, no turn spent", "max-turns-report", false,
			"This is the plan it had written by then."},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e, number, name := startWrapUpIntent(t, "plan", tt.first, "fix")
			in := e.waitPhase(t, name, v1alpha1.IntentAwaitingApproval)
			planRun := v1alpha1.IntentRunName(name, v1alpha1.IntentStagePlan, 1, "", 1)
			job := e.kubelet.waitRun(t, "the plan job to run", phaseIs("plan"))
			calls := scriptedCalls(t, job)
			turns := 0
			if tt.resumed {
				turns = checkWrapUpCalls(t, calls) + 1
			} else {
				if len(calls) != 1 || calls[0].mode != "first" {
					t.Fatalf("claude calls = %+v, want the first run alone", calls)
				}
				n, err := strconv.Atoi(calls[0].maxTurns)
				if err != nil {
					t.Fatalf("first run's --max-turns = %q: %v", calls[0].maxTurns, err)
				}
				turns = n
			}
			run := e.run(t, planRun)
			if run.Status.Outcome != "ok" {
				t.Fatalf("plan run = %s %q, want ok", run.Status.Outcome, run.Status.Detail)
			}
			checkWrapUp(t, run, v1alpha1.IntentRunWrapUp{Reason: v1alpha1.WrapUpMaxTurns, Resumed: tt.resumed,
				Kept: true}, turns, calls[0].session)

			pl := in.Status.Plan
			comments := e.own(number, templates.PlanMarker(namespace, name, 1, pl.Digest))
			if len(comments) != 1 {
				t.Fatalf("plan comments = %+v, want exactly one", comments)
			}
			body := comments[0].Body
			if !strings.Contains(body, "The planner was cut off before it finished: it reached its turn limit. "+
				tt.says) {
				t.Errorf("the plan comment does not say the planner was cut off (%q):\n%s", tt.says, body)
			}
			if !strings.Contains(body, "summary: Add a VERSION file (scripted plan from hack/fake-agent/claude)") {
				t.Errorf("the plan comment does not show the plan the scripted claude wrote:\n%s", body)
			}
			checkOneTranscript(t, job)
		})
	}
}

// TestIntentWrapUpTouchingCodeRefused: a wrap-up that changes the working
// tree — the code it touched was never built or tested — is refused whole,
// though the report it wrote is valid: the attempt keeps the cut-off run's
// outcome with the refusal after it, records the wrap-up as not kept, and
// nothing is pushed.
func TestIntentWrapUpTouchingCodeRefused(t *testing.T) {
	e, number, name := startWrapUpIntent(t, "build", "max-turns", "touch")
	e.approvePlan(t, number, name)
	job, run := e.buildAttempt(t, name, 1)
	if run.Status.Outcome != "budget_exceeded" ||
		!strings.Contains(run.Status.Detail, "(wrap-up refused: it changed the working tree: VERSION") {
		t.Errorf("build attempt 1 = %s %q, want budget_exceeded with the wrap-up refused for changing VERSION",
			run.Status.Outcome, run.Status.Detail)
	}
	calls := scriptedCalls(t, job)
	first := checkWrapUpCalls(t, calls)
	checkWrapUp(t, run, v1alpha1.IntentRunWrapUp{Reason: v1alpha1.WrapUpMaxTurns, Resumed: true},
		first+1, calls[0].session)
	e.waitFailed(t, name)
	if writes := e.gh.RefWrites(); len(writes) != 0 {
		t.Errorf("ref writes = %+v, want nothing pushed", writes)
	}
}

// TestIntentStaleSuccessReportNeedsAWrapUp: a build that wrote a report
// claiming success: true and then went on working until its turn limit
// stopped it is never pushed on that report alone. Its session is resumed
// for a wrap-up turn that checks the report against the tree: one that
// cannot run leaves the report recorded beside the cut-off outcome, never
// pushed; one that owns up to unfinished work ends the attempt not built.
func TestIntentStaleSuccessReportNeedsAWrapUp(t *testing.T) {
	for _, tt := range []struct {
		name, repair  string
		outcome, says string
		kept          bool
	}{
		{name: "the wrap-up cannot resume", repair: "lost", outcome: "budget_exceeded",
			says: "its report was recorded, but a build cut off before confirming it is never pushed"},
		{name: "the wrap-up owns up", repair: "report", outcome: "not_built",
			says: "the build agent reported the plan not built: VERSION is written, but the suite never ran on it.",
			kept: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e, number, name := startWrapUpIntent(t, "build", "max-turns-report", tt.repair)
			e.approvePlan(t, number, name)
			job, run := e.buildAttempt(t, name, 1)
			if run.Status.Outcome != tt.outcome || !strings.Contains(run.Status.Detail, tt.says) {
				t.Errorf("build attempt 1 = %s %q, want %s saying %q", run.Status.Outcome, run.Status.Detail,
					tt.outcome, tt.says)
			}
			calls := scriptedCalls(t, job)
			if len(calls) != 2 || calls[0].mode != "first" || calls[1].mode != "resume" ||
				calls[1].session != calls[0].session {
				t.Fatalf("claude calls = %+v, want the first run, then an attempt to resume its session", calls)
			}
			want := v1alpha1.IntentRunWrapUp{Reason: v1alpha1.WrapUpMaxTurns, Resumed: true, Kept: tt.kept}
			if w := run.Status.WrapUp; w == nil || *w != want {
				t.Errorf("run wrap-up = %+v, want %+v", w, want)
			}
			if run.Status.PushedCommit != "" {
				t.Errorf("run pushed %s on a report no wrap-up confirmed", run.Status.PushedCommit)
			}
			e.waitFailed(t, name)
			if writes := e.gh.RefWrites(); len(writes) != 0 {
				t.Errorf("ref writes = %+v, want nothing pushed", writes)
			}
		})
	}
}
