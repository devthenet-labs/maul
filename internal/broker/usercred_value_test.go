// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package broker

import (
	"math/rand"
	"reflect"
	"testing"
	"testing/quick"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/devthenet-labs/maul/api/v1alpha1"
)

// TestStoredValueIsWhatTheBrokerSends: whatever the status server's Account
// page accepts (v1alpha1.ModelCredentialValue) and stores, for every
// provider, the broker's own Secret validation accepts, and sends exactly
// that value: the two grammars cannot drift so that a credential a person
// stored is refused. The provider is drawn from
// v1alpha1.ModelCredentialProviders, the prefixes from every provider's, and
// the property must accept values of each provider so it is never vacuous.
func TestStoredValueIsWhatTheBrokerSends(t *testing.T) {
	owner := v1alpha1.ModelCredentialOwner(42)
	providers := v1alpha1.ModelCredentialProviders
	accepted := map[v1alpha1.ModelCredentialProvider]int{}
	cfg := &quick.Config{MaxCount: 5000, Rand: rand.New(rand.NewSource(propertySeed))}
	cfg.Values = func(args []reflect.Value, r *rand.Rand) {
		const alphabet = "abcXYZ019-_.~:/+= \t\n\x00é"
		prefixes := []string{"", "sk-ant-api03-", "sk-ant-oat01-", "sk-proj-", "sk-", " "}
		b := []byte(prefixes[r.Intn(len(prefixes))])
		for range r.Intn(200) {
			b = append(b, alphabet[r.Intn(len(alphabet))])
		}
		kinds := []v1alpha1.ModelCredentialKind{v1alpha1.ModelCredentialAPIKey, v1alpha1.ModelCredentialOAuthToken}
		args[0], args[1] = reflect.ValueOf(string(b)), reflect.ValueOf(kinds[r.Intn(2)])
		args[2] = reflect.ValueOf(providers[r.Intn(len(providers))])
	}
	prop := func(raw string, kind v1alpha1.ModelCredentialKind, provider v1alpha1.ModelCredentialProvider) bool {
		value, ok := v1alpha1.ModelCredentialValue(raw, provider, kind)
		if !ok {
			return true
		}
		accepted[provider]++
		return storedValueSent(owner, provider, value)
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
	for _, p := range providers {
		if accepted[p] == 0 {
			t.Errorf("the property accepted no %s value: it checks nothing for that provider", p)
		}
	}
}

// TestStoredOpenAIValueIsWhatTheBrokerSends pins an OpenAI project key, the
// case the property once never drew, as a plain example.
func TestStoredOpenAIValueIsWhatTheBrokerSends(t *testing.T) {
	owner := v1alpha1.ModelCredentialOwner(42)
	provider := v1alpha1.ModelCredentialProviderOpenAI
	value, ok := v1alpha1.ModelCredentialValue(" sk-proj-abc_XYZ-019\n", provider, v1alpha1.ModelCredentialAPIKey)
	if !ok || value != "sk-proj-abc_XYZ-019" {
		t.Fatalf("ModelCredentialValue = %q, %v", value, ok)
	}
	if !storedValueSent(owner, provider, value) {
		t.Errorf("the broker does not send the stored OpenAI key %q", value)
	}
}

// storedValueSent is whether the broker's validateSecret accepts the Secret
// the status server stores value in for owner and provider, and sends value.
func storedValueSent(owner string, provider v1alpha1.ModelCredentialProvider, value string) bool {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.ModelCredentialSecretName(owner, provider),
			Labels: map[string]string{v1alpha1.LabelCredentialOwner: v1alpha1.ModelCredentialOwnerHash(owner)}},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{v1alpha1.ModelCredentialSecretKey: []byte(value)},
	}
	sent, ok := validateSecret(owner, provider, secret)
	return ok && sent == value
}
