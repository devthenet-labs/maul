// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package chart_test

import (
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/controller/intent"
	"github.com/devthenet-labs/maul/internal/kube"
)

// observedOldCRDs are the CRDs of 0.14.15, the last release that saved,
// reviewed and replayed checks (tag v0.14.15, deploy/kustomize/base/crds),
// frozen for the "evaluator observed" upgrade test. Each later release reads
// them from here, never from the tag.
const observedOldCRDs = "../../hack/testdata/crd-upgrade/0.14.15/"

// observedKinds are the CRDs the upgrade swaps, both ways.
var observedKinds = []string{"maul.run_intents.yaml", "maul.run_previewverifications.yaml",
	"maul.run_intentruns.yaml", "maul.run_projects.yaml"}

// The objects 0.14.15 leaves: one Project, one open Intent mid-replay, its
// two verifications (the full round it judged, the replay round running),
// and round 1's evaluate and checkreview runs.
const (
	obProject   = "beatbattle"
	obIntent    = "beatbattle-48"
	obFullPV    = "beatbattle-48-v1-a1"
	obReplayPV  = "beatbattle-48-v2-a1"
	obEvaluate  = "beatbattle-48-ev-r1-a1"
	obReview    = "beatbattle-48-cr-r1-a1"
	obFreshPV   = "beatbattle-48-v3-a1"
	obFreshEval = "beatbattle-48-ev-r3-a1"
)

// observedUpgrade is one API server moved from 0.14.15's CRDs to this
// release's and back.
type observedUpgrade struct {
	t     *testing.T
	admin client.Client
	// crds is a client of the CustomResourceDefinitions.
	crds client.Client
}

// TestObservedUpgradeAgainstAPIServer ("evaluator observed", W11): release N
// only adds optional fields (status.verification.judge, an outcome's cites,
// a ledger entry's side) and changes no rule, so what 0.14.15 left stays
// valid, and what N writes stays valid under 0.14.15's CRDs after a helm
// rollback. Against a real API server it installs 0.14.15's CRDs with what
// that release leaves (a Project choosing a checkReview model and limit, an
// Intent mid-replay with saved checks, reviews and the cost ladder, a replay
// and an agents full verification with saved checks, confirmations and a
// base side, evaluate and checkreview runs) and holds:
//
//   - forward, under N's CRDs: every object is admitted whole (a renamed
//     copy created and its status written, so no rule is ratcheted), takes
//     the writes N's controllers make (the supersede's withdrawal and fresh
//     round, the retired run settled Aborted, the replay round deleted, a
//     settled evaluate run, the Project's Ready), and keeps every retired
//     field the rollback reads (status.acceptance.checks above all);
//   - back, under 0.14.15's CRDs again: what N created (an observed
//     judgement with cites, an agents verification whose ledger names a
//     side, a fresh evaluate run) reads back with N's fields pruned, is
//     admitted whole, and takes 0.14.15's own writes;
//   - the Project stays valid and keeps Ready both ways (that its retired
//     checkReview fields never block Ready is
//     TestRetiredCheckReviewSpecNeverBlocksReady's);
//   - rolling forward again, the marker stays absent on an Intent 0.14.15
//     rewrote while rolled back (it stored the object pruned), so the
//     supersede runs once more, as designed. An Intent 0.14.15 never
//     rewrote keeps its marker in etcd and is not superseded again, which
//     is harmless: nothing check-era was written to it.
func TestObservedUpgradeAgainstAPIServer(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via mise run envtest")
	}
	u := startObservedUpgrade(t)
	u.leaveCheckEraBehind()

	u.applyCRDs(newCRDs)
	u.waitServed(true)
	u.forward()
	u.createAsN()

	u.applyCRDs(observedOldCRDs)
	u.waitServed(false)
	u.back()

	u.applyCRDs(newCRDs)
	u.waitServed(true)
	var in v1alpha1.Intent
	u.getTyped(obIntent, &in)
	if in.Status.Verification == nil || in.Status.Verification.Judge != "" {
		t.Errorf("rolled forward again, the Intent's verification = %+v; want the marker the rollback pruned "+
			"absent, so the supersede runs once more", in.Status.Verification)
	}
}

func startObservedUpgrade(t *testing.T) *observedUpgrade {
	t.Helper()
	paths := make([]string, 0, len(observedKinds))
	for _, f := range observedKinds {
		paths = append(paths, observedOldCRDs+f)
	}
	env := &envtest.Environment{CRDInstallOptions: envtest.CRDInstallOptions{Paths: paths}}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})
	admin, err := client.New(cfg, client.Options{Scheme: kube.Scheme()})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: egressRelease}}
	if err := admin.Create(t.Context(), ns); err != nil {
		t.Fatalf("namespace: %v", err)
	}
	scheme := runtime.NewScheme()
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	crds, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("crd client: %v", err)
	}
	return &observedUpgrade{t: t, admin: admin, crds: crds}
}

var (
	obNow     = metav1.NewTime(time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC))
	obDigestA = "sha256:" + strings.Repeat("a", 64)
	obDigestB = "sha256:" + strings.Repeat("b", 64)
	obDigestC = "sha256:" + strings.Repeat("c", 64)
	obHex     = strings.Repeat("a", 64)
	obHeads   = []v1alpha1.VerificationHead{{Component: "app", Repository: "https://github.com/acme/app",
		Revision: strings.Repeat("a", 40)}}
	obOutcomes = []v1alpha1.AcceptanceOutcome{{ID: "counts", Kind: v1alpha1.OutcomeKindChange,
		Outcome: "bbcount prints the number of beats.", From: "print the beat count"}}
)

// obProjectObject is a verifying Project as 0.14.15 left it: a checkReview
// model and limit chosen, Ready.
func obProjectObject(name string) *v1alpha1.Project {
	p := &v1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: egressRelease},
		Spec: v1alpha1.ProjectSpec{
			IntentRepository: "https://github.com/acme/intents",
			Approvers:        v1alpha1.ProjectApprovers{Logins: []string{"octocat"}},
			Repositories:     []v1alpha1.ProjectRepository{{Name: "app", URL: "https://github.com/acme/app"}},
			Verification:     &v1alpha1.ProjectVerification{Enabled: true},
			Models: &v1alpha1.ProjectModels{Evaluate: "anthropic/claude-opus-5-5",
				CheckReview: "anthropic/claude-opus-5-5"},
		},
	}
	p.Spec.Limits.CheckReview.MaxCostMicroUSD = 1000000
	p.Spec.Limits.Evaluate.MaxCostMicroUSD = 6000000
	return p
}

// obReady is the Ready condition intent-controller writes on a Project.
func obReady(generation int64) []metav1.Condition {
	return []metav1.Condition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "Ready",
		Message: "the Project is ready", LastTransitionTime: obNow, ObservedGeneration: generation}}
}

// obSavedCheck is one saved check as 0.14.15 stored it.
func obSavedCheck() v1alpha1.SavedCheck {
	return v1alpha1.SavedCheck{ID: "counts-1", Outcome: "counts", Form: v1alpha1.CheckFormRun,
		Body: `{"files":{"main.go":"package main"}}`, Digest: obDigestC}
}

// obCheckRun is one check run of a 0.14.15 replay, confirmation or base.
func obCheckRun(tree v1alpha1.TreeSide, seq int32) v1alpha1.CheckRun {
	return v1alpha1.CheckRun{CheckDigest: obDigestC, Result: v1alpha1.CheckRunPass, Token: "t0k3n", Seq: seq,
		Tree: tree}
}

// obFullVerification is round 1, an agents full round 0.14.15 settled
// Observed with saved checks, confirmations and a base side.
func obFullVerification(name string) *v1alpha1.PreviewVerification {
	v := agentsVerification(name)
	v.Spec.IntentRef = v1alpha1.ObjectReference{Name: obIntent, UID: "u-intent"}
	v.Spec.Checks, v.Spec.ChecksDigest = []v1alpha1.SavedCheck{obSavedCheck()}, obDigestC
	v.Status = v1alpha1.PreviewVerificationStatus{
		Phase: v1alpha1.VerificationObserved, StartedAt: &obNow, CompletedAt: &obNow,
		Ledger: &v1alpha1.LedgerSummary{Count: 3, SHA256: obHex, Entries: []v1alpha1.LedgerIndexEntry{
			{Seq: 1, Kind: v1alpha1.LedgerKindRelay, Verb: v1alpha1.LedgerVerbBuild, ResultSHA256: obHex},
			{Seq: 2, Kind: v1alpha1.LedgerKindRunner, Verb: v1alpha1.LedgerVerbCheckRun, ResultSHA256: obHex},
			{Seq: 3, Kind: v1alpha1.LedgerKindAgent, Verb: v1alpha1.LedgerVerbConfirm, ResultSHA256: obHex}}},
		Replay:        []v1alpha1.CheckRun{obCheckRun("", 2)},
		Confirmations: []v1alpha1.CheckRun{obCheckRun("", 3)},
		Confirmed:     []v1alpha1.SavedCheck{obSavedCheck()},
		Base: &v1alpha1.BaseStatus{Required: true,
			Runs: []v1alpha1.CheckRun{obCheckRun(v1alpha1.TreeBase, 4)}},
	}
	return v
}

// obReplayVerification is round 2, the replay round 0.14.15 started at the
// same heads, running.
func obReplayVerification(name string) *v1alpha1.PreviewVerification {
	v := obFullVerification(name)
	v.Spec.Round, v.Spec.Run, v.Spec.Rung = 2, "1b2c3d", v1alpha1.RungReplay
	v.Status = v1alpha1.PreviewVerificationStatus{Phase: v1alpha1.VerificationRunning, StartedAt: &obNow}
	return v
}

// obRun is a role run of the Intent's verification round, as
// intent-controller creates it.
func obRun(name string, stage v1alpha1.IntentStage, round int32, pv string) *v1alpha1.IntentRun {
	run := attributedIntentRun("x", nil)
	run.Name = name
	run.Spec.IntentRef = v1alpha1.ObjectReference{Name: obIntent, UID: "u-intent"}
	run.Spec.Stage, run.Spec.Round = stage, round
	run.Spec.Inputs.PlanRevision, run.Spec.Inputs.PlanDigest = 1, obDigestA
	if stage == v1alpha1.IntentStageEvaluate {
		run.Spec.Evaluation = &v1alpha1.IntentRunEvaluation{Verification: pv, UID: types.UID("u-" + pv), ToolIP: "10.0.0.7",
			Rung: v1alpha1.RungFull}
	}
	return run
}

// obIntentObject is the open Intent as 0.14.15 left it, mid-replay: its
// acceptance with a check store, reviews, a red clearance and a narrowed
// check, and its verification on round 2's replay rung with the ladder's
// records, a dispute, a suspected defect and a quarantined outcome.
func obIntentObject(name string) *v1alpha1.Intent {
	n := int64(48)
	if name != obIntent {
		n = 148
	}
	in := attributedIntent(n, 0)
	in.Name, in.Spec.Project = name, obProject
	in.Spec.Issue.Number = n
	in.Status = v1alpha1.IntentStatus{
		Phase: v1alpha1.IntentInReview,
		Acceptance: &v1alpha1.IntentAcceptance{PlanRevision: 1, Digest: obDigestA, Outcomes: obOutcomes,
			OutcomesDigest: obDigestB,
			Checks:         &v1alpha1.CheckStoreRef{ConfigMap: name + "-checks-1", Digest: obDigestC, Count: 1},
			Reviews: []v1alpha1.CheckReviewRecord{{CheckDigest: obDigestC, OutcomeDigest: obDigestB,
				Answer: v1alpha1.ReviewProves, Run: obReview}},
			RedCleared: []v1alpha1.RedClearance{{Digest: obDigestC, Verification: obFullPV}},
			Narrowed:   []v1alpha1.NarrowedCheck{{Digest: obDigestC, Verification: obFullPV}}},
		Verification: &v1alpha1.IntentVerification{
			State: v1alpha1.VerificationEvaluating, Round: 2, Attempt: 1,
			Ref:           &v1alpha1.ObjectReference{Name: obReplayPV, UID: "u-replay"},
			HeadSetDigest: obDigestA, Heads: obHeads,
			Passed: 0, Quarantined: 1, QuarantinedIDs: []string{"counts"},
			Outcomes: []v1alpha1.OutcomeResult{{ID: "counts", Result: v1alpha1.OutcomeQuarantined,
				Basis: obDigestC, Line: "a check went red"}},
			OutcomesRound: 1, OutcomesHeadSetDigest: obDigestA,
			Anomalies: []v1alpha1.AnomalyRecord{{Summary: "a stray 500", Ledger: []int32{2}, Round: 1}},
			Disputed:  []string{obDigestC},
			Suspected: &v1alpha1.SuspectedDefect{Verification: obFullPV, HeadSetDigest: obDigestA,
				Outcomes: []string{"counts"}, RecordedAt: obNow},
			Rung: v1alpha1.RungReplay,
			ModelRound: &v1alpha1.VerificationModelRound{Round: 1, HeadSetDigest: obDigestA,
				OutcomesDigest: obDigestB, Clear: true},
			Escalation: &v1alpha1.VerificationEscalation{Round: 1, HeadSetDigest: obDigestA},
			Automatic:  3, LastAutomaticAt: &obNow, EvaluationCostMicroUSD: 400000,
			Handoff: &v1alpha1.HandoffState{Mode: v1alpha1.HandoffGated, Released: true, ReleasedAt: &obNow,
				Reason: v1alpha1.HandoffReasonHuman},
			FirstGreenAt: &obNow,
			NoDefect: &v1alpha1.NoDefectRecord{Run: "a-fix-run", Reason: "nothing to fix", At: obNow,
				HeadSetDigest: obDigestA, Round: 1},
			CancelRun: "a-cancelled-run",
		},
	}
	return in
}

// leaveCheckEraBehind creates what 0.14.15 leaves, each object's status
// written under 0.14.15's CRDs.
func (u *observedUpgrade) leaveCheckEraBehind() {
	project := obProjectObject(obProject)
	project.Status.Conditions = obReady(1)
	u.createWithStatus(project)
	u.createWithStatus(obIntentObject(obIntent))
	u.createWithStatus(obFullVerification(obFullPV))
	u.createWithStatus(obReplayVerification(obReplayPV))
	ev := obRun(obEvaluate, v1alpha1.IntentStageEvaluate, 1, obFullPV)
	ev.Status = v1alpha1.IntentRunStatus{Phase: v1alpha1.RunComplete, Outcome: "evaluated", StartedAt: &obNow,
		FinishedAt: &obNow}
	u.createWithStatus(ev)
	review := obRun(obReview, v1alpha1.IntentStageCheckReview, 1, "")
	review.Status = v1alpha1.IntentRunStatus{Phase: v1alpha1.RunRunning, StartedAt: &obNow,
		JobRef: &v1alpha1.JobReference{Name: obReview}}
	u.createWithStatus(review)
}

// forward holds what 0.14.15 left to N's CRDs and N's writes.
func (u *observedUpgrade) forward() {
	u.retiredFieldsRead()
	u.rebornUnderN()
	u.supersede()
	u.settleLeftRuns()
	u.projectReadyUnderN()
}

// retiredFieldsRead: every retired field reads back under N's CRDs,
// unpruned, before anything is written.
func (u *observedUpgrade) retiredFieldsRead() {
	for _, f := range []struct {
		kind, name string
		path       []string
	}{
		{"Intent", obIntent, []string{"status", "acceptance", "checks", "configMap"}},
		{"Intent", obIntent, []string{"status", "acceptance", "reviews"}},
		{"Intent", obIntent, []string{"status", "verification", "rung"}},
		{"Intent", obIntent, []string{"status", "verification", "modelRound"}},
		{"PreviewVerification", obFullPV, []string{"spec", "checks"}},
		{"PreviewVerification", obFullPV, []string{"status", "base", "runs"}},
		{"PreviewVerification", obFullPV, []string{"status", "replay"}},
		{"PreviewVerification", obFullPV, []string{"status", "confirmed"}},
		{"PreviewVerification", obReplayPV, []string{"spec", "rung"}},
		{"Project", obProject, []string{"spec", "models", "checkReview"}},
		{"Project", obProject, []string{"spec", "limits", "checkReview", "maxCostMicroUSD"}},
	} {
		if !u.has(f.kind, f.name, f.path...) {
			u.t.Errorf("under N's CRDs %s %s lost %s", f.kind, f.name, strings.Join(f.path, "."))
		}
	}
}

// rebornUnderN: each of 0.14.15's objects is admitted whole by N's CRDs, a
// renamed copy created and its status written fresh.
func (u *observedUpgrade) rebornUnderN() {
	project := obProjectObject("beatbattle-copy")
	project.Status.Conditions = obReady(1)
	u.reborn(project)
	u.reborn(obIntentObject("beatbattle-148"))
	u.reborn(obFullVerification("beatbattle-48-v1-a9"))
	u.reborn(obReplayVerification("beatbattle-48-v2-a9"))
	u.reborn(obRun("beatbattle-48-cr-r1-a9", v1alpha1.IntentStageCheckReview, 1, ""))
	u.reborn(obRun("beatbattle-48-ev-r1-a9", v1alpha1.IntentStageEvaluate, 1, obFullPV))
}

// supersede makes verify_migrate.go's writes: the in-flight replay round is
// dropped, the old judgement withdrawn, every preserved field kept.
func (u *observedUpgrade) supersede() {
	t := u.t
	var pv v1alpha1.PreviewVerification
	u.getTyped(obReplayPV, &pv)
	if err := u.admin.Delete(t.Context(), &pv); err != nil {
		t.Errorf("N deletes the in-flight replay round: %v", err)
	}
	var in v1alpha1.Intent
	u.getTyped(obIntent, &in)
	v := in.Status.Verification
	v.State, v.Cause = v1alpha1.VerificationWaitingForCI, ""
	v.Ref, v.Attempt, v.InfraRetries, v.RetryOwed = nil, 0, 0, false
	v.Outcomes, v.OutcomesRound, v.OutcomesHeadSetDigest, v.OutcomesEnvironment = nil, 0, "", ""
	v.Anomalies, v.Disputed, v.Suspected = nil, nil, nil
	v.Rung, v.ModelRound, v.Escalation = "", nil, nil
	v.Passed, v.Failed, v.Quarantined, v.QuarantinedIDs = 0, 0, 0, nil
	meta.SetStatusCondition(&in.Status.Conditions, metav1.Condition{Type: v1alpha1.ConditionVerificationSuperseded,
		Status: metav1.ConditionTrue, Reason: "VerificationRulesChanged", Message: "withdrawn"})
	u.statusUpdate(&in, "N's supersede withdrawal")
	u.getTyped(obIntent, &in)
	if c := in.Status.Acceptance.Checks; c == nil || c.ConfigMap != obIntent+"-checks-1" || c.Count != 1 {
		t.Errorf("after the withdrawal status.acceptance.checks = %+v; want the 0.14.15 store kept for a "+
			"rollback", c)
	}
	if a := in.Status.Acceptance; len(a.Reviews) != 1 || len(a.RedCleared) != 1 || len(a.Narrowed) != 1 {
		t.Errorf("after the withdrawal status.acceptance = %+v; want reviews, red clearances and narrowed "+
			"checks kept", a)
	}
	if v := in.Status.Verification; v.Handoff == nil || !v.Handoff.Released || v.NoDefect == nil ||
		v.CancelRun == "" || v.Automatic != 3 || v.EvaluationCostMicroUSD != 400000 {
		t.Errorf("after the withdrawal status.verification = %+v; want the preserved fields kept", v)
	}
}

// settleLeftRuns: the retired checkreview run is settled Aborted (its Job
// deleted), and the settled round-1 evaluate run and its verification take
// N's metadata writes (a finalizer's removal, garbage collection).
func (u *observedUpgrade) settleLeftRuns() {
	var review v1alpha1.IntentRun
	u.getTyped(obReview, &review)
	review.Status.Phase, review.Status.Outcome = v1alpha1.RunFailed, intent.OutcomeAborted
	review.Status.Detail, review.Status.FinishedAt = "the check reviewer is retired", &obNow
	u.statusUpdate(&review, "N settling the retired checkreview run")
	var run v1alpha1.IntentRun
	u.getTyped(obEvaluate, &run)
	u.touch(&run)
	var full v1alpha1.PreviewVerification
	u.getTyped(obFullPV, &full)
	u.touch(&full)
}

// projectReadyUnderN: the Project takes N's writes and keeps Ready and its
// inert checkReview fields.
func (u *observedUpgrade) projectReadyUnderN() {
	t := u.t
	var p v1alpha1.Project
	u.getTyped(obProject, &p)
	u.touch(&p)
	u.getTyped(obProject, &p)
	p.Status.Conditions = obReady(p.Generation)
	u.statusUpdate(&p, "N's Project Ready")
	u.getTyped(obProject, &p)
	if c := meta.FindStatusCondition(p.Status.Conditions, v1alpha1.ConditionReady); c == nil ||
		c.Status != metav1.ConditionTrue {
		t.Errorf("the Project's Ready under N = %+v, want True", c)
	}
	if p.Spec.Models == nil || p.Spec.Models.CheckReview == "" || p.Spec.Limits.CheckReview.MaxCostMicroUSD == 0 {
		t.Errorf("the Project's retired checkReview fields were lost: models %+v, limits %+v", p.Spec.Models,
			p.Spec.Limits.CheckReview)
	}
}

// createAsN creates what N writes after the supersede: the fresh round's
// agents verification and evaluate run, and the observed judgement.
func (u *observedUpgrade) createAsN() {
	t := u.t
	u.createWithStatus(obFreshVerification(obFreshPV))
	ev := obRun(obFreshEval, v1alpha1.IntentStageEvaluate, 3, obFreshPV)
	ev.Status = v1alpha1.IntentRunStatus{Phase: v1alpha1.RunComplete, Outcome: "evaluated", StartedAt: &obNow,
		FinishedAt: &obNow}
	u.createWithStatus(ev)

	var in v1alpha1.Intent
	u.getTyped(obIntent, &in)
	obJudge(&in)
	u.statusUpdate(&in, "N's observed judgement")
	u.getTyped(obIntent, &in)
	if v := in.Status.Verification; v.Judge != v1alpha1.JudgeObserved || len(v.Outcomes) != 1 ||
		len(v.Outcomes[0].Cites) != 2 {
		t.Errorf("under N's CRDs the observed judgement reads back as %+v; want the marker and the cites", v)
	}
	var pv v1alpha1.PreviewVerification
	u.getTyped(obFreshPV, &pv)
	if e := pv.Status.Ledger.Entries; len(e) < 3 || e[2].Side != v1alpha1.TreeBase {
		t.Errorf("under N's CRDs the fresh round's ledger reads back as %+v; want a base entry", e)
	}
}

// obFreshVerification is the fresh round N starts at the supersede (round
// 3, attempt 1, rung full as the observed evaluator always writes),
// settled Observed with a ledger whose base entry names its side.
func obFreshVerification(name string) *v1alpha1.PreviewVerification {
	v := agentsVerification(name)
	v.Spec.IntentRef = v1alpha1.ObjectReference{Name: obIntent, UID: "u-intent"}
	v.Spec.Round, v.Spec.Run = 3, "2c3d4e"
	v.Status = v1alpha1.PreviewVerificationStatus{
		Phase: v1alpha1.VerificationObserved, StartedAt: &obNow, CompletedAt: &obNow,
		Ledger: &v1alpha1.LedgerSummary{Count: 3, SHA256: obHex, Entries: []v1alpha1.LedgerIndexEntry{
			{Seq: 1, Kind: v1alpha1.LedgerKindRelay, Verb: v1alpha1.LedgerVerbBuild, ResultSHA256: obHex},
			{Seq: 2, Kind: v1alpha1.LedgerKindAgent, Verb: v1alpha1.LedgerVerbExec, ResultSHA256: obHex},
			{Seq: 3, Kind: v1alpha1.LedgerKindAgent, Verb: v1alpha1.LedgerVerbExec, ResultSHA256: obHex,
				Side: v1alpha1.TreeBase}},
			Builds: []v1alpha1.LedgerBuild{{Side: v1alpha1.TreeHead, Result: v1alpha1.BuildBuilt, Seq: 1}}},
	}
	return v
}

// obJudge writes the fresh round's observed judgement onto in, as
// createVerification and applyJudgement do.
func obJudge(in *v1alpha1.Intent) {
	v := in.Status.Verification
	v.Judge = v1alpha1.JudgeObserved
	v.State, v.Round, v.Attempt = v1alpha1.VerificationPassed, 3, 1
	v.Ref = &v1alpha1.ObjectReference{Name: obFreshPV, UID: "u-fresh"}
	v.Outcomes = []v1alpha1.OutcomeResult{{ID: "counts", Result: v1alpha1.OutcomePassed,
		Cites: []int32{2, 3}}}
	v.Passed, v.OutcomesRound, v.OutcomesHeadSetDigest = 1, 3, obDigestA
	meta.RemoveStatusCondition(&in.Status.Conditions, v1alpha1.ConditionVerificationSuperseded)
}

// back holds what N created to 0.14.15's CRDs and 0.14.15's writes.
func (u *observedUpgrade) back() {
	t := u.t
	// N's fields are pruned; everything else reads back.
	for _, f := range []struct {
		kind, name string
		path       []string
	}{
		{"Intent", obIntent, []string{"status", "verification", "judge"}},
		{"Intent", obIntent, []string{"status", "verification", "outcomes", "cites"}},
		{"PreviewVerification", obFreshPV, []string{"status", "ledger", "entries", "side"}},
	} {
		if u.has(f.kind, f.name, f.path...) {
			t.Errorf("under 0.14.15's CRDs %s %s still reads %s; want it pruned", f.kind, f.name,
				strings.Join(f.path, "."))
		}
	}
	var in v1alpha1.Intent
	u.getTyped(obIntent, &in)
	if c := in.Status.Acceptance.Checks; c == nil || c.ConfigMap != obIntent+"-checks-1" {
		t.Errorf("rolled back, status.acceptance.checks = %+v; want the store writeCheckStore resumes from", c)
	}
	if v := in.Status.Verification; len(v.Outcomes) != 1 || v.Outcomes[0].Result != v1alpha1.OutcomePassed {
		t.Errorf("rolled back, the judgement reads %+v; want N's result, its cites pruned", v.Outcomes)
	}

	// What N created is admitted whole under 0.14.15's rules: CEL :303 (an
	// outcome verification names its rung and tools) and the evaluate run's
	// required spec.evaluation.rung.
	u.reborn(obFreshVerification("beatbattle-48-v3-a9"))
	fresh := obRun("beatbattle-48-ev-r3-a9", v1alpha1.IntentStageEvaluate, 3, obFreshPV)
	fresh.Status = v1alpha1.IntentRunStatus{Phase: v1alpha1.RunComplete, Outcome: "evaluated"}
	u.reborn(fresh)
	judged := obIntentObject("beatbattle-148")
	obJudge(judged)
	u.reborn(judged)
	project := obProjectObject("beatbattle-copy")
	project.Status.Conditions = obReady(1)
	u.reborn(project)

	// 0.14.15's own writes: its controller picks a full round with no
	// modelRound, records the ladder, and its hosts write their status.
	u.getTyped(obIntent, &in)
	v := in.Status.Verification
	v.Rung = v1alpha1.RungFull
	v.ModelRound = &v1alpha1.VerificationModelRound{Round: 3, HeadSetDigest: obDigestA, OutcomesDigest: obDigestB,
		Clear: true}
	u.statusUpdate(&in, "0.14.15 recording a full round")
	var pv v1alpha1.PreviewVerification
	u.getTyped(obFreshPV, &pv)
	pv.Status.Base = &v1alpha1.BaseStatus{Required: true, Runs: []v1alpha1.CheckRun{obCheckRun(v1alpha1.TreeBase, 3)}}
	pv.Status.Confirmations = []v1alpha1.CheckRun{obCheckRun("", 2)}
	u.statusUpdate(&pv, "0.14.15's agents host on N's verification")
	var run v1alpha1.IntentRun
	u.getTyped(obFreshEval, &run)
	run.Status.Report = "evaluated under N"
	u.statusUpdate(&run, "0.14.15 on N's evaluate run")
	u.touch(&run)

	var p v1alpha1.Project
	u.getTyped(obProject, &p)
	p.Status.Conditions = obReady(p.Generation)
	u.statusUpdate(&p, "0.14.15's Project Ready")
}

// createWithStatus creates obj and writes the status it carries.
func (u *observedUpgrade) createWithStatus(obj client.Object) {
	u.t.Helper()
	status := statusOf(u.t, obj)
	if err := u.admin.Create(u.t.Context(), obj); err != nil {
		u.t.Fatalf("create %T %s: %v", obj, obj.GetName(), err)
	}
	if status == nil {
		return
	}
	setStatus(u.t, obj, status)
	if err := u.admin.Status().Update(u.t.Context(), obj); err != nil {
		u.t.Fatalf("status of %T %s: %v", obj, obj.GetName(), err)
	}
}

// reborn creates a copy of obj (already renamed, its status set) and writes
// its status, so the API server judges every field against the CRDs it
// serves now, with no old value to ratchet against; then deletes it.
func (u *observedUpgrade) reborn(obj client.Object) {
	u.t.Helper()
	status := statusOf(u.t, obj)
	if err := u.admin.Create(u.t.Context(), obj); err != nil {
		u.t.Errorf("a copy of %T refused: %v", obj, err)
		return
	}
	if status != nil {
		setStatus(u.t, obj, status)
		if err := u.admin.Status().Update(u.t.Context(), obj); err != nil {
			u.t.Errorf("the status of a copy of %T refused: %v", obj, err)
		}
	}
	if err := u.admin.Delete(u.t.Context(), obj); err != nil {
		u.t.Errorf("delete the copy %s: %v", obj.GetName(), err)
	}
}

// statusOf returns obj's status as a JSON map, nil when it is empty.
func statusOf(t *testing.T, obj client.Object) map[string]any {
	t.Helper()
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := m["status"].(map[string]any)
	if len(s) == 0 {
		return nil
	}
	return s
}

// setStatus replaces obj's status with status (as statusOf returned it).
func setStatus(t *testing.T, obj client.Object, status map[string]any) {
	t.Helper()
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		t.Fatal(err)
	}
	m["status"] = status
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(m, obj); err != nil {
		t.Fatal(err)
	}
}

func (u *observedUpgrade) getTyped(name string, obj client.Object) {
	u.t.Helper()
	if err := u.admin.Get(u.t.Context(), client.ObjectKey{Namespace: egressRelease, Name: name}, obj); err != nil {
		u.t.Fatalf("get %T %s: %v", obj, name, err)
	}
}

func (u *observedUpgrade) statusUpdate(obj client.Object, what string) {
	u.t.Helper()
	if err := u.admin.Status().Update(u.t.Context(), obj); err != nil {
		u.t.Errorf("%s refused: %v", what, err)
	}
}

// touch: obj takes a metadata update, as a finalizer's removal would.
func (u *observedUpgrade) touch(obj client.Object) {
	u.t.Helper()
	labels := obj.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels["maul.run/upgraded"] = "true"
	obj.SetLabels(labels)
	if err := u.admin.Update(u.t.Context(), obj); err != nil {
		u.t.Errorf("%T %s refuses a metadata update: %v", obj, obj.GetName(), err)
	}
}

// has: the stored kind/name holds path, every list element on the way
// searched (a list holds path when any element does).
func (u *observedUpgrade) has(kind, name string, path ...string) bool {
	u.t.Helper()
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(v1alpha1.GroupVersion.WithKind(kind))
	if err := u.admin.Get(u.t.Context(), client.ObjectKey{Namespace: egressRelease, Name: name}, obj); err != nil {
		u.t.Fatalf("get %s %s: %v", kind, name, err)
	}
	return holds(obj.Object, path)
}

func holds(v any, path []string) bool {
	if len(path) == 0 {
		return v != nil
	}
	switch x := v.(type) {
	case map[string]any:
		next, ok := x[path[0]]
		return ok && holds(next, path[1:])
	case []any:
		for _, e := range x {
			if holds(e, path) {
				return true
			}
		}
	}
	return false
}

// applyCRDs replaces the four CRDs with those under dir.
func (u *observedUpgrade) applyCRDs(dir string) {
	t, ctx := u.t, u.t.Context()
	for _, file := range observedKinds {
		b, err := os.ReadFile(dir + file)
		if err != nil {
			t.Fatal(err)
		}
		var want apiextensionsv1.CustomResourceDefinition
		dec := utilyaml.NewYAMLOrJSONDecoder(strings.NewReader(string(b)), 4096)
		for want.Name == "" {
			if err := dec.Decode(&want); err != nil {
				t.Fatalf("decode %s: %v", file, err)
			}
		}
		var crd apiextensionsv1.CustomResourceDefinition
		if err := u.crds.Get(ctx, client.ObjectKey{Name: want.Name}, &crd); err != nil {
			t.Fatal(err)
		}
		crd.Spec = want.Spec
		if err := u.crds.Update(ctx, &crd); err != nil {
			t.Fatalf("apply %s%s: %v", dir, file, err)
		}
	}
}

// waitServed waits until the API server serves the CRDs just applied: N's
// (nKept) keeps an Intent's judge and a ledger entry's side on a dry-run
// status write, 0.14.15's prunes both. Without it either leg could run
// against the other's schema and pass vacuously.
func (u *observedUpgrade) waitServed(nKept bool) {
	t, ctx := u.t, u.t.Context()
	deadline := time.Now().Add(time.Minute)
	for {
		var in v1alpha1.Intent
		u.getTyped(obIntent, &in)
		in.Status.Verification.Judge = v1alpha1.JudgeObserved
		errIn := u.admin.Status().Update(ctx, &in, client.DryRunAll)
		var pv v1alpha1.PreviewVerification
		u.getTyped(obFullPV, &pv)
		pv.Status.Ledger.Entries[0].Side = v1alpha1.TreeBase
		errPV := u.admin.Status().Update(ctx, &pv, client.DryRunAll)
		inKept := errIn == nil && in.Status.Verification.Judge == v1alpha1.JudgeObserved
		pvKept := errPV == nil && pv.Status.Ledger.Entries[0].Side == v1alpha1.TreeBase
		if errIn == nil && errPV == nil && inKept == nKept && pvKept == nKept {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the CRDs are not served after a minute (want N's fields kept: %v): intent %v kept %v, "+
				"verification %v kept %v", nKept, errIn, inKept, errPV, pvKept)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
