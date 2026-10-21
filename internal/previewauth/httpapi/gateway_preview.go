// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/devthenet-labs/maul/internal/previewauth"
)

// Preview-host results, the gateway counter's and the audit line's.
const (
	resultProxied      = "proxied"
	resultSignIn       = "sign_in"
	resultUnauth       = "unauthenticated"
	resultCrossOrigin  = "cross_origin"
	resultWorker       = "service_worker"
	resultNotAllowed   = "not_on_allowlist"
	resultNotServing   = "not_serving"
	resultNoRoute      = "no_route"
	resultUpstreamDown = "upstream_error"
	resultTooLarge     = "too_large"
	resultStateCSRF    = "state_mismatch"
	resultReplay       = "replay"
	resultSession      = "session"
	resultLimited      = "rate_limited"
)

// preview serves one request to a preview host. Order matters: the reserved
// prefix first (never forwarded), then the allowlist and the service-worker
// refusal, then the host session; a request without one is limited and,
// only as a top-level navigation, sent to sign in.
func (g *gateway) preview(w http.ResponseWriter, r *http.Request, label string) {
	start := g.s.now()
	a := &record{endpoint: "preview", slot: -1, label: label}
	sw := &statusWriter{ResponseWriter: w}
	r = r.WithContext(context.WithValue(r.Context(), auditKey{}, a))
	defer func() {
		observe(r.Context(), a.endpoint, g.s.now().Sub(start))
		count(r.Context(), gatewayCounter, resultAttr(a.result))
		level := slog.LevelInfo
		if a.result == resultProxied {
			level = slog.LevelDebug
		}
		g.s.log.LogAttrs(r.Context(), level, "preview-gateway",
			slog.String("endpoint", a.endpoint), slog.String("method", r.Method), slog.Int("status", sw.status),
			slog.Int("slot", a.slot), slog.String("label", a.label), slog.String("project", a.project),
			slog.String("result", a.result), slog.String("sub", a.sub), slog.String("jti", a.jti),
			slog.Duration("duration", g.s.now().Sub(start)))
	}()
	if previewauth.ReservedPath(r.URL.Path) {
		if r.URL.Path == previewauth.HandoffPath {
			a.endpoint = "handoff"
			g.handleHandoff(sw, r, label)
			return
		}
		g.page(sw, r, resultNoRoute, http.StatusNotFound, pageNotFound)
		return
	}
	if r.Method == http.MethodConnect || r.Method == http.MethodTrace {
		sw.Header().Set("Allow", "GET, HEAD, POST, PUT, PATCH, DELETE, OPTIONS")
		g.page(sw, r, "method_not_allowed", http.StatusMethodNotAllowed, pageNotAllowed)
		return
	}
	ip := clientIP(r, g.cfg.RateLimit.ForwardedHops)
	if !g.inbound(ip) {
		g.page(sw, r, resultNotAllowed, http.StatusForbidden, pageNotOnAllowlist)
		return
	}
	if previewauth.IsServiceWorkerScript(r.Header) {
		g.page(sw, r, resultWorker, http.StatusForbidden, pageServiceWorker)
		return
	}
	sess, ok := g.hostSession(r, label)
	if !ok {
		g.unauthenticated(sw, r, label)
		return
	}
	a.bind(sess.Bound, sess.Sub, sess.ID)
	if err := previewauth.CheckOrigin(r.Method, r.Header, g.cfg.Hosts.Origin(label)); err != nil {
		g.page(sw, r, resultCrossOrigin, http.StatusForbidden, pageCrossOrigin)
		return
	}
	view, ok := g.authorize(sw, r, label, sess)
	if !ok {
		return
	}
	if len(view.Routes) == 0 {
		g.page(sw, r, resultNotServing, http.StatusServiceUnavailable, pageStarting)
		return
	}
	route, ok := view.Pick(r.URL.Path)
	if !ok {
		g.page(sw, r, resultNoRoute, http.StatusNotFound, pageNotFound)
		return
	}
	g.forward(sw, r, view, route, sess, ip)
}

// hostSession opens the request's host session, which must be for this
// host's label.
func (g *gateway) hostSession(r *http.Request, label string) (previewauth.HostSession, bool) {
	c, err := r.Cookie(previewauth.HostSessionCookie)
	if err != nil {
		return previewauth.HostSession{}, false
	}
	sess, err := g.s.cfg.Ring.OpenHostSession(c.Value, g.s.now())
	if err != nil || sess.Label != label {
		return previewauth.HostSession{}, false
	}
	return sess, true
}

// authorize re-checks a host session against the live Preview and the
// access review. A definitive refusal clears the cookie and answers with a
// page (or, for a new Preview at the label, starts a new sign-in); a
// transient failure answers 503 and keeps it.
func (g *gateway) authorize(w http.ResponseWriter, r *http.Request, label string,
	sess previewauth.HostSession) (previewauth.View, bool) {
	ctx := r.Context()
	view, err := g.s.cfg.Lookup.ByLabel(ctx, label)
	if err != nil {
		g.logRefusal(r, "preview lookup", err)
		g.page(w, r, resultError, http.StatusServiceUnavailable, pageUnavailable)
		return view, false
	}
	rec(r).project = view.Project
	switch err := sess.Matches(view); {
	case errors.Is(err, previewauth.ErrNotBound) && view.Live:
		http.SetCookie(w, expire(previewauth.HostSessionCookie))
		g.unauthenticated(w, r, label)
		return view, false
	case err != nil:
		http.SetCookie(w, expire(previewauth.HostSessionCookie))
		g.page(w, r, resultNoPreview, http.StatusNotFound, pageNoPreview)
		return view, false
	}
	allowed, err := g.s.review(ctx, sess.Identity, view)
	switch {
	case errors.Is(err, previewauth.ErrNoProject) || err == nil && !allowed:
		http.SetCookie(w, expire(previewauth.HostSessionCookie))
		g.page(w, r, resultDenied, http.StatusForbidden, pageDenied)
		return view, false
	case err != nil:
		g.logRefusal(r, "access review", err)
		g.page(w, r, resultError, http.StatusServiceUnavailable, pageUnavailable)
		return view, false
	}
	return view, true
}

// unauthenticated answers a preview-host request with no usable host
// session. It is rate limited. A label with no live Preview is the same 404
// whether it never existed or is not live. Only a top-level navigation gets
// a state cookie and a redirect to sign in; anything else is a 401 with no
// cookie, so background requests never start a sign-in loop.
func (g *gateway) unauthenticated(w http.ResponseWriter, r *http.Request, label string) {
	if !g.limiter.allow(r) {
		w.Header().Set("Retry-After", "1")
		g.page(w, r, resultLimited, http.StatusTooManyRequests, pageRateLimited)
		count(r.Context(), limitedCounter)
		return
	}
	view, err := g.s.cfg.Lookup.ByLabel(r.Context(), label)
	if err != nil {
		g.logRefusal(r, "preview lookup", err)
		g.page(w, r, resultError, http.StatusServiceUnavailable, pageUnavailable)
		return
	}
	if !view.Live || view.Label != label {
		g.page(w, r, resultNoPreview, http.StatusNotFound, pageNoPreview)
		return
	}
	if !previewauth.TopLevelNavigation(r.Method, r.Header) {
		g.page(w, r, resultUnauth, http.StatusUnauthorized, pageSignInRequired)
		return
	}
	now := g.s.now()
	st, nonce, err := previewauth.NewState(now, g.cfg.Lifetimes, view, r.URL.RequestURI())
	if err != nil {
		g.logRefusal(r, "start sign-in", err)
		g.page(w, r, resultError, http.StatusServiceUnavailable, pageUnavailable)
		return
	}
	sealedState, err := g.s.cfg.Ring.SealState(st)
	if err != nil {
		g.logRefusal(r, "seal sign-in state", err)
		g.page(w, r, resultError, http.StatusServiceUnavailable, pageUnavailable)
		return
	}
	pageHeaders(w.Header())
	http.SetCookie(w, &http.Cookie{
		Name: previewauth.StateCookieName(sealedState), Value: nonce, Path: "/",
		MaxAge: int(g.cfg.Lifetimes.State.Seconds()), Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	rec(r).result = resultSignIn
	http.Redirect(w, r, g.cfg.Hosts.StartURL(label, sealedState), http.StatusFound)
}

// handleHandoff is /.maul/callback?code=: the sign-in host sent the
// browser back with a handoff code. It is spent only after the state cookie
// it names matches, the Preview is the same one and the access review
// passes again; then the host session is set and the browser returns to the
// path it asked for.
func (g *gateway) handleHandoff(w http.ResponseWriter, r *http.Request, label string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		g.page(w, r, "method_not_allowed", http.StatusMethodNotAllowed, pageNotAllowed)
		return
	}
	if !g.limiter.allow(r) {
		w.Header().Set("Retry-After", "1")
		g.page(w, r, resultLimited, http.StatusTooManyRequests, pageRateLimited)
		count(r.Context(), limitedCounter)
		return
	}
	now := g.s.now()
	h, ok := g.redeem(w, r, label, now)
	if !ok {
		return
	}
	sess, err := previewauth.NewHostSession(now, g.cfg.Lifetimes, h)
	if err != nil {
		g.page(w, r, "expired", http.StatusBadRequest, pageSignInExpired)
		return
	}
	tok, err := g.s.cfg.Ring.SealHostSession(sess)
	if errors.Is(err, previewauth.ErrIdentityTooLarge) {
		g.page(w, r, "too_large", http.StatusBadRequest, pageTooManyGroups)
		return
	}
	if err != nil {
		g.logRefusal(r, "seal host session", err)
		g.page(w, r, resultError, http.StatusServiceUnavailable, pageUnavailable)
		return
	}
	pageHeaders(w.Header())
	http.SetCookie(w, &http.Cookie{
		Name: previewauth.HostSessionCookie, Value: tok, Path: "/", MaxAge: int(sess.Expires - now.Unix()),
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	http.SetCookie(w, expire(h.StateCookie()))
	rec(r).result = resultSession
	http.Redirect(w, r, g.cfg.Hosts.Location(label, h.Return), http.StatusFound)
}

// redeem opens a handoff code, checks it against this browser's state
// cookie, the live Preview and the access review, and spends it in the
// ledger. Every refusal is answered here.
func (g *gateway) redeem(w http.ResponseWriter, r *http.Request, label string,
	now time.Time) (previewauth.Handoff, bool) {
	ctx := r.Context()
	a := rec(r)
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || hasRepeats(q) || q.Get("code") == "" || len(q.Get("code")) > previewauth.MaxTokenBytes {
		g.page(w, r, resultBadRequest, http.StatusBadRequest, pageBadRequest)
		return previewauth.Handoff{}, false
	}
	h, err := g.s.cfg.Ring.OpenHandoff(q.Get("code"), now)
	if err != nil {
		g.page(w, r, "expired", http.StatusBadRequest, pageSignInExpired)
		return h, false
	}
	a.bind(h.Bound, h.Sub, h.JTI)
	var cookie string
	if c, err := r.Cookie(h.StateCookie()); err == nil {
		cookie = c.Value
	}
	if h.CheckState(cookie) != nil {
		// Not the browser that started this sign-in: spend the code, so
		// the browser holding its state (perhaps one that lured this
		// viewer to /start) can never redeem it.
		if err := g.s.cfg.Ledger.Consume(ctx, previewauth.HandoffKey(h), time.Unix(h.Expires, 0)); err != nil &&
			!errors.Is(err, previewauth.ErrReplayed) {
			g.logRefusal(r, "code ledger", err)
		}
		g.page(w, r, resultStateCSRF, http.StatusBadRequest, pageSignInExpired)
		return h, false
	}
	view, err := g.s.cfg.Lookup.ByLabel(ctx, label)
	if err != nil {
		g.logRefusal(r, "preview lookup", err)
		g.page(w, r, resultError, http.StatusServiceUnavailable, pageUnavailable)
		return h, false
	}
	a.project = view.Project
	switch err := h.Redeem(label, cookie, view); {
	case errors.Is(err, previewauth.ErrStateMismatch):
		g.page(w, r, resultStateCSRF, http.StatusBadRequest, pageSignInExpired)
		return h, false
	case err != nil:
		g.page(w, r, resultNoPreview, http.StatusNotFound, pageNoPreview)
		return h, false
	}
	allowed, err := g.s.review(ctx, h.Identity, view)
	switch {
	case errors.Is(err, previewauth.ErrNoProject) || err == nil && !allowed:
		g.page(w, r, resultDenied, http.StatusForbidden, pageDenied)
		return h, false
	case err != nil:
		g.logRefusal(r, "access review", err)
		g.page(w, r, resultError, http.StatusServiceUnavailable, pageUnavailable)
		return h, false
	}
	switch err := g.s.cfg.Ledger.Consume(ctx, previewauth.HandoffKey(h), time.Unix(h.Expires, 0)); {
	case errors.Is(err, previewauth.ErrReplayed):
		g.page(w, r, resultReplay, http.StatusBadRequest, pageSignInExpired)
		return h, false
	case err != nil:
		g.logRefusal(r, "code ledger", err)
		g.page(w, r, resultError, http.StatusServiceUnavailable, pageUnavailable)
		return h, false
	}
	return h, true
}

// page renders one of the gateway's own pages on a preview host, with the
// relay's envelope and COOP.
func (g *gateway) page(w http.ResponseWriter, r *http.Request, result string, status int, p pageData) {
	pageHeaders(w.Header())
	w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
	rec(r).result = result
	renderPage(w, status, p)
}

// proxyTarget is what the reverse proxy's Rewrite needs, carried on the
// request context.
type proxyTarget struct {
	upstream string
	fwd      previewauth.ForwardedFor
}

type targetKey struct{}

// forward proxies an authorised request to its route. The request's context
// ends at the host session's absolute end and is tracked, so a stream or an
// upgraded connection is closed when the session ends, a re-check refuses it
// or its Preview changes.
func (g *gateway) forward(w http.ResponseWriter, r *http.Request, view previewauth.View, route previewauth.Route,
	sess previewauth.HostSession, ip string) {
	if r.ContentLength > g.cfg.BodyLimit {
		g.page(w, r, resultTooLarge, http.StatusRequestEntityTooLarge, pageTooLarge)
		return
	}
	// A proxied request alone is not bounded by the connection deadline
	// handler set: a stream, an upload or an upgraded connection may run
	// as long as its host session.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})
	ctx, cancel := context.WithDeadline(r.Context(), time.Unix(sess.Expires, 0))
	defer cancel()
	id := g.streams.add(&tracked{label: view.Label, uid: view.UID, slot: view.Slot, project: view.Project,
		identity: sess.Identity, cancel: cancel})
	defer g.streams.remove(id)
	ctx = context.WithValue(ctx, targetKey{}, proxyTarget{upstream: route.Upstream(view.Slot),
		fwd: previewauth.ForwardedFor{Viewer: sess.Sub, ClientIP: ip, Host: r.Host}})
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, g.cfg.BodyLimit)
	}
	rec(r).result = resultProxied
	g.proxy.ServeHTTP(w, r.WithContext(ctx))
}

func (g *gateway) newProxy() *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			t, _ := pr.In.Context().Value(targetKey{}).(proxyTarget)
			pr.Out.URL.Scheme, pr.Out.URL.Host = "http", t.upstream
			pr.Out.Host = pr.In.Host
			previewauth.CleanRequest(pr.Out.Header, t.fwd)
		},
		Transport:     g.cfg.Transport,
		FlushInterval: -1,
		ModifyResponse: func(res *http.Response) error {
			previewauth.CleanResponse(res.StatusCode, res.Header)
			if g.cfg.HSTS {
				res.Header.Set("Strict-Transport-Security", "max-age=31536000")
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				g.page(w, r, resultTooLarge, http.StatusRequestEntityTooLarge, pageTooLarge)
				return
			}
			if r.Context().Err() == nil {
				g.s.log.LogAttrs(r.Context(), slog.LevelWarn, "preview gateway: upstream", slog.Any("error", err))
			}
			g.page(w, r, resultUpstreamDown, http.StatusBadGateway, pageUpstreamDown)
		},
		ErrorLog: slog.NewLogLogger(g.s.log.Handler(), slog.LevelDebug),
	}
}
