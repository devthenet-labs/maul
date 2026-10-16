// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	toolscache "k8s.io/client-go/tools/cache"
	crcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/previewauth"
	"github.com/bitwise-media-group/patchy/internal/previewauth/httpapi"
)

// gatewayArgs are staticArgs with an issuer outside the host suffix, as the
// relay requires, and the gateway on.
func gatewayArgs(t *testing.T, extra ...string) []string {
	t.Helper()
	return staticArgs(t, append([]string{"--preview-auth-issuer", "https://auth.example.com",
		"--preview-auth-gateway"}, extra...)...)
}

// TestGatewayOffByDefault: without the flag nothing of the gateway is
// loaded, and the server keeps its timeouts.
func TestGatewayOffByDefault(t *testing.T) {
	st, err := loadStatic(serveOpts(t, staticArgs(t)...))
	if err != nil {
		t.Fatal(err)
	}
	if st.gateway != nil {
		t.Fatalf("gateway loaded without --preview-auth-gateway: %+v", st.gateway)
	}
	srv := newHTTPServer(":0", http.NotFoundHandler(), false)
	if srv.ReadTimeout != 30*time.Second || srv.WriteTimeout != 30*time.Second {
		t.Errorf("relay server timeouts changed: %s %s", srv.ReadTimeout, srv.WriteTimeout)
	}
}

func TestLoadGateway(t *testing.T) {
	st, err := loadStatic(serveOpts(t, gatewayArgs(t,
		"--preview-auth-gateway-forwarded-hops", "1", "--preview-auth-gateway-inbound-cidrs", "10.1.2.3/8, 192.0.2.0/24",
		"--preview-auth-gateway-body-limit", "1024", "--preview-auth-gateway-session-timeout", "10m",
		"--preview-auth-gateway-hsts", "--preview-auth-rate-per-second", "2", "--preview-auth-review-ttl", "15s")...))
	if err != nil {
		t.Fatal(err)
	}
	g := st.gateway
	if g == nil || g.signInUpstream != nil || g.cfg.SignInUpstream != nil {
		t.Fatalf("gateway = %+v", g)
	}
	c := g.cfg
	if c.Hosts.SignIn() != "auth.example.com" || c.Hosts.Suffix() != "previews.example.com" {
		t.Errorf("hosts = %s, %s", c.Hosts.SignIn(), c.Hosts.Suffix())
	}
	if !slices.Equal(c.InboundCIDRs, []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("192.0.2.0/24")}) {
		t.Errorf("inbound CIDRs = %v", c.InboundCIDRs)
	}
	if c.BodyLimit != 1024 || !c.HSTS || c.RateLimit.ForwardedHops != 1 || c.RateLimit.PerSecond != 2 ||
		c.Recheck != 15*time.Second || c.Lifetimes.HostSession != 10*time.Minute || c.Transport == nil {
		t.Errorf("gateway config = %+v", c)
	}
	st, err = loadStatic(serveOpts(t, gatewayArgs(t, "--preview-auth-gateway-sign-in-host", "signin.example.com")...))
	if err != nil {
		t.Fatal(err)
	}
	if st.gateway.signInUpstream == nil || st.gateway.cfg.SignInUpstream == nil ||
		st.gateway.cfg.Hosts.SignIn() != "signin.example.com" {
		t.Errorf("a sign-in host of its own has no Dex client: %+v", st.gateway)
	}
}

func TestGatewayServerTimeouts(t *testing.T) {
	srv := newHTTPServer(":0", http.NotFoundHandler(), true)
	if srv.ReadTimeout != 0 || srv.WriteTimeout != 0 || srv.ReadHeaderTimeout != 10*time.Second {
		t.Errorf("gateway server timeouts: %s %s %s", srv.ReadTimeout, srv.WriteTimeout, srv.ReadHeaderTimeout)
	}
}

func TestLoadGatewayRefuses(t *testing.T) {
	for _, tt := range []struct {
		name    string
		extra   []string
		wantErr string
	}{
		{"sign-in host under the suffix", []string{"--preview-auth-gateway-sign-in-host", "x.previews.example.com"},
			"sits under the preview host suffix"},
		{"bad sign-in host", []string{"--preview-auth-gateway-sign-in-host", "Auth.example.com"}, "sign-in host"},
		{"bad CIDR", []string{"--preview-auth-gateway-inbound-cidrs", "10.0.0.0/33"}, "inbound-cidrs"},
		{"zero body limit", []string{"--preview-auth-gateway-body-limit", "0"}, "body-limit"},
		{"negative hops", []string{"--preview-auth-gateway-forwarded-hops", "-1"}, "forwarded-hops"},
		{"zero dial timeout", []string{"--preview-auth-gateway-dial-timeout", "0s"}, "timeout"},
		{"long session", []string{"--preview-auth-gateway-session-timeout", "13h"}, "host session"},
		{"short handoff", []string{"--preview-auth-gateway-handoff-ttl", "1s"}, "handoff"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadStatic(serveOpts(t, gatewayArgs(t, tt.extra...)...))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("loadStatic error = %v, want %q", err, tt.wantErr)
			}
		})
	}
	// Off, the gateway's flags are not even read.
	if _, err := loadStatic(serveOpts(t, staticArgs(t, "--preview-auth-gateway-body-limit", "0")...)); err != nil {
		t.Errorf("an unused gateway flag failed startup: %v", err)
	}
}

// fakeInformer records the handler watchPreviews registers.
type fakeInformer struct {
	crcache.Informer
	handler toolscache.ResourceEventHandler
}

func (f *fakeInformer) AddEventHandler(h toolscache.ResourceEventHandler) (toolscache.ResourceEventHandlerRegistration,
	error) {
	f.handler = h
	return nil, nil
}

type fakeSource struct {
	inf *fakeInformer
	err error
}

func (s fakeSource) GetInformer(context.Context, client.Object, ...crcache.InformerGetOption) (crcache.Informer,
	error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.inf, nil
}

// TestWatchPreviews: every update and delete (a tombstone included) names
// its Preview to the gateway; adds do not.
func TestWatchPreviews(t *testing.T) {
	inf := &fakeInformer{}
	var got []string
	if err := watchPreviews(t.Context(), fakeSource{inf: inf}, func(_ context.Context, label string) {
		got = append(got, label)
	}); err != nil {
		t.Fatal(err)
	}
	p := func(name string) *v1alpha1.Preview {
		pv := &v1alpha1.Preview{}
		pv.Namespace, pv.Name = "patchy", name
		return pv
	}
	inf.handler.OnAdd(p("added-1"), false)
	inf.handler.OnUpdate(p("demo-7"), p("demo-7"))
	inf.handler.OnDelete(p("gone-1"))
	inf.handler.OnDelete(toolscache.DeletedFinalStateUnknown{Key: "patchy/tomb-1", Obj: p("tomb-1")})
	if !slices.Equal(got, []string{"demo-7", "gone-1", "tomb-1"}) {
		t.Errorf("changed labels = %v", got)
	}
	if err := watchPreviews(t.Context(), fakeSource{err: errors.New("no cache")}, nil); err == nil {
		t.Error("an informer error was not returned")
	}
}

func TestRechecker(t *testing.T) {
	if (rechecker{}).NeedLeaderElection() {
		t.Error("the rechecker waits for leader election; every replica proxies")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	off := rechecker{srv: &httpapi.Server{}}
	if err := off.Start(ctx); err != nil {
		t.Errorf("Start = %v", err)
	}
}

// TestGatewayListener: with the gateway on, its exposure has a listener of
// its own (:8082 by default) serving the gateway's handler, so whatever
// reaches the relay's endpoints through an edge that does not filter by
// Host is limited behind the gateway's hops; the relay's listener keeps the
// relay's handler. Off, there is only the relay's.
func TestGatewayListener(t *testing.T) {
	st, err := loadStatic(serveOpts(t, gatewayArgs(t)...))
	if err != nil {
		t.Fatal(err)
	}
	if st.gateway.addr != ":8082" {
		t.Errorf("gateway listen address = %q, want :8082", st.gateway.addr)
	}
	st, err = loadStatic(serveOpts(t, gatewayArgs(t, "--preview-auth-gateway-listen-addr", ":9082")...))
	if err != nil || st.gateway.addr != ":9082" {
		t.Fatalf("gateway listen address = %v, %v", st.gateway, err)
	}
	relay := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("relay")) })
	gw := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("gateway")) })
	var added addedRunnables
	srv, err := addListeners(&added, ":8080", &gatewayStatic{addr: ":8082"}, relay, gw, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 2 || srv.Addr != ":8080" {
		t.Fatalf("added %d runnables (relay at %s), want the relay's and the gateway's listeners", len(added), srv.Addr)
	}
	for i, want := range []struct{ addr, body string }{{":8080", "relay"}, {":8082", "gateway"}} {
		l, ok := added[i].(listener)
		if !ok || l.tls || l.srv.Addr != want.addr || l.NeedLeaderElection() {
			t.Fatalf("listener %d = %+v", i, added[i])
		}
		rec := httptest.NewRecorder()
		l.srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		if rec.Body.String() != want.body {
			t.Errorf("%s serves the %s handler, want %s", want.addr, rec.Body, want.body)
		}
	}
	added = nil
	if _, err := addListeners(&added, ":8080", nil, relay, nil, nil); err != nil || len(added) != 1 {
		t.Fatalf("gateway off: %d runnables, %v", len(added), err)
	}
}

// TestProbeMode: the probe's mode follows the gateway's switch unless set,
// and a mode expecting the gateway's answers needs the gateway.
func TestProbeMode(t *testing.T) {
	for _, tt := range []struct {
		args []string
		want previewauth.ProbeMode
	}{
		{staticArgs(t), previewauth.ProbeALB},
		{gatewayArgs(t), previewauth.ProbeEither},
		{gatewayArgs(t, "--preview-auth-probe-mode", "gateway"), previewauth.ProbeGateway},
		{gatewayArgs(t, "--preview-auth-probe-mode", "alb"), previewauth.ProbeALB},
		{staticArgs(t, "--preview-auth-probe-mode", "alb"), previewauth.ProbeALB},
	} {
		st, err := loadStatic(serveOpts(t, tt.args...))
		if err != nil || st.probeMode != tt.want {
			t.Errorf("%v: mode %q, %v; want %q", tt.args[len(tt.args)-2:], st.probeMode, err, tt.want)
		}
	}
	for _, args := range [][]string{
		staticArgs(t, "--preview-auth-probe-mode", "gateway"),
		staticArgs(t, "--preview-auth-probe-mode", "either"),
		gatewayArgs(t, "--preview-auth-probe-mode", "both"),
	} {
		if _, err := loadStatic(serveOpts(t, args...)); err == nil || !strings.Contains(err.Error(), "probe-mode") {
			t.Errorf("%v: err = %v", args[len(args)-2:], err)
		}
	}
}
