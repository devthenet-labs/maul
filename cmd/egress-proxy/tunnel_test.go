// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"log/slog"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bitwise-media-group/patchy/internal/egressproxy"
)

// tunnelProfile is a tunnel-only profile beside a go profile.
const tunnelProfile = `[{"name":"go","route":"go","port":8090,"repositories":["devthenet-labs/patchy-target"],
  "modules":["github.com/google/uuid"]},
 {"name":"py","route":"tunnel","port":8092,"repositories":["devthenet-labs/patchy-pip-fixture"],
  "modules":[],"hosts":["pypi.org"],"clients":["pip"]}]`

// TestConfigureTunnelDefaults: with no tunnel flag set the engine gets
// the documented defaults (its zero values, which the engine fills, and
// the flag defaults themselves), and a profile set with hosts starts.
func TestConfigureTunnelDefaults(t *testing.T) {
	s, err := configure(serveOpts(t, "--network-profiles", tunnelProfile))
	if err != nil {
		t.Fatal(err)
	}
	want := egressproxy.TunnelLimits{
		MaxConcurrent:        egressproxy.DefaultTunnelMaxConcurrent,
		PerSourceConcurrent:  egressproxy.DefaultTunnelPerSourceConcurrent,
		PerSourceRate:        egressproxy.DefaultTunnelPerSourceRate,
		PerSourceBurst:       egressproxy.DefaultTunnelPerSourceBurst,
		Wait:                 egressproxy.DefaultTunnelLimitWait,
		DialTimeout:          egressproxy.DefaultTunnelDialTimeout,
		HelloTimeout:         egressproxy.DefaultTunnelHelloTimeout,
		IdleTimeout:          egressproxy.DefaultTunnelIdleTimeout,
		MaxDuration:          egressproxy.DefaultTunnelMaxDuration,
		MaxUploadBytes:       egressproxy.DefaultTunnelMaxUploadBytes,
		MaxDownloadBytes:     egressproxy.DefaultTunnelMaxDownloadBytes,
		SourceUploadBudget:   egressproxy.DefaultTunnelSourceUploadBudget,
		SourceDownloadBudget: egressproxy.DefaultTunnelSourceDownloadBudget,
		SourceBudgetIdle:     egressproxy.DefaultTunnelSourceBudgetIdle,
		MinBudgetIdle:        egressproxy.DefaultTunnelMinBudgetIdle,
		MaxSources:           egressproxy.DefaultTunnelMaxSources,
		ProgressInterval:     egressproxy.DefaultTunnelProgressInterval,
		Deny:                 []netip.Prefix{},
	}
	if !reflect.DeepEqual(s.engine.TunnelLimits, want) {
		t.Errorf("tunnel limits = %+v\nwant %+v", s.engine.TunnelLimits, want)
	}
	if _, err := egressproxy.New(s.engine, nil); err != nil {
		t.Errorf("a profile set with hosts did not start: %v", err)
	}
}

// TestConfigureTunnelFlags: every tunnel flag reaches the engine, from the
// command line and from its PATCHY_TUNNEL_* variable; 0 turns the progress
// line off.
func TestConfigureTunnelFlags(t *testing.T) {
	s, err := configure(serveOpts(t, "--network-profiles", tunnelProfile,
		"--tunnel-max-concurrent", "9", "--tunnel-max-concurrent-per-source", "3",
		"--tunnel-rate-per-source", "1.5", "--tunnel-burst-per-source", "4", "--tunnel-limit-wait", "2s",
		"--tunnel-dial-timeout", "3s", "--tunnel-hello-timeout", "4s", "--tunnel-idle-timeout", "5m",
		"--tunnel-max-duration", "1h", "--tunnel-max-upload-bytes", "65536",
		"--tunnel-max-download-bytes", "131072", "--tunnel-source-upload-budget", "262144",
		"--tunnel-source-download-budget", "524288", "--tunnel-source-budget-idle", "3h",
		"--tunnel-min-budget-idle", "150m", "--tunnel-max-sources", "10", "--tunnel-progress-interval", "0",
		"--tunnel-deny-cidrs", "203.0.113.10/32, 198.51.100.0/24"))
	if err != nil {
		t.Fatal(err)
	}
	l := s.engine.TunnelLimits
	got := []any{l.MaxConcurrent, l.PerSourceConcurrent, l.PerSourceRate, l.PerSourceBurst, l.Wait, l.DialTimeout,
		l.HelloTimeout, l.IdleTimeout, l.MaxDuration, l.MaxUploadBytes, l.MaxDownloadBytes, l.SourceUploadBudget,
		l.SourceDownloadBudget, l.SourceBudgetIdle, l.MinBudgetIdle, l.MaxSources}
	want := []any{9, 3, 1.5, 4, 2 * time.Second, 3 * time.Second, 4 * time.Second, 5 * time.Minute, time.Hour,
		int64(65536), int64(131072), int64(262144), int64(524288), 3 * time.Hour, 150 * time.Minute, 10}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tunnel limits = %v, want %v", got, want)
	}
	if l.ProgressInterval >= 0 {
		t.Errorf("--tunnel-progress-interval 0 = %s, want off (negative)", l.ProgressInterval)
	}
	if len(l.Deny) != 2 || l.Deny[0].String() != "203.0.113.10/32" || len(s.engine.Policy.Deny) != 0 {
		t.Errorf("tunnel deny %v, route deny %v: the tunnel ranges must not reach --deny-cidrs", l.Deny,
			s.engine.Policy.Deny)
	}

	t.Setenv("PATCHY_TUNNEL_MAX_SOURCES", "77")
	t.Setenv("PATCHY_TUNNEL_DENY_CIDRS", "192.0.2.1/32")
	t.Setenv("PATCHY_TUNNEL_IDLE_TIMEOUT", "90s")
	s, err = configure(serveOpts(t, "--network-profiles", tunnelProfile))
	if err != nil {
		t.Fatal(err)
	}
	if l := s.engine.TunnelLimits; l.MaxSources != 77 || l.IdleTimeout != 90*time.Second || len(l.Deny) != 1 {
		t.Errorf("tunnel limits from the environment = %+v", l)
	}
}

// TestConfigureTunnelRefuses: a budget window below its floor and an
// unparsable tunnel CIDR refuse to start, naming the flag; so does a
// profile whose hosts fail validation, naming the profile.
func TestConfigureTunnelRefuses(t *testing.T) {
	for _, tt := range []struct {
		args    []string
		wantErr string
	}{
		{[]string{"--network-profiles", tunnelProfile, "--tunnel-source-budget-idle", "30m"},
			"--tunnel-source-budget-idle 30m0s is below --tunnel-min-budget-idle 2h0m0s"},
		{[]string{"--network-profiles", tunnelProfile, "--tunnel-source-budget-idle", "2h",
			"--tunnel-min-budget-idle", "3h"}, "is below --tunnel-min-budget-idle"},
		{[]string{"--network-profiles", tunnelProfile, "--tunnel-deny-cidrs", "not-a-cidr"}, "--tunnel-deny-cidrs"},
		{[]string{"--network-profiles", strings.Replace(tunnelProfile, `"pypi.org"`, `"github.com"`, 1)},
			"profile py"},
		{[]string{"--network-profiles", strings.Replace(tunnelProfile, `"pypi.org"`, `"1.2.3.4"`, 1)},
			"profile py"},
	} {
		_, err := configure(serveOpts(t, tt.args...))
		if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("configure(%v) = %v, want an error containing %q", tt.args[2:], err, tt.wantErr)
		}
	}
}

// TestLogTunnel: one line per tunnel profile, a warning per client preset
// missing its usual hosts, and a warning when no tunnel deny range is set;
// nothing at all without a tunnel profile.
func TestLogTunnel(t *testing.T) {
	s, err := configure(serveOpts(t, "--network-profiles", tunnelProfile))
	if err != nil {
		t.Fatal(err)
	}
	out := logged(t, s)
	for _, want := range []string{`"msg":"tunnel configured"`, `"profile":"py"`, `"hosts":["pypi.org"]`,
		"files.pythonhosted.org", "--tunnel-deny-cidrs is empty"} {
		if !strings.Contains(out, want) {
			t.Errorf("startup lines lack %q:\n%s", want, out)
		}
	}
	if n := strings.Count(out, `"level":"WARN"`); n != 2 {
		t.Errorf("%d warnings, want 2 (pip's hosts, the empty deny list):\n%s", n, out)
	}

	s, err = configure(serveOpts(t, "--network-profiles", tunnelProfile, "--tunnel-deny-cidrs", "203.0.113.0/24"))
	if err != nil {
		t.Fatal(err)
	}
	if out := logged(t, s); strings.Contains(out, "is empty") {
		t.Errorf("deny warning with a deny list set:\n%s", out)
	}

	s, err = configure(serveOpts(t, "--network-profiles", twoProfiles))
	if err != nil {
		t.Fatal(err)
	}
	if out := logged(t, s); out != "" {
		t.Errorf("go-only profiles logged tunnel lines:\n%s", out)
	}
}

// logged is what logTunnel writes for s.
func logged(t *testing.T, s settings) string {
	t.Helper()
	var buf syncBuffer
	logTunnel(t.Context(), slog.New(slog.NewJSONHandler(&buf, nil)), s.engine.Profiles, s.engine.TunnelLimits)
	return buf.String()
}
