// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package cdpshim

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"golang.org/x/net/websocket"
)

// KeyHeader is the request header that carries the per-evaluation key
// (the session config's browser.cdpHeaders, SP-1).
const KeyHeader = "X-Maul-Cdp-Key"

// MinKeyBytes is the shortest key the shim accepts: the key is 256 bits,
// so its text form is at least 32 bytes.
const MinKeyBytes = 32

// The limits SP-1 settled on.
const (
	// DefaultMaxLive browsers at once: the CLI session and one check.
	DefaultMaxLive = 2
	// DefaultIdle a connection may carry nothing before its browser is
	// killed: long enough for the agent to think between CLI calls; the
	// round's deadline bounds it anyway.
	DefaultIdle = 10 * time.Minute
)

// webSocketPath is the one WebSocket path /json/version names.
const webSocketPath = "/devtools/browser/shim"

// Options configures a Shim.
type Options struct {
	// Key is the per-evaluation key a caller must present.
	Key []byte
	// Launch starts one browser per accepted connection.
	Launch Launcher
	// MaxLive bounds the live browsers (DefaultMaxLive when 0).
	MaxLive int
	// Idle kills a connection's browser after this long without a message
	// either way (DefaultIdle when 0).
	Idle time.Duration
	// Log receives the shim's lines (slog.Default when nil).
	Log *slog.Logger
}

// Shim serves CDP to keyed callers, one browser per connection.
type Shim struct {
	key     []byte
	launch  Launcher
	maxLive int
	idle    time.Duration
	log     *slog.Logger

	mu     sync.Mutex
	live   map[*session]struct{}
	slots  int
	closed bool
}

// New checks o and returns a Shim.
func New(o Options) (*Shim, error) {
	if len(o.Key) < MinKeyBytes {
		return nil, fmt.Errorf("cdpshim: key of %d bytes, need at least %d", len(o.Key), MinKeyBytes)
	}
	if o.Launch == nil {
		return nil, errors.New("cdpshim: no launcher")
	}
	if o.MaxLive < 0 || o.Idle < 0 {
		return nil, errors.New("cdpshim: negative limit")
	}
	s := &Shim{
		key:     bytes.Clone(o.Key),
		launch:  o.Launch,
		maxLive: o.MaxLive,
		idle:    o.Idle,
		log:     o.Log,
		live:    map[*session]struct{}{},
	}
	if s.maxLive == 0 {
		s.maxLive = DefaultMaxLive
	}
	if s.idle == 0 {
		s.idle = DefaultIdle
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	return s, nil
}

// ReadKey reads the key from a mounted file, without surrounding
// whitespace.
func ReadKey(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cdpshim: key file: %w", err)
	}
	key := bytes.TrimSpace(raw)
	if len(key) < MinKeyBytes {
		return nil, fmt.Errorf("cdpshim: key of %d bytes, need at least %d", len(key), MinKeyBytes)
	}
	return key, nil
}

// Handler serves /json/version and the WebSocket. The gate runs first, for
// every path: the mux's own answers (a 307 canonicalising a path, a 404)
// would otherwise tell a caller without the key what routes exist.
func (s *Shim) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/json/version", s.version)
	mux.HandleFunc("/json/version/", s.version)
	mux.HandleFunc("/devtools/browser/", s.devtools)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.gate(w, r) {
			mux.ServeHTTP(w, r)
		}
	})
}

// Live is the number of connections holding a browser slot.
func (s *Shim) Live() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.slots
}

// Close kills every live browser and refuses new connections.
func (s *Shim) Close() {
	s.mu.Lock()
	s.closed = true
	live := make([]*session, 0, len(s.live))
	for ss := range s.live {
		live = append(live, ss)
	}
	s.mu.Unlock()
	for _, ss := range live {
		ss.stop("shim closing")
	}
}

// gate refuses a request with any Origin header or without the key, and
// any method but GET. Handler runs it before the mux and the upgrade.
func (s *Shim) gate(w http.ResponseWriter, r *http.Request) bool {
	if _, ok := r.Header["Origin"]; ok {
		s.refuse(w, r, http.StatusForbidden, "origin")
		return false
	}
	got := []byte(r.Header.Get(KeyHeader))
	if subtle.ConstantTimeCompare(got, s.key) != 1 {
		s.refuse(w, r, http.StatusUnauthorized, "key")
		return false
	}
	if r.Method != http.MethodGet {
		s.refuse(w, r, http.StatusMethodNotAllowed, "method")
		return false
	}
	return true
}

// refuse answers status and logs the reason, never a header's value.
func (s *Shim) refuse(w http.ResponseWriter, r *http.Request, status int, reason string) {
	s.log.Warn("refused", "reason", reason, "path", r.URL.Path, "remote", r.RemoteAddr)
	http.Error(w, http.StatusText(status), status)
}

func (s *Shim) version(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"Browser":              "maul-cdp-shim",
		"Protocol-Version":     "1.3",
		"webSocketDebuggerUrl": "ws://" + r.Host + webSocketPath,
	})
}

func (s *Shim) devtools(w http.ResponseWriter, r *http.Request) {
	if !s.acquire() {
		s.refuse(w, r, http.StatusServiceUnavailable, "busy")
		return
	}
	defer s.release()
	websocket.Server{
		// The gate refused every Origin already; x/net's default check
		// would refuse the callers that send none.
		Handshake: func(*websocket.Config, *http.Request) error { return nil },
		Handler:   s.bridge,
	}.ServeHTTP(w, r)
}

func (s *Shim) acquire() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.slots >= s.maxLive {
		return false
	}
	s.slots++
	return true
}

func (s *Shim) release() {
	s.mu.Lock()
	s.slots--
	s.mu.Unlock()
}

// session is one connection and its browser.
type session struct {
	ws       *websocket.Conn
	browser  Browser
	log      *slog.Logger
	once     sync.Once
	activity chan struct{}
	done     chan struct{}
}

// stop kills the browser and closes the connection, once.
func (ss *session) stop(why string) {
	ss.once.Do(func() {
		ss.log.Info("closing", "why", why)
		ss.browser.Kill()
		_ = ss.ws.Close()
	})
}

// poke records traffic for the idle watchdog.
func (ss *session) poke() {
	select {
	case ss.activity <- struct{}{}:
	default:
	}
}

// bridge runs one browser for the connection and copies messages both
// ways until either side ends or the connection idles.
func (s *Shim) bridge(ws *websocket.Conn) {
	ws.MaxPayloadBytes = MaxMessageBytes
	b, err := s.launch()
	if err != nil {
		s.log.Error("cannot start browser", "err", err)
		_ = ws.Close()
		return
	}
	ss := &session{ws: ws, browser: b, log: s.log, activity: make(chan struct{}, 1), done: make(chan struct{})}
	if !s.track(ss) {
		ss.stop("shim closing")
		_ = b.Wait()
		return
	}
	defer s.untrack(ss)
	s.log.Info("browser started")
	go s.watchIdle(ss)
	fromBrowser := make(chan struct{})
	go func() {
		defer close(fromBrowser)
		for {
			msg, err := b.Recv()
			if err != nil {
				ss.stop("browser closed")
				return
			}
			ss.poke()
			if err := websocket.Message.Send(ws, string(msg)); err != nil {
				ss.stop("client write failed")
				return
			}
		}
	}()
	for {
		var msg []byte
		if err := websocket.Message.Receive(ws, &msg); err != nil {
			ss.stop("client closed")
			break
		}
		ss.poke()
		if err := b.Send(msg); err != nil {
			if errors.Is(err, ErrNUL) {
				ss.stop("client sent a NUL")
			} else {
				ss.stop("browser write failed")
			}
			break
		}
	}
	<-fromBrowser
	close(ss.done)
	err = b.Wait()
	s.log.Info("browser exited", "err", err)
}

// watchIdle stops ss after s.idle without traffic.
func (s *Shim) watchIdle(ss *session) {
	t := time.NewTimer(s.idle)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			ss.stop("idle")
			return
		case <-ss.activity:
			if !t.Stop() {
				select {
				case <-t.C:
				default:
				}
			}
			t.Reset(s.idle)
		case <-ss.done:
			return
		}
	}
}

// track registers ss for Close; false once the shim is closing.
func (s *Shim) track(ss *session) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.live[ss] = struct{}{}
	return true
}

func (s *Shim) untrack(ss *session) {
	s.mu.Lock()
	delete(s.live, ss)
	s.mu.Unlock()
}
