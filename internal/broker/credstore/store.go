// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package credstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/broker"
	"github.com/bitwise-media-group/patchy/internal/kube"
)

// Store is the broker.CredentialStore over the API server.
type Store struct {
	cfg *rest.Config
	// cache and writer are built by Start, so New contacts nothing and a
	// misconfigured broker fails on its own config first.
	cache     cache.Cache
	cs        kubernetes.Interface
	writer    client.Client
	records   string
	secrets   string
	synced    atomic.Bool
	mu        sync.Mutex
	listeners []func(string)
}

// New builds a Store reading ModelCredentials in recordsNS and credential
// Secrets in secretsNS. Nothing is contacted until Start.
func New(cfg *rest.Config, cs kubernetes.Interface, recordsNS, secretsNS string) (*Store, error) {
	if recordsNS == "" || secretsNS == "" || recordsNS == secretsNS {
		return nil, fmt.Errorf("credstore: the records namespace (%q) and the credentials namespace (%q) "+
			"must both be set and differ", recordsNS, secretsNS)
	}
	return &Store{cfg: cfg, cs: cs, records: recordsNS, secrets: secretsNS}, nil
}

// build makes the cache and the status writer.
func (s *Store) build() error {
	scheme := kube.Scheme()
	c, err := cache.New(s.cfg, cache.Options{
		Scheme: scheme,
		ByObject: map[client.Object]cache.ByObject{
			&v1alpha1.ModelCredential{}: {Namespaces: map[string]cache.Config{s.records: {}}},
			secretMeta():                {Namespaces: map[string]cache.Config{s.secrets: {}}},
		},
		ReaderFailOnMissingInformer: true,
	})
	if err != nil {
		return fmt.Errorf("credstore: cache: %w", err)
	}
	w, err := client.New(s.cfg, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("credstore: client: %w", err)
	}
	s.cache, s.writer = c, w
	return nil
}

// secretMeta is the metadata-only form of a Secret the informer lists and
// watches: names, labels and resourceVersions, never data, so no stored
// credential is cached; data is read live only for a request or a probe.
func secretMeta() *metav1.PartialObjectMetadata {
	m := &metav1.PartialObjectMetadata{}
	m.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Secret"))
	return m
}

// Start registers the informers and their change notifications, runs the
// cache until ctx ends, and marks the store synced once both have listed.
// It returns when ctx ends or the cache fails.
func (s *Store) Start(ctx context.Context) error {
	if err := s.build(); err != nil {
		return err
	}
	handler := toolscache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { s.changed(obj) },
		UpdateFunc: func(_, obj any) { s.changed(obj) },
		DeleteFunc: func(obj any) { s.changed(obj) },
	}
	for _, obj := range []client.Object{&v1alpha1.ModelCredential{}, secretMeta()} {
		inf, err := s.cache.GetInformer(ctx, obj)
		if err != nil {
			return fmt.Errorf("credstore: informer for %T: %w", obj, err)
		}
		if _, err := inf.AddEventHandler(handler); err != nil {
			return fmt.Errorf("credstore: event handler for %T: %w", obj, err)
		}
	}
	errc := make(chan error, 1)
	go func() { errc <- s.cache.Start(ctx) }()
	if s.cache.WaitForCacheSync(ctx) {
		s.synced.Store(true)
	}
	return <-errc
}

// changed notifies the listeners of the record an event is about: the
// record itself, or the record a credential Secret belongs to.
func (s *Store) changed(obj any) {
	key, err := toolscache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		return
	}
	_, name, err := toolscache.SplitMetaNamespaceKey(key)
	if err != nil {
		return
	}
	if t, ok := obj.(toolscache.DeletedFinalStateUnknown); ok {
		obj = t.Obj
	}
	if _, ok := obj.(*v1alpha1.ModelCredential); !ok {
		var found bool
		if name, found = strings.CutPrefix(name, "cred-"); !found {
			return
		}
	}
	s.mu.Lock()
	fns := append([]func(string){}, s.listeners...)
	s.mu.Unlock()
	for _, fn := range fns {
		fn(name)
	}
}

// Synced implements broker.CredentialStore.
func (s *Store) Synced() bool { return s.synced.Load() }

// Record implements broker.CredentialStore.
func (s *Store) Record(name string) (*v1alpha1.ModelCredential, bool) {
	if !s.Synced() { // also: Start has built the cache
		return nil, false
	}
	rec := &v1alpha1.ModelCredential{}
	if err := s.cache.Get(context.Background(), types.NamespacedName{Namespace: s.records, Name: name}, rec); err != nil {
		return nil, false
	}
	return rec, true
}

// SecretVersion implements broker.CredentialStore.
func (s *Store) SecretVersion(name string) (string, bool) {
	if !s.Synced() {
		return "", false
	}
	m := secretMeta()
	if err := s.cache.Get(context.Background(), types.NamespacedName{Namespace: s.secrets, Name: name}, m); err != nil {
		return "", false
	}
	return m.ResourceVersion, true
}

// ReadSecret implements broker.CredentialStore: a live get, in the
// credentials namespace only.
func (s *Store) ReadSecret(ctx context.Context, name string) (*corev1.Secret, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	sec, err := s.cs.CoreV1().Secrets(s.secrets).Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return nil, nil
	case err != nil:
		return nil, err
	}
	return sec, nil
}

// statusPatch is the JSON merge patch one PatchStatus sends: the record's
// resourceVersion as a precondition when set, and only the status fields
// being written.
type statusPatch struct {
	Metadata *patchMeta   `json:"metadata,omitempty"`
	Status   statusFields `json:"status"`
}

type patchMeta struct {
	ResourceVersion string `json:"resourceVersion"`
}

type statusFields struct {
	Health         *v1alpha1.ModelCredentialHealth `json:"health,omitempty"`
	SecretVersion  *string                         `json:"secretVersion,omitempty"`
	LastUsedAt     *metav1.Time                    `json:"lastUsedAt,omitempty"`
	LastRejectedAt *metav1.Time                    `json:"lastRejectedAt,omitempty"`
}

// PatchStatus implements broker.CredentialStore: a merge patch of the
// status subresource, conditional on resourceVersion when it is set (the
// API server answers a stale one with a conflict).
func (s *Store) PatchStatus(ctx context.Context, name, resourceVersion string,
	p broker.CredentialStatusPatch,
) (string, error) {
	if !s.Synced() { // the writer is built by Start, before the sync
		return "", errNotStarted
	}
	body := statusPatch{Status: statusFields{Health: p.Health, SecretVersion: p.SecretVersion}}
	if resourceVersion != "" {
		body.Metadata = &patchMeta{ResourceVersion: resourceVersion}
	}
	if p.LastUsedAt != nil {
		t := metav1.NewTime(*p.LastUsedAt)
		body.Status.LastUsedAt = &t
	}
	if p.LastRejectedAt != nil {
		t := metav1.NewTime(*p.LastRejectedAt)
		body.Status.LastRejectedAt = &t
	}
	data, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rec := &v1alpha1.ModelCredential{ObjectMeta: metav1.ObjectMeta{Namespace: s.records, Name: name}}
	if err := s.writer.Status().Patch(ctx, rec, client.RawPatch(types.MergePatchType, data),
		client.FieldOwner("egress-broker")); err != nil {
		return "", err
	}
	return rec.ResourceVersion, nil
}

// Notify implements broker.CredentialStore.
func (s *Store) Notify(fn func(string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listeners = append(s.listeners, fn)
}

var _ broker.CredentialStore = (*Store)(nil)

// errNotStarted is a write before the store has synced.
var errNotStarted = errors.New("credstore: not synced yet")
