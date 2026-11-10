// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package podstate

import (
	"slices"

	corev1 "k8s.io/api/core/v1"
)

// RestartBound is how many restarts of a container, its last exit in
// error, make a crash loop even between the kubelet's back-offs.
const RestartBound = 3

// MaxImageIDLength is the longest image ID a verification's status holds
// (its field's MaxLength).
const MaxImageIDLength = 512

// statuses is every container status of pod, the inits' (and so the native
// sidecars') first.
func statuses(pod *corev1.Pod) []corev1.ContainerStatus {
	return slices.Concat(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses)
}

// TerminationMessage is the named container's termination message, "" while
// it has not terminated.
func TerminationMessage(pod *corev1.Pod, container string) string {
	for _, s := range pod.Status.ContainerStatuses {
		if s.Name == container && s.State.Terminated != nil {
			return s.State.Terminated.Message
		}
	}
	return ""
}

// ImageID is the image the named container ran, as the kubelet reports it;
// "" for one longer than MaxImageIDLength, which status would refuse.
func ImageID(pod *corev1.Pod, container string) string {
	for _, s := range pod.Status.ContainerStatuses {
		if s.Name == container && len(s.ImageID) <= MaxImageIDLength {
			return s.ImageID
		}
	}
	return ""
}

// CrashLooping is the first of pod's containers, a sidecar's included, that
// keeps exiting in error: the kubelet backing it off, or restarted at least
// bound times with its last exit in error. nil for none.
func CrashLooping(pod *corev1.Pod, bound int32) *corev1.ContainerStatus {
	all := statuses(pod)
	i := slices.IndexFunc(all, func(s corev1.ContainerStatus) bool {
		last := s.LastTerminationState.Terminated
		return s.State.Waiting != nil && s.State.Waiting.Reason == "CrashLoopBackOff" ||
			s.RestartCount >= bound && last != nil && last.ExitCode != 0
	})
	if i < 0 {
		return nil
	}
	return &all[i]
}

// ImagePullReason reports a waiting reason of an image that is not pulled.
func ImagePullReason(reason string) bool {
	switch reason {
	case "ErrImagePull", "ImagePullBackOff", "InvalidImageName", "ErrImageNeverPull":
		return true
	}
	return false
}

// ImagePullWaiting reports any container, a sidecar's included, waiting on
// its image.
func ImagePullWaiting(pod *corev1.Pod) bool {
	return slices.ContainsFunc(statuses(pod), func(s corev1.ContainerStatus) bool {
		return s.State.Waiting != nil && ImagePullReason(s.State.Waiting.Reason)
	})
}

// Unschedulable reports a pod the scheduler could not place.
func Unschedulable(pod *corev1.Pod) bool {
	return slices.ContainsFunc(pod.Status.Conditions, func(c corev1.PodCondition) bool {
		return c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse &&
			c.Reason == corev1.PodReasonUnschedulable
	})
}
