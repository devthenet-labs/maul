// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package broker

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/devthenet-labs/maul/api/v1alpha1"
)

// errConflict is the fake store's optimistic-lock refusal.
var errConflict = errors.New("conflict: the object has been modified")

// fakeStore is an in-memory CredentialStore. informer holds the Secret
// versions the "informer" reports, which a test may let lag behind the
// live Secrets.
type fakeStore struct {
	mu       sync.Mutex
	synced   bool
	records  map[string]*v1alpha1.ModelCredential
	secrets  map[string]*corev1.Secret
	informer map[string]string
	// readErr fails every live Secret read.
	readErr error
	// onRead, when set, runs before each live read (to change a Secret
	// mid-probe).
	onRead  func(name string, n int)
	reads   int
	patches []fakePatch
	notify  []func(string)
	rv      int
}

type fakePatch struct {
	name, version string
	patch         CredentialStatusPatch
}

func newFakeStore() *fakeStore {
	return &fakeStore{synced: true, records: map[string]*v1alpha1.ModelCredential{},
		secrets: map[string]*corev1.Secret{}, informer: map[string]string{}, rv: 100}
}

func (f *fakeStore) nextRV() string {
	f.rv++
	return strconv.Itoa(f.rv)
}

// put stores (or replaces) owner's anthropic record and Secret, both seen
// by the informer at once, and returns the Secret's version.
func (f *fakeStore) put(id int64, kind v1alpha1.ModelCredentialKind, credential string) string {
	return f.putFor(id, v1alpha1.ModelCredentialProviderAnthropic, kind, credential)
}

// putFor is put for provider's record and Secret.
func (f *fakeStore) putFor(id int64, provider v1alpha1.ModelCredentialProvider, kind v1alpha1.ModelCredentialKind,
	credential string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	owner := v1alpha1.ModelCredentialOwner(id)
	name := v1alpha1.ModelCredentialName(owner, provider)
	rec := f.records[name]
	if rec == nil {
		rec = &v1alpha1.ModelCredential{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "maul"}}
	}
	rec.Spec = v1alpha1.ModelCredentialSpec{Owner: owner, Provider: provider, Kind: kind}
	rec.ResourceVersion = f.nextRV()
	f.records[name] = rec
	s := credentialSecretFor(owner, provider, credential)
	s.ResourceVersion = f.nextRV()
	f.secrets[s.Name] = s
	f.informer[s.Name] = s.ResourceVersion
	return s.ResourceVersion
}

// credentialSecret is owner's well-formed anthropic credential Secret.
func credentialSecret(owner, credential string) *corev1.Secret {
	return credentialSecretFor(owner, v1alpha1.ModelCredentialProviderAnthropic, credential)
}

// credentialSecretFor is owner's well-formed credential Secret for provider.
func credentialSecretFor(owner string, provider v1alpha1.ModelCredentialProvider, credential string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      v1alpha1.ModelCredentialSecretName(owner, provider),
			Namespace: v1alpha1.DefaultCredentialsNamespace,
			Labels:    map[string]string{v1alpha1.LabelCredentialOwner: v1alpha1.ModelCredentialOwnerHash(owner)},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{v1alpha1.ModelCredentialSecretKey: []byte(credential)},
	}
}

// update applies mutate to a stored record under the lock.
func (f *fakeStore) update(name string, mutate func(*v1alpha1.ModelCredential)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	mutate(f.records[name])
	f.records[name].ResourceVersion = f.nextRV()
}

func (f *fakeStore) record(name string) v1alpha1.ModelCredential {
	f.mu.Lock()
	defer f.mu.Unlock()
	return *f.records[name].DeepCopy()
}

func (f *fakeStore) Synced() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.synced
}

func (f *fakeStore) Record(name string) (*v1alpha1.ModelCredential, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.records[name]
	if !ok || !f.synced { // as credstore: nothing is read before the sync
		return nil, false
	}
	return r.DeepCopy(), true
}

func (f *fakeStore) SecretVersion(name string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.synced {
		return "", false
	}
	v, ok := f.informer[name]
	return v, ok
}

func (f *fakeStore) ReadSecret(_ context.Context, name string) (*corev1.Secret, error) {
	f.mu.Lock()
	f.reads++
	n, hook := f.reads, f.onRead
	f.mu.Unlock()
	if hook != nil {
		hook(name, n)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readErr != nil {
		return nil, f.readErr
	}
	s, ok := f.secrets[name]
	if !ok {
		return nil, nil
	}
	return s.DeepCopy(), nil
}

func (f *fakeStore) PatchStatus(_ context.Context, name, version string, p CredentialStatusPatch) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.records[name]
	if !ok {
		return "", errors.New("not found")
	}
	if version != "" && version != r.ResourceVersion {
		return "", errConflict
	}
	f.patches = append(f.patches, fakePatch{name: name, version: version, patch: p})
	if p.Health != nil {
		r.Status.Health = *p.Health
	}
	if p.SecretVersion != nil {
		r.Status.SecretVersion = *p.SecretVersion
	}
	if p.LastUsedAt != nil {
		t := metav1.NewTime(*p.LastUsedAt)
		r.Status.LastUsedAt = &t
	}
	if p.LastRejectedAt != nil {
		t := metav1.NewTime(*p.LastRejectedAt)
		r.Status.LastRejectedAt = &t
	}
	r.ResourceVersion = f.nextRV()
	return r.ResourceVersion, nil
}

func (f *fakeStore) Notify(fn func(string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notify = append(f.notify, fn)
}

// fire calls the registered handlers for a record, as an informer event.
func (f *fakeStore) fire(name string) {
	f.mu.Lock()
	fns := append([]func(string){}, f.notify...)
	f.mu.Unlock()
	for _, fn := range fns {
		fn(name)
	}
}

func (f *fakeStore) patchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.patches)
}

// fakeClock is a settable clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)} }

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}
