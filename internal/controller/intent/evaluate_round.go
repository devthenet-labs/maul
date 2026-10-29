// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/acceptance/crdconv"
	"github.com/devthenet-labs/maul/internal/usagerollup"
)

// The evaluation round (DESIGN 2.2, SPEC S1-17), with --intent-evaluator
// on, for a Project the evaluator verifies (Settings.evaluates). It is the
// verification loop's (verify_loop.go) second shape: the same gate, head set
// and bounds, with the approved outcomes in place of slice 1's criteria.
//
//  1. The gate (startEvaluation, startRound's last step) is the kept one:
//     every named check green, no human round pending, the Preview live at
//     exactly the heads, within maxVerifications. Past the evaluation
//     ceiling no round starts: the verification is Unconfirmed, never a
//     block, and the fix rounds keep their budget.
//  2. It creates the outcome PreviewVerification (createEvaluation): the
//     approved outcomes, the saved checks copied inline from the check
//     store, the rung (full; replay, the model-free round after a dispute,
//     when a disputed check awaits one) and the tool pod's tools. State
//     Evaluating. Each attempt is a new PreviewVerification: the tool pod
//     takes one finish, so a retry needs a new one (SPEC W3).
//  3. preview-controller renders the slot tool pod and writes
//     status.tools. Only then, the tool pod's address in hand, is the
//     evaluate IntentRun created (launchEvaluator), through the normal path:
//     the evaluation ceiling, the credential hold, attribution and the
//     budget annotation (lease, createRoleRun). Its Job is pinned to that
//     tool pod by a hostAlias, which is fixed when the pod is created: the
//     order is the point (TestEvaluatorCreatedAfterToolsStatus). A replay
//     round creates no run at all.
//  4. Collection (settleEvaluation): the run settles (collectEvaluate keeps
//     the evaluator's report, its claims) and preview-controller settles the
//     PreviewVerification (the ledger, the confirmations: the facts). Both
//     in hand, the round is judged (judgeEvaluation). A stale verification
//     (the heads moved, the outcomes changed) is dropped and never launches
//     an evaluator; maul's own failure is the round's next attempt.
//
// A codex Project without --intent-evaluate-codex is not evaluated at all
// (SPEC Q16): its hand-off is early, and the Project says why
// (ConditionEvaluatorUnverified).

// keyCheckStore is the check store's data key (status.acceptance.checks):
// its saved checks as the JSON list of v1alpha1.SavedCheck, whose digest
// (checkStoreDigest) the store's ref records and every outcome
// PreviewVerification's checksDigest carries.
const keyCheckStore = "checks.json"

// evaluationReasonNotJudged is an outcome's result reason while maul does
// not yet judge an evaluation round's facts (acceptance.Decide, S1-20):
// every outcome is unsure, never passed, so nothing is ever confirmed
// before it is judged.
const evaluationReasonNotJudged = "maul has not judged this evaluation round"

// evaluationCeiling is the most one intent's evaluate and checkreview runs
// may spend, in micro-USD: the Project's limits.maxEvaluateCostMicroUSD (or
// its default), never more than --intent-evaluation-cost-max.
func (s Settings) evaluationCeiling(p *v1alpha1.Project) int64 {
	c := p.Spec.Limits.MaxEvaluateCostLimit()
	if s.EvaluationCostMax > 0 && s.EvaluationCostMax < c {
		return s.EvaluationCostMax
	}
	return c
}

// evaluatorUnverified reports a Project whose harness has no evaluator
// maul has verified live: codex without --intent-evaluate-codex (SPEC
// Q16). Its intents are not evaluated, and their hand-off is early.
func (s Settings) evaluatorUnverified(p *v1alpha1.Project) bool {
	return s.evaluates(p) && p.Spec.Harness.IsCodex() && !s.EvaluateCodex
}

// toolCalls is an evaluation's tool-call bound: the Project's
// verification.maxToolCalls (or its default), never more than
// --intent-max-tool-calls, held to the schema's bounds.
func (s Settings) toolCalls(p *v1alpha1.Project) int32 {
	n := p.Spec.MaxToolCalls()
	if s.MaxToolCalls > 0 && s.MaxToolCalls < n {
		n = s.MaxToolCalls
	}
	return min(max(n, v1alpha1.MinToolCalls), v1alpha1.MaxToolCallsLimit)
}

// evaluable are the outcomes an evaluation judges: every one but those a
// human checks (manual). An approved plan with none is NotDeclared.
func evaluable(os []v1alpha1.AcceptanceOutcome) bool {
	return slices.ContainsFunc(os, func(o v1alpha1.AcceptanceOutcome) bool { return !o.Manual })
}

// syncEvaluation is syncVerification's evaluator shape: NotDeclared for a
// plan with nothing to evaluate (the head set still tracked, for an
// approver's acceptance), Off for a Project with no verified evaluator,
// the human-round halt and the head set as slice 1 keeps them, then the
// round in flight (settleEvaluation).
func (p *pass) syncEvaluation(ctx context.Context) (bool, error) {
	acc := p.in.Status.Acceptance
	hs, ok := previewHeadSet(p.proj, p.in)
	if !evaluable(acc.Outcomes) {
		if ok {
			if changed, err := p.syncHeadSet(ctx, hs, v1alpha1.VerificationNotDeclared); changed || err != nil {
				return changed, err
			}
		}
		return p.settleState(ctx, v1alpha1.VerificationNotDeclared)
	}
	if p.set.evaluatorUnverified(p.proj) {
		return p.settleUnverified(ctx)
	}
	if reason := p.humanRoundHalt(); reason != "" {
		return true, p.update(ctx, func(cur *v1alpha1.Intent) error {
			halt(cur.Status.Verification, reason, metav1.NewTime(p.now))
			return nil
		})
	}
	if !ok {
		return false, nil
	}
	if changed, err := p.syncHeadSet(ctx, hs, v1alpha1.VerificationWaitingForCI); changed || err != nil {
		return changed, err
	}
	return p.settleEvaluation(ctx)
}

// settleUnverified records an intent of a Project with no verified
// evaluator (evaluatorUnverified): nothing is evaluated (state Off, any
// request consumed) and the hand-off is released at once, early, saying
// why (HandoffReasonEvaluatorUnverified). Written once.
func (p *pass) settleUnverified(ctx context.Context) (bool, error) {
	v := p.in.Status.Verification
	if v.State == v1alpha1.VerificationOff && v.Requested == nil && v.Handoff != nil && v.Handoff.Released &&
		v.Handoff.Reason == v1alpha1.HandoffReasonEvaluatorUnverified {
		return false, nil
	}
	if v.Ref != nil {
		p.dropInFlight(ctx, *v.Ref)
	}
	return true, p.update(ctx, func(cur *v1alpha1.Intent) error {
		nv := cur.Status.Verification
		nv.State, nv.Requested, nv.Ref = v1alpha1.VerificationOff, nil, nil
		at := metav1.NewTime(p.now)
		nv.Handoff = &v1alpha1.HandoffState{Mode: v1alpha1.HandoffEarly, Released: true, ReleasedAt: &at,
			Reason: v1alpha1.HandoffReasonEvaluatorUnverified}
		return nil
	})
}

// startEvaluation is startVerification's evaluator shape: the kept gate,
// then the outcome PreviewVerification (createEvaluation), or, past the
// evaluation ceiling, Unconfirmed (evaluationExhausted).
func (p *pass) startEvaluation(ctx context.Context, prs []v1alpha1.IntentPullRequest) (bool, error) {
	acc, v := p.in.Status.Acceptance, p.in.Status.Verification
	if !evaluable(acc.Outcomes) || p.set.evaluatorUnverified(p.proj) {
		return false, nil
	}
	hs, ok := previewHeadSet(p.proj, p.in)
	if !ok || hs.digest != v.HeadSetDigest {
		return false, nil
	}
	kind, err := p.verifyDue(ctx, v)
	if err != nil || kind == verifyNotDue {
		return false, err
	}
	if kind == verifyRequestRefused {
		p.r.log().LogAttrs(ctx, slog.LevelInfo, "maxVerifications is spent; the verification request is dropped",
			slog.String("intent", p.in.Name), slog.Int64("comment", v.Requested.CommentID))
		return true, p.update(ctx, func(cur *v1alpha1.Intent) error {
			cur.Status.Verification.Requested = nil
			return nil
		})
	}
	preview := p.livePreview(ctx, v)
	if preview == nil {
		return p.waitPreview(ctx)
	}
	green, err := p.checksGreen(ctx, prs)
	if err != nil || !green {
		if err != nil {
			return false, err
		}
		return p.waitVerification(ctx, v1alpha1.VerificationWaitingForCI)
	}
	if p.atEvaluationCeiling() {
		return p.evaluationExhausted(ctx)
	}
	checks, digest, err := p.storedChecks(ctx)
	if err != nil {
		// The store cannot be read as recorded: no round runs against
		// another set of checks than the one on record.
		p.r.log().LogAttrs(ctx, slog.LevelWarn, "read the intent's check store; no evaluation this pass",
			slog.String("intent", p.in.Name), slog.Any("error", err))
		return false, nil
	}
	return true, p.createEvaluation(ctx, kind, preview, hs, checks, digest)
}

// evaluationExhausted records that the intent's evaluations spent their
// ceiling: no round starts, the verification is Unconfirmed (any request
// consumed), and the intent is never blocked for it (DESIGN 5).
func (p *pass) evaluationExhausted(ctx context.Context) (bool, error) {
	v := p.in.Status.Verification
	if v.State == v1alpha1.VerificationUnconfirmed && v.Requested == nil {
		return false, nil
	}
	p.r.log().LogAttrs(ctx, slog.LevelInfo, "the intent's evaluations spent their ceiling; nothing more is evaluated",
		slog.String("intent", p.in.Name), slog.Int64("spent", p.evaluationSpend()),
		slog.Int64("ceiling", p.set.evaluationCeiling(p.proj)))
	return true, p.update(ctx, func(cur *v1alpha1.Intent) error {
		nv := cur.Status.Verification
		nv.State, nv.Requested = v1alpha1.VerificationUnconfirmed, nil
		return nil
	})
}

// evaluationRung is the rung a new round runs (SPEC Q17): replay, with no
// model, when a disputed check awaits its model-free round and there are
// saved checks to replay; otherwise full.
func evaluationRung(v *v1alpha1.IntentVerification, checks []v1alpha1.SavedCheck) v1alpha1.VerificationRung {
	if len(v.Disputed) > 0 && len(checks) > 0 {
		return v1alpha1.RungReplay
	}
	return v1alpha1.RungFull
}

// createEvaluation creates the round's outcome PreviewVerification: the
// approved outcomes and their digest, the saved checks and theirs, the rung,
// the tool pod's tools and its wall clock (the evaluate run's timeout plus
// EvaluationToolsMargin, within a verification's hour).
func (p *pass) createEvaluation(ctx context.Context, kind verifyKind, preview *v1alpha1.Preview, hs headSet,
	checks []v1alpha1.SavedCheck, checksDigest string) error {
	acc, v := p.in.Status.Acceptance, p.in.Status.Verification
	evaluate := p.set.grant(p.proj, v1alpha1.IntentStageEvaluate)
	timeout := min(msDuration(evaluate.TimeoutMilliseconds)+EvaluationToolsMargin, maxVerificationTimeout)
	return p.createVerification(ctx, kind, preview, hs, v1alpha1.VerificationEvaluating,
		func(spec *v1alpha1.PreviewVerificationSpec) error {
			spec.Outcomes, spec.OutcomesDigest = slices.Clone(acc.Outcomes), acc.OutcomesDigest
			if len(checks) > 0 {
				spec.Checks, spec.ChecksDigest = checks, checksDigest
			}
			spec.Rung = evaluationRung(v, checks)
			spec.Tools = &v1alpha1.ToolsSpec{
				Browser:          p.proj.Spec.Surfaces != nil && p.proj.Spec.Surfaces.Browser != nil,
				Sandbox:          true,
				MaxToolCalls:     p.set.toolCalls(p.proj),
				LookAroundRoutes: v1alpha1.MaxLookAroundRoutes,
			}
			spec.Timeout = metav1.Duration{Duration: max(timeout, minVerificationTimeout)}
			return nil
		})
}

// A PreviewVerification's timeout bounds (its schema's).
const (
	minVerificationTimeout = time.Minute
	maxVerificationTimeout = time.Hour
)

// msDuration is a grant's milliseconds as a duration.
func msDuration(ms int64) time.Duration { return time.Duration(ms) * time.Millisecond }

// storedChecks are the intent's saved checks as its check store holds
// them, with the store's digest: none (nil, "") while nothing is saved. A
// store that is gone, not the Intent's, or not what its ref records, is an
// error: no round runs against other checks than the ones on record.
func (p *pass) storedChecks(ctx context.Context) ([]v1alpha1.SavedCheck, string, error) {
	ref := p.in.Status.Acceptance.Checks
	if ref == nil || ref.Count == 0 {
		return nil, "", nil
	}
	var cm corev1.ConfigMap
	if err := p.r.APIReader.Get(ctx, types.NamespacedName{Namespace: p.in.Namespace, Name: ref.ConfigMap},
		&cm); err != nil {
		return nil, "", fmt.Errorf("read check store %s: %w", ref.ConfigMap, err)
	}
	if !controlledBy(cm.OwnerReferences, p.in.UID) {
		return nil, "", fmt.Errorf("check store %s: %w", ref.ConfigMap, errNotOwned)
	}
	raw := cm.Data[keyCheckStore]
	if got := digest([]byte(raw)); got != ref.Digest {
		return nil, "", fmt.Errorf("check store %s holds %s, not the recorded %s", ref.ConfigMap, got, ref.Digest)
	}
	var checks []v1alpha1.SavedCheck
	if err := json.Unmarshal([]byte(raw), &checks); err != nil {
		return nil, "", fmt.Errorf("check store %s: %w", ref.ConfigMap, err)
	}
	if len(checks) != int(ref.Count) || len(checks) > v1alpha1.MaxSavedChecks {
		return nil, "", fmt.Errorf("check store %s holds %d checks, not the recorded %d", ref.ConfigMap,
			len(checks), ref.Count)
	}
	return checks, ref.Digest, nil
}

// encodeCheckStore is a check store's data (keyCheckStore) and its digest:
// the JSON list of checks, in order. The store's writer and storedChecks
// share it.
func encodeCheckStore(checks []v1alpha1.SavedCheck) (string, string, error) {
	b, err := json.Marshal(checks)
	if err != nil {
		return "", "", fmt.Errorf("encode the check store: %w", err)
	}
	return string(b), digest(b), nil
}

// settleEvaluation drives the round in flight, one step a pass: launch
// the evaluator once its tool pod is in status; once both the run and the
// verification settled, judge the round; a verification the intent no
// longer stands on (gone, another head set or other outcomes) is dropped
// and the round's next attempt is due, as is one maul's own machinery
// failed, or whose evaluator never ran.
func (p *pass) settleEvaluation(ctx context.Context) (bool, error) {
	v := p.in.Status.Verification
	if v.Ref == nil || v.State != v1alpha1.VerificationEvaluating {
		return false, nil
	}
	pv, err := p.readVerification(ctx, *v.Ref)
	if err != nil {
		p.r.log().LogAttrs(ctx, slog.LevelWarn, "read the intent's evaluation; settled next pass",
			slog.String("intent", p.in.Name), slog.String("verification", v.Ref.Name), slog.Any("error", err))
		return false, nil
	}
	if pv != nil && (pv.Spec.HeadSetDigest != v.HeadSetDigest ||
		pv.Spec.OutcomesDigest != p.in.Status.Acceptance.OutcomesDigest) {
		p.dropInFlight(ctx, *v.Ref)
		pv = nil
	}
	if pv == nil {
		return true, p.unjudged(ctx)
	}
	run := p.evaluateRun(pv)
	switch pv.Status.Phase {
	case v1alpha1.VerificationPending, "":
		return false, nil // the tool pod is not up yet
	case v1alpha1.VerificationRunning:
		return p.evaluationRunning(ctx, pv, run)
	case v1alpha1.VerificationObserved:
		return p.evaluationObserved(ctx, pv, run)
	case v1alpha1.VerificationEnvFailed:
		if acceptance.ClassifyEnvironment(crdconv.PartFacts(pv.Status.Environment)) == acceptance.EnvAppFailure {
			return true, p.judgeEvaluation(ctx, pv, run)
		}
		return true, p.unjudged(ctx)
	case v1alpha1.VerificationInfra, v1alpha1.VerificationStale:
		return true, p.unjudged(ctx)
	}
	return false, nil
}

// evaluationRunning is settleEvaluation while pv's tool pod runs: a full
// round's evaluator is launched once its address is in status; one that
// never ran or failed drops the round (its tool pod would hold the slot
// until its deadline for nothing) and the next attempt is due; otherwise
// the tools container finishes and is collected. A replay round runs with
// no model.
func (p *pass) evaluationRunning(ctx context.Context, pv *v1alpha1.PreviewVerification,
	run *v1alpha1.IntentRun) (bool, error) {
	switch {
	case pv.Spec.Rung == v1alpha1.RungReplay:
		return false, nil
	case run == nil:
		return p.launchEvaluator(ctx, pv)
	case !runSettled(run) || run.Status.Phase == v1alpha1.RunComplete:
		return false, nil
	}
	p.dropInFlight(ctx, *p.in.Status.Verification.Ref)
	return true, p.unjudged(ctx)
}

// evaluationObserved is settleEvaluation once preview-controller collected
// pv: once its evaluator's run settled complete, its new checks are
// reviewed and stored (reviewChecks), then the round is judged (a replay
// round has no run, and nothing new to store); with no run, or a failed
// one, the round's next attempt is due.
func (p *pass) evaluationObserved(ctx context.Context, pv *v1alpha1.PreviewVerification,
	run *v1alpha1.IntentRun) (bool, error) {
	switch {
	case pv.Spec.Rung == v1alpha1.RungReplay:
		return true, p.judgeEvaluation(ctx, pv, nil)
	case run == nil || runSettled(run) && run.Status.Phase != v1alpha1.RunComplete:
		return true, p.unjudged(ctx)
	case !runSettled(run):
		return false, nil
	}
	// Its new checks reviewed and the proven ones stored (S1-19) first.
	if changed, wait, err := p.reviewChecks(ctx, pv); changed || wait || err != nil {
		return changed, err
	}
	return true, p.judgeEvaluation(ctx, pv, run)
}

// runSettled reports a run that ended.
func runSettled(run *v1alpha1.IntentRun) bool {
	return run.Status.Phase == v1alpha1.RunComplete || run.Status.Phase == v1alpha1.RunFailed
}

// evaluateRun is the evaluate run of pv, nil when none was created: the
// one whose spec pins pv by UID.
func (p *pass) evaluateRun(pv *v1alpha1.PreviewVerification) *v1alpha1.IntentRun {
	for _, run := range slices.Backward(p.runs) {
		if run.Spec.Stage == v1alpha1.IntentStageEvaluate && run.Spec.Evaluation != nil &&
			run.Spec.Evaluation.UID == pv.UID {
			return run
		}
	}
	return nil
}

// launchEvaluator creates pv's evaluate run once its tool pod's address is
// in status (never before: the Job's hostAlias is fixed when its pod is
// created), through the normal path: the credential hold, the evaluation
// ceiling (Unconfirmed past it, the round dropped), attribution, and the
// handoff rendered from the intent's records (evaluateHandoff). The run is
// the PreviewVerification's round and attempt.
func (p *pass) launchEvaluator(ctx context.Context, pv *v1alpha1.PreviewVerification) (bool, error) {
	t := pv.Status.Tools
	if t == nil || !t.Ready || t.PodIP == "" || p.proj.Spec.Suspend {
		return false, nil
	}
	attributed := p.attributeRun(v1alpha1.IntentStageEvaluate)
	if held, err := p.holdForCredential(ctx, p.set.Attribution.Stamp(attributed), false); held || err != nil {
		return held, err
	}
	handoff, err := p.evaluateHandoff(ctx, pv)
	if err != nil {
		return false, err
	}
	_, err = p.createRoleRun(ctx, v1alpha1.IntentStageEvaluate, pv.Spec.Round, pv.Spec.Attempt,
		&v1alpha1.IntentRunEvaluation{Verification: pv.Name, UID: pv.UID, ToolIP: t.PodIP, Rung: pv.Spec.Rung},
		map[string]string{keyIssue: handoff})
	if errors.Is(err, errEvaluationCeiling) {
		p.dropInFlight(ctx, *p.in.Status.Verification.Ref)
		return p.evaluationExhausted(ctx)
	}
	if err != nil {
		return false, err
	}
	p.r.log().LogAttrs(ctx, slog.LevelInfo, "evaluation round: the evaluator is launched",
		slog.String("intent", p.in.Name), slog.String("verification", pv.Name),
		slog.String("tool_pod", t.PodIP))
	return true, nil
}

// createRoleRun creates an evaluate or checkreview run of round and
// attempt (lease: its ceiling is the evaluation ceiling,
// errEvaluationCeiling past it), or adopts the one a failed pass created,
// and its input ConfigMap holding data (the JSON handoff under keyIssue),
// immutable and owned by the run. A role run reads no repository: its
// spec.repository names the Project's first, for display, and no
// Repository is ever created under it. It pins the approved plan, as every
// run after approval does, and an evaluate run its PreviewVerification
// (evaluation). The reviewer round creates its checkreview runs here too.
func (p *pass) createRoleRun(ctx context.Context, stage v1alpha1.IntentStage, round, attempt int32,
	evaluation *v1alpha1.IntentRunEvaluation, data map[string]string) (*v1alpha1.IntentRun, error) {
	if err := p.set.stageEnabled(stage); err != nil {
		return nil, fmt.Errorf("create run: %w", err)
	}
	if !roleStage(stage) || (stage == v1alpha1.IntentStageEvaluate) != (evaluation != nil) {
		return nil, fmt.Errorf("create run: stage %q is not a role stage, or its evaluation is missing", stage)
	}
	ap := p.in.Status.Approval
	if ap == nil {
		return nil, fmt.Errorf("create run: a %s run needs an approved plan", stage)
	}
	name := v1alpha1.IntentRunName(p.in.Name, stage, round, "", attempt)
	attributedTo := p.set.Attribution.Stamp(p.attributeRun(stage))
	run := &v1alpha1.IntentRun{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: p.in.Namespace,
			Labels:          map[string]string{v1alpha1.LabelIntent: p.in.Name},
			Finalizers:      usagerollup.Finalizers(attributedTo, v1alpha1.FinalizerJobs),
			OwnerReferences: []metav1.OwnerReference{intentOwner(p.in)},
		},
		Spec: v1alpha1.IntentRunSpec{
			IntentRef: v1alpha1.ObjectReference{Name: p.in.Name, UID: p.in.UID},
			Stage:     stage,
			Repository: v1alpha1.IntentRunRepository{URL: p.proj.Spec.Repositories[0].URL,
				RepositoryRef: v1alpha1.LocalObjectReference{Name: runRepositoryName(name)}},
			Round: round, Attempt: attempt,
			Inputs: v1alpha1.IntentRunInputs{ConfigMap: runInputName(name), InputDigest: ap.InputDigest,
				PlanRevision: ap.PlanRevision, PlanDigest: ap.PlanDigest},
			Grant:        p.set.grant(p.proj, stage),
			AttributedTo: attributedTo,
			Harness:      p.proj.Spec.Harness,
			Evaluation:   evaluation,
		},
	}
	existing, err := p.lease(ctx, run)
	switch {
	case err != nil:
		return nil, err
	case existing != nil:
		if existing.Spec.IntentRef.UID != p.in.UID {
			return nil, fmt.Errorf("run %s: %w", name, errNotOwned)
		}
		if existing.Spec.Stage != stage || (evaluation != nil &&
			(existing.Spec.Evaluation == nil || existing.Spec.Evaluation.UID != evaluation.UID)) {
			return nil, fmt.Errorf("run %s exists for another %s round: %w", name, existing.Spec.Stage,
				errRunNameTaken)
		}
		run = existing
	}
	if _, err := p.ensureConfigMap(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: run.Spec.Inputs.ConfigMap, Namespace: run.Namespace,
			Labels:          map[string]string{v1alpha1.LabelIntent: p.in.Name, v1alpha1.LabelIntentRun: run.Name},
			OwnerReferences: []metav1.OwnerReference{runOwner(run)},
		},
		Immutable: new(true),
		Data:      data,
	}, run.UID); err != nil {
		return nil, err
	}
	return run, nil
}

// judgeEvaluation settles a collected round: pv's facts (its ledger,
// replays and confirmations) and run's claims (its report; nil on a replay
// round, which has no evaluator). Until maul judges them (acceptance.Decide,
// S1-20) every outcome is unsure, the verification Unconfirmed: nothing is
// confirmed that was not judged.
func (p *pass) judgeEvaluation(ctx context.Context, pv *v1alpha1.PreviewVerification, run *v1alpha1.IntentRun) error {
	var claims string
	if run != nil {
		claims = run.Name
	}
	p.r.log().LogAttrs(ctx, slog.LevelInfo, "evaluation round collected",
		slog.String("intent", p.in.Name), slog.String("verification", pv.Name), slog.String("run", claims),
		slog.Int("ledger_entries", ledgerCount(pv)))
	return p.update(ctx, func(cur *v1alpha1.Intent) error {
		nv := cur.Status.Verification
		clearJudgement(nv)
		nv.State, nv.Outcomes = v1alpha1.VerificationUnconfirmed, nil
		for _, o := range cur.Status.Acceptance.Outcomes {
			nv.Outcomes = append(nv.Outcomes, v1alpha1.OutcomeResult{ID: o.ID, Result: v1alpha1.OutcomeUnsure,
				Reason: evaluationReasonNotJudged})
		}
		return nil
	})
}

// ledgerCount is the number of entries pv's verified ledger holds.
func ledgerCount(pv *v1alpha1.PreviewVerification) int {
	if pv.Status.Ledger == nil {
		return 0
	}
	return int(pv.Status.Ledger.Count)
}

// evaluationCurrent re-reads, uncached, what an evaluate run is launched
// against: its PreviewVerification is the one it pins (by UID), running,
// its tool pod ready at the address the run pins, and still the Intent's
// verification in flight at the same head set. A refusal says why and
// launches nothing: a stale verification never gets an evaluator.
func (r *RunReconciler) evaluationCurrent(ctx context.Context, run *v1alpha1.IntentRun) (*result, error) {
	ev := run.Spec.Evaluation
	stale := func(why string) *result {
		return &result{outcome: OutcomeAborted, detail: "the evaluation is no longer current: " + why +
			"; nothing was launched"}
	}
	if ev == nil {
		return stale("the run pins no verification"), nil
	}
	var pv v1alpha1.PreviewVerification
	err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: run.Namespace, Name: ev.Verification}, &pv)
	switch {
	case kerrors.IsNotFound(err):
		return stale("its verification " + ev.Verification + " is gone"), nil
	case err != nil:
		return nil, err
	}
	t := pv.Status.Tools
	switch {
	case pv.UID != ev.UID || !pv.DeletionTimestamp.IsZero():
		return stale("its verification " + ev.Verification + " was replaced"), nil
	case pv.Status.Phase != v1alpha1.VerificationRunning:
		return stale(fmt.Sprintf("its verification is %s", pv.Status.Phase)), nil
	case t == nil || !t.Ready || t.PodIP != ev.ToolIP:
		return stale("its tool pod is not ready at " + ev.ToolIP), nil
	}
	var in v1alpha1.Intent
	if err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: run.Namespace, Name: run.Spec.IntentRef.Name},
		&in); err != nil {
		return nil, err
	}
	v := in.Status.Verification
	if in.UID != run.Spec.IntentRef.UID || v == nil || v.Ref == nil || v.Ref.UID != pv.UID ||
		v.HeadSetDigest != pv.Spec.HeadSetDigest {
		return stale("the intent no longer verifies at its heads"), nil
	}
	return nil, nil
}

// setEvaluatorUnverified sets cur's EvaluatorUnverified condition: True on
// a Project the evaluator would verify whose harness has no verified
// evaluator (Settings.evaluatorUnverified), absent otherwise. Advisory,
// never part of Ready; it depends on the spec and the flags only.
func setEvaluatorUnverified(cur *v1alpha1.Project, s Settings) {
	if !s.evaluatorUnverified(cur) {
		meta.RemoveStatusCondition(&cur.Status.Conditions, v1alpha1.ConditionEvaluatorUnverified)
		return
	}
	meta.SetStatusCondition(&cur.Status.Conditions, metav1.Condition{Type: v1alpha1.ConditionEvaluatorUnverified,
		Status: metav1.ConditionTrue, Reason: "CodexEvaluatorOff", ObservedGeneration: cur.Generation,
		Message: "this Project runs on codex, and maul has not verified its codex evaluator live " +
			"(intent-controller runs without --intent-evaluate-codex): its intents are not evaluated, and " +
			"their preview is handed over at once"})
}
