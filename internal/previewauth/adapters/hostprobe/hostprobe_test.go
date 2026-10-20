// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package hostprobe

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"

	"github.com/devthenet-labs/maul/internal/previewauth"
)

const (
	suffix = "preview.example.com"
	issuer = "https://preview-auth.example.com"
)

type views []previewauth.View

func (v views) Ready(context.Context) ([]previewauth.View, error) { return v, nil }

type failing struct{}

func (failing) Ready(context.Context) ([]previewauth.View, error) { return nil, errors.New("down") }

// hosts answers each host as the test says: "auth" redirects as the ALB
// would, "app" serves the app, "other-slot" redirects for slot 0's client,
// "down" fails the connection.
type hosts map[string]string

func (h hosts) RoundTrip(r *http.Request) (*http.Response, error) {
	label := r.URL.Hostname()[:len(r.URL.Hostname())-len(suffix)-1]
	if r.Header.Get("Cookie") != "" || r.URL.Path != "/" {
		return nil, errors.New("unexpected request")
	}
	rec := httptest.NewRecorder()
	slot := "1"
	switch h[label] {
	case "down":
		return nil, errors.New("connection refused")
	case "app":
		rec.WriteHeader(http.StatusOK)
		return rec.Result(), nil
	case "other-slot":
		slot = "0"
	case "gateway":
		rec.Header().Set("Location", "https://preview-auth.example.com/start?h="+label+"&s=pa1.t.1.abc")
		rec.WriteHeader(http.StatusFound)
		return rec.Result(), nil
	case "gateway-404":
		gatewayPage(rec)
		rec.WriteHeader(http.StatusNotFound)
		_, _ = rec.WriteString("<title>" + previewauth.NoPreviewTitle + "</title>")
		return rec.Result(), nil
	case "gateway-refused":
		gatewayPage(rec)
		rec.WriteHeader(http.StatusForbidden)
		_, _ = rec.WriteString("<title>Refused</title><p>" + previewauth.NotOnAllowlistMessage + "</p>")
		return rec.Result(), nil
	case "403":
		rec.WriteHeader(http.StatusForbidden)
		_, _ = rec.WriteString(previewauth.NotOnAllowlistMessage)
		return rec.Result(), nil
	case "404":
		rec.WriteHeader(http.StatusNotFound)
		return rec.Result(), nil
	}
	q := url.Values{"client_id": {"maul-preview-s" + slot}, "response_type": {"code"},
		"redirect_uri": {"https://" + label + "." + suffix + "/oauth2/idpresponse"}}
	rec.Header().Set("Location", issuer+"/authorize?"+q.Encode())
	rec.WriteHeader(http.StatusFound)
	return rec.Result(), nil
}

// gatewayPage sets the headers of a page the gateway renders itself.
func gatewayPage(rec *httptest.ResponseRecorder) {
	h := rec.Header()
	h.Set("Content-Security-Policy", previewauth.PageCSP)
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Type", "text/html; charset=utf-8")
}

func TestOnce(t *testing.T) {
	cb, err := previewauth.NewCallbacks(suffix)
	if err != nil {
		t.Fatal(err)
	}
	v := func(label string) previewauth.View {
		return previewauth.View{UID: "u-" + label, Label: label, Project: "p", Slot: 1, Live: true}
	}
	p := &Prober{
		Lister:    views{v("a-1"), v("b-2"), v("c-3"), v("d-4")},
		Callbacks: cb, Issuer: issuer,
		Transport: hosts{"a-1": "auth", "b-2": "app", "c-3": "other-slot", "d-4": "down"},
	}
	res := p.Once(context.Background())
	slices.Sort(res.UnprotectedLabels)
	if res.Probed != 4 || res.Unprotected != 2 || res.Unreachable != 1 ||
		!slices.Equal(res.UnprotectedLabels, []string{"b-2", "c-3"}) {
		t.Fatalf("result %+v", res)
	}
	p.Lister = failing{}
	if res := p.Once(context.Background()); res.Probed != 0 {
		t.Fatalf("failed listing probed %+v", res)
	}
}

// TestOnceGateway: with the gateway on, a host answering with the gateway's
// redirect is protected as well as one behind the ALB; without it, it is not.
func TestOnceGateway(t *testing.T) {
	cb, err := previewauth.NewCallbacks(suffix)
	if err != nil {
		t.Fatal(err)
	}
	gw, err := previewauth.NewHosts(suffix, "preview-auth.example.com")
	if err != nil {
		t.Fatal(err)
	}
	v := func(label string) previewauth.View {
		return previewauth.View{UID: "u-" + label, Label: label, Project: "p", Slot: 1, Live: true}
	}
	p := &Prober{
		Lister: views{v("a-1"), v("b-2"), v("g-3")}, Callbacks: cb, Issuer: issuer,
		Transport: hosts{"a-1": "auth", "b-2": "app", "g-3": "gateway"},
	}
	if res := p.Once(context.Background()); res.Unprotected != 2 {
		t.Fatalf("gateway off: %+v", res)
	}
	p.Gateway = &gw
	res := p.Once(context.Background())
	if res.Probed != 3 || res.Unprotected != 1 || !slices.Equal(res.UnprotectedLabels, []string{"b-2"}) {
		t.Fatalf("gateway on: %+v", res)
	}
}

func TestStartDisabled(t *testing.T) {
	if err := (&Prober{}).Start(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestOnceGatewayMode: in gateway mode only the gateway's redirect protects
// a live host, and each pass also asks for a host no Preview owns, which must
// answer with the gateway's own 404; in the migration mode neither changes.
func TestOnceGatewayMode(t *testing.T) {
	cb, err := previewauth.NewCallbacks(suffix)
	if err != nil {
		t.Fatal(err)
	}
	gw, err := previewauth.NewHosts(suffix, "preview-auth.example.com")
	if err != nil {
		t.Fatal(err)
	}
	v := func(label string) previewauth.View {
		return previewauth.View{UID: "u-" + label, Label: label, Project: "p", Slot: 1, Live: true}
	}
	unknown := previewauth.UnknownProbeLabel
	tests := []struct {
		name      string
		mode      previewauth.ProbeMode
		unknownAs string
		probed    int
		bad       []string
	}{
		{"gateway, its 404", previewauth.ProbeGateway, "gateway-404", 4, []string{"a-1", "b-2"}},
		{"gateway, another 404", previewauth.ProbeGateway, "404", 4, []string{"a-1", "b-2", unknown}},
		{"gateway, an app", previewauth.ProbeGateway, "app", 4, []string{"a-1", "b-2", unknown}},
		{"either", previewauth.ProbeEither, "app", 3, []string{"b-2"}},
		{"derived from Gateway", "", "app", 3, []string{"b-2"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &Prober{
				Lister: views{v("a-1"), v("b-2"), v("g-3")}, Callbacks: cb, Issuer: issuer, Gateway: &gw,
				Mode:      tt.mode,
				Transport: hosts{"a-1": "auth", "b-2": "app", "g-3": "gateway", unknown: tt.unknownAs},
			}
			res := p.Once(context.Background())
			slices.Sort(res.UnprotectedLabels)
			slices.Sort(tt.bad)
			if res.Probed != tt.probed || !slices.Equal(res.UnprotectedLabels, tt.bad) {
				t.Fatalf("result %+v, want %d probed and %v unprotected", res, tt.probed, tt.bad)
			}
		})
	}
}

// TestOnceGatewayAllowlistRefusal: when the gateway refuses the relay's own
// egress address by its inbound allowlist, it does so before it routes any
// host, so a refused host is unreachable, never unprotected, in either mode
// with the gateway on. A bare 403 carrying the same words is not the
// gateway's page and stays unprotected, and without the gateway the
// refusal page protects nothing.
func TestOnceGatewayAllowlistRefusal(t *testing.T) {
	cb, err := previewauth.NewCallbacks(suffix)
	if err != nil {
		t.Fatal(err)
	}
	gw, err := previewauth.NewHosts(suffix, "preview-auth.example.com")
	if err != nil {
		t.Fatal(err)
	}
	v := func(label string) previewauth.View {
		return previewauth.View{UID: "u-" + label, Label: label, Project: "p", Slot: 1, Live: true}
	}
	unknown := previewauth.UnknownProbeLabel
	tests := []struct {
		name                string
		gateway             *previewauth.Hosts
		mode                previewauth.ProbeMode
		probed, unreachable int
		bad                 []string
	}{
		{"gateway", &gw, previewauth.ProbeGateway, 3, 3, nil},
		{"either", &gw, previewauth.ProbeEither, 2, 2, nil},
		{"gateway off", nil, previewauth.ProbeALB, 2, 0, []string{"r-1", "r-2"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &Prober{
				Lister: views{v("r-1"), v("r-2")}, Callbacks: cb, Issuer: issuer, Gateway: tt.gateway,
				Mode:      tt.mode,
				Transport: hosts{"r-1": "gateway-refused", "r-2": "gateway-refused", unknown: "gateway-refused"},
			}
			res := p.Once(context.Background())
			slices.Sort(res.UnprotectedLabels)
			if res.Probed != tt.probed || res.Unreachable != tt.unreachable ||
				!slices.Equal(res.UnprotectedLabels, tt.bad) {
				t.Fatalf("result %+v, want %d probed, %d unreachable, %v unprotected", res, tt.probed,
					tt.unreachable, tt.bad)
			}
		})
	}
	p := &Prober{
		Lister: views{v("r-1")}, Callbacks: cb, Issuer: issuer, Gateway: &gw, Mode: previewauth.ProbeEither,
		Transport: hosts{"r-1": "403"},
	}
	if res := p.Once(context.Background()); res.Unprotected != 1 || res.Unreachable != 0 {
		t.Fatalf("a bare 403: %+v", res)
	}
}
