// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package podindex

import (
	"context"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/egressproxy"
)

// Agent pods carry these labels (internal/jobs).
const (
	labelApp = "app.kubernetes.io/name"
	appName  = "patchy-agent"
)

// Selector selects the agent pods the informer lists and watches.
const Selector = labelApp + "=" + appName

// ipIndex names the pod-IP index.
const ipIndex = "podIP"

// syncTimeout bounds the wait for the first list before an unsynced
// informer is logged; it keeps syncing after.
const syncTimeout = time.Minute

// keptLabels are the pod labels the audit line uses; every other label is
// dropped before the cache.
var keptLabels = []string{
	labelApp,
	v1alpha1.LabelFinding,
	v1alpha1.LabelOwner,
	v1alpha1.LabelRunKind,
	v1alpha1.LabelAttempt,
	v1alpha1.LabelHarness,
}

// Index attributes source IPs to agent pods.
type Index struct {
	factory  informers.SharedInformerFactory
	informer cache.SharedIndexInformer
}

var _ egressproxy.PodLookup = (*Index)(nil)

// New builds the index over the agent pods of namespace. Nothing is listed
// until Start.
func New(cs kubernetes.Interface, namespace string) (*Index, error) {
	factory := informers.NewSharedInformerFactoryWithOptions(cs, 0,
		informers.WithNamespace(namespace),
		informers.WithTweakListOptions(func(o *metav1.ListOptions) { o.LabelSelector = Selector }))
	inf := factory.Core().V1().Pods().Informer()
	if err := inf.SetTransform(Transform); err != nil {
		return nil, err
	}
	if err := inf.AddIndexers(cache.Indexers{ipIndex: indexByIP}); err != nil {
		return nil, err
	}
	return &Index{factory: factory, informer: inf}, nil
}

// Start runs the informer until ctx ends. It does not wait for the first
// list: an unsynced informer only leaves the audit fields empty, so it is
// logged, never a readiness failure.
func (x *Index) Start(ctx context.Context, log *slog.Logger) {
	x.factory.Start(ctx.Done())
	go func() {
		syncCtx, cancel := context.WithTimeout(ctx, syncTimeout)
		defer cancel()
		if !cache.WaitForCacheSync(syncCtx.Done(), x.informer.HasSynced) && ctx.Err() == nil {
			log.LogAttrs(ctx, slog.LevelWarn, "pod attribution not synced; audit lines carry no pod until it is",
				slog.Duration("waited", syncTimeout))
		}
	}()
}

// Synced reports whether the first list has landed.
func (x *Index) Synced() bool { return x.informer.HasSynced() }

// Lookup returns the newest Pending or Running agent pod with the IP.
func (x *Index) Lookup(ip string) (egressproxy.PodInfo, bool) {
	objs, err := x.informer.GetIndexer().ByIndex(ipIndex, ip)
	if err != nil {
		return egressproxy.PodInfo{}, false
	}
	var newest *corev1.Pod
	for _, o := range objs {
		p, ok := o.(*corev1.Pod)
		if !ok || !indexed(p) {
			continue
		}
		if newest == nil || newer(p, newest) {
			newest = p
		}
	}
	if newest == nil {
		return egressproxy.PodInfo{}, false
	}
	l := newest.Labels
	return egressproxy.PodInfo{
		Name:    newest.Name,
		Finding: l[v1alpha1.LabelFinding],
		Owner:   l[v1alpha1.LabelOwner],
		RunKind: l[v1alpha1.LabelRunKind],
		Attempt: l[v1alpha1.LabelAttempt],
		Harness: l[v1alpha1.LabelHarness],
		Repo:    newest.Annotations[v1alpha1.AnnotationRepo],
	}, true
}

// newer orders pods by creation time, then name, so the choice between two
// pods on one IP is deterministic.
func newer(a, b *corev1.Pod) bool {
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return b.CreationTimestamp.Before(&a.CreationTimestamp)
	}
	return a.Name > b.Name
}

// indexed reports whether a pod is attributable: an agent pod, Pending or
// Running, with an IP. The selector already lists only agent pods; the
// label is checked again so nothing else is ever attributed.
func indexed(p *corev1.Pod) bool {
	if p.Labels[labelApp] != appName || p.Status.PodIP == "" {
		return false
	}
	return p.Status.Phase == corev1.PodPending || p.Status.Phase == corev1.PodRunning
}

func indexByIP(obj any) ([]string, error) {
	p, ok := obj.(*corev1.Pod)
	if !ok || !indexed(p) {
		return nil, nil
	}
	return []string{p.Status.PodIP}, nil
}

// Transform reduces a pod to what attribution reads before it reaches the
// cache: name, namespace, UID, resource version, creation time, the kept
// labels, the repo annotation, the pod IP and the phase. The spec, every
// container and its env, and every other label and annotation are dropped.
func Transform(obj any) (any, error) {
	p, ok := obj.(*corev1.Pod)
	if !ok {
		return obj, nil
	}
	out := &corev1.Pod{
		TypeMeta: p.TypeMeta,
		ObjectMeta: metav1.ObjectMeta{
			Name:              p.Name,
			Namespace:         p.Namespace,
			UID:               p.UID,
			ResourceVersion:   p.ResourceVersion,
			CreationTimestamp: p.CreationTimestamp,
		},
		Status: corev1.PodStatus{PodIP: p.Status.PodIP, Phase: p.Status.Phase},
	}
	for _, k := range keptLabels {
		if v, ok := p.Labels[k]; ok {
			if out.Labels == nil {
				out.Labels = map[string]string{}
			}
			out.Labels[k] = v
		}
	}
	if v, ok := p.Annotations[v1alpha1.AnnotationRepo]; ok {
		out.Annotations = map[string]string{v1alpha1.AnnotationRepo: v}
	}
	return out, nil
}
