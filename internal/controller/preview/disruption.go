// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// What happened to a component during a verification attempt, and whose
// it is. The attempt is the tester's run: from its Pod's start, less the
// clock skew, to its container's termination (or now, while it runs).
// Nothing outside that window is charged, and nothing is placed in time by
// deletionTimestamp, which is a deletion's request time plus its grace
// period: an event is placed by a condition's transition or a container's
// own timestamps.
//
// The application's (EnvFailed): its process exiting, OOM included, and
// the kubelet evicting its Pod, since the memory or ephemeral storage it
// used is its own doing. maul's (Infra, retried): anything done through
// the API (the Eviction API, a drain, preemption, the taint manager, Pod
// GC, a deletion), node loss, and a replacement whose old Pod shows no
// cause of the application's. The application holds no API credential, so
// it can cause none of these.

// clockSkew is the tolerance between the kubelet's clock, which stamps a
// container's start and termination, and the API server's, which stamps a
// Pod's creation and conditions, when either is compared with the tester
// Pod's start.
const clockSkew = 2 * time.Second

// attemptWindow is the tester's run, [start - clockSkew, end].
type attemptWindow struct {
	start, end time.Time
}

// holds reports whether t falls in the window.
func (w attemptWindow) holds(t metav1.Time) bool {
	return !t.IsZero() && !t.Time.Before(w.start.Add(-clockSkew)) && !t.After(w.end)
}

// attemptStart is when the attempt began: the tester Pod's start as its
// kubelet reported it (the same clock domain as the components'
// timestamps), else its container's, else when the tester was rendered. It
// needs no state: an attempt already running when the controller was
// upgraded, or a start pass re-entered, judges the same.
func attemptStart(tester *corev1.Pod, pv *v1alpha1.PreviewVerification) time.Time {
	if tester != nil {
		if tester.Status.StartTime != nil && !tester.Status.StartTime.IsZero() {
			return tester.Status.StartTime.Time
		}
		if s := testerStatus(tester); s != nil {
			switch {
			case s.State.Running != nil && !s.State.Running.StartedAt.IsZero():
				return s.State.Running.StartedAt.Time
			case s.State.Terminated != nil && !s.State.Terminated.StartedAt.IsZero():
				return s.State.Terminated.StartedAt.Time
			}
		}
	}
	return startedAt(pv)
}

// attemptEnd is when the attempt ended: the tester container's
// termination, read from its Pod as the reconciler read it live, else now,
// while it runs.
func attemptEnd(tester *corev1.Pod, now time.Time) time.Time {
	if s := testerStatus(tester); s != nil && s.State.Terminated != nil && !s.State.Terminated.FinishedAt.IsZero() {
		return s.State.Terminated.FinishedAt.Time
	}
	return now
}

func testerStatus(tester *corev1.Pod) *corev1.ContainerStatus {
	if tester == nil {
		return nil
	}
	for i := range tester.Status.ContainerStatuses {
		if tester.Status.ContainerStatuses[i].Name == testerContainer {
			return &tester.Status.ContainerStatuses[i]
		}
	}
	return nil
}

// podCauseKind is what happened to a component Pod during the attempt,
// in rising precedence: the cluster disrupting a Pod may explain the rest.
type podCauseKind int

const (
	causeNone podCauseKind = iota
	// causeCrashed: the component's process exited (OOM included). The
	// application's: component-crashed.
	causeCrashed
	// causeEvicted: the kubelet evicted the Pod. The application's:
	// component-evicted.
	causeEvicted
	// causeDisrupted: the cluster took the Pod away. maul's:
	// component-disrupted.
	causeDisrupted
)

func (k podCauseKind) String() string {
	return [...]string{"none", "crashed", "evicted", "disrupted"}[k]
}

// Kubelet and controller reasons the table reads.
const (
	podReasonEvicted  = "Evicted"
	podReasonNodeLost = "NodeLost"
)

// podCause is the one table of what happened to a component Pod during
// the attempt w, for its container `container`:
//
//   - a DisruptionTarget condition placed in w: TerminationByKubelet is the
//     kubelet's eviction (evicted, the application's); EvictionByEvictionAPI,
//     PreemptionByScheduler, DeletionByTaintManager, DeletionByPodGC and any
//     reason not known yet were done through the API (disrupted, maul's);
//   - status.reason Evicted, the Pod's Ready transition in w: the kubelet's
//     eviction (evicted);
//   - status.reason NodeLost, or Ready Unknown, placed in w: node loss
//     (disrupted);
//   - a Pod being deleted, its Ready transition or its container's end in
//     w: a deletion through the API (disrupted), whatever its container did
//     while it was stopped;
//   - the container's last termination, its current termination, or the
//     current run of a container that has restarted, placed in w: its
//     process exited (crashed);
//   - else nothing.
func podCause(pod *corev1.Pod, container string, w attemptWindow) podCauseKind {
	if c := podConditionOf(pod, corev1.DisruptionTarget); c != nil && c.Status == corev1.ConditionTrue &&
		w.holds(c.LastTransitionTime) {
		return disruptionCause(c.Reason)
	}
	ready := podConditionOf(pod, corev1.PodReady)
	readyMoved := ready != nil && w.holds(ready.LastTransitionTime)
	ended := containerEnded(pod, container, w)
	switch {
	case pod.Status.Reason == podReasonEvicted && readyMoved:
		return causeEvicted
	case (pod.Status.Reason == podReasonNodeLost || ready != nil && ready.Status == corev1.ConditionUnknown) &&
		readyMoved:
		return causeDisrupted
	case pod.DeletionTimestamp != nil && (readyMoved || ended):
		return causeDisrupted
	case ended:
		return causeCrashed
	}
	return causeNone
}

// disruptionCause attributes a DisruptionTarget condition by its reason.
func disruptionCause(reason string) podCauseKind {
	switch reason {
	case "TerminationByKubelet":
		// The kubelet's own eviction: node pressure from memory or
		// ephemeral storage, the application's own use.
		return causeEvicted
	case "EvictionByEvictionAPI", "PreemptionByScheduler", "DeletionByTaintManager", "DeletionByPodGC":
		return causeDisrupted
	default:
		// A reason not known yet: set by a control-plane component, never
		// by the application, which holds no API credential.
		return causeDisrupted
	}
}

// containerEnded reports whether the Pod's container ended a run in w: its
// last termination, its current termination, or the current run of a
// container that has restarted began in w.
func containerEnded(pod *corev1.Pod, container string, w attemptWindow) bool {
	for _, s := range pod.Status.ContainerStatuses {
		if s.Name != container {
			continue
		}
		if t := s.LastTerminationState.Terminated; t != nil && w.holds(t.FinishedAt) {
			return true
		}
		if t := s.State.Terminated; t != nil && w.holds(t.FinishedAt) {
			return true
		}
		if r := s.State.Running; r != nil && s.RestartCount > 0 && w.holds(r.StartedAt) {
			return true
		}
	}
	return false
}

// podConditionOf is the Pod's condition of type t, nil when it has none.
func podConditionOf(pod *corev1.Pod, t corev1.PodConditionType) *corev1.PodCondition {
	for i := range pod.Status.Conditions {
		if pod.Status.Conditions[i].Type == t {
			return &pod.Status.Conditions[i]
		}
	}
	return nil
}

// componentCause is what happened to a component during the attempt, over
// all its Pods: the gravest of each Pod's podCause, and, when its current
// Pod was created during the attempt and no Pod shows a cause, a
// replacement by the cluster (disrupted). A Pod created a moment before
// the tester (a fast cluster serves one within seconds) is the one it ran
// against, never a replacement.
func componentCause(all []corev1.Pod, current *corev1.Pod, container string, w attemptWindow) podCauseKind {
	cause := causeNone
	for i := range all {
		cause = max(cause, podCause(&all[i], container, w))
	}
	if cause == causeNone && current != nil && current.CreationTimestamp.After(w.start.Add(clockSkew)) &&
		!current.CreationTimestamp.After(w.end) {
		return causeDisrupted
	}
	return cause
}
