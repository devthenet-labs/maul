// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"maps"
	"reflect"
	"slices"
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

// redCheck is versionCheck saved red.
func redCheck(t *testing.T) v1alpha1.SavedCheck {
	c := savedCheck(t, versionCheck)
	c.Expect, c.FailedStep, c.FailedAssertion = v1alpha1.CheckExpectFail, 2, "json /note"
	return c
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
	before := acceptance.Decide(decideInput(acc, pv, cs, nil))
	addRedCleared(acc, clearedRed(cs, pv.Status.Replay), pv.Name)
	if len(acc.RedCleared) != 1 || acc.RedCleared[0].Digest != red.Digest {
		t.Fatalf("red cleared = %+v, want the red check, by this round", acc.RedCleared)
	}
	after := acceptance.Decide(decideInput(acc, pv, cs, nil))
	if !reflect.DeepEqual(before, after) {
		t.Errorf("the fix path decides\n%+v\nthe judgement decided\n%+v", after, before)
	}
	next := *pv
	next.Name = "x-v4-a1"
	if in := decideInput(acc, &next, cs, nil); !in.Checks[0].Cleared {
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
		// The exemption is the workspace's own checks': an http or browser
		// check on an agents round is off its surface, refused before the
		// red rule is reached.
		{"agents http read", red(healthCheck, 1), confirmed(red(healthCheck, 1)), cli, false},
		{"agents browser text read", red(slotLayoutTextRed, 2), confirmed(red(slotLayoutTextRed, 2)), lib,
			false},
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
			if tt.surface.Workspace != nil && tt.check.Form != v1alpha1.CheckFormExec &&
				tt.check.Form != v1alpha1.CheckFormRun && !slices.Equal(reasons, []string{"refused by the saved-check rules"}) {
				t.Errorf("refusals %v, want the saved-check rules'", reasons)
			}
		})
	}
}

// versionReadCheck reads the version endpoint and writes nothing of its own
// run: in a preview it cannot show a defect.
const versionReadCheck = `{"id":"version-shown","outcome":"version-shown","form":"http","http":{"target":"app",
 "steps":[{"request":{"path":"/version"},"expect":{"json":{"/note":"v1"}}}]}}`

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
		reviews   []v1alpha1.CheckReviewRecord
		stored    []v1alpha1.SavedCheck
		want      map[string]string
	}{
		{"a slot read", []v1alpha1.SavedCheck{read}, []v1alpha1.CheckRun{runOf(read, "json /note")}, slot, nil, nil,
			map[string]string{"version-shown": templates.UnkeptRunData}},
		{"red by another assertion", []v1alpha1.SavedCheck{version}, []v1alpha1.CheckRun{runOf(version, "status")},
			slot, nil, nil, map[string]string{"version-shown": templates.UnkeptConfirmation}},
		{"confirmed green", []v1alpha1.SavedCheck{version}, []v1alpha1.CheckRun{{CheckDigest: version.Digest,
			Result: v1alpha1.CheckRunPass, Token: "t", Seq: 10, Expect: v1alpha1.CheckExpectFail}}, slot, nil, nil,
			map[string]string{"version-shown": templates.UnkeptConfirmation}},
		{"the saved-check rules", []v1alpha1.SavedCheck{exec}, []v1alpha1.CheckRun{runOf(exec, "stdout")}, slot,
			nil, nil, map[string]string{"offbeat-rule": templates.UnkeptRules}},
		// A candidate the store admits but no review proved (none ran: the
		// evaluation ceiling, a failed reviewer; or one answered otherwise)
		// is not kept either, and says so.
		{"a workspace check no review proved", []v1alpha1.SavedCheck{exec},
			[]v1alpha1.CheckRun{runOf(exec, "stdout")}, cli, nil, nil,
			map[string]string{"offbeat-rule": templates.UnkeptReview}},
		{"a workspace check reviewed unclear", []v1alpha1.SavedCheck{exec},
			[]v1alpha1.CheckRun{runOf(exec, "stdout")}, cli, []v1alpha1.CheckReviewRecord{redReview(exec,
				outcomes[2], v1alpha1.ReviewUnclear)}, nil, map[string]string{"offbeat-rule": templates.UnkeptReview}},
		{"a workspace check reviewed for a green question", []v1alpha1.SavedCheck{exec},
			[]v1alpha1.CheckRun{runOf(exec, "stdout")}, cli, []v1alpha1.CheckReviewRecord{{CheckDigest: exec.Digest,
				OutcomeDigest: outcomeDigest(outcomes[2]), Answer: v1alpha1.ReviewProves}}, nil,
			map[string]string{"offbeat-rule": templates.UnkeptReview}},
		{"a proven workspace check the full store left out", []v1alpha1.SavedCheck{exec},
			[]v1alpha1.CheckRun{runOf(exec, "stdout")}, cli, []v1alpha1.CheckReviewRecord{redReview(exec,
				outcomes[2], v1alpha1.ReviewProves)}, nil, map[string]string{"offbeat-rule": templates.UnkeptStoreFull}},
		{"a stored workspace check", []v1alpha1.SavedCheck{exec}, []v1alpha1.CheckRun{runOf(exec, "stdout")}, cli,
			[]v1alpha1.CheckReviewRecord{redReview(exec, outcomes[2], v1alpha1.ReviewProves)},
			[]v1alpha1.SavedCheck{exec}, map[string]string{}},
		{"a green check", []v1alpha1.SavedCheck{health}, nil, slot, nil, nil, map[string]string{}},
	} {
		pv := &v1alpha1.PreviewVerification{Status: v1alpha1.PreviewVerificationStatus{Confirmed: tt.confirmed,
			Confirmations: tt.runs}}
		if got := unkeptRed(pv, outcomes, tt.stored, tt.reviews, tt.surface); !maps.Equal(got, tt.want) {
			t.Errorf("%s: unkept = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// redReview is a cached review of the red question for c and o.
func redReview(c v1alpha1.SavedCheck, o v1alpha1.AcceptanceOutcome,
	a v1alpha1.ReviewAnswer) v1alpha1.CheckReviewRecord {
	return v1alpha1.CheckReviewRecord{CheckDigest: c.Digest, OutcomeDigest: outcomeDigest(o), Answer: a,
		Expect: v1alpha1.CheckExpectFail, FailedStep: c.FailedStep, FailedAssertion: c.FailedAssertion}
}
