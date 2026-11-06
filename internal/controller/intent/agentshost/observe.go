// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentshost

import (
	"context"
	"log/slog"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/jobs"
	"github.com/devthenet-labs/maul/internal/runnerguard"
	"github.com/devthenet-labs/maul/internal/toolpod"
)

// observe follows a running tool pod: its sandbox probe refusing (exit 78,
// the breaker tripped), an eviction, or a pod that will not get going is
// Infra; once tools and both sandboxes are Ready it records where the
// evaluator reaches it; once the Job has finished it collects the ledger.
func (r *Reconciler) observe(ctx context.Context, pv *v1alpha1.PreviewVerification) (ctrl.Result, error) {
	name := JobName(pv)
	job, pod, err := r.Tools.Get(ctx, name)
	if err != nil {
		return ctrl.Result{}, err
	}
	if job == nil || job.Labels[v1alpha1.LabelPreviewVerification] != string(pv.UID) {
		return ctrl.Result{}, r.settle(ctx, pv, v1alpha1.VerificationInfra, v1alpha1.VerificationReasonToolsNotReady)
	}
	if pod != nil && sandboxRefused(pod) {
		// The pod's network is open: no repository image is run again until
		// the operator fixes the CNI and restarts the controller.
		r.Images.Breaker.Trip(ctx, name, pv.Spec.IntentRef.Name)
		return ctrl.Result{}, r.settle(ctx, pv, v1alpha1.VerificationInfra, v1alpha1.VerificationReasonSandboxUnenforced)
	}
	if pod != nil && evicted(pod) {
		return ctrl.Result{}, r.settle(ctx, pv, v1alpha1.VerificationInfra, v1alpha1.VerificationReasonToolsEvicted)
	}
	finishedAt, finished := jobFinished(job)
	if !finished {
		if reason := r.stuck(pv, pod); reason != "" {
			r.log().LogAttrs(ctx, slog.LevelWarn, "the agents tool pod will not finish",
				slog.String("verification", pv.Name), slog.String("reason", string(reason)))
			return ctrl.Result{}, r.settle(ctx, pv, v1alpha1.VerificationInfra, reason)
		}
		if pv.Status.Tools == nil && pod != nil && toolsReady(pod) {
			pv.Status.Tools = &v1alpha1.ToolsStatus{PodIP: pod.Status.PodIP, Port: jobs.ToolsPort, Ready: true}
			return r.wait(), r.Status().Update(ctx, pv)
		}
		return r.wait(), nil
	}
	switch {
	case pod == nil:
		return ctrl.Result{}, r.settle(ctx, pv, v1alpha1.VerificationInfra, v1alpha1.VerificationReasonToolsDeadline)
	case !toolsTerminated(pod):
		// An init failed (a tree that did not fetch or verify, a seed), or
		// the pod ended before tools ran: there is no ledger to read.
		return ctrl.Result{}, r.settle(ctx, pv, v1alpha1.VerificationInfra, v1alpha1.VerificationReasonToolsNotReady)
	}
	log, err := r.Tools.Log(ctx, pod)
	if err != nil {
		if !kerrors.IsNotFound(err) && r.now().Sub(finishedAt) < grace {
			r.log().LogAttrs(ctx, slog.LevelInfo, "the tools log is not readable yet",
				slog.String("verification", pv.Name), slog.Any("error", err))
			return r.wait(), nil
		}
		return ctrl.Result{}, r.settle(ctx, pv, v1alpha1.VerificationInfra, v1alpha1.VerificationReasonLedgerMismatch)
	}
	return ctrl.Result{}, r.collect(ctx, pv, pod, log)
}

// stuck is why a running tool pod will not finish, past its grace: no pod
// created (a quota or LimitRange refusal), none scheduled, an image not
// pulled (at once for one that will never pull, runnerguard's judgement),
// a container crash-looping, or the Job's own deadline long past. "" while
// it may yet.
func (r *Reconciler) stuck(pv *v1alpha1.PreviewVerification, pod *corev1.Pod) v1alpha1.PreviewVerificationReason {
	start := pv.CreationTimestamp.Time
	if pv.Status.StartedAt != nil {
		start = pv.Status.StartedAt.Time
	}
	since := r.now().Sub(start)
	if pod != nil {
		if _, why := runnerguard.Pending(pullStatus(pod, start), r.now()); why != "" {
			return v1alpha1.VerificationReasonToolsImagePull
		}
	}
	if since < grace {
		return ""
	}
	jobDeadline, _, _ := toolpod.Deadlines(pv.Spec.Timeout.Duration, v1alpha1.MaxAgentsVerificationTimeout+
		deadlineMargin)
	switch {
	case pod == nil:
		return v1alpha1.VerificationReasonToolsNotReady
	case unschedulable(pod):
		return v1alpha1.VerificationReasonToolsUnscheduled
	case imagePullWaiting(pod):
		return v1alpha1.VerificationReasonToolsImagePull
	case crashLooping(pod):
		return v1alpha1.VerificationReasonToolsCrashLoop
	case since >= jobDeadline+grace:
		return v1alpha1.VerificationReasonToolsDeadline
	}
	return ""
}

// statuses is every container status of pod, the inits' first.
func statuses(pod *corev1.Pod) []corev1.ContainerStatus {
	return slices.Concat(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses)
}

// sandboxRefused reports prepare's exit with jobs.ExitSandboxUnenforced:
// the strict probe found the pod's egress open.
func sandboxRefused(pod *corev1.Pod) bool {
	for _, s := range pod.Status.InitContainerStatuses {
		if s.Name == prepareContainer && s.State.Terminated != nil &&
			s.State.Terminated.ExitCode == jobs.ExitSandboxUnenforced {
			return true
		}
	}
	return false
}

// evicted reports a pod the cluster took away: evicted, preempted, or its
// node lost (a DisruptionTarget condition).
func evicted(pod *corev1.Pod) bool {
	if pod.Status.Reason == "Evicted" {
		return true
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.DisruptionTarget && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// pullStatus is the jobs.Status runnerguard.Pending judges a
// repository-image pod's pull by: the first container waiting on its image,
// from start.
func pullStatus(pod *corev1.Pod, start time.Time) jobs.Status {
	st := jobs.Status{RunnerImageSource: v1alpha1.RunnerImageSourceRepository, AgentStarted: true, Created: start}
	for _, s := range statuses(pod) {
		if s.State.Waiting != nil && imagePullReason(s.State.Waiting.Reason) {
			st.Waiting, st.WaitingMessage, st.AgentStarted = s.State.Waiting.Reason, s.State.Waiting.Message, false
			break
		}
	}
	return st
}

// imagePullReason reports a waiting reason of an image that is not pulled.
func imagePullReason(reason string) bool {
	switch reason {
	case "ErrImagePull", "ImagePullBackOff", "InvalidImageName", "ErrImageNeverPull":
		return true
	}
	return false
}

// imagePullWaiting reports any container waiting on its image.
func imagePullWaiting(pod *corev1.Pod) bool {
	return slices.ContainsFunc(statuses(pod), func(s corev1.ContainerStatus) bool {
		return s.State.Waiting != nil && imagePullReason(s.State.Waiting.Reason)
	})
}

// unschedulable reports a pod the scheduler could not place.
func unschedulable(pod *corev1.Pod) bool {
	return slices.ContainsFunc(pod.Status.Conditions, func(c corev1.PodCondition) bool {
		return c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse &&
			c.Reason == corev1.PodReasonUnschedulable
	})
}

// crashLooping reports a container, a sidecar's included, that keeps
// exiting in error: backed off, or restarted restartBound times with its
// last exit in error.
func crashLooping(pod *corev1.Pod) bool {
	return slices.ContainsFunc(statuses(pod), func(s corev1.ContainerStatus) bool {
		last := s.LastTerminationState.Terminated
		return s.State.Waiting != nil && s.State.Waiting.Reason == "CrashLoopBackOff" ||
			s.RestartCount >= restartBound && last != nil && last.ExitCode != 0
	})
}

// toolsReady reports the pod Ready with an address, the tools container
// Ready and both sandboxes running and Ready: the pod's own Ready
// condition does not wait for a native sidecar that restarts after it
// started.
func toolsReady(pod *corev1.Pod) bool {
	if pod.Status.PodIP == "" || !slices.ContainsFunc(pod.Status.Conditions, func(c corev1.PodCondition) bool {
		return c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue
	}) || !containerReady(pod.Status.ContainerStatuses, toolsContainer) {
		return false
	}
	for _, sb := range sandboxes {
		if !containerReady(pod.Status.InitContainerStatuses, sb) {
			return false
		}
	}
	return true
}

// containerReady reports the named container running and Ready.
func containerReady(statuses []corev1.ContainerStatus, name string) bool {
	i := slices.IndexFunc(statuses, func(s corev1.ContainerStatus) bool { return s.Name == name })
	return i >= 0 && statuses[i].Ready && statuses[i].State.Running != nil
}
