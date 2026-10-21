// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package credstore

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/web"
)

// Store writes credentials and their records.
type Store struct {
	// Client writes Secrets and records (writes are never cached).
	Client client.Client
	// Reader reads records uncached.
	Reader client.Reader
	// Namespace is the release namespace, where the records live.
	Namespace string
	// CredentialsNamespace is where the credential Secrets live.
	CredentialsNamespace string
}

var _ web.CredentialStore = (*Store)(nil)

// Record reads owner's record for provider, nil when there is none.
func (s *Store) Record(ctx context.Context, owner string, provider v1alpha1.ModelCredentialProvider) (
	*v1alpha1.ModelCredential, error) {
	var rec v1alpha1.ModelCredential
	key := types.NamespacedName{Namespace: s.Namespace, Name: v1alpha1.ModelCredentialName(owner, provider)}
	if err := s.Reader.Get(ctx, key, &rec); err != nil {
		if kerrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return &rec, nil
}

// Put stores w's credential, then its record, then the record's status.
func (s *Store) Put(ctx context.Context, w web.CredentialWrite) (*v1alpha1.ModelCredential, error) {
	version, err := s.putSecret(ctx, w)
	if err != nil {
		return nil, err
	}
	if err := s.putRecord(ctx, w); err != nil {
		return nil, err
	}
	fp := v1alpha1.ModelCredentialFingerprint([]byte(w.Credential))
	var out *v1alpha1.ModelCredential
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		rec, err := s.Record(ctx, w.Owner, w.Provider)
		if err != nil {
			return err
		}
		if rec == nil {
			return fmt.Errorf("model credential %s vanished while it was written",
				v1alpha1.ModelCredentialName(w.Owner, w.Provider))
		}
		if rec.Status.SecretVersion != version {
			// Not (yet) judged by the broker for this credential: whatever
			// it says now speaks for another.
			rec.Status.Health = v1alpha1.ModelCredentialUnknown
			rec.Status.SecretVersion = ""
		}
		rec.Status.Fingerprint = fp
		if err := s.Client.Status().Update(ctx, rec); err != nil {
			return err
		}
		out = rec
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("write the status of model credential %s: %w",
			v1alpha1.ModelCredentialName(w.Owner, w.Provider), err)
	}
	return out, nil
}

// putSecret creates the credential Secret, or replaces it unconditionally
// (no read: the status server may not read one), and returns the
// resourceVersion the API server gave it.
func (s *Store) putSecret(ctx context.Context, w web.CredentialWrite) (string, error) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      v1alpha1.ModelCredentialSecretName(w.Owner, w.Provider),
			Namespace: s.CredentialsNamespace,
			Labels: map[string]string{
				v1alpha1.LabelCredentialOwner: v1alpha1.ModelCredentialOwnerHash(w.Owner),
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{v1alpha1.ModelCredentialSecretKey: []byte(w.Credential)},
	}
	err := s.Client.Create(ctx, secret)
	if kerrors.IsAlreadyExists(err) {
		secret.ResourceVersion = ""
		err = s.Client.Update(ctx, secret)
	}
	if err != nil {
		return "", fmt.Errorf("write credential secret %s/%s: %w", s.CredentialsNamespace, secret.Name, err)
	}
	return secret.ResourceVersion, nil
}

// putRecord creates owner's record, or updates its kind and login.
func (s *Store) putRecord(ctx context.Context, w web.CredentialWrite) error {
	name := v1alpha1.ModelCredentialName(w.Owner, w.Provider)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		rec, err := s.Record(ctx, w.Owner, w.Provider)
		if err != nil {
			return err
		}
		if rec == nil {
			err := s.Client.Create(ctx, &v1alpha1.ModelCredential{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.Namespace},
				Spec: v1alpha1.ModelCredentialSpec{
					Owner: w.Owner, OwnerLogin: w.OwnerLogin, Provider: w.Provider, Kind: w.Kind,
				},
			})
			if kerrors.IsAlreadyExists(err) {
				return kerrors.NewConflict(v1alpha1.GroupVersion.WithResource("modelcredentials").GroupResource(),
					name, err)
			}
			return err
		}
		if rec.Spec.Kind == w.Kind && (w.OwnerLogin == "" || rec.Spec.OwnerLogin == w.OwnerLogin) {
			return nil
		}
		rec.Spec.Kind = w.Kind
		if w.OwnerLogin != "" {
			rec.Spec.OwnerLogin = w.OwnerLogin
		}
		return s.Client.Update(ctx, rec)
	})
}

// Delete removes owner's credential Secret and record for provider. The
// Secret goes first: once it is gone the broker refuses the credential
// whatever the record says.
func (s *Store) Delete(ctx context.Context, owner string, provider v1alpha1.ModelCredentialProvider) error {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: v1alpha1.ModelCredentialSecretName(owner, provider), Namespace: s.CredentialsNamespace,
	}}
	if err := s.Client.Delete(ctx, secret); err != nil && !kerrors.IsNotFound(err) {
		return fmt.Errorf("delete credential secret %s/%s: %w", s.CredentialsNamespace, secret.Name, err)
	}
	rec := &v1alpha1.ModelCredential{ObjectMeta: metav1.ObjectMeta{
		Name: v1alpha1.ModelCredentialName(owner, provider), Namespace: s.Namespace,
	}}
	if err := s.Client.Delete(ctx, rec); err != nil && !kerrors.IsNotFound(err) {
		return fmt.Errorf("delete model credential %s: %w", rec.Name, err)
	}
	return nil
}
