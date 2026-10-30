// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package cdpshim

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

var testKey = []byte(strings.Repeat("k", 43) + "=")

// fakeLauncher launches in-process fake pipe peers and counts launches and
// kills; answer is what each peer does (echo when nil).
type fakeLauncher struct {
	t        *testing.T
	answer   func(in *bufio.Reader, out io.Writer)
	launched atomic.Int32
	killed   atomic.Int32
	fail     bool
}

func (f *fakeLauncher) Launch() (Browser, error) {
	if f.fail {
		return nil, errors.New("no chromium")
	}
	f.launched.Add(1)
	answer := f.answer
	if answer == nil {
		answer = echo
	}
	return newPeerPipe(f.t, answer, func() { f.killed.Add(1) }), nil
}

// syncBuffer is a log sink the shim's goroutines may write concurrently.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// start serves a shim over f and returns it with its server.
func start(t *testing.T, f *fakeLauncher, tweak func(*Options)) (*Shim, *httptest.Server, *syncBuffer) {
	t.Helper()
	logs := &syncBuffer{}
	o := Options{Key: testKey, Launch: f.Launch, Log: slog.New(slog.NewTextHandler(logs, nil))}
	if tweak != nil {
		tweak(&o)
	}
	s, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		s.Close()
		srv.Close()
		if strings.Contains(logs.String(), string(testKey)) {
			t.Error("the key reached the log")
		}
	})
	return s, srv, logs
}

// noOrigin drops the Origin line x/net/websocket's client always writes,
// as Playwright's CDP client (which sends none) would.
type noOrigin struct {
	net.Conn
	once sync.Once
}

func (c *noOrigin) Write(b []byte) (int, error) {
	out := b
	c.once.Do(func() {
		var kept []string
		for line := range strings.SplitAfterSeq(string(b), "\r\n") {
			if !strings.HasPrefix(line, "Origin: ") {
				kept = append(kept, line)
			}
		}
		out = []byte(strings.Join(kept, ""))
	})
	if _, err := c.Conn.Write(out); err != nil {
		return 0, err
	}
	return len(b), nil
}

// dial opens a CDP WebSocket on srv with key (none when nil); withOrigin
// keeps the client's Origin header.
func dial(t *testing.T, srv *httptest.Server, key []byte, withOrigin bool) (*websocket.Conn, error) {
	t.Helper()
	u := "ws" + strings.TrimPrefix(srv.URL, "http") + "/devtools/browser/shim"
	cfg, err := websocket.NewConfig(u, "http://localhost/")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Header = http.Header{}
	if key != nil {
		cfg.Header.Set(KeyHeader, string(key))
	}
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	rwc := conn
	if !withOrigin {
		rwc = &noOrigin{Conn: conn}
	}
	ws, err := websocket.NewClient(cfg, rwc)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	ws.MaxPayloadBytes = MaxMessageBytes
	t.Cleanup(func() { _ = ws.Close() })
	return ws, nil
}

// get sends a plain GET to path with the given headers.
func get(t *testing.T, srv *httptest.Server, method, path string, header http.Header) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestGate: no key, a wrong key and any Origin header are refused before
// any upgrade, on both endpoints, and no browser starts for them.
func TestGate(t *testing.T) {
	f := &fakeLauncher{t: t}
	_, srv, _ := start(t, f, nil)
	key := http.Header{KeyHeader: {string(testKey)}}
	withOrigin := func(origin string) http.Header {
		h := key.Clone()
		h["Origin"] = []string{origin}
		return h
	}
	wsHeaders := func(h http.Header) http.Header {
		h = h.Clone()
		h.Set("Upgrade", "websocket")
		h.Set("Connection", "Upgrade")
		h.Set("Sec-WebSocket-Version", "13")
		h.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
		return h
	}
	cases := []struct {
		name   string
		header http.Header
		want   int
	}{
		{"no key", http.Header{}, http.StatusUnauthorized},
		{"empty key", http.Header{KeyHeader: {""}}, http.StatusUnauthorized},
		{"wrong key", http.Header{KeyHeader: {strings.Repeat("x", len(testKey))}}, http.StatusUnauthorized},
		{"key prefix", http.Header{KeyHeader: {string(testKey[:len(testKey)-1])}}, http.StatusUnauthorized},
		{"key and more", http.Header{KeyHeader: {string(testKey) + "x"}}, http.StatusUnauthorized},
		{"origin", withOrigin("http://evil.example"), http.StatusForbidden},
		{"null origin", withOrigin("null"), http.StatusForbidden},
		{"empty origin", withOrigin(""), http.StatusForbidden},
		{"origin without key", http.Header{"Origin": {"http://localhost"}}, http.StatusForbidden},
	}
	for _, tc := range cases {
		for _, path := range []string{"/json/version", "/devtools/browser/shim"} {
			t.Run(tc.name+" "+path, func(t *testing.T) {
				h := tc.header
				if strings.HasPrefix(path, "/devtools") {
					h = wsHeaders(h)
				}
				if resp := get(t, srv, http.MethodGet, path, h); resp.StatusCode != tc.want {
					t.Errorf("status %d, want %d", resp.StatusCode, tc.want)
				}
			})
		}
	}
	if _, err := dial(t, srv, nil, false); err == nil {
		t.Error("WebSocket without a key admitted")
	}
	if _, err := dial(t, srv, testKey, true); err == nil {
		t.Error("WebSocket with an Origin admitted")
	}
	if n := f.launched.Load(); n != 0 {
		t.Errorf("%d browsers launched for refused requests", n)
	}
}

// TestRoutes: only GET of the two endpoints is served.
func TestRoutes(t *testing.T) {
	_, srv, _ := start(t, &fakeLauncher{t: t}, nil)
	key := http.Header{KeyHeader: {string(testKey)}}
	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{http.MethodPost, "/json/version", http.StatusMethodNotAllowed},
		{http.MethodGet, "/json/list", http.StatusNotFound},
		{http.MethodGet, "/json/new", http.StatusNotFound},
		{http.MethodGet, "/devtools/page/x", http.StatusNotFound},
	} {
		if resp := get(t, srv, tc.method, tc.path, key); resp.StatusCode != tc.want {
			t.Errorf("%s %s: status %d, want %d", tc.method, tc.path, resp.StatusCode, tc.want)
		}
	}
}

// TestVersion: /json/version names the shim's own WebSocket at the address
// the caller dialled, which is what connectOverCDP follows.
func TestVersion(t *testing.T) {
	_, srv, _ := start(t, &fakeLauncher{t: t}, nil)
	resp := get(t, srv, http.MethodGet, "/json/version", http.Header{KeyHeader: {string(testKey)}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var v map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	host := strings.TrimPrefix(srv.URL, "http://")
	if want := "ws://" + host + "/devtools/browser/shim"; v["webSocketDebuggerUrl"] != want {
		t.Errorf("webSocketDebuggerUrl %q, want %q", v["webSocketDebuggerUrl"], want)
	}
	if v["Browser"] == "" || v["Protocol-Version"] == "" {
		t.Errorf("version %v", v)
	}
}

// TestBridge: CDP messages pass both ways unchanged, one browser per
// connection, and the browser is killed when the client disconnects.
func TestBridge(t *testing.T) {
	f := &fakeLauncher{t: t}
	s, srv, _ := start(t, f, nil)
	ws, err := dial(t, srv, testKey, false)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	for _, msg := range []string{`{"id":1,"method":"Browser.getVersion"}`, strings.Repeat("z", 2<<20)} {
		if err := websocket.Message.Send(ws, msg); err != nil {
			t.Fatal(err)
		}
		var got string
		if err := websocket.Message.Receive(ws, &got); err != nil {
			t.Fatal(err)
		}
		if got != msg {
			t.Errorf("got %d bytes back, want %d", len(got), len(msg))
		}
	}
	if s.Live() != 1 || f.launched.Load() != 1 {
		t.Errorf("live %d launched %d, want 1 and 1", s.Live(), f.launched.Load())
	}
	_ = ws.Close()
	eventually(t, "the browser killed", func() bool { return f.killed.Load() == 1 && s.Live() == 0 })
}

// TestBrowserExit: when Chromium goes away the client is disconnected.
func TestBrowserExit(t *testing.T) {
	f := &fakeLauncher{t: t, answer: func(in *bufio.Reader, out io.Writer) {
		_, _ = in.ReadBytes(0)
		_, _ = out.Write([]byte(`{"bye":true}` + "\x00"))
	}}
	s, srv, _ := start(t, f, nil)
	ws, err := dial(t, srv, testKey, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := websocket.Message.Send(ws, `{"id":1}`); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := websocket.Message.Receive(ws, &got); err != nil || got != `{"bye":true}` {
		t.Fatalf("Receive = %q, %v", got, err)
	}
	if err := websocket.Message.Receive(ws, &got); err == nil {
		t.Fatal("connection open after the browser exited")
	}
	eventually(t, "the slot freed", func() bool { return s.Live() == 0 })
}

// TestNULFromClient: a client message holding a NUL would be two messages
// on the pipe; the connection is closed and the browser killed instead.
func TestNULFromClient(t *testing.T) {
	var seen syncBuffer
	f := &fakeLauncher{t: t, answer: func(in *bufio.Reader, _ io.Writer) {
		_, _ = io.Copy(&seen, in)
	}}
	_, srv, _ := start(t, f, nil)
	ws, err := dial(t, srv, testKey, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := websocket.Message.Send(ws, "{\"id\":1}\x00{\"id\":2}"); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := websocket.Message.Receive(ws, &got); err == nil {
		t.Fatal("connection open after a NUL")
	}
	eventually(t, "the browser killed", func() bool { return f.killed.Load() == 1 })
	if seen.String() != "" {
		t.Errorf("browser read %q", seen.String())
	}
}

// TestMaxLive: a third connection is refused while two are live, before
// any browser starts, and admitted once one ends.
func TestMaxLive(t *testing.T) {
	f := &fakeLauncher{t: t}
	s, srv, _ := start(t, f, nil)
	first, err := dial(t, srv, testKey, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dial(t, srv, testKey, false); err != nil {
		t.Fatal(err)
	}
	// A slot is taken before its browser launches, so wait for both
	// launches before asking whether the refused third added one.
	eventually(t, "two live", func() bool { return s.Live() == 2 && f.launched.Load() == 2 })
	if _, err := dial(t, srv, testKey, false); err == nil {
		t.Fatal("third connection admitted")
	}
	if n := f.launched.Load(); n != 2 {
		t.Errorf("%d browsers launched, want 2", n)
	}
	_ = first.Close()
	eventually(t, "a slot freed", func() bool { return s.Live() == 1 })
	if _, err := dial(t, srv, testKey, false); err != nil {
		t.Errorf("connection after a slot freed: %v", err)
	}
}

// TestIdle: a connection that carries nothing for Idle is closed and its
// browser killed; traffic keeps it open.
func TestIdle(t *testing.T) {
	f := &fakeLauncher{t: t}
	s, srv, _ := start(t, f, func(o *Options) { o.Idle = 300 * time.Millisecond })
	busy, err := dial(t, srv, testKey, false)
	if err != nil {
		t.Fatal(err)
	}
	quiet, err := dial(t, srv, testKey, false)
	if err != nil {
		t.Fatal(err)
	}
	stop := time.Now().Add(time.Second)
	for time.Now().Before(stop) {
		if err := websocket.Message.Send(busy, `{"id":1}`); err != nil {
			t.Fatalf("busy connection closed: %v", err)
		}
		var got string
		if err := websocket.Message.Receive(busy, &got); err != nil {
			t.Fatalf("busy connection closed: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	var got string
	if err := websocket.Message.Receive(quiet, &got); err == nil {
		t.Fatal("idle connection still open")
	}
	eventually(t, "the idle browser killed", func() bool { return f.killed.Load() == 1 && s.Live() == 1 })
}

// TestLaunchFailure: a browser that cannot start ends the connection and
// frees its slot.
func TestLaunchFailure(t *testing.T) {
	f := &fakeLauncher{t: t, fail: true}
	s, srv, logs := start(t, f, nil)
	ws, err := dial(t, srv, testKey, false)
	if err == nil {
		var got string
		if err := websocket.Message.Receive(ws, &got); err == nil {
			t.Fatal("connection open without a browser")
		}
	}
	eventually(t, "the slot freed", func() bool { return s.Live() == 0 })
	if !strings.Contains(logs.String(), "no chromium") {
		t.Errorf("launch error not logged: %s", logs.String())
	}
}

// TestClose kills every live browser and refuses new connections.
func TestClose(t *testing.T) {
	f := &fakeLauncher{t: t}
	s, srv, _ := start(t, f, nil)
	ws, err := dial(t, srv, testKey, false)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "one live", func() bool { return s.Live() == 1 })
	s.Close()
	var got string
	if err := websocket.Message.Receive(ws, &got); err == nil {
		t.Fatal("connection open after Close")
	}
	eventually(t, "the browser killed", func() bool { return f.killed.Load() == 1 })
	if _, err := dial(t, srv, testKey, false); err == nil {
		t.Error("connection admitted after Close")
	}
}

// TestNew: the options are checked, and the defaults are SP-1's limits.
func TestNew(t *testing.T) {
	f := &fakeLauncher{t: t}
	if _, err := New(Options{Key: testKey[:MinKeyBytes-1], Launch: f.Launch}); err == nil {
		t.Error("short key accepted")
	}
	if _, err := New(Options{Key: testKey}); err == nil {
		t.Error("no launcher accepted")
	}
	if _, err := New(Options{Key: testKey, Launch: f.Launch, MaxLive: -1}); err == nil {
		t.Error("negative MaxLive accepted")
	}
	s, err := New(Options{Key: testKey, Launch: f.Launch})
	if err != nil {
		t.Fatal(err)
	}
	if s.maxLive != DefaultMaxLive || s.idle != DefaultIdle {
		t.Errorf("defaults %d %v", s.maxLive, s.idle)
	}
	if DefaultMaxLive != 2 || DefaultIdle != 10*time.Minute {
		t.Errorf("SP-1 limits changed: %d %v", DefaultMaxLive, DefaultIdle)
	}
}

// TestReadKey: surrounding whitespace is dropped and a short key refused.
func TestReadKey(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	got, err := ReadKey(write("ok", "  "+string(testKey)+"\n"))
	if err != nil || !bytes.Equal(got, testKey) {
		t.Errorf("ReadKey = %q, %v", got, err)
	}
	if _, err := ReadKey(write("short", "abc\n")); err == nil {
		t.Error("short key accepted")
	}
	if _, err := ReadKey(filepath.Join(dir, "missing")); err == nil {
		t.Error("missing file accepted")
	}
}
