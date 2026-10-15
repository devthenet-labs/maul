// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bitwise-media-group/patchy/internal/httpguard"
	"github.com/bitwise-media-group/patchy/internal/netprofile"
)

// The TLS alerts a refused ClientHello is answered with (RFC 8446 §6).
const (
	alertUnrecognizedName = 112
	alertHandshakeFailure = 40
	recordTypeAlert       = 21
	alertLevelFatal       = 2
)

// lingerTimeout bounds how long a refused connection is drained after the
// proxy's answer, so closing it does not reset the answer away.
const lingerTimeout = time.Second

// established is the answer to an admitted CONNECT.
const established = "HTTP/1.1 200 Connection established\r\n\r\n"

// suspendedPhrase is step 1's suspended words, shared by both routes.
const suspendedPhrase = "egress-proxy: profile routes are suspended by the operator"

// shuttingDownPhrase answers a CONNECT that arrives while the proxy closes
// its tunnels.
const shuttingDownPhrase = "egress-proxy: shutting down; retry the CONNECT"

// refusal is one answer serveConnect writes itself, before or instead of
// the 200.
type refusal struct {
	status          int
	outcome, reason string
	phrase          string
}

// ConfigureListener prepares a profile listener's http.Server: HTTP/1
// only (no h2c, so every request is one the tunnel can hijack), and a
// Shutdown that also closes the open tunnels, which net/http stops
// tracking once they are hijacked. Shutdown neither waits for that nor
// for their close lines: a caller that is about to exit calls
// DrainTunnels after it.
func (s *Server) ConfigureListener(hs *http.Server) {
	var p http.Protocols
	p.SetHTTP1(true)
	hs.Protocols = &p
	hs.RegisterOnShutdown(s.CloseTunnels)
}

// CloseTunnels ends every open tunnel (outcome shutdown), and every tunnel
// hijacked after it at once: a rolling restart or a suspendProfiles roll
// ends the old pod's tunnels at its termination, and the clients reconnect
// to a new pod.
func (s *Server) CloseTunnels() { s.tunnels.closeAll() }

// DrainTunnels closes the tunnels (CloseTunnels) and waits until every
// CONNECT handler has written its close line and metrics, or ctx ends
// (its error). Call it after the listeners' Shutdown has returned, so no
// CONNECT is still to arrive.
func (s *Server) DrainTunnels(ctx context.Context) error {
	s.CloseTunnels()
	return s.tunnels.wait(ctx)
}

// tunnel is one CONNECT, from the hijack to the close line.
type tunnel struct {
	s      *Server
	client net.Conn
	// fromClient reads the client: first what net/http had buffered past
	// the CONNECT headers (a client may pipeline its hello), then the
	// connection itself.
	fromClient io.Reader
	ctx        context.Context
	cancel     context.CancelFunc

	// caps are the per-tunnel byte caps by direction; budget and table the
	// source's budget on the profile.
	caps   [2]int64
	table  *budgetTable
	budget *sourceBudget
	bytes  [2]atomic.Int64
	// last is the unix-nanosecond time of the last byte either way.
	last atomic.Int64

	mu              sync.Mutex
	upstream        net.Conn
	done            bool
	outcome, reason string
}

// finish records how the tunnel ended; the first call wins.
func (t *tunnel) finish(outcome, reason string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return false
	}
	t.done, t.outcome, t.reason = true, outcome, reason
	return true
}

// end finishes the tunnel and closes both sides, whoever got there first.
func (t *tunnel) end(outcome, reason string) {
	t.finish(outcome, reason)
	t.cancel()
	t.mu.Lock()
	upstream := t.upstream
	t.mu.Unlock()
	_ = t.client.Close()
	if upstream != nil {
		_ = upstream.Close()
	}
}

func (t *tunnel) result() (string, string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.outcome, t.reason
}

// setUpstream records the dialled upstream; false (and the connection
// closed) when the tunnel has already ended.
func (t *tunnel) setUpstream(c net.Conn) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		_ = c.Close()
		return false
	}
	t.upstream = c
	return true
}

// refuse answers a refusal before the 200 on the hijacked connection, raw
// (net/http cannot write a custom reason phrase), then closes it.
func (t *tunnel) refuse(e *tunnelEntry, rf refusal) {
	if !t.finish(rf.outcome, rf.reason) {
		return
	}
	e.status = rf.status
	_ = t.client.SetWriteDeadline(time.Now().Add(tunnelRefusalWriteTimeout))
	_ = writeRawRefusal(t.client, rf.status, rf.phrase)
	t.closeClient()
	t.end(rf.outcome, rf.reason)
}

// alert refuses the ClientHello with one fatal TLS alert: nothing was
// sent upstream, and nothing will be.
func (t *tunnel) alert(outcome, reason string, desc byte) {
	if !t.finish(outcome, reason) {
		return
	}
	_ = t.client.SetWriteDeadline(time.Now().Add(tunnelRefusalWriteTimeout))
	_, _ = t.client.Write([]byte{recordTypeAlert, 3, 3, 0, 2, alertLevelFatal, desc})
	t.closeClient()
	t.end(outcome, reason)
}

// closeClient half-closes the client, drains what it already sent for at
// most lingerTimeout, then closes it: closing a socket with unread data
// would reset the answer away before the client reads it.
func (t *tunnel) closeClient() {
	if cw, ok := t.client.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		_ = t.client.SetReadDeadline(time.Now().Add(lingerTimeout))
		_, _ = io.Copy(io.Discard, io.LimitReader(t.client, MaxHelloBytes))
	}
	_ = t.client.Close()
}

// writeRawRefusal writes a whole HTTP/1.1 refusal: the status line with
// the reason as its phrase (what curl and the JVM show), the same line as
// the body (what other clients show), and Connection: close. The phrase is
// built from validated values only; any control byte is replaced anyway,
// so no CR or LF can reach the wire.
func writeRawRefusal(w io.Writer, status int, phrase string) error {
	b := []byte(phrase)
	for i, c := range b {
		if c < ' ' || c == 0x7f {
			b[i] = '?'
		}
	}
	phrase = string(b)
	body := phrase + "\n"
	_, err := fmt.Fprintf(w, "HTTP/1.1 %d %s\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\n"+
		"Connection: close\r\n\r\n%s", status, phrase, len(body), body)
	return err
}

// auditTarget is what a tunnel's audit line says about the CONNECT
// target: host is the validated host, or the raw target's host part when
// it has one; target is the raw target, clipped, set only while host is
// not a validated name. Neither is pre-quoted: the slog handler quotes and
// escapes a value that needs it (control bytes, spaces, quotes, invalid
// UTF-8), so a grep for the bare host matches.
type auditTarget struct {
	host, target string
}

// rawTarget is the audit view of a target no policy has validated: its
// host part when it has the authority shape (any port), and the whole
// value, clipped.
func rawTarget(target string) auditTarget {
	v := auditTarget{target: clip(target)}
	switch host, err := ParseConnectTarget(target); {
	case err == nil:
		v.host = clip(host)
	case errors.Is(err, ErrConnectPort):
		// The shape check passed up to the port: one ':', a non-empty
		// host with no control byte, space, '/', '@' or bracket.
		h, _, _ := strings.Cut(target, ":")
		v.host = clip(h)
	}
	return v
}

// judgeTarget is the pure half of the CONNECT pipeline (§2.3 steps 1-2 of
// the step-3 spec): the target's shape, then the host policy in order (an
// IP literal, the host grammar after normalisation, the hard-deny table
// with deny's hosts, the profile's exact allowlist). It returns the
// normalised host to dial, or the refusal; view is the target for the
// audit line (the validated host, or rawTarget's host part and target).
func judgeTarget(p netprofile.Profile, deny netprofile.DenyContext, target string) (host string, view auditTarget,
	rf *refusal) {
	view = rawTarget(target)
	raw, err := ParseConnectTarget(target)
	switch {
	case errors.Is(err, ErrConnectPort):
		return "", view, &refusal{http.StatusForbidden, OutcomePort, "", ErrConnectPort.Error()}
	case err != nil:
		return "", view, &refusal{http.StatusBadRequest, OutcomeBadRequest, "target", ErrConnectTarget.Error()}
	}
	if netprofile.IPLiteral(raw) {
		return "", view, &refusal{http.StatusForbidden, OutcomeIPLiteral, "",
			"egress-proxy: an IP address is never tunnelled; CONNECT to an allowed host name"}
	}
	h, err := netprofile.NormalizeHost(raw)
	if err != nil || !netprofile.ValidHost(h) {
		return "", view, &refusal{http.StatusForbidden, OutcomeBadHost, "",
			"egress-proxy: the CONNECT host is not a valid host name"}
	}
	view = auditTarget{host: h}
	if err := netprofile.DeniedTunnelHost(h, deny); err != nil {
		return "", view, &refusal{http.StatusForbidden, OutcomeHardDenied, "",
			fmt.Sprintf("egress-proxy: %s is never tunnelled (a forge, model API, control plane, internal or "+
				"publish host)", h)}
	}
	if !p.Hosts.Allows(h) {
		return "", view, &refusal{http.StatusForbidden, OutcomeNotAllowed, "",
			fmt.Sprintf("egress-proxy: %s is not in profile %s's hosts", h, p.Name)}
	}
	return h, view, nil
}

// serveConnect runs one CONNECT through the tunnel pipeline (§2.3 of the
// step-3 spec) on a profile with hosts. Every answer is written raw on the
// hijacked connection, and exactly one close line is audited whatever
// happens (a panic included).
func (s *Server) serveConnect(p netprofile.Profile, w http.ResponseWriter, r *http.Request) {
	start := s.cfg.Now()
	e := tunnelEntry{profile: p.Name, port: p.Port, sourceIP: httpguard.SourceIP(r)}
	if s.cfg.Pods != nil {
		if info, ok := s.cfg.Pods.Lookup(e.sourceIP); ok {
			e.pod = info
		}
	}
	// Counted from before the hijack to after the close line, so
	// DrainTunnels waits for both.
	s.tunnels.enter()
	conn, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		// Only a request that is not HTTP/1 cannot be hijacked, and the
		// listener speaks HTTP/1 alone; answer without a tunnel anyway.
		defer s.tunnels.leave(nil)
		e.status, e.outcome, e.reason = http.StatusBadRequest, OutcomeBadRequest, "not_hijackable"
		http.Error(w, "egress-proxy: CONNECT needs HTTP/1.1", http.StatusBadRequest)
		auditTunnel(r.Context(), s.log, e)
		countTunnel(r.Context(), p.Name, e.outcome, "", 0, 0)
		return
	}
	// The tunnel's context ends only with the tunnel: net/http cancels
	// the request's when the client half-closes.
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	t := &tunnel{s: s, client: conn, fromClient: pipelined(brw.Reader, conn), ctx: ctx, cancel: cancel}
	live := s.tunnels.track(t)
	defer func() {
		// Leave last, once the close line and the metrics are out.
		defer s.tunnels.leave(t)
		t.end(OutcomeTunnelled, "")
		e.outcome, e.reason = t.result()
		e.bytesUp, e.bytesDown = t.bytes[up].Load(), t.bytes[down].Load()
		e.elapsed = s.cfg.Now().Sub(start)
		auditTunnel(ctx, s.log, e)
		host := ""
		if e.target == "" && p.Hosts.Allows(e.host) {
			host = e.host
		}
		countTunnel(context.WithoutCancel(ctx), p.Name, e.outcome, host, e.bytesUp, e.bytesDown)
	}()
	if !live {
		// Hijacked after CloseTunnels walked the open tunnels: the pod is
		// going away, so this one is refused before any dial, and the
		// client retries against a new pod.
		e.setTarget(rawTarget(r.RequestURI))
		t.refuse(&e, refusal{http.StatusServiceUnavailable, OutcomeShutdown, "", shuttingDownPhrase})
		return
	}
	_ = conn.SetDeadline(time.Time{})
	host, ok := s.admitConnect(t, p, r, &e)
	if !ok {
		return
	}
	release, reason := s.tunnelLimits.acquire(ctx, e.sourceIP)
	if reason != "" {
		t.refuse(&e, limitRefusal(reason))
		return
	}
	defer release()
	table := s.budgets[p.Name]
	budget, reason := table.open(e.sourceIP)
	if reason != "" {
		t.refuse(&e, limitRefusal(reason))
		return
	}
	defer table.close(budget)
	t.table, t.budget = table, budget
	t.caps = [2]int64{s.cfg.TunnelLimits.MaxUploadBytes, s.cfg.TunnelLimits.MaxDownloadBytes}
	if p.Upload != nil && p.Upload.PerTunnel > 0 {
		t.caps[up] = p.Upload.PerTunnel
	}
	if !s.dialTunnel(t, &e, host) {
		return
	}
	s.runTunnel(t, &e, host, start)
}

// admitConnect is §2.3 steps 0-2: suspended, the request's shape and the
// host policy. It answers every refusal itself.
func (s *Server) admitConnect(t *tunnel, p netprofile.Profile, r *http.Request, e *tunnelEntry) (string, bool) {
	e.setTarget(rawTarget(r.RequestURI))
	if s.cfg.Suspended {
		t.refuse(e, refusal{http.StatusGone, OutcomeSuspended, "", suspendedPhrase})
		return "", false
	}
	switch {
	case r.ProtoMajor != 1 || r.ProtoMinor != 1:
		t.refuse(e, refusal{http.StatusBadRequest, OutcomeBadRequest, "proto", "egress-proxy: CONNECT must be HTTP/1.1"})
		return "", false
	case r.ContentLength != 0 || len(r.TransferEncoding) > 0:
		t.refuse(e, refusal{http.StatusBadRequest, OutcomeBadRequest, "body", "egress-proxy: a CONNECT carries no body"})
		return "", false
	}
	deny := netprofile.DenyContext{Hosts: append([]string{p.Host}, s.cfg.DenyHosts...)}
	host, view, rf := judgeTarget(p, deny, r.RequestURI)
	e.setTarget(view)
	if rf != nil {
		t.refuse(e, *rf)
		return "", false
	}
	return host, true
}

// limitRefusal is the 503 for a limit or budget reason.
func limitRefusal(reason string) refusal {
	return refusal{http.StatusServiceUnavailable, OutcomeLimited, reason,
		"egress-proxy: over the tunnel limit (" + reason + ")"}
}

// dialTunnel is §2.3 step 4: resolve the host as a fully-qualified name
// (the trailing dot keeps the proxy pod's search domains from mapping it
// to a cluster name) and dial it on 443 through the guarded dialer, every
// address judged before the dial and again on connect. It happens before
// the 200, so a failure is a readable answer.
func (s *Server) dialTunnel(t *tunnel, e *tunnelEntry, host string) bool {
	ctx, cancel := context.WithTimeout(t.ctx, s.cfg.TunnelLimits.DialTimeout)
	defer cancel()
	c, err := s.tunnelDialer.DialContext(ctx, "tcp4", net.JoinHostPort(host+".", TunnelPort))
	if err != nil {
		if errors.Is(err, ErrDeniedAddress) {
			t.refuse(e, refusal{http.StatusBadGateway, OutcomeDeniedAddress, "",
				fmt.Sprintf("egress-proxy: %s resolves to an address the proxy never dials", host)})
		} else {
			t.refuse(e, refusal{http.StatusBadGateway, OutcomeDialError, "",
				fmt.Sprintf("egress-proxy: could not connect to %s", host)})
		}
		return false
	}
	if ap, err := netip.ParseAddrPort(c.RemoteAddr().String()); err == nil {
		e.upstreamIP = ap.Addr().Unmap().String()
	}
	return t.setUpstream(c)
}

// runTunnel is §2.3 steps 5-8: the 200 and the opened line, the
// ClientHello judged before any byte goes upstream, then the splice under
// the caps, the idle and absolute deadlines and the progress lines.
func (s *Server) runTunnel(t *tunnel, e *tunnelEntry, host string, start time.Time) {
	// The opened line goes out first, so it is in the log before the
	// client can send a byte (and survives whatever happens next).
	e.status = http.StatusOK
	auditTunnelOpened(t.ctx, s.log, *e)
	_ = t.client.SetWriteDeadline(time.Now().Add(tunnelRefusalWriteTimeout))
	if _, err := io.WriteString(t.client, established); err != nil {
		t.end(OutcomeTunnelled, "")
		return
	}
	_ = t.client.SetWriteDeadline(time.Time{})
	countTunnelOpen(context.WithoutCancel(t.ctx), e.profile, 1)
	defer countTunnelOpen(context.WithoutCancel(t.ctx), e.profile, -1)
	t.last.Store(time.Now().UnixNano())
	stop := make(chan struct{})
	defer close(stop)
	go t.watch(stop, *e, start)

	_ = t.client.SetReadDeadline(time.Now().Add(s.cfg.TunnelLimits.HelloTimeout))
	hello, raw, err := ReadClientHello(t.fromClient)
	_ = t.client.SetReadDeadline(time.Time{})
	if err == nil {
		e.sni, e.alpn = hello.SNI, hello.ALPN
	}
	var ne net.Error
	switch {
	case err != nil && errors.As(err, &ne) && ne.Timeout():
		t.alert(OutcomeBadHello, reasonTimeout, alertHandshakeFailure)
		return
	case errors.Is(err, ErrSNIList):
		t.alert(OutcomeSNIMissing, "", alertUnrecognizedName)
		return
	case err != nil:
		t.alert(OutcomeBadHello, "", alertHandshakeFailure)
		return
	case hello.SNI == "":
		t.alert(OutcomeSNIMissing, "", alertUnrecognizedName)
		return
	}
	if sni, err := netprofile.NormalizeHost(hello.SNI); err != nil || sni != host {
		t.alert(OutcomeSNIMismatch, "", alertUnrecognizedName)
		return
	}
	if hello.HasECH {
		t.alert(OutcomeECH, "", alertHandshakeFailure)
		return
	}
	e.forwarded = true
	if t.pass(up, t.upstream, raw) != nil {
		return
	}
	t.splice()
}

// pipelined reads what br has buffered, then conn directly, so no read
// after the hijack goes through net/http's connection reader.
func pipelined(br *bufio.Reader, conn net.Conn) io.Reader {
	n := br.Buffered()
	if n == 0 {
		return conn
	}
	b, _ := br.Peek(n)
	return io.MultiReader(bytes.NewReader(bytes.Clone(b)), conn)
}

// errCapped ends a copy whose tunnel crossed a byte cap.
var errCapped = errors.New("egress-proxy: tunnel byte cap")

// pass writes p to dst within d's per-tunnel cap and the source's budget:
// never a byte past either. Crossing one ends the tunnel (upload_cap or
// download_cap) after writing what still fitted.
func (t *tunnel) pass(d direction, dst net.Conn, p []byte) error {
	n := int64(len(p))
	allow := min(n, max(t.caps[d]-t.bytes[d].Load(), 0))
	reason := reasonTunnelCap
	if g := t.table.take(t.budget, d, allow); g < allow {
		allow = g
		reason = reasonSourceUploadBudget
		if d == down {
			reason = reasonSourceDownloadBudget
		}
	}
	if allow > 0 {
		w, err := dst.Write(p[:allow])
		t.bytes[d].Add(int64(w))
		t.last.Store(time.Now().UnixNano())
		if err != nil {
			t.end(OutcomeTunnelled, "")
			return err
		}
	}
	if allow < n {
		outcome := OutcomeUploadCap
		if d == down {
			outcome = OutcomeDownloadCap
		}
		t.end(outcome, reason)
		return errCapped
	}
	return nil
}
