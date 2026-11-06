// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package prepare

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"regexp"
	"time"
)

// ErrDigest is a tarball whose bytes are not its digest's: refused at
// once, never retried, never unpacked.
var ErrDigest = errors.New("prepare: tarball does not match its digest")

// DefaultAttempts is how many times a fetch is tried before it fails, as
// the agent Jobs' `curl --retry 5` does.
const DefaultAttempts = 6

// MaxTarballBytes bounds one downloaded tarball, within the prepare init's
// /tmp volume (internal/jobs: 512Mi, one tarball at a time).
const MaxTarballBytes = 480 << 20

// maxBackoff caps the wait between attempts.
const maxBackoff = 16 * time.Second

// digestPattern is a sha256 digest as the controllers write it.
var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidDigest reports whether s is a sha256 digest: 64 lower-case hex.
func ValidDigest(s string) bool { return digestPattern.MatchString(s) }

// Fetcher downloads tarballs from the artifact server. The zero value of
// every field but Client takes its default.
type Fetcher struct {
	// Client makes the requests (NewClient).
	Client *http.Client
	// Attempts is the most tries per tarball, DefaultAttempts when 0.
	Attempts int
	// Limit bounds a tarball's bytes, MaxTarballBytes when 0.
	Limit int64
	// Sleep waits between attempts; a timer bound to ctx when nil.
	Sleep func(ctx context.Context, d time.Duration) error
}

// NewClient is the prepare init's HTTP client: no proxy from the
// environment and no redirect followed (the artifact server never sends
// one), so it reaches exactly the URL it was given.
func NewClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	return &http.Client{
		Transport:     tr,
		Timeout:       10 * time.Minute,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Tree fetches the tarball at url, verifies it against digest and only
// then extracts it into dst (Extract, MaxTreeBytes), its download held in
// an unlinked file under tmpDir.
func (f Fetcher) Tree(ctx context.Context, url, digest, tmpDir, dst string) error {
	file, err := f.Fetch(ctx, url, digest, tmpDir)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	return Extract(file, dst, MaxTreeBytes)
}

// Fetch downloads url into a file under tmpDir, unlinked at once so that
// nothing is ever left behind, and returns it open at its start once its
// sha256 is digest. A transient failure (a refused connection, a status
// other than 200, a cut-off body) is retried up to Attempts times; a
// digest mismatch or an oversized tarball is not.
func (f Fetcher) Fetch(ctx context.Context, url, digest, tmpDir string) (*os.File, error) {
	if !ValidDigest(digest) {
		return nil, refuse("digest %q is not 64 lower-case hex characters", digest)
	}
	attempts := f.Attempts
	if attempts <= 0 {
		attempts = DefaultAttempts
	}
	sleep := f.Sleep
	if sleep == nil {
		sleep = sleepContext
	}
	for attempt := 1; ; attempt++ {
		file, err := f.once(ctx, url, digest, tmpDir)
		if err == nil {
			return file, nil
		}
		if errors.Is(err, ErrDigest) || errors.Is(err, ErrRefused) || ctx.Err() != nil {
			return nil, err
		}
		if attempt >= attempts {
			return nil, fmt.Errorf("fetch %s: gave up after %d attempts: %w", url, attempts, err)
		}
		if err := sleep(ctx, backoff(attempt)); err != nil {
			return nil, err
		}
	}
}

// once is one attempt.
func (f Fetcher) once(ctx context.Context, url, digest, tmpDir string) (*os.File, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = MaxTarballBytes
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, refuse("tarball URL %q: %v", url, err)
	}
	resp, err := f.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: status %d", url, resp.StatusCode)
	}
	if resp.ContentLength > limit {
		return nil, refuse("tarball %s is %d bytes, over the %d-byte limit", url, resp.ContentLength, limit)
	}
	file, err := os.CreateTemp(tmpDir, "tree-*.tar.gz")
	if err != nil {
		return nil, err
	}
	if err := os.Remove(file.Name()); err != nil {
		_ = file.Close()
		return nil, err
	}
	h := sha256.New()
	if err := download(file, h, resp.Body, url, limit); err != nil {
		_ = file.Close()
		return nil, err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != digest {
		_ = file.Close()
		return nil, fmt.Errorf("%w: %s has sha256 %s, want %s", ErrDigest, url, got, digest)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

// download copies body into file and h, at most limit bytes.
func download(file *os.File, h hash.Hash, body io.Reader, url string, limit int64) error {
	n, err := io.Copy(io.MultiWriter(file, h), io.LimitReader(body, limit+1))
	if err != nil {
		return fmt.Errorf("fetch %s: %w", url, err)
	}
	if n > limit {
		return refuse("tarball %s is over the %d-byte limit", url, limit)
	}
	return nil
}

// backoff is the wait after attempt: 1s, doubling, capped.
func backoff(attempt int) time.Duration {
	d := time.Second << min(attempt-1, 4)
	return min(d, maxBackoff)
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
