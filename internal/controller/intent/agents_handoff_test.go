// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/controller/intent/agentshost"
	"github.com/devthenet-labs/maul/internal/jobs"
	"github.com/devthenet-labs/maul/internal/templates"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

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
		v.Cause != v1alpha1.SettleToolsBuildFailed ||
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
		v.Cause != v1alpha1.SettleSandboxImage {
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
	if v.State != v1alpha1.VerificationUnavailable || v.Cause != v1alpha1.SettleTree {
		t.Fatalf("verification = %+v, want Unavailable for the trees", v)
	}
	e.review(name, 1)
	if h := e.prVerifyPass(name).handoffView(); h == nil || h.Why != templates.HandoffWhyTree {
		t.Errorf("hand-off = %+v, want the trees' reason", h)
	}
}

// TestAgentsBuildHandoffNeverReferencesTheToolPod extends
// TestBuildHandoffCarriesOnlyCitedEvidence to an agents round (S2-20): a
// head run the evaluator's fail cites starts a verify-fix round, and
// neither its revise handoff nor any build's names the tool pod (its -tools
// Job or Secret), carries its key, or shows a call at the base: the base is
// exploration only, and never part of the path a fixer is shown.
func TestAgentsBuildHandoffNeverReferencesTheToolPod(t *testing.T) {
	e, name := newAgentsEnv(t, repoImage)
	e.evaluateReporting(failingReport)
	pv := e.agentsEvaluated(name)
	program := func(seq int64, side toolwire.Side, output string) toolwire.LedgerEntry {
		args, _ := json.Marshal(toolwire.RunRequest{Side: side, Language: "go",
			Files: []toolwire.File{{Name: "main.go", Content: []byte("package main")}},
			Args:  []string{string(side) + "-sentinel"}})
		sum := sha256.Sum256([]byte(output))
		return toolwire.LedgerEntry{Seq: seq, Kind: toolwire.KindAgent, Verb: toolwire.VerbRun, Side: side,
			Args: args, Exit: 1, Output: output, OutputSHA256: hex.EncodeToString(sum[:])}
	}
	e.playTools(pv.Name, v1alpha1.VerificationObserved, toolPodIP, true)
	e.ledgerOfEntries(pv.Name, program(1, toolwire.SideBase, "base-sentinel output"),
		program(2, "", "version: dev"), program(3, "", "healthy"))
	e.settleReviews(name)
	if v := e.verification(name); v.State != v1alpha1.VerificationFailing {
		t.Fatalf("verification = %s %+v, want Failing on the cited head run", v.State, v.Outcomes)
	}
	all := e.fixInput(name)
	// The reproduction is in the workspace's words and shows the cited run.
	for _, want := range []string{"against maul's builds of these commits", "Call 2, a Go program run",
		"version: dev"} {
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
		toolsKey, "base-sentinel"} {
		if strings.Contains(all, f) {
			t.Errorf("a build or revise handoff carries %q", f)
		}
	}
}
