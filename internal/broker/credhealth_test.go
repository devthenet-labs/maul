// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package broker

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bitwise-media-group/patchy/api/v1alpha1"
)

// scriptedProbe answers each probe with the next result and records what
// it was asked.
type scriptedProbe struct {
	mu      sync.Mutex
	results []ProbeResult
	asked   []string
}

func (p *scriptedProbe) probe(_ context.Context, kind v1alpha1.ModelCredentialKind, cred string) ProbeResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.asked = append(p.asked, string(kind)+":"+cred)
	if len(p.results) == 0 {
		return ProbeInconclusive
	}
	r := p.results[0]
	if len(p.results) > 1 {
		p.results = p.results[1:]
	}
	return r
}

func (p *scriptedProbe) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.asked)
}

// healthFixture is one person's stored credential under a health probe on
// a fake clock.
type healthFixture struct {
	store  *fakeStore
	clock  *fakeClock
	probe  *scriptedProbe
	h      *credentialHealth
	record string
}

func newHealthFixture(t *testing.T, results ...ProbeResult) *healthFixture {
	t.Helper()
	f := &healthFixture{store: newFakeStore(), clock: newFakeClock(), probe: &scriptedProbe{results: results},
		record: v1alpha1.ModelCredentialName(person, v1alpha1.ModelCredentialProviderAnthropic)}
	f.store.put(personID, v1alpha1.ModelCredentialAPIKey, userKey)
	f.h = newCredentialHealth(HealthConfig{Probe: f.probe.probe}, f.store, v1alpha1.ModelCredentialProviderAnthropic,
		f.clock.now, slog.New(slog.DiscardHandler))
	return f
}

func (f *healthFixture) status() v1alpha1.ModelCredentialStatus {
	return f.store.record(f.record).Status
}

func (f *healthFixture) secretVersion() string {
	v, _ := f.store.SecretVersion(secretNameOf(f.record))
	return v
}

// TestHealthProbeJudgesTheStoredVersion: an unjudged credential is probed
// once and its health written for the Secret version probed; an informer
// resync after that probes nothing.
func TestHealthProbeJudgesTheStoredVersion(t *testing.T) {
	f := newHealthFixture(t, ProbeValid)
	f.store.fire(f.record)
	f.h.step(t.Context())
	st := f.status()
	if st.Health != v1alpha1.ModelCredentialValid || st.SecretVersion != f.secretVersion() {
		t.Fatalf("status = %+v, want Valid for version %s", st, f.secretVersion())
	}
	if got := f.probe.asked; len(got) != 1 || got[0] != "api-key:"+userKey {
		t.Fatalf("probes = %v", got)
	}
	f.clock.advance(time.Hour)
	f.store.fire(f.record)
	f.h.step(t.Context())
	if f.probe.count() != 1 {
		t.Errorf("probes = %d after a resync, want still 1", f.probe.count())
	}
}

// TestHealthResetsOnAnySecretChange: a credential replaced after it was
// judged is reset to Unknown (for the new version) before it is probed,
// and the probe's verdict is then written for the new version.
func TestHealthResetsOnAnySecretChange(t *testing.T) {
	f := newHealthFixture(t, ProbeValid, ProbeRejected)
	f.store.fire(f.record)
	f.h.step(t.Context())
	f.store.put(personID, v1alpha1.ModelCredentialAPIKey, "sk-ant-api03-user42-replaced000")
	next := f.secretVersion()
	f.clock.advance(DefaultProbeSpacing)
	f.store.fire(f.record)
	f.h.step(t.Context())

	var healths []string
	for _, p := range f.store.patches {
		if p.patch.Health != nil {
			healths = append(healths, string(*p.patch.Health)+"@"+*p.patch.SecretVersion)
		}
	}
	first := f.store.patches[0].patch.SecretVersion
	want := []string{"Valid@" + *first, "Unknown@" + next, "Rejected@" + next}
	if strings.Join(healths, ",") != strings.Join(want, ",") {
		t.Fatalf("health writes = %v, want %v", healths, want)
	}
	if st := f.status(); st.LastRejectedAt == nil {
		t.Error("lastRejectedAt not set with Rejected")
	}
}

// TestRejectedOnlyWhileTheProbedVersionIsCurrent: a credential replaced
// while it was being probed is never marked with the old one's verdict;
// the new one is judged instead.
func TestRejectedOnlyWhileTheProbedVersionIsCurrent(t *testing.T) {
	f := newHealthFixture(t, ProbeRejected, ProbeValid)
	f.store.onRead = func(_ string, n int) {
		if n == 2 { // the re-read after the first probe
			f.store.onRead = nil
			f.store.put(personID, v1alpha1.ModelCredentialAPIKey, "sk-ant-api03-user42-replaced000")
		}
	}
	f.store.fire(f.record)
	f.h.step(t.Context())
	for _, p := range f.store.patches {
		if p.patch.Health != nil && *p.patch.Health == v1alpha1.ModelCredentialRejected {
			t.Fatalf("Rejected written for a replaced credential: %+v", p)
		}
	}
	f.clock.advance(DefaultProbeSpacing)
	f.h.step(t.Context())
	if st := f.status(); st.Health != v1alpha1.ModelCredentialValid || st.SecretVersion != f.secretVersion() {
		t.Errorf("status = %+v, want the replacement judged Valid", st)
	}
}

// TestHealthWriteIsConditionalOnTheRecord: a record changed between the
// read and the write (the status server resetting it) refuses the write,
// which is retried, not forced.
func TestHealthWriteIsConditionalOnTheRecord(t *testing.T) {
	f := newHealthFixture(t, ProbeRejected)
	f.store.onRead = func(_ string, n int) {
		if n == 2 {
			f.store.update(f.record, func(r *v1alpha1.ModelCredential) {
				r.Status.Health = v1alpha1.ModelCredentialUnknown
			})
		}
	}
	f.store.fire(f.record)
	f.h.step(t.Context())
	if f.store.patchCount() != 0 {
		t.Fatalf("patches = %+v, want the conflicting write refused", f.store.patches)
	}
	f.store.onRead = nil
	f.clock.advance(verdictRetry)
	f.h.step(t.Context())
	if st := f.status(); st.Health != v1alpha1.ModelCredentialRejected {
		t.Errorf("health = %s after the retry, want Rejected", st.Health)
	}
	if f.probe.count() != 1 {
		t.Errorf("probes = %d, want 1: a refused write is retried without probing again", f.probe.count())
	}
}

// TestRecheckIsThrottled: a run's 401s ask for rechecks, honoured at most
// once per Interval for the same stored credential.
func TestRecheckIsThrottled(t *testing.T) {
	f := newHealthFixture(t, ProbeValid)
	f.store.fire(f.record)
	f.h.step(t.Context())
	for range 5 {
		f.h.recheck(f.record)
		f.clock.advance(time.Minute)
		f.h.step(t.Context())
	}
	if f.probe.count() != 1 {
		t.Fatalf("probes = %d within the interval, want 1", f.probe.count())
	}
	f.clock.advance(DefaultProbeInterval)
	f.h.step(t.Context())
	if f.probe.count() != 2 {
		t.Errorf("probes = %d past the interval, want 2", f.probe.count())
	}
}

// TestProbesAreSpaced: many credentials due at once are probed one per
// Spacing, never in a burst.
func TestProbesAreSpaced(t *testing.T) {
	f := newHealthFixture(t, ProbeValid)
	for id := int64(100); id < 105; id++ {
		f.store.put(id, v1alpha1.ModelCredentialAPIKey, userKey)
		f.store.fire(v1alpha1.ModelCredentialName(v1alpha1.ModelCredentialOwner(id),
			v1alpha1.ModelCredentialProviderAnthropic))
	}
	f.h.step(t.Context())
	if f.probe.count() != 1 {
		t.Fatalf("probes at once = %d, want 1", f.probe.count())
	}
	for i := 2; i <= 5; i++ {
		f.clock.advance(DefaultProbeSpacing)
		f.h.step(t.Context())
		if f.probe.count() != i {
			t.Fatalf("probes after %d spacings = %d, want %d", i-1, f.probe.count(), i)
		}
	}
}

// TestInconclusiveProbesAreBounded: an inconclusive probe writes nothing
// and is retried a bounded number of times.
func TestInconclusiveProbesAreBounded(t *testing.T) {
	f := newHealthFixture(t, ProbeInconclusive)
	f.store.fire(f.record)
	for range 20 {
		f.h.step(t.Context())
		f.clock.advance(time.Hour)
	}
	if f.probe.count() != probeAttempts {
		t.Errorf("probes = %d, want %d", f.probe.count(), probeAttempts)
	}
	if f.store.patchCount() != 0 {
		t.Errorf("patches = %+v, want none", f.store.patches)
	}
}

// TestLastUsedAtIsCoarse: lastUsedAt is written at most once per
// UsedEvery, however many requests use the credential.
func TestLastUsedAtIsCoarse(t *testing.T) {
	f := newHealthFixture(t, ProbeValid)
	f.store.fire(f.record)
	f.h.step(t.Context())
	before := f.store.patchCount()
	for range 10 {
		f.h.used(f.record)
		f.clock.advance(time.Minute)
		f.h.step(t.Context())
	}
	if got := f.store.patchCount() - before; got != 1 {
		t.Fatalf("lastUsedAt writes = %d, want 1", got)
	}
	f.clock.advance(DefaultUsedEvery)
	f.h.used(f.record)
	f.h.step(t.Context())
	if got := f.store.patchCount() - before; got != 2 {
		t.Errorf("lastUsedAt writes = %d after the period, want 2", got)
	}
	if st := f.status(); st.LastUsedAt == nil || !st.LastUsedAt.Time.Equal(f.clock.now()) {
		t.Errorf("lastUsedAt = %v, want %v", st.LastUsedAt, f.clock.now())
	}
}

// TestMalformedCredentialIsNotProbed: a Secret that fails the contract is
// never sent to the provider.
func TestMalformedCredentialIsNotProbed(t *testing.T) {
	f := newHealthFixture(t, ProbeValid)
	f.store.secrets[secretNameOf(f.record)].Labels = nil
	f.store.fire(f.record)
	f.h.step(t.Context())
	if f.probe.count() != 0 {
		t.Errorf("probes = %d, want none", f.probe.count())
	}
}

// TestAnthropicProbe: an API key lists one model and a setup-token asks
// for one token with the OAuth beta, each with its own header; 2xx is
// Valid, 401 Rejected, anything else (a redirect included, never
// followed) inconclusive.
func TestAnthropicProbe(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	status := http.StatusOK
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.RequestURI()+" key="+r.Header.Get("x-api-key")+
			" auth="+r.Header.Get("Authorization")+" beta="+r.Header.Get("anthropic-beta")+
			" version="+r.Header.Get("anthropic-version"))
		code := status
		mu.Unlock()
		if code == http.StatusFound {
			http.Redirect(w, r, "https://elsewhere.example/steal", code)
			return
		}
		w.WriteHeader(code)
		_, _ = io.WriteString(w, "{}")
	}))
	defer up.Close()
	probe := AnthropicProbe(mustTarget(t, up.URL), "claude-haiku-4-5", probeClient())

	if got := probe(t.Context(), v1alpha1.ModelCredentialAPIKey, "k1"); got != ProbeValid {
		t.Errorf("api key probe = %v, want Valid", got)
	}
	if got := probe(t.Context(), v1alpha1.ModelCredentialOAuthToken, "t1"); got != ProbeValid {
		t.Errorf("token probe = %v, want Valid", got)
	}
	want := []string{
		"GET /v1/models?limit=1 key=k1 auth= beta= version=2023-06-01",
		"POST /v1/messages key= auth=Bearer t1 beta=oauth-2025-04-20 version=2023-06-01",
	}
	mu.Lock()
	if strings.Join(seen, "\n") != strings.Join(want, "\n") {
		t.Errorf("probe requests:\n%s\nwant:\n%s", strings.Join(seen, "\n"), strings.Join(want, "\n"))
	}
	mu.Unlock()
	for code, want := range map[int]ProbeResult{
		http.StatusUnauthorized: ProbeRejected, http.StatusForbidden: ProbeInconclusive,
		http.StatusTooManyRequests: ProbeInconclusive, http.StatusInternalServerError: ProbeInconclusive,
		http.StatusFound: ProbeInconclusive,
	} {
		mu.Lock()
		status = code
		mu.Unlock()
		if got := probe(t.Context(), v1alpha1.ModelCredentialAPIKey, "k1"); got != want {
			t.Errorf("probe answered %d = %v, want %v", code, got, want)
		}
	}
}
