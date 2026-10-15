// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package jobs

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

// errTerminated is the API server's answer for a container whose log the
// kubelet no longer has (live: an agent pod evicted for ephemeral storage).
var errTerminated = errors.New(`container "agent" in pod "patchy-x-int-a1-xgrsm" is terminated`)

// goneJob is the live Job this regression comes from (0.12.37).
const goneJob = "patchy-71b6a6d37a-int-a1"

// failedJob is goneJob with its Failed condition true since failedAt.
func failedJob(reason string, failedAt time.Time) *batchv1.Job {
	job := bareJob(goneJob)
	job.Status.Conditions = []batchv1.JobCondition{{
		Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: reason,
		LastTransitionTime: metav1.NewTime(failedAt),
	}}
	return job
}

// evictedPod is the pod the kubelet leaves after evicting it: failed, with
// the eviction reason, and the agent container terminated with no container
// ID, its log removed with it.
func evictedPod(jobName string) *corev1.Pod {
	pod := jobPodInState(jobName, corev1.PodFailed, corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{Reason: "ContainerStatusUnknown", ExitCode: 137},
	})
	pod.Status.Reason = "Evicted"
	pod.Status.Message = "Pod ephemeral local storage usage exceeds the total limit of containers 8Gi."
	return pod
}

// deletingLogs fails the stream and deletes the pod as it does, as a pod
// garbage-collected between the wait and the read would.
type deletingLogs struct {
	cs  *fake.Clientset
	err error
}

func (d deletingLogs) Stream(ctx context.Context, pod, _ string, _ bool) (io.ReadCloser, error) {
	_ = d.cs.CoreV1().Pods("patchy-agents").Delete(ctx, pod, metav1.DeleteOptions{})
	return nil, d.err
}

// TestResultLogsGone: a finished Job whose agent log can never be read again
// (evicted pod, container removed, pod gone, agent never ran, no pod at all,
// or unreadable past LogReadGrace since the Job finished) is reported as
// ErrLogsGone, so a collector settles it instead of retrying forever; a log
// that may still be readable stays an ordinary error the collector retries.
func TestResultLogsGone(t *testing.T) {
	const jobName = goneJob
	now := time.Now()
	removed := jobPodInState(jobName, corev1.PodFailed, corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 1},
	})
	neverRan := jobPodInState(jobName, corev1.PodFailed, corev1.ContainerState{
		Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"},
	})

	tests := []struct {
		name     string
		objects  []runtime.Object
		streamFn func(cs *fake.Clientset) logReader
		failList bool
		wantGone bool
		wantMsg  string
	}{
		{name: "evicted pod", wantGone: true, wantMsg: "evicted",
			objects: []runtime.Object{failedJob("PodFailurePolicy", now), evictedPod(jobName)}},
		{name: "terminated container removed", wantGone: true, wantMsg: "removed",
			objects: []runtime.Object{failedJob("BackoffLimitExceeded", now), removed}},
		{name: "pod deleted after the wait", wantGone: true, wantMsg: "pod is gone",
			objects:  []runtime.Object{failedJob("BackoffLimitExceeded", now), jobPod(jobName)},
			streamFn: func(cs *fake.Clientset) logReader { return deletingLogs{cs: cs, err: errTerminated} }},
		{name: "agent never started", wantGone: true, wantMsg: "never started",
			objects: []runtime.Object{failedJob("BackoffLimitExceeded", now), neverRan}},
		{name: "finished job without pods", wantGone: true, wantMsg: "no pods for job",
			objects: []runtime.Object{failedJob("BackoffLimitExceeded", now)}},
		{name: "unreadable past the grace", wantGone: true, wantMsg: "unreadable",
			objects: []runtime.Object{failedJob("BackoffLimitExceeded", now.Add(-LogReadGrace-time.Minute)),
				jobPod(jobName)}},
		{name: "transient error on a readable container",
			objects: []runtime.Object{failedJob("BackoffLimitExceeded", now), jobPod(jobName)}},
		{name: "transient error while the pod cannot be looked at", failList: true,
			objects: []runtime.Object{failedJob("BackoffLimitExceeded", now), evictedPod(jobName)}},
	}
	for _, tt := range tests {
		for _, lines := range []bool{false, true} {
			name := tt.name
			if lines {
				name += " (lines)"
			}
			t.Run(name, func(t *testing.T) {
				cs := fake.NewClientset(tt.objects...)
				c := New(cs, testConfig(), nil)
				c.logs = &fakeLogs{err: errTerminated}
				if tt.streamFn != nil {
					c.logs = tt.streamFn(cs)
				}
				if tt.failList {
					// The wait lists pods once; the classification lists them again.
					c.logs = listFailingLogs{cs: cs}
				}
				var err error
				if lines {
					err = c.ResultLines(context.Background(), jobName, func([]byte) error { return nil })
				} else {
					_, err = c.Result(context.Background(), jobName)
				}
				if err == nil {
					t.Fatal("Result succeeded, want an error")
				}
				if got := errors.Is(err, ErrLogsGone); got != tt.wantGone {
					t.Fatalf("Result = %v; errors.Is(ErrLogsGone) = %v, want %v", err, got, tt.wantGone)
				}
				if tt.wantMsg != "" && !strings.Contains(err.Error(), tt.wantMsg) {
					t.Errorf("Result = %v, want it to say %q", err, tt.wantMsg)
				}
			})
		}
	}
}

// listFailingLogs fails the stream and makes every later pod list fail, so
// the classification cannot see the pod.
type listFailingLogs struct{ cs *fake.Clientset }

func (l listFailingLogs) Stream(context.Context, string, string, bool) (io.ReadCloser, error) {
	failOn(l.cs, "list", "pods")
	return nil, errTerminated
}
