// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package broker

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/provider"
	"github.com/devthenet-labs/maul/internal/usagewire"
)

// openaiCredentialUpstream records the Authorization header of every
// request it answers, replying with status, or a completed Responses
// stream when status is 0.
type openaiCredentialUpstream struct {
	*httptest.Server
	mu     sync.Mutex
	seen   []string
	status atomic.Int64
}

func newOpenAICredentialUpstream(t *testing.T) *openaiCredentialUpstream {
	t.Helper()
	u := &openaiCredentialUpstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		u.mu.Lock()
		u.seen = append(u.seen, r.Header.Get("Authorization"))
		u.mu.Unlock()
		if code := int(u.status.Load()); code != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_, _ = io.WriteString(w, `{"error":{"message":"Incorrect API key provided","type":"invalid_request_error",`+
				`"code":"invalid_api_key"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, openaiStream(0, "response.completed", openaiUsageObject(300, 100, 20)))
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *openaiCredentialUpstream) requests() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.seen...)
}

// postOpenAIAs sends a Responses request for gpt-5.3-codex under the sub's
// token.
func postOpenAIAs(h http.Handler, sub string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/openai/v1/responses",
		strings.NewReader(`{"model":"gpt-5.3-codex","stream":true,"input":"hi","max_output_tokens":1000}`))
	req.Header.Set(TokenHeader, tok(sub))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestEnforceOpenAIRouteSendsThePersonsOwnKey: under enforce, with both
// routes, a person's run on the openai route carries that person's OpenAI
// key as a bearer and on the anthropic route their Anthropic key, never the
// org's and never the other provider's; the audit line carries each
// credential's fingerprint, and the ledger records the openai request as
// paid by an API key.
func TestEnforceOpenAIRouteSendsThePersonsOwnKey(t *testing.T) {
	ant := newCredentialUpstream(t)
	oai := newOpenAICredentialUpstream(t)
	store := newFakeStore()
	store.put(personID, v1alpha1.ModelCredentialAPIKey, userKey)
	store.putFor(personID, v1alpha1.ModelCredentialProviderOpenAI, v1alpha1.ModelCredentialAPIKey, userOpenAIKey+"\n")
	s, buf := credServer(t, ant.URL, credServerOpts{
		mode: UserCredentialsEnforce, pod: attributedPod(person), store: store, openai: oai.URL,
	}, nil)
	h := s.Handler()

	if rec := postOpenAIAs(h, "jobpod"); rec.Code != http.StatusOK {
		t.Fatalf("openai: status %d: %s", rec.Code, rec.Body.String())
	}
	if got := oai.requests(); len(got) != 1 || got[0] != "Bearer "+userOpenAIKey {
		t.Fatalf("openai upstream saw %q, want the person's OpenAI key", got)
	}
	line := lastAudit(t, buf)
	if line["route"] != provider.OpenAI || line["user"] != person ||
		line["credential"] != v1alpha1.ModelCredentialFingerprint([]byte(userOpenAIKey)) {
		t.Errorf("openai audit = %v", line)
	}

	if rec := postAs(h, "jobpod"); rec.Code != http.StatusOK {
		t.Fatalf("anthropic: status %d: %s", rec.Code, rec.Body.String())
	}
	if got := ant.requests(); len(got) != 1 || got[0] != (arrived{apiKey: userKey}) {
		t.Fatalf("anthropic upstream saw %+v, want the person's Anthropic key", got)
	}
	for _, secret := range []string{userKey, userOpenAIKey, orgKey, orgOpenAIKey} {
		if strings.Contains(buf.String(), secret) {
			t.Errorf("a credential reached the audit log")
		}
	}
	pods := s.ledger.jobUsage(testJob)
	if len(pods) != 1 {
		t.Fatalf("ledger = %+v", pods)
	}
	var found bool
	for _, m := range pods[0].Models {
		if m.Route == provider.OpenAI {
			found = true
			if m.Credential != usagewire.CredentialAPIKey || m.Input != 200 || m.CacheRead != 100 {
				t.Errorf("openai ledger entry = %+v", m)
			}
		}
	}
	if !found {
		t.Errorf("ledger has no openai entry: %+v", pods[0].Models)
	}
}

// TestEnforceNeverCrossesProviders: a person's record pays only for its own
// provider's route. With only an Anthropic key stored, an openai request is
// refused as credential_missing (never sent with that key or the org's),
// and with only an OpenAI key, so is an anthropic request.
func TestEnforceNeverCrossesProviders(t *testing.T) {
	for _, tc := range []struct {
		name     string
		stored   v1alpha1.ModelCredentialProvider
		credExpr string
		post     func(http.Handler, string) *httptest.ResponseRecorder
	}{
		{"an anthropic key on the openai route", v1alpha1.ModelCredentialProviderAnthropic, userKey, postOpenAIAs},
		{"an openai key on the anthropic route", v1alpha1.ModelCredentialProviderOpenAI, userOpenAIKey, postAs},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ant := newCredentialUpstream(t)
			oai := newOpenAICredentialUpstream(t)
			store := newFakeStore()
			store.putFor(personID, tc.stored, v1alpha1.ModelCredentialAPIKey, tc.credExpr)
			s, buf := credServer(t, ant.URL, credServerOpts{
				mode: UserCredentialsEnforce, pod: attributedPod(person), store: store, openai: oai.URL,
			}, nil)
			rec := tc.post(s.Handler(), "jobpod")
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status %d, want 403: %s", rec.Code, rec.Body.String())
			}
			if msg := rec.Body.String(); !strings.Contains(msg, "("+reasonCredentialMissing+")") {
				t.Errorf("message %q does not name %s", msg, reasonCredentialMissing)
			}
			if n := len(ant.requests()) + len(oai.requests()); n != 0 {
				t.Errorf("upstreams saw %d requests, want none", n)
			}
			if line := lastAudit(t, buf); line["reason"] != reasonCredentialMissing {
				t.Errorf("audit = %v", line)
			}
		})
	}
}

// TestOpenAIRejectionAsksForARecheck: a 401 on a person's OpenAI key
// queues a recheck of the openai record alone, which the openai health
// probes; agent traffic is never itself the verdict.
func TestOpenAIRejectionAsksForARecheck(t *testing.T) {
	ant := newCredentialUpstream(t)
	oai := newOpenAICredentialUpstream(t)
	oai.status.Store(http.StatusUnauthorized)
	store := newFakeStore()
	v := store.putFor(personID, v1alpha1.ModelCredentialProviderOpenAI, v1alpha1.ModelCredentialAPIKey, userOpenAIKey)
	name := v1alpha1.ModelCredentialName(person, v1alpha1.ModelCredentialProviderOpenAI)
	store.update(name, func(r *v1alpha1.ModelCredential) {
		r.Status.Health, r.Status.SecretVersion = v1alpha1.ModelCredentialValid, v
	})
	var probes atomic.Int64
	s, _ := credServer(t, ant.URL, credServerOpts{
		mode: UserCredentialsEnforce, pod: attributedPod(person), store: store, openai: oai.URL,
		probe: func(_ context.Context, _ v1alpha1.ModelCredentialKind, cred string) ProbeResult {
			if cred == userOpenAIKey {
				probes.Add(1)
			}
			return ProbeRejected
		},
	}, nil)
	if rec := postOpenAIAs(s.Handler(), "jobpod"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want the upstream's 401", rec.Code)
	}
	if got := store.record(name).Status.Health; got != v1alpha1.ModelCredentialValid {
		t.Fatalf("health = %s after agent traffic alone, want Valid", got)
	}
	s.creds[provider.Anthropic].health.step(t.Context())
	if probes.Load() != 0 {
		t.Fatalf("the anthropic health probed an openai record")
	}
	s.creds[provider.OpenAI].health.step(t.Context())
	if probes.Load() != 1 {
		t.Fatalf("probes = %d, want 1", probes.Load())
	}
	if got := store.record(name).Status.Health; got != v1alpha1.ModelCredentialRejected {
		t.Errorf("health = %s after the probe, want Rejected", got)
	}
}

// TestOpenAIProbe: an API key lists the models with the key as a bearer
// (nothing billed); 2xx is Valid, 401 Rejected, anything else no verdict;
// a kind OpenAI does not take is never sent.
func TestOpenAIProbe(t *testing.T) {
	var status atomic.Int64
	var hits atomic.Int64
	var auth, path atomic.Pointer[string]
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		a, p := r.Header.Get("Authorization"), r.Method+" "+r.URL.Path
		auth.Store(&a)
		path.Store(&p)
		w.WriteHeader(int(status.Load()))
	}))
	t.Cleanup(up.Close)
	probe := OpenAIProbe(mustTarget(t, up.URL), probeClient())
	for _, tc := range []struct {
		status int
		want   ProbeResult
	}{
		{http.StatusOK, ProbeValid},
		{http.StatusUnauthorized, ProbeRejected},
		{http.StatusForbidden, ProbeInconclusive},
		{http.StatusTooManyRequests, ProbeInconclusive},
		{http.StatusInternalServerError, ProbeInconclusive},
		{http.StatusFound, ProbeInconclusive},
	} {
		status.Store(int64(tc.status))
		if got := probe(t.Context(), v1alpha1.ModelCredentialAPIKey, userOpenAIKey); got != tc.want {
			t.Errorf("status %d: probe = %v, want %v", tc.status, got, tc.want)
		}
		if *auth.Load() != "Bearer "+userOpenAIKey || *path.Load() != "GET /v1/models" {
			t.Errorf("probe sent %q to %q", *auth.Load(), *path.Load())
		}
	}
	before := hits.Load()
	if got := probe(t.Context(), v1alpha1.ModelCredentialOAuthToken, "sk-ant-oat01-x"); got != ProbeInconclusive {
		t.Errorf("setup-token probe = %v, want inconclusive", got)
	}
	if hits.Load() != before {
		t.Error("a setup-token was sent to OpenAI")
	}
	unreachable := OpenAIProbe(mustTarget(t, "http://127.0.0.1:1"), probeClient())
	if got := unreachable(t.Context(), v1alpha1.ModelCredentialAPIKey, userOpenAIKey); got != ProbeInconclusive {
		t.Errorf("unreachable probe = %v, want inconclusive", got)
	}
}

// TestOpenAIRouteRefusesAnAttachedSetupToken: the openai route never sends
// a person's credential of a kind OpenAI does not take (decide refuses such
// a record first; this is the route's own guard).
func TestOpenAIRouteRefusesAnAttachedSetupToken(t *testing.T) {
	route := OpenAI(mustTarget(t, "https://api.openai.com"), keyFile(t, orgOpenAIKey))
	req := httptest.NewRequest(http.MethodPost, "https://api.openai.com/v1/responses", nil)
	ctx := withAttached(t.Context(), &attached{kind: v1alpha1.ModelCredentialOAuthToken, credential: "sk-ant-oat01-x"})
	if err := route.Credential(ctx, req); err == nil {
		t.Fatal("Credential = nil for an attached setup-token")
	}
	if v := req.Header.Get("Authorization"); v != "" {
		t.Errorf("Authorization = %q, want none", v)
	}
	ctx = withAttached(t.Context(), &attached{kind: v1alpha1.ModelCredentialAPIKey, credential: userOpenAIKey})
	if err := route.Credential(ctx, req); err != nil || req.Header.Get("Authorization") != "Bearer "+userOpenAIKey {
		t.Errorf("Credential = %v, Authorization %q", err, req.Header.Get("Authorization"))
	}
}
