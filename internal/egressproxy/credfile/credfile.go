// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package credfile

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bitwise-media-group/patchy/internal/egressproxy"
	"github.com/bitwise-media-group/patchy/internal/netprofile"
)

// The poll interval's default and floor.
const (
	DefaultInterval = 30 * time.Second
	MinInterval     = 5 * time.Second
)

// The file keys of each credential type.
const (
	KeyToken    = "token"
	KeyUsername = "username"
	KeyPassword = "password"
)

// The refresh results OnRefresh reports, the refreshes metric's result
// attribute.
const (
	ResultLoaded   = "loaded"
	ResultReloaded = "reloaded"
	ResultInvalid  = "invalid"
)

// Registry is one file-backed registry: its name and its auth type
// (netprofile.AuthBearer or netprofile.AuthBasic).
type Registry struct {
	Name string
	Type string
}

// keys are the files the registry's credential is read from.
func (r Registry) keys() []string {
	if r.Type == netprofile.AuthBasic {
		return []string{KeyUsername, KeyPassword}
	}
	return []string{KeyToken}
}

// Options tune a Source.
type Options struct {
	// Interval is the poll interval; 0 is DefaultInterval, and less than
	// MinInterval is refused.
	Interval time.Duration
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// Log receives the lifecycle lines; nil discards them.
	Log *slog.Logger
	// OnRefresh is called once per loaded, reloaded or invalid outcome
	// (the refreshes metric); nil is no-op.
	OnRefresh func(registry, result string)
}

// state is one registry's current credential and what the poller saw.
type state struct {
	reg Registry
	// cred is the last good credential; ok reports there is one.
	cred egressproxy.Credential
	ok   bool
	// seen is the fingerprint of the files the last good value came from.
	seen [sha256.Size]byte
	// lastBad is the fingerprint and reason of the last invalid outcome
	// logged, so one bad file is logged once, not on every poll.
	lastBad string
	// forced is when Invalidate last re-read the registry.
	forced time.Time
}

// Source is the file-backed CredentialSource.
type Source struct {
	dir  string
	opts Options

	// reload serialises file reads; mu guards the states.
	reload sync.Mutex
	mu     sync.RWMutex
	states map[string]*state

	// tick replaces the poll ticker in tests.
	tick <-chan time.Time
}

var _ egressproxy.CredentialSource = (*Source)(nil)

// New builds a Source over dir for the registries and reads each one
// once. A registry whose files are not there yet is not an error (its
// route answers 503 until they are); a file that exists and cannot be
// read is, as are a bad name, type or interval.
func New(dir string, regs []Registry, opts Options) (*Source, error) {
	switch {
	case opts.Interval == 0:
		opts.Interval = DefaultInterval
	case opts.Interval < MinInterval:
		return nil, fmt.Errorf("credential poll interval %s is under %s", opts.Interval, MinInterval)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	if opts.OnRefresh == nil {
		opts.OnRefresh = func(string, string) {}
	}
	s := &Source{dir: dir, opts: opts, states: map[string]*state{}}
	for _, r := range regs {
		if err := netprofile.ValidName(r.Name); err != nil {
			return nil, fmt.Errorf("credential registry: %w", err)
		}
		if r.Type != netprofile.AuthBearer && r.Type != netprofile.AuthBasic {
			return nil, fmt.Errorf("registry %s: auth type %q is not file-backed (bearer or basic)", r.Name, r.Type)
		}
		if _, dup := s.states[r.Name]; dup {
			return nil, fmt.Errorf("registry %s is listed twice", r.Name)
		}
		s.states[r.Name] = &state{reg: r}
	}
	for name := range s.states {
		if err := s.refresh(name); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Get returns the registry's last good credential, or
// ErrCredentialUnavailable when none has loaded (or the registry is not
// file-backed).
func (s *Source) Get(registry string) (egressproxy.Credential, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st, ok := s.states[registry]
	if !ok || !st.ok {
		return egressproxy.Credential{}, fmt.Errorf("%w: registry %s has no loaded credential",
			egressproxy.ErrCredentialUnavailable, registry)
	}
	return st.cred, nil
}

// Invalidate re-reads the registry's files at once, after the upstream
// answered 401: at most once per poll interval per registry, so a run of
// 401s cannot turn into a run of file reads.
func (s *Source) Invalidate(registry string) {
	s.mu.Lock()
	st, ok := s.states[registry]
	now := s.opts.Now()
	if !ok || (!st.forced.IsZero() && now.Sub(st.forced) < s.opts.Interval) {
		s.mu.Unlock()
		return
	}
	st.forced = now
	s.mu.Unlock()
	// A read error is logged by refresh and keeps the last good value.
	_ = s.refresh(registry)
}

// Poll re-reads every registry once.
func (s *Source) Poll() {
	s.mu.RLock()
	names := make([]string, 0, len(s.states))
	for n := range s.states {
		names = append(names, n)
	}
	s.mu.RUnlock()
	for _, n := range names {
		_ = s.refresh(n)
	}
}

// Run polls on the interval until ctx ends.
func (s *Source) Run(ctx context.Context) error {
	tick := s.tick
	if tick == nil {
		t := time.NewTicker(s.opts.Interval)
		defer t.Stop()
		tick = t.C
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick:
			s.Poll()
		}
	}
}

// refresh reads one registry's files and applies the outcome. It returns
// an error only for a file that exists and cannot be read, which New
// treats as fatal; afterwards the poller logs it and keeps the last good
// value.
func (s *Source) refresh(name string) error {
	s.reload.Lock()
	defer s.reload.Unlock()
	s.mu.RLock()
	st := s.states[name]
	reg := st.reg
	s.mu.RUnlock()

	values, sum, reason, readErr := s.read(reg)
	if reason == "" {
		var cred egressproxy.Credential
		var err error
		if reg.Type == netprofile.AuthBasic {
			cred, err = egressproxy.BasicCredential(values[KeyUsername], values[KeyPassword])
		} else {
			cred, err = egressproxy.BearerCredential(values[KeyToken])
		}
		if err != nil {
			reason = err.Error()
		} else {
			s.apply(st, cred, sum)
			return nil
		}
	}
	s.invalid(st, sum, reason)
	return readErr
}

// apply swaps a good credential in when the files changed.
func (s *Source) apply(st *state, cred egressproxy.Credential, sum [sha256.Size]byte) {
	s.mu.Lock()
	first, changed := !st.ok, sum != st.seen
	if changed || first {
		st.cred, st.ok, st.seen = cred, true, sum
	}
	st.lastBad = ""
	s.mu.Unlock()
	switch {
	case first:
		s.opts.Log.Info("credential_loaded", "registry", st.reg.Name, "credential", "static")
		s.opts.OnRefresh(st.reg.Name, ResultLoaded)
	case changed:
		s.opts.Log.Info("credential_reloaded", "registry", st.reg.Name, "credential", "static")
		s.opts.OnRefresh(st.reg.Name, ResultReloaded)
	}
}

// invalid logs a bad outcome once per distinct (files, reason) and keeps
// the last good value.
func (s *Source) invalid(st *state, sum [sha256.Size]byte, reason string) {
	key := fmt.Sprintf("%x %s", sum, reason)
	s.mu.Lock()
	repeat := st.lastBad == key
	st.lastBad = key
	kept := st.ok
	s.mu.Unlock()
	if repeat {
		return
	}
	s.opts.Log.Warn("credential_invalid", "registry", st.reg.Name, "credential", "static", "reason", reason,
		"kept_previous", kept)
	s.opts.OnRefresh(st.reg.Name, ResultInvalid)
}

// read loads the registry's keys. A missing key is a reason, not an error;
// an unreadable one is both. The fingerprint covers every key's bytes (as
// read, before the one trailing newline is dropped), so any change to any
// file is a change.
func (s *Source) read(reg Registry) (map[string]string, [sha256.Size]byte, string, error) {
	h := sha256.New()
	values := map[string]string{}
	for _, k := range reg.keys() {
		path := filepath.Join(s.dir, reg.Name, k)
		raw, err := readCapped(path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return nil, [sha256.Size]byte{}, "missing key " + k, nil
		case errors.Is(err, errTooLong):
			return nil, [sha256.Size]byte{}, fmt.Sprintf("key %s is over %d bytes", k, egressproxy.MaxSecretBytes), nil
		case err != nil:
			return nil, [sha256.Size]byte{}, "unreadable key " + k,
				fmt.Errorf("registry %s: read credential key %s: %w", reg.Name, k, err)
		}
		_, _ = fmt.Fprintf(h, "%s\x00%d\x00", k, len(raw))
		_, _ = h.Write(raw)
		values[k] = trimNewline(string(raw))
	}
	var sum [sha256.Size]byte
	copy(sum[:], h.Sum(nil))
	return values, sum, "", nil
}

// errTooLong is a key file over the value limit (plus a newline).
var errTooLong = errors.New("credential file too long")

// readCapped reads at most MaxSecretBytes plus a CRLF.
func readCapped(path string) ([]byte, error) {
	f, err := os.Open(path) // <dir>/<validated name>/<fixed key>
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	limit := int64(egressproxy.MaxSecretBytes + 2)
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, errTooLong
	}
	return raw, nil
}

// trimNewline drops exactly one trailing "\n" or "\r\n", which a Secret
// created from a file usually ends with. Any other control character,
// including a second newline, stays and is refused by the validator.
func trimNewline(v string) string {
	if t, ok := strings.CutSuffix(v, "\r\n"); ok {
		return t
	}
	return strings.TrimSuffix(v, "\n")
}
