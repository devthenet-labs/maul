// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/devthenet-labs/maul/internal/previewtools/prepare"
	"github.com/devthenet-labs/maul/internal/sandboxprobe"
)

// treeTarball is a one-file GitHub-style archive whose file holds body.
func treeTarball(t *testing.T, name, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, h := range []*tar.Header{
		{Name: "owner-repo-abc/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "owner-repo-abc/" + name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))},
	} {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// prepareFixture is an artifact server with a head and a base tarball and
// the volumes prepare writes.
type prepareFixture struct {
	head, base       []byte
	srv              *httptest.Server
	trees, bin, tmp  string
	toolExec         string
	probed           bool
	probeCode        int
	headSum, baseSum string
}

func newPrepareFixture(t *testing.T) *prepareFixture {
	t.Helper()
	f := &prepareFixture{head: treeTarball(t, "main.go", "package main // head\n"),
		base: treeTarball(t, "main.go", "package main // base\n")}
	f.headSum, f.baseSum = sum(f.head), sum(f.base)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/artifacts/head.tar.gz":
			_, _ = w.Write(f.head)
		case "/artifacts/base.tar.gz":
			_, _ = w.Write(f.base)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	dir := t.TempDir()
	f.trees, f.bin, f.tmp = filepath.Join(dir, "trees"), filepath.Join(dir, "maul-bin"), filepath.Join(dir, "tmp")
	for _, d := range []string{filepath.Join(f.trees, "head"), filepath.Join(f.trees, "base"), f.bin, f.tmp} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.toolExec = filepath.Join(dir, "image", "tool-exec")
	if err := os.MkdirAll(filepath.Dir(f.toolExec), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.toolExec, []byte("\x7fELF tool-exec"), 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *prepareFixture) args() []string {
	return []string{
		"--head-url=" + f.srv.URL + "/artifacts/head.tar.gz", "--head-sha256=" + f.headSum,
		"--base-url=" + f.srv.URL + "/artifacts/base.tar.gz", "--base-sha256=" + f.baseSum,
		"--trees=" + f.trees, "--bin=" + f.bin,
	}
}

func (f *prepareFixture) deps() prepareDeps {
	return prepareDeps{
		fetcher: prepare.Fetcher{Client: f.srv.Client(),
			Sleep: func(context.Context, time.Duration) error { return nil }},
		tmpDir: f.tmp, toolExec: f.toolExec,
		probe: func(context.Context) int { f.probed = true; return f.probeCode },
	}
}

func TestPrepare(t *testing.T) {
	f := newPrepareFixture(t)
	var stderr bytes.Buffer
	if code := runPrepare(context.Background(), f.args(), f.deps(), &stderr); code != 0 {
		t.Fatalf("prepare = %d, stderr %s", code, stderr.String())
	}
	for side, want := range map[string]string{"head": "package main // head\n", "base": "package main // base\n"} {
		p := filepath.Join(f.trees, side, "main.go")
		raw, err := os.ReadFile(p)
		if err != nil || string(raw) != want {
			t.Errorf("%s = %q, %v; want %q", p, raw, err, want)
		}
		if info, err := os.Stat(p); err != nil || info.Mode().Perm() != 0o644 {
			t.Errorf("%s mode %v, %v; want readable by every side", p, info.Mode(), err)
		}
	}
	info, err := os.Stat(filepath.Join(f.bin, "tool-exec"))
	if err != nil || info.Mode().Perm() != 0o555 {
		t.Errorf("tool-exec %v, %v; want installed 0555", info, err)
	}
	if !f.probed {
		t.Error("the strict probe did not run")
	}
}

// TestPrepareProbeExit: the probe's verdict is the init's exit status, 78
// on open egress, after every tree is in place.
func TestPrepareProbeExit(t *testing.T) {
	for _, code := range []int{sandboxprobe.ExitUnenforced, 1, 2} {
		f := newPrepareFixture(t)
		f.probeCode = code
		if got := runPrepare(context.Background(), f.args(), f.deps(), io.Discard); got != code {
			t.Errorf("prepare with the probe exiting %d = %d", code, got)
		}
	}
}

// TestPrepareRealProbeArguments: the in-process probe is the strict one
// over every target, the cluster resolver included: with no resolver
// address it is a configuration error, never a pass.
func TestPrepareRealProbeArguments(t *testing.T) {
	env := map[string]string{"KUBERNETES_SERVICE_HOST": "10.96.0.1"}
	code := strictProbe(func(k string) string { return env[k] })(context.Background())
	if code != 2 {
		t.Fatalf("strict probe without %s = %d, want 2", sandboxprobe.DNSEnv, code)
	}
	if strings.Join(probeArgs, " ") != "--mode=strict --targets=internet,k8s,imds,dns" {
		t.Errorf("probe arguments %v", probeArgs)
	}
}

// TestPrepareTampered: a tarball that is not its digest's fails the init
// before anything of it is unpacked, the probe never reached.
func TestPrepareTampered(t *testing.T) {
	f := newPrepareFixture(t)
	f.base = treeTarball(t, "main.go", "package main // tampered\n")
	if code := runPrepare(context.Background(), f.args(), f.deps(), io.Discard); code != 1 {
		t.Fatalf("prepare = %d, want 1", code)
	}
	if left, _ := os.ReadDir(filepath.Join(f.trees, "base")); len(left) != 0 {
		t.Errorf("base holds %d entries of a tampered tarball", len(left))
	}
	if f.probed {
		t.Error("the probe ran after a refusal")
	}
	if left, _ := os.ReadDir(f.bin); len(left) != 0 {
		t.Error("tool-exec was installed after a refusal")
	}
}

func TestPrepareFlagRefusals(t *testing.T) {
	f := newPrepareFixture(t)
	with := func(flag, value string) []string {
		var out []string
		for _, a := range f.args() {
			if strings.HasPrefix(a, flag+"=") {
				if value == "<drop>" {
					continue
				}
				a = flag + "=" + value
			}
			out = append(out, a)
		}
		return out
	}
	tests := map[string][]string{
		"no head URL":       with("--head-url", "<drop>"),
		"no base digest":    with("--base-sha256", "<drop>"),
		"short digest":      with("--head-sha256", "abc"),
		"upper-case digest": with("--head-sha256", strings.ToUpper(f.headSum)),
		"file URL":          with("--base-url", "file:///etc/passwd"),
		"relative trees":    with("--trees", "trees"),
		"relative bin":      with("--bin", "maul-bin"),
		"positional":        append(f.args(), "extra"),
		"unknown flag":      append(f.args(), "--side=head"),
		"one tarball twice": with("--base-url", f.srv.URL+"/artifacts/head.tar.gz"),
	}
	for name, args := range tests {
		t.Run(name, func(t *testing.T) {
			if code := runPrepare(context.Background(), args, f.deps(), io.Discard); code != 2 {
				t.Errorf("prepare = %d, want 2", code)
			}
			if f.probed {
				t.Error("the probe ran")
			}
		})
	}
}
