// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package kubecred

import (
	"context"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/bitwise-media-group/patchy/api/v1alpha1"
)

// Reader reads ModelCredentials from one namespace.
type Reader struct {
	Client    client.Reader
	Namespace string
}

// ModelCredential reads owner's record for provider, nil when there is none.
func (r Reader) ModelCredential(ctx context.Context, owner string, provider v1alpha1.ModelCredentialProvider) (
	*v1alpha1.ModelCredential, error) {
	var c v1alpha1.ModelCredential
	key := types.NamespacedName{Namespace: r.Namespace, Name: v1alpha1.ModelCredentialName(owner, provider)}
	if err := r.Client.Get(ctx, key, &c); err != nil {
		if kerrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return &c, nil
}
