// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package credfile

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bitwise-media-group/patchy/internal/egressproxy"
	"github.com/bitwise-media-group/patchy/internal/netprofile"
)

const sentinel = "s3cr3t-TOKEN-1234"

// clock is a settable fake clock.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// recorder collects the lifecycle lines and refresh outcomes.
type recorder struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	results []string
}

func (r *recorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.Write(p)
}

func (r *recorder) onRefresh(reg, result string) {
	r.mu.Lock()
	r.results = append(r.results, reg+"="+result)
	r.mu.Unlock()
}

func (r *recorder) log() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.String()
}

func (r *recorder) count(event string) int { return strings.Count(r.log(), "msg="+event) }

func (r *recorder) outcomes() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.results, ",")
}

// projected lays a registry's keys out the way kubelet mounts a projected
// volume: the data in a timestamped directory, "..data" a symlink to it,
// and each registry directory a symlink through "..data". Writing a new
// generation and renaming a fresh "..data" link over the old one is the
// atomic swap kubelet makes on a Secret update.
type projected struct {
	t   *testing.T
	dir string
	gen int
}

func newProjected(t *testing.T) *projected {
	t.Helper()
	return &projected{t: t, dir: t.TempDir()}
}

// write publishes a new generation holding files (registry/key -> value).
func (p *projected) write(files map[string]string) {
	p.t.Helper()
	p.gen++
	genDir := fmt.Sprintf("..2026_10_15_%02d", p.gen)
	regs := map[string]bool{}
	for path, v := range files {
		full := filepath.Join(p.dir, genDir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			p.t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(v), 0o440); err != nil {
			p.t.Fatal(err)
		}
		regs[strings.Split(path, "/")[0]] = true
	}
	if err := os.MkdirAll(filepath.Join(p.dir, genDir), 0o755); err != nil {
		p.t.Fatal(err)
	}
	tmp := filepath.Join(p.dir, "..data_tmp")
	if err := os.Symlink(genDir, tmp); err != nil {
		p.t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(p.dir, "..data")); err != nil {
		p.t.Fatal(err)
	}
	for reg := range regs {
		link := filepath.Join(p.dir, reg)
		if _, err := os.Lstat(link); err == nil {
			continue
		}
		if err := os.Symlink(filepath.Join("..data", reg), link); err != nil {
			p.t.Fatal(err)
		}
	}
}

func newSource(t *testing.T, dir string, regs []Registry, clk *clock, rec *recorder) *Source {
	t.Helper()
	s, err := New(dir, regs, Options{
		Interval: 30 * time.Second, Now: clk.Now, Log: slog.New(slog.NewTextHandler(rec, nil)),
		OnRefresh: rec.onRefresh,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func header(t *testing.T, s *Source, reg string) string {
	t.Helper()
	c, err := s.Get(reg)
	if err != nil {
		t.Fatalf("Get(%s): %v", reg, err)
	}
	return c.Authorization()
}

func basicHeader(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

func TestLoadAndKubeletSwap(t *testing.T) {
	p := newProjected(t)
	p.write(map[string]string{
		"art-npm/token":         sentinel + "\n", // a Secret made from a file ends in a newline
		"nexus/username":        "svc",
		"nexus/password":        sentinel + "-pw\r\n",
		"other-registry/token":  "not ours",
		"art-npm/unrelated-key": "ignored",
	})
	clk, rec := &clock{now: time.Unix(1_800_000_000, 0)}, &recorder{}
	regs := []Registry{{Name: "art-npm", Type: netprofile.AuthBearer}, {Name: "nexus", Type: netprofile.AuthBasic}}
	s := newSource(t, p.dir, regs, clk, rec)
	if got := header(t, s, "art-npm"); got != "Bearer "+sentinel {
		t.Fatal("bearer header differs from the file (newline not dropped?)")
	}
	if got := header(t, s, "nexus"); got != basicHeader("svc", sentinel+"-pw") {
		t.Fatal("basic header differs from the files")
	}
	if c, _ := s.Get("art-npm"); c.Kind() != egressproxy.CredentialStatic {
		t.Fatalf("kind = %s", c.Kind())
	}
	if rec.count("credential_loaded") != 2 {
		t.Fatalf("log:\n%s", rec.log())
	}
	// Nothing changed: a poll logs nothing.
	s.Poll()
	if rec.count("credential_reloaded") != 0 || rec.count("credential_invalid") != 0 {
		t.Fatalf("an unchanged poll logged:\n%s", rec.log())
	}
	// Kubelet swaps ..data to a new generation with a rotated token.
	p.write(map[string]string{"art-npm/token": sentinel + "-rotated", "nexus/username": "svc",
		"nexus/password": sentinel + "-pw\r\n"})
	s.Poll()
	if got := header(t, s, "art-npm"); got != "Bearer "+sentinel+"-rotated" {
		t.Fatal("the rotated token was not reloaded within one poll")
	}
	if got := header(t, s, "nexus"); got != basicHeader("svc", sentinel+"-pw") {
		t.Fatal("the unchanged registry changed")
	}
	if rec.count("credential_reloaded") != 1 {
		t.Fatalf("want one credential_reloaded:\n%s", rec.log())
	}
	if got := rec.outcomes(); !strings.Contains(got, "art-npm=reloaded") || strings.Contains(got, "nexus=reloaded") {
		t.Fatalf("outcomes = %s", got)
	}
	assertNoSecret(t, rec.log())
}

func TestInvalidKeepsLastGood(t *testing.T) {
	for name, bad := range map[string]map[string]string{
		"empty":        {"r/token": ""},
		"only newline": {"r/token": "\n"},
		"crlf inside":  {"r/token": "abc\r\nX-Evil: 1"},
		"two newlines": {"r/token": sentinel + "\n\n"},
		"nul":          {"r/token": "a\x00b"},
		"over-long":    {"r/token": strings.Repeat("a", egressproxy.MaxSecretBytes+1)},
		"far over":     {"r/token": strings.Repeat("a", 3*egressproxy.MaxSecretBytes)},
		"missing key":  {"r/other": "x"},
	} {
		t.Run(name, func(t *testing.T) {
			p := newProjected(t)
			p.write(map[string]string{"r/token": sentinel})
			clk, rec := &clock{now: time.Unix(1_800_000_000, 0)}, &recorder{}
			s := newSource(t, p.dir, []Registry{{Name: "r", Type: netprofile.AuthBearer}}, clk, rec)
			p.write(bad)
			s.Poll()
			s.Poll()
			if got := header(t, s, "r"); got != "Bearer "+sentinel {
				t.Fatal("an invalid file replaced the last good credential")
			}
			if n := rec.count("credential_invalid"); n != 1 {
				t.Fatalf("credential_invalid logged %d times over two polls, want once:\n%s", n, rec.log())
			}
			if !strings.Contains(rec.log(), "kept_previous=true") {
				t.Fatalf("log does not say the previous value was kept:\n%s", rec.log())
			}
			// Fixing the file is a reload.
			p.write(map[string]string{"r/token": sentinel + "-fixed"})
			s.Poll()
			if got := header(t, s, "r"); got != "Bearer "+sentinel+"-fixed" {
				t.Fatal("the fixed file was not loaded")
			}
			assertNoSecret(t, rec.log())
		})
	}
}

func TestBasicUsernameColonInvalid(t *testing.T) {
	p := newProjected(t)
	p.write(map[string]string{"r/username": "a:b", "r/password": sentinel})
	clk, rec := &clock{now: time.Unix(1_800_000_000, 0)}, &recorder{}
	s := newSource(t, p.dir, []Registry{{Name: "r", Type: netprofile.AuthBasic}}, clk, rec)
	if _, err := s.Get("r"); !errors.Is(err, egressproxy.ErrCredentialUnavailable) {
		t.Fatalf("Get = %v, want unavailable", err)
	}
	if !strings.Contains(rec.log(), "colon") {
		t.Fatalf("log:\n%s", rec.log())
	}
}

func TestStartupWithoutFile(t *testing.T) {
	p := newProjected(t)
	p.write(map[string]string{"present/token": sentinel})
	clk, rec := &clock{now: time.Unix(1_800_000_000, 0)}, &recorder{}
	regs := []Registry{{Name: "present", Type: netprofile.AuthBearer}, {Name: "absent", Type: netprofile.AuthBearer}}
	s := newSource(t, p.dir, regs, clk, rec)
	if _, err := s.Get("absent"); !errors.Is(err, egressproxy.ErrCredentialUnavailable) {
		t.Fatalf("Get(absent) = %v, want ErrCredentialUnavailable", err)
	}
	if header(t, s, "present") != "Bearer "+sentinel {
		t.Fatal("the other registry is affected")
	}
	if _, err := s.Get("never-configured"); !errors.Is(err, egressproxy.ErrCredentialUnavailable) {
		t.Fatal("an unknown registry has a credential")
	}
	if !strings.Contains(rec.log(), "missing key token") || !strings.Contains(rec.log(), "kept_previous=false") {
		t.Fatalf("log:\n%s", rec.log())
	}
	p.write(map[string]string{"present/token": sentinel, "absent/token": sentinel + "-late"})
	s.Poll()
	if header(t, s, "absent") != "Bearer "+sentinel+"-late" {
		t.Fatal("a file that appeared later was not loaded")
	}
}

func TestInvalidateRateLimited(t *testing.T) {
	p := newProjected(t)
	p.write(map[string]string{"r/token": "v1"})
	clk, rec := &clock{now: time.Unix(1_800_000_000, 0)}, &recorder{}
	s := newSource(t, p.dir, []Registry{{Name: "r", Type: netprofile.AuthBearer}}, clk, rec)

	p.write(map[string]string{"r/token": "v2"})
	s.Invalidate("r") // a 401: one immediate re-read
	if header(t, s, "r") != "Bearer v2" {
		t.Fatal("Invalidate did not re-read at once")
	}
	p.write(map[string]string{"r/token": "v3"})
	clk.Add(29 * time.Second)
	s.Invalidate("r") // within the interval: no read
	if header(t, s, "r") != "Bearer v2" {
		t.Fatal("a second Invalidate within the poll interval re-read")
	}
	clk.Add(2 * time.Second)
	s.Invalidate("r")
	if header(t, s, "r") != "Bearer v3" {
		t.Fatal("Invalidate after the interval did not re-read")
	}
	s.Invalidate("unknown") // harmless
}

func TestRunPollsOnTick(t *testing.T) {
	p := newProjected(t)
	p.write(map[string]string{"r/token": "v1"})
	clk, rec := &clock{now: time.Unix(1_800_000_000, 0)}, &recorder{}
	s := newSource(t, p.dir, []Registry{{Name: "r", Type: netprofile.AuthBearer}}, clk, rec)
	tick := make(chan time.Time)
	s.tick = tick
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- s.Run(ctx) }()
	p.write(map[string]string{"r/token": "v2"})
	tick <- time.Now()
	tick <- time.Now() // the second send returns only after the first poll ran
	if header(t, s, "r") != "Bearer v2" {
		t.Fatal("Run did not poll on its tick")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestNewRefuses(t *testing.T) {
	dir := t.TempDir()
	for name, tt := range map[string]struct {
		regs []Registry
		opts Options
	}{
		"bad name":     {[]Registry{{Name: "Bad", Type: netprofile.AuthBearer}}, Options{}},
		"none type":    {[]Registry{{Name: "r", Type: netprofile.AuthNone}}, Options{}},
		"codeartifact": {[]Registry{{Name: "r", Type: netprofile.AuthCodeArtifact}}, Options{}},
		"duplicate": {
			[]Registry{{Name: "r", Type: netprofile.AuthBearer}, {Name: "r", Type: netprofile.AuthBasic}}, Options{},
		},
		"short interval": {nil, Options{Interval: time.Second}},
	} {
		if _, err := New(dir, tt.regs, tt.opts); err == nil {
			t.Errorf("%s: New accepted it", name)
		}
	}
	if os.Geteuid() == 0 {
		t.Skip("root reads unreadable files")
	}
	if err := os.MkdirAll(filepath.Join(dir, "r"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "r", "token"), []byte("x"), 0o000); err != nil {
		t.Fatal(err)
	}
	_, err := New(dir, []Registry{{Name: "r", Type: netprofile.AuthBearer}}, Options{})
	if err == nil || !strings.Contains(err.Error(), "read credential key token") {
		t.Fatalf("New over an unreadable file = %v, want a refusal", err)
	}
}

func TestDefaults(t *testing.T) {
	s, err := New(t.TempDir(), nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if s.opts.Interval != DefaultInterval {
		t.Fatalf("interval = %s", s.opts.Interval)
	}
	s.Poll()
	s.Invalidate("x")
}

func assertNoSecret(t *testing.T, out string) {
	t.Helper()
	for _, f := range []string{sentinel, base64.StdEncoding.EncodeToString([]byte("svc:" + sentinel + "-pw"))} {
		if strings.Contains(out, f) {
			t.Fatalf("the log holds a credential:\n%s", out)
		}
	}
}
