// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package awsca

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"testing/quick"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/codeartifact"
	"github.com/aws/smithy-go"

	"github.com/bitwise-media-group/patchy/internal/egressproxy"
	"github.com/bitwise-media-group/patchy/internal/netprofile"
)

const sentinel = "s3cr3t-TOKEN-1234"

// fakeAPI answers GetAuthorizationToken from a settable script and records
// every input.
type fakeAPI struct {
	mu     sync.Mutex
	answer func() (*codeartifact.GetAuthorizationTokenOutput, error)
	inputs []codeartifact.GetAuthorizationTokenInput
}

func (f *fakeAPI) GetAuthorizationToken(_ context.Context, in *codeartifact.GetAuthorizationTokenInput,
	_ ...func(*codeartifact.Options)) (*codeartifact.GetAuthorizationTokenOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inputs = append(f.inputs, *in)
	return f.answer()
}

func (f *fakeAPI) set(answer func() (*codeartifact.GetAuthorizationTokenOutput, error)) {
	f.mu.Lock()
	f.answer = answer
	f.mu.Unlock()
}

func (f *fakeAPI) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.inputs)
}

func token(tok string, exp time.Time) func() (*codeartifact.GetAuthorizationTokenOutput, error) {
	return func() (*codeartifact.GetAuthorizationTokenOutput, error) {
		return &codeartifact.GetAuthorizationTokenOutput{AuthorizationToken: &tok, Expiration: &exp}, nil
	}
}

func failing(code string) func() (*codeartifact.GetAuthorizationTokenOutput, error) {
	return func() (*codeartifact.GetAuthorizationTokenOutput, error) {
		return nil, &smithy.GenericAPIError{Code: code, Message: "detail " + sentinel}
	}
}

// harness drives one registry's loop with a fake clock and timer.
type harness struct {
	t      *testing.T
	api    *fakeAPI
	m      *Minter
	delays chan time.Duration
	fire   chan time.Time
	log    *syncBuffer
	now    time.Time
	nowMu  sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
	events []string
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

var t0 = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

func newHarness(t *testing.T, reg Registry, first func() (*codeartifact.GetAuthorizationTokenOutput, error)) *harness {
	t.Helper()
	h := &harness{t: t, api: &fakeAPI{answer: first}, delays: make(chan time.Duration, 16),
		fire: make(chan time.Time), log: &syncBuffer{}, now: t0, done: make(chan struct{})}
	var evMu sync.Mutex
	m, err := New([]Registry{reg}, func(string) (API, error) { return h.api, nil }, Options{
		Now: h.clock,
		After: func(d time.Duration) <-chan time.Time {
			h.delays <- d
			return h.fire
		},
		Jitter: func() float64 { return 0.5 },
		Log:    slog.New(slog.NewTextHandler(h.log, nil)),
		OnRefresh: func(r, result string) {
			evMu.Lock()
			h.events = append(h.events, r+"="+result)
			evMu.Unlock()
		},
		AddressClass: func() string { return ClassPublic },
	})
	if err != nil {
		t.Fatal(err)
	}
	h.m = m
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() {
		_ = m.Run(ctx)
		close(h.done)
	}()
	t.Cleanup(h.stop)
	return h
}

func (h *harness) clock() time.Time {
	h.nowMu.Lock()
	defer h.nowMu.Unlock()
	return h.now
}

func (h *harness) setNow(t time.Time) {
	h.nowMu.Lock()
	h.now = t
	h.nowMu.Unlock()
}

func (h *harness) stop() {
	h.cancel()
	<-h.done
}

// next waits for the loop's next timer and returns its delay.
func (h *harness) next() time.Duration {
	h.t.Helper()
	select {
	case d := <-h.delays:
		return d
	case <-time.After(5 * time.Second):
		h.t.Fatal("the mint loop set no timer")
		return 0
	}
}

func (h *harness) header() (string, error) {
	c, err := h.m.Get("ca")
	return c.Authorization(), err
}

func TestMintScheduleBackoffAndExpiry(t *testing.T) {
	reg := Registry{Name: "ca", Ecosystem: netprofile.EcosystemNPM, Domain: "patchy-gate", DomainOwner: "377946145366",
		Region: "us-east-1"}
	h := newHarness(t, reg, token(sentinel, t0.Add(12*time.Hour)))

	// First mint at once; refresh at the half-life, jittered +5%.
	if d := h.next(); d != time.Duration(float64(6*time.Hour)*1.05) {
		t.Fatalf("first refresh in %s, want 6h +5%%", d)
	}
	if got, err := h.header(); err != nil || got != "Bearer "+sentinel {
		t.Fatalf("Get = %v", err)
	}
	if c, _ := h.m.Get("ca"); c.Kind() != egressproxy.CredentialCodeArtifact {
		t.Fatalf("kind = %s", c.Kind())
	}
	in := h.api.inputs[0]
	if *in.Domain != "patchy-gate" || *in.DomainOwner != "377946145366" || *in.DurationSeconds != 43200 {
		t.Fatalf("input = %+v", in)
	}
	if exp, ok := h.m.ExpiresAt("ca"); !ok || !exp.Equal(t0.Add(12*time.Hour)) {
		t.Fatalf("ExpiresAt = %s %v", exp, ok)
	}

	// Refreshes fail: back off 30s doubling to 5m, serving the current
	// token meanwhile.
	h.api.set(failing("AccessDeniedException"))
	h.setNow(t0.Add(6 * time.Hour))
	for _, want := range []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute,
		5 * time.Minute, 5 * time.Minute} {
		h.fire <- h.clock()
		if d := h.next(); d != want {
			t.Fatalf("backoff = %s, want %s", d, want)
		}
		if got, err := h.header(); err != nil || got != "Bearer "+sentinel {
			t.Fatalf("the current token stopped serving before its expiry: %v", err)
		}
	}
	if !strings.Contains(h.log.String(), "code=AccessDeniedException endpoint_class=public") {
		t.Fatalf("log:\n%s", h.log.String())
	}

	// Past expiry: unavailable (the route's 503).
	h.setNow(t0.Add(12 * time.Hour))
	if _, err := h.header(); !errors.Is(err, egressproxy.ErrCredentialUnavailable) {
		t.Fatalf("Get after expiry = %v, want ErrCredentialUnavailable", err)
	}

	// Recovery: a fresh token, and the schedule is back to the half-life.
	h.api.set(token(sentinel+"-2", t0.Add(24*time.Hour)))
	h.fire <- h.clock()
	if d := h.next(); d != time.Duration(float64(6*time.Hour)*1.05) {
		t.Fatalf("refresh after recovery in %s", d)
	}
	if got, err := h.header(); err != nil || got != "Bearer "+sentinel+"-2" {
		t.Fatalf("Get after recovery = %v", err)
	}
	h.stop()
	assertLifecycle(t, h)
}

// assertLifecycle checks the schedule test's lines and events: two mints
// around six failures, the expiry on the line, and no token anywhere.
func assertLifecycle(t *testing.T, h *harness) {
	t.Helper()
	log := h.log.String()
	if strings.Count(log, "msg=credential_minted") != 2 || strings.Count(log, "msg=credential_mint_failed") != 6 {
		t.Fatalf("log:\n%s", log)
	}
	if !strings.Contains(log, "expires_at=2026-10-16T00:00:00Z") {
		t.Fatalf("credential_minted does not carry expires_at:\n%s", log)
	}
	assertNoSecret(t, log)
	if got := strings.Join(h.events, ","); !strings.HasPrefix(got, "ca=minted,ca=mint_failed") ||
		!strings.HasSuffix(got, "ca=minted") {
		t.Fatalf("refresh events = %s", got)
	}
}

func TestMintPyPIBasic(t *testing.T) {
	reg := Registry{Name: "ca", Ecosystem: netprofile.EcosystemPyPI, Domain: "d1", DomainOwner: "377946145366",
		Region: "us-east-1", Duration: 900 * time.Second}
	h := newHarness(t, reg, token(sentinel, t0.Add(15*time.Minute)))
	// A 15-minute token refreshes at a quarter of its life, not at once.
	if d := h.next(); d != time.Duration(float64(225*time.Second)*1.05) {
		t.Fatalf("refresh in %s", d)
	}
	got, err := h.header()
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("aws:"+sentinel))
	if err != nil || got != want {
		t.Fatalf("Get = %v", err)
	}
	if *h.api.inputs[0].DurationSeconds != 900 {
		t.Fatalf("duration = %d", *h.api.inputs[0].DurationSeconds)
	}
}

func TestMintBadAnswers(t *testing.T) {
	for name, answer := range map[string]func() (*codeartifact.GetAuthorizationTokenOutput, error){
		"empty": func() (*codeartifact.GetAuthorizationTokenOutput, error) {
			return &codeartifact.GetAuthorizationTokenOutput{}, nil
		},
		"control char": token("a\r\nb", t0.Add(time.Hour)),
		"denied dial": func() (*codeartifact.GetAuthorizationTokenOutput, error) {
			// An output beside an error is ignored.
			out, _ := token(sentinel, t0.Add(time.Hour))()
			return out, fmt.Errorf("op: %w", egressproxy.ErrDeniedAddress)
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, Registry{Name: "ca", Ecosystem: "npm", Domain: "d", DomainOwner: "1", Region: "r"}, answer)
			if d := h.next(); d != BackoffMin {
				t.Fatalf("delay = %s", d)
			}
			if _, err := h.header(); !errors.Is(err, egressproxy.ErrCredentialUnavailable) {
				t.Fatalf("Get = %v", err)
			}
		})
	}
}

func TestMintNoExpirationUsesDuration(t *testing.T) {
	tok := sentinel
	h := newHarness(t, Registry{Name: "ca", Ecosystem: "npm", Domain: "d", DomainOwner: "1", Region: "r",
		Duration: time.Hour}, func() (*codeartifact.GetAuthorizationTokenOutput, error) {
		return &codeartifact.GetAuthorizationTokenOutput{AuthorizationToken: &tok}, nil
	})
	h.next()
	if exp, _ := h.m.ExpiresAt("ca"); !exp.Equal(t0.Add(time.Hour)) {
		t.Fatalf("expiry = %s", exp)
	}
}

func TestInvalidateRemintsRateLimited(t *testing.T) {
	h := newHarness(t, Registry{Name: "ca", Ecosystem: "npm", Domain: "d", DomainOwner: "1", Region: "r"},
		token(sentinel, t0.Add(12*time.Hour)))
	h.next()
	h.m.Invalidate("ca") // a 401: mint again without waiting for the timer
	h.next()
	if h.api.calls() != 2 {
		t.Fatalf("calls = %d, want 2", h.api.calls())
	}
	e := h.m.entries["ca"]
	h.m.Invalidate("ca") // within MinRemint: nothing
	if len(e.wake) != 0 {
		t.Fatal("a second Invalidate within MinRemint woke the loop")
	}
	h.setNow(t0.Add(MinRemint))
	h.m.Invalidate("ca")
	h.next()
	if h.api.calls() != 3 {
		t.Fatalf("calls = %d, want 3", h.api.calls())
	}
	h.m.Invalidate("other") // harmless
}

func TestGetBeforeMint(t *testing.T) {
	m, err := New([]Registry{{Name: "ca", Ecosystem: "npm", Domain: "d", DomainOwner: "1", Region: "r"}},
		func(string) (API, error) { return &fakeAPI{}, nil }, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []string{"ca", "unknown"} {
		if _, err := m.Get(r); !errors.Is(err, egressproxy.ErrCredentialUnavailable) {
			t.Errorf("Get(%s) = %v", r, err)
		}
		if _, ok := m.ExpiresAt(r); ok {
			t.Errorf("ExpiresAt(%s) reports a token", r)
		}
	}
}

func TestNewRefuses(t *testing.T) {
	api := func(string) (API, error) { return &fakeAPI{}, nil }
	ok := Registry{Name: "ca", Ecosystem: "npm", Domain: "d", DomainOwner: "1", Region: "r"}
	for name, regs := range map[string][]Registry{
		"go":        {{Name: "ca", Ecosystem: "go", Domain: "d", DomainOwner: "1", Region: "r"}},
		"bad name":  {{Name: "CA", Ecosystem: "npm", Domain: "d", DomainOwner: "1", Region: "r"}},
		"no domain": {{Name: "ca", Ecosystem: "npm", DomainOwner: "1", Region: "r"}},
		"duplicate": {ok, ok},
	} {
		if _, err := New(regs, api, Options{}); err == nil {
			t.Errorf("%s: New accepted it", name)
		}
	}
	if _, err := New([]Registry{ok}, func(string) (API, error) { return nil, errors.New("no") }, Options{}); err == nil {
		t.Error("New ignored the API constructor's error")
	}
	// One API per region.
	regions := map[string]int{}
	_, err := New([]Registry{ok, {Name: "cb", Ecosystem: "pypi", Domain: "d", DomainOwner: "1", Region: "r"},
		{Name: "cc", Ecosystem: "pypi", Domain: "d", DomainOwner: "1", Region: "s"}},
		func(r string) (API, error) { regions[r]++; return &fakeAPI{}, nil }, Options{})
	if err != nil || regions["r"] != 1 || regions["s"] != 1 {
		t.Fatalf("regions = %v, %v", regions, err)
	}
}

func TestFromNetprofile(t *testing.T) {
	r := netprofile.Registry{Name: "ca", Ecosystem: "pypi", Auth: netprofile.Auth{Type: netprofile.AuthCodeArtifact,
		CodeArtifact: &netprofile.CodeArtifact{Domain: "d", DomainOwner: "377946145366", Region: "us-east-1"}}}
	got, err := FromNetprofile(r)
	want := Registry{Name: "ca", Ecosystem: "pypi", Domain: "d", DomainOwner: "377946145366", Region: "us-east-1",
		Duration: 12 * time.Hour}
	if err != nil || got != want {
		t.Fatalf("FromNetprofile = %+v, %v", got, err)
	}
	if _, err := FromNetprofile(netprofile.Registry{Name: "x", Auth: netprofile.Auth{Type: "bearer"}}); err == nil {
		t.Fatal("a bearer registry converted")
	}
}

func TestRefreshDelayExamples(t *testing.T) {
	for _, tt := range []struct {
		lifetime, want time.Duration
	}{
		{12 * time.Hour, 6 * time.Hour},
		{time.Hour, 30 * time.Minute},
		{30 * time.Minute, 15 * time.Minute},
		{20 * time.Minute, 5 * time.Minute}, // expiry-15m, above the quarter floor
		{15 * time.Minute, 225 * time.Second},
		{0, BackoffMin},
		{-time.Minute, BackoffMin},
	} {
		if got := RefreshDelay(tt.lifetime, 0); got != tt.want {
			t.Errorf("RefreshDelay(%s) = %s, want %s", tt.lifetime, got, tt.want)
		}
	}
}

// TestPropertyRefreshDelay: for every lifetime CodeArtifact issues
// (15 min to 12 h) and any jitter, the refresh lands within ±10% of a base
// between a quarter and a half of the lifetime, so always well before
// expiry, and at least RefreshMargin (less jitter) before it once the
// lifetime allows.
func TestPropertyRefreshDelay(t *testing.T) {
	r := rand.New(rand.NewSource(20261015))
	prop := func() bool {
		lifetime := 15*time.Minute + time.Duration(r.Int63n(int64(11*time.Hour+45*time.Minute)))
		j := 2*r.Float64() - 1
		d := RefreshDelay(lifetime, j)
		lo, hi := time.Duration(float64(lifetime/4)*0.9), time.Duration(float64(lifetime/2)*1.1)
		if d < lo || d > hi || d >= lifetime {
			t.Logf("lifetime %s jitter %f: delay %s outside [%s, %s]", lifetime, j, d, lo, hi)
			return false
		}
		base := RefreshDelay(lifetime, 0)
		if lifetime >= 2*RefreshMargin && lifetime-base < RefreshMargin {
			t.Logf("lifetime %s: base %s leaves under %s", lifetime, base, RefreshMargin)
			return false
		}
		return true
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 5000, Rand: r}); err != nil {
		t.Fatal(err)
	}
}

func TestBackoff(t *testing.T) {
	for n, want := range map[int]time.Duration{1: 30 * time.Second, 2: time.Minute, 3: 2 * time.Minute,
		4: 4 * time.Minute, 5: 5 * time.Minute, 50: 5 * time.Minute} {
		if got := Backoff(n); got != want {
			t.Errorf("Backoff(%d) = %s, want %s", n, got, want)
		}
	}
}

func TestErrorCode(t *testing.T) {
	for want, err := range map[string]error{
		"AccessDeniedException": fmt.Errorf("op: %w", &smithy.GenericAPIError{Code: "AccessDeniedException"}),
		"DeniedAddress":         fmt.Errorf("dial: %w", egressproxy.ErrDeniedAddress),
		"Timeout":               fmt.Errorf("x: %w", context.DeadlineExceeded),
		"EmptyToken":            errEmptyToken,
		"InvalidToken":          fmt.Errorf("%w: x", errInvalidToken),
		"RequestFailure":        errors.New("connection reset " + sentinel),
	} {
		if got := ErrorCode(err); got != want {
			t.Errorf("ErrorCode(%v) = %s, want %s", err, got, want)
		}
	}
}

func assertNoSecret(t *testing.T, out string) {
	t.Helper()
	for _, f := range []string{sentinel, base64.StdEncoding.EncodeToString([]byte("aws:" + sentinel)), "detail"} {
		if strings.Contains(out, f) {
			t.Fatalf("output holds %q:\n%s", f, out)
		}
	}
}
