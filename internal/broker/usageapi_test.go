// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package broker

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bitwise-media-group/patchy/internal/usagewire"
)

// usageTok mints a token like tok, bound to the given audience and pod.
func usageTok(sub, audience string) string {
	return tokWith(map[string]any{
		"sub": sub, "aud": []string{audience}, "exp": tokenExp,
		"kubernetes.io": map[string]any{"pod": map[string]any{"name": "controller-pod"}},
	})
}

func usageServer(t *testing.T, upstream string) *Server {
	t.Helper()
	return newTestServer(t, Config{
		Upstreams: map[string]Upstream{"anthropic": {
			Target: mustTarget(t, upstream), CredentialKind: usagewire.CredentialOAuthToken,
		}},
		Usage: UsageConfig{Callers: []string{testController}},
	}, nil)
}

func getUsage(h http.Handler, method, job, authz string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, usagewire.PathPrefix+job, nil)
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// proxyAs sends one Messages request through the proxy as the given caller.
func proxyAs(h http.Handler, sub string) int {
	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-5","max_tokens":1000}`))
	req.Header.Set(TokenHeader, tok(sub))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

// TestUsageEndpointAuth: the endpoint answers only a controller identity
// presenting a token for the usage audience. An agent pod's broker token,
// an agent identity under the usage audience, a missing or malformed bearer
// and an unauthenticated token are all 401; a controller's usage token is
// refused on the proxy.
func TestUsageEndpointAuth(t *testing.T) {
	var hits atomic.Int64
	up := countingUpstream(t, &hits, "text/event-stream", sseUsage(50, 3, true))
	s := usageServer(t, up.URL)
	h := s.UsageHandler()
	for _, tt := range []struct {
		name, authz string
		want        int
	}{
		{"no token", "", http.StatusUnauthorized},
		{"not bearer", "Basic " + usageTok("controller", usagewire.Audience), http.StatusUnauthorized},
		{"agent token for the proxy audience", "Bearer " + tok("good"), http.StatusUnauthorized},
		{"agent identity under the usage audience", "Bearer " + usageTok("good", usagewire.Audience),
			http.StatusUnauthorized},
		{"controller identity under the agent audience", "Bearer " + usageTok("controller", DefaultAudience),
			http.StatusUnauthorized},
		{"unauthenticated", "Bearer " + usageTok("nobody", usagewire.Audience), http.StatusUnauthorized},
		{"junk", "Bearer not-a-jwt", http.StatusUnauthorized},
		{"controller", "Bearer " + usageTok("controller", usagewire.Audience), http.StatusOK},
		{"bearer scheme case", "bearer " + usageTok("controller", usagewire.Audience), http.StatusOK},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if rec := getUsage(h, http.MethodGet, testJob, tt.authz); rec.Code != tt.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.want, rec.Body.String())
			}
		})
	}
	// The controller's usage token opens nothing on the proxy.
	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(`{}`))
	req.Header.Set(TokenHeader, usageTok("controller", usagewire.Audience))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || hits.Load() != 0 {
		t.Fatalf("controller token on the proxy: status %d, upstream hits %d", rec.Code, hits.Load())
	}
	// And the proxy does not serve the usage path at all.
	if rec := getUsage(s.Handler(), http.MethodGet, testJob,
		"Bearer "+usageTok("controller", usagewire.Audience)); rec.Code != http.StatusNotFound {
		t.Errorf("usage path on the proxy listener: status %d, want 404", rec.Code)
	}
}

// TestUsageEndpointRequests: a bad Job name is 400, a method other than GET
// is refused, and the endpoint is off without callers.
func TestUsageEndpointRequests(t *testing.T) {
	s := usageServer(t, "https://api.anthropic.example")
	h := s.UsageHandler()
	authz := "Bearer " + usageTok("controller", usagewire.Audience)
	if rec := getUsage(h, http.MethodGet, "Not_A_Job", authz); rec.Code != http.StatusBadRequest {
		t.Errorf("bad job name: status %d, want 400", rec.Code)
	}
	if rec := getUsage(h, http.MethodGet, strings.Repeat("a", usagewire.MaxJobName+1), authz); rec.Code !=
		http.StatusBadRequest {
		t.Errorf("over-long job name: status %d, want 400", rec.Code)
	}
	if rec := getUsage(h, http.MethodPost, testJob, authz); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: status %d, want 405", rec.Code)
	}
	off := newTestServer(t, Config{Upstreams: map[string]Upstream{"anthropic": {
		Target: mustTarget(t, "https://api.anthropic.example")}}}, nil)
	if off.UsageHandler() != nil {
		t.Error("usage handler served with no callers configured")
	}
}

// TestUsageEndpointReportsJob: a Job's pod's proxied traffic is reported per
// model, observed apart from the shortfall of a cut stream, with the route's
// credential kind, the broker's boot time and the pod's firstSeen; another
// pod's traffic is not.
func TestUsageEndpointReportsJob(t *testing.T) {
	var cut atomic.Bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sseUsage(50, 3, !cut.Load())))
	}))
	t.Cleanup(up.Close)
	s := usageServer(t, up.URL)
	if code := proxyAs(s.Handler(), "jobpod"); code != http.StatusOK {
		t.Fatalf("proxy: %d", code)
	}
	cut.Store(true)
	if code := proxyAs(s.Handler(), "jobpod"); code != http.StatusOK {
		t.Fatalf("proxy: %d", code)
	}
	if code := proxyAs(s.Handler(), "good"); code != http.StatusOK { // agent-pod-1, another Job
		t.Fatalf("proxy: %d", code)
	}
	rec := getUsage(s.UsageHandler(), http.MethodGet, testJob, "Bearer "+usageTok("controller", usagewire.Audience))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q", cc)
	}
	var resp usagewire.Response
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.BootTime.Equal(s.boot.UTC()) || resp.BootTime.IsZero() {
		t.Errorf("bootTime = %v, want %v", resp.BootTime, s.boot)
	}
	if len(resp.Pods) != 1 {
		t.Fatalf("pods = %+v, want the one pod of %s", resp.Pods, testJob)
	}
	pod := resp.Pods[0]
	if pod.Name != testJobPod || pod.UID != "uid-1" || pod.Inflight != 0 || pod.FirstSeen.IsZero() ||
		pod.FirstSeen.Before(resp.BootTime) {
		t.Errorf("pod = %+v", pod)
	}
	if len(pod.Models) != 1 {
		t.Fatalf("models = %+v", pod.Models)
	}
	m := pod.Models[0]
	// Two responses each observed 50 input, 10 cache writes, 20 cache
	// reads and 3 output; the cut one's worst case adds max_tokens' output
	// beyond what it reported (1000 - 3) as shortfall.
	want := usagewire.Model{Model: "claude-sonnet-5", Route: "anthropic", Credential: usagewire.CredentialOAuthToken,
		Input: 100, Output: 6, CacheRead: 40, CacheWrite5m: 20, EstimatedShortfall: 1000 - 3}
	if m != want {
		t.Errorf("model = %+v\nwant    %+v", m, want)
	}
}

// TestUsageEndpointRestartEpoch: a broker that restarts loses its ledger and
// says so through a later boot time, which is what keeps a controller from
// taking a partial ledger for a whole one.
func TestUsageEndpointRestartEpoch(t *testing.T) {
	up := countingUpstream(t, new(atomic.Int64), "text/event-stream", sseUsage(50, 3, true))
	first := usageServer(t, up.URL)
	if code := proxyAs(first.Handler(), "jobpod"); code != http.StatusOK {
		t.Fatalf("proxy: %d", code)
	}
	time.Sleep(2 * time.Millisecond)
	second := usageServer(t, up.URL) // the restarted process
	authz := "Bearer " + usageTok("controller", usagewire.Audience)
	var before, after usagewire.Response
	for h, out := range map[http.Handler]*usagewire.Response{
		first.UsageHandler(): &before, second.UsageHandler(): &after,
	} {
		rec := getUsage(h, http.MethodGet, testJob, authz)
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatal(err)
		}
	}
	if len(before.Pods) != 1 || len(after.Pods) != 0 {
		t.Fatalf("pods before %d after %d, want 1 then 0", len(before.Pods), len(after.Pods))
	}
	if !after.BootTime.After(before.BootTime) {
		t.Errorf("restart boot time %v not after %v", after.BootTime, before.BootTime)
	}
}

// TestUsageEndpointInflight: a request still streaming shows as inflight.
func TestUsageEndpointInflight(t *testing.T) {
	var hits atomic.Int64
	up, started, unblock := blockedUpstream(t, &hits)
	s := usageServer(t, up.URL)
	done := make(chan int, 1)
	go func() { done <- proxyAs(s.Handler(), "jobpod") }()
	<-started
	authz := "Bearer " + usageTok("controller", usagewire.Audience)
	var resp usagewire.Response
	_ = json.Unmarshal(getUsage(s.UsageHandler(), http.MethodGet, testJob, authz).Body.Bytes(), &resp)
	if len(resp.Pods) != 1 || resp.Pods[0].Inflight != 1 {
		t.Fatalf("while streaming: %+v, want inflight 1", resp.Pods)
	}
	unblock()
	<-done
	_ = json.Unmarshal(getUsage(s.UsageHandler(), http.MethodGet, testJob, authz).Body.Bytes(), &resp)
	if resp.Pods[0].Inflight != 0 {
		t.Fatalf("after: inflight %d, want 0", resp.Pods[0].Inflight)
	}
}

// TestUsageConfigValidation: the endpoint answers ServiceAccounts only,
// never the agent one, under an audience of its own.
func TestUsageConfigValidation(t *testing.T) {
	base := Config{AgentNamespace: testNamespace, AgentServiceAccount: testSA,
		Upstreams: map[string]Upstream{"anthropic": {Target: mustTarget(t, "https://a.example")}}}
	for name, u := range map[string]UsageConfig{
		"not a service account": {Callers: []string{"alice"}},
		"no name":               {Callers: []string{"system:serviceaccount:patchy:"}},
		"extra segment":         {Callers: []string{"system:serviceaccount:patchy:a:b"}},
		"the agent":             {Callers: []string{testSubject}},
		"agent audience":        {Callers: []string{testController}, Audience: DefaultAudience},
	} {
		cfg := base
		cfg.Usage = u
		if err := cfg.withDefaults().validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	cfg := base
	cfg.Usage = UsageConfig{Callers: []string{testController}}
	if err := cfg.withDefaults().validate(); err != nil {
		t.Errorf("valid usage config refused: %v", err)
	}
}
