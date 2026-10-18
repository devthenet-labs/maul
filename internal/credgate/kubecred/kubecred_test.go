// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package kubecred

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/kube"
)

func TestReader(t *testing.T) {
	owner := v1alpha1.ModelCredentialOwner(42)
	name := v1alpha1.ModelCredentialName(owner, v1alpha1.ModelCredentialProviderAnthropic)
	stored := &v1alpha1.ModelCredential{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "patchy"},
		Spec: v1alpha1.ModelCredentialSpec{Owner: owner, Provider: v1alpha1.ModelCredentialProviderAnthropic,
			Kind: v1alpha1.ModelCredentialAPIKey},
		Status: v1alpha1.ModelCredentialStatus{Health: v1alpha1.ModelCredentialValid},
	}
	elsewhere := stored.DeepCopy()
	elsewhere.Namespace = "other"
	elsewhere.Spec.Owner = v1alpha1.ModelCredentialOwner(7)
	elsewhere.Name = v1alpha1.ModelCredentialName(elsewhere.Spec.Owner, v1alpha1.ModelCredentialProviderAnthropic)
	c := fake.NewClientBuilder().WithScheme(kube.Scheme()).WithObjects(stored, elsewhere).
		WithStatusSubresource(&v1alpha1.ModelCredential{}).Build()
	r := Reader{Client: c, Namespace: "patchy"}
	ctx := context.Background()

	got, err := r.ModelCredential(ctx, owner, v1alpha1.ModelCredentialProviderAnthropic)
	if err != nil || got == nil || got.Name != name {
		t.Fatalf("ModelCredential(42) = %+v, %v", got, err)
	}
	got, err = r.ModelCredential(ctx, elsewhere.Spec.Owner, v1alpha1.ModelCredentialProviderAnthropic)
	if err != nil || got != nil {
		t.Fatalf("a record in another namespace was found: %+v, %v", got, err)
	}

	failing := interceptor.NewClient(fake.NewClientBuilder().WithScheme(kube.Scheme()).Build().(client.WithWatch),
		interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object,
			...client.GetOption) error {
			return errors.New("api down")
		}})
	if _, err := (Reader{Client: failing, Namespace: "patchy"}).ModelCredential(ctx, owner,
		v1alpha1.ModelCredentialProviderAnthropic); err == nil {
		t.Fatal("a failed read was not an error")
	}
}
