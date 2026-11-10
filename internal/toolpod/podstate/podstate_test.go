// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package podstate

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// pod is a pod with inits (sidecars) and containers.
func pod(inits, containers []corev1.ContainerStatus, conds ...corev1.PodCondition) *corev1.Pod {
	return &corev1.Pod{Status: corev1.PodStatus{InitContainerStatuses: inits, ContainerStatuses: containers,
		Conditions: conds}}
}

func waiting(name, reason string) corev1.ContainerStatus {
	return corev1.ContainerStatus{Name: name, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
		Reason: reason}}}
}

func restarted(name string, n, exit int32) corev1.ContainerStatus {
	return corev1.ContainerStatus{Name: name, RestartCount: n, LastTerminationState: corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{ExitCode: exit}}}
}

func TestTerminationMessageAndImageID(t *testing.T) {
	done := corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: "summary"}}
	for _, tc := range []struct {
		name          string
		statuses      []corev1.ContainerStatus
		msg, image    string
		containerName string
	}{
		{"terminated", []corev1.ContainerStatus{{Name: "tools", State: done, ImageID: "img@sha256:1"}},
			"summary", "img@sha256:1", "tools"},
		{"running: no message", []corev1.ContainerStatus{{Name: "tools", ImageID: "img@sha256:1"}},
			"", "img@sha256:1", "tools"},
		{"another container", []corev1.ContainerStatus{{Name: "other", State: done, ImageID: "img@sha256:1"}},
			"", "", "tools"},
		{"image ID at the bound", []corev1.ContainerStatus{{Name: "tools", ImageID: strings.Repeat("a", 512)}},
			"", strings.Repeat("a", 512), "tools"},
		{"image ID over the bound", []corev1.ContainerStatus{{Name: "tools", ImageID: strings.Repeat("a", 513)}},
			"", "", "tools"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := pod(nil, tc.statuses)
			if got := TerminationMessage(p, tc.containerName); got != tc.msg {
				t.Errorf("message = %q, want %q", got, tc.msg)
			}
			if got := ImageID(p, tc.containerName); got != tc.image {
				t.Errorf("image ID = %q, want %q", got, tc.image)
			}
		})
	}
}

func TestCrashLooping(t *testing.T) {
	for _, tc := range []struct {
		name  string
		p     *corev1.Pod
		first string
	}{
		{"none", pod(nil, []corev1.ContainerStatus{{Name: "tools"}}), ""},
		{"backed off", pod(nil, []corev1.ContainerStatus{waiting("tools", "CrashLoopBackOff")}), "tools"},
		{"a sidecar backed off", pod([]corev1.ContainerStatus{waiting("browser", "CrashLoopBackOff")},
			[]corev1.ContainerStatus{{Name: "tools"}}), "browser"},
		{"restarted to the bound in error", pod(nil, []corev1.ContainerStatus{restarted("tools", RestartBound, 1)}),
			"tools"},
		{"below the bound", pod(nil, []corev1.ContainerStatus{restarted("tools", RestartBound-1, 1)}), ""},
		{"restarted cleanly", pod(nil, []corev1.ContainerStatus{restarted("tools", RestartBound, 0)}), ""},
		{"the inits first", pod([]corev1.ContainerStatus{restarted("sandbox", RestartBound, 2)},
			[]corev1.ContainerStatus{waiting("tools", "CrashLoopBackOff")}), "sandbox"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := CrashLooping(tc.p, RestartBound)
			switch {
			case tc.first == "" && got != nil:
				t.Errorf("crash loop %q, want none", got.Name)
			case tc.first != "" && (got == nil || got.Name != tc.first):
				t.Errorf("crash loop %v, want %q", got, tc.first)
			}
		})
	}
}

func TestImagePullWaiting(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    *corev1.Pod
		want bool
	}{
		{"none", pod(nil, []corev1.ContainerStatus{{Name: "tools"}}), false},
		{"back-off", pod(nil, []corev1.ContainerStatus{waiting("tools", "ImagePullBackOff")}), true},
		{"a sidecar's pull", pod([]corev1.ContainerStatus{waiting("sandbox-head", "ErrImagePull")}, nil), true},
		{"invalid name", pod(nil, []corev1.ContainerStatus{waiting("tools", "InvalidImageName")}), true},
		{"never pull", pod(nil, []corev1.ContainerStatus{waiting("tools", "ErrImageNeverPull")}), true},
		{"creating", pod(nil, []corev1.ContainerStatus{waiting("tools", "ContainerCreating")}), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ImagePullWaiting(tc.p); got != tc.want {
				t.Errorf("ImagePullWaiting = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestUnschedulable(t *testing.T) {
	cond := func(status corev1.ConditionStatus, reason string) corev1.PodCondition {
		return corev1.PodCondition{Type: corev1.PodScheduled, Status: status, Reason: reason}
	}
	for _, tc := range []struct {
		name string
		p    *corev1.Pod
		want bool
	}{
		{"no condition", pod(nil, nil), false},
		{"unschedulable", pod(nil, nil, cond(corev1.ConditionFalse, corev1.PodReasonUnschedulable)), true},
		{"scheduled", pod(nil, nil, cond(corev1.ConditionTrue, "")), false},
		{"not yet, another reason", pod(nil, nil, cond(corev1.ConditionFalse, "SchedulingGated")), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Unschedulable(tc.p); got != tc.want {
				t.Errorf("Unschedulable = %t, want %t", got, tc.want)
			}
		})
	}
}
