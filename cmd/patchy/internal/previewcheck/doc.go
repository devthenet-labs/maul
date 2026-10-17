// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package previewcheck is the preview path's checks, shared by the `check`
// verbs so their verdicts cannot drift: `patchy check project` runs them
// for one Project, and an install-level check is to run them before any
// Project exists. It moved out of projectcheck unchanged.
//
// It reads the controllers' settings from their ConfigMaps (LoadSettings:
// each controller's own over kustomize's shared one; never a Secret), and
// checks, with the caller's own network and kubeconfig:
//
//   - preview-dns and preview-tls (Checker.Host): that <project>-0.<host
//     suffix> resolves to the preview load balancer (the placeholder
//     Ingress's address) and serves a certificate trusted for it. An Intent
//     is <project>-<issue> and no issue is numbered 0, so the probe host is
//     one the wildcard record and certificate cover and no Preview owns.
//     Only NXDOMAIN fails the DNS check; a lookup that times out is a SKIP.
//     The load balancer admits only the chart's inbound CIDRs, so a TLS
//     timeout is a SKIP, not a FAIL;
//   - preview-auth, preview-auth-dex and preview-auth-host (Checker.Auth),
//     only while the chart's preview sign-in is on (the relay's ConfigMap
//     exists, or preview-controller requires sign-in): the relay answers its
//     discovery document at its issuer; Dex accepts the relay's client with
//     its one redirect URI (asked of Dex's authorization endpoint without a
//     session, following only redirects on Dex's own host, such as a
//     single-connector Dex's hop to its connector endpoint, never one off it,
//     since Dex's clients are not readable); and, once sign-in is required,
//     the placeholder host and every Ready Preview of the Project answer
//     without credentials with the load balancer's redirect to the relay for
//     their own slot's client (previewauth.JudgeProbe, the relay's own probe
//     verdict). A timeout from outside the inbound CIDRs is a SKIP;
//   - in gateway mode (the relay's PATCHY_PREVIEW_AUTH_GATEWAY on; the mode
//     is previewauth.ProbeModeFor of that and preview-controller's
//     PATCHY_PREVIEW_LEGACY_INGRESS), preview-dns compares the probe host
//     with the gateway's edge instead: the chart's publishedAddresses (an
//     annotation on the relay's ConfigMap), else the status of the
//     gateway's Ingress or LoadBalancer Service (<release>-preview-gateway),
//     else a SKIP (an HTTPRoute or a tunnel); a private edge (RFC 1918, ULA,
//     CGNAT, so a tailnet) that this resolver does not answer with is a SKIP;
//     while both paths serve, the legacy load balancer passes too.
//     preview-auth-host asks every host as a navigation: the probe host,
//     which no Preview owns, must be the gateway's own 404 page
//     (previewauth.JudgeGatewayUnknown) and each Ready Preview the redirect to
//     sign in for its own label (previewauth.ProbeExpect.Live, either path's
//     while both serve). preview-gateway-allowlist, with inboundCIDRs set,
//     sends the probe host a spoofed X-Forwarded-For naming an admitted
//     address from an address the gateway refuses, which must still be
//     refused; from an admitted address it is a SKIP. The Dex check is the
//     same in both modes.
//
// Every line is about the install as a whole. Network and cluster reads go
// through seams (client.Reader, Resolver, TLSDialer and HTTPDoer) so tests
// need no network at all.
package previewcheck
