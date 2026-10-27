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
// own timestamps. Only Pods of the component's current revision (its
// spec's image and reset generation: runsComponent) count; a previous
// revision's Pods, which a rollout is free to evict, delete or let fail,
// are ignored entirely, so they can neither charge the change nor mask its
// crash.
//
// The application is charged (EnvFailed) only on unambiguous evidence in
// such a Pod: its container terminating non-zero or OOM-killed
// (component-crashed), or the kubelet evicting it, status.reason Evicted
// (component-evicted: the memory or ephemeral storage it used). Everything
// else is maul's (Infra component-disrupted, retried, bounded by the
// attempts): a Pod going not-Ready with no such evidence (a node going
// NotReady sets Ready False with no reason), node loss, a deletion, a
// drain, preemption, the taint manager, Pod GC, a graceful node shutdown or
// kubelet preemption (DisruptionTarget TerminationByKubelet without
// status.reason Evicted), an unknown disruption, a clean exit, or a
// replacement with no evidence. The application holds no API credential, so
// it causes none of these; the most it can do without crashing is make
// itself not-Ready, which earns an honest, bounded Infra and then
// Unavailable, never a pass, while its preview goes not-live and shows the
// error.

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

// podCauseKind is what happened to a component Pod during the attempt, in
// rising precedence: the application's unambiguous evidence wins over a
// disruption, which explains nothing it shows.
type podCauseKind int

const (
	causeNone podCauseKind = iota
	// causeDisrupted: the cluster took the Pod away, or it went not-Ready
	// with no evidence of the application's. maul's: component-disrupted.
	causeDisrupted
	// causeCrashed: the component's process exited non-zero or was
	// OOM-killed. The application's: component-crashed.
	causeCrashed
	// causeEvicted: the kubelet evicted the Pod (status.reason Evicted).
	// The application's: component-evicted.
	causeEvicted
)

func (k podCauseKind) String() string {
	return [...]string{"none", "disrupted", "crashed", "evicted"}[k]
}

// podReasonEvicted is the status.reason of a Pod the kubelet evicted.
const podReasonEvicted = "Evicted"

// podCause is the one table of what happened to a Pod of a component's
// current revision during the attempt w, for its container `container`:
//
//   - status.reason Evicted, placed in w by its Ready or DisruptionTarget
//     transition or its container's end: the kubelet's eviction (evicted,
//     the application's);
//   - a DisruptionTarget condition placed in w, whatever its reason
//     (EvictionByEvictionAPI, PreemptionByScheduler, DeletionByTaintManager,
//     DeletionByPodGC, TerminationByKubelet without Evicted, as a graceful
//     node shutdown or kubelet preemption sets it, or one not known yet):
//     disrupted;
//   - a Pod being deleted, its Ready transition or its container's end in
//     w: a deletion through the API (disrupted), whatever exit code its
//     stopped container gave;
//   - its container terminated in w non-zero or OOM-killed: crashed, the
//     application's;
//   - its container terminated in w otherwise (a clean exit), or its Ready
//     condition moved off True in w (node NotReady or lost, or the
//     application making itself not-Ready): disrupted;
//   - else nothing.
func podCause(pod *corev1.Pod, container string, w attemptWindow) podCauseKind {
	ready := podConditionOf(pod, corev1.PodReady)
	readyMoved := ready != nil && w.holds(ready.LastTransitionTime)
	dt := podConditionOf(pod, corev1.DisruptionTarget)
	dtMoved := dt != nil && dt.Status == corev1.ConditionTrue && w.holds(dt.LastTransitionTime)
	crashed, ended := containerTerminated(pod, container, w)
	switch {
	case pod.Status.Reason == podReasonEvicted && (readyMoved || dtMoved || ended):
		return causeEvicted
	case dtMoved:
		return causeDisrupted
	case pod.DeletionTimestamp != nil && (readyMoved || ended):
		return causeDisrupted
	case crashed:
		return causeCrashed
	case ended, readyMoved && ready.Status != corev1.ConditionTrue:
		return causeDisrupted
	}
	return causeNone
}

// containerTerminated reports whether the Pod's container terminated in w,
// and whether it did so non-zero or OOM-killed: its last termination, or
// its current one.
func containerTerminated(pod *corev1.Pod, container string, w attemptWindow) (crashed, ended bool) {
	for _, s := range pod.Status.ContainerStatuses {
		if s.Name != container {
			continue
		}
		for _, t := range []*corev1.ContainerStateTerminated{s.LastTerminationState.Terminated, s.State.Terminated} {
			if t == nil || !w.holds(t.FinishedAt) {
				continue
			}
			ended = true
			if t.ExitCode != 0 || t.Reason == "OOMKilled" {
				crashed = true
			}
		}
	}
	return crashed, ended
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

// componentCause is what happened to component c during the attempt: the
// gravest podCause over the Pods of its current revision (runsComponent),
// every other Pod ignored; and, when none shows anything, the cluster's
// (disrupted) if its current Pod was created during the attempt, a
// replacement, or is missing. A Pod created a moment before the tester (a
// fast cluster serves one within seconds) is the one it ran against, never
// a replacement.
func componentCause(all []corev1.Pod, current *corev1.Pod, p *v1alpha1.Preview, c v1alpha1.PreviewComponent,
	w attemptWindow) podCauseKind {
	cause := causeNone
	for i := range all {
		if runsComponent(&all[i], p, c) {
			cause = max(cause, podCause(&all[i], c.Name, w))
		}
	}
	if cause != causeNone {
		return cause
	}
	if current == nil || current.CreationTimestamp.After(w.start.Add(clockSkew)) &&
		!current.CreationTimestamp.After(w.end) {
		return causeDisrupted
	}
	return causeNone
}
