// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/mod/module"

	"github.com/bitwise-media-group/patchy/internal/httpguard"
	"github.com/bitwise-media-group/patchy/internal/netprofile"
)

// registryPrefix starts every registry route: /r/<registry>/....
const registryPrefix = "/r/"

// The registry routes' defaults and fixed caps (§2.7 of the step-2 spec).
const (
	// DefaultMaxArtifactBytes caps an npm tarball, a pip file or a go zip
	// on a registry route (--max-artifact-bytes).
	DefaultMaxArtifactBytes = 512 << 20
	// DefaultSelfCheckInterval is the least time between two credential
	// self-checks of one registry (the credential poll interval's default).
	DefaultSelfCheckInterval = 30 * time.Second

	maxPackumentBytes  = 64 << 20
	maxPyPageBytes     = 16 << 20
	maxPyMetadataBytes = 16 << 20
	// echoPrefixBytes is how much of a scanned body is read before the
	// first byte is written: an echo inside it is answered 502, one later
	// aborts the stream.
	echoPrefixBytes = 64 << 10
	// selfCheckRun 403s from one registry within selfCheckWindow trigger
	// the credential self-check.
	selfCheckRun    = 3
	selfCheckWindow = time.Minute
)

// The registry request kinds the audit line names, beside step 1's.
const (
	kindPackument = "packument"
	kindTarball   = "tarball"
	kindPage      = "page"
	kindFile      = "file"
	kindMetadata  = "metadata"
)

// column is which ecosystem's refusal and status table applies.
type column int

const (
	colGo column = iota
	colNPM
	colPyPI
)

func columnFor(ecosystem string) column {
	switch ecosystem {
	case netprofile.EcosystemNPM:
		return colNPM
	case netprofile.EcosystemPyPI:
		return colPyPI
	}
	return colGo
}

// registryRoute is one configured registry's upstream.
type registryRoute struct {
	reg  netprofile.Registry
	base *url.URL
	// client is the registry's own transport: its dial policy (internal
	// admission) and its CA. It never follows a redirect.
	client      *http.Client
	maxArtifact int64

	// The credential self-check's state.
	mu           sync.Mutex
	refused      int
	firstRefused time.Time
	lastCheck    time.Time
}

// buildRegistries makes one transport per registry, and the public client
// file hosts and redirect hops use.
func (s *Server) buildRegistries() {
	cfg := s.cfg
	s.registries = map[string]*registryRoute{}
	for _, r := range cfg.Registries.Registries() {
		base, _ := r.BaseURL() // valid: the set was validated
		roots := cfg.rootCAs
		if r.CA {
			roots = cfg.RegistryCAs[r.Name]
		}
		d := newGuardedDialer(cfg.Policy.ForRegistry(r, cfg.AlwaysDeny), cfg.lookup)
		d.rewrite = cfg.dialRewrite
		maxArtifact := r.MaxArtifactBytes
		if maxArtifact == 0 {
			maxArtifact = cfg.MaxArtifactBytes
		}
		s.registries[r.Name] = &registryRoute{reg: r, base: base, maxArtifact: maxArtifact, client: &http.Client{
			Transport:     newTransportRoots(d, cfg.Limits.HeaderTimeout, roots),
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}}
	}
	d := newGuardedDialer(cfg.Policy.ForRegistry(netprofile.Registry{}, cfg.AlwaysDeny), cfg.lookup)
	d.rewrite = cfg.dialRewrite
	s.public = &http.Client{
		Transport:     newTransportRoots(d, cfg.Limits.HeaderTimeout, cfg.rootCAs),
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// regReq is one admitted registry request.
type regReq struct {
	// target is the upstream URL: Path decoded and RawPath exactly the
	// escaped form the grammar admitted.
	target *url.URL
	// origin0 sends the registry's credential: the target is the
	// registry's own origin. Never true for a file host or the checksum
	// database.
	origin0 bool
	// sumdb forwards to the public checksum database on step 1's client.
	sumdb bool
	// local answers 200 with no upstream contact (sumdb supported).
	local bool
	// probe, when set, is a credentialed HEAD that must answer 200 before
	// the request is forwarded (a go registry's checksum lookup).
	probe *url.URL
	// accept is the outbound Accept ("" sends none).
	accept string
	// limit caps the body; scan echo-scans it (every kind but an archive).
	limit int64
	scan  bool
	// page rewrites a pip simple page (buffered).
	page func(body []byte, final *url.URL, e *auditEntry) ([]byte, error)
	// what names the thing asked for in a proxy-written 404.
	what string
}

// serveRegistry runs a /r/<registry>/... request: grant check, the
// ecosystem's grammar and package policy, the limits, the credential, the
// forward and the status table.
func (s *Server) serveRegistry(p netprofile.Profile, w *httpguard.CountingWriter, r *http.Request, e *auditEntry) {
	ra := &registryAudit{}
	e.reg, e.route = ra, "r"
	rest := strings.TrimPrefix(r.URL.EscapedPath(), registryPrefix)
	name, sub, _ := strings.Cut(rest, "/")
	g, granted := p.Grant(name)
	rt := s.registries[name]
	if !granted || rt == nil {
		// The name is never echoed: it is not one of this profile's.
		status, col := http.StatusForbidden, colNPM
		if goShaped(sub) {
			status, col = http.StatusGone, colGo
		}
		refuseFor(w, e, col, status, OutcomeNotGranted, "",
			fmt.Sprintf("egress-proxy: profile %s holds no grant for this registry route", p.Name))
		return
	}
	e.route, ra.registry, ra.ecosystem = "r/"+name, name, g.Ecosystem
	col := columnFor(g.Ecosystem)
	var req regReq
	var ok bool
	switch col {
	case colNPM:
		req, ok = s.admitNPM(g, rt, w, r, "/"+sub, e)
	case colPyPI:
		refuseFor(w, e, colPyPI, http.StatusForbidden, OutcomeBadPath, "unsupported",
			"egress-proxy: this registry route is not served yet")
		return
	default:
		req, ok = s.admitGoRegistry(p, g, rt, w, r, e)
	}
	if !ok {
		return
	}
	if req.local {
		e.outcome, e.reason = OutcomeProxied, ReasonLocal
		w.WriteHeader(http.StatusOK)
		return
	}
	ctx := r.Context()
	release, reason := s.limits.acquire(ctx, e.sourceIP)
	if reason != "" {
		refuseFor(w, e, col, http.StatusServiceUnavailable, OutcomeLimited, reason,
			"egress-proxy: over the request limit ("+reason+"); retry later")
		return
	}
	defer release()
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Limits.RequestTimeout)
	defer cancel()
	var cred Credential
	if req.origin0 || req.probe != nil {
		c, err := s.credential(rt)
		if err != nil {
			refuseFor(w, e, col, http.StatusServiceUnavailable, OutcomeCredentialUnavailable, "",
				"egress-proxy: the credential of registry "+name+" is unavailable; retry later or ask the operator")
			return
		}
		cred = c
	}
	if req.probe != nil && !s.probeModule(ctx, rt, req.probe, cred) {
		refuseFor(w, e, col, http.StatusGone, OutcomeChecksumNotPublic, "registry_probe",
			checksumNotPublicMsg(e.module, name))
		return
	}
	if !req.origin0 {
		cred = NoCredential()
	}
	s.forwardRegistry(ctx, w, r, rt, col, req, cred, e)
}

// goShaped reports whether a path below a registry name is a GOPROXY or
// checksum-database path, which a refusal answers 410 so the go command
// falls through.
func goShaped(sub string) bool {
	return strings.Contains(sub, "/@v/") || strings.HasSuffix(sub, "/@latest") || strings.HasPrefix(sub, sumdbPrefix)
}

// credential is the registry's credential: none for auth none, else the
// source's.
func (s *Server) credential(rt *registryRoute) (Credential, error) {
	if rt.reg.Auth.Type == netprofile.AuthNone {
		return NoCredential(), nil
	}
	if s.cfg.Credentials == nil {
		return Credential{}, ErrCredentialUnavailable
	}
	c, err := s.cfg.Credentials.Get(rt.reg.Name)
	if err != nil {
		return Credential{}, err
	}
	if !c.Usable(s.cfg.Now()) {
		return Credential{}, ErrCredentialUnavailable
	}
	return c, nil
}

// joinBase is base's path followed by the escaped remainder, with RawPath
// set so the remainder leaves exactly as admitted (a scoped packument's
// %2f, a wheel's %2B). It refuses a remainder net/url would not send
// verbatim.
func joinBase(base *url.URL, escaped string) (*url.URL, error) {
	decoded, err := url.PathUnescape(escaped)
	if err != nil {
		return nil, err
	}
	rawBase := strings.TrimSuffix(base.EscapedPath(), "/")
	u := &url.URL{Scheme: base.Scheme, Host: base.Host, Path: strings.TrimSuffix(base.Path, "/") + decoded,
		RawPath: rawBase + escaped}
	if u.EscapedPath() != rawBase+escaped {
		return nil, fmt.Errorf("path %q does not leave as escaped", escaped)
	}
	return u, nil
}

// refuseFor answers a refusal in the ecosystem's form: npm a JSON
// {"error": ...} body npm prints, everything else step 1's text line.
func refuseFor(w http.ResponseWriter, e *auditEntry, col column, status int, outcome, reason, msg string) {
	if col != colNPM {
		refuse(w, e, status, outcome, reason, msg)
		return
	}
	e.outcome, e.reason = outcome, reason
	body, _ := json.Marshal(map[string]string{"error": msg})
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}

// fetch sends the request and follows at most MaxRedirects hops to the
// registry's redirectHosts, each a fresh request with no credential on
// the public dial policy. It answers every failure itself and returns nil;
// otherwise the response (never a 3xx) and the URL that gave it.
func (s *Server) fetch(ctx context.Context, w http.ResponseWriter, method string, rt *registryRoute, col column,
	req regReq, cred Credential, e *auditEntry,
) (*http.Response, *url.URL) {
	client := s.public
	switch {
	case req.sumdb:
		client = s.client
	case req.origin0:
		client = rt.client
	}
	target := req.target
	for hop := 0; ; hop++ {
		out, err := http.NewRequestWithContext(ctx, method, target.String(), http.NoBody)
		if err != nil {
			refuseFor(w, e, col, http.StatusBadGateway, OutcomeUpstreamError, "request",
				"egress-proxy: cannot build the request")
			return nil, nil
		}
		out.URL, out.Host = target, target.Host
		out.Header = http.Header{"User-Agent": {s.ua}, "Accept-Encoding": {"identity"}}
		if req.accept != "" {
			out.Header.Set("Accept", req.accept)
		}
		if hop == 0 && req.origin0 {
			if a := cred.Authorization(); a != "" {
				out.Header.Set("Authorization", a)
				e.reg.credential = cred.Kind()
			}
		}
		resp, err := client.Do(out)
		if err != nil {
			reason := dialReason(err)
			refuseFor(w, e, col, http.StatusBadGateway, OutcomeUpstreamError, reason,
				"egress-proxy: upstream unreachable ("+reason+")")
			return nil, nil
		}
		if req.sumdb || !isRedirect(resp.StatusCode) {
			return resp, target
		}
		e.upstreamStatus = resp.StatusCode
		loc := resp.Header.Get("Location")
		_ = resp.Body.Close()
		if hop == MaxRedirects {
			refuseFor(w, e, col, http.StatusBadGateway, OutcomeRedirectRefused, "too_many_hops",
				"egress-proxy: the registry redirected more than "+fmt.Sprint(MaxRedirects)+" times")
			return nil, nil
		}
		v := judgeRedirect(rt.reg, target, loc, s.cfg.DenyHosts)
		e.reg.redirectHost = v.host
		if v.next == nil {
			refuseFor(w, e, col, http.StatusBadGateway, v.outcome, v.reason,
				"egress-proxy: the registry redirected to a host the operator has not listed for it")
			return nil, nil
		}
		e.reg.redirects++
		target, client = v.next, s.public
	}
}

// forwardRegistry fetches the admitted request and answers by the status
// table of §2.2 step 7: 200 passes (capped, echo-scanned, a pip page
// rewritten), every other answer gets a body the proxy writes.
func (s *Server) forwardRegistry(ctx context.Context, w *httpguard.CountingWriter, r *http.Request,
	rt *registryRoute, col column, req regReq, cred Credential, e *auditEntry,
) {
	method := r.Method
	if req.page != nil {
		method = http.MethodGet
	}
	resp, final := s.fetch(ctx, w, method, rt, col, req, cred, e)
	if resp == nil {
		return
	}
	defer func() { _ = resp.Body.Close() }()
	e.upstreamStatus = resp.StatusCode
	if req.origin0 {
		s.noteStatus(rt, resp.StatusCode)
	}
	name := rt.reg.Name
	switch resp.StatusCode {
	case http.StatusOK:
		s.writeOK(ctx, w, r, resp, final, col, req, cred, e)
	case http.StatusNotFound, http.StatusGone:
		status := http.StatusNotFound
		if col == colGo && resp.StatusCode == http.StatusGone {
			status = http.StatusGone
		}
		refuseFor(w, e, col, status, OutcomeProxied, ReasonNotFound,
			fmt.Sprintf("egress-proxy: %s: not found in registry %s", clip(req.what), name))
	case http.StatusUnauthorized:
		if req.origin0 {
			s.cfg.Credentials.Invalidate(name)
		}
		refuseFor(w, e, col, http.StatusBadGateway, OutcomeUpstreamAuth, "",
			"egress-proxy: registry "+name+" refused the operator's credential; ask the operator")
	case http.StatusForbidden:
		status := http.StatusForbidden
		if col == colGo {
			status = http.StatusGone
		}
		refuseFor(w, e, col, status, OutcomeRegistryRefused, "",
			fmt.Sprintf("egress-proxy: %s: refused by the policy of registry %s", clip(req.what), name))
	default:
		refuseFor(w, e, col, http.StatusBadGateway, OutcomeUpstreamError, "status",
			fmt.Sprintf("egress-proxy: registry %s answered %d", name, resp.StatusCode))
	}
}

// writeOK answers a 200: only Content-Type, Content-Length and
// Last-Modified are copied; the body is capped, a pip page rewritten, and
// every kind but an archive echo-scanned for the credential sent.
func (s *Server) writeOK(ctx context.Context, w *httpguard.CountingWriter, r *http.Request, resp *http.Response,
	final *url.URL, col column, req regReq, cred Credential, e *auditEntry,
) {
	limit := req.limit
	if resp.ContentLength > limit {
		refuseFor(w, e, col, http.StatusBadGateway, OutcomeTooLarge, "content_length",
			fmt.Sprintf("egress-proxy: upstream response exceeds the %d-byte cap", limit))
		return
	}
	var needles [][]byte
	if req.scan {
		needles = cred.EchoNeedles()
	}
	if req.page != nil {
		s.writePage(ctx, w, r, resp, final, col, req, needles, e)
		return
	}
	if len(needles) == 0 || r.Method == http.MethodHead {
		copyHeaders(w, resp, resp.ContentLength)
		if !setWriteDeadline(ctx, w, e) {
			return
		}
		e.outcome = OutcomeProxied
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			s.stream(w, resp.Body, limit, e)
		}
		return
	}
	s.writeScanned(ctx, w, resp, col, limit, needles, e)
}

// writeScanned streams a scanned body: the first echoPrefixBytes are read
// and checked before the first byte is written (an echo there is a 502);
// after that an echo aborts the stream, and the guard never releases a
// byte that could begin a needle.
func (s *Server) writeScanned(ctx context.Context, w *httpguard.CountingWriter, resp *http.Response, col column,
	limit int64, needles [][]byte, e *auditEntry,
) {
	g := newEchoGuard(needles)
	body := io.LimitReader(resp.Body, limit+1)
	head := make([]byte, min(int64(echoPrefixBytes), limit+1))
	n, err := io.ReadFull(body, head)
	done := err == io.EOF || err == io.ErrUnexpectedEOF
	if done {
		err = nil
	}
	switch {
	case err != nil:
		refuseFor(w, e, col, http.StatusBadGateway, OutcomeUpstreamError, "stream",
			"egress-proxy: the upstream response failed")
		return
	case int64(n) > limit:
		refuseFor(w, e, col, http.StatusBadGateway, OutcomeTooLarge, "stream",
			fmt.Sprintf("egress-proxy: upstream response exceeds the %d-byte cap", limit))
		return
	}
	out, ferr := g.feed(head[:n])
	if ferr == nil && done {
		out = append(out, g.flush()...)
	}
	if ferr != nil {
		refuseFor(w, e, col, http.StatusBadGateway, OutcomeCredentialEcho, "",
			"egress-proxy: the registry's answer was withheld")
		return
	}
	length := resp.ContentLength
	if done {
		length = int64(len(out))
	}
	copyHeaders(w, resp, length)
	if !setWriteDeadline(ctx, w, e) {
		return
	}
	e.outcome = OutcomeProxied
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(out); err != nil || done {
		return
	}
	total := int64(n)
	buf := make([]byte, 32<<10)
	for {
		m, rerr := body.Read(buf)
		if m > 0 {
			total += int64(m)
			if total > limit {
				e.outcome, e.reason = OutcomeTooLarge, "stream"
				panic(http.ErrAbortHandler)
			}
			chunk, ferr := g.feed(buf[:m])
			if ferr != nil {
				e.outcome, e.reason = OutcomeCredentialEcho, "stream"
				panic(http.ErrAbortHandler)
			}
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
		if rerr == io.EOF {
			_, _ = w.Write(g.flush())
			return
		}
		if rerr != nil {
			e.outcome, e.reason = OutcomeUpstreamError, "stream"
			panic(http.ErrAbortHandler)
		}
	}
}

// writePage buffers, rewrites and scans a pip simple page.
func (s *Server) writePage(ctx context.Context, w *httpguard.CountingWriter, r *http.Request,
	resp *http.Response, final *url.URL, col column, req regReq, needles [][]byte, e *auditEntry,
) {
	page, err := io.ReadAll(io.LimitReader(resp.Body, req.limit+1))
	switch {
	case err != nil:
		refuseFor(w, e, col, http.StatusBadGateway, OutcomeUpstreamError, "stream",
			"egress-proxy: the upstream response failed")
		return
	case int64(len(page)) > req.limit:
		refuseFor(w, e, col, http.StatusBadGateway, OutcomeTooLarge, "stream",
			fmt.Sprintf("egress-proxy: upstream page exceeds the %d-byte cap", req.limit))
		return
	}
	out, err := req.page(page, final, e)
	if err != nil {
		refuseFor(w, e, col, http.StatusBadGateway, OutcomeUpstreamError, "rewrite",
			"egress-proxy: the registry's page could not be rewritten")
		return
	}
	countLinks(ctx, e.reg.registry, e.reg.rewritten, e.reg.dropped)
	if containsAny(out, needles) {
		refuseFor(w, e, col, http.StatusBadGateway, OutcomeCredentialEcho, "",
			"egress-proxy: the registry's answer was withheld")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(len(out)))
	if !setWriteDeadline(ctx, w, e) {
		return
	}
	e.outcome = OutcomeProxied
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(out)
	}
}

// copyHeaders copies the allowlisted response headers and the length
// (when known, length >= 0).
func copyHeaders(w http.ResponseWriter, resp *http.Response, length int64) {
	h := w.Header()
	for _, k := range copiedHeaders {
		if v := resp.Header.Get(k); v != "" {
			h.Set(k, v)
		}
	}
	if length >= 0 {
		h.Set("Content-Length", strconv.FormatInt(length, 10))
	}
}

// setWriteDeadline bounds the client side by the request's deadline, as
// step 1's forward does; false when it answered the failure.
func setWriteDeadline(ctx context.Context, w http.ResponseWriter, e *auditEntry) bool {
	if dl, ok := ctx.Deadline(); ok {
		if err := http.NewResponseController(w).SetWriteDeadline(dl); err != nil &&
			!errors.Is(err, http.ErrNotSupported) {
			w.Header().Del("Content-Length")
			refuse(w, e, http.StatusInternalServerError, OutcomeUpstreamError, "write_deadline",
				"egress-proxy: cannot bound the response")
			return false
		}
	}
	return true
}

// probeModule is the credentialed HEAD a go registry's checksum lookup
// needs under packagePolicy registry: true only for a 200.
func (s *Server) probeModule(ctx context.Context, rt *registryRoute, target *url.URL, cred Credential) bool {
	out, err := http.NewRequestWithContext(ctx, http.MethodHead, target.String(), http.NoBody)
	if err != nil {
		return false
	}
	out.URL, out.Host = target, target.Host
	out.Header = http.Header{"User-Agent": {s.ua}, "Accept-Encoding": {"identity"}}
	if a := cred.Authorization(); a != "" {
		out.Header.Set("Authorization", a)
	}
	resp, err := rt.client.Do(out)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		s.cfg.Credentials.Invalidate(rt.reg.Name)
	}
	return resp.StatusCode == http.StatusOK
}

// noteStatus feeds the credential self-check: selfCheckRun 403s from the
// registry within selfCheckWindow, when the registry names a
// healthPackage, start one check, at most once per SelfCheckInterval. Any
// other answer resets the run.
func (s *Server) noteStatus(rt *registryRoute, status int) {
	now := s.cfg.Now()
	rt.mu.Lock()
	if status != http.StatusForbidden {
		rt.refused = 0
		rt.mu.Unlock()
		return
	}
	if rt.refused == 0 || now.Sub(rt.firstRefused) > selfCheckWindow {
		rt.refused, rt.firstRefused = 0, now
	}
	rt.refused++
	run := rt.refused >= selfCheckRun && rt.reg.HealthPackage != "" &&
		(rt.lastCheck.IsZero() || now.Sub(rt.lastCheck) >= s.cfg.SelfCheckInterval)
	if run {
		rt.lastCheck, rt.refused = now, 0
	}
	rt.mu.Unlock()
	if run {
		s.selfChecks.Go(func() { s.selfCheck(rt) })
	}
}

// selfCheck fetches the registry's healthPackage with the credential once;
// a 401 or 403 means the credential, not the registry's curation, is the
// problem, so the source re-reads or re-mints it.
func (s *Server) selfCheck(rt *registryRoute) {
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.Limits.HeaderTimeout)
	defer cancel()
	cred, err := s.credential(rt)
	if err != nil {
		return
	}
	target, err := healthURL(rt.reg, rt.base)
	if err != nil {
		return
	}
	out, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), http.NoBody)
	if err != nil {
		return
	}
	out.URL, out.Host = target, target.Host
	out.Header = http.Header{"User-Agent": {s.ua}, "Accept-Encoding": {"identity"}}
	if a := cred.Authorization(); a != "" {
		out.Header.Set("Authorization", a)
	}
	resp, err := rt.client.Do(out)
	if err != nil {
		return
	}
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		if s.cfg.Credentials != nil {
			s.cfg.Credentials.Invalidate(rt.reg.Name)
		}
		s.log.LogAttrs(ctx, slog.LevelWarn, "credential_rejected",
			slog.String("registry", rt.reg.Name),
			slog.String("credential", string(cred.Kind())),
			slog.Int("upstream_status", resp.StatusCode))
	}
}

// healthURL is the healthPackage's packument, simple page or version list.
func healthURL(r netprofile.Registry, base *url.URL) (*url.URL, error) {
	h := r.HealthPackage
	switch r.Ecosystem {
	case netprofile.EcosystemNPM:
		scope, name, err := netprofile.NPMName(h)
		if err != nil {
			return nil, err
		}
		if scope != "" {
			return joinBase(base, "/@"+scope+"%2f"+name)
		}
		return joinBase(base, "/"+name)
	case netprofile.EcosystemPyPI:
		return joinBase(base, "/"+netprofile.PyNormalize(h)+"/")
	default:
		esc, err := module.EscapePath(h)
		if err != nil {
			return nil, err
		}
		return outboundURL(base, esc+"/@v/list"), nil
	}
}
