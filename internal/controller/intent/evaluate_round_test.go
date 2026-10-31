// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/agentrun"
	"github.com/devthenet-labs/maul/internal/envelope"
	"github.com/devthenet-labs/maul/internal/ghclient"
	"github.com/devthenet-labs/maul/internal/jobs"
)

// evaluationReport is an evaluator's report on outcomesPlan's outcomes.
const evaluationReport = `---
verdicts:
  - id: version-shown
    verdict: pass
    ledger: [3]
  - id: still-healthy
    verdict: pass
    ledger: [4]
---
Checked both outcomes against the preview.
`

// toolPodIP is the slot tool pod's address the fake preview-controller
// writes.
const toolPodIP = "10.42.0.7"

// evaluatorOn switches the agent evaluator on in every reconciler, with the
// ceilings and models serve sets under --intent-evaluator, and makes the
// fake Jobs answer an evaluate run with an evaluation of cost dollars. As
// serve requires, verification and previews are on with it.
func (e *env) evaluatorOn(cost float64) {
	e.verifying(true, true)
	for _, s := range []*Settings{&e.project.Settings, &e.intent.Settings, &e.runs.Settings} {
		s.Evaluator = true
		s.Evaluate = StageCeiling{MaxTurns: DefaultEvaluateMaxTurns, TokenBudget: DefaultEvaluateTokenBudget,
			Timeout: DefaultEvaluateTimeout}
		s.CheckReview = StageCeiling{MaxTurns: DefaultCheckReviewMaxTurns,
			TokenBudget: DefaultCheckReviewTokenBudget, Timeout: DefaultCheckReviewTimeout}
		s.EvaluateModel, s.CheckReviewModel = "anthropic/claude-sonnet-5", "anthropic/claude-haiku-5"
		s.DefaultHandoff, s.HandoffMax = v1alpha1.HandoffGated, time.Hour
		s.EvaluationCostMax, s.MaxToolCalls = 6000000, 300
	}
	plan := e.jobs.output
	e.jobs.output = func(spec jobs.Spec) jobs.RunOutput {
		if spec.Phase == "evaluate" {
			return evaluationOutput(envelope.Stage{Outcome: envelope.OutcomeOK, Harness: "claude", Model: spec.Model,
				Usage: envelope.Usage{InputTokens: 10, OutputTokens: 20, CostUSD: cost}}, evaluationReport)
		}
		return plan(spec)
	}
}

// evaluationOutput is an evaluate Job's output: one evaluation event.
func evaluationOutput(stage envelope.Stage, report string) jobs.RunOutput {
	return jobs.RunOutput{Events: []envelope.Event{{V: envelope.Version, Type: envelope.TypeEvaluation,
		Evaluation: &envelope.Evaluation{Stage: stage, ReportMarkdown: report}}}}
}

// newEvaluatorEnv is newVerifyEnv over outcomesPlan with the evaluator on,
// the app's pull request changing two files, and an intent driven into
// review with its Preview live: it returns the intent's name.
func newEvaluatorEnv(t *testing.T, cost float64, mutate ...func(*v1alpha1.Project)) (*env, string) {
	t.Helper()
	e := newVerifyEnv(t, outcomesPlan, mutate...)
	e.evaluatorOn(cost)
	name := e.reviewing()
	e.gh.mu.Lock()
	for _, pr := range e.gh.prs {
		pr.files = []ghclient.PullRequestFile{{Path: "version.go", Additions: 12, Deletions: 1},
			{Path: "main.go", Additions: 2}}
	}
	e.gh.mu.Unlock()
	e.readyPreview(name)
	return e, name
}

// pvNamed reads PreviewVerification name.
func (e *env) pvNamed(name string) *v1alpha1.PreviewVerification {
	e.t.Helper()
	var pv v1alpha1.PreviewVerification
	if err := e.c.Get(context.Background(), client.ObjectKey{Namespace: testNS, Name: name}, &pv); err != nil {
		e.t.Fatal(err)
	}
	return &pv
}

// playTools plays preview-controller on an outcome verification: phase,
// and the tool pod's status when ip is set (ready as given).
func (e *env) playTools(name string, phase v1alpha1.PreviewVerificationPhase, ip string, ready bool) {
	e.t.Helper()
	pv := e.pvNamed(name)
	pv.Status.Phase = phase
	pv.Status.Tools = nil
	if ip != "" {
		pv.Status.Tools = &v1alpha1.ToolsStatus{PodIP: ip, Port: jobs.ToolsPort, Ready: ready}
	}
	if phase == v1alpha1.VerificationObserved {
		pv.Status.Ledger = &v1alpha1.LedgerSummary{Count: 5, SHA256: strings.Repeat("a", 64)}
	}
	// PreviewVerification carries no status subresource in the fake client.
	if err := e.c.Update(context.Background(), pv); err != nil {
		e.t.Fatal(err)
	}
}

// TestEvaluatorCreatedAfterToolsStatus: the evaluation round creates its
// outcome PreviewVerification at the kept gate and, while its status.tools
// is empty (the tool pod not scheduled, or not ready), no evaluate run and
// no Job; once preview-controller writes the tool pod's address, the
// evaluate run is created pinning it, and its Job is an evaluator whose
// hostAliases hold that address, labelled with the verification's UID, on
// the investigate stage's keys, handed a strict evaluate handoff.
func TestEvaluatorCreatedAfterToolsStatus(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.review(name, 3)
	pv := e.onlyVerification(name)
	if len(pv.Spec.Outcomes) != 3 || pv.Spec.OutcomesDigest == "" || len(pv.Spec.Criteria) != 0 ||
		pv.Spec.Rung != v1alpha1.RungFull || pv.Spec.Tools == nil || pv.Spec.Tools.MaxToolCalls != 300 ||
		pv.Spec.Tools.Browser || !pv.Spec.Tools.Sandbox || pv.Spec.Timeout.Duration != 25*time.Minute {
		t.Fatalf("verification spec = %+v", pv.Spec)
	}
	if v := e.verification(name); v.State != v1alpha1.VerificationEvaluating || v.Ref == nil || v.Ref.UID != pv.UID {
		t.Fatalf("verification = %+v, want Evaluating on %s", v, pv.Name)
	}

	noEvaluator := func(when string) {
		t.Helper()
		e.runRuns()
		if runs := e.runsOf(name, v1alpha1.IntentStageEvaluate); len(runs) != 0 {
			t.Fatalf("%s: evaluate runs = %d, want none before status.tools", when, len(runs))
		}
		if n := len(e.specsOf("evaluate")); n != 0 {
			t.Fatalf("%s: %d evaluate Jobs, want none", when, n)
		}
	}
	noEvaluator("pending")
	e.playTools(pv.Name, v1alpha1.VerificationRunning, "", false)
	e.review(name, 2)
	noEvaluator("running, no tool pod")
	e.playTools(pv.Name, v1alpha1.VerificationRunning, toolPodIP, false)
	e.review(name, 2)
	noEvaluator("tool pod not ready")

	e.playTools(pv.Name, v1alpha1.VerificationRunning, toolPodIP, true)
	e.review(name, 1)
	e.checkEvaluateRun(name, &pv)
	e.runRuns()
	e.checkEvaluatorJob(&pv)
}

// checkEvaluateRun: intent name has one evaluate run, pinning pv and its
// tool pod's address, and the approved plan.
func (e *env) checkEvaluateRun(name string, pv *v1alpha1.PreviewVerification) {
	t := e.t
	t.Helper()
	runs := e.runsOf(name, v1alpha1.IntentStageEvaluate)
	if len(runs) != 1 {
		t.Fatalf("evaluate runs = %d, want one", len(runs))
	}
	run := runs[0]
	if ev := run.Spec.Evaluation; ev == nil || ev.UID != pv.UID || ev.ToolIP != toolPodIP ||
		ev.Verification != pv.Name || ev.Rung != v1alpha1.RungFull {
		t.Fatalf("run evaluation = %+v", run.Spec.Evaluation)
	}
	if run.Name != v1alpha1.IntentRunName(name, v1alpha1.IntentStageEvaluate, 1, "", 1) ||
		run.Spec.Inputs.PlanDigest != e.get(name).Status.Approval.PlanDigest {
		t.Errorf("run %s pins plan %s", run.Name, run.Spec.Inputs.PlanDigest)
	}
}

// checkEvaluatorJob: one evaluate Job was launched, an evaluator of pv's
// tool pod, on the investigate stage's keys, handed its handoff.
func (e *env) checkEvaluatorJob(pv *v1alpha1.PreviewVerification) {
	t := e.t
	t.Helper()
	evals := e.specsOf("evaluate")
	if len(evals) != 1 {
		t.Fatalf("evaluate Jobs = %d, want one", len(evals))
	}
	spec := evals[0]
	if spec.Role != v1alpha1.RoleEvaluator || spec.Evaluation != string(pv.UID) ||
		spec.ToolHost == nil || spec.ToolHost.IP != toolPodIP || spec.ToolHost.Name != jobs.ToolsHostName ||
		spec.ArtifactURL != "" || spec.RunnerImage != "" || spec.Model != "anthropic/claude-sonnet-5" {
		t.Fatalf("evaluate Job spec = %+v", spec)
	}
	env := e.jobs.envs[jobs.NameFor(spec.Finding, spec.Kind, int32(spec.Attempt))]
	if env["MAUL_INVESTIGATE_TIMEOUT"] != "20m0s" || env["MAUL_INVESTIGATE_MAX_TURNS"] != "100" ||
		env["MAUL_REMEDIATE_TIMEOUT"] != "" {
		t.Errorf("evaluate Job env = %v, want the investigate stage's keys alone", env)
	}
	checkEvaluateHandoff(t, spec.IssueMarkdown, pv)
}

// checkEvaluateHandoff: the Job's handoff is strict JSON agent-runner
// decodes: the round and its token, the request, the approved outcomes,
// the previewed component, the tools and the changed paths with counts.
func checkEvaluateHandoff(t *testing.T, raw string, pv *v1alpha1.PreviewVerification) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.DisallowUnknownFields()
	var h agentrun.EvaluateHandoff
	if err := dec.Decode(&h); err != nil {
		t.Fatalf("handoff %s: %v", raw, err)
	}
	if h.Round != 1 || h.RunToken != "ev1a1-"+pv.Spec.Run || len(h.Outcomes) != 3 ||
		h.Outcomes[0].ID != "version-shown" || !h.Outcomes[2].Manual {
		t.Errorf("handoff = %+v", h)
	}
	if len(h.Components) != 1 || h.Components[0] != (agentrun.EvaluateComponent{Key: "app", Path: "/"}) {
		t.Errorf("components = %+v", h.Components)
	}
	if h.Tools.MaxToolCalls != 300 || !h.Tools.Sandbox || h.Tools.LookAroundRoutes != 8 {
		t.Errorf("tools = %+v", h.Tools)
	}
	want := []agentrun.ChangedPath{{Path: "version.go", Added: 12, Deleted: 1}, {Path: "main.go", Added: 2}}
	if len(h.ChangedPaths) != len(want) || h.ChangedPaths[0] != want[0] || h.ChangedPaths[1] != want[1] {
		t.Errorf("changed paths = %+v, want %+v", h.ChangedPaths, want)
	}
	if !strings.Contains(h.Request, "Please add GET /version.") {
		t.Errorf("request = %q, want the issue", h.Request)
	}
}

// TestEvaluationRoundCollects: the evaluator's report is kept on its run,
// and once preview-controller settles the verification the round is judged,
// both in hand: until maul judges the facts every outcome is unsure, the
// verification Unconfirmed, and nothing more is started at those heads.
func TestEvaluationRoundCollects(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.review(name, 3)
	pv := e.onlyVerification(name)
	e.playTools(pv.Name, v1alpha1.VerificationRunning, toolPodIP, true)
	e.review(name, 1)
	e.runRuns()
	e.runRuns()
	run := e.runsOf(name, v1alpha1.IntentStageEvaluate)[0]
	if run.Status.Phase != v1alpha1.RunComplete || run.Status.Report != evaluationReport {
		t.Fatalf("evaluate run = %s %s %q", run.Status.Phase, run.Status.Outcome, run.Status.Detail)
	}
	e.review(name, 2)
	if v := e.verification(name); v.State != v1alpha1.VerificationEvaluating {
		t.Fatalf("state = %s before the verification settled, want Evaluating", v.State)
	}

	e.playTools(pv.Name, v1alpha1.VerificationObserved, toolPodIP, true)
	e.review(name, 3)
	v := e.verification(name)
	if v.State != v1alpha1.VerificationUnconfirmed || len(v.Outcomes) != 3 {
		t.Fatalf("verification = %+v, want Unconfirmed with every outcome", v)
	}
	for _, o := range v.Outcomes {
		if o.Result != v1alpha1.OutcomeUnsure {
			t.Errorf("outcome %s = %s, never passed before maul judges it", o.ID, o.Result)
		}
	}
	if v.EvaluationCostMicroUSD != 400000 {
		t.Errorf("evaluation cost = %d, want the evaluate run's 400000", v.EvaluationCostMicroUSD)
	}
	if n := len(e.verifications(name)); n != 1 {
		t.Errorf("verifications = %d, want the one round", n)
	}
}

// TestEvaluationCeilingSparesTheFixBudget: the evaluation's spend counts
// against its own ceiling only. An evaluation dearer than the Project's
// shared ceiling neither blocks the intent nor stops a revise round's lease;
// once the evaluation ceiling is spent, a requested round starts nothing
// (Unconfirmed, the request consumed) and the intent is still not blocked.
func TestEvaluationCeilingSparesTheFixBudget(t *testing.T) {
	e, name := newEvaluatorEnv(t, 2.0, func(p *v1alpha1.Project) { p.Spec.Limits.MaxCostMicroUSD = 1000000 })
	e.review(name, 3)
	pv := e.onlyVerification(name)
	e.playTools(pv.Name, v1alpha1.VerificationRunning, toolPodIP, true)
	e.review(name, 1)
	e.runRuns()
	e.runRuns()
	e.playTools(pv.Name, v1alpha1.VerificationObserved, toolPodIP, true)
	e.review(name, 3)

	in := e.get(name)
	if in.Status.Phase != v1alpha1.IntentInReview ||
		meta.IsStatusConditionTrue(in.Status.Conditions, v1alpha1.ConditionBudgetExhausted) {
		t.Fatalf("phase %s conditions %+v: an evaluation's spend blocked the intent", in.Status.Phase,
			in.Status.Conditions)
	}
	p := &pass{r: e.intent, set: e.intent.Settings, in: in, proj: e.project0(), now: e.clock.Now()}
	p.runs = e.runPtrs(name)
	if p.atCeiling() || p.evaluationSpend() != 2000000 || p.sharedSpend() >= 1000000 {
		t.Fatalf("shared %d at ceiling %v, evaluation %d", p.sharedSpend(), p.atCeiling(), p.evaluationSpend())
	}

	// The evaluation ceiling, lowered under what was spent: a request
	// starts nothing.
	proj := e.project0()
	proj.Spec.Limits.MaxEvaluateCostMicroUSD = new(int64(1000000))
	if err := e.c.Update(context.Background(), proj); err != nil {
		t.Fatal(err)
	}
	e.setStatus(name, func(in *v1alpha1.Intent) {
		in.Status.Verification.Requested = &v1alpha1.VerifyRequest{By: approver, ByID: 7,
			At: metav1.NewTime(e.clock.Now()), CommentID: 99}
	})
	e.clock.Advance(10 * time.Minute)
	e.review(name, 3)
	v := e.verification(name)
	if v.State != v1alpha1.VerificationUnconfirmed || v.Requested != nil {
		t.Fatalf("verification = %+v, want Unconfirmed with the request consumed", v)
	}
	if n := len(e.verifications(name)); n != 1 {
		t.Errorf("verifications = %d, want no round past the evaluation ceiling", n)
	}
	if in := e.get(name); in.Status.Phase != v1alpha1.IntentInReview {
		t.Errorf("phase = %s, want review to stay open", in.Status.Phase)
	}
}

// TestStaleEvaluationNeverLaunchesEvaluator: an evaluate run whose
// verification is gone, replaced or no longer the intent's by the time its
// slot is granted settles without a Job.
func TestStaleEvaluationNeverLaunchesEvaluator(t *testing.T) {
	for _, tt := range []struct {
		name  string
		stale func(e *env, pv *v1alpha1.PreviewVerification)
	}{
		{"the verification deleted", func(e *env, pv *v1alpha1.PreviewVerification) {
			if err := e.c.Delete(context.Background(), pv); err != nil {
				e.t.Fatal(err)
			}
		}},
		{"the verification settled", func(e *env, pv *v1alpha1.PreviewVerification) {
			e.playTools(pv.Name, v1alpha1.VerificationStale, toolPodIP, true)
		}},
		{"the heads moved", func(e *env, pv *v1alpha1.PreviewVerification) {
			e.setStatus(pv.Spec.IntentRef.Name, func(in *v1alpha1.Intent) {
				in.Status.Verification.HeadSetDigest = "sha256:" + strings.Repeat("0", 64)
			})
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e, name := newEvaluatorEnv(t, 0.4)
			e.review(name, 3)
			pv := e.onlyVerification(name)
			e.playTools(pv.Name, v1alpha1.VerificationRunning, toolPodIP, true)
			e.review(name, 1)
			if len(e.runsOf(name, v1alpha1.IntentStageEvaluate)) != 1 {
				t.Fatal("no evaluate run")
			}
			tt.stale(e, e.pvNamed(pv.Name))
			e.runRuns()
			if n := len(e.specsOf("evaluate")); n != 0 {
				t.Fatalf("%d evaluate Jobs launched on a stale verification", n)
			}
			run := e.runsOf(name, v1alpha1.IntentStageEvaluate)[0]
			if run.Status.Phase != v1alpha1.RunFailed || !strings.Contains(run.Status.Detail, "no longer current") {
				t.Errorf("run = %s %s %q, want it settled unlaunched", run.Status.Phase, run.Status.Outcome,
					run.Status.Detail)
			}
		})
	}
}

// TestEvaluationRetryIsANewVerification: maul's own failure (the tool pod
// never ready), or an evaluator that failed, starts the round's next
// attempt as a new PreviewVerification (the tool pod takes one finish).
func TestEvaluationRetryIsANewVerification(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.review(name, 3)
	first := e.onlyVerification(name)
	e.playTools(first.Name, v1alpha1.VerificationInfra, "", false)
	e.review(name, 3)
	pvs := e.verifications(name)
	if len(pvs) != 2 || pvs[1].Name != v1alpha1.PreviewVerificationName(name, 1, 2) {
		t.Fatalf("verifications = %d, want attempt 2 as its own", len(pvs))
	}
	if v := e.verification(name); v.State != v1alpha1.VerificationEvaluating || v.Attempt != 2 ||
		v.Ref.UID != pvs[1].UID {
		t.Fatalf("verification = %+v", v)
	}

	// The evaluator fails: its tool pod is dropped, and attempt 3 is due.
	e.jobs.output = func(spec jobs.Spec) jobs.RunOutput {
		return evaluationOutput(envelope.Stage{Outcome: envelope.OutcomeRuntimeError, Detail: "crashed"}, "")
	}
	e.playTools(pvs[1].Name, v1alpha1.VerificationRunning, toolPodIP, true)
	e.review(name, 1)
	e.runRuns()
	e.runRuns()
	e.review(name, 3)
	pvs = e.verifications(name)
	if v := e.verification(name); v.Attempt != 3 || len(pvs) < 2 {
		t.Fatalf("verification = %+v after a failed evaluator; verifications %d", v, len(pvs))
	}
}

// TestCodexProjectWithoutEvaluateCodex: a codex Project's intents are not
// evaluated without --intent-evaluate-codex: no verification, state Off,
// the hand-off released early as evaluator-unverified, and the Project says
// why; with it on, the round runs.
func TestCodexProjectWithoutEvaluateCodex(t *testing.T) {
	codex := func(p *v1alpha1.Project) { p.Spec.Harness = v1alpha1.ProjectHarnessCodex }
	e := newVerifyEnv(t, outcomesPlan, codex)
	e.withCodex(codexHarness)
	e.evaluatorOn(0.4)
	name := e.reviewing()
	e.readyPreview(name)
	e.review(name, 3)
	if n := len(e.verifications(name)); n != 0 {
		t.Fatalf("verifications = %d, want none for an unverified evaluator", n)
	}
	v := e.verification(name)
	if v.State != v1alpha1.VerificationOff || v.Handoff == nil || !v.Handoff.Released ||
		v.Handoff.Mode != v1alpha1.HandoffEarly || v.Handoff.Reason != v1alpha1.HandoffReasonEvaluatorUnverified {
		t.Fatalf("verification = %+v, handoff %+v", v, v.Handoff)
	}
	e.reconcileProject()
	c := meta.FindStatusCondition(e.project0().Status.Conditions, v1alpha1.ConditionEvaluatorUnverified)
	if c == nil || c.Status != "True" {
		t.Fatalf("EvaluatorUnverified = %+v", c)
	}

	for _, s := range []*Settings{&e.project.Settings, &e.intent.Settings, &e.runs.Settings} {
		s.EvaluateCodex = true
	}
	e.reconcileProject()
	if c := meta.FindStatusCondition(e.project0().Status.Conditions,
		v1alpha1.ConditionEvaluatorUnverified); c != nil {
		t.Errorf("EvaluatorUnverified = %+v with the codex evaluator on", c)
	}
	e.setStatus(name, func(in *v1alpha1.Intent) { in.Status.Verification.HeadSetDigest = "" })
	e.review(name, 3)
	if n := len(e.verifications(name)); n != 1 {
		t.Fatalf("verifications = %d with the codex evaluator on, want the round", n)
	}
}

// project0 reads the test Project.
func (e *env) project0() *v1alpha1.Project {
	e.t.Helper()
	var p v1alpha1.Project
	if err := e.c.Get(context.Background(), client.ObjectKey{Namespace: testNS, Name: "target"}, &p); err != nil {
		e.t.Fatal(err)
	}
	return &p
}

// runPtrs are the intent's runs as a pass holds them.
func (e *env) runPtrs(name string) []*v1alpha1.IntentRun {
	runs := e.intentRuns(name)
	out := make([]*v1alpha1.IntentRun, len(runs))
	for i := range runs {
		out[i] = &runs[i]
	}
	return out
}

// TestRoleHandoffBounds: the evaluate handoff drops changed paths to fit
// agent-runner's bound and refuses one that cannot fit; the check review
// handoff is refused empty or over its bound, never cut.
func TestRoleHandoffBounds(t *testing.T) {
	h := agentrun.EvaluateHandoff{Round: 1, RunToken: "ev1a1-abcdef"}
	for range 4000 {
		h.ChangedPaths = append(h.ChangedPaths, agentrun.ChangedPath{Path: strings.Repeat("p", 200), Added: 1})
	}
	raw, err := encodeEvaluateHandoff(h)
	if err != nil || len(raw) > evaluateHandoffMaxBytes {
		t.Fatalf("encode = %d bytes, %v", len(raw), err)
	}
	h.ChangedPaths, h.Request = nil, strings.Repeat("r", evaluateHandoffMaxBytes)
	if _, err := encodeEvaluateHandoff(h); err == nil {
		t.Error("an evaluate handoff over its bound with nothing to drop was encoded")
	}
	if _, err := encodeCheckReviewHandoff(nil); err == nil {
		t.Error("an empty check review handoff was encoded")
	}
	big := make([]agentrun.CheckReviewItem, 0, 100)
	for range 100 {
		big = append(big, agentrun.CheckReviewItem{Check: strings.Repeat("c", 8<<10)})
	}
	if _, err := encodeCheckReviewHandoff(big); err == nil {
		t.Error("a check review handoff over its bound was encoded")
	}
	one, err := encodeCheckReviewHandoff([]agentrun.CheckReviewItem{{CheckDigest: "sha256:x", Outcome: "o"}})
	if err != nil || !strings.HasPrefix(one, `{"reviews":[`) {
		t.Errorf("encode = %s, %v", one, err)
	}
}

// TestReplayRoundRunsNoEvaluator: with a disputed check awaiting its
// model-free round and checks in the store, the round replays them (rung
// replay, the store's checks inline under its digest) and is judged once
// the verification settles, with no evaluate run at all. A store that is
// not what its ref records starts no round.
func TestReplayRoundRunsNoEvaluator(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	saved := []v1alpha1.SavedCheck{{ID: "version-shown", Outcome: "version-shown", Form: v1alpha1.CheckFormHTTP,
		Body: `{"id":"version-shown"}`, Digest: "sha256:" + strings.Repeat("b", 64)}}
	data, sum, err := encodeCheckStore(saved)
	if err != nil {
		t.Fatal(err)
	}
	in := e.get(name)
	store := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-checks-1", Namespace: testNS,
			Labels:          map[string]string{v1alpha1.LabelIntent: name},
			OwnerReferences: []metav1.OwnerReference{intentOwner(in)}},
		Data: map[string]string{keyCheckStore: data},
	}
	if err := e.c.Create(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	e.review(name, 2)
	e.setStatus(name, func(in *v1alpha1.Intent) {
		in.Status.Acceptance.Checks = &v1alpha1.CheckStoreRef{ConfigMap: store.Name, Digest: sum, Count: 1}
		in.Status.Verification.Disputed = []string{saved[0].Digest}
		in.Status.Verification.Ref, in.Status.Verification.Attempt = nil, 0
	})
	for _, pv := range e.verifications(name) {
		if err := e.c.Delete(context.Background(), &pv); err != nil {
			t.Fatal(err)
		}
	}
	e.review(name, 3)
	pvs := e.verifications(name)
	if len(pvs) != 1 || pvs[0].Spec.Rung != v1alpha1.RungReplay || pvs[0].Spec.ChecksDigest != sum ||
		len(pvs[0].Spec.Checks) != 1 || pvs[0].Spec.Checks[0] != saved[0] {
		t.Fatalf("verifications = %+v, want one replay of the stored check", pvs)
	}
	e.playTools(pvs[0].Name, v1alpha1.VerificationRunning, toolPodIP, true)
	e.review(name, 2)
	e.playTools(pvs[0].Name, v1alpha1.VerificationObserved, toolPodIP, true)
	e.review(name, 2)
	if runs := e.runsOf(name, v1alpha1.IntentStageEvaluate); len(runs) != 0 {
		t.Fatalf("evaluate runs = %d on a replay round, want none", len(runs))
	}
	if v := e.verification(name); v.State != v1alpha1.VerificationUnconfirmed {
		t.Fatalf("state = %s, want the replay round judged", v.State)
	}

	// A store whose bytes are not its recorded digest starts nothing.
	e.setStatus(name, func(in *v1alpha1.Intent) {
		in.Status.Acceptance.Checks.Digest = "sha256:" + strings.Repeat("c", 64)
		in.Status.Verification.Ref, in.Status.Verification.Attempt = nil, 0
	})
	e.review(name, 3)
	if n := len(e.verifications(name)); n != 1 {
		t.Errorf("verifications = %d, want none started over a store not on record", n)
	}
}
