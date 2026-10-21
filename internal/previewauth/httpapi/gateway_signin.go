// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/devthenet-labs/maul/internal/previewauth"
)

// handleStart is the sign-in host's /start?h=<label>&s=<sealed state>: a
// preview host sent the browser here to sign in. With a relay session the
// access review runs and a handoff code goes back to the preview host's
// /.maul/callback; without one the viewer signs in at Dex first. Every
// refusal is a page here, never a redirect.
func (g *gateway) handleStart(w http.ResponseWriter, r *http.Request) {
	a := rec(r)
	ctx := r.Context()
	q, err := query(r)
	if err != nil || hasRepeats(q) {
		g.startFail(w, r, resultBadRequest, http.StatusBadRequest, pageBadRequest)
		return
	}
	label, sealedState := q.Get("h"), q.Get("s")
	if !previewauth.ValidLabel(label) || sealedState == "" || len(sealedState) > previewauth.MaxStateBytes {
		g.startFail(w, r, resultBadRequest, http.StatusBadRequest, pageBadRequest)
		return
	}
	a.label = label
	st, err := g.s.cfg.Ring.OpenState(sealedState, g.s.now())
	if err != nil {
		g.startFail(w, r, "expired", http.StatusBadRequest, pageSignInExpired)
		return
	}
	view, err := g.s.cfg.Lookup.ByLabel(ctx, label)
	if err != nil {
		g.s.log.LogAttrs(ctx, slog.LevelError, "preview lookup failed", slog.String("label", label),
			slog.Any("error", err))
		g.startFail(w, r, resultError, http.StatusServiceUnavailable, pageUnavailable)
		return
	}
	if st.Start(label, view) != nil {
		g.startFail(w, r, resultNoPreview, http.StatusNotFound, pageNoPreview)
		return
	}
	a.slot, a.project = view.Slot, view.Project
	if c, err := r.Cookie(previewauth.RelaySessionCookie); err == nil {
		if sess, err := g.s.cfg.Ring.OpenSession(c.Value, g.s.now()); err == nil && sess.ID != "" {
			g.handoff(w, r, sealedState, st, view, sess)
			return
		}
	}
	g.startLogin(w, r, label, sealedState)
}

func (g *gateway) startFail(w http.ResponseWriter, r *http.Request, result string, status int, p pageData) {
	rec(r).result = result
	count(r.Context(), gatewayCounter, resultAttr("start_"+result))
	renderPage(w, status, p)
}

// startLogin sends the browser to Dex with a sealed gateway login state and
// its own login cookie.
func (g *gateway) startLogin(w http.ResponseWriter, r *http.Request, label, sealedState string) {
	gl, csrf, err := previewauth.NewGatewayLogin(g.s.now(), g.s.cfg.Lifetimes, label, sealedState)
	if err != nil {
		g.startUnavailable(w, r, "start sign-in", err)
		return
	}
	state, err := g.s.cfg.Ring.SealGatewayLogin(gl)
	if err != nil {
		g.startUnavailable(w, r, "seal sign-in state", err)
		return
	}
	loc, err := g.upstream.AuthURL(r.Context(), state, gl.UpstreamNonce, gl.UpstreamChallenge())
	if err != nil {
		g.startUnavailable(w, r, "identity provider", err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: previewauth.LoginCookieName(state), Value: csrf, Path: "/",
		MaxAge: int(g.s.cfg.Lifetimes.Login.Seconds()), Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	rec(r).result = resultLogin
	count(r.Context(), gatewayCounter, resultAttr("start_"+resultLogin))
	http.Redirect(w, r, loc, http.StatusFound)
}

func (g *gateway) startUnavailable(w http.ResponseWriter, r *http.Request, what string, err error) {
	g.s.log.LogAttrs(r.Context(), slog.LevelError, "preview gateway sign-in: "+what, slog.Any("error", err))
	g.startFail(w, r, resultError, http.StatusServiceUnavailable, pageUnavailable)
}

// handoff runs the access review for a signed-in viewer and sends the
// browser back to the preview host with a handoff code.
func (g *gateway) handoff(w http.ResponseWriter, r *http.Request, sealedState string, st previewauth.State,
	view previewauth.View, sess previewauth.Session) {
	ctx := r.Context()
	allowed, err := g.s.review(ctx, sess.Identity, view)
	switch {
	case errors.Is(err, previewauth.ErrNoProject) || err == nil && !allowed:
		g.startFail(w, r, resultDenied, http.StatusForbidden, pageDenied)
		return
	case err != nil:
		g.startUnavailable(w, r, "access review", err)
		return
	}
	now := g.s.now()
	code, err := g.s.cfg.Ring.IssueHandoff(now, g.cfg.Lifetimes, sealedState, st, view, sess)
	if err != nil {
		g.startUnavailable(w, r, "issue handoff", err)
		return
	}
	if h, err := g.s.cfg.Ring.OpenHandoff(code, now); err == nil {
		rec(r).bind(h.Bound, h.Sub, h.JTI)
	}
	rec(r).result = resultIssued
	count(ctx, gatewayCounter, resultAttr("start_"+resultIssued))
	http.Redirect(w, r, g.cfg.Hosts.HandoffURL(view.Label, code), http.StatusFound)
}

// handleSignInCallback is /dex/callback on a sign-in host of its own: only a
// gateway login state is accepted there.
func (g *gateway) handleSignInCallback(w http.ResponseWriter, r *http.Request) {
	q, err := query(r)
	if err != nil || hasRepeats(q) || q.Get("state") == "" {
		rec(r).result = resultBadRequest
		count(r.Context(), upstreamCounter, resultAttr(resultBadRequest))
		renderPage(w, http.StatusBadRequest, pageBadRequest)
		return
	}
	g.finishLogin(w, r, q)
}

// finishLogin completes a gateway sign-in at Dex: the double-submit check,
// the code exchange and a relay session, then the /start request resumes
// against the same Preview.
func (g *gateway) finishLogin(w http.ResponseWriter, r *http.Request, q url.Values) {
	ctx := r.Context()
	a := rec(r)
	login := func(result string, status int, p pageData) {
		a.result = result
		count(ctx, upstreamCounter, resultAttr(result))
		renderPage(w, status, p)
	}
	state := q.Get("state")
	gl, err := g.s.cfg.Ring.OpenGatewayLogin(state, g.s.now())
	if err != nil {
		login("expired", http.StatusBadRequest, pageSignInExpired)
		return
	}
	cookieName := previewauth.LoginCookieName(state)
	c, err := r.Cookie(cookieName)
	if err != nil || gl.CheckCSRF(c.Value) != nil {
		login("csrf", http.StatusBadRequest, pageSignInExpired)
		return
	}
	http.SetCookie(w, expire(cookieName))
	a.label = gl.Label
	if q.Has("error") {
		login("refused", http.StatusForbidden, pageUpstreamRefused)
		return
	}
	id, err := g.upstream.Exchange(ctx, q.Get("code"), gl.UpstreamVerifier, gl.UpstreamNonce)
	if err != nil {
		g.s.log.LogAttrs(ctx, slog.LevelWarn, "preview sign-in: identity provider exchange", slog.Any("error", err))
		login("failed", http.StatusBadGateway, pageUpstreamFailed)
		return
	}
	now := g.s.now()
	sess, err := previewauth.NewSession(now, g.s.cfg.Lifetimes, id)
	if err == nil {
		sess, err = sess.Named()
	}
	if err != nil {
		login("failed", http.StatusBadGateway, pageUpstreamFailed)
		return
	}
	tok, err := g.s.cfg.Ring.SealSession(sess)
	if errors.Is(err, previewauth.ErrIdentityTooLarge) {
		login("too_large", http.StatusBadRequest, pageTooManyGroups)
		return
	}
	if err != nil {
		login(resultError, http.StatusServiceUnavailable, pageUnavailable)
		return
	}
	http.SetCookie(w, g.s.sessionCookie(tok))
	count(ctx, upstreamCounter, resultAttr("ok"))

	// Resume the /start request against the same Preview.
	st, err := g.s.cfg.Ring.OpenState(gl.State, now)
	if err != nil || st.Label != gl.Label {
		g.startFail(w, r, "expired", http.StatusBadRequest, pageSignInExpired)
		return
	}
	view, err := g.s.cfg.Lookup.ByLabel(ctx, st.Label)
	if err != nil {
		g.startUnavailable(w, r, "preview lookup", err)
		return
	}
	if st.Start(st.Label, view) != nil {
		g.startFail(w, r, resultNoPreview, http.StatusNotFound, pageNoPreview)
		return
	}
	a.slot, a.project = view.Slot, view.Project
	g.handoff(w, r, gl.State, st, view, sess)
}
