// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"slices"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/agentresult"
	"github.com/bitwise-media-group/patchy/internal/envelope"
	"github.com/bitwise-media-group/patchy/internal/jobs"
	"github.com/bitwise-media-group/patchy/internal/runnerguard"
	"github.com/bitwise-media-group/patchy/internal/templates"
)

// updateRun writes the run's status as mutate leaves it, over the version in
// hand; on success run is the written version.
func (r *RunReconciler) updateRun(ctx context.Context, run *v1alpha1.IntentRun,
	mutate func(*v1alpha1.IntentRun)) error {
	cur := run.DeepCopy()
	mutate(cur)
	cur.Status.ObservedGeneration = cur.Generation
	if err := r.Status().Update(ctx, cur); err != nil {
		return fmt.Errorf("update run %s: %w", run.Name, err)
	}
	*run = *cur
	return nil
}

// podUsage is the usage a run records from its pod's stage report. The
// envelope is the pod's stdout, and a build runs in the repository's own
// image, so its counts are untrusted like its outcome (podOutcome): a
// negative count is recorded as zero. The cost is never negative already
// (agentresult.FormatCost drops it); an absurd one is summed with saturation
// (syncUsage).
func podUsage(st *envelope.Stage) v1alpha1.UsageSummary {
	u := agentresult.FromStage(st).Usage
	for _, n := range []*int64{&u.InputTokens, &u.OutputTokens, &u.CacheReadTokens, &u.CacheCreationTokens} {
		*n = max(*n, 0)
	}
	return u
}

// maxRunTurns is the schema's bound on status.numTurns.
const maxRunTurns = 100000

// sessionIDPattern is the schema's pattern on status.sessionID, which is at
// most maxSessionID characters.
var sessionIDPattern = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z_-]*$`)

const maxSessionID = 64

// recordPodFacts writes what the pod's stage report says about the run onto
// its status: the usage (podUsage), the turns, the session and what became
// of a cut-off first run. Every one is untrusted, written by a pod that may
// run on the repository's own image, so each is held to the schema rather
// than refused whole: the turns clamped into its range, a session id it would
// refuse dropped, and a wrap-up with a reason outside its vocabulary dropped.
// None of them is read back to decide anything: the wrap-up is shown to
// people, never used to count attempts.
func recordPodFacts(s *v1alpha1.IntentRunStatus, st *envelope.Stage) {
	s.Usage = podUsage(st)
	s.NumTurns = int32(min(max(st.NumTurns, 0), maxRunTurns))
	s.SessionID = ""
	if len(st.SessionID) <= maxSessionID && sessionIDPattern.MatchString(st.SessionID) {
		s.SessionID = st.SessionID
	}
	s.WrapUp = podWrapUp(st.WrapUp)
}

// podWrapUp is the run's record of a pod's wrap-up report, or nil for none
// or one whose reason the schema does not know.
func podWrapUp(w *envelope.WrapUp) *v1alpha1.IntentRunWrapUp {
	if w == nil {
		return nil
	}
	reason := v1alpha1.WrapUpReason(w.Reason)
	switch reason {
	case v1alpha1.WrapUpMaxTurns, v1alpha1.WrapUpBudget, v1alpha1.WrapUpDeadline, v1alpha1.WrapUpIdle:
	default:
		return nil
	}
	return &v1alpha1.IntentRunWrapUp{Reason: reason, Resumed: w.Resumed, Kept: w.Kept}
}

// runCutOff is what the plan comment and the pull request body say of a run
// whose agent was cut off by a limit and whose report was kept all the same
// (status.wrapUp.kept); nil for any other run, which renders them exactly as
// before. It only ever adds patchy's own words about the run: the wrap-up is
// the pod's report, shown to people, never trusted to decide anything.
func runCutOff(run *v1alpha1.IntentRun) *templates.CutOff {
	w := run.Status.WrapUp
	if w == nil || !w.Kept {
		return nil
	}
	return &templates.CutOff{Reason: string(w.Reason), Resumed: w.Resumed}
}

// settle stamps a run's terminal status (Complete, or Failed with the
// outcome and detail), then deletes a plan run's Repository: only a build's
// is kept, as the intent's runner-image anchor.
func (r *RunReconciler) settle(ctx context.Context, run *v1alpha1.IntentRun, res result) error {
	if err := r.updateRun(ctx, run, func(cur *v1alpha1.IntentRun) {
		now := metav1.NewTime(r.now())
		cur.Status.Phase = v1alpha1.RunFailed
		condStatus := metav1.ConditionFalse
		if res.complete {
			cur.Status.Phase = v1alpha1.RunComplete
			condStatus = metav1.ConditionTrue
		}
		cur.Status.Outcome = res.outcome
		cur.Status.Detail = agentresult.TruncateDetail(res.detail)
		if !res.keep {
			cur.Status.Report = agentresult.TruncateReport(res.report)
			if res.transcript != nil {
				cur.Status.Transcript = res.transcript
			}
			if res.stage != nil {
				recordPodFacts(&cur.Status, res.stage)
			}
		}
		cur.Status.FinishedAt = &now
		reason := res.outcome
		if reason == "" {
			reason = "Unknown"
		}
		meta.SetStatusCondition(&cur.Status.Conditions, metav1.Condition{
			Type: v1alpha1.ConditionComplete, Status: condStatus, Reason: reason,
			Message: cur.Status.Detail, ObservedGeneration: cur.Generation,
		})
		if res.sandboxRefused {
			meta.SetStatusCondition(&cur.Status.Conditions, runnerguard.RefusedCondition(cur.Generation))
		}
		if meta.IsStatusConditionTrue(cur.Status.Conditions, v1alpha1.ConditionPushHeld) {
			meta.SetStatusCondition(&cur.Status.Conditions, metav1.Condition{
				Type: v1alpha1.ConditionPushHeld, Status: metav1.ConditionFalse, Reason: "Settled",
				Message: "the run settled: " + reason, ObservedGeneration: cur.Generation,
			})
		}
	}); err != nil {
		return err
	}
	r.log().LogAttrs(ctx, slog.LevelInfo, "intent run finished",
		slog.String("run", run.Name), slog.String("phase", string(run.Status.Phase)),
		slog.String("outcome", run.Status.Outcome))
	return r.deletePlanRepository(ctx, run)
}

// deletePlanRepository deletes a finished plan run's Repository, and each of
// its tree Repositories, which nothing needs once the run is collected (what
// the trees were is kept on the run's status.trees); only a build's is kept,
// as the intent's runner-image anchor. Only a Repository the run owns is
// deleted, never another object under its name.
func (r *RunReconciler) deletePlanRepository(ctx context.Context, run *v1alpha1.IntentRun) error {
	if run.Spec.Stage != v1alpha1.IntentStagePlan {
		return nil
	}
	for _, name := range runRepositoryNames(run) {
		var repo v1alpha1.Repository
		if err := r.Get(ctx, types.NamespacedName{Namespace: run.Namespace, Name: name}, &repo); err != nil {
			if kerrors.IsNotFound(err) {
				continue
			}
			return err
		}
		if !repo.DeletionTimestamp.IsZero() || !controlledBy(repo.OwnerReferences, run.UID) {
			continue
		}
		uid := repo.UID
		if err := r.Delete(ctx, &repo, &client.DeleteOptions{
			Preconditions: &metav1.Preconditions{UID: &uid},
		}); err != nil && !kerrors.IsNotFound(err) {
			return fmt.Errorf("delete the plan repository %s: %w", repo.Name, err)
		}
	}
	return nil
}

// finalize deletes the run's Job (its Secret goes with it) and releases the
// jobs finalizer: an Intent's foreground deletion waits for this.
func (r *RunReconciler) finalize(ctx context.Context, run *v1alpha1.IntentRun) error {
	job := jobs.NameFor(run.Name, KindIntent, run.Spec.Attempt)
	if run.Status.JobRef != nil {
		job = run.Status.JobRef.Name
	}
	if err := r.Jobs.Delete(ctx, job); err != nil && !kerrors.IsNotFound(err) {
		return fmt.Errorf("delete job %s: %w", job, err)
	}
	if !slices.Contains(run.Finalizers, v1alpha1.FinalizerJobs) {
		return nil
	}
	cur := run.DeepCopy()
	cur.Finalizers = slices.DeleteFunc(cur.Finalizers, func(f string) bool { return f == v1alpha1.FinalizerJobs })
	return client.IgnoreNotFound(r.Update(ctx, cur))
}

// SetupWithManager registers the run reconciler. Every run, intent Job,
// intent Repository and run-input ConfigMap event also reaches the singleton
// scheduler request, since each can make a pending run launchable; an
// Intent's change reaches its runs, which abort when it has ended.
func (r *RunReconciler) SetupWithManager(mgr ctrl.Manager) error {
	scheduler := func(ns string) ctrl.Request {
		return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: runSchedulerRequest}}
	}
	ns := r.Settings.Namespace
	mapSelf := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []ctrl.Request {
		return []ctrl.Request{
			{NamespacedName: client.ObjectKeyFromObject(obj)},
			scheduler(obj.GetNamespace()),
		}
	})
	mapJob := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []ctrl.Request {
		if obj.GetLabels()[v1alpha1.LabelRunKind] != KindIntent {
			return nil
		}
		owner := obj.GetLabels()[v1alpha1.LabelOwner]
		if owner == "" {
			return nil
		}
		return []ctrl.Request{
			{NamespacedName: types.NamespacedName{Namespace: ns, Name: owner}},
			scheduler(ns),
		}
	})
	mapRunChild := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []ctrl.Request {
		run := obj.GetLabels()[v1alpha1.LabelIntentRun]
		if run == "" {
			return nil
		}
		return []ctrl.Request{
			{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: run}},
			scheduler(obj.GetNamespace()),
		}
	})
	mapIntent := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []ctrl.Request {
		var runs v1alpha1.IntentRunList
		if err := mgr.GetClient().List(ctx, &runs, client.InNamespace(obj.GetNamespace()),
			client.MatchingLabels{v1alpha1.LabelIntent: obj.GetName()}); err != nil {
			return nil
		}
		out := []ctrl.Request{scheduler(obj.GetNamespace())}
		for i := range runs.Items {
			if p := runs.Items[i].Status.Phase; p != v1alpha1.RunComplete && p != v1alpha1.RunFailed {
				out = append(out, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&runs.Items[i])})
			}
		}
		return out
	})
	return ctrl.NewControllerManagedBy(mgr).
		Watches(&v1alpha1.IntentRun{}, mapSelf).
		Watches(&batchv1.Job{}, mapJob).
		Watches(&v1alpha1.Repository{}, mapRunChild).
		Watches(&corev1.ConfigMap{}, mapRunChild).
		Watches(&v1alpha1.Intent{}, mapIntent).
		Named("intent-run").
		Complete(r)
}
