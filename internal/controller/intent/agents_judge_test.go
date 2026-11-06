// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/report"
	"github.com/devthenet-labs/maul/internal/templates"
)

// agentsRunCheck is a cli run check of the count outcome: a program that
// runs the built binary and prints the marker naming the assertion it saw
// fail.
const agentsRunCheck = `{"id":"count.run","outcome":"count","form":"run","run":{"language":"go","files":[
 {"path":"main.go","content":"package main\n// runs {{bin}} on {{tree}}; {{marker}}\nfunc main() {}\n"}],
 "args":["{{run}}"],"expect":{"exit":0,"stdoutContains":"ok"}}}`

// agentsPV is an agents round: placement agents, the check's head replay
// passing with a token, and base (nil for none written).
func agentsPV(digest string, base *v1alpha1.BaseStatus) *v1alpha1.PreviewVerification {
	pv := &v1alpha1.PreviewVerification{}
	pv.Name = "pv-agents"
	pv.Spec.Placement = v1alpha1.PlacementAgents
	pv.Status.Phase = v1alpha1.VerificationObserved
	pv.Status.Replay = []v1alpha1.CheckRun{{CheckDigest: digest, Result: v1alpha1.CheckRunPass, Token: "tok-h",
		Seq: 3}}
	pv.Status.Ledger = &v1alpha1.LedgerSummary{Count: 9}
	pv.Status.Base = base
	return pv
}

// baseRunOf is a run of digest at the base tree.
func baseRunOf(digest string, result v1alpha1.CheckRunResult, step int32,
	cause v1alpha1.CheckRunCause) v1alpha1.CheckRun {
	r := v1alpha1.CheckRun{CheckDigest: digest, Result: result, Token: "tok-b", Seq: 5, Tree: v1alpha1.TreeBase,
		Cause: cause}
	if step > 0 {
		r.AssertionStep, r.AssertionID = step, "marker count"
	}
	return r
}

// TestDecideInputBase: an agents round reaches Decide with a required base
// (S2-20): what the agents host recorded of it (unbuildable, its runs at
// the base tree, each with its cause), never a run of another tree; a slot
// round has none, whatever its status says.
func TestDecideInputBase(t *testing.T) {
	check := savedCheck(t, agentsRunCheck)
	o := v1alpha1.AcceptanceOutcome{ID: "count", Outcome: "bb count prints the beat count"}
	acc := &v1alpha1.IntentAcceptance{Outcomes: []v1alpha1.AcceptanceOutcome{o}}
	compile := baseRunOf(check.Digest, v1alpha1.CheckRunError, 0, v1alpha1.CheckRunCauseCompile)
	headTagged := baseRunOf(check.Digest, v1alpha1.CheckRunFail, 1, "")
	headTagged.Tree = v1alpha1.TreeHead

	slot := agentsPV(check.Digest, &v1alpha1.BaseStatus{Required: true, Runs: []v1alpha1.CheckRun{compile}})
	slot.Spec.Placement = ""
	if in := decideInput(acc, slot, nil, nil); in.Base != nil {
		t.Errorf("a slot round has a base: %+v", in.Base)
	}
	if in := decideInput(acc, agentsPV(check.Digest, nil), nil, nil); in.Base == nil || !in.Base.Required ||
		in.Base.Unbuildable || len(in.Base.Runs) != 0 {
		t.Errorf("an agents round with no base written: %+v, want a required base with no run", in.Base)
	}
	in := decideInput(acc, agentsPV(check.Digest, &v1alpha1.BaseStatus{Unbuildable: true,
		Runs: []v1alpha1.CheckRun{compile, headTagged}}), nil, nil)
	b := in.Base
	if b == nil || !b.Required || !b.Unbuildable || len(b.Runs) != 1 || b.Runs[0].Cause != acceptance.CauseCompile ||
		b.Runs[0].Token != "tok-b" {
		t.Fatalf("base = %+v, want required, unbuildable, the base run alone with its cause", b)
	}
}

// TestAgentsJudgementTable is S2-20's table from an agents round's facts
// (what the agents host recorded at head and base) to Decide's result: a
// change outcome passes on its reviewed run check only red by assertion at
// base or where the check could not compile there (n/a), never on a base
// timeout, cut output, leftover processes or a bind error (no cause
// recorded), never with a missing base run, and a head build failure is
// tools-build-failed for every outcome, never a fail.
func TestAgentsJudgementTable(t *testing.T) {
	check := savedCheck(t, agentsRunCheck)
	o := v1alpha1.AcceptanceOutcome{ID: "count", Outcome: "bb count prints the beat count"}
	acc := &v1alpha1.IntentAcceptance{Outcomes: []v1alpha1.AcceptanceOutcome{o},
		Reviews: []v1alpha1.CheckReviewRecord{{CheckDigest: check.Digest, OutcomeDigest: outcomeDigest(o),
			Answer: v1alpha1.ReviewProves, Run: "r"}}}
	pass := &report.Evaluation{Verdicts: []report.OutcomeVerdict{{ID: "count", Verdict: report.VerdictPass,
		Ledger: []int32{3}}}}
	runs := func(rs ...v1alpha1.CheckRun) *v1alpha1.BaseStatus {
		return &v1alpha1.BaseStatus{Required: true, Runs: rs}
	}
	tests := []struct {
		name   string
		base   *v1alpha1.BaseStatus
		env    []v1alpha1.PartFact
		result acceptance.Result
		reason string
	}{
		{"red by assertion at base", runs(baseRunOf(check.Digest, v1alpha1.CheckRunFail, 1, "")),
			nil, acceptance.ResultPassed, acceptance.ReasonConfirmed},
		{"a compile error at base: no base comparison",
			runs(baseRunOf(check.Digest, v1alpha1.CheckRunError, 0, v1alpha1.CheckRunCauseCompile)),
			nil, acceptance.ResultPassed, acceptance.ReasonConfirmed},
		{"a vendored base tree: no base comparison",
			runs(baseRunOf(check.Digest, v1alpha1.CheckRunError, 0, v1alpha1.CheckRunCauseVendoredTree)),
			nil, acceptance.ResultPassed, acceptance.ReasonConfirmed},
		{"a base timeout gives no base evidence", runs(baseRunOf(check.Digest, v1alpha1.CheckRunError, 0, "")),
			nil, acceptance.ResultUnsure, acceptance.ReasonNoBaseEvidence},
		{"green at base", runs(baseRunOf(check.Digest, v1alpha1.CheckRunPass, 0, "")),
			nil, acceptance.ResultUnsure, acceptance.ReasonNoBaseEvidence},
		{"an exit-only fail at base", runs(baseRunOf(check.Digest, v1alpha1.CheckRunFail, 0, "")),
			nil, acceptance.ResultUnsure, acceptance.ReasonNoBaseEvidence},
		{"no base run", runs(), nil, acceptance.ResultUnsure, acceptance.ReasonNoBaseEvidence},
		{"no base written", nil, nil, acceptance.ResultUnsure, acceptance.ReasonNoBaseEvidence},
		{"an unbuildable base", &v1alpha1.BaseStatus{Required: true, Unbuildable: true},
			nil, acceptance.ResultPassed, acceptance.ReasonConfirmed},
		{"the head did not build", runs(baseRunOf(check.Digest, v1alpha1.CheckRunFail, 1, "")),
			[]v1alpha1.PartFact{{Name: "build-head", Role: v1alpha1.PreviewRoleBuild,
				LastTermination: &v1alpha1.Termination{Reason: "Error", ExitCode: 1}}},
			acceptance.ResultUnsure, acceptance.ReasonBuildFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pv := agentsPV(check.Digest, tt.base)
			pv.Spec.Checks = []v1alpha1.SavedCheck{check}
			pv.Status.Environment = tt.env
			j := judgeRound(acc, pv, nil, pass)
			got := j.decision.PerOutcome[0]
			if got.Result != tt.result || got.Reason != tt.reason {
				t.Fatalf("got %+v, want %s (%s)", got, tt.result, tt.reason)
			}
			if j.decision.Fix != nil || j.decision.Suspected != nil {
				t.Errorf("a fix from an agents pass: %+v", j.decision)
			}
			results := outcomeResults(acc, "", j)
			if r := results[0]; string(r.Result) != string(tt.result) || r.Reason != tt.reason ||
				(tt.result == acceptance.ResultPassed && r.Line == "") {
				t.Errorf("recorded %+v", r)
			}
		})
	}
}

// TestAgentsReasonsRendered (S2-20, from wave 0): the agents reasons
// Decide gives, a program that failed with no assertion (not-asserted)
// and a head the tool pod could not build (tools-build-failed), reach the
// verification comment in maul's words.
func TestAgentsReasonsRendered(t *testing.T) {
	e, name := newAgentsEnv(t, repoImage)
	e.reviewReady(name, 4)
	d := acceptance.Decision{Env: acceptance.EnvOK, PerOutcome: []acceptance.OutcomeResult{
		{ID: oVersion, Result: acceptance.ResultUnsure, Reason: acceptance.ReasonNotAsserted},
		{ID: oHealthy, Result: acceptance.ResultUnsure, Reason: acceptance.ReasonBuildFailed},
		{ID: oManual, Result: acceptance.ResultUnsure, Reason: acceptance.ReasonManual}}}
	e.setStatus(name, func(in *v1alpha1.Intent) { applyJudgement(in, roundJudgement{decision: d}, time.Now()) })
	p := e.prVerifyPass(name)
	v := p.in.Status.Verification
	section := p.verifySection(context.Background(), nil)
	body, err := templates.RenderVerifyComment(templates.VerifyComment{Namespace: testNS, Intent: name,
		Round: v.Round, Attempt: v.Attempt, Section: *section})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"the program failed without asserting anything about the change",
		"couldn't confirm: the build did not finish"} {
		if !strings.Contains(body, want) {
			t.Errorf("the verification comment does not say %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "preview") {
		t.Errorf("an agents verification comment mentions a preview:\n%s", body)
	}
}
