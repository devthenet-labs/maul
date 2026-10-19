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
	if got := ModelCredentialSecretName(owner, ModelCredentialProviderOpenAI); got != "cred-bf39649e29052e0e-openai" {
		t.Errorf("ModelCredentialSecretName(openai) = %q", got)
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
	check := func(id githubID, p uint8) bool {
		provider := ModelCredentialProviders[int(p)%len(ModelCredentialProviders)]
		owner := ModelCredentialOwner(int64(id))
		hash := ModelCredentialOwnerHash(owner)
		name := ModelCredentialName(owner, provider)
		secret := ModelCredentialSecretName(owner, provider)
		return strings.HasSuffix(name, "-"+string(provider)) && modelCredentialOwnerRE.MatchString(owner) && len(owner) <= 29 &&
			owner == ModelCredentialOwnerPrefix+strconv.FormatInt(int64(id), 10) &&
			modelCredentialNameRE.MatchString(name) && modelCredentialSecretRE.MatchString(secret) &&
			len(validation.IsDNS1123Label(name)) == 0 && len(validation.IsDNS1123Subdomain(secret)) == 0 &&
			secret == "cred-"+name && strings.HasPrefix(name, hash+"-") &&
			len(validation.IsValidLabelValue(hash)) == 0 &&
			ModelCredentialName(owner, provider) == name
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
		oa, ob := ModelCredentialOwner(int64(a)), ModelCredentialOwner(int64(b))
		// One person's records for two providers never share a name either.
		if ModelCredentialName(oa, ModelCredentialProviderAnthropic) ==
			ModelCredentialName(oa, ModelCredentialProviderOpenAI) {
			return false
		}
		if a == b {
			return true
		}
		return ModelCredentialName(oa, ModelCredentialProviderAnthropic) !=
			ModelCredentialName(ob, ModelCredentialProviderAnthropic) &&
			ModelCredentialName(oa, ModelCredentialProviderOpenAI) != ModelCredentialName(ob, ModelCredentialProviderOpenAI)
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

func TestModelCredentialValue(t *testing.T) {
	const (
		anthropic = ModelCredentialProviderAnthropic
		openai    = ModelCredentialProviderOpenAI
	)
	for _, tc := range []struct {
		raw      string
		provider ModelCredentialProvider
		kind     ModelCredentialKind
		want     string
		ok       bool
	}{
		{"sk-ant-api03-abc", anthropic, ModelCredentialAPIKey, "sk-ant-api03-abc", true},
		{"  sk-ant-oat01-abc\n", anthropic, ModelCredentialOAuthToken, "sk-ant-oat01-abc", true},
		{"sk-ant-oat01-abc", anthropic, ModelCredentialAPIKey, "", false},
		{"sk-ant-api03-abc", anthropic, ModelCredentialOAuthToken, "", false},
		{"some-other-key", anthropic, ModelCredentialAPIKey, "some-other-key", true},
		{"two words", anthropic, ModelCredentialAPIKey, "", false},
		{"crlf\r\nX-Inject: 1", anthropic, ModelCredentialAPIKey, "", false},
		{"   ", anthropic, ModelCredentialAPIKey, "", false},
		{"ключ", anthropic, ModelCredentialAPIKey, "", false},
		{"sk-ant-api03-abc", anthropic, "password", "", false},
		{strings.Repeat("a", ModelCredentialSecretMaxBytes), anthropic, ModelCredentialAPIKey,
			strings.Repeat("a", ModelCredentialSecretMaxBytes), true},
		{strings.Repeat("a", ModelCredentialSecretMaxBytes+1), anthropic, ModelCredentialAPIKey, "", false},
		// An OpenAI key stored as an Anthropic credential, of either kind.
		{"sk-proj-abc", anthropic, ModelCredentialAPIKey, "", false},
		{"sk-abc", anthropic, ModelCredentialAPIKey, "", false},
		{"sk-proj-abc", anthropic, ModelCredentialOAuthToken, "", false},
		// OpenAI: API keys only, and never an Anthropic value.
		{"sk-proj-abc", openai, ModelCredentialAPIKey, "sk-proj-abc", true},
		{" sk-svcacct-abc\n", openai, ModelCredentialAPIKey, "sk-svcacct-abc", true},
		{"sk-abc", openai, ModelCredentialAPIKey, "sk-abc", true},
		{"some-other-key", openai, ModelCredentialAPIKey, "some-other-key", true},
		{"sk-proj-abc", openai, ModelCredentialOAuthToken, "", false},
		{"sk-ant-api03-abc", openai, ModelCredentialAPIKey, "", false},
		{"sk-ant-oat01-abc", openai, ModelCredentialAPIKey, "", false},
		{"two words", openai, ModelCredentialAPIKey, "", false},
		// A provider that takes nothing.
		{"sk-proj-abc", "bedrock", ModelCredentialAPIKey, "", false},
		{"sk-ant-api03-abc", "", ModelCredentialAPIKey, "", false},
	} {
		got, ok := ModelCredentialValue(tc.raw, tc.provider, tc.kind)
		if got != tc.want || ok != tc.ok {
			t.Errorf("ModelCredentialValue(%q, %s, %s) = %q, %v; want %q, %v",
				tc.raw, tc.provider, tc.kind, got, ok, tc.want, tc.ok)
		}
	}
}

// TestModelCredentialKinds: Anthropic takes an API key or a setup-token,
// OpenAI an API key only, anything else nothing.
func TestModelCredentialKinds(t *testing.T) {
	for _, tc := range []struct {
		provider ModelCredentialProvider
		want     []ModelCredentialKind
	}{
		{ModelCredentialProviderAnthropic, []ModelCredentialKind{ModelCredentialAPIKey, ModelCredentialOAuthToken}},
		{ModelCredentialProviderOpenAI, []ModelCredentialKind{ModelCredentialAPIKey}},
		{"bedrock", nil},
	} {
		if got := ModelCredentialKinds(tc.provider); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ModelCredentialKinds(%s) = %v, want %v", tc.provider, got, tc.want)
		}
	}
	if ModelCredentialKindAllowed(ModelCredentialProviderOpenAI, ModelCredentialOAuthToken) {
		t.Error("an openai setup-token is allowed")
	}
	if !reflect.DeepEqual(ModelCredentialProviders,
		[]ModelCredentialProvider{ModelCredentialProviderAnthropic, ModelCredentialProviderOpenAI}) {
		t.Errorf("ModelCredentialProviders = %v", ModelCredentialProviders)
	}
}

// TestModelCredentialValueProperty: whatever is offered, an accepted value
// is a single token of printable ASCII within the bound, the input trimmed
// and nothing else changed, so what the status server stores is exactly
// what the broker will send; it is of a kind its provider takes, and never
// carries the other provider's prefix.
func TestModelCredentialValueProperty(t *testing.T) {
	cfg := &quick.Config{MaxCount: 5000, Rand: rand.New(rand.NewSource(20261018))}
	cfg.Values = func(args []reflect.Value, r *rand.Rand) {
		const alphabet = "abcXYZ019-_.~ \t\r\n\x00\x7f:/é"
		b := make([]byte, 0, 64)
		if r.Intn(3) == 0 {
			b = append(b, []string{"sk-ant-api", "sk-ant-oat", "sk-proj-", "sk-", " ", "\n"}[r.Intn(6)]...)
		}
		for range r.Intn(48) {
			b = append(b, alphabet[r.Intn(len(alphabet))])
		}
		kinds := []ModelCredentialKind{ModelCredentialAPIKey, ModelCredentialOAuthToken}
		providers := []ModelCredentialProvider{ModelCredentialProviderAnthropic, ModelCredentialProviderOpenAI}
		args[0], args[1] = reflect.ValueOf(string(b)), reflect.ValueOf(providers[r.Intn(2)])
		args[2] = reflect.ValueOf(kinds[r.Intn(2)])
	}
	prop := func(raw string, provider ModelCredentialProvider, kind ModelCredentialKind) bool {
		v, ok := ModelCredentialValue(raw, provider, kind)
		if !ok {
			return v == ""
		}
		if v != strings.TrimSpace(raw) || len(v) == 0 || len(v) > ModelCredentialSecretMaxBytes {
			return false
		}
		// Never a kind the provider does not take, and never the other
		// provider's value.
		if !ModelCredentialKindAllowed(provider, kind) {
			return false
		}
		ant := strings.HasPrefix(v, "sk-ant-")
		if provider == ModelCredentialProviderOpenAI && ant ||
			provider == ModelCredentialProviderAnthropic && strings.HasPrefix(v, "sk-") && !ant {
			return false
		}
		for i := 0; i < len(v); i++ {
			if v[i] < 0x21 || v[i] > 0x7e {
				return false
			}
		}
		return true
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}
