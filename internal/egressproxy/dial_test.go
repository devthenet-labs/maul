// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
)

func TestAddrPolicy(t *testing.T) {
	deny, err := ParseDenyCIDRs([]string{"8.8.8.0/24", "fd00::/8"})
	if err != nil {
		t.Fatal(err)
	}
	prod := AddrPolicy{Deny: deny}
	tests := []struct {
		addr string
		ok   bool
	}{
		{"8.8.4.4:443", true},
		{"142.250.1.1:443", true},
		{"8.8.4.4:80", false},
		{"8.8.4.4:8443", false},
		{"8.8.8.8:443", false}, // a --deny-cidrs entry
		{"10.0.0.5:443", false},
		{"169.254.169.254:443", false}, // IMDS
		{"169.254.170.23:443", false},  // EKS Pod Identity
		{"100.64.1.1:443", false},
		{"127.0.0.1:443", false},
		{"0.0.0.0:443", false},
		{"172.16.5.4:443", false},
		{"192.168.1.1:443", false},
		{"192.0.0.8:443", false},
		{"192.0.2.1:443", false},
		{"198.18.0.1:443", false},
		{"198.51.100.1:443", false},
		{"203.0.113.9:443", false},
		{"224.0.0.1:443", false},
		{"240.0.0.1:443", false},
		{"255.255.255.255:443", false},
		{"[::1]:443", false},
		{"[2606:4700::1111]:443", false},
		{"[::ffff:8.8.4.4]:443", false},
	}
	for _, tt := range tests {
		err := prod.Check(netip.MustParseAddrPort(tt.addr))
		if (err == nil) != tt.ok || (err != nil && !errors.Is(err, ErrDeniedAddress)) {
			t.Errorf("Check(%s) = %v, want ok=%v", tt.addr, err, tt.ok)
		}
	}
	// The test policy admits loopback on any port, and nothing else new.
	test := AddrPolicy{testLoopback: true}
	if err := test.Check(netip.MustParseAddrPort("127.0.0.1:34567")); err != nil {
		t.Errorf("test policy refused loopback: %v", err)
	}
	if err := test.Check(netip.MustParseAddrPort("10.0.0.5:443")); err == nil {
		t.Error("test policy admitted 10.0.0.5")
	}
	if _, err := ParseDenyCIDRs([]string{"10.0.0.0/33"}); err == nil {
		t.Error("ParseDenyCIDRs admitted a bad CIDR")
	}
}

// TestControlJudgesTheConnectedAddress: the dialer's Control hook, which
// sees the address actually connected, applies the policy.
func TestControlJudgesTheConnectedAddress(t *testing.T) {
	d := newGuardedDialer(AddrPolicy{}, nil)
	for addr, ok := range map[string]bool{
		"8.8.4.4:443":         true,
		"10.0.0.5:443":        false,
		"169.254.169.254:443": false,
		"8.8.4.4:80":          false,
		"[::1]:443":           false,
		"not-an-address":      false,
	} {
		if err := d.control("tcp4", addr, nil); (err == nil) != ok {
			t.Errorf("control(%s) = %v, want ok=%v", addr, err, ok)
		}
	}
}

// fixedLookup answers each lookup with the next entry of answers (the
// last repeating), recording how often it was asked.
type fixedLookup struct {
	mu      sync.Mutex
	answers [][]netip.Addr
	calls   int
}

func (f *fixedLookup) lookup(_ context.Context, _ string) ([]netip.Addr, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := min(f.calls, len(f.answers)-1)
	f.calls++
	return f.answers[i], nil
}

func addrs(s ...string) []netip.Addr {
	out := make([]netip.Addr, len(s))
	for i, a := range s {
		out[i] = netip.MustParseAddr(a)
	}
	return out
}

// TestDialerRefusesNonPublicAnswers: whatever the fixed upstream host
// resolves to, a private, link-local, shared, loopback, IPv6 or denied
// address is refused with ErrDeniedAddress, answered 502 denied_address,
// and nothing is dialled.
func TestDialerRefusesNonPublicAnswers(t *testing.T) {
	deny, _ := ParseDenyCIDRs([]string{"8.8.8.0/24"})
	for _, a := range []string{"10.0.0.5", "169.254.169.254", "169.254.170.23", "100.64.1.1", "127.0.0.1", "::1",
		"8.8.8.8"} {
		fl := &fixedLookup{answers: [][]netip.Addr{addrs(a)}}
		srv, err := New(Config{
			Profiles: testProfiles(t),
			GoProxy:  mustURL(t, "https://proxy.test"),
			Policy:   AddrPolicy{Deny: deny},
			lookup:   fl.lookup,
		}, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatal(err)
		}
		_, err = srv.client.Transport.(*http.Transport).DialContext(t.Context(), "tcp", "proxy.test:443")
		if !errors.Is(err, ErrDeniedAddress) {
			t.Errorf("dial with %s = %v, want ErrDeniedAddress", a, err)
		}
		h, _ := srv.Handler("go")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/go/github.com/google/uuid/@v/list", nil))
		if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), ReasonDeniedAddress) {
			t.Errorf("request with %s = %d %q, want 502 denied_address", a, rec.Code, rec.Body.String())
		}
	}
}

// TestDialerRefusesOtherPorts: an upstream on any port but 443 is refused
// even at a public address.
func TestDialerRefusesOtherPorts(t *testing.T) {
	fl := &fixedLookup{answers: [][]netip.Addr{addrs("8.8.4.4")}}
	d := newGuardedDialer(AddrPolicy{}, fl.lookup)
	if _, err := d.DialContext(t.Context(), "tcp", "proxy.test:8443"); !errors.Is(err, ErrDeniedAddress) {
		t.Errorf("dial :8443 = %v, want ErrDeniedAddress", err)
	}
}

// TestDialerNeverRetriesAfterADenial: a denied address ends the dial, even
// with an admissible address after it.
func TestDialerNeverRetriesAfterADenial(t *testing.T) {
	up := failingUpstream(t)
	port := mustURL(t, up.URL).Port()
	fl := &fixedLookup{answers: [][]netip.Addr{addrs("10.0.0.5", "127.0.0.1")}}
	d := newGuardedDialer(AddrPolicy{testLoopback: true}, fl.lookup)
	if _, err := d.DialContext(t.Context(), "tcp", "upstream.test:"+port); !errors.Is(err, ErrDeniedAddress) {
		t.Errorf("dial = %v, want ErrDeniedAddress", err)
	}
}

// TestDialerRebinding: a name that resolves to an admissible address and
// then, on the next connection, to a private one is refused the second
// time: every connection is judged on what it actually dials.
func TestDialerRebinding(t *testing.T) {
	goUp := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Connection", "close") // every request dials anew
		_, _ = w.Write([]byte("ok"))
	})
	fl := &fixedLookup{answers: [][]netip.Addr{addrs("127.0.0.1"), addrs("10.0.0.5")}}
	u := mustURL(t, goUp.URL)
	u.Host = "upstream.test:" + u.Port()
	srv, err := New(Config{
		Profiles: testProfiles(t),
		GoProxy:  u,
		Policy:   AddrPolicy{testLoopback: true},
		lookup:   fl.lookup,
	}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	h, _ := srv.Handler("go")
	get := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/go/github.com/google/uuid/@v/list", nil))
		return rec
	}
	if rec := get(); rec.Code != http.StatusOK {
		t.Fatalf("first request = %d %q, want 200", rec.Code, rec.Body.String())
	}
	if rec := get(); rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), ReasonDeniedAddress) {
		t.Errorf("rebound request = %d %q, want 502 denied_address", rec.Code, rec.Body.String())
	}
	if len(goUp.Hits()) != 1 {
		t.Errorf("upstream saw %d requests, want 1", len(goUp.Hits()))
	}
	if fl.calls != 2 {
		t.Errorf("lookup called %d times, want 2", fl.calls)
	}
}

// TestUpstreamURLJoin: the outbound URL is the fixed base joined with the
// validated path, nothing else.
func TestUpstreamURLJoin(t *testing.T) {
	for _, tt := range []struct{ base, path, want string }{
		{"https://proxy.golang.org", "github.com/google/uuid/@v/list",
			"https://proxy.golang.org/github.com/google/uuid/@v/list"},
		{"https://proxy.example/base/", "x.com/@latest", "https://proxy.example/base/x.com/@latest"},
		{"https://proxy.golang.org", "github.com/!burnt!sushi/toml/@v/list",
			"https://proxy.golang.org/github.com/%21burnt%21sushi/toml/@v/list"},
	} {
		base, _ := url.Parse(tt.base)
		if got := outboundURL(base, tt.path).String(); got != tt.want {
			t.Errorf("outboundURL(%s, %s) = %s, want %s", tt.base, tt.path, got, tt.want)
		}
	}
}
