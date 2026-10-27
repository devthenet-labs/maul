// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
)

// KubeLogReader is the LogReader over the pods/log endpoint, the one read
// the chart grants preview-controller there (get, in its slots only). The
// API server applies the limit, and the read is bounded again here.
type KubeLogReader struct {
	Pods corev1client.PodsGetter
}

// ReadLog streams the container's log, never following it.
func (k KubeLogReader) ReadLog(ctx context.Context, namespace, pod, container string, opts LogOptions) ([]byte, error) {
	if opts.LimitBytes <= 0 {
		return nil, errNoLimit
	}
	o := &corev1.PodLogOptions{Container: container, LimitBytes: &opts.LimitBytes, TailLines: opts.TailLines}
	if opts.SinceTime != nil {
		t := metav1.NewTime(*opts.SinceTime)
		o.SinceTime = &t
	}
	stream, err := k.Pods.Pods(namespace).GetLogs(pod, o).Stream(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = stream.Close() }()
	return readBounded(stream, opts.LimitBytes)
}
