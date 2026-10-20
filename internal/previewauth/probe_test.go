// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewauth

import (
	"errors"
	"math/rand"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
)

func probeLocation(issuer string, slot int, label string, edit func(url.Values)) string {
	q := url.Values{
		"client_id":     {ClientID(slot)},
		"redirect_uri":  {"https://" + label + "." + testSuffix + CallbackPath},
		"response_type": {"code"},
		"scope":         {"openid"},
		"state":         {"alb-state"},
	}
	if edit != nil {
		edit(q)
	}
	return issuer + "/authorize?" + q.Encode()
}

func TestJudgeProbe(t *testing.T) {
	cb := newCallbacks(t)
	good := probeLocation(testIssuer, 1, "intent-7", nil)
	tests := []struct {
		name     string
		status   int
		location string
		ok       bool
	}{
		{"the ALB redirect", 302, good, true},
		{"the app answers", 200, "", false},
		{"a 401", 401, "", false},
		{"a 301 redirect", 301, good, false},
		{"another slot's client", 302, probeLocation(testIssuer, 0, "intent-7", nil), false},
		{"another host's callback", 302, probeLocation(testIssuer, 1, "intent-8", nil), false},
		{"another issuer", 302, probeLocation("https://evil.example.com", 1, "intent-7", nil), false},
		{"issuer lookalike", 302, probeLocation(testIssuer+".evil", 1, "intent-7", nil), false},
		{"another path", 302, testIssuer + "/authorize/x?" + url.Values{"client_id": {ClientID(1)}}.Encode(), false},
		{"repeated client", 302, probeLocation(testIssuer, 1, "intent-7", func(q url.Values) {
			q.Add("client_id", ClientID(0))
		}), false},
		{"no response type", 302, probeLocation(testIssuer, 1, "intent-7", func(q url.Values) {
			q.Del("response_type")
		}), false},
		{"userinfo in location", 302, "https://u@preview-auth.example.com/authorize?client_id=maul-preview-s1", false},
		{"unparseable", 302, "%zz", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := JudgeProbe(tt.status, tt.location, testIssuer, 1, "intent-7", cb)
			if (err == nil) != tt.ok {
				t.Fatalf("JudgeProbe = %v, want ok %v", err, tt.ok)
			}
			if err != nil && !errors.Is(err, ErrUnprotected) {
				t.Fatalf("error %v is not ErrUnprotected", err)
			}
		})
	}
}

// TestJudgeProbeOnlyItsOwnSlotAndLabel: a redirect for any (slot, label)
// passes for exactly that pair and for no other.
func TestJudgeProbeOnlyItsOwnSlotAndLabel(t *testing.T) {
	cb := newCallbacks(t)
	type pair struct {
		Slot  int
		Label string
	}
	gen := func(vals []reflect.Value, r *rand.Rand) {
		vals[0] = reflect.ValueOf(pair{r.Intn(MaxSlots), randLabel(r)})
		switch r.Intn(3) {
		case 0:
			vals[1] = vals[0]
		case 1:
			vals[1] = reflect.ValueOf(pair{r.Intn(MaxSlots), vals[0].Interface().(pair).Label})
		default:
			vals[1] = reflect.ValueOf(pair{r.Intn(MaxSlots), randLabel(r)})
		}
	}
	prop := func(issued, asked pair) bool {
		loc := probeLocation(testIssuer, issued.Slot, issued.Label, nil)
		err := JudgeProbe(302, loc, testIssuer, asked.Slot, asked.Label, cb)
		return (err == nil) == (issued == asked)
	}
	cfg := quickConfig(2000)
	cfg.Values = gen
	if err := quick.Check(prop, cfg); err != nil {
		t.Fatal(err)
	}
}

func TestJudgeGatewayProbe(t *testing.T) {
	h, err := NewHosts(testSuffix, "preview-auth.example.com")
	if err != nil {
		t.Fatal(err)
	}
	good := h.StartURL("intent-7", "pa1.t.1.abc")
	if err := JudgeGatewayProbe(302, good, h, "intent-7"); err != nil {
		t.Errorf("the gateway's own redirect: %v", err)
	}
	for _, tc := range []struct {
		status int
		loc    string
	}{
		{200, ""},
		{301, good},
		{302, h.StartURL("other-1", "pa1.t.1.abc")},
		{302, "https://preview-auth.example.com/start?h=intent-7&s=pa1.o.1.abc"},
		{302, "https://evil.example.com/start?h=intent-7&s=pa1.t.1.abc"},
		{302, "http://preview-auth.example.com/start?h=intent-7&s=pa1.t.1.abc"},
		{302, "https://preview-auth.example.com/authorize?h=intent-7&s=pa1.t.1.abc"},
		{302, good + "&h=intent-7"},
		{302, good + "&x=1"},
		{302, "https://u@preview-auth.example.com/start?h=intent-7&s=pa1.t.1.abc"},
	} {
		if err := JudgeGatewayProbe(tc.status, tc.loc, h, "intent-7"); err == nil {
			t.Errorf("JudgeGatewayProbe(%d, %q) passed", tc.status, tc.loc)
		}
	}
}

func TestProbeModes(t *testing.T) {
	for _, tc := range []struct {
		gateway, legacy bool
		want            ProbeMode
	}{
		{false, true, ProbeALB}, {false, false, ProbeALB}, {true, true, ProbeEither}, {true, false, ProbeGateway},
	} {
		if got := ProbeModeFor(tc.gateway, tc.legacy); got != tc.want {
			t.Errorf("ProbeModeFor(%v, %v) = %s, want %s", tc.gateway, tc.legacy, got, tc.want)
		}
	}
	for _, s := range []string{"", "alb", "either", "gateway"} {
		if m, err := ParseProbeMode(s); err != nil || string(m) != s {
			t.Errorf("ParseProbeMode(%q) = %q, %v", s, m, err)
		}
	}
	for _, s := range []string{"ALB", "both", " gateway"} {
		if _, err := ParseProbeMode(s); err == nil {
			t.Errorf("ParseProbeMode(%q) passed", s)
		}
	}
}

// TestProbeExpectLive: each mode accepts exactly its paths' redirects for
// the host's own label and slot.
func TestProbeExpectLive(t *testing.T) {
	cb := newCallbacks(t)
	h, err := NewHosts(testSuffix, "preview-auth.example.com")
	if err != nil {
		t.Fatal(err)
	}
	alb := probeLocation(testIssuer, 1, "intent-7", nil)
	gw := h.StartURL("intent-7", "pa1.t.1.abc")
	for _, tc := range []struct {
		mode    ProbeMode
		albOK   bool
		gwOK    bool
		otherOK bool
	}{
		{ProbeALB, true, false, false},
		{ProbeEither, true, true, false},
		{ProbeGateway, false, true, false},
		{"", false, false, false},
	} {
		e := ProbeExpect{Mode: tc.mode, Issuer: testIssuer, Callbacks: cb, Gateway: h}
		for loc, want := range map[string]bool{alb: tc.albOK, gw: tc.gwOK,
			h.StartURL("other-1", "pa1.t.1.abc"): tc.otherOK} {
			if err := e.Live(302, loc, 1, "intent-7"); (err == nil) != want {
				t.Errorf("%q: Live(%q) = %v, want ok %v", tc.mode, loc, err, want)
			}
		}
		if err := e.Live(200, "", 1, "intent-7"); !errors.Is(err, ErrUnprotected) {
			t.Errorf("%q: the app's 200 = %v", tc.mode, err)
		}
	}
}

// gatewayNotFound is the gateway's 404 for a host no Preview owns, as
// httpapi renders it (TestGatewayProbeVerdictsMatchThePages holds the two
// together).
func gatewayNotFound() (int, http.Header, []byte) {
	h := http.Header{}
	h.Set("Content-Security-Policy", PageCSP)
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Type", "text/html; charset=utf-8")
	return http.StatusNotFound, h, []byte("<!doctype html><html><head><title>" + NoPreviewTitle +
		"</title></head><body></body></html>")
}

func TestJudgeGatewayUnknown(t *testing.T) {
	status, h, body := gatewayNotFound()
	if err := JudgeGatewayUnknown(status, h, body); err != nil {
		t.Fatalf("the gateway's own 404: %v", err)
	}
	for name, edit := range map[string]func(*int, http.Header, *[]byte){
		"an app's 200":         func(s *int, _ http.Header, _ *[]byte) { *s = 200 },
		"the sign-in redirect": func(s *int, _ http.Header, _ *[]byte) { *s = 302 },
		"no CSP":               func(_ *int, h http.Header, _ *[]byte) { h.Del("Content-Security-Policy") },
		"another CSP":          func(_ *int, h http.Header, _ *[]byte) { h.Set("Content-Security-Policy", "default-src *") },
		"two CSPs":             func(_ *int, h http.Header, _ *[]byte) { h.Add("Content-Security-Policy", PageCSP) },
		"no COOP":              func(_ *int, h http.Header, _ *[]byte) { h.Del("Cross-Origin-Opener-Policy") },
		"cacheable":            func(_ *int, h http.Header, _ *[]byte) { h.Set("Cache-Control", "public") },
		"plain text":           func(_ *int, h http.Header, _ *[]byte) { h.Set("Content-Type", "text/plain") },
		"another page":         func(_ *int, _ http.Header, b *[]byte) { *b = []byte("<title>Not found</title>") },
		"a default backend": func(_ *int, h http.Header, b *[]byte) {
			h.Set("Content-Type", "text/plain")
			*b = []byte("404 page not found")
		},
	} {
		s, hh, b := gatewayNotFound()
		edit(&s, hh, &b)
		if err := JudgeGatewayUnknown(s, hh, b); !errors.Is(err, ErrUnprotected) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestJudgeGatewayUnknownProperty: with any one of its marks changed, the
// gateway's 404 is no longer the gateway's 404.
func TestJudgeGatewayUnknownProperty(t *testing.T) {
	headers := []string{"Content-Security-Policy", "Cross-Origin-Opener-Policy", "Cache-Control", "Content-Type"}
	prop := func(which uint8, status uint16, junk string) bool {
		s, h, b := gatewayNotFound()
		switch which % 3 {
		case 0:
			if s = int(status%500) + 100; s == http.StatusNotFound {
				return true
			}
		case 1:
			name := headers[int(which/3)%len(headers)]
			if junk == h.Get(name) || name == "Content-Type" && strings.HasPrefix(junk, "text/html") {
				return true
			}
			h.Set(name, junk)
		default:
			b = []byte(junk)
			if strings.Contains(junk, "<title>"+NoPreviewTitle+"</title>") {
				return true
			}
		}
		return errors.Is(JudgeGatewayUnknown(s, h, b), ErrUnprotected)
	}
	if err := quick.Check(prop, quickConfig(2000)); err != nil {
		t.Fatal(err)
	}
}

func TestIsAllowlistRefusal(t *testing.T) {
	_, h, _ := gatewayNotFound()
	page := []byte("<p>" + NotOnAllowlistMessage + "</p>")
	if !IsAllowlistRefusal(http.StatusForbidden, h, page) {
		t.Fatal("the gateway's allowlist page was not recognised")
	}
	if IsAllowlistRefusal(http.StatusForbidden, h, []byte("<p>Previews cannot register service workers.</p>")) ||
		IsAllowlistRefusal(http.StatusNotFound, h, page) || IsAllowlistRefusal(http.StatusForbidden, http.Header{}, page) {
		t.Fatal("another answer was taken for the allowlist page")
	}
}
