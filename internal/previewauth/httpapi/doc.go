// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package httpapi is the preview sign-in relay's HTTP surface: the OpenID
// provider every preview host's ALB signs viewers in through, the one Dex
// client callback and, when it is on, the preview gateway's sign-in and
// authenticating proxy. It is wiring over the pure core (internal/previewauth)
// and its ports; every decision about a token, a redirect URI or a binding
// is the core's.
//
// Endpoints (relay host; GET and POST only, 405 otherwise; bodies capped at
// 8 KiB; repeated parameters refused):
//
//   - GET /.well-known/openid-configuration and GET /jwks: discovery and the
//     ID-token keys, cacheable for five minutes.
//   - GET /authorize: the browser arrives from a preview host's ALB. Every
//     refusal is a page on the relay host, never a redirect, because how the
//     ALB treats an OAuth error redirect is unknown. With no relay session the
//     viewer is sent to Dex (PKCE, nonce, a sealed login state, a login
//     cookie per sign-in); with one, the access review runs and a code goes
//     back to the preview host's /oauth2/idpresponse.
//   - GET /dex/callback: the one fixed Dex redirect URI. Double-submit
//     check, code exchange, a relay session cookie, then the authorize
//     request resumes against the same Preview.
//   - POST /token: the ALB's backchannel only (client_secret_basic or
//     client_secret_post, exactly one). Codes are single-use through the
//     ledger; refresh tokens do not rotate. A transient failure is 503, never
//     invalid_grant, so the ALB keeps its session.
//   - GET|POST /userinfo: {"sub":"<pairwise>"} and nothing else.
//   - POST /logout: ends the relay session (same-origin only).
//
// /authorize, /dex/callback and the gateway's /start (on either sign-in
// host) are held to previewauth.CheckNavigation: a request from another site
// that is not a top-level navigation is a 403 page, before the relay session
// is read or a cookie set. A sign-in host on the preview suffix's
// registrable domain is same-site with preview code, whose background
// requests would otherwise carry the SameSite=Lax relay session.
//
// Every response carries HSTS (without includeSubDomains), nosniff,
// a CSP with frame-ancestors 'none', no-referrer and, but for discovery and
// the JWKS, Cache-Control: no-store. Each source address is rate limited
// (critique F10), except that /token and /userinfo charge it only for a
// request that fails to authenticate: those calls all come from the ALB's
// own addresses, so a limit in front of client authentication would let a
// stranger driving the ALB starve every viewer. One audit line per request names the endpoint, slot, host
// label, Project, result, pairwise subject and a short hash of the token id;
// never a token, code, state, cookie or login, except that a refused access
// review is logged at warn with the viewer's login for operators.
//
// The preview gateway (Config.Gateway; off unless set, and then beside every
// endpoint above, unchanged on the issuer host) adds, by Host:
//
//   - any host, GET /.patchy/healthz: 200, for the edge's health check.
//   - the sign-in host (the issuer's by default), GET /start?h=&s=: a preview
//     host's sealed state; with a relay session (one that carries an id) the
//     access review runs and a single-use handoff code goes to
//     https://<label>.<suffix>/.patchy/callback, else the viewer signs in at
//     Dex first (a gateway login state, kind g, through the same
//     /dex/callback). A sign-in host of its own serves only /start, its
//     /dex/callback, / and /logout, with its own Dex client.
//   - preview hosts <label>.<suffix>: /.patchy/ is the gateway's and never
//     forwarded; /.patchy/callback spends a handoff code once its state
//     cookie matches, the Preview is the same and the review passes again,
//     and sets the host session. Every other request with a host session is
//     held to the origin rule (Sec-Fetch-Site, Origin on unsafe and upgrade
//     requests), re-checked against the live Preview and the review (cached
//     20 s) and proxied to the route status.serving names, with the hygiene
//     of previewauth.CleanRequest and CleanResponse. Without one, only a
//     top-level navigation gets a state cookie and a 302 to /start; anything
//     else is a 401. The rate limit applies to unauthenticated requests and
//     handoff callbacks, never to a signed-in viewer's traffic.
//
// Proxied requests are tracked: each ends at its host session's absolute
// end, and RunRechecks (every 20 s) and PreviewChanged close
// those a new request would now refuse, streams and upgraded connections
// included.
package httpapi
