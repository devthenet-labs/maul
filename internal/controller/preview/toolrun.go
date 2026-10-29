// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/acceptance/crdconv"
	"github.com/devthenet-labs/maul/internal/checks"
	"github.com/devthenet-labs/maul/internal/previewtools"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// An outcome verification's life on the slot tool pod (DESIGN 2.2 steps 3,
// 4, 10 and 12; SPEC S1-13). Re-checked as a criteria one is, and its
// outcomes and saved checks held to the Intent's approved ones, it waits as
// the tester does for the rollout and the edge policies, then renders the
// per-verification ingress policy, the tool Job, and the CDP key Secret and
// tool ConfigMap the Job owns. While it runs, each poll refreshes the
// capture in the tool ConfigMap and, once the tools container is Ready,
// writes status.tools: only then may intent-controller create the
// evaluator, whose hosts entry is fixed when its pod is created. A pod that
// never comes (a quota or LimitRange refusal leaves the Job Running with no
// pod), is never scheduled, cannot pull an image or never finishes is
// maul's trouble past its bound (Infra tools-not-ready, tools-unscheduled,
// tools-image-pull). Once the Job has finished, its ledger is collected
// (ledger.go, evidence.go) and the attempt settles as a tester's does:
// Observed, EnvFailed (the application's, a component never reached
// included) or Infra (ledger-mismatch). Settling deletes the Job, the
// Secret, the ConfigMap and the policy.

// maxToolVerificationObject caps an outcome verification as written: the
// ledger index (every entry, never cut) and up to MaxSavedChecks confirmed
// bodies sit on its status, well over a criteria verification's cap and
// still well under the API server's object size limit.
const maxToolVerificationObject = 5 << 18

// toolState is what preview-controller keeps of a running tool pod: its CDP
// key, redacted from the capture, and the tool ConfigMap's data as last
// written (it never reads the ConfigMap either: each refresh is a whole,
// unconditional update). The key is known only to the process that wrote
// its Secret, and it never reads the Secret back: after a restart the key
// is "" (unknown), and a browser tool pod's capture is then refused rather
// than refreshed unredacted (keyUnknown).
type toolState struct {
	key  string
	data map[string]string
}

// keyUnknown reports a browser tool pod whose CDP key this process does
// not hold, so cannot redact.
func (st *toolState) keyUnknown(pv *v1alpha1.PreviewVerification) bool {
	return pv.Spec.Tools != nil && pv.Spec.Tools.Browser && st.key == ""
}

// redactions are the keys the capture redacts: the CDP key, when there is
// one.
func (st *toolState) redactions() []string {
	if st.key == "" {
		return nil
	}
	return []string{st.key}
}

// toolStateOf is pv's tool state, made empty (no key) when there is none.
func (v *VerificationReconciler) toolStateOf(uid types.UID) *toolState {
	v.toolsMu.Lock()
	defer v.toolsMu.Unlock()
	if v.toolStates == nil {
		v.toolStates = map[types.UID]*toolState{}
	}
	st := v.toolStates[uid]
	if st == nil {
		st = &toolState{}
		v.toolStates[uid] = st
	}
	return st
}

// forgetTools drops pv's tool state.
func (v *VerificationReconciler) forgetTools(uid types.UID) {
	v.toolsMu.Lock()
	defer v.toolsMu.Unlock()
	delete(v.toolStates, uid)
}

// deadlineMax is the slot Job deadline bound in force.
func (v *VerificationReconciler) deadlineMax() time.Duration {
	return cmp.Or(v.DeadlineMax, testerTimeoutMax)
}

// approvedOutcomes holds an outcome verification to its Intent: exactly
// the approved outcomes, under their digest; its saved checks under the
// check store's digest (none before a first check is saved), each valid,
// its id and digest its body's, bound to an approved outcome; and the
// tools it asks for.
func approvedOutcomes(pv *v1alpha1.PreviewVerification, in *v1alpha1.Intent) bool {
	a := in.Status.Acceptance
	if a == nil || len(a.Outcomes) == 0 || len(pv.Spec.Outcomes) == 0 || pv.Spec.Tools == nil {
		return false
	}
	approved, ok := planOutcomes(a.Outcomes)
	requested, ok2 := planOutcomes(pv.Spec.Outcomes)
	digest := acceptance.OutcomesDigest(approved)
	if !ok || !ok2 || digest != a.OutcomesDigest || digest != pv.Spec.OutcomesDigest ||
		acceptance.OutcomesDigest(requested) != digest {
		return false
	}
	if a.Checks == nil {
		return len(pv.Spec.Checks) == 0 && pv.Spec.ChecksDigest == ""
	}
	return pv.Spec.ChecksDigest == a.Checks.Digest && savedChecksValid(pv.Spec.Checks, a.Outcomes)
}

// savedChecksValid reports whether every saved check parses, its id,
// outcome, form and digest its body's, within the saved bound, bound to one
// of outcomes.
func savedChecksValid(saved []v1alpha1.SavedCheck, outcomes []v1alpha1.AcceptanceOutcome) bool {
	ids := map[string]bool{}
	for _, o := range outcomes {
		ids[o.ID] = true
	}
	for _, sc := range saved {
		c, err := checks.Parse([]byte(sc.Body))
		if err != nil || c.ID != sc.ID || c.Outcome != sc.Outcome || string(c.Form) != string(sc.Form) ||
			checks.Digest(c) != sc.Digest || len(sc.Body) > v1alpha1.MaxSavedCheckBytes || !ids[sc.Outcome] {
			return false
		}
	}
	return true
}

// planOutcomes converts stored outcomes to the plan grammar's.
func planOutcomes(os []v1alpha1.AcceptanceOutcome) ([]acceptance.PlanOutcome, bool) {
	b, err := json.Marshal(os)
	if err != nil {
		return nil, false
	}
	var out []acceptance.PlanOutcome
	return out, json.Unmarshal(b, &out) == nil
}

// startTools renders the tool pod once the rollout has settled and the
// edge policies are in place, each wait bounded as the tester's.
func (v *VerificationReconciler) startTools(ctx context.Context, pv *v1alpha1.PreviewVerification,
	s *verifySubject) (ctrl.Result, error) {
	if !v.Settings.evaluating() {
		v.logRefusal(ctx, pv, "outcome verification not run: the evaluator is off", nil)
		return ctrl.Result{}, v.settle(ctx, pv, v1alpha1.VerificationInfra, v1alpha1.VerificationReasonToolsNotReady)
	}
	ready, err := v.rolloutSettled(ctx, s.preview, s.slot)
	if err == nil && ready {
		ready, err = v.workload().policiesInPlace(ctx, s.preview, s.slot)
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	waited := v.now().Sub(pv.CreationTimestamp.Time)
	if !ready {
		if waited >= verifyGrace {
			return ctrl.Result{}, v.settle(ctx, pv, v1alpha1.VerificationInfra,
				v1alpha1.VerificationReasonRolloutUnsettled)
		}
		return v.wait(), nil
	}
	// Waiting for room is bounded by the verification's own timeout: past
	// it, the round could not have run anyway.
	if room, err := v.evaluationRoom(ctx, pv); err != nil {
		return ctrl.Result{}, err
	} else if !room {
		if waited >= pv.Spec.Timeout.Duration+verifyGrace {
			return ctrl.Result{}, v.settle(ctx, pv, v1alpha1.VerificationInfra,
				v1alpha1.VerificationReasonToolsNotReady)
		}
		return v.wait(), nil
	}
	data, reason, err := v.toolData(ctx, pv, s)
	switch {
	case reason == v1alpha1.VerificationReasonServiceNoClusterIP:
		return ctrl.Result{}, v.settle(ctx, pv, v1alpha1.VerificationInfra, reason)
	case reason != "":
		v.logRefusal(ctx, pv, "tool pod not rendered", err)
		return ctrl.Result{}, v.settle(ctx, pv, v1alpha1.VerificationStale, reason)
	case err != nil:
		return ctrl.Result{}, err
	}
	job, err := v.Settings.toolJob(s.preview, pv, s.slot, v.deadlineMax())
	if err != nil {
		v.logRefusal(ctx, pv, "tool pod not rendered", err)
		return ctrl.Result{}, v.settle(ctx, pv, v1alpha1.VerificationStale, v1alpha1.VerificationReasonStaleCriteria)
	}
	if reason, err := v.createTools(ctx, pv, s, job, data); reason != "" {
		return ctrl.Result{}, v.settle(ctx, pv, v1alpha1.VerificationInfra, reason)
	} else if err != nil {
		return ctrl.Result{}, err
	}
	t := metav1.NewTime(v.now())
	pv.Status.Phase = v1alpha1.VerificationRunning
	pv.Status.Reason = ""
	pv.Status.StartedAt = &t
	return v.wait(), v.Status().Update(ctx, pv)
}

// evaluationRoom reports whether another tool pod may start: fewer than
// MaxEvaluations outcome verifications run.
func (v *VerificationReconciler) evaluationRoom(ctx context.Context, pv *v1alpha1.PreviewVerification) (bool, error) {
	var list v1alpha1.PreviewVerificationList
	if err := v.List(ctx, &list, client.InNamespace(v.Settings.Namespace)); err != nil {
		return false, err
	}
	running := 0
	for _, other := range list.Items {
		if other.UID != pv.UID && len(other.Spec.Outcomes) > 0 && other.Status.Phase == v1alpha1.VerificationRunning {
			running++
		}
	}
	return running < v.Settings.maxEvaluations(), nil
}

// toolData is the tool ConfigMap's own data for pv: the relay's config,
// its targets each component's Service ClusterIP read live, and the
// outcomes. A component Service with no ClusterIP is the reason
// service-no-cluster-ip; a config the relay would refuse, stale-criteria.
func (v *VerificationReconciler) toolData(ctx context.Context, pv *v1alpha1.PreviewVerification,
	s *verifySubject) (map[string]string, v1alpha1.PreviewVerificationReason, error) {
	targets, err := v.serviceTargets(ctx, s.preview, s.slot)
	if errors.Is(err, errNoClusterIP) {
		v.logRefusal(ctx, pv, "tool pod not rendered", err)
		return nil, v1alpha1.VerificationReasonServiceNoClusterIP, nil
	}
	if err != nil {
		return nil, "", err
	}
	_, _, wall := toolDeadlines(pv.Spec.Timeout.Duration, v.deadlineMax())
	cfg, err := v.Settings.toolConfig(s.preview, pv, targets, wall)
	if err != nil {
		return nil, v1alpha1.VerificationReasonStaleCriteria, err
	}
	data, err := toolConfigData(cfg, pv.Spec.Outcomes)
	if err != nil {
		return nil, v1alpha1.VerificationReasonStaleCriteria, err
	}
	return data, "", nil
}

// createTools writes the ingress policy first (nothing may reach the pod
// before it), then the Job, then the Secret and ConfigMap it owns. Each is
// per verification, so one already there is this attempt's own. A write
// the quota refuses is quota-refused; one admission refuses,
// tools-unscheduled.
func (v *VerificationReconciler) createTools(ctx context.Context, pv *v1alpha1.PreviewVerification,
	s *verifySubject, job *batchv1.Job, data map[string]string) (v1alpha1.PreviewVerificationReason, error) {
	refused := func(what string, err error) (v1alpha1.PreviewVerificationReason, error) {
		switch {
		case quotaExceeded(err):
			return v1alpha1.VerificationReasonQuotaRefused, nil
		case admissionRefused(err) || kerrors.IsInvalid(err):
			v.logRefusal(ctx, pv, what+" refused", err)
			return v1alpha1.VerificationReasonToolsUnscheduled, nil
		}
		return "", err
	}
	if err := v.Create(ctx, v.Settings.evaluatorPolicy(s.preview, pv, s.slot)); err != nil &&
		!kerrors.IsAlreadyExists(err) {
		return refused("tool ingress policy", err)
	}
	if err := v.Create(ctx, job); kerrors.IsAlreadyExists(err) {
		live, err := v.liveTesterJob(ctx, pv, s.preview, s.slot)
		if err != nil || live == nil {
			return "", cmp.Or(err, errors.New("tool Job exists but is not this verification's"))
		}
		job = live
	} else if err != nil {
		return refused("tool Job", err)
	}
	st := v.toolStateOf(pv.UID)
	if pv.Spec.Tools.Browser {
		// A Secret already there with no key in memory is an earlier
		// process's: its key stays unknown.
		key := cmp.Or(st.key, newCDPKey())
		err := v.Create(ctx, v.Settings.toolKeySecret(s.preview, pv, job, key))
		switch {
		case err == nil:
			st.key = key
		case !kerrors.IsAlreadyExists(err):
			return refused("tool key Secret", err)
		}
	}
	cm := v.Settings.toolConfigMap(s.preview, pv, job, data)
	err := v.Create(ctx, cm)
	if kerrors.IsAlreadyExists(err) {
		err = v.Update(ctx, cm)
	}
	if err != nil {
		return refused("tool ConfigMap", err)
	}
	st.data = data
	return "", nil
}

// observeTools follows a running tool pod: while it runs it refreshes the
// capture and publishes where the evaluator reaches it; once the Job has
// finished it collects the ledger.
func (v *VerificationReconciler) observeTools(ctx context.Context, pv *v1alpha1.PreviewVerification,
	s *verifySubject) (ctrl.Result, error) {
	job, err := v.liveTesterJob(ctx, pv, s.preview, s.slot)
	if err != nil {
		return ctrl.Result{}, err
	}
	if job == nil {
		return ctrl.Result{}, v.settle(ctx, pv, v1alpha1.VerificationInfra, v1alpha1.VerificationReasonToolsNotReady)
	}
	pod, err := v.testerPod(ctx, job)
	if errors.Is(err, errTesterPods) {
		return ctrl.Result{}, v.settle(ctx, pv, v1alpha1.VerificationInfra, v1alpha1.VerificationReasonLedgerMismatch)
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	finishedAt, finished := jobFinished(job)
	if !finished {
		if reason := v.toolsStuck(pv, pod); reason != "" {
			return ctrl.Result{}, v.settle(ctx, pv, v1alpha1.VerificationInfra, reason)
		}
		return v.wait(), v.followTools(ctx, pv, s, job, pod)
	}
	if pod == nil {
		return ctrl.Result{}, v.settle(ctx, pv, v1alpha1.VerificationInfra,
			v1alpha1.VerificationReasonDeadlineNoSummary)
	}
	log, err := v.Logs.ReadLog(ctx, pod.Namespace, pod.Name, toolsContainer,
		LogOptions{LimitBytes: toolwire.MaxLogBytes + 1})
	if err != nil {
		if !kerrors.IsNotFound(err) && v.now().Sub(finishedAt) < verifyGrace {
			v.logRefusal(ctx, pv, "tools log not read yet", err)
			return v.wait(), nil
		}
		return ctrl.Result{}, v.settle(ctx, pv, v1alpha1.VerificationInfra, v1alpha1.VerificationReasonLedgerMismatch)
	}
	return ctrl.Result{}, v.recordTools(ctx, pv, s, pod, log, finishedAt)
}

// toolsStuck is why a running tool pod will not finish, past its grace: no
// pod created (a quota or LimitRange refusal), none scheduled, an image not
// pulled, or the Job's own deadline long past. "" while it may yet.
func (v *VerificationReconciler) toolsStuck(pv *v1alpha1.PreviewVerification,
	pod *corev1.Pod) v1alpha1.PreviewVerificationReason {
	since := v.now().Sub(startedAt(pv))
	if since < verifyGrace {
		return ""
	}
	jobDeadline, _, _ := toolDeadlines(pv.Spec.Timeout.Duration, v.deadlineMax())
	switch {
	case pod == nil:
		return v1alpha1.VerificationReasonToolsNotReady
	case unschedulable(pod):
		return v1alpha1.VerificationReasonToolsUnscheduled
	case anyImagePullWaiting(pod):
		return v1alpha1.VerificationReasonToolsImagePull
	case since >= jobDeadline+verifyGrace:
		return v1alpha1.VerificationReasonToolsNotReady
	}
	return ""
}

// anyImagePullWaiting reports an image pull back-off in any container, a
// sidecar's included.
func anyImagePullWaiting(pod *corev1.Pod) bool {
	for _, s := range slices.Concat(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses) {
		if s.State.Waiting != nil && imagePullReason(s.State.Waiting.Reason) {
			return true
		}
	}
	return false
}

// followTools refreshes the capture in the tool ConfigMap and, once the
// tools container is Ready, records where the evaluator reaches it; it
// writes status only when either changed. The capture is best effort: a
// failed refresh is logged and the last one stands, as it does for a
// browser tool pod whose CDP key this process does not hold (it restarted
// since writing the Secret), which is never captured.
func (v *VerificationReconciler) followTools(ctx context.Context, pv *v1alpha1.PreviewVerification,
	s *verifySubject, job *batchv1.Job, pod *corev1.Pod) error {
	before := pv.Status.DeepCopy()
	v.followCapture(ctx, pv, s, job)
	if pv.Status.Tools == nil && pod != nil && toolsReady(pod) {
		pv.Status.Tools = &v1alpha1.ToolsStatus{PodIP: pod.Status.PodIP, Port: previewtools.Port, Ready: true}
	}
	if reflect.DeepEqual(before, &pv.Status) {
		return nil
	}
	return v.Status().Update(ctx, pv)
}

// errCDPKeyUnknown is a browser tool pod's capture refused: this process
// did not write its CDP key Secret, so cannot redact the key.
var errCDPKeyUnknown = errors.New("CDP key not held by this process (restarted since its Secret was written)")

// followCapture refreshes the capture in the tool ConfigMap, best effort,
// never with a CDP key unknown.
func (v *VerificationReconciler) followCapture(ctx context.Context, pv *v1alpha1.PreviewVerification,
	s *verifySubject, job *batchv1.Job) {
	st := v.toolStateOf(pv.UID)
	if st.keyUnknown(pv) {
		v.logRefusal(ctx, pv, "capture not refreshed", errCDPKeyUnknown)
		return
	}
	if st.data == nil {
		data, _, err := v.toolData(ctx, pv, s)
		if err != nil || data == nil {
			v.logRefusal(ctx, pv, "tool config not re-derived for the capture", err)
			return
		}
		st.data = data
	}
	cm := v.Settings.toolConfigMap(s.preview, pv, job, maps.Clone(st.data))
	if err := v.refreshCapture(ctx, pv, s.preview, s.slot, cm, st.redactions()); err != nil {
		v.logRefusal(ctx, pv, "capture not refreshed", err)
		return
	}
	st.data = cm.Data
}

// toolsReady reports the tools container Ready on a pod with an address.
func toolsReady(pod *corev1.Pod) bool {
	if pod.Status.PodIP == "" || !podCondition(pod, corev1.PodReady) {
		return false
	}
	i := slices.IndexFunc(pod.Status.ContainerStatuses, func(s corev1.ContainerStatus) bool {
		return s.Name == toolsContainer
	})
	return i >= 0 && pod.Status.ContainerStatuses[i].Ready
}

// recordTools settles a finished tool pod's attempt from its ledger and
// the slot's environment. A ledger that holds is indexed onto status with
// its runs and confirmed checks, stored whole in <pv>-ledger, and its
// evidence in <pv>-evidence.
func (v *VerificationReconciler) recordTools(ctx context.Context, pv *v1alpha1.PreviewVerification,
	s *verifySubject, pod *corev1.Pod, log []byte, finishedAt time.Time) error {
	out := parseToolOutput(log, terminationMessageOf(pod, toolsContainer))
	facts, err := v.environment(ctx, s.preview, s.slot, pod, pv)
	if err != nil {
		return err
	}
	if out.reason == "" {
		waits := readinessOf(out.ledger.Entries)
		markUnreachable(facts, waits)
		if pv.Status.Readiness, err = crdconv.WaitsToCRD(waits); err != nil {
			out.reason = v1alpha1.VerificationReasonLedgerMismatch
		}
	}
	phase, reason := route(facts, out.reason)
	if out.reason == "" {
		if err := v.storeLedger(ctx, pv, out, finishedAt); err != nil {
			return err
		}
	}
	recordToolsSettled(ctx, phase, reason)
	pv.Status.Environment = facts
	pv.Status.TesterImageID = imageIDOf(pod, toolsContainer)
	return v.settleWith(ctx, pv, phase, reason)
}

// storeLedger writes the verified ledger's index, runs and confirmed
// checks onto pv's status, and the ledger and its kept evidence into their
// ConfigMaps. A ConfigMap that cannot be written is retried for the grace
// after the Job finished, then left out of status: the index stands.
func (v *VerificationReconciler) storeLedger(ctx context.Context, pv *v1alpha1.PreviewVerification, out toolOutput,
	finishedAt time.Time) error {
	entries := out.ledger.Entries
	images, dropped := keepEvidence(entries, out.evidence)
	if out.dropped > 0 {
		dropped[dropMalformed] += out.dropped
	}
	for why, n := range dropped {
		recordEvidenceDropped(ctx, why, n)
		if v.Log != nil {
			v.Log.LogAttrs(ctx, slog.LevelWarn, "evidence image dropped", slog.String("verification", pv.Name),
				slog.String("reason", why), slog.Int("count", n))
		}
	}
	sum := &v1alpha1.LedgerSummary{Count: clampInt32(out.ledger.Summary.Count), SHA256: out.ledger.Summary.SHA256,
		Entries: ledgerIndex(entries)}
	cm, err := ledgerConfigMap(pv, out.ledger)
	if err == nil {
		sum.ConfigMap, err = v.createOwned(ctx, cm, finishedAt)
	}
	if err != nil {
		return err
	}
	if ev := evidenceConfigMap(pv, images); ev != nil {
		if sum.Evidence, err = v.createOwned(ctx, ev, finishedAt); err != nil {
			return err
		}
	}
	pv.Status.Ledger = sum
	pv.Status.Replay = replayRuns(entries)
	pv.Status.Confirmations = confirmationRuns(entries)
	pv.Status.Confirmed = confirmedChecks(entries, pv.Spec.Outcomes)
	return nil
}

// createOwned creates a verification-owned ConfigMap once (one already
// there is this attempt's own, written before a status update that did not
// land) and returns its name; past the grace after the Job finished, one
// that cannot be written is logged and named "".
func (v *VerificationReconciler) createOwned(ctx context.Context, cm *corev1.ConfigMap,
	finishedAt time.Time) (string, error) {
	err := v.Create(ctx, cm)
	if err == nil || kerrors.IsAlreadyExists(err) {
		return cm.Name, nil
	}
	if v.now().Sub(finishedAt) < verifyGrace {
		return "", err
	}
	if v.Log != nil {
		v.Log.LogAttrs(ctx, slog.LevelWarn, "verification ConfigMap not written", slog.String("configmap", cm.Name),
			slog.Any("error", err))
	}
	return "", nil
}

// teardown deletes what an outcome verification put in any slot beside
// its Job (deleteTesterJobs): the ingress policy, the key Secret and the
// tool ConfigMap, by name (preview-controller holds no read of either),
// and forgets its tool state. Deleting what is gone is no error.
func (v *VerificationReconciler) teardown(ctx context.Context, pv *v1alpha1.PreviewVerification) error {
	if err := v.deleteTesterJobs(ctx, pv); err != nil {
		return err
	}
	if len(pv.Spec.Outcomes) == 0 || !v.Settings.evaluating() {
		return nil
	}
	p := &v1alpha1.Preview{ObjectMeta: metav1.ObjectMeta{Name: pv.Spec.PreviewRef.Name}}
	for slot := range v.Settings.SlotCount {
		ns := v.Settings.slotName(int32(slot))
		for _, obj := range []client.Object{
			&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: toolName(p, pv, toolConfigInfix), Namespace: ns}},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: toolName(p, pv, toolKeyInfix), Namespace: ns}},
		} {
			if err := v.Delete(ctx, obj); err != nil && !kerrors.IsNotFound(err) {
				return err
			}
		}
		np := v.Settings.evaluatorPolicy(p, pv, int32(slot))
		if err := v.Delete(ctx, np); err != nil && !kerrors.IsNotFound(err) {
			return err
		}
	}
	v.forgetTools(pv.UID)
	return nil
}

// fitToolStatus holds an outcome verification to maxToolVerificationObject:
// log tails go first, from the last part back; one still over is maul's
// trouble, its ledger fields dropped.
func fitToolStatus(pv *v1alpha1.PreviewVerification) {
	for i := len(pv.Status.Environment) - 1; i >= 0 && jsonSize(pv) > maxToolVerificationObject; i-- {
		pv.Status.Environment[i].LogTail = ""
	}
	if n := jsonSize(pv); n >= 0 && n <= maxToolVerificationObject {
		return
	}
	pv.Status.Ledger, pv.Status.Replay, pv.Status.Confirmations, pv.Status.Confirmed = nil, nil, nil, nil
	pv.Status.Phase = v1alpha1.VerificationInfra
	pv.Status.Reason = v1alpha1.VerificationReasonLedgerMismatch
}
