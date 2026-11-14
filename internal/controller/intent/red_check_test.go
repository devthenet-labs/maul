// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/agentrun"
	"github.com/devthenet-labs/maul/internal/checks"
	"github.com/devthenet-labs/maul/internal/templates"
)

// A defect found in round 1 (S1 bug 7, the post-demo decision): the
// evaluator reports fail and proposes a check that shows it; tools confirms
// it red by assertion; the reviewer answers the red question; maul stores
// it expecting to fail and judges the outcome failed, which starts a
// verify-fix round.

// redReport is the evaluator's report that the version outcome fails,
// proposing the check that shows it.
const redReport = `---
verdicts:
  - outcome: version-shown
    verdict: fail
    ledger: [4, 10]
  - outcome: still-healthy
    verdict: unsure
    ledger: []
proposed:
  - id: version-shown
    outcome: version-shown
    file: "/work/scratch/checks/version-shown.json"
    expect: fail
---
The version endpoint answers with the wrong note.
`

// redCheck is versionCheck saved red.
func redCheck(t *testing.T) v1alpha1.SavedCheck {
	c := savedCheck(t, versionCheck)
	c.Expect, c.FailedStep, c.FailedAssertion = v1alpha1.CheckExpectFail, 2, "json /note"
	return c
}

// confirmRed plays preview-controller collecting pv with red confirmed as
// expected to fail, its confirmation run as given.
func (e *env) confirmRed(pvName string, red v1alpha1.SavedCheck, run v1alpha1.CheckRun, confirmed bool) {
	e.t.Helper()
	e.playTools(pvName, v1alpha1.VerificationObserved, toolPodIP, true)
	pv := e.pvNamed(pvName)
	pv.Status.Confirmed = nil
	if confirmed {
		pv.Status.Confirmed = []v1alpha1.SavedCheck{red}
	}
	pv.Status.Confirmations = []v1alpha1.CheckRun{run}
	if err := e.c.Update(context.Background(), pv); err != nil {
		e.t.Fatal(err)
	}
}

// redRun is the red check's confirmation red by assertion at its second
// step.
func redRun(red v1alpha1.SavedCheck) v1alpha1.CheckRun {
	return v1alpha1.CheckRun{CheckDigest: red.Digest, Result: v1alpha1.CheckRunFail, Token: "cf-tok", Seq: 10,
		DryRunSeq: 4, Expect: v1alpha1.CheckExpectFail, AssertionStep: 2, AssertionID: "json /note"}
}

// redRound drives intent name's first round to its collection, the
// evaluator reporting redReport and the red check confirmed as run shows.
func (e *env) redRound(name string, red v1alpha1.SavedCheck, run v1alpha1.CheckRun, confirmed bool) {
	e.t.Helper()
	e.evaluateReporting(redReport)
	e.review(name, 3)
	pv := e.onlyVerification(name)
	e.playTools(pv.Name, v1alpha1.VerificationRunning, toolPodIP, true)
	e.review(name, 1)
	e.runRuns()
	e.runRuns()
	e.confirmRed(pv.Name, red, run, confirmed)
}

// TestRedCheckSuspectedUntilAPersonConfirms: confirmed red by assertion,
// reviewed with the red question (told the assertion that failed) and
// answered proves, the check is stored expecting to fail, its review cached
// as the red question's; the outcome is a suspected defect (fourth review):
// unsure, no automatic fix, the hand-off held and the evidence shown; an
// approver's /maul verify fix confirms it and starts the verify-fix round.
func TestRedCheckSuspectedUntilAPersonConfirms(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	red := redCheck(t)
	r := &fakeReviewer{}
	e.reviewChecksWith(r)
	e.redRound(name, red, redRun(red), true)
	e.settleReviews(name)

	if len(r.handoffs) != 1 || len(r.handoffs[0].Reviews) != 1 {
		t.Fatalf("handoffs = %+v, want the red check reviewed", r.handoffs)
	}
	if it := r.handoffs[0].Reviews[0]; it.Expect != "fail" || it.FailedStep != 2 ||
		it.FailedAssertion != "json /note" || it.CheckDigest != red.Digest {
		t.Errorf("review = %+v, want the red question about json /note at step 2", it)
	}
	if _, stored := e.checkStore(name); len(stored) != 1 || stored[0] != red {
		t.Fatalf("store = %+v, want the red check, expecting to fail", stored)
	}
	acc := e.get(name).Status.Acceptance
	if i := slices.IndexFunc(acc.Reviews, func(rv v1alpha1.CheckReviewRecord) bool {
		return rv.CheckDigest == red.Digest
	}); i < 0 || acc.Reviews[i].Expect != v1alpha1.CheckExpectFail || acc.Reviews[i].Answer != v1alpha1.ReviewProves ||
		acc.Reviews[i].FailedStep != 2 || acc.Reviews[i].FailedAssertion != "json /note" {
		t.Errorf("reviews = %+v, want the red question's proves, keyed to json /note at step 2", acc.Reviews)
	}
	e.checkSuspectedHeld(name, red)

	// An approver confirms it: the fix round starts exactly as an
	// automatic one, on the red check's failure.
	e.gh.comment(approver, "/maul verify fix")
	e.settleActions(name)
	if c := e.verification(name).Suspected; c == nil || c.Confirmed == nil || c.Confirmed.ByID != actorOf(approver).ID {
		t.Fatalf("suspected = %+v, want confirmed by the approver", c)
	}
	e.reviewUntil(name, func() bool { return len(e.runsOf(name, v1alpha1.IntentStageRevise)) > 0 })
	if run := e.runsOf(name, v1alpha1.IntentStageRevise)[0]; run.Spec.Trigger != v1alpha1.IntentRunTriggerVerify {
		t.Errorf("revise run = %+v, want the verify fix", run.Spec)
	}
}

// TestRedCheckGuards: each guard alone keeps the outcome unsure, never
// failed, and starts no fix: a reviewer's no (nothing stored), a red that is
// no assertion (never a candidate, never reviewed), and a check proposed red
// whose own confirmation ran green (flaky: never stored).
func TestRedCheckGuards(t *testing.T) {
	for _, tc := range []struct {
		name      string
		answer    v1alpha1.ReviewAnswer
		run       func(v1alpha1.SavedCheck) v1alpha1.CheckRun
		confirmed bool
		reviewed  bool
	}{
		{"the reviewer says no", v1alpha1.ReviewDoesNotProve, redRun, true, true},
		{"red, but not by assertion", v1alpha1.ReviewProves, func(c v1alpha1.SavedCheck) v1alpha1.CheckRun {
			r := redRun(c)
			r.AssertionStep, r.AssertionID = 0, ""
			return r
		}, true, false},
		{"red by another assertion than it was confirmed at", v1alpha1.ReviewProves,
			func(c v1alpha1.SavedCheck) v1alpha1.CheckRun {
				r := redRun(c)
				r.AssertionID = "header x-note"
				return r
			}, true, false},
		{"its confirmation ran green", v1alpha1.ReviewProves, func(c v1alpha1.SavedCheck) v1alpha1.CheckRun {
			r := redRun(c)
			r.Result, r.AssertionStep = v1alpha1.CheckRunPass, 0
			return r
		}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, name := newEvaluatorEnv(t, 0.4)
			red := redCheck(t)
			r := &fakeReviewer{answers: map[string]v1alpha1.ReviewAnswer{red.Digest: tc.answer}}
			e.reviewChecksWith(r)
			e.redRound(name, red, tc.run(red), tc.confirmed)
			e.settleReviews(name)
			if reviewed := len(r.handoffs) > 0; reviewed != tc.reviewed {
				t.Errorf("reviewed %v, want %v", reviewed, tc.reviewed)
			}
			if _, stored := e.checkStore(name); len(stored) != 0 {
				t.Errorf("store = %+v, want nothing", stored)
			}
			v := e.verification(name)
			if v.State != v1alpha1.VerificationUnconfirmed || v.Failed != 0 ||
				v.Outcomes[0].Result != v1alpha1.OutcomeUnsure {
				t.Fatalf("verification = %s %+v, want version-shown unsure", v.State, v.Outcomes)
			}
			e.review(name, 3)
			if n := len(e.runsOf(name, v1alpha1.IntentStageRevise)); n != 0 {
				t.Errorf("revise runs = %d, want no fix", n)
			}
		})
	}
}

// TestRedCheckClearedOnceGreen (review finding 2): a check saved red that
// replays green is recorded as cleared, so from then on its replays are
// judged as any saved check's; one that has not stays uncleared.
func TestRedCheckClearedOnceGreen(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	red, other := redCheck(t), savedCheck(t, healthCheck)
	red.FailedStep = 2
	other.Expect, other.FailedStep = v1alpha1.CheckExpectFail, 1
	e.storeChecks(name, red, other)
	e.review(name, 3)
	pv := e.onlyVerification(name)
	e.playTools(pv.Name, v1alpha1.VerificationRunning, toolPodIP, true)
	e.review(name, 1)
	e.runRuns()
	e.runRuns()
	e.replayed(pv.Name, v1alpha1.CheckRun{CheckDigest: red.Digest, Result: v1alpha1.CheckRunPass, Token: "t1", Seq: 1},
		v1alpha1.CheckRun{CheckDigest: other.Digest, Result: v1alpha1.CheckRunFail, Token: "t2", Seq: 2})
	e.settleReviews(name)
	if got := e.get(name).Status.Acceptance.RedCleared; len(got) != 1 || got[0].Digest != red.Digest ||
		got[0].Verification != pv.Name {
		t.Errorf("red cleared = %v, want the red check that replayed green alone", got)
	}
}

// TestRedCheckReviewedGreenOnceGreen (third review, F5): a check saved red
// that replays green after the fix is asked the green question too, in that
// round's reviewer run, and only then passes its outcome.
func TestRedCheckReviewedGreenOnceGreen(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	red := redCheck(t)
	e.storeChecks(name, red)
	e.setStatus(name, func(in *v1alpha1.Intent) {
		rv := in.Status.Acceptance.Reviews
		for i := range rv {
			rv[i].Expect, rv[i].FailedStep, rv[i].FailedAssertion = v1alpha1.CheckExpectFail, red.FailedStep,
				red.FailedAssertion
		}
	})
	r := &fakeReviewer{}
	e.reviewChecksWith(r)
	e.evaluateReporting(`---
verdicts:
  - outcome: version-shown
    verdict: pass
    ledger: [1]
  - outcome: still-healthy
    verdict: unsure
    ledger: []
---
Fixed.
`)
	e.review(name, 3)
	pv := e.onlyVerification(name)
	e.playTools(pv.Name, v1alpha1.VerificationRunning, toolPodIP, true)
	e.review(name, 1)
	e.runRuns()
	e.runRuns()
	e.replayed(pv.Name, v1alpha1.CheckRun{CheckDigest: red.Digest, Result: v1alpha1.CheckRunPass, Token: "t1", Seq: 1})
	e.settleReviews(name)
	if len(r.handoffs) != 1 || len(r.handoffs[0].Reviews) != 1 || r.handoffs[0].Reviews[0].Expect != "" ||
		r.handoffs[0].Reviews[0].CheckDigest != red.Digest {
		t.Fatalf("handoffs %+v, want the red check asked the green question", r.handoffs)
	}
	if _, stored := e.checkStore(name); len(stored) != 1 {
		t.Errorf("store %+v, want the red check alone, not stored twice", stored)
	}
	v := e.verification(name)
	if v.Outcomes[0].Result != v1alpha1.OutcomePassed || v.Outcomes[0].Basis != red.Digest {
		t.Errorf("verification %s %+v, want version-shown passed on the red check", v.State, v.Outcomes)
	}
}

// TestJudgementAndFixDecideAlike (third review, F1): the judgement records
// the red checks its round saw replay green; the fix path re-decides that
// same round afterwards, and must decide exactly as the judgement did: a
// clearance counts only from the round after the one that earned it.
func TestJudgementAndFixDecideAlike(t *testing.T) {
	red, green := redCheck(t), savedCheck(t, healthCheck)
	green.Outcome = red.Outcome
	o := v1alpha1.AcceptanceOutcome{ID: red.Outcome, Outcome: "GET /version answers with the commit it runs"}
	acc := &v1alpha1.IntentAcceptance{Outcomes: []v1alpha1.AcceptanceOutcome{o}, Reviews: []v1alpha1.CheckReviewRecord{
		{CheckDigest: green.Digest, OutcomeDigest: outcomeDigest(o), Answer: v1alpha1.ReviewProves, Run: "r"}}}
	pv := &v1alpha1.PreviewVerification{ObjectMeta: metav1.ObjectMeta{Name: "x-v3-a1"},
		Status: v1alpha1.PreviewVerificationStatus{
			Replay: []v1alpha1.CheckRun{{CheckDigest: red.Digest, Result: v1alpha1.CheckRunPass, Token: "t", Seq: 1},
				{CheckDigest: green.Digest, Result: v1alpha1.CheckRunPass, Token: "t", Seq: 2}},
			Confirmations: []v1alpha1.CheckRun{{CheckDigest: red.Digest, Result: v1alpha1.CheckRunFail, Token: "t",
				Seq: 9}},
		}}
	cs := []v1alpha1.SavedCheck{red, green}
	before := acceptance.Decide(decideInput(acc, pv, cs, nil, nil))
	addRedCleared(acc, clearedRed(cs, pv.Status.Replay), pv.Name)
	if len(acc.RedCleared) != 1 || acc.RedCleared[0].Digest != red.Digest {
		t.Fatalf("red cleared = %+v, want the red check, by this round", acc.RedCleared)
	}
	after := acceptance.Decide(decideInput(acc, pv, cs, nil, nil))
	if !reflect.DeepEqual(before, after) {
		t.Errorf("the fix path decides\n%+v\nthe judgement decided\n%+v", after, before)
	}
	next := *pv
	next.Name = "x-v4-a1"
	if in := decideInput(acc, &next, cs, nil, nil); !in.Checks[0].Cleared {
		t.Errorf("the next round does not see the clearance: %+v", in.Checks[0])
	}
}

// TestRedCheckWithoutRunDataIsNotStored (review finding 3): a check
// proposed to show a defect that acts on no data of its own run is never
// a store candidate, whatever its confirmation showed.
func TestRedCheckWithoutRunDataIsNotStored(t *testing.T) {
	c := savedCheck(t, healthCheck) // reads /healthz: no {{run}} in a request
	c.Expect = v1alpha1.CheckExpectFail
	pv := &v1alpha1.PreviewVerification{Status: v1alpha1.PreviewVerificationStatus{
		Confirmed: []v1alpha1.SavedCheck{c},
		Confirmations: []v1alpha1.CheckRun{{CheckDigest: c.Digest, Result: v1alpha1.CheckRunFail, Token: "t", Seq: 9,
			DryRunSeq: 3, Expect: v1alpha1.CheckExpectFail, AssertionStep: 1}},
	}}
	var reasons []string
	got := storeCandidates(pv, []v1alpha1.AcceptanceOutcome{{ID: "still-healthy", Outcome: "it answers"}}, nil,
		checks.Context{Preview: &checks.Preview{Paths: []string{"/"}, Components: []string{"app"}}},
		func(reason string, _ v1alpha1.SavedCheck, _ string) { reasons = append(reasons, reason) })
	if len(got) != 0 || len(reasons) != 1 {
		t.Errorf("candidates %+v (refusals %v), want the red check refused", got, reasons)
	}
}

// TestCheckReviewDefaultTurnsLeaveARepair (S1 bug 8): each check review's
// default turns keep enough back for agent-runner to repair its report.
func TestCheckReviewDefaultTurnsLeaveARepair(t *testing.T) {
	if !agentrun.Repairable(int(DefaultCheckReviewMaxTurns)) {
		t.Errorf("DefaultCheckReviewMaxTurns %d leaves a review no repair", DefaultCheckReviewMaxTurns)
	}
}

// TestUnreviewedCheckIsUnclearAlone (S1 bug 8): one review that ended
// without an accepted report leaves only its check unstored and its answer
// uncached; the other checks of the round are stored as reviewed.
func TestUnreviewedCheckIsUnclearAlone(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	version, health, again := savedCheck(t, versionCheck), savedCheck(t, healthCheck), savedCheck(t, healthCheck2)
	r := &fakeReviewer{unreviewed: map[string]bool{health.Digest: true}}
	e.reviewChecksWith(r)
	e.evaluatedRound(name, version, health, again)
	e.settleReviews(name)

	if _, stored := e.checkStore(name); !slices.Equal(digestsOf(stored), []string{version.Digest, again.Digest}) {
		t.Fatalf("store = %v, want every check but the unreviewed one", digestsOf(stored))
	}
	for _, rv := range e.get(name).Status.Acceptance.Reviews {
		if rv.CheckDigest == health.Digest {
			t.Errorf("the unreviewed check's answer was cached: %+v", rv)
		}
	}
	if n := len(e.runsOf(name, v1alpha1.IntentStageCheckReview)); n != 1 {
		t.Errorf("checkreview runs = %d, want the round's one", n)
	}
	if v := e.verification(name); v.State == v1alpha1.VerificationEvaluating {
		t.Errorf("state = %s, want the round judged", v.State)
	}
}

// checkSuspectedHeld (fourth review): intent name's round found a defect
// on red alone: suspected, never fixed on maul's own; unsure, the hand-off
// held and the evidence on the issue.
func (e *env) checkSuspectedHeld(name string, red v1alpha1.SavedCheck) {
	t := e.t
	t.Helper()
	v := e.verification(name)
	if v.State != v1alpha1.VerificationUnconfirmed || v.Failed != 0 ||
		v.Outcomes[0].Result != v1alpha1.OutcomeUnsure || v.Outcomes[0].Reason != acceptance.ReasonSuspectedDefect ||
		v.Outcomes[0].Basis != red.Digest {
		t.Fatalf("verification = %s %+v, want version-shown unsure, a suspected defect on the red check", v.State,
			v.Outcomes)
	}
	if v.Suspected == nil || v.Suspected.Confirmed != nil || v.Suspected.HeadSetDigest != v.HeadSetDigest ||
		len(v.Suspected.Outcomes) != 1 || v.Suspected.Outcomes[0] != red.Outcome {
		t.Fatalf("suspected = %+v, want version-shown at the round's heads, unconfirmed", v.Suspected)
	}
	e.review(name, 4)
	if n := len(e.runsOf(name, v1alpha1.IntentStageRevise)); n != 0 {
		t.Fatalf("revise runs = %d: a suspected defect started an automatic fix", n)
	}
	if h := e.verification(name).Handoff; h == nil || h.Released {
		t.Errorf("hand-off = %+v, want held while the defect awaits a person", h)
	}
	evidence := ""
	for _, c := range e.verifyComments(name, v.Round) {
		evidence += c.Body
	}
	for _, want := range []string{"maul believes", "a person should confirm", "`/maul verify fix`", "`version-shown`",
		"the evaluator reported", "POST /notes"} {
		if !strings.Contains(evidence, want) {
			t.Errorf("the verification comment lacks %q:\n%s", want, evidence)
		}
	}

}

// TestRedLayoutCheckMayShowADefect (S1 demo bug 9): a browser check that
// writes nothing may be stored red when the assertion it was confirmed
// failing measures the page's layout; failing a text assertion, or with
// no data of its own run behind an http read, it may not.
func TestRedLayoutCheckMayShowADefect(t *testing.T) {
	const layout = `{"id":"pads","outcome":"pads","form":"browser","browser":{"steps":[{"goto":"/"},` +
		`{"expect":{"assert":"size","locator":{"testid":"pad-1"},"height":48,"tolerance":0.5}},` +
		`{"expect":{"assert":"text","locator":{"testid":"title"},"text":{"contains":"{{run}}"}}}]}}`
	for _, tc := range []struct {
		name string
		step int32
		want bool
	}{
		{"failing its size", 2, true},
		{"failing its text", 3, false},
		{"failing its navigation", 1, false},
	} {
		c := savedCheck(t, layout)
		c.Expect, c.FailedStep, c.FailedAssertion = v1alpha1.CheckExpectFail, tc.step, "expect"
		if got := mayShowDefect(c); got != tc.want {
			t.Errorf("%s: mayShowDefect = %v, want %v", tc.name, got, tc.want)
		}
	}
	if c := redCheck(t); !mayShowDefect(c) {
		t.Errorf("the red http check that writes its run's data may not show a defect: %+v", c)
	}
}

// Red checks of a workspace round (G3-B-CLI-1): gate 3's cli check, an
// exec writing its own input file and no {{run}}, as the tool pod
// confirmed it red by its stdout assertion; and a library run of the same
// shape.
const (
	workspaceRedExec = `{"id":"offbeat-rule-x","outcome":"offbeat-rule","form":"exec","exec":{` +
		`"argv":["{{bin}}","-offbeat","p.json"],"files":[{"path":"p.json","content":"{\"tracks\":{\"kick\":` +
		`[false,true,true,false],\"hat\":[true,false,false,false]}}\n"}],"timeout":"30s",` +
		`"expect":{"exit":0,"stdoutContains":"offbeat snare 2"}}}`
	workspaceRedRun = `{"id":"parse.too-long","outcome":"offbeat-rule","form":"run","run":{"language":"go",
	 "files":[{"path":"main.go","content":"package main\n\nfunc main() { println(true) }\n"}],
	 "expect":{"exit":0,"stdoutJSON":{"":true}}}}`
	// slotExecRed is a slot exec of a client reading the application's
	// shared state, its own run named nowhere.
	slotExecRed = `{"id":"ws.echo","outcome":"offbeat-rule","form":"exec","exec":{
	 "argv":["python3","client.py"],"files":[{"path":"client.py","content":"import sys\nprint(sys.argv)\n"}],
	 "timeout":"30s","expect":{"exit":0,"stdoutContains":"players 2"}}}`
	// slotLayoutTextRed is a browser check that writes nothing and fails a
	// text assertion: no measured layout read, so it cannot show a defect.
	slotLayoutTextRed = `{"id":"label-shown","outcome":"offbeat-rule","form":"browser","browser":{"steps":[
	 {"goto":"/"},{"expect":{"assert":"text","locator":{"testid":"t"},"text":{"contains":"Kick"}}}]}}`
	// slotErrorStatusRed writes its run's data but lists an error status.
	slotErrorStatusRed = `{"id":"make-row","outcome":"offbeat-rule","form":"http","http":{"target":"app","steps":[
	 {"request":{"method":"POST","path":"/rows","json":{"n":"{{run}}"}},"expect":{"status":[201,400],
	 "json":{"/n":"{{run}}"}}}]}}`
)

// TestRedCheckRunDataRuleIsTheSlots (G3-B-CLI-1; SPEC-S2 S2-04): the rule
// that a check showing a defect writes its run's data guards a preview's
// shared, persistent data, so it holds a slot round's checks only. Every
// check run of a workspace round starts from the tree, build output and
// home as the build left them, in a scratch directory of its own, so no
// state another run left can move it: a red workspace check is a candidate
// without {{run}}, held as every red check is to a confirmation red by the
// assertion it was proposed failing. A slot check that reads shared data,
// or lists an error status, is still refused.
func TestRedCheckRunDataRuleIsTheSlots(t *testing.T) {
	cli := checks.Context{Workspace: &checks.Workspace{Binary: "/work/bin/bbcount"}}
	lib := checks.Context{Workspace: &checks.Workspace{Language: checks.LanguageGo}}
	slot := checks.Context{Preview: &checks.Preview{Paths: []string{"/"}, Components: []string{"app"}}}
	red := func(raw string, step int32) v1alpha1.SavedCheck {
		c := savedCheck(t, raw)
		c.Expect, c.FailedStep, c.FailedAssertion = v1alpha1.CheckExpectFail, step, "stdout"
		return c
	}
	confirmed := func(c v1alpha1.SavedCheck) []v1alpha1.CheckRun {
		return []v1alpha1.CheckRun{{CheckDigest: c.Digest, Result: v1alpha1.CheckRunFail, Token: "t", Seq: 31,
			Expect: v1alpha1.CheckExpectFail, AssertionStep: c.FailedStep, AssertionID: c.FailedAssertion}}
	}
	exec, run := red(workspaceRedExec, 1), red(workspaceRedRun, 1)
	for _, tt := range []struct {
		name    string
		check   v1alpha1.SavedCheck
		runs    []v1alpha1.CheckRun
		surface checks.Context
		want    bool
	}{
		{"cli exec writing no run data", exec, confirmed(exec), cli, true},
		{"library run writing no run data", run, confirmed(run), lib, true},
		{"cli exec with no confirmation", exec, nil, cli, false},
		{"cli exec confirmed green", exec, []v1alpha1.CheckRun{{CheckDigest: exec.Digest,
			Result: v1alpha1.CheckRunPass, Token: "t", Seq: 31, Expect: v1alpha1.CheckExpectFail}}, cli, false},
		{"cli exec failing without an assertion", exec, []v1alpha1.CheckRun{{CheckDigest: exec.Digest,
			Result: v1alpha1.CheckRunFail, Token: "t", Seq: 31, Expect: v1alpha1.CheckExpectFail}}, cli, false},
		{"cli exec red by another assertion", exec, []v1alpha1.CheckRun{{CheckDigest: exec.Digest,
			Result: v1alpha1.CheckRunFail, Token: "t", Seq: 31, Expect: v1alpha1.CheckExpectFail, AssertionStep: 1,
			AssertionID: "exit"}}, cli, false},
		{"cli exec red with no token", exec, []v1alpha1.CheckRun{{CheckDigest: exec.Digest,
			Result: v1alpha1.CheckRunFail, Seq: 31, Expect: v1alpha1.CheckExpectFail, AssertionStep: 1,
			AssertionID: "stdout"}}, cli, false},
		{"slot exec writing no run data", red(slotExecRed, 1), confirmed(red(slotExecRed, 1)), slot, false},
		{"slot http read", red(healthCheck, 1), confirmed(red(healthCheck, 1)), slot, false},
		{"slot browser failing a text read", red(slotLayoutTextRed, 2), confirmed(red(slotLayoutTextRed, 2)),
			slot, false},
		{"slot http listing an error status", red(slotErrorStatusRed, 1), confirmed(red(slotErrorStatusRed, 1)),
			slot, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pv := &v1alpha1.PreviewVerification{Status: v1alpha1.PreviewVerificationStatus{
				Confirmed: []v1alpha1.SavedCheck{tt.check}, Confirmations: tt.runs}}
			var reasons []string
			got := storeCandidates(pv, []v1alpha1.AcceptanceOutcome{{ID: tt.check.Outcome, Outcome: "it counts"}},
				nil, tt.surface, func(reason string, _ v1alpha1.SavedCheck, _ string) {
					reasons = append(reasons, reason)
				})
			if stored := len(got) == 1; stored != tt.want || stored == (len(reasons) == 1) {
				t.Errorf("candidates %d, refusals %v; want stored %v", len(got), reasons, tt.want)
			}
			if !tt.want && tt.runs != nil && tt.surface.Preview != nil && !slices.Equal(reasons, []string{redRuleRefusal}) {
				t.Errorf("refusals %v, want the run-data rule's", reasons)
			}
		})
	}
}

// versionReadCheck reads the version endpoint and writes nothing of its own
// run: in a preview it cannot show a defect.
const versionReadCheck = `{"id":"version-shown","outcome":"version-shown","form":"http","http":{"target":"app",
 "steps":[{"request":{"path":"/version"},"expect":{"json":{"/note":"v1"}}}]}}`

// TestUnkeptRedCheckNamedOnTheIssue (G3-B-CLI-1): a red check maul's own
// confirmation ran red but the store refused is named, with maul's reason,
// on its outcome's row of the issue's verification comment, never only in
// intent-controller's log; the outcome stays unsure and nothing is stored.
func TestUnkeptRedCheckNamedOnTheIssue(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	red := savedCheck(t, versionReadCheck)
	red.Expect, red.FailedStep, red.FailedAssertion = v1alpha1.CheckExpectFail, 1, "json /note"
	r := &fakeReviewer{}
	e.reviewChecksWith(r)
	e.redRound(name, red, v1alpha1.CheckRun{CheckDigest: red.Digest, Result: v1alpha1.CheckRunFail,
		Token: "cf-tok", Seq: 10, DryRunSeq: 4, Expect: v1alpha1.CheckExpectFail, AssertionStep: 1,
		AssertionID: "json /note"}, true)
	e.settleReviews(name)
	e.review(name, 3)

	if len(r.handoffs) != 0 {
		t.Errorf("handoffs = %+v, want the refused check never reviewed", r.handoffs)
	}
	if _, stored := e.checkStore(name); len(stored) != 0 {
		t.Errorf("store = %+v, want nothing stored", stored)
	}
	cs := e.verifyComments(name, 1)
	if len(cs) != 1 {
		t.Fatalf("verification comments = %d, want one", len(cs))
	}
	if want := "| `version-shown` | `change` | unsure: maul couldn't confirm it; the evaluator says it does not " +
		"hold, but no check maul re-ran shows it; a check proposed to show it failing was not kept: in a " +
		"preview, a check that shows a defect must write data of its own run"; !strings.Contains(cs[0].Body, want) {
		t.Errorf("comment lacks %q:\n%s", want, cs[0].Body)
	}
	if strings.Count(cs[0].Body, "was not kept") != 1 {
		t.Errorf("the unkept note is not on the one outcome's row alone:\n%s", cs[0].Body)
	}
}

// TestUnkeptRed: why a red check was not kept, by outcome, from the store's
// own refusals: the slot's run-data rule, a confirmation not red by its
// assertion, the saved-check rules; a red workspace check without run data
// and a green check refused add nothing.
func TestUnkeptRed(t *testing.T) {
	slot := checks.Context{Preview: &checks.Preview{Paths: []string{"/"}, Components: []string{"app"}}}
	cli := checks.Context{Workspace: &checks.Workspace{Binary: "/work/bin/bbcount"}}
	outcomes := []v1alpha1.AcceptanceOutcome{{ID: "version-shown", Outcome: "v"}, {ID: "still-healthy",
		Outcome: "h"}, {ID: "offbeat-rule", Outcome: "o"}}
	redOf := func(raw string, step int32, assertion string) v1alpha1.SavedCheck {
		c := savedCheck(t, raw)
		c.Expect, c.FailedStep, c.FailedAssertion = v1alpha1.CheckExpectFail, step, assertion
		return c
	}
	runOf := func(c v1alpha1.SavedCheck, assertion string) v1alpha1.CheckRun {
		return v1alpha1.CheckRun{CheckDigest: c.Digest, Result: v1alpha1.CheckRunFail, Token: "t", Seq: 10,
			Expect: v1alpha1.CheckExpectFail, AssertionStep: c.FailedStep, AssertionID: assertion}
	}
	read, version := redOf(versionReadCheck, 1, "json /note"), redOf(versionCheck, 2, "json /note")
	health, exec := savedCheck(t, healthCheck), redOf(workspaceRedExec, 1, "stdout")
	for _, tt := range []struct {
		name      string
		confirmed []v1alpha1.SavedCheck
		runs      []v1alpha1.CheckRun
		surface   checks.Context
		want      map[string]string
	}{
		{"a slot read", []v1alpha1.SavedCheck{read}, []v1alpha1.CheckRun{runOf(read, "json /note")}, slot,
			map[string]string{"version-shown": templates.UnkeptRunData}},
		{"red by another assertion", []v1alpha1.SavedCheck{version}, []v1alpha1.CheckRun{runOf(version, "status")},
			slot, map[string]string{"version-shown": templates.UnkeptConfirmation}},
		{"confirmed green", []v1alpha1.SavedCheck{version}, []v1alpha1.CheckRun{{CheckDigest: version.Digest,
			Result: v1alpha1.CheckRunPass, Token: "t", Seq: 10, Expect: v1alpha1.CheckExpectFail}}, slot,
			map[string]string{"version-shown": templates.UnkeptConfirmation}},
		{"the saved-check rules", []v1alpha1.SavedCheck{exec}, []v1alpha1.CheckRun{runOf(exec, "stdout")}, slot,
			map[string]string{"offbeat-rule": templates.UnkeptRules}},
		{"a workspace check", []v1alpha1.SavedCheck{exec}, []v1alpha1.CheckRun{runOf(exec, "stdout")}, cli,
			map[string]string{}},
		{"a green check", []v1alpha1.SavedCheck{health}, nil, slot, map[string]string{}},
	} {
		pv := &v1alpha1.PreviewVerification{Status: v1alpha1.PreviewVerificationStatus{Confirmed: tt.confirmed,
			Confirmations: tt.runs}}
		if got := unkeptRed(pv, outcomes, nil, tt.surface); !maps.Equal(got, tt.want) {
			t.Errorf("%s: unkept = %v, want %v", tt.name, got, tt.want)
		}
	}
}
