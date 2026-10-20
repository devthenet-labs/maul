// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"math/rand"
	"net"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/quick"
	"time"

	"github.com/devthenet-labs/maul/internal/netprofile"
)

// fakeClock is a settable clock.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func defaultTunnelLimits(t *testing.T, l TunnelLimits) TunnelLimits {
	t.Helper()
	l, err := l.withDefaults()
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// TestTunnelDefaultsFromMeasurements pins the defaults the live client
// fixtures set (step-3 spec §2.5, PR #213): the upload caps at about 4x the
// largest measured (21 KB a tunnel, 602 KB a source), and the per-source
// concurrency and rate above what pnpm 11 and yarn 4 asked for (50 open,
// 183 new a second), so an ordinary install is never queued. A change here
// is a change to what an install may upload or how fast it runs: re-measure
// first.
func TestTunnelDefaultsFromMeasurements(t *testing.T) {
	l := defaultTunnelLimits(t, TunnelLimits{})
	for _, c := range []struct {
		name      string
		got, want any
	}{
		{"max upload bytes", l.MaxUploadBytes, int64(1 << 20)},
		{"source upload budget", l.SourceUploadBudget, int64(8 << 20)},
		{"max concurrent", l.MaxConcurrent, 512},
		{"max concurrent per source", l.PerSourceConcurrent, 64},
		{"rate per source", l.PerSourceRate, 50.0},
		{"burst per source", l.PerSourceBurst, 200},
	} {
		if c.got != c.want {
			t.Errorf("default %s = %v, want %v", c.name, c.got, c.want)
		}
	}
	// The measured peaks fit under the limits with room to spare.
	const pnpmYarnOpen, yarnNewPerSecond, largestSourceUpload, largestTunnelUpload = 50, 183, 601996, 21486
	if l.PerSourceConcurrent < pnpmYarnOpen || float64(l.PerSourceBurst) < yarnNewPerSecond {
		t.Errorf("per-source limits %d open, burst %d are below the measured peaks %d open, %d new a second",
			l.PerSourceConcurrent, l.PerSourceBurst, pnpmYarnOpen, yarnNewPerSecond)
	}
	if l.SourceUploadBudget < 4*largestSourceUpload || l.MaxUploadBytes < 4*largestTunnelUpload {
		t.Errorf("upload caps %d a tunnel, %d a source are under 4x the measured %d, %d",
			l.MaxUploadBytes, l.SourceUploadBudget, largestTunnelUpload, largestSourceUpload)
	}
}

// TestBudgetKeptAcrossAPause: a source that spent its budget, closed
// every tunnel and paused for most of the longest Job deadline (90m - 1m)
// is still refused; only a whole window after its last tunnel does the
// entry expire.
func TestBudgetKeptAcrossAPause(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_800_000_000, 0)}
	l := defaultTunnelLimits(t, TunnelLimits{SourceUploadBudget: 1000})
	b := newBudgetTable(netprofile.Profile{}, l, clock.Now)
	e, reason := b.open("10.1.0.7")
	if reason != "" {
		t.Fatal(reason)
	}
	if g := b.take(e, up, 900); g != 900 {
		t.Fatalf("granted %d, want 900", g)
	}
	if g := b.take(e, up, 500); g != 100 {
		t.Fatalf("granted %d past the budget, want the 100 left", g)
	}
	b.close(e)
	clock.advance(90*time.Minute - time.Minute)
	if _, reason := b.open("10.1.0.7"); reason != reasonSourceUploadBudget {
		t.Errorf("after a pause shorter than a Job deadline: %q, want %s", reason, reasonSourceUploadBudget)
	}
	clock.advance(DefaultTunnelSourceBudgetIdle - (90*time.Minute - time.Minute) - time.Second)
	if _, reason := b.open("10.1.0.7"); reason != reasonSourceUploadBudget {
		t.Errorf("a second before the window ends: %q, want still refused", reason)
	}
	clock.advance(time.Second)
	if e, reason := b.open("10.1.0.7"); reason != "" || e.used[up] != 0 {
		t.Errorf("after a whole window: %q, %v, want a fresh budget", reason, e)
	}
}

// TestBudgetWindowStartsAtLastClose: an open tunnel keeps the entry alive
// however long it runs; the window counts from the last close.
func TestBudgetWindowStartsAtLastClose(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_800_000_000, 0)}
	l := defaultTunnelLimits(t, TunnelLimits{SourceDownloadBudget: 10})
	b := newBudgetTable(netprofile.Profile{}, l, clock.Now)
	e, _ := b.open("ip")
	b.take(e, down, 10)
	clock.advance(5 * time.Hour) // still open
	if _, reason := b.open("ip"); reason != reasonSourceDownloadBudget {
		t.Errorf("open entry: %q, want %s", reason, reasonSourceDownloadBudget)
	}
	b.close(e)
	clock.advance(time.Hour)
	if _, reason := b.open("ip"); reason != reasonSourceDownloadBudget {
		t.Errorf("an hour after the close: %q, want still refused", reason)
	}
}

// TestBudgetTableRefusesWhenFull: a full table refuses a new source and
// evicts no live entry; an expired entry makes room.
func TestBudgetTableRefusesWhenFull(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_800_000_000, 0)}
	l := defaultTunnelLimits(t, TunnelLimits{MaxSources: 2})
	b := newBudgetTable(netprofile.Profile{}, l, clock.Now)
	a, _ := b.open("a")
	c, _ := b.open("b")
	b.take(a, up, 7)
	if _, reason := b.open("c"); reason != reasonSourceTableFull {
		t.Errorf("third source: %q, want %s", reason, reasonSourceTableFull)
	}
	if b.entries["a"].used[up] != 7 || len(b.entries) != 2 {
		t.Error("a full table evicted an entry")
	}
	b.close(a)
	b.close(c)
	clock.advance(time.Hour)
	if _, reason := b.open("c"); reason != reasonSourceTableFull {
		t.Errorf("before any entry expired: %q, want %s", reason, reasonSourceTableFull)
	}
	clock.advance(DefaultTunnelSourceBudgetIdle)
	if _, reason := b.open("c"); reason != "" {
		t.Errorf("after the entries expired: %q, want admitted", reason)
	}
}

// TestBudgetProfileOverride: a profile's upload.perSource replaces the
// flag's budget for that profile only.
func TestBudgetProfileOverride(t *testing.T) {
	l := defaultTunnelLimits(t, TunnelLimits{})
	b := newBudgetTable(netprofile.Profile{Upload: &netprofile.Upload{PerSource: 70000}}, l, time.Now)
	if b.limit[up] != 70000 || b.limit[down] != DefaultTunnelSourceDownloadBudget {
		t.Errorf("limits %v", b.limit)
	}
	b = newBudgetTable(netprofile.Profile{Upload: &netprofile.Upload{PerTunnel: 70000}}, l, time.Now)
	if b.limit[up] != DefaultTunnelSourceUploadBudget {
		t.Errorf("a perTunnel-only override changed the source budget: %v", b.limit)
	}
}

// TestTunnelLimitsRefuseAShortWindow: a budget window below the floor (the
// longest Job deadline) refuses to start.
func TestTunnelLimitsRefuseAShortWindow(t *testing.T) {
	_, err := New(Config{Profiles: tunnelProfiles(t), TunnelLimits: TunnelLimits{SourceBudgetIdle: 30 * time.Minute}},
		nil)
	if err == nil || !strings.Contains(err.Error(), "below the minimum") {
		t.Errorf("New = %v, want a refusal of the short window", err)
	}
	_, err = New(Config{Profiles: tunnelProfiles(t), TunnelLimits: TunnelLimits{SourceBudgetIdle: 3 * time.Hour,
		MinBudgetIdle: 3 * time.Hour}}, nil)
	if err != nil {
		t.Errorf("New with the window at its floor = %v", err)
	}
	l := defaultTunnelLimits(t, TunnelLimits{})
	if l.ProgressInterval != DefaultTunnelProgressInterval || l.MaxSources != DefaultTunnelMaxSources ||
		l.SourceBudgetIdle != DefaultTunnelSourceBudgetIdle || l.MaxUploadBytes != DefaultTunnelMaxUploadBytes {
		t.Errorf("defaults %+v", l)
	}
	if off := defaultTunnelLimits(t, TunnelLimits{ProgressInterval: -1}); off.ProgressInterval >= 0 {
		t.Error("a negative progress interval did not stay off")
	}
}

// sendAfterHandshake opens a tunnel and writes n bytes, returning the
// write error.
func sendAfterHandshake(t *testing.T, h *tunnelHarness, profile, host string, n int) error {
	t.Helper()
	tc, err := h.open(t, profile, host, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tc.Close() }()
	_, err = tc.Write(bytes.Repeat([]byte{'u'}, n))
	_, _ = io.Copy(io.Discard, tc)
	return err
}

// TestTunnelUploadCap: a tunnel that crosses the per-tunnel upload cap is
// closed; the upstream received at most the cap.
func TestTunnelUploadCap(t *testing.T) {
	h := newTunnelHarness(t, Config{TunnelLimits: TunnelLimits{MaxUploadBytes: 8192}})
	h.up.serve = func(c *tls.Conn) { _, _ = io.Copy(io.Discard, c) }
	_ = sendAfterHandshake(t, h, "web", "pkg.registry.dev", 16384)
	line := h.closeLine(t, 1)
	if line["outcome"] != OutcomeUploadCap || line["reason"] != reasonTunnelCap || line["msg"] != "tunnel" {
		t.Errorf("close line %v, want upload_cap/tunnel", line)
	}
	if got := h.up.received.Load(); got > 8192 || line["bytes_up"] != float64(8192) {
		t.Errorf("upstream received %d, bytes_up %v; want the cap, 8192, exactly on the line", got, line["bytes_up"])
	}
}

// TestTunnelProfileUploadOverride: a profile's upload.perTunnel replaces
// the flag's cap on that profile only.
func TestTunnelProfileUploadOverride(t *testing.T) {
	h := newTunnelHarness(t, Config{})
	h.up.serve = func(c *tls.Conn) { _, _ = io.Copy(io.Discard, c) }
	_ = sendAfterHandshake(t, h, "go", "npm.registry.dev", 100<<10)
	if line := h.closeLine(t, 1); line["outcome"] != OutcomeUploadCap || line["bytes_up"] != float64(65536) {
		t.Errorf("go profile (perTunnel 65536): %v", line)
	}
	h2 := newTunnelHarness(t, Config{})
	tc, err := h2.open(t, "web", "pkg.registry.dev", "")
	if err != nil {
		t.Fatal(err)
	}
	msg := bytes.Repeat([]byte{'u'}, 100<<10)
	go func() { _, _ = tc.Write(msg) }()
	if _, err := io.ReadFull(tc, make([]byte, len(msg))); err != nil {
		t.Errorf("web profile (the 1 MiB default) cut a 100 KiB upload: %v", err)
	}
	_ = tc.Close()
}

// TestTunnelDownloadCap: the upstream sending past the per-tunnel download
// cap closes the tunnel.
func TestTunnelDownloadCap(t *testing.T) {
	h := newTunnelHarness(t, Config{TunnelLimits: TunnelLimits{MaxDownloadBytes: 16384}})
	h.up.serve = func(c *tls.Conn) { _, _ = c.Write(bytes.Repeat([]byte{'d'}, 64<<10)) }
	_ = sendAfterHandshake(t, h, "web", "pkg.registry.dev", 1)
	line := h.closeLine(t, 1)
	if line["outcome"] != OutcomeDownloadCap || line["bytes_down"] != float64(16384) {
		t.Errorf("close line %v, want download_cap at 16384", line)
	}
}

// TestTunnelSourceBudget: a tunnel open when its source's budget runs out
// is cut; the next CONNECT from that source is refused 503 before any
// resolve or dial; another profile keeps its own budget for the source.
func TestTunnelSourceBudget(t *testing.T) {
	h := newTunnelHarness(t, Config{TunnelLimits: TunnelLimits{SourceUploadBudget: 20000}})
	h.up.serve = func(c *tls.Conn) { _, _ = io.Copy(io.Discard, c) }
	_ = sendAfterHandshake(t, h, "web", "pkg.registry.dev", 30000)
	line := h.closeLine(t, 1)
	if line["outcome"] != OutcomeUploadCap || line["reason"] != reasonSourceUploadBudget {
		t.Errorf("close line %v, want upload_cap/source_upload_budget", line)
	}
	if got := h.up.received.Load(); got > 20000 {
		t.Errorf("upstream received %d past the 20000-byte budget", got)
	}
	c, br := h.connect(t, "web", "CONNECT files.registry.dev:443 HTTP/1.1")
	r := readRaw(t, br)
	_ = c.Close()
	if r.statusLine != "HTTP/1.1 503 egress-proxy: over the tunnel limit (source_upload_budget)" {
		t.Errorf("CONNECT after the budget = %q", r.statusLine)
	}
	if names := h.lookup.names(); len(names) != 1 {
		t.Errorf("resolved %v; the refusal must come before any lookup", names)
	}
	// The go profile's own budget (perSource 131072) is untouched.
	tc, err := h.open(t, "go", "npm.registry.dev", "")
	if err != nil {
		t.Fatalf("another profile for the same source: %v", err)
	}
	_ = tc.Close()
}

// TestRefusalsNeverWriteHeader: no serveConnect path answers through
// net/http (which cannot carry a custom reason phrase): every one writes
// raw on the hijacked connection.
func TestRefusalsNeverWriteHeader(t *testing.T) {
	var called atomic.Bool
	h := newTunnelHarness(t, Config{TunnelLimits: TunnelLimits{SourceUploadBudget: 1, HelloTimeout: 100 *
		time.Millisecond}})
	handler, _ := h.srv.Handler("web")
	ts := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(&noWriteHeader{ResponseWriter: w, called: &called}, r)
	})}
	h.srv.ConfigureListener(ts)
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = ts.Serve(ln) }()
	t.Cleanup(func() { _ = ts.Close() })
	h.addrs["web"] = ln.Addr().String()
	for _, line := range []string{
		"CONNECT other.registry.dev:443 HTTP/1.1", "CONNECT github.com:443 HTTP/1.1",
		"CONNECT 1.2.3.4:443 HTTP/1.1", "CONNECT pkg.registry.dev:80 HTTP/1.1", "CONNECT a/b:443 HTTP/1.1",
		"CONNECT pkg.registry.dev:443 HTTP/1.0",
		"CONNECT pkg.registry.dev:443 HTTP/1.1", // hello timeout, after the 200
		"CONNECT pkg.registry.dev:443 HTTP/1.1", // spent budget: 503
	} {
		c, br := h.connect(t, "web", line)
		_ = readRaw(t, br)
		_, _ = io.ReadAll(br)
		_ = c.Close()
	}
	h.closeLine(t, 8)
	if called.Load() {
		t.Error("a serveConnect path called WriteHeader or Write on the ResponseWriter")
	}
}

// noWriteHeader records any WriteHeader or Write; Unwrap keeps Hijack.
type noWriteHeader struct {
	http.ResponseWriter
	called *atomic.Bool
}

func (w *noWriteHeader) WriteHeader(int)             { w.called.Store(true) }
func (w *noWriteHeader) Write(b []byte) (int, error) { w.called.Store(true); return len(b), nil }
func (w *noWriteHeader) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// TestWriteRawRefusalNeverSplits: a control byte in a phrase never reaches
// the wire as one.
func TestWriteRawRefusalNeverSplits(t *testing.T) {
	var b bytes.Buffer
	if err := writeRawRefusal(&b, 403, "egress-proxy: a\r\nSet-Cookie: x\x00"); err != nil {
		t.Fatal(err)
	}
	if got := b.String(); strings.Count(got, "\r\n") != 5 || strings.Contains(got, "\nSet-Cookie") {
		t.Errorf("refusal %q", got)
	}
}

// TestTunnelConcurrencyLimits: the per-source and global tunnel caps and
// the per-source rate answer 503 with their reason; the Go route keeps
// serving while every tunnel slot is taken (separate limiters).
func TestTunnelConcurrencyLimits(t *testing.T) {
	goUp := newUpstream(t, okBody("v1.0.0\n"))
	for _, tt := range []struct {
		limits TunnelLimits
		reason string
		close  bool
	}{
		{TunnelLimits{PerSourceConcurrent: 1, Wait: 50 * time.Millisecond}, reasonConcurrency, false},
		{TunnelLimits{MaxConcurrent: 1, Wait: 50 * time.Millisecond}, reasonGlobal, false},
		{TunnelLimits{PerSourceRate: 0.001, PerSourceBurst: 1, Wait: 50 * time.Millisecond}, reasonRate, true},
	} {
		h := newTunnelHarness(t, Config{TunnelLimits: tt.limits, GoProxy: mustURL(t, goUp.URL),
			Limits: Limits{MaxConcurrent: 1}})
		tc, err := h.open(t, "web", "pkg.registry.dev", "")
		if err != nil {
			t.Fatal(err)
		}
		if tt.close {
			_ = tc.Close()
			h.closeLine(t, 1)
		}
		c, br := h.connect(t, "web", "CONNECT files.registry.dev:443 HTTP/1.1")
		r := readRaw(t, br)
		_ = c.Close()
		if want := "HTTP/1.1 503 egress-proxy: over the tunnel limit (" + tt.reason + ")"; r.statusLine != want {
			t.Errorf("%s: %q, want %q", tt.reason, r.statusLine, want)
		}
		resp, err := http.Get(h.urls["plain"] + "/go/github.com/google/uuid/@v/list")
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Errorf("%s: the Go route while the tunnel is limited = %v, %v", tt.reason, resp, err)
		} else {
			_ = resp.Body.Close()
		}
		_ = tc.Close()
	}
}

// TestTunnelAuditLines: the opened line is written at info with the 200,
// before any hello byte; a long tunnel writes progress lines with rising
// counts; exactly one opened and one close line per tunnel.
func TestTunnelAuditLines(t *testing.T) {
	h := newTunnelHarness(t, Config{TunnelLimits: TunnelLimits{ProgressInterval: 20 * time.Millisecond}})
	c, br := h.connect(t, "web", "CONNECT pkg.registry.dev:443 HTTP/1.1")
	if r := readRaw(t, br); r.statusLine != "HTTP/1.1 200 Connection established" {
		t.Fatal(r.statusLine)
	}
	opened := h.linesWith(t, "tunnel opened")
	if len(opened) != 1 || opened[0]["level"] != "INFO" || opened[0]["host"] != "pkg.registry.dev" {
		t.Fatalf("opened lines before the hello: %v", opened)
	}
	tc := tls.Client(&bufferedConn{Conn: c, r: br}, &tls.Config{ServerName: "pkg.registry.dev", RootCAs: h.up.pool})
	for range 6 {
		if _, err := tc.Write(bytes.Repeat([]byte{'p'}, 1000)); err != nil {
			t.Fatal(err)
		}
		_, _ = io.ReadFull(tc, make([]byte, 1000))
		time.Sleep(25 * time.Millisecond)
	}
	_ = tc.Close()
	h.closeLine(t, 1)
	progress := h.linesWith(t, "tunnel progress")
	if len(progress) < 2 {
		t.Fatalf("%d progress lines, want several", len(progress))
	}
	first, last := progress[0]["bytes_up"].(float64), progress[len(progress)-1]["bytes_up"].(float64)
	if last <= first || progress[0]["level"] != "INFO" {
		t.Errorf("progress bytes_up %v then %v, want rising, at INFO", first, last)
	}
	if n := len(h.linesWith(t, "tunnel opened")); n != 1 {
		t.Errorf("%d opened lines", n)
	}
	if n := len(h.linesWith(t, "tunnel")); n != 1 {
		t.Errorf("%d close lines", n)
	}
}

// TestTunnelDeadlines: no byte in either direction for the idle timeout
// ends a tunnel; so does the absolute lifetime, however busy it is.
func TestTunnelDeadlines(t *testing.T) {
	h := newTunnelHarness(t, Config{TunnelLimits: TunnelLimits{IdleTimeout: 150 * time.Millisecond}})
	tc, err := h.open(t, "web", "pkg.registry.dev", "")
	if err != nil {
		t.Fatal(err)
	}
	if line := h.closeLine(t, 1); line["outcome"] != OutcomeIdleTimeout {
		t.Errorf("idle tunnel: %v", line)
	}
	_ = tc.Close()

	h = newTunnelHarness(t, Config{TunnelLimits: TunnelLimits{MaxDuration: 300 * time.Millisecond}})
	tc, err = h.open(t, "web", "pkg.registry.dev", "")
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
				if _, err := tc.Write([]byte("busy")); err != nil {
					return
				}
			}
		}
	}()
	go func() { _, _ = io.Copy(io.Discard, tc) }()
	if line := h.closeLine(t, 1); line["outcome"] != OutcomeMaxDuration {
		t.Errorf("busy tunnel past its lifetime: %v", line)
	}
	close(stop)
	_ = tc.Close()
}

// TestTunnelShutdown: shutting the listener down closes the hijacked
// tunnels net/http no longer tracks.
func TestTunnelShutdown(t *testing.T) {
	h := newTunnelHarness(t, Config{})
	tc, err := h.open(t, "web", "pkg.registry.dev", "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := h.https["web"].Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if line := h.closeLine(t, 1); line["outcome"] != OutcomeShutdown {
		t.Errorf("close line %v, want shutdown", line)
	}
	_ = tc.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := tc.Read(make([]byte, 1)); err == nil {
		t.Error("the tunnel stayed open after Shutdown")
	}
}

// TestProfileListenerRefusesH2C: the prior-knowledge h2c preface is never
// spoken to, on a tunnel profile and on a go profile alike.
func TestProfileListenerRefusesH2C(t *testing.T) {
	h := newTunnelHarness(t, Config{})
	for _, p := range []string{"web", "plain"} {
		c, err := net.Dial("tcp4", h.addrs[p])
		if err != nil {
			t.Fatal(err)
		}
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		_, _ = io.WriteString(c, "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n")
		got, _ := io.ReadAll(c)
		_ = c.Close()
		// net/http hands the HTTP/1-only listener's PRI request to the
		// handler like any other method, which refuses it.
		if !bytes.HasPrefix(got, []byte("HTTP/1.1 405 ")) {
			t.Errorf("%s answered the h2c preface with %q, want an HTTP/1.1 405", p, got)
		}
	}
	if h.https["web"].Protocols == nil || h.https["web"].Protocols.UnencryptedHTTP2() || h.https["web"].Protocols.HTTP2() {
		t.Error("the profile listener speaks more than HTTP/1")
	}
}

// TestTunnelMetrics: tunnels by outcome with the host only when it is
// allowlisted, bytes per direction, and nothing left open.
func TestTunnelMetrics(t *testing.T) {
	testMetrics()
	tunnelled := map[string]string{"profile": "web", "outcome": OutcomeTunnelled, "host": "pkg.registry.dev"}
	refused := map[string]string{"profile": "web", "outcome": OutcomeNotAllowed, "host": ""}
	bytesUp := map[string]string{"profile": "web", "host": "pkg.registry.dev", "direction": "up"}
	before := [3]int64{sum(t, "maul.egress_proxy.tunnels", tunnelled),
		sum(t, "maul.egress_proxy.tunnels", refused), sum(t, "maul.egress_proxy.tunnel_bytes", bytesUp)}
	h := newTunnelHarness(t, Config{})
	tc, err := h.open(t, "web", "pkg.registry.dev", "")
	if err != nil {
		t.Fatal(err)
	}
	_ = tc.Close()
	h.closeLine(t, 1)
	c, br := h.connect(t, "web", "CONNECT other.registry.dev:443 HTTP/1.1")
	_ = readRaw(t, br)
	_ = c.Close()
	h.closeLine(t, 2)
	if got := sum(t, "maul.egress_proxy.tunnels", tunnelled) - before[0]; got != 1 {
		t.Errorf("tunnelled count +%d, want +1", got)
	}
	if got := sum(t, "maul.egress_proxy.tunnels", refused) - before[1]; got != 1 {
		t.Errorf("refused count with host=\"\" +%d, want +1", got)
	}
	if sum(t, "maul.egress_proxy.tunnels", map[string]string{"host": "other.registry.dev"}) != 0 {
		t.Error("a refused host became a metric attribute")
	}
	if got := sum(t, "maul.egress_proxy.tunnel_bytes", bytesUp) - before[2]; got <= 0 {
		t.Errorf("tunnel bytes up +%d, want some", got)
	}
	if got := sum(t, "maul.egress_proxy.tunnels_open", map[string]string{"profile": "web"}); got != 0 {
		t.Errorf("tunnels_open = %d after every tunnel closed", got)
	}
}

// genCaseHost is an allowlist-shaped host with random letter case.
type genCaseHost struct{ lower, sent string }

func (genCaseHost) Generate(r *rand.Rand, _ int) reflect.Value {
	h := string(genHost("").Generate(r, 0).Interface().(genHost))
	b := []byte(h)
	for i := range b {
		if b[i] >= 'a' && b[i] <= 'z' && r.Intn(3) == 0 {
			b[i] -= 'a' - 'A'
		}
	}
	return reflect.ValueOf(genCaseHost{lower: h, sent: string(b)})
}

// TestPropertyJudgeTarget is the combined CONNECT property (§8.2): an
// admitted target is exactly an allowed host, in any letter case, plus
// ":443"; and every host that passes the grammar and the deny table is
// admitted when it is in the profile's hosts.
func TestPropertyJudgeTarget(t *testing.T) {
	deny := netprofile.DenyContext{Hosts: []string{"github.com"}}
	admitted := func(target genTarget, h genCaseHost) bool {
		p := netprofile.Profile{Name: "p", Hosts: netprofile.Hosts{h.lower}}
		for _, tg := range []string{string(target), h.sent + ":443", h.sent + string(target)} {
			host, _, rf := judgeTarget(p, deny, tg)
			if rf != nil {
				continue
			}
			raw, ok := strings.CutSuffix(tg, ":443")
			n, err := netprofile.NormalizeHost(raw)
			if !ok || err != nil || n != host || !netprofile.ValidHost(host) || !p.Hosts.Allows(host) ||
				netprofile.DeniedTunnelHost(host, deny) != nil {
				t.Logf("judgeTarget(%q) admitted %q", tg, host)
				return false
			}
		}
		return true
	}
	if err := quick.Check(admitted, quickConfig(3000)); err != nil {
		t.Error(err)
	}
	complete := func(h genCaseHost) bool {
		p := netprofile.Profile{Name: "p", Hosts: netprofile.Hosts{h.lower}}
		host, _, rf := judgeTarget(p, deny, h.sent+":443")
		if netprofile.DeniedTunnelHost(h.lower, deny) != nil {
			return rf != nil && rf.outcome == OutcomeHardDenied
		}
		return rf == nil && host == h.lower
	}
	if err := quick.Check(complete, quickConfig(3000)); err != nil {
		t.Error(err)
	}
}
