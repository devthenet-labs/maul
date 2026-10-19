// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package broker

import (
	"math/rand"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"testing/quick"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/attribution"
)

// propertySeed keeps the properties deterministic.
const propertySeed = 20261018

// genFacts is a random credFacts over the values that matter: every mode,
// verified or not, attributions valid, org, absent and malformed, records
// matching or not, Secrets present, absent, invalid, unreadable, and every
// health against the current or another version, for either provider's
// route, the record sometimes another provider's.
type genFacts credFacts

func (genFacts) Generate(r *rand.Rand, _ int) reflect.Value {
	pick := func(n int) int { return r.Intn(n) }
	modes := []UserCredentialMode{UserCredentialsOff, UserCredentialsAttribute, UserCredentialsEnforce}
	attributions := []string{"", "org", "github-id:42", "github-id:43", "github-id:042", "github:octocat", "ORG",
		"github-id:9223372036854775807"}
	providers := v1alpha1.ModelCredentialProviders
	f := credFacts{
		mode: modes[pick(len(modes))], provider: providers[pick(len(providers))],
		podVerified: pick(4) > 0, attribution: attributions[pick(len(attributions))], synced: pick(5) > 0,
	}
	owner := []string{"github-id:42", "github-id:43", "github-id:9223372036854775807"}[pick(3)]
	if pick(5) > 0 {
		kinds := []v1alpha1.ModelCredentialKind{v1alpha1.ModelCredentialAPIKey, v1alpha1.ModelCredentialOAuthToken,
			"password"}
		healths := []v1alpha1.ModelCredentialHealth{"", v1alpha1.ModelCredentialUnknown, v1alpha1.ModelCredentialValid,
			v1alpha1.ModelCredentialRejected}
		recProvider := f.provider
		if pick(4) == 0 {
			recProvider = providers[pick(len(providers))]
		}
		name := v1alpha1.ModelCredentialName(owner, []v1alpha1.ModelCredentialProvider{recProvider, f.provider}[pick(2)])
		if pick(6) == 0 {
			name = "0123456789abcdef-" + string(recProvider)
		}
		f.record = &v1alpha1.ModelCredential{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: v1alpha1.ModelCredentialSpec{Owner: owner, Provider: recProvider,
				Kind: kinds[pick(len(kinds))]},
			Status: v1alpha1.ModelCredentialStatus{Health: healths[pick(len(healths))],
				SecretVersion: []string{"", "1", "2"}[pick(3)]},
		}
	}
	switch pick(4) {
	case 0:
		f.secretUnavailable = true
	case 1:
	default:
		f.secret = &secretFacts{version: []string{"1", "2"}[pick(2)], valid: pick(4) > 0,
			credential: "sk-ant-api03-x", fingerprint: "fp"}
	}
	return reflect.ValueOf(genFacts(f))
}

// TestDecideProperties holds the decision's invariants over random facts:
//   - off and attribute always pay with the org credential and never refuse;
//   - every refusal is enforce's, carries a known reason and is a 403, or a
//     503 for a store not ready or a Secret not read;
//   - enforce never pays a person's run with the org credential, and pays a
//     person's run only with that person's own record, valid Secret and a
//     health that is not Rejected for the very version stored;
//   - a run attributed to the org (verified) is always paid by the org;
//   - a person's run on one provider's route is never paid by a record of
//     another provider, nor by a kind that provider does not take (an
//     OpenAI setup-token).
func TestDecideProperties(t *testing.T) {
	prop := func(g genFacts) bool {
		f := credFacts(g)
		d := decide(f)
		switch {
		case f.mode != UserCredentialsEnforce:
			return d.org && d.refusal == nil
		case d.refusal != nil:
			return !d.org && refusalHolds(d.refusal)
		case d.org:
			id, err := attribution.ParseValue(f.attribution)
			return f.podVerified && err == nil && id == 0
		}
		return personsOwn(f, d)
	}
	cfg := &quick.Config{MaxCount: 20000, Rand: rand.New(rand.NewSource(propertySeed))}
	if err := quick.Check(prop, cfg); err != nil {
		t.Fatal(err)
	}
}

// refusalHolds: a known reason, a 503 exactly for a store not ready or a
// Secret not read, a 403 otherwise.
func refusalHolds(r *rejection) bool {
	if refusalMessages[r.reason] == "" {
		return false
	}
	unavailable := r.reason == reasonCredentialsUnsynced || r.reason == reasonCredentialUnavailable
	if unavailable {
		return r.status == http.StatusServiceUnavailable
	}
	return r.status == http.StatusForbidden
}

// personsOwn: a person's run paid with a person's credential only when
// every link holds, and with exactly that record's credential.
func personsOwn(f credFacts, d credDecision) bool {
	id, err := attribution.ParseValue(f.attribution)
	if err != nil || id < 1 || !f.podVerified || !f.synced || f.record == nil || f.secretUnavailable ||
		f.secret == nil || !f.secret.valid {
		return false
	}
	owner := v1alpha1.ModelCredentialOwner(id)
	r := f.record
	return r.Spec.Owner == owner && r.Name == v1alpha1.ModelCredentialName(owner, f.provider) &&
		r.Spec.Provider == f.provider && v1alpha1.ModelCredentialKindAllowed(f.provider, r.Spec.Kind) && r.Status.HealthFor(f.secret.version) != v1alpha1.ModelCredentialRejected &&
		d.credential == f.secret.credential && d.kind == r.Spec.Kind && d.record == r.Name
}

// genSecret is a random credential Secret near the contract: right or
// wrong name, label, type, keys and value bytes.
type genSecret struct {
	owner  string
	secret *corev1.Secret
}

func (genSecret) Generate(r *rand.Rand, _ int) reflect.Value {
	owners := []string{"github-id:42", "github-id:43"}
	owner := owners[r.Intn(2)]
	other := owners[r.Intn(2)]
	s := credentialSecret(other, "")
	if r.Intn(5) == 0 {
		s.Name = "anthropic-key"
	}
	if r.Intn(5) == 0 {
		s.Labels[v1alpha1.LabelCredentialOwner] = v1alpha1.ModelCredentialOwnerHash(owners[r.Intn(2)])
	}
	if r.Intn(8) == 0 {
		s.Type = corev1.SecretTypeDockerConfigJson
	}
	if r.Intn(8) == 0 {
		s.Type = ""
	}
	n := r.Intn(48)
	if r.Intn(10) == 0 {
		n = v1alpha1.ModelCredentialSecretMaxBytes - 2 + r.Intn(5)
	}
	value := make([]byte, n)
	for i := range value {
		switch r.Intn(12) {
		case 0:
			value[i] = byte(r.Intn(256))
		case 1:
			value[i] = " \t\r\n"[r.Intn(4)]
		default:
			value[i] = byte(0x21 + r.Intn(0x7e-0x21+1))
		}
	}
	s.Data[v1alpha1.ModelCredentialSecretKey] = value
	if r.Intn(10) == 0 {
		s.Data["extra"] = []byte("x")
	}
	return reflect.ValueOf(genSecret{owner: owner, secret: s})
}

// TestValidateSecretProperties: an accepted Secret is exactly the owner's
// (derived name, the owner's hash label, Opaque, the one key), and the
// credential it yields is non-empty, bounded, printable ASCII with no
// space or control character (so it can never split a header), and is the
// stored value trimmed.
func TestValidateSecretProperties(t *testing.T) {
	prop := func(g genSecret) bool {
		cred, ok := validateSecret(g.owner, v1alpha1.ModelCredentialProviderAnthropic, g.secret)
		if !ok {
			return cred == ""
		}
		s := g.secret
		raw := s.Data[v1alpha1.ModelCredentialSecretKey]
		if s.Name != v1alpha1.ModelCredentialSecretName(g.owner, v1alpha1.ModelCredentialProviderAnthropic) ||
			s.Labels[v1alpha1.LabelCredentialOwner] != v1alpha1.ModelCredentialOwnerHash(g.owner) ||
			len(s.Data) != 1 || (s.Type != "" && s.Type != corev1.SecretTypeOpaque) ||
			len(raw) > v1alpha1.ModelCredentialSecretMaxBytes || cred == "" || cred != strings.TrimSpace(string(raw)) {
			return false
		}
		for i := 0; i < len(cred); i++ {
			if cred[i] < 0x21 || cred[i] > 0x7e {
				return false
			}
		}
		return true
	}
	cfg := &quick.Config{MaxCount: 20000, Rand: rand.New(rand.NewSource(propertySeed))}
	if err := quick.Check(prop, cfg); err != nil {
		t.Fatal(err)
	}
}
