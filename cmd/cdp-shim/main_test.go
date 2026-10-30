// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/devthenet-labs/maul/internal/cdpshim"
)

// lockedBuffer is a stderr the server goroutines may write concurrently.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func keyFile(t *testing.T, key string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(p, []byte(key), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestRunRefuses: a bad flag, a missing or short key and an address it
// cannot listen on stop the shim before it serves.
func TestRunRefuses(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = busy.Close() }()
	good := keyFile(t, strings.Repeat("k", 44))
	for _, tc := range []struct {
		name string
		args []string
		want int
	}{
		{"bad flag", []string{"--nope"}, 2},
		{"no key file", []string{"--addr", freeAddr(t)}, 2},
		{"missing key file", []string{"--key-file", filepath.Join(t.TempDir(), "none")}, 2},
		{"short key", []string{"--key-file", keyFile(t, "short\n")}, 2},
		{"bad max-live", []string{"--key-file", good, "--max-live", "-1"}, 2},
		{"address in use", []string{"--key-file", good, "--addr", busy.Addr().String()}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stderr lockedBuffer
			if got := run(context.Background(), tc.args, &stderr); got != tc.want {
				t.Errorf("exit %d, want %d; stderr %s", got, tc.want, stderr.String())
			}
		})
	}
}

// TestRun: the binary serves the keyed endpoints and exits 0 when told to
// stop. The bridge itself is internal/cdpshim's to test.
func TestRun(t *testing.T) {
	key := strings.Repeat("s", 44)
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stderr lockedBuffer
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{"--addr", addr, "--key-file", keyFile(t, key+"\n"), "--tmp", t.TempDir()}, &stderr)
	}()

	get := func(withKey bool) (int, error) {
		req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/json/version", nil)
		if err != nil {
			t.Fatal(err)
		}
		if withKey {
			req.Header.Set(cdpshim.KeyHeader, key)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, err
		}
		_ = resp.Body.Close()
		return resp.StatusCode, nil
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		status, err := get(true)
		if err == nil {
			if status != http.StatusOK {
				t.Fatalf("status %d", status)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never served: %v; stderr %s", err, stderr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if status, err := get(false); err != nil || status != http.StatusUnauthorized {
		t.Errorf("without the key: %d, %v", status, err)
	}

	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit %d; stderr %s", code, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not stop")
	}
	if strings.Contains(stderr.String(), key) {
		t.Error("the key reached the log")
	}
}
