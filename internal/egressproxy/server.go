// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/bitwise-media-group/patchy/internal/httpguard"
	"github.com/bitwise-media-group/patchy/internal/netprofile"
	"github.com/bitwise-media-group/patchy/internal/version"
)

// routePrefix is the path every profile listener serves below.
const routePrefix = "/go/"

// sumdbPrefix starts a checksum-database path below routePrefix.
const sumdbPrefix = "sumdb/"

// Server is the egress proxy engine: one handler per profile, a health
// handler, and the shared limits and outbound client.
type Server struct {
	cfg    Config
	log    *slog.Logger
	client *http.Client
	limits *limiter
	ua     string
	ready  atomic.Bool
}

// New validates cfg and builds the engine.
func New(cfg Config, log *slog.Logger) (*Server, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	dialer := newGuardedDialer(cfg.Policy, cfg.lookup)
	return &Server{
		cfg: cfg,
		log: log,
		client: &http.Client{
			Transport: newTransport(dialer, cfg.Limits.HeaderTimeout),
			// Never follow a redirect: a 3xx becomes a 502 and no
			// Location ever leaves the proxy.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		limits: newLimiter(cfg.Limits, cfg.Now),
		ua:     "patchy-egress-proxy/" + version.Version,
	}, nil
}

// Profiles is the configured profile set.
func (s *Server) Profiles() netprofile.Set { return s.cfg.Profiles }

// Handler returns the handler for the profile called name, the only one its
// listener serves.
func (s *Server) Handler(name string) (http.Handler, error) {
	p, ok := s.cfg.Profiles.Get(name)
	if !ok {
		return nil, fmt.Errorf("egress proxy: no profile %q", name)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.serve(p, w, r) }), nil
}

// SetReady marks every profile listener bound, so /readyz answers 200.
func (s *Server) SetReady() { s.ready.Store(true) }

// HealthHandler serves /healthz (always 200) and /readyz (200 once SetReady
// has been called). Neither calls an upstream, so probes cannot amplify.
func (s *Server) HealthHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !s.ready.Load() {
			http.Error(w, "profile listeners not bound", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

// serve runs one request through the pipeline and audits it, whatever the
// result (an aborted stream included: the deferred audit runs while the
// abort panic unwinds).
func (s *Server) serve(p netprofile.Profile, w http.ResponseWriter, r *http.Request) {
	start := s.cfg.Now()
	ctx := r.Context()
	cw := &httpguard.CountingWriter{ResponseWriter: w}
	e := &auditEntry{
		profile:  p.Name,
		port:     p.Port,
		sourceIP: httpguard.SourceIP(r),
		method:   r.Method,
		route:    "none",
	}
	if s.cfg.Pods != nil {
		if info, ok := s.cfg.Pods.Lookup(e.sourceIP); ok {
			e.pod = info
		}
	}
	countInflight(ctx, p.Name, 1)
	defer func() {
		countInflight(ctx, p.Name, -1)
		e.status, e.bytes, e.elapsed = cw.Status, cw.Bytes, s.cfg.Now().Sub(start)
		audit(ctx, s.log, *e)
		countRequest(ctx, p.Name, e.route, e.outcome, cw.Bytes)
	}()

	req, ok := s.admit(p, cw, r, e)
	if !ok {
		return
	}
	if req.Kind == KindSupported {
		// The go command takes the first GOPROXY entry whose
		// sumdb/<name>/supported answers 200 as the database's proxy.
		e.outcome, e.reason = OutcomeProxied, ReasonLocal
		cw.WriteHeader(http.StatusOK)
		return
	}
	release, reason := s.limits.acquire(ctx, e.sourceIP)
	if reason != "" {
		refuse(cw, e, http.StatusServiceUnavailable, OutcomeLimited, reason,
			"egress-proxy: over the request limit ("+reason+"); retry later")
		return
	}
	defer release()
	s.forward(cw, r, req, e)
}

// admit runs the pipeline's checks up to the limits: suspended, method,
// body, shape, parse, allowlist and version gate. It answers every refusal
// itself and returns ok only for a request to serve.
func (s *Server) admit(p netprofile.Profile, w http.ResponseWriter, r *http.Request, e *auditEntry) (Request, bool) {
	if s.cfg.Suspended {
		refuse(w, e, http.StatusGone, OutcomeSuspended, "",
			"egress-proxy: profile routes are suspended by the operator")
		return Request{}, false
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		refuse(w, e, http.StatusMethodNotAllowed, OutcomeMethod, "", "egress-proxy: only GET and HEAD")
		return Request{}, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 0)
	switch {
	case r.ContentLength != 0 || len(r.TransferEncoding) > 0:
		refuse(w, e, http.StatusBadRequest, OutcomeBadRequest, "body", "egress-proxy: a request carries no body")
		return Request{}, false
	case r.URL.IsAbs() || r.URL.Host != "":
		refuse(w, e, http.StatusBadRequest, OutcomeBadRequest, "absolute_uri",
			"egress-proxy: absolute-form request URIs are refused")
		return Request{}, false
	case r.URL.RawQuery != "" || r.URL.ForceQuery:
		refuse(w, e, http.StatusBadRequest, OutcomeBadRequest, "query", "egress-proxy: a request carries no query")
		return Request{}, false
	}
	req, err := parsePath(r)
	if err != nil {
		var pe *PathError
		status := http.StatusGone
		reason := err.Error()
		if errors.As(err, &pe) && pe.NotFound {
			status, reason = http.StatusNotFound, ReasonNotFound
		}
		refuse(w, e, status, OutcomeBadPath, reason, "egress-proxy: "+err.Error())
		return Request{}, false
	}
	e.route, e.kind, e.module, e.version = string(req.Route), string(req.Kind), req.Module, req.Version
	if req.ModuleChecked() && !p.Modules.Allows(req.Module) {
		refuse(w, e, http.StatusGone, OutcomeNotAllowed, ReasonPrefix,
			fmt.Sprintf("egress-proxy: module path %s is not allowed by profile %s; allowed prefixes: %s",
				clip(req.Module), p.Name, p.Modules))
		return Request{}, false
	}
	if req.VersionGated() && !VersionAllowed(req.Version, p.AllowPseudoVersions) {
		msg := fmt.Sprintf("egress-proxy: version %s of %s is not a tagged release; profile %s admits tagged releases only",
			clip(req.Version), clip(req.Module), p.Name)
		if p.AllowPseudoVersions {
			msg = fmt.Sprintf("egress-proxy: version %s of %s is not a canonical version; profile %s admits "+
				"tagged releases and pseudo-versions only", clip(req.Version), clip(req.Module), p.Name)
		}
		refuse(w, e, http.StatusGone, OutcomeNotAllowed, ReasonVersion, msg)
		return Request{}, false
	}
	return req, true
}

// parsePath checks the request path's envelope (the /go/ prefix, no
// percent-encoding, the length) and parses what is below it.
func parsePath(r *http.Request) (Request, error) {
	rest, ok := strings.CutPrefix(r.URL.Path, routePrefix)
	if !ok {
		return Request{}, &PathError{NotFound: true, Rule: "not below " + routePrefix}
	}
	// RawPath is set exactly when the path arrived in an escaping other
	// than net/url's canonical one, which is the go command's: a percent
	// escape of a plain character, or a raw character it would escape.
	if r.URL.RawPath != "" {
		return Request{}, badPath("path is not in canonical escaping")
	}
	if len(r.URL.Path) > MaxPathLen {
		return Request{}, badPath("path too long")
	}
	if db, ok := strings.CutPrefix(rest, sumdbPrefix); ok {
		return ParseSumDB(db)
	}
	return ParseGoProxy(rest)
}

// refuse answers a refusal: the status and a one-line reason the go command
// shows its user verbatim.
func refuse(w http.ResponseWriter, e *auditEntry, status int, outcome, reason, msg string) {
	e.outcome, e.reason = outcome, reason
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = fmt.Fprintln(w, msg)
}
