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

	"github.com/bitwise-media-group/patchy/api/v1alpha1"
)

// TestStoredValueIsWhatTheBrokerSends: whatever the status server's Account
// page accepts (v1alpha1.ModelCredentialValue) and stores, the broker's own
// Secret validation accepts, and sends exactly that value: the two
// grammars cannot drift so that a credential a person stored is refused.
func TestStoredValueIsWhatTheBrokerSends(t *testing.T) {
	owner := v1alpha1.ModelCredentialOwner(42)
	provider := v1alpha1.ModelCredentialProviderAnthropic
	cfg := &quick.Config{MaxCount: 5000, Rand: rand.New(rand.NewSource(propertySeed))}
	cfg.Values = func(args []reflect.Value, r *rand.Rand) {
		const alphabet = "abcXYZ019-_.~:/+= \t\n\x00é"
		b := []byte([]string{"", "sk-ant-api03-", "sk-ant-oat01-", " "}[r.Intn(4)])
		for range r.Intn(200) {
			b = append(b, alphabet[r.Intn(len(alphabet))])
		}
		kinds := []v1alpha1.ModelCredentialKind{v1alpha1.ModelCredentialAPIKey, v1alpha1.ModelCredentialOAuthToken}
		args[0], args[1] = reflect.ValueOf(string(b)), reflect.ValueOf(kinds[r.Intn(2)])
	}
	prop := func(raw string, kind v1alpha1.ModelCredentialKind) bool {
		value, ok := v1alpha1.ModelCredentialValue(raw, kind)
		if !ok {
			return true
		}
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.ModelCredentialSecretName(owner, provider),
				Labels: map[string]string{v1alpha1.LabelCredentialOwner: v1alpha1.ModelCredentialOwnerHash(owner)}},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{v1alpha1.ModelCredentialSecretKey: []byte(value)},
		}
		sent, ok := validateSecret(owner, provider, secret)
		return ok && sent == value
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}
