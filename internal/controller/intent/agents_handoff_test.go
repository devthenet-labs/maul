// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/controller/intent/agentshost"
	"github.com/devthenet-labs/maul/internal/jobs"
	"github.com/devthenet-labs/maul/internal/templates"
)

// agentsVersionRun is a cli run check of outcomesPlan's version outcome: a
// program that runs the built binary and asserts on what it prints.
const agentsVersionRun = `{"id":"version-shown.run","outcome":"version-shown","form":"run","run":{"language":"go",
 "files":[{"path":"main.go","content":"package main\n// runs {{bin}} on {{tree}}; {{marker}}\nfunc main() {}\n"}],
 "args":["{{run}}"],"expect":{"exit":0,"stdoutContains":"ok"}}}`

// toolsKey is the agents tool pod's key in these tests.
var toolsKey = strings.Repeat("ab", 32)

// toolsSecret plays the agents host creating pv's tool Secret, labelled
// with uid (pv's own unless given).
func (e *env) toolsSecret(pv *v1alpha1.PreviewVerification, uid string) {
	e.t.Helper()
	if uid == "" {
		uid = string(pv.UID)
	}
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "maul-agents",
		Name:   agentshost.JobName(pv) + jobs.ToolsSecretSuffix,
		Labels: map[string]string{v1alpha1.LabelPreviewVerification: uid}},
		Data: map[string][]byte{jobs.ToolsKeyFile: []byte(toolsKey)}}
	if err := e.c.Create(context.Background(), s); err != nil {
		e.t.Fatal(err)
	}
}

// TestAgentsEvaluateHandoff (S2-20, from S2-16): an agents round's
// evaluator is handed its workspace (the cli surface's build and binary)
// and the tool pod's key, read from the very Secret the tool pod mounts;
// no component, no browser, no look-around. With no key to read, no
// evaluator is launched; a Secret of another verification is never read
// as this one's.
func TestAgentsEvaluateHandoff(t *testing.T) {
	e, name := newAgentsEnv(t, repoImage)
	e.reviewReady(name, 4)
	pv := e.onlyVerification(name)
	e.playTools(pv.Name, v1alpha1.VerificationRunning, toolPodIP, true)
	e.review(name, 2)
	if runs := e.runsOf(name, v1alpha1.IntentStageEvaluate); len(runs) != 0 {
		t.Fatalf("evaluate runs = %d with no key to hand over", len(runs))
	}
	e.toolsSecret(&pv, "another-uid")
	e.review(name, 2)
	if runs := e.runsOf(name, v1alpha1.IntentStageEvaluate); len(runs) != 0 {
		t.Fatalf("evaluate runs = %d on another verification's key", len(runs))
	}
	if err := e.c.DeleteAllOf(context.Background(), &corev1.Secret{}); err != nil {
		t.Fatal(err)
	}
	e.toolsSecret(&pv, "")
	e.review(name, 1)
	e.runRuns()
	evals := e.specsOf("evaluate")
	if len(evals) != 1 {
		t.Fatalf("evaluate Jobs = %d, want one", len(evals))
	}
	h := decodeEvaluateHandoff(t, evals[0].IssueMarkdown)
	ws := h.Tools.Workspace
	if ws == nil || ws.Surface != "cli" || ws.Binary != "/work/bin/app" ||
		strings.Join(ws.Build, " ") != "go build -o /work/bin/app ." || ws.Language != "" {
		t.Fatalf("workspace = %+v", ws)
	}
	if h.Tools.Key != toolsKey || len(h.Components) != 0 || h.Tools.Browser || h.Tools.BrowserEntry != "" ||
		h.Tools.LookAroundRoutes != 0 || !h.Tools.Sandbox {
		t.Fatalf("tools = %+v, components %+v", h.Tools, h.Components)
	}
	// The key is a credential: it reaches the Job's handoff Secret only, never
	// the run's input ConfigMap (a ConfigMap is not a Secret; SPEC-S2 Q32).
	run := e.runsOf(name, v1alpha1.IntentStageEvaluate)[0]
	var cm corev1.ConfigMap
	if err := e.c.Get(context.Background(), client.ObjectKey{Namespace: testNS, Name: run.Spec.Inputs.ConfigMap},
		&cm); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(joinedData(cm.Data), toolsKey) {
		t.Fatal("the evaluate run's input ConfigMap carries the tools key")
	}
}

// TestAgentsEvaluateKeyGoneAtLaunch: an evaluate run whose tool pod's key
// is gone by the time it launches is never launched without it.
func TestAgentsEvaluateKeyGoneAtLaunch(t *testing.T) {
	e, name := newAgentsEnv(t, repoImage)
	e.reviewReady(name, 4)
	pv := e.onlyVerification(name)
	e.toolsSecret(&pv, "")
	e.playTools(pv.Name, v1alpha1.VerificationRunning, toolPodIP, true)
	e.review(name, 1)
	if runs := e.runsOf(name, v1alpha1.IntentStageEvaluate); len(runs) != 1 {
		t.Fatalf("evaluate runs = %d, want one", len(runs))
	}
	if err := e.c.DeleteAllOf(context.Background(), &corev1.Secret{}, client.InNamespace("maul-agents")); err != nil {
		t.Fatal(err)
	}
	e.runRuns()
	if evals := e.specsOf("evaluate"); len(evals) != 0 {
		t.Fatalf("evaluate Jobs = %d launched with no key", len(evals))
	}
}

// TestSlotEvaluateHandoffHasNoKey: a slot round's handoff is S1's: no
// workspace and no key.
func TestSlotEvaluateHandoffHasNoKey(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.review(name, 3)
	pv := e.onlyVerification(name)
	e.playTools(pv.Name, v1alpha1.VerificationRunning, toolPodIP, true)
	e.review(name, 1)
	e.runRuns()
	h := decodeEvaluateHandoff(t, e.specsOf("evaluate")[0].IssueMarkdown)
	if h.Tools.Workspace != nil || h.Tools.Key != "" {
		t.Fatalf("a slot handoff carries %+v", h.Tools)
	}
}

// agentsConfirm plays the agents host collecting pv: confirmed checks each
// confirmed at head by run (green unless red), and at base by baseRun (nil
// for none), the base unbuildable or not.
func (e *env) agentsConfirm(pvName string, unbuildable bool, head func(c v1alpha1.SavedCheck) v1alpha1.CheckRun,
	base func(c v1alpha1.SavedCheck) *v1alpha1.CheckRun, confirmed ...v1alpha1.SavedCheck) {
	e.t.Helper()
	e.playTools(pvName, v1alpha1.VerificationObserved, toolPodIP, true)
	pv := e.pvNamed(pvName)
	pv.Status.Confirmed = confirmed
	pv.Status.Confirmations = nil
	pv.Status.Base = &v1alpha1.BaseStatus{Required: true, Unbuildable: unbuildable}
	for _, c := range confirmed {
		pv.Status.Confirmations = append(pv.Status.Confirmations, head(c))
		if b := base(c); b != nil {
			pv.Status.Base.Runs = append(pv.Status.Base.Runs, *b)
		}
	}
	if err := e.c.Update(context.Background(), pv); err != nil {
		e.t.Fatal(err)
	}
}

// agentsEvaluated drives an agents intent's first round to its evaluator's
// completion.
func (e *env) agentsEvaluated(name string) v1alpha1.PreviewVerification {
	e.t.Helper()
	e.reviewReady(name, 4)
	pv := e.onlyVerification(name)
	e.toolsSecret(&pv, "")
	e.playTools(pv.Name, v1alpha1.VerificationRunning, toolPodIP, true)
	e.review(name, 1)
	e.runRuns()
	e.runRuns()
	return pv
}

// greenAtHead is a check's clean head confirmation.
func greenAtHead(c v1alpha1.SavedCheck) v1alpha1.CheckRun {
	return v1alpha1.CheckRun{CheckDigest: c.Digest, Result: v1alpha1.CheckRunPass, Token: "cf-tok", Seq: 10,
		DryRunSeq: 1}
}

// TestAgentsCheckReviewHandoff (S2-20, from S2-16): an agents round's new
// run check is validated for its workspace (a slot's rules would refuse
// it), reviewed with the workspace and each side's run (the head's pass,
// the base's fail by its assertion), and stored once proven.
func TestAgentsCheckReviewHandoff(t *testing.T) {
	e, name := newAgentsEnv(t, repoImage)
	check := savedCheck(t, agentsVersionRun)
	r := &fakeReviewer{}
	e.reviewChecksWith(r)
	pv := e.agentsEvaluated(name)
	e.agentsConfirm(pv.Name, false, greenAtHead, func(c v1alpha1.SavedCheck) *v1alpha1.CheckRun {
		return &v1alpha1.CheckRun{CheckDigest: c.Digest, Result: v1alpha1.CheckRunFail, Token: "b-tok", Seq: 11,
			AssertionStep: 1, AssertionID: "marker version", Tree: v1alpha1.TreeBase}
	}, check)
	e.settleReviews(name)
	if len(r.handoffs) != 1 {
		t.Fatalf("check reviews = %d, want one", len(r.handoffs))
	}
	h := r.handoffs[0]
	if w := h.Workspace; w == nil || w.Surface != "cli" || w.Binary != "/work/bin/app" || w.BaseUnbuildable ||
		len(h.Components) != 0 {
		t.Fatalf("review workspace = %+v, components %+v", h.Workspace, h.Components)
	}
	it := h.Reviews[0]
	if it.Head == nil || it.Head.Result != "pass" || it.Head.Assertion != "" || it.Base == nil ||
		it.Base.Result != "fail" || it.Base.Assertion != "marker version" {
		t.Fatalf("review runs = head %+v, base %+v", it.Head, it.Base)
	}
	if _, stored := e.checkStore(name); len(stored) != 1 || stored[0].Digest != check.Digest {
		t.Fatalf("store = %+v, want the proven run check", stored)
	}
}

// TestAgentsCheckReviewBaseCause (S2-20b): the reviewer is told why a
// base run errored when maul recorded a cause for it.
func TestAgentsCheckReviewBaseCause(t *testing.T) {
	e, name := newAgentsEnv(t, repoImage)
	check := savedCheck(t, agentsVersionRun)
	r := &fakeReviewer{}
	e.reviewChecksWith(r)
	pv := e.agentsEvaluated(name)
	e.agentsConfirm(pv.Name, false, greenAtHead, func(c v1alpha1.SavedCheck) *v1alpha1.CheckRun {
		return &v1alpha1.CheckRun{CheckDigest: c.Digest, Result: v1alpha1.CheckRunError, Token: "b-tok", Seq: 11,
			Tree: v1alpha1.TreeBase, Cause: v1alpha1.CheckRunCauseCompile}
	}, check)
	e.settleReviews(name)
	if len(r.handoffs) != 1 {
		t.Fatalf("check reviews = %d, want one", len(r.handoffs))
	}
	if b := r.handoffs[0].Reviews[0].Base; b == nil || b.Result != "error" || b.Cause != "compile" {
		t.Fatalf("base run = %+v, want an error with cause compile", b)
	}
}

// TestAgentsCheckReviewUnbuildableBase: with a base that did not build the
// reviewer is told so, and no base run is handed over.
func TestAgentsCheckReviewUnbuildableBase(t *testing.T) {
	e, name := newAgentsEnv(t, repoImage)
	check := savedCheck(t, agentsVersionRun)
	r := &fakeReviewer{}
	e.reviewChecksWith(r)
	pv := e.agentsEvaluated(name)
	e.agentsConfirm(pv.Name, true, greenAtHead, func(v1alpha1.SavedCheck) *v1alpha1.CheckRun { return nil }, check)
	e.settleReviews(name)
	if len(r.handoffs) != 1 || r.handoffs[0].Workspace == nil || !r.handoffs[0].Workspace.BaseUnbuildable ||
		r.handoffs[0].Reviews[0].Base != nil {
		t.Fatalf("review handoffs = %+v", r.handoffs)
	}
}

// TestAgentsRedReviewNeedsItsAssertion: a check confirmed red is handed
// to the reviewer only with the assertion its runs failed; one confirmed
// red with none is never sent (a bare exit code never counts, SPEC-S2).
func TestAgentsRedReviewNeedsItsAssertion(t *testing.T) {
	for _, assertion := range []string{"marker version", ""} {
		t.Run("assertion "+assertion, func(t *testing.T) {
			e, name := newAgentsEnv(t, repoImage)
			check := savedCheck(t, agentsVersionRun)
			check.Expect, check.FailedStep, check.FailedAssertion = v1alpha1.CheckExpectFail, 1, assertion
			r := &fakeReviewer{}
			e.reviewChecksWith(r)
			pv := e.agentsEvaluated(name)
			red := func(c v1alpha1.SavedCheck) v1alpha1.CheckRun {
				return v1alpha1.CheckRun{CheckDigest: c.Digest, Result: v1alpha1.CheckRunFail, Token: "cf-tok", Seq: 10,
					DryRunSeq: 1, Expect: v1alpha1.CheckExpectFail, AssertionStep: 1, AssertionID: assertion}
			}
			e.agentsConfirm(pv.Name, false, red, func(v1alpha1.SavedCheck) *v1alpha1.CheckRun { return nil }, check)
			e.settleReviews(name)
			sent := len(r.handoffs) == 1 && len(r.handoffs[0].Reviews) == 1
			if sent != (assertion != "") {
				t.Fatalf("assertion %q: review handoffs %+v", assertion, r.handoffs)
			}
			if sent {
				it := r.handoffs[0].Reviews[0]
				if it.FailedAssertion != assertion || it.Head == nil || it.Head.Assertion != assertion {
					t.Errorf("red review = %+v head %+v", it, it.Head)
				}
			}
		})
	}
}

// settleTools plays the agents host settling pv with phase and reason.
func (e *env) settleTools(pvName string, phase v1alpha1.PreviewVerificationPhase,
	reason v1alpha1.PreviewVerificationReason) {
	e.t.Helper()
	pv := e.pvNamed(pvName)
	pv.Status.Phase, pv.Status.Reason = phase, reason
	if err := e.c.Update(context.Background(), pv); err != nil {
		e.t.Fatal(err)
	}
}

// TestAgentsHeadBuildFailedIsUnconfirmed (S2-20): a tool pod that cannot
// build the head CI built green is maul's Infra: the round is retried
// within its attempts, never judged and never fixed; once they are spent
// the verification is Unconfirmed for that reason (ToolsBuildFailed), and
// the hand-off says maul's own build did not finish.
func TestAgentsHeadBuildFailedIsUnconfirmed(t *testing.T) {
	e, name := newAgentsEnv(t, repoImage)
	e.reviewReady(name, 4)
	for attempt := int32(1); attempt <= v1alpha1.MaxVerificationAttempts; attempt++ {
		v := e.verification(name)
		if v.Ref == nil || v.Attempt != attempt {
			t.Fatalf("attempt %d: verification = %+v", attempt, v)
		}
		e.settleTools(v.Ref.Name, v1alpha1.VerificationInfra, v1alpha1.VerificationReasonToolsBuildFailed)
		e.reviewReady(name, 3)
	}
	v := e.verification(name)
	if v.State != v1alpha1.VerificationUnconfirmed ||
		v.UnavailableReason != v1alpha1.VerificationUnavailableToolsBuildFailed ||
		v.InfraRetries != v1alpha1.MaxVerificationAttempts-1 || len(v.Outcomes) != 0 {
		t.Fatalf("verification = %+v, want Unconfirmed for the head build after every attempt", v)
	}
	if runs := e.runsOf(name, v1alpha1.IntentStageRevise); len(runs) != 0 {
		t.Fatalf("revise runs = %d: a build failure started a fix", len(runs))
	}
	if h := e.prVerifyPass(name).handoffView(); h == nil || h.State != templates.HandoffUnconfirmed ||
		h.Why != templates.HandoffWhyBuildFailed {
		t.Fatalf("hand-off = %+v, want unconfirmed for the build", h)
	}
}

// TestAgentsUnconfirmedReasonsAreRecorded (S2-20, from S2-14): a round that
// cannot run for want of an accepted image is Unconfirmed, and one whose
// trees cannot be fetched Unavailable, each recording why, which the
// hand-off then says.
func TestAgentsUnconfirmedReasonsAreRecorded(t *testing.T) {
	e, name := newAgentsEnv(t, "", func(p *v1alpha1.Project) { p.Spec.RequireRepositoryImage = new(false) })
	e.reviewReady(name, 4)
	v := e.verification(name)
	if v.State != v1alpha1.VerificationUnconfirmed ||
		v.UnavailableReason != v1alpha1.VerificationUnavailableSandboxImage {
		t.Fatalf("verification = %+v, want Unconfirmed for the image", v)
	}
	if h := e.prVerifyPass(name).handoffView(); h == nil || h.Why != templates.HandoffWhySandboxImage {
		t.Errorf("hand-off = %+v, want the image's reason", h)
	}

	e, name = newAgentsEnv(t, repoImage)
	e.review(name, 3)
	e.clock.Advance(treeWaitMax)
	e.review(name, 1)
	v = e.verification(name)
	if v.State != v1alpha1.VerificationUnavailable || v.UnavailableReason != v1alpha1.VerificationUnavailableTree {
		t.Fatalf("verification = %+v, want Unavailable for the trees", v)
	}
	e.review(name, 1)
	if h := e.prVerifyPass(name).handoffView(); h == nil || h.Why != templates.HandoffWhyTree {
		t.Errorf("hand-off = %+v, want the trees' reason", h)
	}
}

// TestAgentsBuildHandoffNeverReferencesTheToolPod extends
// TestBuildHandoffNeverReferencesEvaluator to an agents round (S2-20): a
// reproduced failure of a run check starts a verify-fix round, and neither
// its revise handoff nor any build's names the tool pod (its -tools Job
// or Secret), carries its key, or shows a base run.
func TestAgentsBuildHandoffNeverReferencesTheToolPod(t *testing.T) {
	e, name := newAgentsEnv(t, repoImage)
	e.evaluateReporting(failingReport)
	check := savedCheck(t, agentsVersionRun)
	e.storeChecks(name, check)
	pv := e.agentsEvaluated(name)
	e.replayed(pv.Name, v1alpha1.CheckRun{CheckDigest: check.Digest, Result: v1alpha1.CheckRunFail, Token: "t1",
		Seq: 2, AssertionStep: 1, AssertionID: "marker version"})
	cur := e.pvNamed(pv.Name)
	cur.Status.Base = &v1alpha1.BaseStatus{Required: true, Runs: []v1alpha1.CheckRun{{CheckDigest: check.Digest,
		Result: v1alpha1.CheckRunFail, Token: "base-sentinel-tok", Seq: 4, AssertionStep: 1,
		AssertionID: "marker base-sentinel", Tree: v1alpha1.TreeBase}}}
	if err := e.c.Update(context.Background(), cur); err != nil {
		t.Fatal(err)
	}
	e.settleReviews(name)
	if v := e.verification(name); v.State != v1alpha1.VerificationFailing {
		t.Fatalf("verification = %s %+v, want Failing on the run check's assertion", v.State, v.Outcomes)
	}
	all := e.fixInput(name)
	// The reproduction is in the workspace's words and shows the run
	// check's steps (S2-20b).
	for _, want := range []string{"against maul's builds of these commits", "against the pull request's build",
		"Steps to reproduce (a Go program",
		"Write the file `main.go`", "Expected: exit status `0`"} {
		if !strings.Contains(all, want) {
			t.Errorf("the reproduction lacks %q:\n%s", want, all)
		}
	}
	for _, not := range []string{"the preview of these commits", "against that preview", "cannot show"} {
		if strings.Contains(all, not) {
			t.Errorf("the reproduction says %q:\n%s", not, all)
		}
	}
	for _, run := range e.runsOf(name, v1alpha1.IntentStageBuild) {
		var cm corev1.ConfigMap
		if err := e.c.Get(context.Background(), client.ObjectKey{Namespace: testNS, Name: run.Spec.Inputs.ConfigMap},
			&cm); err == nil {
			all += joinedData(cm.Data)
		}
	}
	for _, f := range []string{jobs.ToolsSecretSuffix + "\"", agentshost.JobName(&pv) + jobs.ToolsSecretSuffix,
		toolsKey, "base-sentinel", "base-sentinel-tok", check.Digest} {
		if strings.Contains(all, f) {
			t.Errorf("a build or revise handoff carries %q", f)
		}
	}
}
