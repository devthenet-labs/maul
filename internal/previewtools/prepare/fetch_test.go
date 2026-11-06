// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package prepare

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func digestOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// server serves body after failing the first fails requests with status.
func server(t *testing.T, body []byte, fails int, status int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if int(n.Add(1)) <= fails {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

// fetcher retries without waiting.
func fetcher(srv *httptest.Server) Fetcher {
	return Fetcher{Client: srv.Client(), Sleep: func(context.Context, time.Duration) error { return nil }}
}

func TestFetchVerified(t *testing.T) {
	body := tarball(t, githubEntries()...)
	srv, n := server(t, body, 0, 0)
	tmp := t.TempDir()
	f, err := fetcher(srv).Fetch(context.Background(), srv.URL+"/a.tar.gz", digestOf(body), tmp)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	got, err := io.ReadAll(f)
	if err != nil || string(got) != string(body) {
		t.Fatalf("fetched %d bytes, %v; want the body from the start", len(got), err)
	}
	if n.Load() != 1 {
		t.Errorf("%d requests, want 1", n.Load())
	}
}

// TestFetchTampered: bytes that are not the digest's are refused at once,
// never retried and never left behind.
func TestFetchTampered(t *testing.T) {
	body := tarball(t, githubEntries()...)
	srv, n := server(t, body, 0, 0)
	tmp := t.TempDir()
	_, err := fetcher(srv).Fetch(context.Background(), srv.URL, digestOf(append([]byte("x"), body...)), tmp)
	if !errors.Is(err, ErrDigest) {
		t.Fatalf("Fetch = %v, want ErrDigest", err)
	}
	if n.Load() != 1 {
		t.Errorf("%d requests, want 1: a digest mismatch is not retried", n.Load())
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Errorf("left %d files behind", len(left))
	}
}

func TestFetchRetries(t *testing.T) {
	body := []byte("tarball")
	srv, n := server(t, body, 3, http.StatusServiceUnavailable)
	f, err := fetcher(srv).Fetch(context.Background(), srv.URL, digestOf(body), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if n.Load() != 4 {
		t.Errorf("%d requests, want 4", n.Load())
	}
}

func TestFetchGivesUp(t *testing.T) {
	body := []byte("tarball")
	srv, n := server(t, body, 100, http.StatusNotFound)
	tmp := t.TempDir()
	_, err := fetcher(srv).Fetch(context.Background(), srv.URL, digestOf(body), tmp)
	if err == nil || errors.Is(err, ErrDigest) {
		t.Fatalf("Fetch = %v, want the last status", err)
	}
	if n.Load() != DefaultAttempts {
		t.Errorf("%d requests, want %d", n.Load(), DefaultAttempts)
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Errorf("left %d files behind", len(left))
	}
}

func TestFetchLimit(t *testing.T) {
	body := []byte("0123456789")
	srv, n := server(t, body, 0, 0)
	f := fetcher(srv)
	f.Limit = 9
	_, err := f.Fetch(context.Background(), srv.URL, digestOf(body), t.TempDir())
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("Fetch = %v, want ErrRefused over the limit", err)
	}
	if n.Load() != 1 {
		t.Errorf("%d requests, want 1: an oversized tarball is not retried", n.Load())
	}
}

func TestFetchCancelled(t *testing.T) {
	srv, _ := server(t, nil, 100, http.StatusBadGateway)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := Fetcher{Client: srv.Client()}
	if _, err := f.Fetch(ctx, srv.URL, digestOf(nil), t.TempDir()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Fetch = %v, want context.Canceled", err)
	}
}

// TestTree: fetch, verify and extract one side, the tarball removed.
func TestTree(t *testing.T) {
	body := tarball(t, githubEntries()...)
	srv, _ := server(t, body, 0, 0)
	tmp, dst := t.TempDir(), t.TempDir()
	if err := fetcher(srv).Tree(context.Background(), srv.URL, digestOf(body), tmp, dst); err != nil {
		t.Fatal(err)
	}
	sameListing(t, listing(t, dst), githubListing)
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Errorf("left %d files behind", len(left))
	}
}

// TestTreeTamperedExtractsNothing: unverified bytes are never unpacked.
func TestTreeTamperedExtractsNothing(t *testing.T) {
	body := tarball(t, githubEntries()...)
	srv, _ := server(t, body, 0, 0)
	dst := t.TempDir()
	err := fetcher(srv).Tree(context.Background(), srv.URL, digestOf([]byte("other")), t.TempDir(), dst)
	if !errors.Is(err, ErrDigest) {
		t.Fatalf("Tree = %v, want ErrDigest", err)
	}
	if got := listing(t, dst); len(got) != 0 {
		t.Errorf("extracted %v from an unverified tarball", got)
	}
}
