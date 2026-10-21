// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/devthenet-labs/maul/internal/cli"
	"github.com/devthenet-labs/maul/internal/egressproxy"
)

const twoProfiles = `[{"name":"go","route":"go","port":8090,"repositories":["devthenet-labs/patchy-target"],
  "modules":["github.com/google/uuid"]},
 {"name":"tools","route":"go","port":8091,"repositories":["devthenet-labs/patchy-preview-demo"],
  "modules":["golang.org/x"]}]`

// serveOpts resolves args through the real serve flag surface, as the
// binary does, without starting anything.
func serveOpts(t *testing.T, args ...string) *cli.Options {
	t.Helper()
	opts := cli.NewOptions()
	root := cli.NewControllerRoot("egress-proxy", "test", opts)
	serve := newServeCmd(opts)
	serve.RunE = func(*cobra.Command, []string) error { return nil }
	root.AddCommand(serve)
	root.SetArgs(append([]string{"serve"}, args...))
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	return opts
}

func TestConfigureRefuses(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"no profiles", nil, "no profile configured"},
		{"empty array", []string{"--network-profiles", "[]"}, "no profile configured"},
		{"bad json", []string{"--network-profiles", "{"}, "--network-profiles"},
		{"unknown field", []string{"--network-profiles",
			`[{"name":"go","route":"go","port":8090,"repositories":["a/b"],"modules":["x.com/y"],"extra":1}]`},
			"unknown field"},
		{"invalid profile", []string{"--network-profiles",
			`[{"name":"go","route":"npm","port":8090,"repositories":["a/b"],"modules":["x.com/y"]}]`},
			"route"},
		{"health port collision", []string{"--network-profiles", twoProfiles, "--health-addr", ":8091"},
			"profile tools: port 8091 is --health-addr's port"},
		{"bad health addr", []string{"--network-profiles", twoProfiles, "--health-addr", "8081"}, "--health-addr"},
		{"http module proxy", []string{"--network-profiles", twoProfiles,
			"--go-proxy-upstream", "http://proxy.golang.org"}, "--go-proxy-upstream \"http://proxy.golang.org\": " +
			"the upstream must be https"},
		{"http sumdb", []string{"--network-profiles", twoProfiles,
			"--go-sumdb-upstream", "http://127.0.0.1:8080"}, "--go-sumdb-upstream"},
		{"upstream with a query", []string{"--network-profiles", twoProfiles,
			"--go-proxy-upstream", "https://proxy.golang.org/?x=1"}, "--go-proxy-upstream"},
		{"bad CIDR", []string{"--network-profiles", twoProfiles, "--deny-cidrs", "10.0.0.0/8,nope"},
			"--deny-cidrs"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := configure(serveOpts(t, tt.args...))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("configure error = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestConfigureMapsFlags(t *testing.T) {
	s, err := configure(serveOpts(t, "--network-profiles", twoProfiles,
		"--deny-cidrs", "10.100.0.0/16, 172.20.0.10/32",
		"--max-concurrent", "7", "--max-concurrent-per-source", "3",
		"--requests-per-second-per-source", "2.5", "--burst-per-source", "4",
		"--limit-wait", "5s", "--request-timeout", "1m", "--upstream-header-timeout", "20s",
		"--max-zip-bytes", "1048576", "--suspended"))
	if err != nil {
		t.Fatal(err)
	}
	want := egressproxy.Limits{MaxConcurrent: 7, PerSourceConcurrent: 3, PerSourceRate: 2.5, PerSourceBurst: 4,
		Wait: 5 * time.Second, RequestTimeout: time.Minute, HeaderTimeout: 20 * time.Second, MaxZipBytes: 1 << 20}
	if s.engine.Limits != want {
		t.Errorf("limits = %+v, want %+v", s.engine.Limits, want)
	}
	if !s.engine.Suspended || s.healthAddr != ":8081" || s.engine.Profiles.Len() != 2 {
		t.Errorf("settings = %+v", s)
	}
	if got := s.engine.GoProxy.String() + " " + s.engine.SumDB.String(); got !=
		"https://proxy.golang.org https://sum.golang.org" {
		t.Errorf("upstreams = %s", got)
	}
	if len(s.engine.Policy.Deny) != 2 || s.engine.Policy.Deny[1].String() != "172.20.0.10/32" {
		t.Errorf("deny = %v", s.engine.Policy.Deny)
	}
}

// TestConfigureAttribution: attribution is off by default; on, it carries
// the namespace and kubeconfig to the pod index.
func TestConfigureAttribution(t *testing.T) {
	s, err := configure(serveOpts(t, "--network-profiles", twoProfiles))
	if err != nil {
		t.Fatal(err)
	}
	if s.attributePods || s.agentNamespace != "maul-agents" {
		t.Errorf("default attribution = %v in %q, want off in maul-agents", s.attributePods, s.agentNamespace)
	}
	s, err = configure(serveOpts(t, "--network-profiles", twoProfiles, "--attribute-pods",
		"--agent-namespace", "agents", "--kubeconfig", "/k"))
	if err != nil {
		t.Fatal(err)
	}
	if !s.attributePods || s.agentNamespace != "agents" || s.kubeconfig != "/k" {
		t.Errorf("attribution settings = %+v", s)
	}
	if _, err := configure(serveOpts(t, "--network-profiles", twoProfiles, "--attribute-pods",
		"--agent-namespace", "")); err == nil {
		t.Error("--attribute-pods without a namespace was accepted")
	}
	if _, err := podIndex(settings{kubeconfig: "/nonexistent/kubeconfig", agentNamespace: "agents"}); err == nil ||
		!strings.Contains(err.Error(), "--attribute-pods") {
		t.Errorf("podIndex with a missing kubeconfig = %v, want an --attribute-pods error", err)
	}
}

// TestConfigureFromEnvironment: the chart's ConfigMap sets MAUL_*
// variables, not flags.
func TestConfigureFromEnvironment(t *testing.T) {
	t.Setenv("MAUL_NETWORK_PROFILES", twoProfiles)
	t.Setenv("MAUL_SUSPENDED", "true")
	t.Setenv("MAUL_HEALTH_ADDR", ":9000")
	s, err := configure(serveOpts(t))
	if err != nil {
		t.Fatal(err)
	}
	if s.engine.Profiles.Len() != 2 || !s.engine.Suspended || s.healthAddr != ":9000" {
		t.Errorf("settings from the environment = %+v", s)
	}
}

// recordingListen binds every requested address on loopback instead,
// recording what was asked, and holds the listen of hold until release is
// closed.
type recordingListen struct {
	mu      sync.Mutex
	asked   []string
	bound   map[string]string // asked -> actual
	hold    string
	release chan struct{}
	holding chan struct{}
}

func (r *recordingListen) listen(network, address string) (net.Listener, error) {
	if address == r.hold {
		close(r.holding)
		<-r.release
	}
	ln, err := net.Listen(network, "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.asked = append(r.asked, address)
	r.bound[address] = ln.Addr().String()
	return ln, nil
}

func (r *recordingListen) addr(asked string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bound[asked]
}

// TestRunListenerPerProfile: one listener per profile on the profile's
// port, each serving that profile alone; /readyz only after every profile
// listener is bound; a clean stop on cancel.
func TestRunListenerPerProfile(t *testing.T) {
	s, err := configure(serveOpts(t, "--network-profiles", twoProfiles, "--health-addr", ":18081"))
	if err != nil {
		t.Fatal(err)
	}
	var logBuf syncBuffer
	srv, err := egressproxy.New(s.engine, slog.New(slog.NewJSONHandler(&logBuf, nil)))
	if err != nil {
		t.Fatal(err)
	}
	rl := &recordingListen{bound: map[string]string{}, hold: ":8091", release: make(chan struct{}),
		holding: make(chan struct{})}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- run(ctx, srv, s.healthAddr, rl.listen, slog.New(slog.DiscardHandler)) }()

	<-rl.holding // the health listener and profile go are bound, tools is not
	if code := probe(t, srv.HealthHandler(), "/readyz"); code != http.StatusServiceUnavailable {
		t.Errorf("readyz before every profile listener is bound = %d, want 503", code)
	}
	close(rl.release)
	deadline := time.Now().Add(5 * time.Second)
	for probe(t, srv.HealthHandler(), "/readyz") != http.StatusOK {
		if time.Now().After(deadline) {
			t.Fatal("readyz never became 200")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if want := []string{":18081", ":8090", ":8091"}; !slices.Equal(rl.asked, want) {
		t.Errorf("listened on %v, want %v", rl.asked, want)
	}
	resp, err := http.Get("http://" + rl.addr(":18081") + "/readyz")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Errorf("readyz over the health listener = %v, %v", resp, err)
	} else {
		_ = resp.Body.Close()
	}
	for asked, profile := range map[string]string{":8090": "go", ":8091": "tools"} {
		resp, err := http.Get("http://" + rl.addr(asked) + "/go/sumdb/sum.golang.org/supported")
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("supported on %s = %v, %v", asked, resp, err)
		}
		_ = resp.Body.Close()
		if !strings.Contains(logBuf.String(), `"profile":"`+profile+`"`) {
			t.Errorf("listener %s did not serve profile %s:\n%s", asked, profile, logBuf.String())
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("run = %v, want a clean stop", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run did not stop")
	}
}

// TestRunListenFailure: a profile port that cannot be bound fails the
// start, naming the profile.
func TestRunListenFailure(t *testing.T) {
	s, err := configure(serveOpts(t, "--network-profiles", twoProfiles))
	if err != nil {
		t.Fatal(err)
	}
	srv, err := egressproxy.New(s.engine, nil)
	if err != nil {
		t.Fatal(err)
	}
	listen := func(network, address string) (net.Listener, error) {
		if address == ":8091" {
			return nil, &net.OpError{Op: "listen", Err: io.EOF}
		}
		return net.Listen(network, "127.0.0.1:0")
	}
	err = run(t.Context(), srv, s.healthAddr, listen, slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "profile tools listener :8091") {
		t.Errorf("run = %v, want the tools listener failure", err)
	}
	if code := probe(t, srv.HealthHandler(), "/readyz"); code != http.StatusServiceUnavailable {
		t.Errorf("readyz after a failed bind = %d, want 503", code)
	}
}

func probe(t *testing.T, h http.Handler, path string) int {
	t.Helper()
	rec := &statusRecorder{header: http.Header{}}
	req, err := http.NewRequest(http.MethodGet, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	h.ServeHTTP(rec, req)
	if rec.code == 0 {
		return http.StatusOK
	}
	return rec.code
}

type statusRecorder struct {
	header http.Header
	code   int
}

func (r *statusRecorder) Header() http.Header         { return r.header }
func (r *statusRecorder) Write(p []byte) (int, error) { return len(p), nil }
func (r *statusRecorder) WriteHeader(code int)        { r.code = code }

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
