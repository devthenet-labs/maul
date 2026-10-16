// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewauth

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// ErrUnprotected means a preview host answered an unauthenticated request
// with something other than the ALB's redirect to this relay for its own
// slot's client: the host serves its app, or signs viewers in somewhere
// else, without the relay.
var ErrUnprotected = errors.New("previewauth: preview host is not protected by the relay")

// JudgeProbe judges a preview host's answer to an unauthenticated request
// (no cookies, redirects not followed). The host is protected only when it
// answered 302 to exactly <issuer>/authorize with response_type=code, slot's
// client id and the host's own callback as redirect_uri. Every other answer,
// a 200 from the app above all, is ErrUnprotected.
func JudgeProbe(status int, location, issuer string, slot int, label string, cb Callbacks) error {
	if status != http.StatusFound {
		return ErrUnprotected
	}
	want, err := cb.URL(label)
	if err != nil {
		return ErrUnprotected
	}
	u, err := url.Parse(location)
	if err != nil || u.Scheme+"://"+u.Host != issuer || u.Path != "/authorize" || u.User != nil || u.Fragment != "" {
		return ErrUnprotected
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil || len(q["client_id"]) != 1 || len(q["redirect_uri"]) != 1 || len(q["response_type"]) != 1 {
		return ErrUnprotected
	}
	if q.Get("client_id") != ClientID(slot) || q.Get("redirect_uri") != want || q.Get("response_type") != "code" {
		return ErrUnprotected
	}
	return nil
}

// JudgeGatewayProbe judges a preview host's answer to an unauthenticated
// navigation when the preview gateway serves it: protected only when it
// answered 302 to exactly https://<sign-in host>/start with h the host's own
// label and s a sealed state. Every other answer is ErrUnprotected.
func JudgeGatewayProbe(status int, location string, hosts Hosts, label string) error {
	if status != http.StatusFound || hosts.SignIn() == "" {
		return ErrUnprotected
	}
	u, err := url.Parse(location)
	if err != nil || u.Scheme != "https" || u.Host != hosts.SignIn() || u.Path != "/start" || u.User != nil ||
		u.Fragment != "" {
		return ErrUnprotected
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil || len(q) != 2 || len(q["h"]) != 1 || len(q["s"]) != 1 || q.Get("h") != label ||
		TokenKind(q.Get("s")) != KindState {
		return ErrUnprotected
	}
	return nil
}

// The marks of a page the relay or the gateway renders itself, which the
// probes recognise it by. httpapi renders its pages with exactly these, and
// its tests hold the two together.
const (
	// PageCSP is the Content-Security-Policy of every such page.
	PageCSP = "default-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"
	// NoPreviewTitle is the title of the gateway's page for a preview host
	// with no live Preview: the same whether the label never existed or is
	// not live.
	NoPreviewTitle = "No live preview"
	// NotOnAllowlistMessage is the sentence of the gateway's page refusing a
	// client outside its inbound CIDRs.
	NotOnAllowlistMessage = "Previews cannot be reached from this network address."
)

// UnknownProbeLabel is the label the relay's background probe asks for to
// see the gateway's answer to a host no Preview owns. An Intent is named
// <project>-<issue> and no issue is numbered 0, so no Preview is ever
// labelled with it.
const UnknownProbeLabel = "patchy-probe-0"

// ProbeMode is which path is expected to answer preview hosts, and so which
// answers protect them.
type ProbeMode string

const (
	// ProbeALB: only the legacy ALB path serves (the gateway is off). A
	// live host must answer with the ALB's redirect to the relay.
	ProbeALB ProbeMode = "alb"
	// ProbeEither: release N's migration, both paths serve. A live host
	// may answer with either redirect; wherever the wildcard record points,
	// both authenticate.
	ProbeEither ProbeMode = "either"
	// ProbeGateway: only the gateway serves (the legacy slot Ingress is
	// off). A live host must answer with the gateway's redirect to its
	// sign-in host, and a host no Preview owns with the gateway's 404.
	ProbeGateway ProbeMode = "gateway"
)

// ProbeModeFor is the mode in force for an install: the gateway's switch
// and preview-controller's legacy slot Ingress (--preview-legacy-ingress).
func ProbeModeFor(gateway, legacyIngress bool) ProbeMode {
	switch {
	case !gateway:
		return ProbeALB
	case legacyIngress:
		return ProbeEither
	default:
		return ProbeGateway
	}
}

// ParseProbeMode reads a mode; empty is "", for the caller to derive.
func ParseProbeMode(s string) (ProbeMode, error) {
	switch m := ProbeMode(s); m {
	case "", ProbeALB, ProbeEither, ProbeGateway:
		return m, nil
	}
	return "", fmt.Errorf("previewauth: probe mode %q is not alb, either or gateway", s)
}

// ProbeExpect is what an unauthenticated request to a preview host must be
// answered with under a mode.
type ProbeExpect struct {
	Mode ProbeMode
	// Issuer and Callbacks are the relay's, for the ALB's redirect.
	Issuer    string
	Callbacks Callbacks
	// Gateway is the gateway's host grammar; required unless Mode is
	// ProbeALB.
	Gateway Hosts
}

// Live judges a live Preview's host's answer (no cookies, an HTML
// navigation, redirects not followed): the ALB's redirect under ProbeALB,
// the gateway's under ProbeGateway, either under ProbeEither.
func (e ProbeExpect) Live(status int, location string, slot int, label string) error {
	alb := func() error { return JudgeProbe(status, location, e.Issuer, slot, label, e.Callbacks) }
	gw := func() error { return JudgeGatewayProbe(status, location, e.Gateway, label) }
	switch e.Mode {
	case ProbeALB:
		return alb()
	case ProbeGateway:
		return gw()
	case ProbeEither:
		if alb() == nil {
			return nil
		}
		return gw()
	}
	return ErrUnprotected
}

// JudgeGatewayUnknown judges the answer to an unauthenticated navigation to
// a preview host no Preview owns, when the gateway serves preview hosts: it
// must be the gateway's own 404 page (the page envelope, COOP and the
// no-preview title), never an app's answer or another server's. Every other
// answer is ErrUnprotected.
func JudgeGatewayUnknown(status int, h http.Header, body []byte) error {
	if status != http.StatusNotFound || !gatewayPage(h) ||
		!bytes.Contains(body, []byte("<title>"+NoPreviewTitle+"</title>")) {
		return ErrUnprotected
	}
	return nil
}

// IsAllowlistRefusal reports whether an answer is the gateway's page
// refusing a client outside its inbound CIDRs.
func IsAllowlistRefusal(status int, h http.Header, body []byte) bool {
	return status == http.StatusForbidden && gatewayPage(h) && bytes.Contains(body, []byte(NotOnAllowlistMessage))
}

// gatewayPage reports the headers of a page the gateway renders itself on
// a preview host.
func gatewayPage(h http.Header) bool {
	return len(h.Values("Content-Security-Policy")) == 1 && h.Get("Content-Security-Policy") == PageCSP &&
		h.Get("Cross-Origin-Opener-Policy") == "same-origin" && h.Get("Cache-Control") == "no-store" &&
		strings.HasPrefix(h.Get("Content-Type"), "text/html")
}
