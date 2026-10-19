// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1_test

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	patchyv1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

// testModelCredentialSchema: a ModelCredential is named for its provider
// behind a 16-hex-digit hash, its owner is a GitHub id in the attributed-to
// person grammar, owner and provider are immutable while kind may change
// (an openai record is an API key only), and its status holds a fingerprint and health only in their grammars.
func testModelCredentialSchema(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	owner := patchyv1.ModelCredentialOwner(42)
	cred := func(name string, mutate func(*patchyv1.ModelCredentialSpec)) *patchyv1.ModelCredential {
		m := &patchyv1.ModelCredential{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: patchyv1.ModelCredentialSpec{
				Owner: owner, OwnerLogin: "octocat",
				Provider: patchyv1.ModelCredentialProviderAnthropic, Kind: patchyv1.ModelCredentialAPIKey,
			},
		}
		if mutate != nil {
			mutate(&m.Spec)
		}
		return m
	}
	for _, tc := range []struct {
		name   string
		obj    *patchyv1.ModelCredential
		reason string
	}{
		{"no hash", cred("anthropic", nil), "name"},
		{"a short hash", cred("0123456789abcde-anthropic", nil), "name"},
		{"an upper-case hash", cred("0123456789ABCDEF-anthropic", nil), "name"},
		{"another provider's suffix", cred("0123456789abcdef-bedrock", nil), "name"},
		{"a provider outside the enum", cred("0123456789abcdef-bedrock", func(s *patchyv1.ModelCredentialSpec) {
			s.Provider = "bedrock"
		}), "provider"},
		{"a login for an owner", cred("0123456789abcdef-anthropic", func(s *patchyv1.ModelCredentialSpec) {
			s.Owner = "github:octocat"
		}), "owner"},
		{"the org as an owner", cred("0123456789abcdef-anthropic", func(s *patchyv1.ModelCredentialSpec) {
			s.Owner = "org"
		}), "owner"},
		{"a leading zero", cred("0123456789abcdef-anthropic", func(s *patchyv1.ModelCredentialSpec) {
			s.Owner = "github-id:042"
		}), "owner"},
		{"a kind outside the enum", cred("0123456789abcdef-anthropic", func(s *patchyv1.ModelCredentialSpec) {
			s.Kind = "password"
		}), "kind"},
		{"an openai name for an anthropic record", cred("0123456789abcdef-openai", nil), "name"},
		{"an openai setup-token", cred("0123456789abcdef-openai", func(s *patchyv1.ModelCredentialSpec) {
			s.Provider, s.Kind = patchyv1.ModelCredentialProviderOpenAI, patchyv1.ModelCredentialOAuthToken
		}), "kind"},
	} {
		if err := c.Create(ctx, tc.obj); err == nil {
			t.Errorf("Create(%s) = nil, want a %s rejection", tc.name, tc.reason)
		}
	}

	m := cred(patchyv1.ModelCredentialName(owner, patchyv1.ModelCredentialProviderAnthropic), nil)
	if err := c.Create(ctx, m); err != nil {
		t.Fatalf("Create(the derived name) = %v", err)
	}
	// The provider is immutable too (the name rule would also refuse the
	// move, so this is belt and braces).
	switched := m.DeepCopy()
	switched.Spec.Provider = patchyv1.ModelCredentialProviderOpenAI
	if err := c.Update(ctx, switched); err == nil {
		t.Error("Update of spec.provider = nil, want rejection (immutable)")
	}
	moved := m.DeepCopy()
	moved.Spec.Owner = patchyv1.ModelCredentialOwner(43)
	if err := c.Update(ctx, moved); err == nil {
		t.Error("Update of spec.owner = nil, want rejection (immutable)")
	}
	m.Spec.Kind = patchyv1.ModelCredentialOAuthToken
	m.Spec.OwnerLogin = "octocat-renamed"
	if err := c.Update(ctx, m); err != nil {
		t.Fatalf("Update of kind and login = %v, want nil", err)
	}

	// An OpenAI record: an API key under its own derived name, which may
	// never become a setup-token.
	o := cred(patchyv1.ModelCredentialName(owner, patchyv1.ModelCredentialProviderOpenAI),
		func(s *patchyv1.ModelCredentialSpec) { s.Provider = patchyv1.ModelCredentialProviderOpenAI })
	if err := c.Create(ctx, o); err != nil {
		t.Fatalf("Create(an openai record) = %v", err)
	}
	o.Spec.Kind = patchyv1.ModelCredentialOAuthToken
	if err := c.Update(ctx, o); err == nil {
		t.Error("Update of an openai record to a setup-token = nil, want rejection")
	}

	now := metav1.Now()
	m.Status = patchyv1.ModelCredentialStatus{
		Fingerprint: patchyv1.ModelCredentialFingerprint([]byte("sk-ant-api03-abcdefghijklmnop")),
		Health:      patchyv1.ModelCredentialValid, LastUsedAt: &now,
	}
	if err := c.Status().Update(ctx, m); err != nil {
		t.Fatalf("status update = %v", err)
	}
	for _, bad := range []patchyv1.ModelCredentialStatus{
		{Fingerprint: "sk-ant-api03-abcdefghijklmnop"},
		{Fingerprint: "mnop:2C975580F00E"},
		{Health: "Expired"},
		{SecretVersion: strings.Repeat("1", 65)},
	} {
		next := m.DeepCopy()
		next.Status = bad
		if err := c.Status().Update(ctx, next); err == nil {
			t.Errorf("status update %+v = nil, want rejection", bad)
		}
	}
}
