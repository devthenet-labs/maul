// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1

import (
	"bytes"
	"math/rand"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"testing/quick"

	"k8s.io/apimachinery/pkg/util/validation"
)

var (
	modelCredentialNameRE   = regexp.MustCompile(ModelCredentialNamePattern)
	modelCredentialSecretRE = regexp.MustCompile(ModelCredentialSecretNamePattern)
	modelCredentialOwnerRE  = regexp.MustCompile(`^github-id:[1-9][0-9]{0,18}$`)
	fingerprintRE           = regexp.MustCompile(`^[A-Za-z0-9_*-]{4}:[0-9a-f]{12}$`)
)

// TestModelCredentialNamesExample pins the derivation to one worked value,
// so a change to the hash input or its encoding cannot pass silently: the
// broker and the status server find each other's records only through it.
func TestModelCredentialNamesExample(t *testing.T) {
	owner := ModelCredentialOwner(42)
	if owner != "github-id:42" {
		t.Fatalf("ModelCredentialOwner(42) = %q", owner)
	}
	// sha256("github-id:42") = bf39649e29052e0e...
	if got := ModelCredentialOwnerHash(owner); got != "bf39649e29052e0e" {
		t.Errorf("ModelCredentialOwnerHash = %q", got)
	}
	if got := ModelCredentialName(owner, ModelCredentialProviderAnthropic); got != "bf39649e29052e0e-anthropic" {
		t.Errorf("ModelCredentialName = %q", got)
	}
	if got := ModelCredentialSecretName(owner, ModelCredentialProviderAnthropic); got !=
		"cred-bf39649e29052e0e-anthropic" {
		t.Errorf("ModelCredentialSecretName = %q", got)
	}
}

func TestModelCredentialFingerprintExamples(t *testing.T) {
	for _, tc := range []struct {
		credential, want string
	}{
		{"sk-ant-api03-abcdefghijklmnop", "mnop:2c975580f00e"},
		// Too short to show any of it.
		{"short", "****:f9b0078b5df5"},
	} {
		if got := ModelCredentialFingerprint([]byte(tc.credential)); got != tc.want {
			t.Errorf("ModelCredentialFingerprint(%q) = %q, want %q", tc.credential, got, tc.want)
		}
	}
	// A character outside the fingerprint alphabet is masked, never shown.
	if got := ModelCredentialFingerprint([]byte("0123456789abcdef\n+/=")); !strings.HasPrefix(got, "****:") {
		t.Errorf("fingerprint of a tail outside the alphabet = %q, want it masked", got)
	}
}

// githubID generates a valid GitHub numeric id: 1 to the largest int64,
// spread over every digit count.
type githubID int64

func (githubID) Generate(r *rand.Rand, _ int) reflect.Value {
	digits := 1 + r.Intn(19)
	var n int64
	for i := 0; i < digits; i++ {
		n = n*10 + int64(r.Intn(10))
		if n < 0 {
			n = 1<<63 - 1
			break
		}
	}
	if n < 1 {
		n = 1 + int64(r.Intn(1000))
	}
	return reflect.ValueOf(githubID(n))
}

// TestModelCredentialNamesProperty: for every GitHub id, the owner value is
// in the attributed-to grammar (and the CRD's owner pattern), the record and
// Secret names are in the grammars the chart's policy holds them to and are
// valid object names, the Secret's is the record's behind "cred-", and the
// owner hash is a valid label value equal to both names' hash. Seeded, so
// the gate is deterministic.
func TestModelCredentialNamesProperty(t *testing.T) {
	cfg := &quick.Config{MaxCount: 5000, Rand: rand.New(rand.NewSource(20261018))}
	check := func(id githubID) bool {
		owner := ModelCredentialOwner(int64(id))
		hash := ModelCredentialOwnerHash(owner)
		name := ModelCredentialName(owner, ModelCredentialProviderAnthropic)
		secret := ModelCredentialSecretName(owner, ModelCredentialProviderAnthropic)
		return modelCredentialOwnerRE.MatchString(owner) && len(owner) <= 29 &&
			owner == ModelCredentialOwnerPrefix+strconv.FormatInt(int64(id), 10) &&
			modelCredentialNameRE.MatchString(name) && modelCredentialSecretRE.MatchString(secret) &&
			len(validation.IsDNS1123Label(name)) == 0 && len(validation.IsDNS1123Subdomain(secret)) == 0 &&
			secret == "cred-"+name && strings.HasPrefix(name, hash+"-") &&
			len(validation.IsValidLabelValue(hash)) == 0 &&
			ModelCredentialName(owner, ModelCredentialProviderAnthropic) == name
	}
	if err := quick.Check(check, cfg); err != nil {
		t.Error(err)
	}
}

// TestModelCredentialNamesDistinctProperty: two different people never
// share a record name (a 64-bit hash: a collision here would be a broken
// derivation, not bad luck). Seeded.
func TestModelCredentialNamesDistinctProperty(t *testing.T) {
	cfg := &quick.Config{MaxCount: 5000, Rand: rand.New(rand.NewSource(20261018))}
	check := func(a, b githubID) bool {
		if a == b {
			return true
		}
		return ModelCredentialName(ModelCredentialOwner(int64(a)), ModelCredentialProviderAnthropic) !=
			ModelCredentialName(ModelCredentialOwner(int64(b)), ModelCredentialProviderAnthropic)
	}
	if err := quick.Check(check, cfg); err != nil {
		t.Error(err)
	}
}

// TestModelCredentialFingerprintProperty: every credential's fingerprint is
// in the status pattern, depends on nothing but the credential, and shows
// at most its last four characters: never any earlier byte, and none at all
// of a credential shorter than 16 bytes. Seeded.
func TestModelCredentialFingerprintProperty(t *testing.T) {
	cfg := &quick.Config{MaxCount: 5000, Rand: rand.New(rand.NewSource(20261018))}
	check := func(credential []byte) bool {
		fp := ModelCredentialFingerprint(credential)
		if !fingerprintRE.MatchString(fp) || fp != ModelCredentialFingerprint(bytes.Clone(credential)) {
			return false
		}
		tail := fp[:4]
		if len(credential) < fingerprintMinBytes {
			return tail == "****"
		}
		last := credential[len(credential)-4:]
		for i := range 4 {
			if tail[i] != '*' && tail[i] != last[i] {
				return false
			}
		}
		return true
	}
	if err := quick.Check(check, cfg); err != nil {
		t.Error(err)
	}
}

// TestHealthForExamples: a health speaks only for the Secret version it was
// judged against.
func TestHealthForExamples(t *testing.T) {
	judged := ModelCredentialStatus{Health: ModelCredentialRejected, SecretVersion: "41"}
	for _, tc := range []struct {
		status  ModelCredentialStatus
		current string
		want    ModelCredentialHealth
	}{
		{judged, "41", ModelCredentialRejected},
		{judged, "42", ModelCredentialUnknown},
		{judged, "", ModelCredentialUnknown},
		{ModelCredentialStatus{Health: ModelCredentialValid}, "41", ModelCredentialUnknown},
		{ModelCredentialStatus{Health: ModelCredentialValid, SecretVersion: "7"}, "7", ModelCredentialValid},
		{ModelCredentialStatus{SecretVersion: "7"}, "7", ModelCredentialUnknown},
	} {
		if got := tc.status.HealthFor(tc.current); got != tc.want {
			t.Errorf("%+v.HealthFor(%q) = %s, want %s", tc.status, tc.current, got, tc.want)
		}
	}
}

// TestHealthForProperty: a known health is only ever reported for the very
// version it was judged against, and a matching version reports the
// recorded health whenever it is a known one.
func TestHealthForProperty(t *testing.T) {
	healths := []ModelCredentialHealth{"", ModelCredentialUnknown, ModelCredentialValid, ModelCredentialRejected}
	versions := []string{"", "1", "2", "41"}
	prop := func(h, judged, current uint8) bool {
		s := ModelCredentialStatus{Health: healths[int(h)%len(healths)], SecretVersion: versions[int(judged)%len(versions)]}
		cur := versions[int(current)%len(versions)]
		got := s.HealthFor(cur)
		if got != ModelCredentialUnknown && (cur == "" || cur != s.SecretVersion || got != s.Health) {
			return false
		}
		if cur != "" && cur == s.SecretVersion && (s.Health == ModelCredentialValid || s.Health == ModelCredentialRejected) {
			return got == s.Health
		}
		return got == ModelCredentialUnknown
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 2000, Rand: rand.New(rand.NewSource(7))}); err != nil {
		t.Fatal(err)
	}
}
