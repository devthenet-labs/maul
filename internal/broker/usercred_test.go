// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package broker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"

	"github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/usagewire"
)

const (
	orgKey   = "sk-ant-org-0000000000000000"
	userKey  = "sk-ant-api03-user42-abcdefghijkl"
	userTok  = "sk-ant-oat01-user42-mnopqrstuvwx"
	personID = 42
)

var person = v1alpha1.ModelCredentialOwner(personID)

// arrived is what credential headers one upstream request carried.
type arrived struct {
	apiKey, authorization string
}

// credentialUpstream records the credential headers of every request it
// answers, replying with status (200 with a usage body when 0).
type credentialUpstream struct {
	*httptest.Server
	mu     sync.Mutex
	seen   []arrived
	status atomic.Int64
}

func newCredentialUpstream(t *testing.T) *credentialUpstream {
	t.Helper()
	u := &credentialUpstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.seen = append(u.seen, arrived{apiKey: r.Header.Get("x-api-key"), authorization: r.Header.Get("Authorization")})
		u.mu.Unlock()
		if code := int(u.status.Load()); code != 0 {
			w.WriteHeader(code)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseUsage(100, 5, true))
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *credentialUpstream) requests() []arrived {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]arrived(nil), u.seen...)
}

// attributedPod is testJobPod (uid-1, the "jobpod" token's) with the
// attributed-to annotation value (none when empty).
func attributedPod(value string) *corev1.Pod {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: testJobPod, Namespace: testNamespace, UID: types.UID("uid-1"),
	}}
	if value != "" {
		pod.Annotations = map[string]string{v1alpha1.AnnotationAttributedTo: value}
	}
	return pod
}

// credServer is a broker over upstream with per-user credentials in mode,
// the org credential an API key (or a setup-token, bearer), store as the
// credential store and pod (nil: none) in the agent namespace. It counts
// pod reads in gets and logs JSON to the returned buffer.
type credServerOpts struct {
	mode   UserCredentialMode
	bearer bool
	ack    bool
	pod    *corev1.Pod
	store  *fakeStore
	probe  ProbeFunc
}

func credServer(t *testing.T, upstream string, o credServerOpts, gets *atomic.Int64) (*Server, *syncBuffer) {
	t.Helper()
	cs := fakeReviews(nil)
	if o.pod != nil {
		if err := cs.Tracker().Add(o.pod); err != nil {
			t.Fatal(err)
		}
	}
	cs.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetNamespace() != testNamespace {
			t.Errorf("pod read in namespace %q, want only the agents namespace", action.GetNamespace())
		}
		if gets != nil {
			gets.Add(1)
		}
		return false, nil, nil
	})
	probe := o.probe
	if probe == nil {
		probe = func(context.Context, v1alpha1.ModelCredentialKind, string) ProbeResult { return ProbeInconclusive }
	}
	var store CredentialStore
	if o.store != nil {
		store = o.store
	}
	var buf syncBuffer
	s, err := New(cs, Config{
		AgentNamespace: testNamespace, AgentServiceAccount: testSA,
		Upstreams: map[string]Upstream{"anthropic": Anthropic(mustTarget(t, upstream), keyFile(t, orgKey), o.bearer)},
		UserCredentials: UserCredentialConfig{
			Mode: o.mode, Store: store, OrgCredentialIsPersonal: o.ack, Health: HealthConfig{Probe: probe},
		},
	}, slog.New(slog.NewJSONHandler(&buf, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return s, &buf
}

// TestEnforceSendsThePersonsOwnCredential is the end-to-end assertion
// against a fake upstream: under enforce a run attributed to a person
// carries that person's API key as x-api-key, or their setup-token as a
// bearer, and never the org credential; the audit line names the person
// and the credential's fingerprint, never the credential; and the usage
// ledger records the kind that paid.
func TestEnforceSendsThePersonsOwnCredential(t *testing.T) {
	for _, tc := range []struct {
		name       string
		kind       v1alpha1.ModelCredentialKind
		credential string
		want       arrived
		usage      string
	}{
		{"api key", v1alpha1.ModelCredentialAPIKey, userKey, arrived{apiKey: userKey}, usagewire.CredentialAPIKey},
		{"setup-token", v1alpha1.ModelCredentialOAuthToken, userTok, arrived{authorization: "Bearer " + userTok},
			usagewire.CredentialOAuthToken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newCredentialUpstream(t)
			store := newFakeStore()
			store.put(personID, tc.kind, tc.credential+"\n")
			s, buf := credServer(t, up.URL, credServerOpts{
				mode: UserCredentialsEnforce, pod: attributedPod(person), store: store,
			}, nil)
			if rec := postAs(s.Handler(), "jobpod"); rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			if got := up.requests(); len(got) != 1 || got[0] != tc.want {
				t.Fatalf("upstream saw %+v, want %+v", got, tc.want)
			}
			line := lastAudit(t, buf)
			if line["user"] != person {
				t.Errorf("audit user = %v, want %s", line["user"], person)
			}
			if want := v1alpha1.ModelCredentialFingerprint([]byte(tc.credential)); line["credential"] != want {
				t.Errorf("audit credential = %v, want %s", line["credential"], want)
			}
			if strings.Contains(buf.String(), tc.credential) || strings.Contains(buf.String(), orgKey) {
				t.Error("a credential reached the audit log")
			}
			pods := s.ledger.jobUsage(testJob)
			if len(pods) != 1 || len(pods[0].Models) != 1 || pods[0].Models[0].Credential != tc.usage {
				t.Errorf("ledger = %+v, want one model paid with %s", pods, tc.usage)
			}
		})
	}
}

// TestOrgAttributedRunsKeepTheOrgCredential: under enforce a run attributed
// to the org uses the org credential, and so does every run under
// attribute, which records the person all the same.
func TestOrgAttributedRunsKeepTheOrgCredential(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mode       UserCredentialMode
		annotation string
		wantUser   string
	}{
		{"enforce, org", UserCredentialsEnforce, "org", "org"},
		{"attribute, a person with a key", UserCredentialsAttribute, person, person},
		{"attribute, org", UserCredentialsAttribute, "org", "org"},
		{"attribute, no annotation", UserCredentialsAttribute, "", auditUserNone},
		{"attribute, invalid annotation", UserCredentialsAttribute, "github:octocat", auditUserInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newCredentialUpstream(t)
			store := newFakeStore()
			store.put(personID, v1alpha1.ModelCredentialAPIKey, userKey)
			s, buf := credServer(t, up.URL, credServerOpts{mode: tc.mode, pod: attributedPod(tc.annotation),
				store: store}, nil)
			if rec := postAs(s.Handler(), "jobpod"); rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			if got := up.requests(); len(got) != 1 || got[0] != (arrived{apiKey: orgKey}) {
				t.Fatalf("upstream saw %+v, want the org key", got)
			}
			line := lastAudit(t, buf)
			if line["user"] != tc.wantUser || line["credential"] != auditCredentialOrg {
				t.Errorf("audit user, credential = %v, %v, want %s, org", line["user"], line["credential"], tc.wantUser)
			}
			if store.reads != 0 {
				t.Errorf("secret reads = %d, want none for an org-paid request", store.reads)
			}
		})
	}
}

// TestEnforceFailsClosed: every broken link under enforce is a refusal
// with its own reason, before any byte reaches the upstream and never with
// the org credential.
func TestEnforceFailsClosed(t *testing.T) {
	ownerName := v1alpha1.ModelCredentialName(person, v1alpha1.ModelCredentialProviderAnthropic)
	secretName := v1alpha1.ModelCredentialSecretName(person, v1alpha1.ModelCredentialProviderAnthropic)
	registered := func() *fakeStore {
		s := newFakeStore()
		s.put(personID, v1alpha1.ModelCredentialAPIKey, userKey)
		return s
	}
	for _, tc := range []struct {
		name   string
		sub    string
		pod    *corev1.Pod
		store  func() *fakeStore
		status int
		reason string
	}{
		{"no annotation", "jobpod", attributedPod(""), registered, http.StatusForbidden, reasonAttributionMissing},
		{"annotation outside the grammar", "jobpod", attributedPod("github:octocat"), registered,
			http.StatusForbidden, reasonAttributionInvalid},
		{"another pod by that name", "jobpod-recycled", attributedPod(person), registered,
			http.StatusForbidden, reasonAttributionUnverified},
		{"pod gone", "jobpod", nil, registered, http.StatusForbidden, reasonAttributionUnverified},
		{"token bound to no pod uid", "good", attributedPod(person), registered, http.StatusForbidden,
			reasonAttributionUnverified},
		{"store not synced", "jobpod", attributedPod(person), func() *fakeStore {
			s := registered()
			s.synced = false
			return s
		}, http.StatusServiceUnavailable, reasonCredentialsUnsynced},
		{"not registered", "jobpod", attributedPod(person), newFakeStore, http.StatusForbidden,
			reasonCredentialMissing},
		{"record for another owner", "jobpod", attributedPod(person), func() *fakeStore {
			s := registered()
			s.update(ownerName, func(r *v1alpha1.ModelCredential) { r.Spec.Owner = v1alpha1.ModelCredentialOwner(43) })
			return s
		}, http.StatusForbidden, reasonCredentialMismatch},
		{"no secret", "jobpod", attributedPod(person), func() *fakeStore {
			s := registered()
			delete(s.secrets, secretName)
			delete(s.informer, secretName)
			return s
		}, http.StatusForbidden, reasonSecretMissing},
		{"secret labelled for another owner", "jobpod", attributedPod(person), func() *fakeStore {
			s := registered()
			s.secrets[secretName].Labels[v1alpha1.LabelCredentialOwner] = v1alpha1.ModelCredentialOwnerHash("github-id:43")
			return s
		}, http.StatusForbidden, reasonSecretInvalid},
		{"secret with a header break", "jobpod", attributedPod(person), func() *fakeStore {
			s := registered()
			s.secrets[secretName].Data[v1alpha1.ModelCredentialSecretKey] = []byte("sk-ant\r\nX-Evil: 1")
			return s
		}, http.StatusForbidden, reasonSecretInvalid},
		{"secret unreadable", "jobpod", attributedPod(person), func() *fakeStore {
			s := registered()
			s.readErr = errors.New("apiserver down")
			return s
		}, http.StatusServiceUnavailable, reasonCredentialUnavailable},
		{"rejected for the stored version", "jobpod", attributedPod(person), func() *fakeStore {
			s := registered()
			v := s.informer[secretName]
			s.update(ownerName, func(r *v1alpha1.ModelCredential) {
				r.Status.Health, r.Status.SecretVersion = v1alpha1.ModelCredentialRejected, v
			})
			return s
		}, http.StatusForbidden, reasonCredentialRejected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newCredentialUpstream(t)
			s, buf := credServer(t, up.URL, credServerOpts{mode: UserCredentialsEnforce, pod: tc.pod,
				store: tc.store()}, nil)
			rec := postAs(s.Handler(), tc.sub)
			if rec.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.status, rec.Body.String())
			}
			if msg := errorMessage(t, rec.Body.Bytes()); !strings.Contains(msg, "("+tc.reason+")") {
				t.Errorf("message %q does not name %s", msg, tc.reason)
			}
			if n := len(up.requests()); n != 0 {
				t.Errorf("upstream saw %d requests, want none", n)
			}
			line := lastAudit(t, buf)
			if line["reason"] != tc.reason || line["credential"] != "" {
				t.Errorf("audit reason, credential = %v, %v, want %s and none", line["reason"], line["credential"],
					tc.reason)
			}
		})
	}
}

// TestRejectionOfAnEarlierCredentialIsNotThisOnes: a person who stored a
// new credential after the last one was rejected uses the new one at once;
// the rejection speaks only for the version it was found for.
func TestRejectionOfAnEarlierCredentialIsNotThisOnes(t *testing.T) {
	up := newCredentialUpstream(t)
	store := newFakeStore()
	old := store.put(personID, v1alpha1.ModelCredentialAPIKey, "sk-ant-api03-old-key-0000000")
	name := v1alpha1.ModelCredentialName(person, v1alpha1.ModelCredentialProviderAnthropic)
	store.update(name, func(r *v1alpha1.ModelCredential) {
		r.Status.Health, r.Status.SecretVersion = v1alpha1.ModelCredentialRejected, old
	})
	store.put(personID, v1alpha1.ModelCredentialAPIKey, userKey)
	s, _ := credServer(t, up.URL, credServerOpts{mode: UserCredentialsEnforce, pod: attributedPod(person),
		store: store}, nil)
	if rec := postAs(s.Handler(), "jobpod"); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if got := up.requests(); len(got) != 1 || got[0].apiKey != userKey {
		t.Fatalf("upstream saw %+v, want the new key", got)
	}
}

// TestRevocationReachesTheBroker: a credential deleted or replaced stops
// being used as soon as the Secret informer reports it, and within
// MaxSecretAge even when the informer has not (a stalled watch): the
// broker's copy is never used past it.
func TestRevocationReachesTheBroker(t *testing.T) {
	secretName := v1alpha1.ModelCredentialSecretName(person, v1alpha1.ModelCredentialProviderAnthropic)
	setup := func(t *testing.T) (*credentialUpstream, *fakeStore, http.Handler, *fakeClock) {
		up := newCredentialUpstream(t)
		store := newFakeStore()
		store.put(personID, v1alpha1.ModelCredentialAPIKey, userKey)
		s, _ := credServer(t, up.URL, credServerOpts{mode: UserCredentialsEnforce, pod: attributedPod(person),
			store: store}, nil)
		clock := newFakeClock()
		s.creds.now = clock.now
		h := s.Handler()
		if rec := postAs(h, "jobpod"); rec.Code != http.StatusOK {
			t.Fatalf("first request: %d", rec.Code)
		}
		return up, store, h, clock
	}

	t.Run("informer reports the delete", func(t *testing.T) {
		_, store, h, _ := setup(t)
		store.mu.Lock()
		delete(store.secrets, secretName)
		delete(store.informer, secretName)
		store.mu.Unlock()
		if rec := postAs(h, "jobpod"); rec.Code != http.StatusForbidden {
			t.Fatalf("after delete: %d, want 403", rec.Code)
		}
	})

	t.Run("informer stalled", func(t *testing.T) {
		up, store, h, clock := setup(t)
		store.mu.Lock()
		delete(store.secrets, secretName) // live only: the informer still has it
		store.mu.Unlock()
		clock.advance(DefaultMaxSecretAge - time.Second)
		if rec := postAs(h, "jobpod"); rec.Code != http.StatusOK {
			t.Fatalf("within the max age: %d, want the cached copy", rec.Code)
		}
		if store.reads != 1 {
			t.Errorf("secret reads = %d, want 1 (cached while the version holds)", store.reads)
		}
		clock.advance(2 * time.Second)
		if rec := postAs(h, "jobpod"); rec.Code != http.StatusForbidden {
			t.Fatalf("past the max age: %d, want 403", rec.Code)
		}
		if n := len(up.requests()); n != 2 {
			t.Errorf("upstream saw %d requests, want 2", n)
		}
	})

	t.Run("replaced", func(t *testing.T) {
		up, store, h, _ := setup(t)
		const next = "sk-ant-api03-user42-replacement0"
		store.put(personID, v1alpha1.ModelCredentialAPIKey, next)
		if rec := postAs(h, "jobpod"); rec.Code != http.StatusOK {
			t.Fatalf("after replace: %d", rec.Code)
		}
		if got := up.requests(); got[len(got)-1].apiKey != next {
			t.Errorf("upstream saw %q, want the replacement", got[len(got)-1].apiKey)
		}
	})
}

// TestUserCredentialsOffIsUnchanged: with the mode off the broker reads no
// pod, keeps the org credential and its audit line has no user or
// credential attribute.
func TestUserCredentialsOffIsUnchanged(t *testing.T) {
	up := newCredentialUpstream(t)
	var gets atomic.Int64
	s, buf := credServer(t, up.URL, credServerOpts{pod: attributedPod(person)}, &gets)
	if rec := postAs(s.Handler(), "jobpod"); rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if gets.Load() != 0 {
		t.Errorf("pod reads = %d, want none", gets.Load())
	}
	line := lastAudit(t, buf)
	for _, key := range []string{"user", "credential"} {
		if _, ok := line[key]; ok {
			t.Errorf("audit line has %s while off", key)
		}
	}
	if got := up.requests(); got[0].apiKey != orgKey {
		t.Errorf("upstream saw %+v, want the org key", got)
	}
}

// TestAttributionReadOncePerVerdict: the pod is read with the verdict, not
// per request.
func TestAttributionReadOncePerVerdict(t *testing.T) {
	up := newCredentialUpstream(t)
	store := newFakeStore()
	store.put(personID, v1alpha1.ModelCredentialAPIKey, userKey)
	var gets atomic.Int64
	s, _ := credServer(t, up.URL, credServerOpts{mode: UserCredentialsEnforce, pod: attributedPod(person),
		store: store}, &gets)
	h := s.Handler()
	for range 3 {
		if rec := postAs(h, "jobpod"); rec.Code != http.StatusOK {
			t.Fatalf("status %d", rec.Code)
		}
	}
	if gets.Load() != 1 || store.reads != 1 {
		t.Errorf("pod reads, secret reads = %d, %d, want 1, 1", gets.Load(), store.reads)
	}
}

// TestReadinessIsTheOrgCredentialsAlone: an unsynced credential store and
// people's credentials never make the broker NotReady.
func TestReadinessIsTheOrgCredentialsAlone(t *testing.T) {
	up := newCredentialUpstream(t)
	store := newFakeStore()
	store.synced = false
	store.readErr = errors.New("down")
	s, _ := credServer(t, up.URL, credServerOpts{mode: UserCredentialsEnforce, store: store}, nil)
	rec := httptest.NewRecorder()
	s.HealthHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("readyz = %d, want 200", rec.Code)
	}
}

// TestAgentTrafficRejectionAsksForARecheck: a 401 on a person's
// credential queues a (throttled) recheck; it is never itself the
// verdict.
func TestAgentTrafficRejectionAsksForARecheck(t *testing.T) {
	up := newCredentialUpstream(t)
	up.status.Store(http.StatusUnauthorized)
	store := newFakeStore()
	v := store.put(personID, v1alpha1.ModelCredentialAPIKey, userKey)
	name := v1alpha1.ModelCredentialName(person, v1alpha1.ModelCredentialProviderAnthropic)
	store.update(name, func(r *v1alpha1.ModelCredential) {
		r.Status.Health, r.Status.SecretVersion = v1alpha1.ModelCredentialValid, v
	})
	var probes atomic.Int64
	s, _ := credServer(t, up.URL, credServerOpts{mode: UserCredentialsEnforce, pod: attributedPod(person),
		store: store, probe: func(context.Context, v1alpha1.ModelCredentialKind, string) ProbeResult {
			probes.Add(1)
			return ProbeRejected
		}}, nil)
	if rec := postAs(s.Handler(), "jobpod"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want the upstream's 401", rec.Code)
	}
	if got := store.record(name).Status.Health; got != v1alpha1.ModelCredentialValid {
		t.Fatalf("health = %s after agent traffic alone, want Valid", got)
	}
	s.creds.health.step(t.Context())
	if probes.Load() != 1 {
		t.Fatalf("probes = %d, want 1", probes.Load())
	}
	if got := store.record(name).Status.Health; got != v1alpha1.ModelCredentialRejected {
		t.Errorf("health = %s after the probe, want Rejected", got)
	}
}

func TestUserCredentialConfigValidation(t *testing.T) {
	target := mustTarget(t, "https://api.anthropic.com")
	key := Anthropic(target, "/dev/null", false)
	token := Anthropic(target, "/dev/null", true)
	store := newFakeStore()
	for _, tc := range []struct {
		name    string
		routes  map[string]Upstream
		uc      UserCredentialConfig
		wantErr string
	}{
		{"off needs nothing", map[string]Upstream{"bedrock": {Target: target}}, UserCredentialConfig{}, ""},
		{"enforce over an org key", map[string]Upstream{"anthropic": key},
			UserCredentialConfig{Mode: UserCredentialsEnforce, Store: store}, ""},
		{"no store", map[string]Upstream{"anthropic": key},
			UserCredentialConfig{Mode: UserCredentialsAttribute}, "credential store"},
		{"a cloud route", map[string]Upstream{"bedrock": {Target: target}},
			UserCredentialConfig{Mode: UserCredentialsEnforce, Store: store}, "anthropic route"},
		{"a second route", map[string]Upstream{"anthropic": key, "bedrock": {Target: target}},
			UserCredentialConfig{Mode: UserCredentialsEnforce, Store: store}, "anthropic route"},
		{"the subscription guard", map[string]Upstream{"anthropic": token},
			UserCredentialConfig{Mode: UserCredentialsAttribute, Store: store}, "setup-token"},
		{"the guard acknowledged", map[string]Upstream{"anthropic": token},
			UserCredentialConfig{Mode: UserCredentialsEnforce, Store: store, OrgCredentialIsPersonal: true}, ""},
		{"an unknown mode", map[string]Upstream{"anthropic": key},
			UserCredentialConfig{Mode: "sometimes", Store: store}, "not off"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(fakeReviews(nil), Config{AgentNamespace: testNamespace, AgentServiceAccount: testSA,
				Upstreams: tc.routes, UserCredentials: tc.uc}, nil)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("New = %v, want nil", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("New = %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestParseUserCredentialMode(t *testing.T) {
	for in, want := range map[string]UserCredentialMode{
		"": UserCredentialsOff, "off": UserCredentialsOff, " attribute ": UserCredentialsAttribute,
		"enforce": UserCredentialsEnforce,
	} {
		if got, err := ParseUserCredentialMode(in); err != nil || got != want {
			t.Errorf("ParseUserCredentialMode(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := ParseUserCredentialMode("on"); err == nil {
		t.Error("ParseUserCredentialMode(on) = nil error")
	}
}

// TestLedgerKeepsCredentialKindsApart: a pod whose person replaced an API
// key with a setup-token midway reports the tokens each paid for under its
// own kind, so a subscription's notional cost is never priced as money
// spent, nor the reverse.
func TestLedgerKeepsCredentialKindsApart(t *testing.T) {
	up := newCredentialUpstream(t)
	store := newFakeStore()
	store.put(personID, v1alpha1.ModelCredentialAPIKey, userKey)
	s, _ := credServer(t, up.URL, credServerOpts{mode: UserCredentialsEnforce, pod: attributedPod(person),
		store: store}, nil)
	h := s.Handler()
	if rec := postAs(h, "jobpod"); rec.Code != http.StatusOK {
		t.Fatalf("first: %d", rec.Code)
	}
	store.put(personID, v1alpha1.ModelCredentialOAuthToken, userTok)
	if rec := postAs(h, "jobpod"); rec.Code != http.StatusOK {
		t.Fatalf("second: %d", rec.Code)
	}
	pods := s.ledger.jobUsage(testJob)
	if len(pods) != 1 || len(pods[0].Models) != 2 {
		t.Fatalf("ledger = %+v, want one pod with two entries", pods)
	}
	for i, want := range []string{usagewire.CredentialAPIKey, usagewire.CredentialOAuthToken} {
		m := pods[0].Models[i]
		if m.Model != "claude-sonnet-5" || m.Credential != want || m.Input != 100 {
			t.Errorf("entry %d = %+v, want claude-sonnet-5 paid with %s", i, m, want)
		}
	}
}

// TestAttributeModeProbesStoredCredentials pins what the security notes
// say: under attribute, where the org pays for every run, the broker still
// reads a stored credential and probes it with no request of its owner's,
// and nothing it probed reaches the upstream proxy or the log.
func TestAttributeModeProbesStoredCredentials(t *testing.T) {
	up := newCredentialUpstream(t)
	store := newFakeStore()
	store.put(personID, v1alpha1.ModelCredentialAPIKey, userKey)
	var probed atomic.Int64
	s, buf := credServer(t, up.URL, credServerOpts{
		mode: UserCredentialsAttribute, store: store,
		probe: func(_ context.Context, _ v1alpha1.ModelCredentialKind, cred string) ProbeResult {
			if cred == userKey {
				probed.Add(1)
			}
			return ProbeValid
		},
	}, nil)
	store.fire(v1alpha1.ModelCredentialName(person, v1alpha1.ModelCredentialProviderAnthropic))
	s.creds.health.step(t.Context())
	if probed.Load() != 1 {
		t.Fatalf("probes = %d, want the stored credential probed once under attribute", probed.Load())
	}
	if store.reads == 0 {
		t.Error("the probe did not read the Secret's data")
	}
	if len(up.requests()) != 0 {
		t.Errorf("upstream saw %+v, want nothing: no run called", up.requests())
	}
	if strings.Contains(buf.String(), userKey) {
		t.Error("the probed credential reached the log")
	}
}

// TestOwnersShapeSetupTokenOrgCredential runs the recorded owner decision
// end to end: the org credential a setup-token (a bearer) with
// orgCredentialIsPersonal acknowledged. An org-paid run carries only the
// org bearer; a person's API key arrives as x-api-key with no org
// Authorization beside it; a person's setup-token replaces the org bearer;
// and under attribute a person's run still carries only the org bearer.
func TestOwnersShapeSetupTokenOrgCredential(t *testing.T) {
	orgBearer := arrived{authorization: "Bearer " + orgKey}
	for _, tc := range []struct {
		name       string
		mode       UserCredentialMode
		annotation string
		kind       v1alpha1.ModelCredentialKind
		credential string
		want       arrived
		reads      bool
	}{
		{"enforce, org", UserCredentialsEnforce, "org", v1alpha1.ModelCredentialAPIKey, userKey, orgBearer, false},
		{"enforce, a person's api key", UserCredentialsEnforce, person, v1alpha1.ModelCredentialAPIKey, userKey,
			arrived{apiKey: userKey}, true},
		{"enforce, a person's setup-token", UserCredentialsEnforce, person, v1alpha1.ModelCredentialOAuthToken,
			userTok, arrived{authorization: "Bearer " + userTok}, true},
		{"attribute, a person", UserCredentialsAttribute, person, v1alpha1.ModelCredentialAPIKey, userKey,
			orgBearer, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newCredentialUpstream(t)
			store := newFakeStore()
			store.put(personID, tc.kind, tc.credential)
			s, buf := credServer(t, up.URL, credServerOpts{
				mode: tc.mode, bearer: true, ack: true, pod: attributedPod(tc.annotation), store: store,
			}, nil)
			if rec := postAs(s.Handler(), "jobpod"); rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			if got := up.requests(); len(got) != 1 || got[0] != tc.want {
				t.Fatalf("upstream saw %+v, want %+v", got, tc.want)
			}
			if got := store.reads != 0; got != tc.reads {
				t.Errorf("secret read = %v, want %v", got, tc.reads)
			}
			if strings.Contains(buf.String(), orgKey) || strings.Contains(buf.String(), tc.credential) {
				t.Error("a credential reached the audit log")
			}
		})
	}
}
