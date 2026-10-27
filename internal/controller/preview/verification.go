// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/acceptance/crdconv"
	"github.com/devthenet-labs/maul/internal/previewtester"
)

// The verification reconciler runs one PreviewVerification at a time in its
// Preview's slot and writes what it observed, never a verdict. Before it
// renders the tester it re-checks the request against everything it can see
// (spec section 5.6): the Intent owns it, the Preview is the Intent's, Ready
// at the requested generation and heads, its environment re-derives from the
// Project, and the criteria are exactly the automatable subset, in plan
// order, of the Intent's approved list under its digest. Any mismatch is
// Stale and renders nothing. It then waits, bounded, for the rollout to
// settle and the edge policies to be in place, renders the tester Job, and
// once it has finished reads the tester's log (LogReader, bounded), checks it
// against its summary and termination message and holds it to the spec's
// criteria ids and run tokens, captures each part's container facts and log
// tail, and settles the attempt: Observed, EnvFailed (the application's
// environment failed, charged to the intent), Infra (maul's own trouble,
// retried by intent-controller as a new attempt) or Stale. A settled
// attempt's tester Job is deleted, so the slot's one Job of quota is free for
// the next.

const (
	// verifyGrace bounds each wait that is not the application's: the
	// rollout settling and the policies landing before the tester starts,
	// the tester's Pod being created, scheduled and its image pulled, its
	// log becoming readable after it finished, and the Job's own deadline.
	verifyGrace = 2 * time.Minute
	// maxTesterLog is the most of the tester's log read: well above what
	// MaxCriteria observations take on the wire.
	maxTesterLog = 512 << 10
	// componentLogLimit and componentLogLines bound a component's log read;
	// maxLogTail is the tail of it kept (PartFact.LogTail's maxLength).
	componentLogLimit = 16 << 10
	componentLogLines = 200
	maxLogTail        = 4 << 10
	// maxVerificationObject caps a PreviewVerification as written.
	maxVerificationObject = 256 << 10
	// acceptanceSlice is the slice whose criteria kinds the tester runs.
	acceptanceSlice = 1
)

// VerificationReconciler runs PreviewVerifications in the release
// namespace. Its Client is the uncached one the preview Reconciler uses: it
// reads the Intent, Project and Preview of each request fresh, and only
// Jobs, Pods and NetworkPolicies in slots. Its status writes are the
// PreviewVerification status subresource's only.
type VerificationReconciler struct {
	client.Client
	Settings Settings
	// Logs reads the tester's output and the components' log tails.
	Logs LogReader
	// DeadlineMax bounds the tester Job's deadline, the bound the slot Job
	// admission policy holds it to (chart preview.verification.
	// deadlineMaxSeconds); a request asking more is clamped. Zero is the
	// schema's own bound.
	DeadlineMax time.Duration
	Now         func() time.Time
	Log         *slog.Logger
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
	intent   *v1alpha1.Intent
	preview  *v1alpha1.Preview
	project  *v1alpha1.Project
	criteria []acceptance.Criterion
	slot     int32
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

func (v *VerificationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var pv v1alpha1.PreviewVerification
	if err := v.Get(ctx, req.NamespacedName, &pv); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !pv.DeletionTimestamp.IsZero() {
		// Its tester Job, if any, is the sweep's: its verification is gone.
		return ctrl.Result{}, nil
	}
	if settled(pv.Status.Phase) {
		return ctrl.Result{}, v.deleteTesterJobs(ctx, &pv)
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
		return v.observe(ctx, &pv, s)
	}
	return v.start(ctx, &pv, s)
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
	criteria, ok := approvedSubset(pv, s.intent)
	if !ok {
		return s, v1alpha1.VerificationReasonStaleCriteria, nil
	}
	s.criteria = criteria
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

// approvedSubset decodes the request's criteria and holds them to the
// Intent's approved list: the list digests to its recorded digest and to the
// request's, and the request carries exactly its automatable criteria, in
// plan order, each valid. API objects are compared by their canonical JSON
// (acceptance.Digest), never byte for byte.
func approvedSubset(pv *v1alpha1.PreviewVerification, in *v1alpha1.Intent) ([]acceptance.Criterion, bool) {
	a := in.Status.Acceptance
	if a == nil {
		return nil, false
	}
	approved, err := crdconv.CriteriaFromCRD(a.Criteria)
	if err != nil {
		return nil, false
	}
	requested, err := crdconv.CriteriaFromCRD(pv.Spec.Criteria)
	if err != nil || len(requested) == 0 || acceptance.Validate(requested) != nil {
		return nil, false
	}
	digest := acceptance.Digest(approved)
	auto := acceptance.Automatable(approved, acceptanceSlice)
	if digest != a.Digest || digest != pv.Spec.CriteriaDigest || len(auto) != len(requested) ||
		acceptance.Digest(auto) != acceptance.Digest(requested) {
		return nil, false
	}
	return acceptance.Normalize(requested), true
}

// start renders the tester once the rollout has settled and the edge
// policies are in place (spec section 5.6 pre-check 3), each wait bounded
// by verifyGrace from the request's creation.
func (v *VerificationReconciler) start(ctx context.Context, pv *v1alpha1.PreviewVerification,
	s *verifySubject) (ctrl.Result, error) {
	ready, err := v.rolloutSettled(ctx, s.preview, s.slot)
	if err == nil && ready {
		ready, err = v.workload().policiesInPlace(ctx, s.preview, s.slot)
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if !ready {
		if v.now().Sub(pv.CreationTimestamp.Time) >= verifyGrace {
			return ctrl.Result{}, v.settle(ctx, pv, v1alpha1.VerificationInfra,
				v1alpha1.VerificationReasonRolloutUnsettled)
		}
		return v.wait(), nil
	}
	job, err := v.renderTester(pv, s)
	if err != nil {
		v.logRefusal(ctx, pv, "tester not rendered", err)
		return ctrl.Result{}, v.settle(ctx, pv, v1alpha1.VerificationStale, v1alpha1.VerificationReasonStaleCriteria)
	}
	if err := v.Create(ctx, job); err != nil && !kerrors.IsAlreadyExists(err) {
		if quotaExceeded(err) {
			return ctrl.Result{}, v.settle(ctx, pv, v1alpha1.VerificationInfra,
				v1alpha1.VerificationReasonQuotaRefused)
		}
		if admissionRefused(err) || kerrors.IsInvalid(err) {
			// The vocabulary has no reason for a refused Job; the tester was
			// never scheduled.
			v.logRefusal(ctx, pv, "tester Job refused", err)
			return ctrl.Result{}, v.settle(ctx, pv, v1alpha1.VerificationInfra,
				v1alpha1.VerificationReasonTesterUnscheduled)
		}
		return ctrl.Result{}, err
	}
	t := metav1.NewTime(v.now())
	pv.Status.Phase = v1alpha1.VerificationRunning
	pv.Status.Reason = ""
	pv.Status.StartedAt = &t
	return v.wait(), v.Status().Update(ctx, pv)
}

// renderTester renders the request's tester Job: the spec the tester reads
// (its criteria, run tokens and, per component, the Service its requests go
// to, by the derivation the Preview's Services are named with, on the
// Service port) and its budget below the Job's deadline, under a deadline
// clamped to DeadlineMax.
func (v *VerificationReconciler) renderTester(pv *v1alpha1.PreviewVerification,
	s *verifySubject) (*batchv1.Job, error) {
	targets := make(map[string]string, len(s.preview.Spec.Components))
	for i, c := range s.preview.Spec.Components {
		targets[c.Name] = fmt.Sprintf("%s:%d", v1alpha1.PreviewComponentServiceName(s.preview, i),
			v1alpha1.PreviewServicePort)
	}
	clamped := pv.DeepCopy()
	if limit := cmp.Or(v.DeadlineMax, testerTimeoutMax); clamped.Spec.Timeout.Duration > limit {
		clamped.Spec.Timeout.Duration = limit
	}
	input, err := previewtester.Spec{Round: pv.Spec.Round, Attempt: pv.Spec.Attempt, Run: pv.Spec.Run,
		BudgetSeconds: previewtester.BudgetFor(clamped.Spec.Timeout.Duration),
		Criteria:      s.criteria, Targets: targets}.Encode()
	if err != nil {
		return nil, err
	}
	return v.Settings.testerJob(s.preview, clamped, s.slot, input)
}

// rolloutSettled reports whether every live Pod of each component runs it as
// the Preview's spec renders it now: no Pod of an earlier revision (or
// reset) is left to answer the tester.
func (v *VerificationReconciler) rolloutSettled(ctx context.Context, p *v1alpha1.Preview, slot int32) (bool, error) {
	for i, c := range p.Spec.Components {
		pods, err := v.componentPods(ctx, p, i, slot)
		if err != nil {
			return false, err
		}
		for j := range pods {
			if !runsComponent(&pods[j], p, c) {
				return false, nil
			}
		}
	}
	return true, nil
}

// componentPods are component i's live Pods: not deleting, not finished.
func (v *VerificationReconciler) componentPods(ctx context.Context, p *v1alpha1.Preview, i int,
	slot int32) ([]corev1.Pod, error) {
	var pods corev1.PodList
	if err := v.List(ctx, &pods, client.InNamespace(v.Settings.slotName(slot)),
		client.MatchingLabels(componentLabels(p, i))); err != nil {
		return nil, err
	}
	return slices.DeleteFunc(pods.Items, func(pod corev1.Pod) bool {
		return !pod.DeletionTimestamp.IsZero() || pod.Status.Phase == corev1.PodSucceeded ||
			pod.Status.Phase == corev1.PodFailed
	}), nil
}

// observe follows a running tester: it waits, bounded, for the Job to
// finish, then reads and settles what it observed.
func (v *VerificationReconciler) observe(ctx context.Context, pv *v1alpha1.PreviewVerification,
	s *verifySubject) (ctrl.Result, error) {
	job, err := v.liveTesterJob(ctx, pv, s.preview, s.slot)
	if err != nil {
		return ctrl.Result{}, err
	}
	if job == nil {
		return ctrl.Result{}, v.settle(ctx, pv, v1alpha1.VerificationInfra,
			v1alpha1.VerificationReasonTesterOutputInvalid)
	}
	pod, err := v.testerPod(ctx, job)
	if errors.Is(err, errTesterPods) {
		return ctrl.Result{}, v.settle(ctx, pv, v1alpha1.VerificationInfra,
			v1alpha1.VerificationReasonTesterOutputInvalid)
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	finishedAt, finished := jobFinished(job)
	if !finished {
		if reason := v.testerStuck(pv, pod); reason != "" {
			return ctrl.Result{}, v.settle(ctx, pv, v1alpha1.VerificationInfra, reason)
		}
		return v.wait(), nil
	}
	if pod == nil {
		return ctrl.Result{}, v.settle(ctx, pv, v1alpha1.VerificationInfra,
			v1alpha1.VerificationReasonDeadlineNoSummary)
	}
	log, err := v.Logs.ReadLog(ctx, pod.Namespace, pod.Name, testerContainer,
		LogOptions{LimitBytes: maxTesterLog + 1})
	if err != nil {
		if !kerrors.IsNotFound(err) && v.now().Sub(finishedAt) < verifyGrace {
			v.logRefusal(ctx, pv, "tester log not read yet", err)
			return v.wait(), nil
		}
		return ctrl.Result{}, v.settle(ctx, pv, v1alpha1.VerificationInfra,
			v1alpha1.VerificationReasonTesterOutputInvalid)
	}
	return ctrl.Result{}, v.record(ctx, pv, s, pod, log)
}

// record settles a finished tester's attempt from its output and the
// slot's environment.
func (v *VerificationReconciler) record(ctx context.Context, pv *v1alpha1.PreviewVerification,
	s *verifySubject, pod *corev1.Pod, log []byte) error {
	rt := acceptance.RunTokens{Round: pv.Spec.Round, Attempt: pv.Spec.Attempt, Run: pv.Spec.Run}
	ids := make([]string, len(s.criteria))
	for i, c := range s.criteria {
		ids[i] = c.ID
	}
	obs, sum, outReason := parseTesterOutput(log, terminationMessage(pod), rt, ids)
	facts, err := v.environment(ctx, s.preview, s.slot, pod, startedAt(pv))
	if err != nil {
		return err
	}
	phase, reason := route(facts, outReason)
	if outReason == "" {
		converted, err := crdconv.ObservationsToCRD(obs)
		if err != nil {
			phase, reason = v1alpha1.VerificationInfra, v1alpha1.VerificationReasonTesterOutputInvalid
		}
		pv.Status.Observations = converted
		pv.Status.SummarySHA256 = sum.SHA256
	}
	pv.Status.Environment = facts
	pv.Status.TesterImageID = testerImageID(pod)
	return v.settleWith(ctx, pv, phase, reason)
}

// errTesterPods is a tester Job with more than one Pod, which a run-once
// Job never has.
var errTesterPods = errors.New("tester Job has more than one Pod")

// liveTesterJob is the request's tester Job, nil when it is gone or not the
// one rendered for it.
func (v *VerificationReconciler) liveTesterJob(ctx context.Context, pv *v1alpha1.PreviewVerification,
	p *v1alpha1.Preview, slot int32) (*batchv1.Job, error) {
	var job batchv1.Job
	key := types.NamespacedName{Namespace: v.Settings.slotName(slot), Name: testerJobName(p, pv)}
	if err := v.Get(ctx, key, &job); err != nil {
		if kerrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if !ownedBy(&job, p) || job.Labels[labelVerificationUID] != string(pv.UID) || !job.DeletionTimestamp.IsZero() {
		return nil, nil
	}
	return &job, nil
}

// testerPod is the Pod the Job's controller created for it (by the Job's
// UID, as its controller reference), nil before there is one.
func (v *VerificationReconciler) testerPod(ctx context.Context, job *batchv1.Job) (*corev1.Pod, error) {
	var pods corev1.PodList
	if err := v.List(ctx, &pods, client.InNamespace(job.Namespace),
		client.MatchingLabels{labelVerificationUID: job.Labels[labelVerificationUID]}); err != nil {
		return nil, err
	}
	var found *corev1.Pod
	for i := range pods.Items {
		owner := metav1.GetControllerOf(&pods.Items[i])
		if owner == nil || owner.Kind != "Job" || owner.UID != job.UID {
			continue
		}
		if found != nil {
			return nil, errTesterPods
		}
		found = &pods.Items[i]
	}
	return found, nil
}

// jobFinished reports whether the Job has completed or failed, and when.
func jobFinished(job *batchv1.Job) (time.Time, bool) {
	for _, c := range job.Status.Conditions {
		if (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) && c.Status == corev1.ConditionTrue {
			return c.LastTransitionTime.Time, true
		}
	}
	return time.Time{}, false
}

// testerStuck is why a running tester will not finish, past its grace: no
// Pod created (the slot quota), none scheduled, its image not pulled, or the
// Job's deadline long past. "" while it may yet.
func (v *VerificationReconciler) testerStuck(pv *v1alpha1.PreviewVerification,
	pod *corev1.Pod) v1alpha1.PreviewVerificationReason {
	since := v.now().Sub(startedAt(pv))
	if since < verifyGrace {
		return ""
	}
	switch {
	case pod == nil:
		return v1alpha1.VerificationReasonQuotaRefused
	case unschedulable(pod):
		return v1alpha1.VerificationReasonTesterUnscheduled
	case imagePullWaiting(pod):
		return v1alpha1.VerificationReasonTesterImagePull
	case since >= pv.Spec.Timeout.Duration+verifyGrace:
		return v1alpha1.VerificationReasonDeadlineNoSummary
	}
	return ""
}

// startedAt is when the tester was rendered.
func startedAt(pv *v1alpha1.PreviewVerification) time.Time {
	if pv.Status.StartedAt != nil {
		return pv.Status.StartedAt.Time
	}
	return pv.CreationTimestamp.Time
}

func unschedulable(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse &&
			c.Reason == corev1.PodReasonUnschedulable {
			return true
		}
	}
	return false
}

func imagePullWaiting(pod *corev1.Pod) bool {
	for _, s := range pod.Status.ContainerStatuses {
		if s.State.Waiting != nil && imagePullReason(s.State.Waiting.Reason) {
			return true
		}
	}
	return false
}

func imagePullReason(reason string) bool {
	switch reason {
	case "ImagePullBackOff", "ErrImagePull", "InvalidImageName", "ErrImageNeverPull":
		return true
	}
	return false
}

// terminationMessage is the tester container's termination message.
func terminationMessage(pod *corev1.Pod) string {
	for _, s := range pod.Status.ContainerStatuses {
		if s.Name == testerContainer && s.State.Terminated != nil {
			return s.State.Terminated.Message
		}
	}
	return ""
}

// testerImageID is the image the tester ran, as the kubelet reports it.
func testerImageID(pod *corev1.Pod) string {
	for _, s := range pod.Status.ContainerStatuses {
		if s.Name == testerContainer && len(s.ImageID) <= 512 {
			return s.ImageID
		}
	}
	return ""
}

// parseTesterOutput reads the tester's MAUL-VERIFY v1 log: its lines
// verified against their summary and the termination message, then held to
// the request (id ownership): exactly one observation per requested
// criterion, in the request's order, each with at least one try, each try
// under the run token it was given. The reason is "" for output that holds.
func parseTesterOutput(log []byte, msg string, rt acceptance.RunTokens,
	ids []string) ([]acceptance.CriterionObservation, acceptance.Summary, v1alpha1.PreviewVerificationReason) {
	if len(log) > maxTesterLog {
		return nil, acceptance.Summary{}, v1alpha1.VerificationReasonTesterOutputInvalid
	}
	obs, sum, err := acceptance.ParseWire(log)
	switch {
	case errors.Is(err, acceptance.ErrNoSummary):
		return nil, sum, v1alpha1.VerificationReasonDeadlineNoSummary
	case errors.Is(err, acceptance.ErrSummaryMismatch):
		return nil, sum, v1alpha1.VerificationReasonSummaryMismatch
	case err != nil:
		return nil, sum, v1alpha1.VerificationReasonTesterOutputInvalid
	}
	if term, err := acceptance.ParseSummary(msg); err != nil || term != sum {
		return nil, sum, v1alpha1.VerificationReasonSummaryMismatch
	}
	if len(obs) != len(ids) {
		return nil, sum, v1alpha1.VerificationReasonTesterOutputInvalid
	}
	for i, o := range obs {
		if o.ID != ids[i] || len(o.Tries) == 0 {
			return nil, sum, v1alpha1.VerificationReasonTesterOutputInvalid
		}
		for t, try := range o.Tries {
			if try.Run != rt.Token(t+1) {
				return nil, sum, v1alpha1.VerificationReasonTesterOutputInvalid
			}
		}
	}
	return obs, sum, ""
}

// route settles an attempt from its environment and its output: a part of
// the change failing is the intent's (EnvFailed); trouble the change cannot
// cause is maul's (Infra), as is output that does not hold; else Observed.
// The classification is acceptance.ClassifyEnvironment's, the one
// intent-controller applies to the same facts.
func route(facts []v1alpha1.PartFact,
	outReason v1alpha1.PreviewVerificationReason) (v1alpha1.PreviewVerificationPhase, v1alpha1.PreviewVerificationReason) {
	parts := crdconv.PartFacts(facts)
	switch acceptance.ClassifyEnvironment(parts) {
	case acceptance.EnvAppFailure:
		return v1alpha1.VerificationEnvFailed, appFailureReason(facts, acceptance.AppFailureParts(parts))
	case acceptance.EnvInfra:
		return v1alpha1.VerificationInfra, infraReason(facts, outReason)
	}
	if outReason != "" {
		return v1alpha1.VerificationInfra, outReason
	}
	return v1alpha1.VerificationObserved, v1alpha1.VerificationReasonOK
}

// appFailureReason names how the first failing part failed.
func appFailureReason(facts []v1alpha1.PartFact, failing []string) v1alpha1.PreviewVerificationReason {
	i := slices.IndexFunc(facts, func(f v1alpha1.PartFact) bool { return len(failing) > 0 && f.Name == failing[0] })
	if i < 0 {
		return v1alpha1.VerificationReasonComponentNotReady
	}
	f := facts[i]
	switch {
	case f.LastTermination != nil && f.LastTermination.Reason == "OOMKilled":
		return v1alpha1.VerificationReasonComponentOOM
	case imagePullReason(f.Waiting):
		return v1alpha1.VerificationReasonComponentImagePull
	case f.Waiting != "":
		return v1alpha1.VerificationReasonComponentCrashLoop
	case f.Evicted:
		return v1alpha1.VerificationReasonComponentEvicted
	}
	return v1alpha1.VerificationReasonComponentNotReady
}

// infraReason names maul's trouble: the tester not scheduled or its image
// not pulled, else what was wrong with its output, else a part no node
// would take.
func infraReason(facts []v1alpha1.PartFact,
	outReason v1alpha1.PreviewVerificationReason) v1alpha1.PreviewVerificationReason {
	for _, f := range facts {
		if f.Role != v1alpha1.PreviewRoleTester {
			continue
		}
		switch {
		case f.Unschedulable:
			return v1alpha1.VerificationReasonTesterUnscheduled
		case imagePullReason(f.Waiting):
			return v1alpha1.VerificationReasonTesterImagePull
		}
	}
	if outReason != "" {
		return outReason
	}
	for _, f := range facts {
		if f.Unschedulable && f.Role == v1alpha1.PreviewRoleComponent {
			return v1alpha1.VerificationReasonComponentNotReady
		}
	}
	return v1alpha1.VerificationReasonTesterOutputInvalid
}

// environment captures each part of the slot: every component, from its
// current Pod (the one running it as the spec renders it, else its newest),
// with its log tail since `since`, and the tester when it has a Pod. A Pod
// list that fails is an error, never a part without a Pod: that would charge
// the intent for the API server's trouble.
func (v *VerificationReconciler) environment(ctx context.Context, p *v1alpha1.Preview, slot int32,
	tester *corev1.Pod, since time.Time) ([]v1alpha1.PartFact, error) {
	facts := make([]v1alpha1.PartFact, 0, len(p.Spec.Components)+1)
	for i, c := range p.Spec.Components {
		fact := v1alpha1.PartFact{Name: c.Name, Role: v1alpha1.PreviewRoleComponent}
		pods, err := v.componentPods(ctx, p, i, slot)
		if err != nil {
			return nil, err
		}
		if pod := currentPod(pods, p, c); pod != nil {
			fact = podFact(pod, c.Name, c.Name, v1alpha1.PreviewRoleComponent)
			fact.LogTail = v.logTail(ctx, pod, c.Name, since)
		}
		facts = append(facts, fact)
	}
	if tester != nil {
		facts = append(facts, podFact(tester, v1alpha1.PreviewPartTester, testerContainer, v1alpha1.PreviewRoleTester))
	}
	return facts, nil
}

// currentPod is the Pod running component c as the spec renders it, the
// newest such; else the newest Pod of the component at all.
func currentPod(pods []corev1.Pod, p *v1alpha1.Preview, c v1alpha1.PreviewComponent) *corev1.Pod {
	newest := func(a, b corev1.Pod) int {
		return cmp.Or(b.CreationTimestamp.Compare(a.CreationTimestamp.Time), strings.Compare(a.Name, b.Name))
	}
	slices.SortFunc(pods, newest)
	for i := range pods {
		if runsComponent(&pods[i], p, c) {
			return &pods[i]
		}
	}
	if len(pods) > 0 {
		return &pods[0]
	}
	return nil
}

// podFact is the part's fact from its Pod and its container's status. A
// Job's container (the tester's) reports its current termination, a
// component's its last one.
func podFact(pod *corev1.Pod, name, container string, role v1alpha1.PreviewPartRole) v1alpha1.PartFact {
	f := v1alpha1.PartFact{Name: name, Role: role, Evicted: pod.Status.Reason == "Evicted",
		Unschedulable: unschedulable(pod)}
	i := slices.IndexFunc(pod.Status.ContainerStatuses, func(s corev1.ContainerStatus) bool {
		return s.Name == container
	})
	if i < 0 {
		return f
	}
	s := pod.Status.ContainerStatuses[i]
	f.Ready = s.Ready && podCondition(pod, corev1.PodReady)
	f.Restarts = max(s.RestartCount, 0)
	if s.State.Waiting != nil {
		f.Waiting = kubeletWord(s.State.Waiting.Reason)
	}
	term := s.LastTerminationState.Terminated
	if role != v1alpha1.PreviewRoleComponent {
		term = s.State.Terminated
	}
	if term != nil {
		f.LastTermination = &v1alpha1.Termination{Reason: kubeletWord(term.Reason), ExitCode: term.ExitCode}
	}
	return f
}

// kubeletWord keeps a kubelet reason within the schema's grammar: letters
// and digits, at most 64. Anything else is not a reason the kubelet gives.
func kubeletWord(s string) string {
	if len(s) > 64 || strings.IndexFunc(s, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9')
	}) >= 0 {
		return "Unknown"
	}
	return s
}

// logTail is the end of a component's log since the verification started:
// at most componentLogLines lines and componentLogLimit bytes read, the last
// maxLogTail bytes of it kept, as valid UTF-8. Untrusted and opaque; a log
// that cannot be read leaves none.
func (v *VerificationReconciler) logTail(ctx context.Context, pod *corev1.Pod, container string,
	since time.Time) string {
	if v.Logs == nil {
		return ""
	}
	lines := int64(componentLogLines)
	opts := LogOptions{LimitBytes: componentLogLimit, TailLines: &lines}
	if !since.IsZero() {
		opts.SinceTime = &since
	}
	b, err := v.Logs.ReadLog(ctx, pod.Namespace, pod.Name, container, opts)
	if err != nil {
		return ""
	}
	return lastValidUTF8(b, maxLogTail)
}

// lastValidUTF8 is the last at most n bytes of b, cut on a rune boundary,
// invalid sequences replaced.
func lastValidUTF8(b []byte, n int) string {
	s := strings.ToValidUTF8(string(b), "�")
	if len(s) <= n {
		return s
	}
	cut := len(s) - n
	for cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut++
	}
	return s[cut:]
}

// capture settles a running verification whose Preview is being released
// (it failed, expired or retries) from what the slot shows now: a failing
// part of the change is the intent's, with its own reason; else the
// Preview's failure is.
func (v *VerificationReconciler) capture(ctx context.Context, pv *v1alpha1.PreviewVerification,
	p *v1alpha1.Preview, slot int32) error {
	job, err := v.liveTesterJob(ctx, pv, p, slot)
	if err != nil {
		return err
	}
	var tester *corev1.Pod
	if job != nil {
		if tester, err = v.testerPod(ctx, job); err != nil && !errors.Is(err, errTesterPods) {
			return err
		}
	}
	facts, err := v.environment(ctx, p, slot, tester, startedAt(pv))
	if err != nil {
		return err
	}
	phase, reason := v1alpha1.VerificationEnvFailed, v1alpha1.VerificationReasonPreviewFailed
	parts := crdconv.PartFacts(facts)
	switch acceptance.ClassifyEnvironment(parts) {
	case acceptance.EnvAppFailure:
		reason = appFailureReason(facts, acceptance.AppFailureParts(parts))
	case acceptance.EnvInfra:
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
		if pv.Spec.PreviewRef.UID != p.UID || pv.Status.Phase != v1alpha1.VerificationRunning ||
			!pv.DeletionTimestamp.IsZero() {
			continue
		}
		if err := v.capture(ctx, pv, p, *p.Status.Slot); err != nil {
			return err
		}
	}
	return nil
}

// settle writes a final phase with no observations of its own.
func (v *VerificationReconciler) settle(ctx context.Context, pv *v1alpha1.PreviewVerification,
	phase v1alpha1.PreviewVerificationPhase, reason v1alpha1.PreviewVerificationReason) error {
	return v.settleWith(ctx, pv, phase, reason)
}

// settleWith writes the attempt's final phase and reason, held to the size
// cap, then deletes its tester Job.
func (v *VerificationReconciler) settleWith(ctx context.Context, pv *v1alpha1.PreviewVerification,
	phase v1alpha1.PreviewVerificationPhase, reason v1alpha1.PreviewVerificationReason) error {
	t := metav1.NewTime(v.now())
	pv.Status.Phase = phase
	pv.Status.Reason = reason
	pv.Status.CompletedAt = &t
	fitStatus(pv)
	if err := v.Status().Update(ctx, pv); err != nil {
		return err
	}
	if v.Log != nil {
		v.Log.LogAttrs(ctx, slog.LevelInfo, "preview verification settled",
			slog.String("verification", pv.Name), slog.String("phase", string(pv.Status.Phase)),
			slog.String("reason", string(pv.Status.Reason)))
	}
	return v.deleteTesterJobs(ctx, pv)
}

// fitStatus holds the object to maxVerificationObject, so an application
// cannot turn its own failure into maul's by answering with large values.
// In order, while it is over: body prefixes go, from the last step back (a
// step whose body prefix was dropped still records its body's length), then
// log tails, then the pointer, header and capture values whose judgement
// cannot change (shrinkValues), then whole criteria, largest first, each
// replaced by its TooLarge marker (dropLargest), which acceptance.Judge
// fails as observation-too-large: the application's failure, never a pass,
// and every other criterion keeps its verdict. Markers are a few dozen bytes
// and log tails are gone by then, so no observation content can keep the
// object over; one that somehow still does keeps no observations and is
// maul's trouble.
func fitStatus(pv *v1alpha1.PreviewVerification) {
	excess := objectSize(pv) - maxVerificationObject
	if excess <= 0 {
		return
	}
	obs := pv.Status.Observations
	for i := len(obs) - 1; i >= 0 && excess > 0; i-- {
		for j := len(obs[i].Tries) - 1; j >= 0 && excess > 0; j-- {
			steps := obs[i].Tries[j].Steps
			for k := len(steps) - 1; k >= 0 && excess > 0; k-- {
				excess -= len(steps[k].BodyPrefix)
				steps[k].BodyPrefix = ""
			}
		}
	}
	for i := len(pv.Status.Environment) - 1; i >= 0 && objectSize(pv) > maxVerificationObject; i-- {
		pv.Status.Environment[i].LogTail = ""
	}
	if excess := objectSize(pv) - maxVerificationObject; excess > 0 {
		shrinkValues(pv, excess)
	}
	dropLargest(pv)
	if objectSize(pv) > maxVerificationObject {
		pv.Status.Observations = nil
		pv.Status.SummarySHA256 = ""
		pv.Status.Phase = v1alpha1.VerificationInfra
		pv.Status.Reason = v1alpha1.VerificationReasonTesterOutputInvalid
	}
}

// shrinkValues saves up to excess bytes by replacing recorded pointer,
// header and capture values with acceptance.TooLarge, from the last
// criterion, try and step back, keeping each replacement only when the
// criterion's judgement (acceptance.Judge, as intent-controller judges it)
// stays exactly what it was: a $present value, one that did not match
// anyway, or a capture no later step's request or expectation is built
// from. A value the verdict depends on is never touched, and an observation
// whose criterion is not in the request is left as it is.
func shrinkValues(pv *v1alpha1.PreviewVerification, excess int) {
	cs, err := crdconv.CriteriaFromCRD(pv.Spec.Criteria)
	if err != nil {
		return
	}
	byID := make(map[string]acceptance.Criterion, len(cs))
	for _, c := range cs {
		byID[c.ID] = c
	}
	rt := acceptance.RunTokens{Round: pv.Spec.Round, Attempt: pv.Spec.Attempt, Run: pv.Spec.Run}
	obs := pv.Status.Observations
	for i := len(obs) - 1; i >= 0 && excess > 0; i-- {
		if c, ok := byID[obs[i].ID]; ok {
			excess -= shrinkObservation(c, &obs[i], rt, excess)
		}
	}
}

// shrinkObservation replaces o's values with TooLarge where c's verdict
// over o does not change, until excess bytes are saved; it returns the
// bytes saved.
func shrinkObservation(c acceptance.Criterion, o *v1alpha1.CriterionObservation, rt acceptance.RunTokens,
	excess int) int {
	judged, err := crdconv.ObservationFromCRD(*o)
	if err != nil || len(judged.Tries) != len(o.Tries) {
		return 0
	}
	want := acceptance.Judge(c, judged, rt)
	saved := 0
	for j := len(o.Tries) - 1; j >= 0; j-- {
		for k := len(o.Tries[j].Steps) - 1; k >= 0; k-- {
			step, mirror := &o.Tries[j].Steps[k], &judged.Tries[j].Steps[k]
			for _, m := range [][2]map[string]string{
				{step.Pointers, mirror.Pointers}, {step.Headers, mirror.Headers}, {step.Captures, mirror.Captures},
			} {
				for _, key := range slices.Sorted(maps.Keys(m[0])) {
					gain := jsonSize(m[0][key]) - jsonSize(acceptance.TooLarge)
					if gain <= 0 {
						continue
					}
					m[1][key] = acceptance.TooLarge
					if !reflect.DeepEqual(acceptance.Judge(c, judged, rt), want) {
						m[1][key] = m[0][key]
						continue
					}
					m[0][key] = acceptance.TooLarge
					if saved += gain; saved >= excess {
						return saved
					}
				}
			}
		}
	}
	return saved
}

// dropLargest replaces the largest criterion observation with its
// TooLarge marker, one at a time, until the object fits or every
// observation is a marker.
func dropLargest(pv *v1alpha1.PreviewVerification) {
	obs := pv.Status.Observations
	for objectSize(pv) > maxVerificationObject {
		worst, worstSize := -1, 0
		for i := range obs {
			if n := jsonSize(obs[i]); !obs[i].TooLarge && n > worstSize {
				worst, worstSize = i, n
			}
		}
		if worst < 0 {
			return
		}
		obs[worst] = v1alpha1.CriterionObservation{ID: obs[worst].ID, TooLarge: true}
	}
}

// objectSize is the object's size as the API machinery sends and stores
// it: encoding/json, HTML-escaped, as json.Marshal writes it.
func objectSize(pv *v1alpha1.PreviewVerification) int {
	if n := jsonSize(pv); n >= 0 {
		return n
	}
	return maxVerificationObject + 1
}

// jsonSize is v's length as json.Marshal writes it, as objectSize counts
// it; -1 when it does not encode.
func jsonSize(v any) int {
	b, err := json.Marshal(v)
	if err != nil {
		return -1
	}
	return len(b)
}

// deleteTesterJobs deletes, in the background, every tester Job rendered
// for the verification in any slot: a settled attempt frees the slot's Job
// quota for the next.
func (v *VerificationReconciler) deleteTesterJobs(ctx context.Context, pv *v1alpha1.PreviewVerification) error {
	for slot := range v.Settings.SlotCount {
		var jobs batchv1.JobList
		if err := v.List(ctx, &jobs, client.InNamespace(v.Settings.slotName(int32(slot))),
			client.MatchingLabels{labelManagedBy: managedBy, labelVerificationUID: string(pv.UID),
				labelPreviewUID: string(pv.Spec.PreviewRef.UID)}); err != nil {
			return err
		}
		for i := range jobs.Items {
			if !renderedName(&jobs.Items[i]) {
				continue
			}
			if err := deleteJob(ctx, v.Client, &jobs.Items[i]); err != nil {
				return err
			}
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

// SetupWithManager watches PreviewVerifications in the release namespace,
// one at a time, so one attempt's Job never races another's for the slot.
func (v *VerificationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&v1alpha1.PreviewVerification{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Named("preview-verification").Complete(v)
}
