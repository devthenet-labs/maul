// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"maps"
	"math"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	batchv1 "k8s.io/api/batch/v1"
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
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/acceptance/crdconv"
	"github.com/devthenet-labs/maul/internal/httpstep"
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
	// maxTesterLog is the most of the tester's log read. It is the grammar's
	// own bound, so acceptance.Validate's plan-wide budget
	// (acceptance.MaxWireBytes, below it) keeps every accepted plan's output
	// within it whatever the application answers.
	maxTesterLog = acceptance.MaxTesterLogBytes
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
	intent   *v1alpha1.Intent
	preview  *v1alpha1.Preview
	project  *v1alpha1.Project
	criteria []acceptance.Criterion
	slot     int32
	// outcomes marks an outcome verification: the slot tool pod's.
	outcomes bool
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
		// Its tester or tool Job and its evaluator ingress policy, if any,
		// are the sweep's: its verification is gone.
		return ctrl.Result{}, nil
	}
	if settled(pv.Status.Phase) {
		return ctrl.Result{}, v.teardown(ctx, &pv)
	}
	s, reason, err := v.check(ctx, &pv)
	switch {
	case err != nil:
		return ctrl.Result{}, err
	case reason == v1alpha1.VerificationReasonPreviewFailed && pv.Status.Phase == v1alpha1.VerificationRunning:
		return ctrl.Result{}, v.capture(ctx, &pv, s.preview, s.slot)
	case reason != "":
		return ctrl.Result{}, v.settle(ctx, &pv, v1alpha1.VerificationStale, reason)
	case pv.Status.Phase == v1alpha1.VerificationRunning && s.outcomes:
		return v.observeTools(ctx, &pv, s)
	case pv.Status.Phase == v1alpha1.VerificationRunning:
		return v.observe(ctx, &pv, s)
	case s.outcomes:
		return v.startTools(ctx, &pv, s)
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
	if len(pv.Spec.Outcomes) > 0 {
		if !approvedOutcomes(pv, s.intent) {
			return s, v1alpha1.VerificationReasonStaleCriteria, nil
		}
		s.outcomes = true
		return s, "", nil
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
	if !v.Settings.testing() {
		// An evaluator-only install runs no tester: a criteria request is
		// not this install's, so it is answered stale and nothing renders.
		v.logRefusal(ctx, pv, "criteria verification not run: no tester image", nil)
		return ctrl.Result{}, v.settle(ctx, pv, v1alpha1.VerificationStale, v1alpha1.VerificationReasonStaleCriteria)
	}
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
	job, reason, err := v.renderTester(ctx, pv, s)
	switch {
	case reason == v1alpha1.VerificationReasonServiceNoClusterIP:
		return ctrl.Result{}, v.settle(ctx, pv, v1alpha1.VerificationInfra, reason)
	case reason != "":
		v.logRefusal(ctx, pv, "tester not rendered", err)
		return ctrl.Result{}, v.settle(ctx, pv, v1alpha1.VerificationStale, reason)
	case err != nil:
		return ctrl.Result{}, err
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

// renderedSpec is the spec a tester Job was rendered with, read back from
// its container's environment.
func renderedSpec(job *batchv1.Job) (previewtester.Spec, error) {
	for _, c := range job.Spec.Template.Spec.Containers {
		if c.Name != testerContainer {
			continue
		}
		for _, e := range c.Env {
			if e.Name == previewtester.EnvSpec {
				return previewtester.ParseSpec([]byte(e.Value))
			}
		}
	}
	return previewtester.Spec{}, errors.New("tester Job carries no spec")
}

// renderTester renders the request's tester Job: the spec the tester reads
// (its criteria, run tokens and, per component, its Service's ClusterIP on
// the Service port, serviceTargets) and its budget below the Job's
// deadline, under a deadline clamped to DeadlineMax. A component Service
// with no ClusterIP is the reason service-no-cluster-ip, no Job.
func (v *VerificationReconciler) renderTester(ctx context.Context, pv *v1alpha1.PreviewVerification,
	s *verifySubject) (*batchv1.Job, v1alpha1.PreviewVerificationReason, error) {
	targets, err := v.serviceTargets(ctx, s.preview, s.slot)
	if errors.Is(err, errNoClusterIP) {
		v.logRefusal(ctx, pv, "tester not rendered", err)
		return nil, v1alpha1.VerificationReasonServiceNoClusterIP, nil
	}
	if err != nil {
		return nil, "", err
	}
	clamped := pv.DeepCopy()
	if limit := cmp.Or(v.DeadlineMax, testerTimeoutMax); clamped.Spec.Timeout.Duration > limit {
		clamped.Spec.Timeout.Duration = limit
	}
	input, err := previewtester.Spec{Round: pv.Spec.Round, Attempt: pv.Spec.Attempt, Run: pv.Spec.Run,
		BudgetSeconds: previewtester.BudgetFor(clamped.Spec.Timeout.Duration),
		Criteria:      s.criteria, Targets: targets}.Encode()
	if err != nil {
		return nil, v1alpha1.VerificationReasonStaleCriteria, err
	}
	job, err := v.Settings.testerJob(s.preview, clamped, s.slot, input)
	if err != nil {
		return nil, v1alpha1.VerificationReasonStaleCriteria, err
	}
	return job, "", nil
}

// observe follows a running tester: it waits, bounded, for the Job to
// finish, then reads and settles what it observed.
func (v *VerificationReconciler) observe(ctx context.Context, pv *v1alpha1.PreviewVerification,
	s *verifySubject) (ctrl.Result, error) {
	job, err := v.liveJob(ctx, pv, s.preview, s.slot)
	if err != nil {
		return ctrl.Result{}, err
	}
	if job == nil {
		return ctrl.Result{}, v.settle(ctx, pv, v1alpha1.VerificationInfra,
			v1alpha1.VerificationReasonTesterOutputInvalid)
	}
	pod, err := v.jobPod(ctx, job)
	if errors.Is(err, errJobPods) {
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
	return ctrl.Result{}, v.record(ctx, pv, s, job, pod, log)
}

// record settles a finished tester's attempt from its output and the
// slot's environment. The targets its readiness waits are held to are the
// ones its Job was rendered with, read back from the Job; a rendered spec
// that no longer matches the Preview means the Preview changed under the
// attempt (stale-preview).
func (v *VerificationReconciler) record(ctx context.Context, pv *v1alpha1.PreviewVerification,
	s *verifySubject, job *batchv1.Job, pod *corev1.Pod, log []byte) error {
	spec, err := renderedSpec(job)
	if errors.Is(err, previewtester.ErrNameTarget) {
		// A tester Job an older preview-controller rendered, its targets
		// Service names: run the attempt again under this one.
		v.logRefusal(ctx, pv, "tester Job rendered in the format before ClusterIP targets", err)
		return v.settle(ctx, pv, v1alpha1.VerificationStale, v1alpha1.VerificationReasonStalePreview)
	}
	if err != nil {
		v.logRefusal(ctx, pv, "tester Job spec unreadable", err)
		return v.settle(ctx, pv, v1alpha1.VerificationInfra, v1alpha1.VerificationReasonTesterOutputInvalid)
	}
	live, err := v.serviceTargets(ctx, s.preview, s.slot)
	if err != nil && !errors.Is(err, errNoClusterIP) {
		return err
	}
	if err != nil || !maps.Equal(spec.Targets, live) {
		return v.settle(ctx, pv, v1alpha1.VerificationStale, v1alpha1.VerificationReasonStalePreview)
	}
	rt := acceptance.RunTokens{Round: pv.Spec.Round, Attempt: pv.Spec.Attempt, Run: pv.Spec.Run}
	ids := make([]string, len(s.criteria))
	for i, c := range s.criteria {
		ids[i] = c.ID
	}
	targets := slices.Sorted(maps.Keys(spec.Targets))
	out := parseTesterOutput(log, terminationMessageOf(pod, testerContainer), rt, ids, targets)
	facts, err := v.environment(ctx, s.preview, s.slot, pod, pv)
	if err != nil {
		return err
	}
	held := out.reason == ""
	if held {
		markUnreachable(facts, out.sum.Waits)
	}
	phase, reason := route(facts, out.reason)
	v.logOutcome(ctx, pv, out, phase, reason)
	if out.reason == v1alpha1.VerificationReasonTesterVersionMismatch {
		pv.Status.TesterWireVersion = int32(min(max(out.version, 0), math.MaxInt32))
	}
	if held {
		if pv.Status.Readiness, err = crdconv.WaitsToCRD(out.sum.Waits); err != nil {
			phase, reason = v1alpha1.VerificationInfra, v1alpha1.VerificationReasonTesterOutputInvalid
		}
	}
	obs, sum, outReason := out.obs, out.sum, out.reason
	if outReason == "" {
		converted, err := crdconv.ObservationsToCRD(obs)
		if err != nil {
			phase, reason = v1alpha1.VerificationInfra, v1alpha1.VerificationReasonTesterOutputInvalid
		}
		pv.Status.Observations = converted
		pv.Status.SummarySHA256 = sum.SHA256
	}
	pv.Status.Environment = facts
	pv.Status.TesterImageID = imageIDOf(pod, testerContainer)
	return v.settleWith(ctx, pv, phase, reason)
}

// logOutcome logs what an operator needs from a tester's output that the
// status's fixed vocabulary cannot carry: the targets never reached, and
// both versions of a wire version mismatch.
func (v *VerificationReconciler) logOutcome(ctx context.Context, pv *v1alpha1.PreviewVerification,
	out parsedOutput, phase v1alpha1.PreviewVerificationPhase, reason v1alpha1.PreviewVerificationReason) {
	if v.Log == nil {
		return
	}
	switch {
	case out.reason == v1alpha1.VerificationReasonTesterVersionMismatch:
		v.Log.LogAttrs(ctx, slog.LevelWarn, "tester wire version mismatch: the tester image and preview-controller disagree",
			slog.String("verification", pv.Name), slog.Int("testerVersion", out.version),
			slog.Int("controllerVersion", acceptance.WireVersion))
	case out.sum.Unreached():
		v.Log.LogAttrs(ctx, slog.LevelWarn, "unreachable from the tester for its whole readiness window while Ready",
			slog.String("verification", pv.Name), slog.Any("unreachable", out.sum.Unreachable),
			slog.String("phase", string(phase)), slog.String("reason", string(reason)))
	}
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

func imagePullWaiting(pod *corev1.Pod) bool {
	for _, s := range pod.Status.ContainerStatuses {
		if s.State.Waiting != nil && imagePullReason(s.State.Waiting.Reason) {
			return true
		}
	}
	return false
}

// parsedOutput is what parseTesterOutput read: the observations and the
// summary, the reason the output does not hold ("" when it does), and the
// wire version the tester wrote when that is the reason.
type parsedOutput struct {
	obs     []acceptance.CriterionObservation
	sum     acceptance.Summary
	reason  v1alpha1.PreviewVerificationReason
	version int
}

// parseTesterOutput reads the tester's MAUL-VERIFY log: of the controller's
// wire version (tester-version-mismatch otherwise), its lines verified
// against their summary and the termination message, then held to the
// request: a readiness wait for exactly the targets it was given (its
// rendered spec's, in name order), and, when every one was reached, exactly
// one observation per requested criterion, in the request's order, each
// with at least one try, each try under the run token it was given (id
// ownership). Output that ended at its readiness wait, a target never
// reached, holds with no observation: the environment says whose failure
// that is (markUnreachable).
func parseTesterOutput(log []byte, msg string, rt acceptance.RunTokens, ids []string,
	targets []string) parsedOutput {
	bad := func(sum acceptance.Summary, r v1alpha1.PreviewVerificationReason) parsedOutput {
		return parsedOutput{sum: sum, reason: r}
	}
	if len(log) > maxTesterLog {
		return bad(acceptance.Summary{}, v1alpha1.VerificationReasonTesterOutputInvalid)
	}
	obs, sum, err := acceptance.ParseWire(log)
	var ve *acceptance.WireVersionError
	switch {
	case errors.As(err, &ve):
		return parsedOutput{reason: v1alpha1.VerificationReasonTesterVersionMismatch, version: ve.Got}
	case errors.Is(err, acceptance.ErrNoSummary):
		return bad(sum, v1alpha1.VerificationReasonDeadlineNoSummary)
	case errors.Is(err, acceptance.ErrSummaryMismatch):
		return bad(sum, v1alpha1.VerificationReasonSummaryMismatch)
	case err != nil:
		return bad(sum, v1alpha1.VerificationReasonTesterOutputInvalid)
	}
	if term, err := acceptance.ParseSummary(msg); err != nil || !term.Equal(sum) {
		return bad(sum, v1alpha1.VerificationReasonSummaryMismatch)
	}
	if !slices.EqualFunc(sum.Waits, targets, func(w httpstep.TargetWait, t string) bool { return w.Target == t }) {
		return bad(sum, v1alpha1.VerificationReasonTesterOutputInvalid)
	}
	if sum.Unreached() {
		// The parser holds an unreachable summary to no observation.
		return parsedOutput{sum: sum}
	}
	if !ownsObservations(obs, rt, ids) {
		return bad(sum, v1alpha1.VerificationReasonTesterOutputInvalid)
	}
	return parsedOutput{obs: obs, sum: sum}
}

// ownsObservations reports whether obs are exactly one observation per
// requested criterion, in order, each with at least one try, each try under
// the run token it was given.
func ownsObservations(obs []acceptance.CriterionObservation, rt acceptance.RunTokens, ids []string) bool {
	if len(obs) != len(ids) {
		return false
	}
	for i, o := range obs {
		if o.ID != ids[i] || len(o.Tries) == 0 {
			return false
		}
		for t, try := range o.Tries {
			if try.Run != rt.Token(t+1) {
				return false
			}
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
	var tester *corev1.Pod
	if job != nil {
		if tester, err = v.jobPod(ctx, job); err != nil && !errors.Is(err, errJobPods) {
			return err
		}
	}
	facts, err := v.environment(ctx, p, slot, tester, pv)
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
			!pv.DeletionTimestamp.IsZero() {
			continue
		}
		if err := v.capture(ctx, pv, p, *p.Status.Slot); err != nil {
			return err
		}
	}
	return nil
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
// header and capture values with httpstep.TooLarge, from the last
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
					gain := jsonSize(m[0][key]) - jsonSize(httpstep.TooLarge)
					if gain <= 0 {
						continue
					}
					m[1][key] = httpstep.TooLarge
					if !reflect.DeepEqual(acceptance.Judge(c, judged, rt), want) {
						m[1][key] = m[0][key]
						continue
					}
					m[0][key] = httpstep.TooLarge
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
