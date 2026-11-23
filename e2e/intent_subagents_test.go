// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package e2e

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/agentresult"
	"github.com/devthenet-labs/maul/internal/envelope"
	"github.com/devthenet-labs/maul/internal/transcript"
	"github.com/devthenet-labs/maul/internal/transcriptstore"
)

// A delegating build, end to end. The kubelet runs the real agent-runner for
// build Jobs with hack/fake-agent/claude in the model's place, told to hand
// part of the build to a subagent (MAUL_FAKE_CLAUDE_DELEGATE). The scripted
// CLI decides, as the real one does, from the argv agent-runner gives it: a
// run whose --disallowedTools names Agent has its Agent call refused, a run
// granted subagents (Agent off the deny list, the --settings levers given)
// runs the child in the foreground at depth 1, as claude 2.1.322 printed it
// (internal/harness/testdata/claude-2.1.322-subagent-foreground.jsonl).

// The scripted delegating build's spend: the parent's own calls (the
// result's top-level usage, the scripted build's as ever) and the child's,
// which only the result's per-model figures carry.
var (
	delegatingParent = scriptedFirst
	delegatingChild  = v1alpha1.UsageSummary{
		InputTokens: 2000, CacheCreationTokens: 100, CacheReadTokens: 50000, OutputTokens: 600,
	}
)

// delegatingCall is the parent's Agent call in the scripted stream.
const delegatingCall = "toolu_delegate"

// startDelegatingBuild runs the intent stack with intent-controller's extra
// flags, the real agent-runner for builds and the scripted claude told to
// delegate, files an intent, approves its plan and waits for the first build
// attempt to be collected.
func startDelegatingBuild(t *testing.T, extra ...string) (*intentEnv, string, agentRun, *v1alpha1.IntentRun) {
	t.Helper()
	e := startIntents(t, extra...)
	e.kubelet.useAgentRunner(t, "build")
	e.kubelet.setAgentEnv("MAUL_FAKE_CLAUDE_DELEGATE", "1")
	number, name := e.fileIntent(t)
	in := e.waitPhase(t, name, v1alpha1.IntentAwaitingApproval)
	afterSecond(in.Status.Plan.PostedAt.Time)
	e.gh.LabelIssue(number, approveLabel, approver)
	job, run := e.buildAttempt(t, name, 1)
	if run.Status.Outcome != "ok" {
		t.Fatalf("build run = %s %q, want ok", run.Status.Outcome, run.Status.Detail)
	}
	return e, name, job, run
}

// delegation is what the scripted claude made of its Agent call in a run's
// pod: "granted" or "refused", one line per call that tried.
func delegation(t *testing.T, r agentRun) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(r.Workspace, ".fake-claude", "delegation"))
	if err != nil {
		t.Fatalf("read the scripted claude's delegation in %s: %v", r.Job.Name, err)
	}
	return strings.TrimSpace(string(raw))
}

// buildPrompt is the prompt the scripted claude read in a run's pod.
func buildPrompt(t *testing.T, r agentRun) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(r.Workspace, ".fake-claude", "prompt-build"))
	if err != nil {
		t.Fatalf("read the build prompt in %s: %v", r.Job.Name, err)
	}
	return string(raw)
}

// buildStage is the stage the build's result event reported on the pod log.
func buildStage(t *testing.T, r agentRun) envelope.Stage {
	t.Helper()
	for line := range bytes.SplitSeq(r.Stdout, []byte("\n")) {
		if ev, ok := envelope.Decode(line); ok && ev.Remediation != nil {
			return ev.Remediation.Stage
		}
	}
	t.Fatalf("%s printed no build result event", r.Job.Name)
	return envelope.Stage{}
}

// storedTurns loads the transcript intent-controller persisted for run.
func (e *intentEnv) storedTurns(t *testing.T, run *v1alpha1.IntentRun) []transcript.Turn {
	t.Helper()
	if run.Status.Transcript == nil {
		t.Fatalf("%s: the transcript was not persisted", run.Name)
	}
	turns, err := transcriptstore.Load(context.Background(), e.cl.client, namespace, run.Status.Transcript.Name)
	if err != nil {
		t.Fatalf("load %s's transcript: %v", run.Name, err)
	}
	return turns
}

// sumUsage is a and b added, token by token.
func sumUsage(a, b v1alpha1.UsageSummary) v1alpha1.UsageSummary {
	return v1alpha1.UsageSummary{
		InputTokens:         a.InputTokens + b.InputTokens,
		CacheCreationTokens: a.CacheCreationTokens + b.CacheCreationTokens,
		CacheReadTokens:     a.CacheReadTokens + b.CacheReadTokens,
		OutputTokens:        a.OutputTokens + b.OutputTokens,
	}
}

// sameTokens reports whether u spent exactly want's tokens.
func sameTokens(u, want v1alpha1.UsageSummary) bool {
	return u.InputTokens == want.InputTokens && u.CacheCreationTokens == want.CacheCreationTokens &&
		u.CacheReadTokens == want.CacheReadTokens && u.OutputTokens == want.OutputTokens
}

// checkGranted checks a delegating build was granted subagents end to end:
// recorded on the run with a cost budget, carried on its Job, rendered in its
// prompt, and its Agent call granted by the argv agent-runner gave the CLI.
func checkGranted(t *testing.T, job agentRun, run *v1alpha1.IntentRun) {
	t.Helper()
	if g := run.Spec.Grant; !g.Subagents || g.Workflows {
		t.Errorf("build run grant = %+v, want subagents recorded, workflows not", run.Spec.Grant)
	}
	if run.Spec.Grant.BudgetMicroUSD <= 0 {
		t.Errorf("build run grant = %+v, want a cost budget under the cost unit", run.Spec.Grant)
	}
	if got := job.Env["MAUL_BUILD_SUBAGENTS"]; got != "true" {
		t.Errorf("build Job MAUL_BUILD_SUBAGENTS = %q, want true", got)
	}
	if got := job.Env["MAUL_BUILD_WORKFLOWS"]; got == "true" {
		t.Errorf("build Job MAUL_BUILD_WORKFLOWS = %q, want it ungranted", got)
	}
	if got := delegation(t, job); got != "granted" {
		t.Errorf("the scripted claude's Agent call was %q, want granted by the run's argv", got)
	}
	if prompt := buildPrompt(t, job); !strings.Contains(prompt, "## Subagents") {
		t.Error("the granted build's prompt has no Subagents section")
	}
}

// checkNestedTranscript checks the transcript intent-controller persisted
// for run nests the child's calls and the delegation notices, and nothing
// else, under the Agent call.
func (e *intentEnv) checkNestedTranscript(t *testing.T, run *v1alpha1.IntentRun) {
	t.Helper()
	var child, notices []string
	for _, turn := range e.storedTurns(t, run) {
		if turn.Parent == "" {
			continue
		}
		if turn.Parent != delegatingCall {
			t.Errorf("turn %d nests under %q, want %q", turn.Seq, turn.Parent, delegatingCall)
		}
		switch turn.Kind {
		case transcript.KindNotice:
			notices = append(notices, turn.Text)
		case transcript.KindToolUse:
			child = append(child, turn.Tool)
		}
	}
	if strings.Join(child, ",") != "Bash,Read" {
		t.Errorf("the child's tool calls in the transcript = %v, want Bash, Read under the Agent call", child)
	}
	wantNotices := []string{"subagent general-purpose started", "delegated work finished: completed, 2 tool uses"}
	if strings.Join(notices, "|") != strings.Join(wantNotices, "|") {
		t.Errorf("the delegation notices = %q, want %q", notices, wantNotices)
	}
}

// TestIntentBuildDelegates: with --intent-build-subagents (and the broker's
// cost unit it needs), the build run records the grant, its Job carries it,
// agent-runner renders the granted posture and the prompt's Subagents
// section, and the scripted CLI's child runs. The run then records what the
// child spent (the per-model figures, not the parent's top-level usage
// alone), the stage event counts the child's calls apart from the parent's,
// and the transcript intent-controller persists nests the child's turns and
// the delegation notices under the Agent call. The build is pushed as any
// other.
func TestIntentBuildDelegates(t *testing.T) {
	e, name, job, run := startDelegatingBuild(t, "--broker-limit-unit", "cost", "--intent-build-subagents")
	checkGranted(t, job, run)

	want := sumUsage(delegatingParent, delegatingChild)
	if u := run.Status.Usage; !sameTokens(u, want) {
		t.Errorf("usage = %+v, want the parent's and the child's tokens, %+v", u, want)
	} else if u.CostUSD != agentresult.FormatCost(0.3) {
		t.Errorf("cost = %s, want the CLI's 0.3 for the parent and the child", u.CostUSD)
	}
	if run.Status.NumTurns != 3 {
		t.Errorf("turns = %d, want the parent's 3", run.Status.NumTurns)
	}
	if stage := buildStage(t, job); stage.SubagentTurns != 2 || stage.NumTurns != 3 {
		t.Errorf("stage turns = %d parent + %d subagent, want 3 + 2", stage.NumTurns, stage.SubagentTurns)
	}

	e.checkNestedTranscript(t, run)
	checkOneTranscript(t, job)

	if in := e.waitPhase(t, name, v1alpha1.IntentInReview); len(in.Status.PullRequests) != 1 {
		t.Errorf("pull requests = %+v, want the build's one", in.Status.PullRequests)
	}
}

// TestIntentBuildDelegationRefusedWithoutTheFlag: the same scripted build
// with the flag off. Nothing records or carries a grant, the prompt says
// nothing of subagents, and the CLI refuses the Agent call from the deny
// list agent-runner gives it, so the run spends and records the parent's
// calls alone and its transcript shows the refusal and nothing nested.
func TestIntentBuildDelegationRefusedWithoutTheFlag(t *testing.T) {
	e, name, job, run := startDelegatingBuild(t)

	if run.Spec.Grant.Subagents {
		t.Errorf("build run grant = %+v, want no subagents recorded", run.Spec.Grant)
	}
	if got := job.Env["MAUL_BUILD_SUBAGENTS"]; got == "true" {
		t.Errorf("build Job MAUL_BUILD_SUBAGENTS = %q, want it ungranted", got)
	}
	if got := delegation(t, job); got != "refused" {
		t.Errorf("the scripted claude's Agent call was %q, want refused by the run's argv", got)
	}
	if prompt := buildPrompt(t, job); strings.Contains(prompt, "## Subagents") {
		t.Error("an ungranted build's prompt has a Subagents section")
	}

	if u := run.Status.Usage; !sameTokens(u, delegatingParent) {
		t.Errorf("usage = %+v, want the parent's tokens alone, %+v", u, delegatingParent)
	}
	if stage := buildStage(t, job); stage.SubagentTurns != 0 {
		t.Errorf("stage subagent turns = %d, want none", stage.SubagentTurns)
	}

	refused := false
	for _, turn := range e.storedTurns(t, run) {
		if turn.Parent != "" {
			t.Errorf("turn %d nests under %q, want nothing nested", turn.Seq, turn.Parent)
		}
		if turn.Kind == transcript.KindToolResult && strings.Contains(turn.Text, "No such tool available: Agent") {
			refused = true
		}
	}
	if !refused {
		t.Error("the transcript does not show the Agent call refused")
	}
	checkOneTranscript(t, job)

	if in := e.waitPhase(t, name, v1alpha1.IntentInReview); len(in.Status.PullRequests) != 1 {
		t.Errorf("pull requests = %+v, want the build's one", in.Status.PullRequests)
	}
}
