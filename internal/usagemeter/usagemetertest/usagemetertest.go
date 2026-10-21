// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package usagemetertest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/devthenet-labs/maul/internal/usagemeter"
	"github.com/devthenet-labs/maul/internal/usagewire"
)

// Token is the bearer the fake broker accepts.
const Token = "controller-usage-token"

// Broker is a fake usage endpoint: it answers each Job as Set says and
// records the Jobs asked for.
type Broker struct {
	mu      sync.Mutex
	answers map[string]usagewire.Response
	asked   []string
	// fallback answers a Job Set never named; nil answers no entry.
	fallback func(job string) usagewire.Response
}

// SetDefault makes the broker answer every Job Set never named with
// answer(job).
func (b *Broker) SetDefault(answer func(job string) usagewire.Response) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fallback = answer
}

// Set makes the broker answer job with resp.
func (b *Broker) Set(job string, resp usagewire.Response) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.answers[job] = resp
}

// Asked is the Jobs asked for, in order.
func (b *Broker) Asked() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.asked...)
}

// New starts a fake broker and returns it with a Meter reading it through
// usagemeter.Client, on the given clock (nil: time.Now).
func New(t *testing.T, now func() time.Time) (*Broker, *usagemeter.Meter) {
	t.Helper()
	b := &Broker{answers: map[string]usagewire.Response{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+Token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		job, ok := strings.CutPrefix(r.URL.Path, usagewire.PathPrefix)
		if !ok || r.Method != http.MethodGet {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		b.mu.Lock()
		b.asked = append(b.asked, job)
		resp, ok := b.answers[job]
		fallback := b.fallback
		b.mu.Unlock()
		switch {
		case ok:
		case fallback != nil:
			resp = fallback(job)
		default:
			resp = usagewire.Response{BootTime: time.Unix(0, 0).UTC(), Pods: []usagewire.Pod{}}
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(Token), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := usagemeter.NewClient(srv.URL, tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	return b, &usagemeter.Meter{Fetcher: c, Now: now}
}

// Answer is a broker answer for one Job's pod: booted at boot, the pod
// first seen at seen, with inflight requests and the models' usage.
func Answer(job string, boot, seen time.Time, inflight int64, models ...usagewire.Model) usagewire.Response {
	return usagewire.Response{BootTime: boot.UTC(), Pods: []usagewire.Pod{{
		Name: job + "-x7k2p", UID: "pod-uid", FirstSeen: seen.UTC(), Inflight: inflight, Models: models,
	}}}
}

// Sonnet is a usage of claude-sonnet-5 on an API key: a million input
// tokens ($3.00) and the given shortfall.
func Sonnet(shortfall int64) usagewire.Model {
	return usagewire.Model{Model: "claude-sonnet-5-20260514", Route: "anthropic",
		Credential: usagewire.CredentialAPIKey, Input: 1_000_000, EstimatedShortfall: shortfall}
}
