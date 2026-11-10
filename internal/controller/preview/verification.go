// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance/crdconv"
	"github.com/devthenet-labs/maul/internal/httpstep"
)

// The verification reconciler runs one PreviewVerification at a time in its
// Preview's slot and writes what it observed, never a verdict. Before it
// renders the slot tool pod (toolrun.go) it re-checks the request against
// everything it can see (spec section 5.6): the Intent owns it, the Preview
// is the Intent's, Ready at the requested generation and heads, its
// environment re-derives from the Project, and the outcomes and saved checks
// are exactly the Intent's approved ones. Any mismatch is Stale and renders
// nothing. It then waits, bounded, for the rollout to settle and the edge
// policies to be in place, renders the tool pod, and once it has finished
// reads its ledger, captures each part's container facts and log tail, and
// settles the attempt: Observed, EnvFailed (the application's environment
// failed, charged to the intent), Infra (maul's own trouble, retried by
// intent-controller as a new attempt) or Stale. A settled attempt's Job is
// deleted, so the slot's one Job of quota is free for the next.
//
// A verification of slice 1's criteria (spec.criteria, no outcomes) is no
// longer run (S4): it is answered Stale stale-criteria before anything else
// is read, and a tester Job an earlier release rendered for it is deleted
// with it.

const (
	// verifyGrace bounds each wait that is not the application's: the
	// rollout settling and the policies landing before the tool pod starts,
	// its Pod being created, scheduled and its images pulled, its log
	// becoming readable after it finished, and the Job's own deadline.
	verifyGrace = 2 * time.Minute
)

// VerificationReconciler runs PreviewVerifications in the release
// namespace. Its Client is the uncached one the preview Reconciler uses: it
// reads the Intent, Project and Preview of each request fresh, and only
// Jobs, Pods and NetworkPolicies in slots. Its status writes are the
// PreviewVerification status subresource's only.
type VerificationReconciler struct {
	client.Client
	Settings Settings
	// Logs reads the tool pod's ledger and the components' log tails.
	Logs LogReader
	// DeadlineMax bounds the tool Job's deadline, the bound the slot Job
	// admission policy holds it to (chart preview.verification.
	// deadlineMaxSeconds); a request asking more is clamped. Zero is the
	// schema's own bound.
	DeadlineMax time.Duration
	Now         func() time.Time
	Log         *slog.Logger

	// toolStates is each running tool pod's state (toolrun.go), guarded
	// by toolsMu: BeforeRelease runs on the preview reconciler's goroutine.
	toolsMu    sync.Mutex
	toolStates map[types.UID]*toolState
}

func (v *VerificationReconciler) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

func (v *VerificationReconciler) wait() ctrl.Result {
	return ctrl.Result{RequeueAfter: v.Settings.PollInterval}
}

// workload is the preview Reconciler's view of the slot, for the helpers
// the two share (the edge policies' state).
func (v *VerificationReconciler) workload() *Reconciler {
	return &Reconciler{Client: v.Client, Settings: v.Settings}
}

// verifySubject is what a request was re-checked against.
type verifySubject struct {
	intent  *v1alpha1.Intent
	preview *v1alpha1.Preview
	project *v1alpha1.Project
	slot    int32
}

func (v *VerificationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var pv v1alpha1.PreviewVerification
	if err := v.Get(ctx, req.NamespacedName, &pv); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if pv.Spec.IsAgents() {
		return ctrl.Result{}, nil // the agents host's: no slot, no Preview to judge it against
	}
	if !pv.DeletionTimestamp.IsZero() {
		// Its Job and its evaluator ingress policy, if any,
		// are the sweep's: its verification is gone.
		return ctrl.Result{}, nil
	}
	if settled(pv.Status.Phase) {
		return ctrl.Result{}, v.teardown(ctx, &pv)
	}
	if sliceOne(&pv) {
		v.logRefusal(ctx, &pv, "slice-1 criteria verification not run: criteria no longer run, "+
			"see docs/intents/evaluator.md#migrating-from-slice-1", nil)
		return ctrl.Result{}, v.settle(ctx, &pv, v1alpha1.VerificationStale, v1alpha1.VerificationReasonStaleCriteria)
	}
	s, reason, err := v.check(ctx, &pv)
	switch {
	case err != nil:
		return ctrl.Result{}, err
	case reason == v1alpha1.VerificationReasonPreviewFailed && pv.Status.Phase == v1alpha1.VerificationRunning:
		return ctrl.Result{}, v.capture(ctx, &pv, s.preview, s.slot)
	case reason != "":
		return ctrl.Result{}, v.settle(ctx, &pv, v1alpha1.VerificationStale, reason)
	case pv.Status.Phase == v1alpha1.VerificationRunning:
		return v.observeTools(ctx, &pv, s)
	}
	return v.startTools(ctx, &pv, s)
}

// sliceOne reports a verification of slice 1's criteria: one with no
// outcomes (the schema admits exactly one of criteria and outcomes).
func sliceOne(pv *v1alpha1.PreviewVerification) bool {
	return len(pv.Spec.Outcomes) == 0
}

// check re-checks the request against its Intent, Preview and Project
// (pre-checks 1, 2 and 4 of spec section 5.6) and returns the reason it is
// stale, or VerificationReasonPreviewFailed when the Preview it ran against
// has failed or expired.
func (v *VerificationReconciler) check(ctx context.Context,
	pv *v1alpha1.PreviewVerification) (*verifySubject, v1alpha1.PreviewVerificationReason, error) {
	s := &verifySubject{intent: &v1alpha1.Intent{}, preview: &v1alpha1.Preview{}, project: &v1alpha1.Project{}}
	stale, err := v.get(ctx, pv.Spec.IntentRef.Name, s.intent)
	if err != nil {
		return nil, "", err
	}
	if stale || s.intent.UID != pv.Spec.IntentRef.UID || !controlledBy(pv, s.intent) {
		return s, v1alpha1.VerificationReasonStaleCriteria, nil
	}
	if stale, err = v.get(ctx, pv.Spec.PreviewRef.Name, s.preview); err != nil || stale {
		return s, v1alpha1.VerificationReasonStalePreview, err
	}
	if stale, err = v.get(ctx, s.intent.Spec.Project, s.project); err != nil || stale {
		return s, v1alpha1.VerificationReasonStalePreview, err
	}
	reason := previewMatches(pv, s, v.Settings.SlotCount)
	if slot := s.preview.Status.Slot; slot != nil && *slot >= 0 && int(*slot) < v.Settings.SlotCount {
		s.slot = *slot
	} else if reason == v1alpha1.VerificationReasonPreviewFailed {
		reason = v1alpha1.VerificationReasonStalePreview
	}
	if reason != "" {
		return s, reason, nil
	}
	if !approvedOutcomes(pv, s.intent) {
		return s, v1alpha1.VerificationReasonStaleCriteria, nil
	}
	return s, "", nil
}

// get reads a release-namespace object by name, reporting one that is gone.
func (v *VerificationReconciler) get(ctx context.Context, name string, obj client.Object) (bool, error) {
	err := v.Get(ctx, types.NamespacedName{Namespace: v.Settings.Namespace, Name: name}, obj)
	if kerrors.IsNotFound(err) {
		return true, nil
	}
	return false, err
}

// controlledBy reports whether the Intent is the verification's controller.
func controlledBy(pv *v1alpha1.PreviewVerification, in *v1alpha1.Intent) bool {
	owner := metav1.GetControllerOf(pv)
	return owner != nil && owner.Kind == "Intent" && owner.UID == in.UID && owner.Name == in.Name
}

// previewMatches holds the Preview to the request: the Intent's own, its
// spec re-derived from the Project (environment included), Ready in a slot
// at the requested generation, its spec and status running exactly the
// requested heads of the Project's repositories.
func previewMatches(pv *v1alpha1.PreviewVerification, s *verifySubject,
	slots int) v1alpha1.PreviewVerificationReason {
	p, in := s.preview, s.intent
	stale := v1alpha1.VerificationReasonStalePreview
	switch {
	case p.UID != pv.Spec.PreviewRef.UID || p.Spec.IntentRef.UID != in.UID || !p.DeletionTimestamp.IsZero():
		return stale
	case p.Spec.Environment == nil || !p.Spec.Environment.Verification || !matchesApprovedPreview(p, in, s.project):
		return stale
	case p.Generation != pv.Spec.PreviewGeneration || p.Status.ObservedGeneration != p.Generation:
		return stale
	case p.Status.Phase == v1alpha1.PreviewFailed || p.Status.Phase == v1alpha1.PreviewExpired:
		return v1alpha1.VerificationReasonPreviewFailed
	case p.Status.Phase != v1alpha1.PreviewReady || p.Status.Slot == nil || *p.Status.Slot < 0 ||
		int(*p.Status.Slot) >= slots:
		return stale
	}
	if !headsMatch(pv.Spec.Heads, p, s.project) {
		return stale
	}
	return ""
}

// headsMatch reports whether heads are exactly the Preview's components, in
// order, each at the revision its spec renders and its Ready status serves,
// of the Project repository of its name.
func headsMatch(heads []v1alpha1.VerificationHead, p *v1alpha1.Preview, project *v1alpha1.Project) bool {
	if len(heads) != len(p.Spec.Components) || len(p.Status.Components) != len(heads) {
		return false
	}
	for i, h := range heads {
		c, st := p.Spec.Components[i], p.Status.Components[i]
		repo := slices.IndexFunc(project.Spec.Repositories, func(r v1alpha1.ProjectRepository) bool {
			return r.Name == h.Component
		})
		if h.Component != c.Name || h.Revision != c.Revision || st.Name != c.Name || st.Revision != c.Revision ||
			repo < 0 || !strings.EqualFold(project.Spec.Repositories[repo].URL, h.Repository) {
			return false
		}
	}
	return true
}

// capture settles a running verification whose Preview is being released
// (it failed, expired or retries) from what the slot shows now: a failing
// part of the change is the intent's, with its own reason; else the
// Preview's failure is.
func (v *VerificationReconciler) capture(ctx context.Context, pv *v1alpha1.PreviewVerification,
	p *v1alpha1.Preview, slot int32) error {
	job, err := v.liveJob(ctx, pv, p, slot)
	if err != nil {
		return err
	}
	var pod *corev1.Pod
	if job != nil {
		if pod, err = v.jobPod(ctx, job); err != nil && !errors.Is(err, errJobPods) {
			return err
		}
	}
	facts, err := v.environment(ctx, p, slot, pod, pv)
	if err != nil {
		return err
	}
	phase, reason := v1alpha1.VerificationEnvFailed, v1alpha1.VerificationReasonPreviewFailed
	parts := crdconv.PartFacts(facts)
	switch httpstep.ClassifyEnvironment(parts) {
	case httpstep.EnvAppFailure:
		reason = appFailureReason(facts, httpstep.AppFailureParts(parts))
	case httpstep.EnvInfra:
		phase = v1alpha1.VerificationInfra
	}
	pv.Status.Environment = facts
	return v.settleWith(ctx, pv, phase, reason)
}

// BeforeRelease is the preview Reconciler's hook: before a Preview's slot
// is torn down (a retry, a failure, an expiry), each of its running
// verifications records what the slot shows while it still can. Settled
// ones are left alone, so it may run again at every poll.
func (v *VerificationReconciler) BeforeRelease(ctx context.Context, p *v1alpha1.Preview) error {
	if p.Status.Slot == nil {
		return nil
	}
	var list v1alpha1.PreviewVerificationList
	if err := v.List(ctx, &list, client.InNamespace(v.Settings.Namespace)); err != nil {
		return err
	}
	for i := range list.Items {
		pv := &list.Items[i]
		if pv.Spec.IsAgents() || pv.Spec.PreviewRef.UID != p.UID || pv.Status.Phase != v1alpha1.VerificationRunning ||
			!pv.DeletionTimestamp.IsZero() || sliceOne(pv) {
			continue // a slice-1 verification is its own pass's to settle stale
		}
		if err := v.capture(ctx, pv, p, *p.Status.Slot); err != nil {
			return err
		}
	}
	return nil
}

func (v *VerificationReconciler) logRefusal(ctx context.Context, pv *v1alpha1.PreviewVerification, msg string,
	err error) {
	if v.Log == nil {
		return
	}
	name := ""
	if pv != nil {
		name = pv.Name
	}
	v.Log.LogAttrs(ctx, slog.LevelWarn, msg, slog.String("verification", name), slog.Any("error", err))
}

// slotPlacement admits a PreviewVerification that runs in a slot. An agents
// one (placement agents; unset is slot) belongs to intent-controller's host
// and is never this controller's to fetch, settle, sweep or count.
func slotPlacement(obj client.Object) bool {
	pv, ok := obj.(*v1alpha1.PreviewVerification)
	return ok && !pv.Spec.IsAgents()
}

// SetupWithManager watches the slot PreviewVerifications in the release
// namespace, one at a time, so one attempt's Job never races another's for
// the slot.
func (v *VerificationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.PreviewVerification{}, builder.WithPredicates(predicate.NewPredicateFuncs(slotPlacement))).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Named("preview-verification").Complete(v)
}
