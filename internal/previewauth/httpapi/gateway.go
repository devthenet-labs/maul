// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"strings"
	"time"

	"github.com/bitwise-media-group/patchy/internal/previewauth"
)

// DefaultBodyLimit is a proxied request body's default cap.
const DefaultBodyLimit = 32 << 20

// DefaultRecheck is how often every live proxied stream is re-checked: the
// access review's cache lifetime, so a revocation reaches a stream as soon as
// it reaches a new request.
const DefaultRecheck = 20 * time.Second

// GatewayConfig turns the preview gateway on (design §2, §2.1, Revision 1):
// the sign-in host's /start and preview hosts <label>.<suffix>, each with its
// own host session, proxied to the Preview's served components.
type GatewayConfig struct {
	// Hosts is the preview host suffix (which must be the relay's) and the
	// sign-in host.
	Hosts     previewauth.Hosts
	Lifetimes previewauth.GatewayLifetimes
	// SignInUpstream is Dex with the sign-in host's /dex/callback as its
	// redirect URI. Required when the sign-in host is not the issuer's host;
	// nil shares the relay's Upstream otherwise.
	SignInUpstream previewauth.Upstream
	// RateLimit is applied to the sign-in host (when it is not the
	// issuer's), to unauthenticated preview-host requests and to handoff
	// callbacks; never to a signed-in viewer's traffic. Its ForwardedHops is
	// the gateway edge's.
	RateLimit RateLimit
	// InboundCIDRs, when set, is a second-layer allowlist of client
	// addresses (after ForwardedHops) for preview hosts. The edge should
	// enforce it first.
	InboundCIDRs []netip.Prefix
	// BodyLimit caps a proxied request body; 0 is DefaultBodyLimit.
	BodyLimit int64
	// Transport carries proxied requests to slot Services; nil is
	// NewUpstreamTransport(DefaultUpstreamTimeouts(), nil).
	Transport http.RoundTripper
	// HSTS adds Strict-Transport-Security to proxied responses.
	HSTS bool
	// Recheck is how often live streams are re-checked; 0 is DefaultRecheck.
	Recheck time.Duration
}

func (g GatewayConfig) validate(c Config) error {
	switch {
	case g.Hosts.Suffix() == "" || g.Hosts.SignIn() == "":
		return errors.New("no host grammar")
	case g.Hosts.Suffix() != c.Callbacks.Suffix():
		return fmt.Errorf("preview host suffix %s is not the relay's %s", g.Hosts.Suffix(), c.Callbacks.Suffix())
	case g.Hosts.SignIn() != strings.TrimPrefix(c.Issuer, "https://") && g.SignInUpstream == nil:
		return errors.New("a sign-in host other than the issuer's needs its own Dex client (SignInUpstream)")
	case g.BodyLimit < 0:
		return errors.New("negative body limit")
	case g.Recheck < 0:
		return errors.New("negative recheck interval")
	}
	return g.Lifetimes.Validate()
}

// UpstreamTimeouts bound the gateway's calls to slot Services.
type UpstreamTimeouts struct {
	Dial, ResponseHeader, Idle time.Duration
}

// DefaultUpstreamTimeouts are the design's: dial 5 s, response header 60 s,
// idle 90 s.
func DefaultUpstreamTimeouts() UpstreamTimeouts {
	return UpstreamTimeouts{Dial: 5 * time.Second, ResponseHeader: time.Minute, Idle: 90 * time.Second}
}

// DialFunc dials an upstream address.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// NewUpstreamTransport is the transport to slot Services: no proxy from the
// environment, no HTTP/2, bounded dial, header and idle times. dial, when
// set, replaces the network dialer (tests map Service DNS names to local
// listeners with it).
func NewUpstreamTransport(t UpstreamTimeouts, dial DialFunc) *http.Transport {
	if dial == nil {
		dial = (&net.Dialer{Timeout: t.Dial}).DialContext
	}
	return &http.Transport{
		Proxy: nil, DialContext: dial, ResponseHeaderTimeout: t.ResponseHeader, IdleConnTimeout: t.Idle,
		MaxIdleConnsPerHost: 16, ForceAttemptHTTP2: false, DisableCompression: true,
	}
}

// gateway is the preview gateway's HTTP surface.
type gateway struct {
	s   *Server
	cfg GatewayConfig
	// sharedHost: the sign-in host is the issuer's, so /start and the
	// gateway's Dex callback live on the relay's own mux.
	sharedHost bool
	upstream   previewauth.Upstream
	limiter    *limiter
	proxy      *httputil.ReverseProxy
	streams    *tracker
	signIn     http.Handler
}

func newGateway(s *Server, cfg GatewayConfig) *gateway {
	if cfg.BodyLimit == 0 {
		cfg.BodyLimit = DefaultBodyLimit
	}
	if cfg.Recheck == 0 {
		cfg.Recheck = DefaultRecheck
	}
	if cfg.Transport == nil {
		cfg.Transport = NewUpstreamTransport(DefaultUpstreamTimeouts(), nil)
	}
	g := &gateway{s: s, cfg: cfg, streams: newTracker(),
		sharedHost: cfg.Hosts.SignIn() == strings.TrimPrefix(s.cfg.Issuer, "https://")}
	g.upstream = cfg.SignInUpstream
	if g.upstream == nil {
		g.upstream = s.cfg.Upstream
	}
	g.limiter = newLimiter(cfg.RateLimit, s.now)
	g.proxy = g.newProxy()
	if !g.sharedHost {
		mux := http.NewServeMux()
		mux.HandleFunc("/start", s.only(http.MethodGet, g.handleStart))
		mux.HandleFunc("/dex/callback", s.only(http.MethodGet, g.handleSignInCallback))
		mux.HandleFunc("/logout", s.only(http.MethodPost, s.handleLogout))
		mux.HandleFunc("/", s.handleRoot)
		g.signIn = s.envelope(mux, g.limiter)
	}
	return g
}

// handler dispatches by Host: the edge health check on any host, a preview
// host to the proxy, the sign-in host to /start and its pages, and anything
// else to the relay exactly as before.
func (g *gateway) handler(relay http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A relay request on this connection may have left a write
		// deadline behind: the server has no WriteTimeout to reset it.
		_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
		if r.URL.Path == previewauth.HealthPath {
			g.health(w, r)
			return
		}
		if label, ok := g.cfg.Hosts.Preview(r.Host); ok {
			g.preview(w, r, label)
			return
		}
		if !g.sharedHost && g.cfg.Hosts.IsSignIn(r.Host) {
			g.signIn.ServeHTTP(w, r)
			return
		}
		relay.ServeHTTP(w, r)
	})
}

// health is the reserved /.patchy/healthz: 200 on every host, never
// forwarded, never rate limited.
func (g *gateway) health(w http.ResponseWriter, r *http.Request) {
	pageHeaders(w.Header())
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write([]byte("ok\n"))
	}
}

// inbound reports whether ip may reach a preview host: always without an
// allowlist, else only from inside one of its prefixes.
func (g *gateway) inbound(ip string) bool {
	if len(g.cfg.InboundCIDRs) == 0 {
		return true
	}
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	for _, p := range g.cfg.InboundCIDRs {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// logRefusal logs a gateway refusal that is the operator's to see.
func (g *gateway) logRefusal(r *http.Request, what string, err error) {
	g.s.log.LogAttrs(r.Context(), slog.LevelWarn, "preview gateway: "+what, slog.Any("error", err))
}
