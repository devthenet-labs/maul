// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestTunnelHappyPath: an allowlisted CONNECT gets the 200, the TLS
// handshake completes end to end with the upstream's certificate, bytes
// round-trip, the name is resolved fully qualified, and the audit has one
// opened line then one close line whose byte counts are what the upstream
// counted on the wire. No header value is ever logged.
func TestTunnelHappyPath(t *testing.T) {
	h := newTunnelHarness(t, Config{})
	tc, err := h.open(t, "web", "pkg.registry.dev", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := tc.ConnectionState().NegotiatedProtocol; got != "h2" {
		t.Errorf("ALPN through the tunnel = %q, want h2", got)
	}
	msg := bytes.Repeat([]byte("dependency bytes "), 1000)
	if _, err := tc.Write(msg); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(tc, got); err != nil || !bytes.Equal(got, msg) {
		t.Fatalf("echo through the tunnel: %v", err)
	}
	_ = tc.Close()
	line := h.closeLine(t, 1)
	for k, want := range map[string]any{"msg": "tunnel", "outcome": OutcomeTunnelled, "host": "pkg.registry.dev",
		"sni": "pkg.registry.dev", "alpn": "h2", "upstream_ip": "127.0.0.1", "status": float64(200),
		"profile": "web", "port": float64(8092)} {
		if line[k] != want {
			t.Errorf("close line %s = %v, want %v", k, line[k], want)
		}
	}
	// The upstream's own handler counts are final once its side closed.
	deadline := time.Now().Add(5 * time.Second)
	for line["bytes_up"] != float64(h.up.received.Load()) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if line["bytes_up"] != float64(h.up.received.Load()) || line["bytes_down"] != float64(h.up.sent.Load()) {
		t.Errorf("close line bytes %v/%v, upstream counted %d/%d", line["bytes_up"], line["bytes_down"],
			h.up.received.Load(), h.up.sent.Load())
	}
	if opened := h.linesWith(t, "tunnel opened"); len(opened) != 1 || opened[0]["level"] != "INFO" ||
		opened[0]["upstream_ip"] != "127.0.0.1" {
		t.Errorf("opened lines = %v, want one at INFO", opened)
	}
	if names := h.lookup.names(); !slices.Equal(names, []string{"pkg.registry.dev."}) {
		t.Errorf("resolved %v, want the fully-qualified pkg.registry.dev.", names)
	}
	if strings.Contains(h.log.String(), "c2VjcmV0") || strings.Contains(h.log.String(), "secret-canary") {
		t.Error("a header value reached the audit log")
	}
}

// TestTunnelPipelinedHello: a client that sends its ClientHello in the
// same write as the CONNECT headers still tunnels: the bytes net/http
// buffered are forwarded first.
func TestTunnelPipelinedHello(t *testing.T) {
	h := newTunnelHarness(t, Config{})
	c, err := net.Dial("tcp4", h.addrs["web"])
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	pc := &pipelinedConn{Conn: c, prefix: []byte("CONNECT files.registry.dev:443 HTTP/1.1\r\nHost: x\r\n\r\n")}
	tc := tls.Client(pc, &tls.Config{ServerName: "files.registry.dev", RootCAs: h.up.pool})
	if err := tc.Handshake(); err != nil {
		t.Fatalf("handshake over a pipelined CONNECT: %v", err)
	}
	if _, err := tc.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 4)
	if _, err := io.ReadFull(tc, b); err != nil || string(b) != "ping" {
		t.Fatalf("echo = %q, %v", b, err)
	}
}

// pipelinedConn prepends prefix to its first write and strips the 200 off
// its first reads.
type pipelinedConn struct {
	net.Conn
	prefix  []byte
	skipped bool
}

func (c *pipelinedConn) Write(b []byte) (int, error) {
	if c.prefix != nil {
		p := append(c.prefix, b...)
		c.prefix = nil
		if _, err := c.Conn.Write(p); err != nil {
			return 0, err
		}
		return len(b), nil
	}
	return c.Conn.Write(b)
}

func (c *pipelinedConn) Read(b []byte) (int, error) {
	if !c.skipped {
		c.skipped = true
		want := make([]byte, len(established))
		if _, err := io.ReadFull(c.Conn, want); err != nil || string(want) != established {
			return 0, errors.New("no 200 before the TLS bytes: " + string(want))
		}
	}
	return c.Conn.Read(b)
}

// TestTunnelRefusalsOnTheWire: every refusal before the 200 is written
// raw: the status line carries the reason as its phrase, the body the
// same line, Connection: close; the resolver and the upstream are never
// touched; the close line names the outcome.
func TestTunnelRefusalsOnTheWire(t *testing.T) {
	tests := []struct {
		name, line string
		extra      []string
		status     string
		phrase     string
		outcome    string
	}{
		{"not allowlisted", "CONNECT other.registry.dev:443 HTTP/1.1", nil, "403",
			"egress-proxy: other.registry.dev is not in profile web's hosts", OutcomeNotAllowed},
		{"subdomain of an allowed host", "CONNECT x.pkg.registry.dev:443 HTTP/1.1", nil, "403",
			"egress-proxy: x.pkg.registry.dev is not in profile web's hosts", OutcomeNotAllowed},
		{"forge", "CONNECT github.com:443 HTTP/1.1", nil, "403",
			"egress-proxy: github.com is never tunnelled (a forge, model API, control plane, internal or publish " +
				"host)", OutcomeHardDenied},
		{"model API", "CONNECT api.anthropic.com:443 HTTP/1.1", nil, "403", "", OutcomeHardDenied},
		{"control plane", "CONNECT abc.gr7.us-east-1.eks.amazonaws.com:443 HTTP/1.1", nil, "403", "",
			OutcomeHardDenied},
		{"dotted IP", "CONNECT 1.2.3.4:443 HTTP/1.1", nil, "403",
			"egress-proxy: an IP address is never tunnelled; CONNECT to an allowed host name", OutcomeIPLiteral},
		{"hex IP", "CONNECT 0x7f.1:443 HTTP/1.1", nil, "403", "", OutcomeIPLiteral},
		{"decimal IP", "CONNECT 2130706433:443 HTTP/1.1", nil, "403", "", OutcomeIPLiteral},
		{"short IP", "CONNECT 127.1:443 HTTP/1.1", nil, "403", "", OutcomeIPLiteral},
		{"underscore", "CONNECT pkg_x.registry.dev:443 HTTP/1.1", nil, "403",
			"egress-proxy: the CONNECT host is not a valid host name", OutcomeBadHost},
		{"trailing dot", "CONNECT pkg.registry.dev.:443 HTTP/1.1", nil, "403", "", OutcomeBadHost},
		{"port 80", "CONNECT pkg.registry.dev:80 HTTP/1.1", nil, "403",
			"egress-proxy: only port 443 may be tunnelled", OutcomePort},
		{"port 8443", "CONNECT pkg.registry.dev:8443 HTTP/1.1", nil, "403", "", OutcomePort},
		{"HTTP/1.0", "CONNECT pkg.registry.dev:443 HTTP/1.0", nil, "400",
			"egress-proxy: CONNECT must be HTTP/1.1", OutcomeBadRequest},
		{"absolute-form", "CONNECT https://pkg.registry.dev:443/ HTTP/1.1", nil, "400",
			"egress-proxy: the CONNECT target is not host:443 authority-form", OutcomeBadRequest},
		{"userinfo", "CONNECT u@pkg.registry.dev:443 HTTP/1.1", nil, "400", "", OutcomeBadRequest},
		{"body", "CONNECT pkg.registry.dev:443 HTTP/1.1", []string{"Content-Length: 3\r\n"}, "400",
			"egress-proxy: a CONNECT carries no body", OutcomeBadRequest},
		{"chunked", "CONNECT pkg.registry.dev:443 HTTP/1.1", []string{"Transfer-Encoding: chunked\r\n"}, "400",
			"", OutcomeBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTunnelHarness(t, Config{})
			c, br := h.connect(t, "web", tt.line, tt.extra...)
			r := readRaw(t, br)
			if !strings.HasPrefix(r.statusLine, "HTTP/1.1 "+tt.status+" egress-proxy: ") {
				t.Errorf("status line %q, want HTTP/1.1 %s egress-proxy: ...", r.statusLine, tt.status)
			}
			phrase := strings.TrimPrefix(r.statusLine, "HTTP/1.1 "+tt.status+" ")
			if tt.phrase != "" && phrase != tt.phrase {
				t.Errorf("phrase %q, want %q", phrase, tt.phrase)
			}
			if r.body != phrase+"\n" {
				t.Errorf("body %q, want the phrase %q", r.body, phrase)
			}
			if r.header.Get("Connection") != "close" ||
				r.header.Get("Content-Type") != "text/plain; charset=utf-8" {
				t.Errorf("headers %v", r.header)
			}
			if _, err := br.ReadByte(); !errors.Is(err, io.EOF) {
				t.Errorf("connection still open after the refusal: %v", err)
			}
			_ = c.Close() // ends the proxy's drain of what the client sent
			line := h.closeLine(t, 1)
			if line["msg"] != "refused" || line["outcome"] != tt.outcome || line["status"] != float64(atoi(tt.status)) {
				t.Errorf("close line %v, want refused %s", line, tt.outcome)
			}
			if n := h.lookup.names(); len(n) != 0 || h.up.accepted.Load() != 0 {
				t.Errorf("resolved %v and dialled %d times for a refusal", n, h.up.accepted.Load())
			}
			if len(h.linesWith(t, "tunnel opened")) != 0 {
				t.Error("a refusal wrote an opened line")
			}
		})
	}
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}

// TestTunnelNormalisesTheHost: an uppercase CONNECT host is lowercased
// before the allowlist, and the hello's SNI is judged against that.
func TestTunnelNormalisesTheHost(t *testing.T) {
	h := newTunnelHarness(t, Config{})
	tc, err := h.open(t, "web", "PKG.Registry.dev", "pkg.registry.dev")
	if err != nil {
		t.Fatal(err)
	}
	_ = tc.Close()
	if line := h.closeLine(t, 1); line["host"] != "pkg.registry.dev" || line["outcome"] != OutcomeTunnelled {
		t.Errorf("close line %v", line)
	}
}

// TestTunnelSuspended: a suspended tunnel profile answers CONNECT 410 with
// step 1's words, raw; a tunnel-only profile answers GET and HEAD 405
// Allow: CONNECT, and 410 when suspended.
func TestTunnelSuspended(t *testing.T) {
	h := newTunnelHarness(t, Config{Suspended: true})
	_, br := h.connect(t, "web", "CONNECT pkg.registry.dev:443 HTTP/1.1")
	r := readRaw(t, br)
	if r.statusLine != "HTTP/1.1 410 "+suspendedPhrase || r.body != suspendedPhrase+"\n" {
		t.Errorf("suspended CONNECT = %q %q", r.statusLine, r.body)
	}
	if line := h.closeLine(t, 1); line["outcome"] != OutcomeSuspended {
		t.Errorf("close line %v", line)
	}
	for _, m := range []string{http.MethodGet, http.MethodHead} {
		resp := doMethod(t, h.urls["web"], m)
		if resp.StatusCode != http.StatusGone {
			t.Errorf("suspended %s on a tunnel-only profile = %d, want 410", m, resp.StatusCode)
		}
	}

	h = newTunnelHarness(t, Config{})
	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodPost} {
		resp := doMethod(t, h.urls["web"], m)
		if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != "CONNECT" {
			t.Errorf("%s on a tunnel-only profile = %d Allow %q, want 405 Allow: CONNECT", m, resp.StatusCode,
				resp.Header.Get("Allow"))
		}
	}
	if l := h.log.lines(t); len(l) != 3 || l[0]["outcome"] != OutcomeMethod {
		t.Errorf("tunnel-only GET audit %v", l)
	}
}

func doMethod(t *testing.T, base, method string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, base+"/go/github.com/google/uuid/@v/list", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp
}

// goOnlyConnect405 and goOnlyConnect410 are step 1's answers to a CONNECT
// on a go-only profile, recorded from origin/main (0.12.34) with the Date
// header removed.
const (
	goOnlyConnect405 = "HTTP/1.1 405 Method Not Allowed\r\nAllow: GET, HEAD\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\nX-Content-Type-Options: nosniff\r\n" +
		"Content-Length: 32\r\nConnection: close\r\n\r\negress-proxy: only GET and HEAD\n"
	goOnlyConnect410 = "HTTP/1.1 410 Gone\r\nContent-Type: text/plain; charset=utf-8\r\n" +
		"X-Content-Type-Options: nosniff\r\nContent-Length: 59\r\nConnection: close\r\n\r\n" +
		"egress-proxy: profile routes are suspended by the operator\n"
)

// TestGoOnlyConnectUnchanged: a CONNECT on a profile without hosts gets
// step 1's answer byte for byte (405, and 410 when suspended), and never
// reaches the tunnel.
func TestGoOnlyConnectUnchanged(t *testing.T) {
	for _, tt := range []struct {
		suspended bool
		want      string
	}{{false, goOnlyConnect405}, {true, goOnlyConnect410}} {
		h := newTunnelHarness(t, Config{Suspended: tt.suspended})
		c, _ := h.connect(t, "plain", "CONNECT pkg.registry.dev:443 HTTP/1.1")
		got := readUntilBody(t, c, len(tt.want))
		if got != tt.want {
			t.Errorf("go-only CONNECT (suspended %v):\n got %q\nwant %q", tt.suspended, got, tt.want)
		}
		if len(h.lookup.names()) != 0 || len(h.linesWith(t, "tunnel opened")) != 0 {
			t.Error("a go-only CONNECT reached the tunnel")
		}
	}
}

// readUntilBody reads a response and drops its Date header line.
func readUntilBody(t *testing.T, c net.Conn, approx int) string {
	t.Helper()
	var buf bytes.Buffer
	b := make([]byte, 4096)
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	for buf.Len() < approx+40 {
		n, err := c.Read(b)
		buf.Write(b[:n])
		if err != nil {
			break
		}
	}
	var out []string
	for _, l := range strings.SplitAfter(buf.String(), "\r\n") {
		if !strings.HasPrefix(l, "Date: ") {
			out = append(out, l)
		}
	}
	return strings.Join(out, "")
}

// TestTunnelHelloRefusals: a hello whose SNI is not the CONNECT host, has
// none, carries ECH or ESNI, is malformed or never comes is answered with
// one TLS alert, and the upstream receives zero bytes.
func TestTunnelHelloRefusals(t *testing.T) {
	ech := record(22, 0x0301, buildHello(false, host("pkg.registry.dev"), ext{extECH, []byte{0}}))
	esni := record(22, 0x0301, buildHello(false, host("pkg.registry.dev"), ext{extESNI, []byte{0}}))
	noSNI := record(22, 0x0301, buildHello(false, alpnExt("h2")))
	twoSNI := record(22, 0x0301, buildHello(false, sniExt(sniEntry{sniHostName, "pkg.registry.dev"},
		sniEntry{sniHostName, "other.registry.dev"})))
	mismatch := record(22, 0x0301, buildHello(false, host("other.registry.dev")))
	trailing := record(22, 0x0301, buildHello(false, host("pkg.registry.dev.")))
	tests := []struct {
		name    string
		send    []byte
		outcome string
		reason  string
		alert   byte
	}{
		{"sni mismatch", mismatch, OutcomeSNIMismatch, "", alertUnrecognizedName},
		{"sni trailing dot", trailing, OutcomeSNIMismatch, "", alertUnrecognizedName},
		{"no sni", noSNI, OutcomeSNIMissing, "", alertUnrecognizedName},
		{"two sni", twoSNI, OutcomeSNIMissing, "", alertUnrecognizedName},
		{"ech", ech, OutcomeECH, "", alertHandshakeFailure},
		{"esni", esni, OutcomeECH, "", alertHandshakeFailure},
		{"not tls", []byte("GET / HTTP/1.1\r\nHost: pkg.registry.dev\r\n\r\n"), OutcomeBadHello, "",
			alertHandshakeFailure},
		{"application data first", record(23, 0x0303, []byte("x")), OutcomeBadHello, "", alertHandshakeFailure},
		{"timeout", nil, OutcomeBadHello, reasonTimeout, alertHandshakeFailure},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTunnelHarness(t, Config{TunnelLimits: TunnelLimits{HelloTimeout: 200 * time.Millisecond}})
			c, br := h.connect(t, "web", "CONNECT pkg.registry.dev:443 HTTP/1.1")
			if r := readRaw(t, br); r.statusLine != "HTTP/1.1 200 Connection established" {
				t.Fatalf("CONNECT = %q", r.statusLine)
			}
			if tt.send != nil {
				if _, err := c.Write(tt.send); err != nil {
					t.Fatal(err)
				}
			}
			got, _ := io.ReadAll(br)
			_ = c.Close()
			if want := []byte{recordTypeAlert, 3, 3, 0, 2, alertLevelFatal, tt.alert}; !bytes.Equal(got, want) {
				t.Errorf("client read %v, want the alert %v", got, want)
			}
			line := h.closeLine(t, 1)
			if line["outcome"] != tt.outcome || line["reason"] != tt.reason || line["msg"] != "refused" {
				t.Errorf("close line %v, want refused %s/%s", line, tt.outcome, tt.reason)
			}
			if n := h.up.received.Load(); n != 0 {
				t.Errorf("the upstream received %d bytes of a refused hello", n)
			}
			if line["bytes_up"] != float64(0) {
				t.Errorf("bytes_up %v, want 0", line["bytes_up"])
			}
		})
	}
}

// TestTunnelDeniedAddresses: whatever an allowlisted name resolves to, a
// private, link-local (IMDS, Pod Identity), shared, --deny-cidrs or
// tunnel-only denied address is refused 502 denied_address before any
// connection; a failed resolution is 502 dial_error.
func TestTunnelDeniedAddresses(t *testing.T) {
	deny, _ := ParseDenyCIDRs([]string{"8.8.8.0/24"})
	tunnelDeny, _ := ParseDenyCIDRs([]string{"93.184.216.0/24"})
	for _, a := range []string{"10.0.0.5", "169.254.169.254", "169.254.170.23", "100.64.1.1", "8.8.8.8",
		"93.184.216.34", "192.168.1.1"} {
		h := newTunnelHarness(t, Config{Policy: AddrPolicy{Deny: deny}, TunnelLimits: TunnelLimits{Deny: tunnelDeny}})
		h.lookup.answers["pkg.registry.dev."] = [][]netip.Addr{addrs(a)}
		c, br := h.connect(t, "web", "CONNECT pkg.registry.dev:443 HTTP/1.1")
		r := readRaw(t, br)
		_ = c.Close()
		if r.statusLine != "HTTP/1.1 502 egress-proxy: pkg.registry.dev resolves to an address the proxy never dials" {
			t.Errorf("%s: %q", a, r.statusLine)
		}
		if strings.Contains(r.statusLine+r.body, a) {
			t.Errorf("%s: the refusal names the address", a)
		}
		if line := h.closeLine(t, 1); line["outcome"] != OutcomeDeniedAddress {
			t.Errorf("%s: close line %v", a, line)
		}
	}
	h := newTunnelHarness(t, Config{})
	h.lookup.errs["pkg.registry.dev."] = errors.New("no such host")
	_, br := h.connect(t, "web", "CONNECT pkg.registry.dev:443 HTTP/1.1")
	if r := readRaw(t, br); r.statusLine != "HTTP/1.1 502 egress-proxy: could not connect to pkg.registry.dev" {
		t.Errorf("failed lookup: %q", r.statusLine)
	}
	if line := h.closeLine(t, 1); line["outcome"] != OutcomeDialError {
		t.Errorf("close line %v", line)
	}
}

// TestTunnelDenyIsTunnelOnly: the tunnel-only deny ranges reach the
// tunnel's dialer and never the Go route's.
func TestTunnelDenyIsTunnelOnly(t *testing.T) {
	tunnelDeny, _ := ParseDenyCIDRs([]string{"93.184.216.0/24"})
	fl := &fixedLookup{answers: [][]netip.Addr{addrs("93.184.216.34")}}
	srv, err := New(Config{Profiles: tunnelProfiles(t), GoProxy: mustURL(t, "https://proxy.test"),
		TunnelLimits: TunnelLimits{Deny: tunnelDeny}, lookup: fl.lookup}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // judged before the dial; nothing connects
	if _, err := srv.tunnelDialer.DialContext(ctx, "tcp4", "pkg.registry.dev.:443"); !errors.Is(err, ErrDeniedAddress) {
		t.Errorf("tunnel dial = %v, want ErrDeniedAddress", err)
	}
	_, err = srv.client.Transport.(*http.Transport).DialContext(ctx, "tcp", "proxy.test:443")
	if err == nil || errors.Is(err, ErrDeniedAddress) {
		t.Errorf("go route dial = %v, want only the cancelled context", err)
	}
}

// TestTunnelRebinding: a name that resolved to an admissible address and
// then to a private one is refused the second time.
func TestTunnelRebinding(t *testing.T) {
	h := newTunnelHarness(t, Config{})
	h.lookup.answers["pkg.registry.dev."] = [][]netip.Addr{addrs("127.0.0.1"), addrs("10.0.0.5")}
	tc, err := h.open(t, "web", "pkg.registry.dev", "")
	if err != nil {
		t.Fatal(err)
	}
	_ = tc.Close()
	_, br := h.connect(t, "web", "CONNECT pkg.registry.dev:443 HTTP/1.1")
	if r := readRaw(t, br); !strings.HasPrefix(r.statusLine, "HTTP/1.1 502 ") {
		t.Errorf("rebound CONNECT = %q, want 502", r.statusLine)
	}
	if h.up.accepted.Load() != 1 {
		t.Errorf("upstream accepted %d connections, want 1", h.up.accepted.Load())
	}
}
