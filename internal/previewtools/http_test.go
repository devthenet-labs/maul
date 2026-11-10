// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/devthenet-labs/maul/internal/httpstep"
)

// The relay's HTTP client, held to the guarantees slice 1's tester client
// was (its tests went with internal/previewtester in S4): no redirect
// followed, no cookie jar, no proxy, a bounded body, a classified transport
// failure, and a readiness wait that outlasts a policy not yet programmed.

// clientTarget is the Service address every client test sends to.
const clientTarget = "10.96.0.10:80"

// sendTo sends GET target through a client routed to comp.
func sendTo(t *testing.T, comp *fakeComponent, target string, o ClientOptions) reply {
	t.Helper()
	rt := routes{clientTarget: comp.srv.Listener.Addr().String()}
	if o.Dial == nil {
		o.Dial = rt.dial
	}
	r, err := NewClient(o).send(context.Background(), outbound{Addr: clientTarget, Method: http.MethodGet,
		Target: target})
	if err != nil {
		t.Fatalf("send %s: %v", target, err)
	}
	return r
}

// TestClientRedirectsNotFollowed: a redirect is the response, never a
// second request, wherever it points.
func TestClientRedirectsNotFollowed(t *testing.T) {
	for _, loc := range []string{"/elsewhere", "http://169.254.169.254/latest/meta-data/", "//other:80/x"} {
		t.Run(loc, func(t *testing.T) {
			comp := newFakeComponent(t, func(w http.ResponseWriter, r *http.Request, _ int) {
				http.Redirect(w, r, loc, http.StatusFound)
			})
			var mu sync.Mutex
			var dialled []string
			rt := routes{clientTarget: comp.srv.Listener.Addr().String()}
			dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
				mu.Lock()
				dialled = append(dialled, addr)
				mu.Unlock()
				return rt.dial(ctx, network, addr)
			}
			r := sendTo(t, comp, "/start", ClientOptions{Dial: dial})
			if r.Status != http.StatusFound || r.Transport != httpstep.TransportNone {
				t.Errorf("reply %+v, want the 302 itself", r)
			}
			if n := len(comp.requests()); n != 1 {
				t.Errorf("component saw %d requests, want 1", n)
			}
			for _, a := range dialled {
				if a != clientTarget {
					t.Errorf("dialled %s", a)
				}
			}
		})
	}
}

// TestClientBodyCap: the client reads at most MaxResponseBytes of a body;
// past it the reply is too large, however big the body, and the read stops.
func TestClientBodyCap(t *testing.T) {
	for _, tc := range []struct {
		name     string
		size     int
		tooLarge bool
	}{
		{"at the cap", MaxResponseBytes, false},
		{"one past", MaxResponseBytes + 1, true},
		{"huge", 64 << 20, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			comp := newFakeComponent(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
				chunk := []byte(strings.Repeat("a", 32<<10))
				for left := tc.size; left > 0; left -= len(chunk) {
					if _, err := w.Write(chunk[:min(left, len(chunk))]); err != nil {
						return
					}
				}
			})
			start := time.Now()
			r := sendTo(t, comp, "/", ClientOptions{})
			if took := time.Since(start); took > 5*time.Second {
				t.Errorf("reading took %v: the cap must stop the read", took)
			}
			if r.TooLarge != tc.tooLarge || len(r.Body) > MaxResponseBytes {
				t.Errorf("too large %v with %d bytes kept, want %v and at most %d", r.TooLarge, len(r.Body),
					tc.tooLarge, MaxResponseBytes)
			}
		})
	}
}

// TestClientNoCookieJar: a cookie a component sets is never sent back.
func TestClientNoCookieJar(t *testing.T) {
	comp := newFakeComponent(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "s1", Path: "/"})
		_, _ = io.WriteString(w, `{}`)
	})
	c := NewClient(ClientOptions{Dial: routes{clientTarget: comp.srv.Listener.Addr().String()}.dial})
	for _, target := range []string{"/a", "/b"} {
		if _, err := c.send(context.Background(), outbound{Addr: clientTarget, Method: http.MethodGet,
			Target: target}); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range comp.requests() {
		if r.Header.Get("Cookie") != "" {
			t.Errorf("%s carried Cookie %q", r.RequestURI, r.Header.Get("Cookie"))
		}
	}
}

// TestClientProxyEnvironmentIgnored: an HTTP_PROXY in the pod's
// environment cannot route the relay's requests anywhere else. net/http
// reads the proxy environment once per process, so the transport's
// configuration is pinned as well as its behaviour.
func TestClientProxyEnvironmentIgnored(t *testing.T) {
	if tr, ok := NewClient(ClientOptions{}).http.Transport.(*http.Transport); !ok || tr.Proxy != nil {
		t.Fatal("the relay's transport consults a proxy")
	}
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("http_proxy", "http://127.0.0.1:1")
	comp := newFakeComponent(t, func(w http.ResponseWriter, _ *http.Request, _ int) { _, _ = io.WriteString(w, "ok") })
	if r := sendTo(t, comp, "/", ClientOptions{}); r.Transport != httpstep.TransportNone || r.Status != 200 {
		t.Errorf("reply %+v", r)
	}
}

// TestClientTransportClasses: a request that gets no response is
// classified, never an error.
func TestClientTransportClasses(t *testing.T) {
	closed := func() string {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := l.Addr().String()
		_ = l.Close()
		return addr
	}
	for _, tc := range []struct {
		name    string
		handler func(w http.ResponseWriter, r *http.Request, n int)
		route   func(comp *fakeComponent) string
		want    httpstep.Transport
	}{
		{"refused", nil, func(*fakeComponent) string { return closed() }, httpstep.TransportRefused},
		{"dns", nil, func(*fakeComponent) string { return "" }, httpstep.TransportDNS},
		{"timeout", func(_ http.ResponseWriter, r *http.Request, _ int) {
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		}, func(c *fakeComponent) string { return c.srv.Listener.Addr().String() }, httpstep.TransportTimeout},
		{"reset", func(w http.ResponseWriter, _ *http.Request, _ int) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
		}, func(c *fakeComponent) string { return c.srv.Listener.Addr().String() }, httpstep.TransportReset},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := tc.handler
			if h == nil {
				h = func(http.ResponseWriter, *http.Request, int) {}
			}
			comp := newFakeComponent(t, h)
			rt := routes{}
			if to := tc.route(comp); to != "" {
				rt[clientTarget] = to
			}
			r := sendTo(t, comp, "/", ClientOptions{Dial: rt.dial, Timeout: 200 * time.Millisecond})
			if r.Transport != tc.want || r.Status != 0 {
				t.Errorf("reply %+v, want transport %q and no status", r, tc.want)
			}
		})
	}
}

// refusingDial refuses the first n dials of every address with
// ECONNREFUSED, as kube-router's REJECT does before a new Pod's
// NetworkPolicy is programmed, then routes through next.
func refusingDial(n int, next DialFunc) DialFunc {
	var mu sync.Mutex
	seen := map[string]int{}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		mu.Lock()
		seen[addr]++
		k := seen[addr]
		mu.Unlock()
		if k <= n {
			return nil, &net.OpError{Op: "dial", Net: network, Err: syscall.ECONNREFUSED}
		}
		return next(ctx, network, addr)
	}
}

// TestAwaitTargetsWaitsThroughRefusals: the live k3s evidence. A slot
// policy programmed after the tool pod starts refuses its first
// connections; the wait outlasts the refusals for each target and records
// them, while a target that never accepts is recorded unreached within the
// window.
func TestAwaitTargetsWaitsThroughRefusals(t *testing.T) {
	comp := newFakeComponent(t, func(http.ResponseWriter, *http.Request, int) {})
	rt := routes{clientTarget: comp.srv.Listener.Addr().String(), "10.96.0.11:80": comp.srv.Listener.Addr().String()}
	c := NewClient(ClientOptions{Dial: refusingDial(3, rt.dial), ReadinessBackoff: 10 * time.Millisecond})
	waits := c.awaitTargets(context.Background(), map[string]string{"web": "10.96.0.11:80", "api": clientTarget},
		5*time.Second)
	if len(waits) != 2 || waits[0].Target != "api" || waits[1].Target != "web" {
		t.Fatalf("waits %+v, want api then web", waits)
	}
	for _, w := range waits {
		if !w.Reached() || w.Attempts != 4 || w.Refused != 3 {
			t.Errorf("wait %+v, want reached on the fourth attempt after three refusals", w)
		}
	}

	never := NewClient(ClientOptions{Dial: refusingDial(1<<30, rt.dial), ReadinessBackoff: 10 * time.Millisecond})
	start := time.Now()
	waits = never.awaitTargets(context.Background(), map[string]string{"api": clientTarget}, 200*time.Millisecond)
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the wait took %s past a 200ms window", took)
	}
	if len(waits) != 1 || waits[0].Reached() || waits[0].Refused == 0 {
		t.Errorf("waits %+v, want api unreached and its refusals recorded", waits)
	}
}

// TestReadinessWindowInsideTheBudget: the window is a quarter of the
// evaluation's wall clock, at most ReadinessWindowMax.
func TestReadinessWindowInsideTheBudget(t *testing.T) {
	for _, budget := range []time.Duration{time.Minute, 10 * time.Minute, time.Hour} {
		if w := ReadinessWindow(budget); w <= 0 || w > budget/4 || w > ReadinessWindowMax {
			t.Errorf("budget %s: window %s", budget, w)
		}
	}
	if w := ReadinessWindow(8 * time.Minute); w != ReadinessWindowMax {
		t.Errorf("an 8m budget's window is %s, want %s", w, ReadinessWindowMax)
	}
	if w := ReadinessWindow(time.Minute); w != 15*time.Second {
		t.Errorf("a 1m budget's window is %s, want 15s", w)
	}
}

// TestReadinessBackoffNeverZero: a backoff that is not positive is the
// default, never zero, so the wait never spins.
func TestReadinessBackoffNeverZero(t *testing.T) {
	for _, b := range []time.Duration{0, -1, -time.Second} {
		if got := NewClient(ClientOptions{ReadinessBackoff: b}).backoff; got != readinessBackoffMin {
			t.Errorf("ReadinessBackoff %s: backoff %s, want %s", b, got, readinessBackoffMin)
		}
	}
	if got := NewClient(ClientOptions{ReadinessBackoff: 5 * time.Millisecond}).backoff; got != 5*time.Millisecond {
		t.Errorf("backoff %s, want 5ms", got)
	}
}
