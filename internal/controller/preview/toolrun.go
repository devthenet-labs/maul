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
	"net"
	"reflect"
	"slices"
	"strconv"
	"strings"
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
	"github.com/devthenet-labs/maul/internal/httpstep"
	"github.com/devthenet-labs/maul/internal/previewtools"
	"github.com/devthenet-labs/maul/internal/toolledger"
	"github.com/devthenet-labs/maul/internal/toolpod"
	"github.com/devthenet-labs/maul/internal/toolpod/podstate"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// An outcome verification's life on the slot tool pod (DESIGN 2.2 steps 3,
// 4, 10 and 12; SPEC S1-13). Re-checked as a criteria one is, and its
// outcomes and saved checks held to the Intent's approved ones, it waits as
// the tester does for the rollout and the edge policies, then renders the
// per-verification ingress policy, the tool Job, and the CDP key Secret and
// tool ConfigMap the Job owns. While it runs, each poll refreshes the
// capture in the tool ConfigMap and, once the tools container and every
// sidecar the round asks for are Ready, writes status.tools: only then may intent-controller create the
// evaluator, whose hosts entry is fixed when its pod is created. A pod that
// never comes (a quota or LimitRange refusal leaves the Job Running with no
// pod), is never scheduled, cannot pull an image, has a container
// crash-looping or never finishes is maul's trouble past its bound (Infra
// tools-not-ready, tools-unscheduled, tools-image-pull, tools-crashloop).
// Once the Job has finished, a component Service whose address is no
// longer the one the relay was rendered with (readdressed) settles it
// Stale stale-preview; otherwise its ledger is collected (ledger.go,
// evidence.go) and the attempt settles as a tester's does:
// Observed, EnvFailed (the application's, a component never reached
// included) or Infra (ledger-mismatch; a full round with no evaluator call
// in its ledger, tools-not-ready or evaluator-not-run). Settling deletes the Job, the
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
	return cmp.Or(v.DeadlineMax, verifyTimeoutMax)
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
	data, targets, reason, err := v.toolData(ctx, pv, s)
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
	if err == nil {
		err = recordTargets(job, targets)
	}
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
		if other.UID != pv.UID && !other.Spec.IsAgents() && len(other.Spec.Outcomes) > 0 &&
			other.Status.Phase == v1alpha1.VerificationRunning {
			running++
		}
	}
	return running < v.Settings.maxEvaluations(), nil
}

// toolData is the tool ConfigMap's own data for pv: the relay's config,
// its targets each component's Service ClusterIP read live, and the
// outcomes; and those targets. A component Service with no ClusterIP is
// the reason service-no-cluster-ip; a config the relay would refuse,
// stale-criteria.
func (v *VerificationReconciler) toolData(ctx context.Context, pv *v1alpha1.PreviewVerification,
	s *verifySubject) (data, targets map[string]string, reason v1alpha1.PreviewVerificationReason, err error) {
	targets, err = v.serviceTargets(ctx, s.preview, s.slot)
	if errors.Is(err, errNoClusterIP) {
		v.logRefusal(ctx, pv, "tool pod not rendered", err)
		return nil, nil, v1alpha1.VerificationReasonServiceNoClusterIP, nil
	}
	if err != nil {
		return nil, nil, "", err
	}
	_, _, wall := toolpod.Deadlines(pv.Spec.Timeout.Duration, v.deadlineMax())
	cfg, err := v.Settings.toolConfig(s.preview, pv, targets, wall)
	if err != nil {
		return nil, nil, v1alpha1.VerificationReasonStaleCriteria, err
	}
	if data, err = toolpod.ConfigData(cfg, pv.Spec.Outcomes); err != nil {
		return nil, nil, v1alpha1.VerificationReasonStaleCriteria, err
	}
	return data, targets, "", nil
}

// annotationToolTargets on a tool Job records the targets its relay was
// rendered with (each component's Service ClusterIP and port, as JSON), the
// only record of them preview-controller can read back: it holds no read
// of the tool ConfigMap, and what it keeps in memory is re-derived from
// the live Services after a restart.
const annotationToolTargets = "maul.run/tool-targets"

// recordTargets stamps targets on job (annotationToolTargets).
func recordTargets(job *batchv1.Job, targets map[string]string) error {
	b, err := json.Marshal(targets)
	if err != nil {
		return err
	}
	if job.Annotations == nil {
		job.Annotations = map[string]string{}
	}
	job.Annotations[annotationToolTargets] = string(b)
	return nil
}

// readdressed reports whether the targets job's relay was rendered with
// are no longer the components' Services' addresses: a Service recreated
// with another ClusterIP, or with none, while the round ran, or a Job that
// records no targets (an earlier release rendered it). The relay then
// spoke to other addresses than the preview serves, so its round is about
// no component (stale-preview), never the change's unreachable component.
func (v *VerificationReconciler) readdressed(ctx context.Context, job *batchv1.Job, s *verifySubject) (bool, error) {
	var rendered map[string]string
	raw, ok := job.Annotations[annotationToolTargets]
	if !ok || json.Unmarshal([]byte(raw), &rendered) != nil {
		return true, nil
	}
	live, err := v.serviceTargets(ctx, s.preview, s.slot)
	if errors.Is(err, errNoClusterIP) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return !maps.Equal(rendered, live), nil
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
		live, err := v.liveJob(ctx, pv, s.preview, s.slot)
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
		key := cmp.Or(st.key, toolpod.NewKey())
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
	job, err := v.liveJob(ctx, pv, s.preview, s.slot)
	if err != nil {
		return ctrl.Result{}, err
	}
	if job == nil {
		return ctrl.Result{}, v.settle(ctx, pv, v1alpha1.VerificationInfra, v1alpha1.VerificationReasonToolsNotReady)
	}
	pod, err := v.jobPod(ctx, job)
	if errors.Is(err, errJobPods) {
		return ctrl.Result{}, v.settle(ctx, pv, v1alpha1.VerificationInfra, v1alpha1.VerificationReasonLedgerMismatch)
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	finishedAt, finished := jobFinished(job)
	if !finished {
		if reason := v.toolsStuck(pv, pod); reason != "" {
			if reason == v1alpha1.VerificationReasonToolsCrashLoop {
				v.recordCrashLoop(ctx, pv, pod)
			}
			return ctrl.Result{}, v.settle(ctx, pv, v1alpha1.VerificationInfra, reason)
		}
		return v.wait(), v.followTools(ctx, pv, s, job, pod)
	}
	if pod == nil {
		return ctrl.Result{}, v.settle(ctx, pv, v1alpha1.VerificationInfra,
			v1alpha1.VerificationReasonDeadlineNoSummary)
	}
	if moved, err := v.readdressed(ctx, job, s); err != nil {
		return ctrl.Result{}, err
	} else if moved {
		return ctrl.Result{}, v.settle(ctx, pv, v1alpha1.VerificationStale, v1alpha1.VerificationReasonStalePreview)
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
// pulled, a container crash-looping (a native sidecar's restarts never end
// the pod), or the Job's own deadline long past. "" while it may yet.
func (v *VerificationReconciler) toolsStuck(pv *v1alpha1.PreviewVerification,
	pod *corev1.Pod) v1alpha1.PreviewVerificationReason {
	since := v.now().Sub(startedAt(pv))
	if since < verifyGrace {
		return ""
	}
	jobDeadline, _, _ := toolpod.Deadlines(pv.Spec.Timeout.Duration, v.deadlineMax())
	switch {
	case pod == nil:
		return v1alpha1.VerificationReasonToolsNotReady
	case podstate.Unschedulable(pod):
		return v1alpha1.VerificationReasonToolsUnscheduled
	case podstate.ImagePullWaiting(pod):
		return v1alpha1.VerificationReasonToolsImagePull
	case podstate.CrashLooping(pod, podstate.RestartBound) != nil:
		return v1alpha1.VerificationReasonToolsCrashLoop
	case since >= jobDeadline+verifyGrace:
		return v1alpha1.VerificationReasonToolsNotReady
	}
	return ""
}

// recordCrashLoop records pod's crash-looping container as the attempt's
// environment: its name, waiting reason, restarts and last termination,
// and its termination message as the fact's log tail, redacted as a
// capture is and bounded, or none while this process does not hold the CDP
// key. The same goes to the log for the operator.
func (v *VerificationReconciler) recordCrashLoop(ctx context.Context, pv *v1alpha1.PreviewVerification,
	pod *corev1.Pod) {
	s := podstate.CrashLooping(pod, podstate.RestartBound)
	if s == nil {
		return
	}
	f := v1alpha1.PartFact{Name: s.Name, Role: v1alpha1.PreviewRoleToolPod, Restarts: max(s.RestartCount, 0)}
	if s.State.Waiting != nil {
		f.Waiting = kubeletWord(s.State.Waiting.Reason)
	}
	if last := s.LastTerminationState.Terminated; last != nil {
		f.LastTermination = &v1alpha1.Termination{Reason: kubeletWord(last.Reason), ExitCode: last.ExitCode}
		if keys, unknown := v.captureKeys(pv); !unknown {
			f.LogTail = lastValidUTF8([]byte(redact(strings.ToValidUTF8(last.Message, "�"), keys)), maxLogTail)
		}
	}
	pv.Status.Environment = []v1alpha1.PartFact{f}
	if v.Log == nil {
		return
	}
	attrs := []slog.Attr{slog.String("verification", pv.Name), slog.String("container", f.Name),
		slog.String("waiting", f.Waiting), slog.Int("restarts", int(f.Restarts)), slog.String("message", f.LogTail)}
	if f.LastTermination != nil {
		attrs = append(attrs, slog.String("lastReason", f.LastTermination.Reason),
			slog.Int("exitCode", int(f.LastTermination.ExitCode)))
	}
	v.Log.LogAttrs(ctx, slog.LevelWarn, "preview tool pod container crash-looping", attrs...)
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
	if pv.Status.Tools == nil && pod != nil && toolsReady(pod, pv.Spec.Tools) {
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
		data, _, _, err := v.toolData(ctx, pv, s)
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

// toolsReady reports the tools container Ready on a pod with an address,
// and every sidecar tools asks for (browser, sandbox) running and Ready:
// the pod's own Ready condition does not wait for a native sidecar that
// restarts after it started.
func toolsReady(pod *corev1.Pod, tools *v1alpha1.ToolsSpec) bool {
	if pod.Status.PodIP == "" || !podCondition(pod, corev1.PodReady) ||
		!containerReady(pod.Status.ContainerStatuses, toolsContainer) {
		return false
	}
	if tools == nil {
		return true
	}
	return (!tools.Browser || containerReady(pod.Status.InitContainerStatuses, browserContainer)) &&
		(!tools.Sandbox || containerReady(pod.Status.InitContainerStatuses, sandboxContainer))
}

// containerReady reports the named container running and Ready.
func containerReady(statuses []corev1.ContainerStatus, name string) bool {
	i := slices.IndexFunc(statuses, func(s corev1.ContainerStatus) bool { return s.Name == name })
	return i >= 0 && statuses[i].Ready && statuses[i].State.Running != nil
}

// recordTools settles a finished tool pod's attempt from its ledger and
// the slot's environment. A ledger that holds is indexed onto status with
// its runs and confirmed checks, stored whole in <pv>-ledger, and its
// evidence in <pv>-evidence.
func (v *VerificationReconciler) recordTools(ctx context.Context, pv *v1alpha1.PreviewVerification,
	s *verifySubject, pod *corev1.Pod, log []byte, finishedAt time.Time) error {
	out := toolledger.Parse(log, podstate.TerminationMessage(pod, toolsContainer))
	facts, err := v.environment(ctx, s.preview, s.slot, pod, pv)
	if err != nil {
		return err
	}
	if out.Reason == "" {
		waits := toolledger.Readiness(out.Ledger.Entries)
		markUnreachable(facts, waits)
		if pv.Status.Readiness, err = crdconv.WaitsToCRD(waits); err != nil {
			out.Reason = v1alpha1.VerificationReasonLedgerMismatch
		}
	}
	phase, reason := route(facts, out.Reason)
	if phase == v1alpha1.VerificationObserved {
		if why := toolledger.RoundReason(pv.Spec.Rung, out.Ledger.Entries, pv.Status.Tools != nil,
			previewtools.ReplayFinished(out.Ledger.Entries)); why != "" {
			phase, reason = v1alpha1.VerificationInfra, why
		}
	}
	if out.Reason == "" {
		if err := v.storeLedger(ctx, pv, out, finishedAt); err != nil {
			return err
		}
	}
	recordToolsSettled(ctx, phase, reason)
	pv.Status.Environment = facts
	pv.Status.TesterImageID = podstate.ImageID(pod, toolsContainer)
	return v.settleWith(ctx, pv, phase, reason)
}

// storeLedger writes the verified ledger's index, runs and confirmed
// checks onto pv's status, and the ledger and its kept evidence into their
// ConfigMaps. A ConfigMap that cannot be written is retried for the grace
// after the Job finished, then left out of status: the index stands.
func (v *VerificationReconciler) storeLedger(ctx context.Context, pv *v1alpha1.PreviewVerification,
	out toolledger.Output, finishedAt time.Time) error {
	entries := out.Ledger.Entries
	images, dropped := keepEvidence(entries, out.Evidence)
	if out.Dropped > 0 {
		dropped[dropMalformed] += out.Dropped
	}
	for why, n := range dropped {
		recordEvidenceDropped(ctx, why, n)
		if v.Log != nil {
			v.Log.LogAttrs(ctx, slog.LevelWarn, "evidence image dropped", slog.String("verification", pv.Name),
				slog.String("reason", why), slog.Int("count", n))
		}
	}
	var ledgerName, evidenceName string
	cm, err := ledgerConfigMap(pv, out.Ledger)
	if err == nil {
		ledgerName, err = v.createOwned(ctx, cm, finishedAt)
	}
	if err != nil {
		return err
	}
	if ev := evidenceConfigMap(pv, images); ev != nil {
		if evidenceName, err = v.createOwned(ctx, ev, finishedAt); err != nil {
			return err
		}
	}
	sum := toolledger.ApplyStatus(&pv.Status, &pv.Spec, out)
	sum.ConfigMap, sum.Evidence = ledgerName, evidenceName
	pv.Status.Ledger = sum
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
// its Job (deleteJobs): the ingress policy, the key Secret and the
// tool ConfigMap, by name (preview-controller holds no read of either),
// and forgets its tool state. Deleting what is gone is no error.
func (v *VerificationReconciler) teardown(ctx context.Context, pv *v1alpha1.PreviewVerification) error {
	if err := v.deleteJobs(ctx, pv); err != nil {
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
// log tails go first, from the last part back; then the ledger fields. An
// Observed round's verdict rests on those, so one that loses them is
// maul's trouble (Infra ledger-mismatch); any other phase rests on none
// of them and keeps its own, so an EnvFailed round is never turned from
// the application's failure into maul's.
func fitToolStatus(pv *v1alpha1.PreviewVerification) {
	for i := len(pv.Status.Environment) - 1; i >= 0 && jsonSize(pv) > maxToolVerificationObject; i-- {
		pv.Status.Environment[i].LogTail = ""
	}
	if n := jsonSize(pv); n >= 0 && n <= maxToolVerificationObject {
		return
	}
	pv.Status.Ledger, pv.Status.Replay, pv.Status.Confirmations, pv.Status.Confirmed = nil, nil, nil, nil
	if pv.Status.Phase == v1alpha1.VerificationObserved {
		pv.Status.Phase = v1alpha1.VerificationInfra
		pv.Status.Reason = v1alpha1.VerificationReasonLedgerMismatch
	}
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

// settle writes a final phase with no observations of its own.
func (v *VerificationReconciler) settle(ctx context.Context, pv *v1alpha1.PreviewVerification,
	phase v1alpha1.PreviewVerificationPhase, reason v1alpha1.PreviewVerificationReason) error {
	return v.settleWith(ctx, pv, phase, reason)
}

// settleWith writes the attempt's final phase and reason, held to the size
// cap, then deletes its Job and what its tool pod owns.
func (v *VerificationReconciler) settleWith(ctx context.Context, pv *v1alpha1.PreviewVerification,
	phase v1alpha1.PreviewVerificationPhase, reason v1alpha1.PreviewVerificationReason) error {
	t := metav1.NewTime(v.now())
	pv.Status.Phase = phase
	pv.Status.Reason = reason
	pv.Status.CompletedAt = &t
	fitToolStatus(pv)
	if err := v.Status().Update(ctx, pv); err != nil {
		return err
	}
	if v.Log != nil {
		v.Log.LogAttrs(ctx, slog.LevelInfo, "preview verification settled",
			slog.String("verification", pv.Name), slog.String("phase", string(pv.Status.Phase)),
			slog.String("reason", string(pv.Status.Reason)))
	}
	return v.teardown(ctx, pv)
}

// deleteJobs deletes, in the background, every Job rendered for the
// verification in any slot (its tool pod's, or a tester Job an earlier
// release rendered for a slice-1 one): a settled attempt frees the slot's
// Job quota for the next.
func (v *VerificationReconciler) deleteJobs(ctx context.Context, pv *v1alpha1.PreviewVerification) error {
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
	pods, err := v.allComponentPods(ctx, p, i, slot)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(pods, func(pod corev1.Pod) bool { return !livePod(&pod) }), nil
}

// allComponentPods are every Pod of component i, finished and deleting
// ones included. The reconciler's Client reads the API server directly
// (preview-controller wires it uncached), so this is the live state.
func (v *VerificationReconciler) allComponentPods(ctx context.Context, p *v1alpha1.Preview, i int,
	slot int32) ([]corev1.Pod, error) {
	var pods corev1.PodList
	if err := v.List(ctx, &pods, client.InNamespace(v.Settings.slotName(slot)),
		client.MatchingLabels(componentLabels(p, i))); err != nil {
		return nil, err
	}
	return pods.Items, nil
}

// livePod reports whether a Pod is neither deleting nor finished.
func livePod(pod *corev1.Pod) bool {
	return pod.DeletionTimestamp.IsZero() && pod.Status.Phase != corev1.PodSucceeded &&
		pod.Status.Phase != corev1.PodFailed
}

// errNoClusterIP is a component Service with no ClusterIP to render the
// tester's target from: maul's render error.
var errNoClusterIP = errors.New("component Service has no ClusterIP")

// serviceTargets maps each component to its Service's ClusterIP on the
// Service port, read live: the tester dials it and resolves no name, so
// cluster DNS, which every Pod on the node shares, cannot keep it from a
// component. A Service that is missing, headless or not yet allocated is
// errNoClusterIP.
func (v *VerificationReconciler) serviceTargets(ctx context.Context, p *v1alpha1.Preview,
	slot int32) (map[string]string, error) {
	targets := make(map[string]string, len(p.Spec.Components))
	for i, c := range p.Spec.Components {
		var svc corev1.Service
		key := types.NamespacedName{Namespace: v.Settings.slotName(slot), Name: v1alpha1.PreviewComponentServiceName(p, i)}
		if err := v.Get(ctx, key, &svc); kerrors.IsNotFound(err) {
			return nil, fmt.Errorf("%w: %s", errNoClusterIP, key.Name)
		} else if err != nil {
			return nil, err
		}
		ip := svc.Spec.ClusterIP
		if ip == "" || ip == corev1.ClusterIPNone || net.ParseIP(ip) == nil {
			return nil, fmt.Errorf("%w: %s", errNoClusterIP, key.Name)
		}
		targets[c.Name] = net.JoinHostPort(ip, strconv.Itoa(int(v1alpha1.PreviewServicePort)))
	}
	return targets, nil
}

// errJobPods is a verification's Job with more than one Pod, which a
// run-once Job never has.
var errJobPods = errors.New("verification Job has more than one Pod")

// liveJob is the request's Job (its tool pod's), nil when it is gone or
// not the one rendered for it.
func (v *VerificationReconciler) liveJob(ctx context.Context, pv *v1alpha1.PreviewVerification,
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

// jobPod is the Pod the Job's controller created for it (by the Job's
// UID, as its controller reference), nil before there is one.
func (v *VerificationReconciler) jobPod(ctx context.Context, job *batchv1.Job) (*corev1.Pod, error) {
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
			return nil, errJobPods
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

// startedAt is when the tool pod was rendered.
func startedAt(pv *v1alpha1.PreviewVerification) time.Time {
	if pv.Status.StartedAt != nil {
		return pv.Status.StartedAt.Time
	}
	return pv.CreationTimestamp.Time
}

// markUnreachable marks each component the relay never reached, for its
// whole readiness window, on its environment fact. A Ready component the
// relay could not connect to for the whole window refused it: the window
// absorbs maul's slot policy being programmed, and past it only the
// application can be the cause, so the environment classifies it as the
// application's failure, however its attempts failed: the relay dials a
// ClusterIP and resolves no name.
func markUnreachable(facts []v1alpha1.PartFact, waits []httpstep.TargetWait) {
	for _, w := range waits {
		if w.Reached() {
			continue
		}
		for i := range facts {
			if facts[i].Role == v1alpha1.PreviewRoleComponent && facts[i].Name == w.Target {
				facts[i].UnreachableForMs = max(w.WaitMs, 1)
			}
		}
	}
}

// route settles an attempt from its environment and its output: a part of
// the change failing is the intent's (EnvFailed); trouble the change cannot
// cause is maul's (Infra), as is output that does not hold; else Observed.
// The classification is httpstep.ClassifyEnvironment's, the one
// intent-controller applies to the same facts.
func route(facts []v1alpha1.PartFact,
	outReason v1alpha1.PreviewVerificationReason) (v1alpha1.PreviewVerificationPhase, v1alpha1.PreviewVerificationReason) {
	parts := crdconv.PartFacts(facts)
	switch httpstep.ClassifyEnvironment(parts) {
	case httpstep.EnvAppFailure:
		return v1alpha1.VerificationEnvFailed, appFailureReason(facts, httpstep.AppFailureParts(parts))
	case httpstep.EnvInfra:
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
	case f.CrashedDuring && f.LastTermination != nil && f.LastTermination.Reason == "OOMKilled":
		return v1alpha1.VerificationReasonComponentOOM
	case podstate.ImagePullReason(f.Waiting):
		return v1alpha1.VerificationReasonComponentImagePull
	case f.Waiting != "":
		return v1alpha1.VerificationReasonComponentCrashLoop
	case f.Evicted:
		return v1alpha1.VerificationReasonComponentEvicted
	case !f.Ready:
		return v1alpha1.VerificationReasonComponentNotReady
	case f.CrashedDuring:
		return v1alpha1.VerificationReasonComponentCrashed
	case f.UnreachableForMs > 0:
		return v1alpha1.VerificationReasonComponentUnreachable
	}
	return v1alpha1.VerificationReasonComponentNotReady
}

// infraReason names maul's trouble: the tool pod not scheduled or an
// image of it not pulled, else what was wrong with its output, else a part
// no node would take.
func infraReason(facts []v1alpha1.PartFact,
	outReason v1alpha1.PreviewVerificationReason) v1alpha1.PreviewVerificationReason {
	for _, f := range facts {
		if f.Role != v1alpha1.PreviewRoleToolPod {
			continue
		}
		switch {
		case f.Unschedulable:
			return v1alpha1.VerificationReasonToolsUnscheduled
		case podstate.ImagePullReason(f.Waiting):
			return v1alpha1.VerificationReasonToolsImagePull
		}
	}
	for _, f := range facts {
		if f.Disrupted && f.Role == v1alpha1.PreviewRoleComponent {
			return v1alpha1.VerificationReasonComponentDisrupted
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

// jsonSize is v's length as json.Marshal writes it, as the API machinery
// sends and stores it; -1 when it does not encode.
func jsonSize(v any) int {
	b, err := json.Marshal(v)
	if err != nil {
		return -1
	}
	return len(b)
}
