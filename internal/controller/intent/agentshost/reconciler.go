// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentshost

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/attribution"
	"github.com/devthenet-labs/maul/internal/jobs"
	"github.com/devthenet-labs/maul/internal/runnerguard"
)

const (
	// grace bounds each wait that is not the evaluation's own: the pod
	// being created, scheduled and its images pulled, its log becoming
	// readable after it finished, and the Job's own deadline.
	grace = 2 * time.Minute
	// linger is how long a finished tool Job outlives its settled
	// verification, so the isolation probe's ledger row (S2-21), which
	// reads the tools container's log and termination message once it
	// terminated, can still read them. A Job still running at settle is
	// deleted at once.
	linger = 10 * time.Minute
	// deadlineMargin is what toolpod.Deadlines adds to a verification's
	// timeout for the Job's deadline; the bound passed never clamps it.
	deadlineMargin = 6 * time.Minute
	// maxObject caps an agents verification as written: its ledger index
	// and confirmed bodies, as a slot outcome verification's.
	maxObject = 5 << 18
	// restartBound is how many restarts of a container, its last exit in
	// error, make a crash loop between the kubelet's back-offs.
	restartBound = 3
)

// Settings are the agents host's: intent-controller's S2 flags.
type Settings struct {
	// Namespace is the release namespace the verifications live in.
	Namespace string
	// ToolsImage is maul's digest-pinned preview-tools image.
	ToolsImage string
	// MaxToolPods bounds the live tool pods (Q31).
	MaxToolPods int
	// BuildTimeoutMax bounds each tree's build.
	BuildTimeoutMax time.Duration
	// Resources are the tool pod's containers'.
	Resources jobs.ToolPodResources
	// PollInterval paces a running verification's observation.
	PollInterval time.Duration
	// Harness and CodexHarness are the runner IDs a build runs on: claude's
	// and, with --intent-codex, codex's.
	Harness, CodexHarness string
	// Attribution is --attribution-mode: under attribute every tool pod is
	// annotated the org's (jobs.ToolPodSpec.AttributedTo), as the chart's
	// Job policy requires of every Job intent-controller creates; off, none.
	Attribution attribution.Mode
}

// Reconciler runs agents PreviewVerifications (the package doc). Its
// Client is the manager's: verifications and IntentRuns from the cache,
// status writes to the verification's status subresource only; everything
// a request is re-checked against is read through APIReader, uncached.
type Reconciler struct {
	client.Client
	APIReader client.Reader
	Tools     ToolPods
	// Images is the guard a build launch decides its image and profile by;
	// its Breaker is tripped by a tool pod whose prepare found egress open.
	Images   runnerguard.Guard
	Settings Settings
	Now      func() time.Time
	Log      *slog.Logger
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Reconciler) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.New(slog.DiscardHandler)
}

func (r *Reconciler) wait() ctrl.Result {
	return ctrl.Result{RequeueAfter: max(r.Settings.PollInterval, time.Second)}
}

// settled reports whether a phase is final: nothing writes it again.
func settled(phase v1alpha1.PreviewVerificationPhase) bool {
	switch phase {
	case v1alpha1.VerificationObserved, v1alpha1.VerificationEnvFailed, v1alpha1.VerificationInfra,
		v1alpha1.VerificationStale:
		return true
	}
	return false
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var pv v1alpha1.PreviewVerification
	if err := r.Get(ctx, req.NamespacedName, &pv); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !pv.Spec.IsAgents() || !pv.DeletionTimestamp.IsZero() {
		// A slot verification is preview-controller's; a deleted one's
		// tool pod is the sweep's.
		return ctrl.Result{}, nil
	}
	if settled(pv.Status.Phase) {
		return r.teardown(ctx, &pv)
	}
	s, reason, err := r.check(ctx, &pv)
	switch {
	case err != nil:
		return ctrl.Result{}, err
	case reason != "":
		return ctrl.Result{}, r.settle(ctx, &pv, v1alpha1.VerificationStale, reason)
	case pv.Status.Phase == v1alpha1.VerificationRunning:
		return r.observe(ctx, &pv)
	}
	return r.start(ctx, &pv, s)
}

// settle writes the attempt's final phase and reason, held to maxObject,
// then tears its tool pod down (teardown).
func (r *Reconciler) settle(ctx context.Context, pv *v1alpha1.PreviewVerification,
	phase v1alpha1.PreviewVerificationPhase, reason v1alpha1.PreviewVerificationReason) error {
	t := metav1.NewTime(r.now())
	pv.Status.Phase, pv.Status.Reason, pv.Status.CompletedAt = phase, reason, &t
	fitStatus(pv)
	if err := r.Status().Update(ctx, pv); err != nil {
		return err
	}
	r.log().LogAttrs(ctx, slog.LevelInfo, "agents verification settled", slog.String("verification", pv.Name),
		slog.String("phase", string(pv.Status.Phase)), slog.String("reason", string(pv.Status.Reason)))
	_, err := r.teardown(ctx, pv)
	return err
}

// teardown deletes a settled verification's tool Job and its Secret: at
// once while the Job still runs (or never finished), linger after the
// verification settled once it finished, so the isolation probe can still
// read the finished pod. Deleting what is gone is no error.
func (r *Reconciler) teardown(ctx context.Context, pv *v1alpha1.PreviewVerification) (ctrl.Result, error) {
	name := JobName(pv)
	job, _, err := r.Tools.Get(ctx, name)
	if err != nil {
		return ctrl.Result{}, err
	}
	if job == nil {
		return ctrl.Result{}, r.deleteLoneSecret(ctx, pv, name)
	}
	if job.Labels[v1alpha1.LabelPreviewVerification] != string(pv.UID) {
		return ctrl.Result{}, nil // another verification's: never this one's to delete
	}
	if _, finished := jobFinished(job); finished && pv.Status.CompletedAt != nil {
		if left := pv.Status.CompletedAt.Add(linger).Sub(r.now()); left > 0 {
			return ctrl.Result{RequeueAfter: left}, nil
		}
	}
	return ctrl.Result{}, r.Tools.Delete(ctx, name)
}

// deleteLoneSecret deletes pv's tool Secret when no Job holds it (the
// controller stopped between CreateToolPod's Secret and its Job, so no
// owner reference takes it with a Job): only pv's own, by its label.
func (r *Reconciler) deleteLoneSecret(ctx context.Context, pv *v1alpha1.PreviewVerification, name string) error {
	_, uid, found, err := r.Tools.Key(ctx, name)
	if err != nil || !found || uid != string(pv.UID) {
		return err
	}
	return r.Tools.Delete(ctx, name)
}

// fitStatus holds pv to maxObject: log tails go first; one still over is
// maul's trouble, its ledger fields dropped.
func fitStatus(pv *v1alpha1.PreviewVerification) {
	for i := len(pv.Status.Environment) - 1; i >= 0 && jsonSize(pv) > maxObject; i-- {
		pv.Status.Environment[i].LogTail = ""
	}
	if n := jsonSize(pv); n >= 0 && n <= maxObject {
		return
	}
	pv.Status.Ledger, pv.Status.Replay, pv.Status.Confirmations, pv.Status.Confirmed = nil, nil, nil, nil
	pv.Status.Base = nil
	pv.Status.Phase = v1alpha1.VerificationInfra
	pv.Status.Reason = v1alpha1.VerificationReasonLedgerMismatch
}

// jsonSize is v's JSON size, -1 when it does not encode.
func jsonSize(v any) int {
	b, err := json.Marshal(v)
	if err != nil {
		return -1
	}
	return len(b)
}

// agentsPlacement admits an agents PreviewVerification: a slot one is
// preview-controller's, never this host's to fetch, settle or count.
func agentsPlacement(obj client.Object) bool {
	pv, ok := obj.(*v1alpha1.PreviewVerification)
	return ok && pv.Spec.IsAgents()
}

// SetupWithManager watches the agents PreviewVerifications in the release
// namespace, one at a time, so the tool pod bound is counted consistently.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.PreviewVerification{}, builder.WithPredicates(predicate.NewPredicateFuncs(agentsPlacement))).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Named("agents-host").Complete(r)
}
