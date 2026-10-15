// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bitwise-media-group/patchy/internal/netprofile"
)

// tunnelProfilesJSON holds a tunnel-only profile, a Go profile with hosts
// and an upload override, and a go-only profile (step 1's shape).
const tunnelProfilesJSON = `[
 {"name":"web","route":"tunnel","port":8092,"repositories":["devthenet-labs/patchy-pip-fixture"],
  "modules":[],"bakedModCache":"","allowPseudoVersions":false,
  "hosts":["pkg.registry.dev","files.registry.dev"],"clients":["pip"]},
 {"name":"go","route":"go","port":8090,"repositories":["devthenet-labs/patchy-preview-demo"],
  "modules":["github.com/google/uuid"],"hosts":["npm.registry.dev"],
  "upload":{"perTunnel":65536,"perSource":131072}},
 {"name":"plain","route":"go","port":8091,"repositories":["devthenet-labs/patchy-target"],
  "modules":["github.com/google/uuid"]}]`

func tunnelProfiles(t *testing.T) netprofile.Set {
	t.Helper()
	set, err := netprofile.Parse([]byte(tunnelProfilesJSON))
	if err != nil {
		t.Fatal(err)
	}
	return set
}

// tlsUpstream is a TLS echo server for the allowlisted hosts, counting
// the raw bytes it receives and sends on the wire.
type tlsUpstream struct {
	ln       net.Listener
	cfg      *tls.Config
	pool     *x509.CertPool
	received atomic.Int64
	sent     atomic.Int64
	accepted atomic.Int64
	// serve is what the upstream does once the handshake completes; nil
	// echoes.
	serve func(c *tls.Conn)
}

func newTLSUpstream(t *testing.T, names ...string) *tlsUpstream {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: names[0]},
		DNSNames:              names,
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	u := &tlsUpstream{ln: ln, pool: pool, cfg: &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		NextProtos:   []string{"h2", "http/1.1"},
	}}
	t.Cleanup(func() { _ = ln.Close() })
	go u.accept()
	return u
}

func (u *tlsUpstream) port() string {
	_, p, _ := net.SplitHostPort(u.ln.Addr().String())
	return p
}

func (u *tlsUpstream) accept() {
	for {
		c, err := u.ln.Accept()
		if err != nil {
			return
		}
		u.accepted.Add(1)
		go func() {
			defer func() { _ = c.Close() }()
			tc := tls.Server(&countedConn{Conn: c, in: &u.received, out: &u.sent}, u.cfg)
			if err := tc.Handshake(); err != nil {
				return
			}
			if u.serve != nil {
				u.serve(tc)
				return
			}
			_, _ = io.Copy(tc, tc)
		}()
	}
}

// countedConn counts a connection's raw bytes.
type countedConn struct {
	net.Conn
	in, out *atomic.Int64
}

func (c *countedConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.in.Add(int64(n))
	return n, err
}

func (c *countedConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	c.out.Add(int64(n))
	return n, err
}

// tunnelLookup is the fake resolver: each name maps to a queue of
// answers (the last repeating). It records every name asked, and fails
// the test on a name it does not know.
type tunnelLookup struct {
	t       *testing.T
	mu      sync.Mutex
	answers map[string][][]netip.Addr
	errs    map[string]error
	asked   []string
	calls   map[string]int
}

func (l *tunnelLookup) lookup(_ context.Context, name string) ([]netip.Addr, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.asked = append(l.asked, name)
	if err, ok := l.errs[name]; ok {
		return nil, err
	}
	as, ok := l.answers[name]
	if !ok {
		l.t.Errorf("lookup of unexpected name %q", name)
		return nil, errors.New("unknown name")
	}
	i := min(l.calls[name], len(as)-1)
	l.calls[name]++
	return as[i], nil
}

func (l *tunnelLookup) names() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.asked...)
}

// tunnelHarness is an engine on loopback whose tunnel dials reach up.
type tunnelHarness struct {
	srv    *Server
	up     *tlsUpstream
	lookup *tunnelLookup
	log    *lockedBuffer
	addrs  map[string]string // profile -> listener host:port
	https  map[string]*http.Server
	urls   map[string]string
}

// newTunnelHarness starts the engine over tunnelProfiles (unless cfg sets
// its own) with every allowlisted name resolving to loopback, and the
// tunnel's port-443 dials rewritten to up's port.
func newTunnelHarness(t *testing.T, cfg Config) *tunnelHarness {
	t.Helper()
	if cfg.Profiles.Len() == 0 {
		cfg.Profiles = tunnelProfiles(t)
	}
	up := newTLSUpstream(t, "pkg.registry.dev", "files.registry.dev", "npm.registry.dev", "other.registry.dev")
	lk := &tunnelLookup{t: t, answers: map[string][][]netip.Addr{}, errs: map[string]error{},
		calls: map[string]int{}}
	for _, n := range []string{"pkg.registry.dev.", "files.registry.dev.", "npm.registry.dev."} {
		lk.answers[n] = [][]netip.Addr{addrs("127.0.0.1")}
	}
	cfg.lookup = lk.lookup
	cfg.Policy.testLoopback = true
	cfg.dialRewrite = func(addr string) string {
		if h, p, err := net.SplitHostPort(addr); err == nil && p == TunnelPort {
			return net.JoinHostPort(h, up.port())
		}
		return addr
	}
	if cfg.GoProxy == nil {
		cfg.GoProxy = mustURL(t, "https://proxy.test")
	}
	log := &lockedBuffer{}
	srv, err := New(cfg, slog.New(slog.NewJSONHandler(log, nil)))
	if err != nil {
		t.Fatal(err)
	}
	h := &tunnelHarness{srv: srv, up: up, lookup: lk, log: log, addrs: map[string]string{},
		https: map[string]*http.Server{}, urls: map[string]string{}}
	for _, p := range cfg.Profiles.Profiles() {
		handler, err := srv.Handler(p.Name)
		if err != nil {
			t.Fatal(err)
		}
		ts := httptest.NewUnstartedServer(handler)
		srv.ConfigureListener(ts.Config)
		ts.Start()
		t.Cleanup(ts.Close)
		h.addrs[p.Name] = ts.Listener.Addr().String()
		h.https[p.Name] = ts.Config
		h.urls[p.Name] = ts.URL
	}
	return h
}

// rawResponse is a refusal read off the socket, unparsed.
type rawResponse struct {
	statusLine string
	header     http.Header
	body       string
}

// connect sends one CONNECT request (line, then extra header lines) on a
// fresh connection and returns the connection with its reader.
func (h *tunnelHarness) connect(t *testing.T, profile, line string, extra ...string) (net.Conn, *bufio.Reader) {
	t.Helper()
	c, err := net.Dial("tcp4", h.addrs[profile])
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(20 * time.Second))
	req := line + "\r\nHost: x\r\nProxy-Authorization: Basic c2VjcmV0LWNhbmFyeQ==\r\n" + strings.Join(extra, "") + "\r\n"
	if _, err := io.WriteString(c, req); err != nil {
		t.Fatal(err)
	}
	return c, bufio.NewReader(c)
}

// readRaw reads one HTTP/1.1 response by hand: the status line exactly as
// sent, the headers and the body (by Content-Length, else to EOF).
func readRaw(t *testing.T, br *bufio.Reader) rawResponse {
	t.Helper()
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("status line: %v", err)
	}
	r := rawResponse{statusLine: strings.TrimSuffix(status, "\r\n"), header: http.Header{}}
	for {
		l, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("header: %v", err)
		}
		l = strings.TrimSuffix(l, "\r\n")
		if l == "" {
			break
		}
		k, v, _ := strings.Cut(l, ":")
		r.header.Add(k, strings.TrimSpace(v))
	}
	if cl := r.header.Get("Content-Length"); cl != "" {
		var n int
		_, _ = fmt.Sscan(cl, &n)
		b := make([]byte, n)
		if _, err := io.ReadFull(br, b); err != nil {
			t.Fatalf("body: %v", err)
		}
		r.body = string(b)
	}
	return r
}

// open runs a CONNECT to host on profile and, on the 200, a TLS handshake
// for sni (host when empty) over the tunnel.
func (h *tunnelHarness) open(t *testing.T, profile, host, sni string) (*tls.Conn, error) {
	t.Helper()
	c, br := h.connect(t, profile, "CONNECT "+host+":443 HTTP/1.1")
	r := readRaw(t, br)
	if r.statusLine != "HTTP/1.1 200 Connection established" {
		return nil, fmt.Errorf("CONNECT %s = %q %q", host, r.statusLine, r.body)
	}
	if sni == "" {
		sni = host
	}
	tc := tls.Client(&bufferedConn{Conn: c, r: br}, &tls.Config{ServerName: sni, RootCAs: h.up.pool,
		NextProtos: []string{"h2"}})
	if err := tc.Handshake(); err != nil {
		return nil, err
	}
	return tc, nil
}

// bufferedConn reads through r, which may hold bytes past a response.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(b []byte) (int, error) { return c.r.Read(b) }

// closeLine waits for the n-th close line (msg tunnel or refused) and
// returns it.
func (h *tunnelHarness) closeLine(t *testing.T, n int) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var closes []map[string]any
		for _, l := range h.log.lines(t) {
			if l["route"] == "tunnel" && (l["msg"] == "tunnel" || l["msg"] == "refused") {
				closes = append(closes, l)
			}
		}
		if len(closes) >= n {
			return closes[n-1]
		}
		if time.Now().After(deadline) {
			t.Fatalf("no close line %d in:\n%s", n, h.log.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// linesWith returns the log lines whose msg is msg.
func (h *tunnelHarness) linesWith(t *testing.T, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range h.log.lines(t) {
		if l["msg"] == msg {
			out = append(out, l)
		}
	}
	return out
}
