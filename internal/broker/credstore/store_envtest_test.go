// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package credstore

import (
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/broker"
	"github.com/devthenet-labs/maul/internal/kube"
)

const (
	recordsNS = "maul"
	secretsNS = v1alpha1.DefaultCredentialsNamespace
)

// eventually polls cond for up to ten seconds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// storeFixture is a running Store over envtest, with an admin client and
// the names it notified.
type storeFixture struct {
	store    *Store
	admin    client.Client
	mu       sync.Mutex
	notified []string
}

func (f *storeFixture) seen(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Contains(f.notified, name)
}

func (f *storeFixture) reset() {
	f.mu.Lock()
	f.notified = nil
	f.mu.Unlock()
}

// drain waits until every event for the objects written so far has been
// delivered to the listener, then forgets what it recorded. Each informer
// delivers its events in order, so once a marker record and a marker Secret
// written last have both notified, nothing written before them can still be
// in flight: a late event for an earlier object cannot land after the reset
// and be mistaken for the effect of what the test does next.
func (f *storeFixture) drain(t *testing.T) {
	t.Helper()
	ctx := t.Context()
	owner := v1alpha1.ModelCredentialOwner(1)
	provider := v1alpha1.ModelCredentialProviderAnthropic
	name := v1alpha1.ModelCredentialName(owner, provider)
	rec := &v1alpha1.ModelCredential{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: recordsNS},
		Spec:       v1alpha1.ModelCredentialSpec{Owner: owner, Provider: provider, Kind: v1alpha1.ModelCredentialAPIKey},
	}
	if err := f.admin.Create(ctx, rec); err != nil {
		t.Fatal(err)
	}
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: v1alpha1.ModelCredentialSecretName(owner, provider), Namespace: secretsNS}}
	if err := f.admin.Create(ctx, sec); err != nil {
		t.Fatal(err)
	}
	// One notification from each informer.
	eventually(t, "the drain markers to notify", func() bool { return f.countOf(name) >= 2 })
	f.reset()
}

func (f *storeFixture) countOf(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, got := range f.notified {
		if got == name {
			n++
		}
	}
	return n
}

func (f *storeFixture) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.notified)
}

func startStore(t *testing.T) *storeFixture {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; skipping credstore envtest")
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{"../../../deploy/kustomize/base/crds"},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})
	admin, err := client.New(cfg, client.Options{Scheme: kube.Scheme()})
	if err != nil {
		t.Fatal(err)
	}
	for _, ns := range []string{recordsNS, secretsNS} {
		if err := admin.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			t.Fatal(err)
		}
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(cfg, cs, recordsNS, recordsNS); err == nil {
		t.Error("New with one namespace for both = nil, want an error")
	}
	store, err := New(cfg, cs, recordsNS, secretsNS)
	if err != nil {
		t.Fatal(err)
	}
	f := &storeFixture{store: store, admin: admin}
	store.Notify(func(name string) {
		f.mu.Lock()
		f.notified = append(f.notified, name)
		f.mu.Unlock()
	})
	go func() { _ = store.Start(t.Context()) }()
	eventually(t, "the store to sync", store.Synced)
	return f
}

// TestEnvtestStore drives the adapter against a real API server: the
// informers see records and Secret versions (never Secret data), changes
// notify the record's name, a live read returns the Secret, and a status
// patch is conditional on the record's resourceVersion.
func TestEnvtestStore(t *testing.T) {
	f := startStore(t)
	ctx := t.Context()
	owner := v1alpha1.ModelCredentialOwner(42)
	name := v1alpha1.ModelCredentialName(owner, v1alpha1.ModelCredentialProviderAnthropic)
	secretName := v1alpha1.ModelCredentialSecretName(owner, v1alpha1.ModelCredentialProviderAnthropic)
	if _, ok := f.store.Record(name); ok {
		t.Fatal("Record before it exists = found")
	}
	if s, err := f.store.ReadSecret(ctx, secretName); s != nil || err != nil {
		t.Fatalf("ReadSecret before it exists = %v, %v, want nil, nil", s, err)
	}
	rec := &v1alpha1.ModelCredential{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: recordsNS},
		Spec: v1alpha1.ModelCredentialSpec{Owner: owner, Provider: v1alpha1.ModelCredentialProviderAnthropic,
			Kind: v1alpha1.ModelCredentialAPIKey},
	}
	if err := f.admin.Create(ctx, rec); err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: secretsNS,
			Labels: map[string]string{v1alpha1.LabelCredentialOwner: v1alpha1.ModelCredentialOwnerHash(owner)}},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{v1alpha1.ModelCredentialSecretKey: []byte("sk-ant-api03-user42")},
	}
	if err := f.admin.Create(ctx, secret); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the record and the Secret's version", func() bool {
		_, okR := f.store.Record(name)
		v, okS := f.store.SecretVersion(secretName)
		return okR && okS && v == secret.ResourceVersion && f.seen(name)
	})
	got, err := f.store.ReadSecret(ctx, secretName)
	if err != nil || string(got.Data[v1alpha1.ModelCredentialSecretKey]) != "sk-ant-api03-user42" {
		t.Fatalf("ReadSecret = %v, %v", got, err)
	}
	checkPatch(t, f, rec, secret.ResourceVersion)
	checkOpenAI(t, f, owner)
	checkChanges(t, f, secret, name)
}

// checkOpenAI: the same person's OpenAI record and Secret are their own:
// seen by the informers under the openai names, and a change to the Secret
// notifies the openai record, not the anthropic one.
func checkOpenAI(t *testing.T, f *storeFixture, owner string) {
	t.Helper()
	ctx := t.Context()
	name := v1alpha1.ModelCredentialName(owner, v1alpha1.ModelCredentialProviderOpenAI)
	anthropic := v1alpha1.ModelCredentialName(owner, v1alpha1.ModelCredentialProviderAnthropic)
	secretName := v1alpha1.ModelCredentialSecretName(owner, v1alpha1.ModelCredentialProviderOpenAI)
	rec := &v1alpha1.ModelCredential{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: recordsNS},
		Spec: v1alpha1.ModelCredentialSpec{Owner: owner, Provider: v1alpha1.ModelCredentialProviderOpenAI,
			Kind: v1alpha1.ModelCredentialAPIKey},
	}
	if err := f.admin.Create(ctx, rec); err != nil {
		t.Fatal(err)
	}
	f.drain(t)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: secretsNS,
			Labels: map[string]string{v1alpha1.LabelCredentialOwner: v1alpha1.ModelCredentialOwnerHash(owner)}},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{v1alpha1.ModelCredentialSecretKey: []byte("sk-proj-user42")},
	}
	if err := f.admin.Create(ctx, secret); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the openai record and Secret version", func() bool {
		r, okR := f.store.Record(name)
		v, okS := f.store.SecretVersion(secretName)
		return okR && r.Spec.Provider == v1alpha1.ModelCredentialProviderOpenAI && okS &&
			v == secret.ResourceVersion && f.seen(name)
	})
	if f.seen(anthropic) {
		t.Error("the openai Secret notified the anthropic record")
	}
	got, err := f.store.ReadSecret(ctx, secretName)
	if err != nil || string(got.Data[v1alpha1.ModelCredentialSecretKey]) != "sk-proj-user42" {
		t.Fatalf("ReadSecret(openai) = %v, %v", got, err)
	}
}

// checkPatch: a stale resourceVersion is a conflict, the current one
// writes and returns the record's new version, and an empty one writes
// unconditionally.
func checkPatch(t *testing.T, f *storeFixture, rec *v1alpha1.ModelCredential, version string) {
	t.Helper()
	ctx := t.Context()
	valid := v1alpha1.ModelCredentialValid
	stale := broker.CredentialStatusPatch{Health: &valid}
	if _, err := f.store.PatchStatus(ctx, rec.Name, "1", stale); !apierrors.IsConflict(err) {
		t.Fatalf("PatchStatus at a stale version = %v, want a conflict", err)
	}
	cur := &v1alpha1.ModelCredential{}
	if err := f.admin.Get(ctx, client.ObjectKeyFromObject(rec), cur); err != nil {
		t.Fatal(err)
	}
	next, err := f.store.PatchStatus(ctx, rec.Name, cur.ResourceVersion,
		broker.CredentialStatusPatch{Health: &valid, SecretVersion: &version})
	if err != nil {
		t.Fatalf("PatchStatus at the current version = %v", err)
	}
	if next == "" || next == cur.ResourceVersion {
		t.Errorf("PatchStatus returned version %q, want the record's new one", next)
	}
	used := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	if _, err := f.store.PatchStatus(ctx, rec.Name, "", broker.CredentialStatusPatch{LastUsedAt: &used}); err != nil {
		t.Fatalf("unconditional PatchStatus = %v", err)
	}
	if err := f.admin.Get(ctx, client.ObjectKeyFromObject(rec), cur); err != nil {
		t.Fatal(err)
	}
	st := cur.Status
	if st.Health != valid || st.SecretVersion != version || st.LastUsedAt == nil || !st.LastUsedAt.Time.Equal(used) {
		t.Errorf("status = %+v", st)
	}
}

// checkChanges: replacing and deleting the Secret notify the record and
// reach the informer; a Secret not named for a record notifies nothing.
func checkChanges(t *testing.T, f *storeFixture, secret *corev1.Secret, name string) {
	t.Helper()
	ctx := t.Context()
	f.reset()
	secret.Data[v1alpha1.ModelCredentialSecretKey] = []byte("sk-ant-api03-replaced")
	if err := f.admin.Update(ctx, secret); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the replaced version", func() bool {
		v, ok := f.store.SecretVersion(secret.Name)
		return ok && v == secret.ResourceVersion && f.seen(name)
	})
	f.reset()
	if err := f.admin.Delete(ctx, secret); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the delete", func() bool {
		_, ok := f.store.SecretVersion(secret.Name)
		return !ok && f.seen(name)
	})
	f.reset()
	stray := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "stray", Namespace: secretsNS}}
	if err := f.admin.Create(ctx, stray); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the stray Secret", func() bool {
		_, ok := f.store.SecretVersion("stray")
		return ok
	})
	if n := f.count(); n != 0 {
		t.Errorf("notifications for a stray Secret = %d, want none", n)
	}
}
