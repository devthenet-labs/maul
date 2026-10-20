// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"

	"go.opentelemetry.io/otel/attribute"

	"github.com/devthenet-labs/maul/internal/httpguard"
	"github.com/devthenet-labs/maul/internal/netprofile"
	"github.com/devthenet-labs/maul/internal/version"
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

	// registries are the /r/ routes' upstreams by name; public is the
	// client for file hosts and redirect hops (the public dial policy, no
	// credential ever).
	registries map[string]*registryRoute
	public     *http.Client
	// selfChecks are the credential self-checks in flight.
	selfChecks sync.WaitGroup

	// The CONNECT tunnel (step 3): its own limiter, its dialer (the same
	// guarded dialer, under a policy with the tunnel-only deny ranges
	// added), each tunnel profile's per-source budgets, and the open
	// tunnels, which Shutdown closes (CloseTunnels) and DrainTunnels
	// waits out.
	tunnelLimits *limiter
	tunnelDialer *guardedDialer
	budgets      map[string]*budgetTable
	tunnels      *tunnelSet
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
	s := &Server{
		cfg: cfg,
		log: log,
		client: &http.Client{
			Transport: newTransport(dialer, cfg.Limits.HeaderTimeout),
			// Never follow a redirect: a 3xx becomes a 502 and no
			// Location ever leaves the proxy.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		limits: newLimiter(cfg.Limits, cfg.Now),
		ua:     "maul-egress-proxy/" + version.Version,
	}
	s.buildRegistries()
	s.buildTunnel()
	return s, nil
}

// buildTunnel sets up the tunnel's limiter, dialer and budgets. The
// dialer's policy is the Go route's (built-in ranges and --deny-cidrs)
// plus the always-denied ranges and the tunnel-only deny ranges (the API
// server's addresses); the Go route's own dial never reads the last two.
func (s *Server) buildTunnel() {
	l := s.cfg.TunnelLimits
	policy := AddrPolicy{testLoopback: s.cfg.Policy.testLoopback}
	for _, set := range [][]netip.Prefix{s.cfg.Policy.Deny, netprofile.AlwaysDeniedCIDRs, s.cfg.AlwaysDeny, l.Deny} {
		policy.Deny = append(policy.Deny, set...)
	}
	s.tunnelLimits = newLimiter(l.limits(), s.cfg.Now)
	s.tunnelDialer = newGuardedDialer(policy, s.cfg.lookup)
	s.tunnelDialer.rewrite = s.cfg.dialRewrite
	s.tunnels = newTunnelSet()
	s.budgets = map[string]*budgetTable{}
	for _, p := range s.cfg.Profiles.Profiles() {
		if p.HasTunnel() {
			s.budgets[p.Name] = newBudgetTable(p, l, s.cfg.Now)
		}
	}
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
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A CONNECT reaches the tunnel only on a profile with hosts; on
		// every other profile it gets step 1's answer, byte for byte.
		if r.Method == http.MethodConnect && p.HasTunnel() {
			s.serveConnect(p, w, r)
			return
		}
		s.serve(p, w, r)
	}), nil
}

// tunnelOnly reports a profile whose listener serves the tunnel and
// nothing else: no Go route and no registry grant.
func tunnelOnly(p netprofile.Profile) bool {
	return p.HasTunnel() && !p.HasGoRoute() && len(p.Registries) == 0
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
		var extra []attribute.KeyValue
		if e.reg != nil {
			extra = []attribute.KeyValue{attribute.String("ecosystem", e.reg.ecosystem),
				attribute.String("registry", e.reg.registry)}
		}
		countRequest(ctx, p.Name, e.route, e.outcome, cw.Bytes, extra...)
	}()

	if !s.envelope(p, cw, r, e) {
		return
	}
	// A profile holding registry grants serves /r/; for every other
	// profile /r/ is outside the protocol, exactly as in step 1.
	if len(p.Registries) > 0 && strings.HasPrefix(r.URL.EscapedPath(), registryPrefix) {
		s.serveRegistry(p, cw, r, e)
		return
	}
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

// envelope runs the checks every route shares (step 1's pipeline steps
// 0-2): suspended, method, body, absolute form and query. It answers every
// refusal itself.
func (s *Server) envelope(p netprofile.Profile, w http.ResponseWriter, r *http.Request, e *auditEntry) bool {
	if s.cfg.Suspended {
		refuse(w, e, http.StatusGone, OutcomeSuspended, "", suspendedPhrase)
		return false
	}
	if tunnelOnly(p) {
		w.Header().Set("Allow", http.MethodConnect)
		refuse(w, e, http.StatusMethodNotAllowed, OutcomeMethod, "", "egress-proxy: only CONNECT")
		return false
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		refuse(w, e, http.StatusMethodNotAllowed, OutcomeMethod, "", "egress-proxy: only GET and HEAD")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 0)
	switch {
	case r.ContentLength != 0 || len(r.TransferEncoding) > 0:
		refuse(w, e, http.StatusBadRequest, OutcomeBadRequest, "body", "egress-proxy: a request carries no body")
		return false
	case r.URL.IsAbs() || r.URL.Host != "":
		refuse(w, e, http.StatusBadRequest, OutcomeBadRequest, "absolute_uri",
			"egress-proxy: absolute-form request URIs are refused")
		return false
	case r.URL.RawQuery != "" || r.URL.ForceQuery:
		refuse(w, e, http.StatusBadRequest, OutcomeBadRequest, "query", "egress-proxy: a request carries no query")
		return false
	}
	return true
}

// admit runs the /go route's checks up to the limits: shape, parse, a go
// grant's private modules, allowlist and version gate. It answers every
// refusal itself and returns ok only for a request to serve.
func (s *Server) admit(p netprofile.Profile, w http.ResponseWriter, r *http.Request, e *auditEntry) (Request, bool) {
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
	// A go grant's private modules never reach the public proxy or
	// checksum database: the go command falls through to this route after
	// the registry's 404, and the more specific private prefix wins over
	// the profile's public modules.
	if g, ok := p.GrantFor(netprofile.EcosystemGo); ok && req.ModuleChecked() &&
		g.PrivateModulePrefixes().Allows(req.Module) {
		refuse(w, e, http.StatusGone, OutcomePrivateModule, ReasonPrivateModule,
			fmt.Sprintf("egress-proxy: module path %s is private to registry %s and is never fetched from "+
				"the public module proxy or checksum database", clip(req.Module), g.Name))
		return Request{}, false
	}
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
