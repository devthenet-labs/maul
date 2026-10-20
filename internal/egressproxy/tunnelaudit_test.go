// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/devthenet-labs/maul/internal/netprofile"
)

// TestTunnelRefusalAuditHost: a refused CONNECT's close line carries the
// host part bare (never pre-quoted, so `grep host=1.1.1.1` finds it) and,
// when the host was not validated, the raw target in its own attribute.
// A validated host has no target attribute.
func TestTunnelRefusalAuditHost(t *testing.T) {
	tests := []struct {
		name, line string
		cfg        Config
		outcome    string
		host       string
		target     string // "" means the attribute is absent
	}{
		{"IP literal", "CONNECT 1.1.1.1:443 HTTP/1.1", Config{}, OutcomeIPLiteral, "1.1.1.1", "1.1.1.1:443"},
		{"port 8443", "CONNECT pypi.org:8443 HTTP/1.1", Config{}, OutcomePort, "pypi.org", "pypi.org:8443"},
		{"bad host", "CONNECT pkg_x.registry.dev:443 HTTP/1.1", Config{}, OutcomeBadHost, "pkg_x.registry.dev",
			"pkg_x.registry.dev:443"},
		{"absolute-form", "CONNECT https://pkg.registry.dev:443/ HTTP/1.1", Config{}, OutcomeBadRequest, "",
			"https://pkg.registry.dev:443/"},
		{"suspended", "CONNECT pypi.org:443 HTTP/1.1", Config{Suspended: true}, OutcomeSuspended, "pypi.org",
			"pypi.org:443"},
		{"not allowlisted", "CONNECT other.registry.dev:443 HTTP/1.1", Config{}, OutcomeNotAllowed,
			"other.registry.dev", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTunnelHarness(t, tt.cfg)
			c, br := h.connect(t, "web", tt.line)
			_ = readRaw(t, br)
			_ = c.Close()
			line := h.closeLine(t, 1)
			if line["outcome"] != tt.outcome {
				t.Fatalf("outcome %v, want %s (line %v)", line["outcome"], tt.outcome, line)
			}
			if line["host"] != tt.host {
				t.Errorf("host = %q, want %q", line["host"], tt.host)
			}
			got, present := line["target"]
			switch {
			case tt.target == "" && present:
				t.Errorf("target = %q on a validated host, want no attribute", got)
			case tt.target != "" && got != tt.target:
				t.Errorf("target = %v, want %q", got, tt.target)
			}
		})
	}
}

// TestTunnelAuditTargetIsSafe: an attacker-chosen target with control
// characters, a forged attribute and invalid UTF-8 stays one line under
// both handlers, its bytes escaped by the handler, never raw, and bounded
// in length.
func TestTunnelAuditTargetIsSafe(t *testing.T) {
	raw := "evil\r\nlevel=ERROR host=forged\x1b[31m\x00\xff:443" + strings.Repeat("a", 1000)
	_, view, rf := judgeTarget(netprofile.Profile{}, netprofile.DenyContext{}, raw)
	if rf == nil || rf.outcome != OutcomeBadRequest {
		t.Fatalf("judgeTarget(%q) = %v, want bad_request", raw, rf)
	}
	e := tunnelEntry{profile: "web", host: view.host, target: view.target, outcome: rf.outcome}
	if e.host != "" {
		t.Errorf("host = %q for an unparseable target, want empty", e.host)
	}
	if len(e.target) > maxEcho+len("...(truncated)") {
		t.Errorf("target is %d bytes, want at most the clip", len(e.target))
	}
	for name, mk := range map[string]func(*bytes.Buffer) slog.Handler{
		"text": func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) },
		"json": func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) },
	} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			auditTunnel(context.Background(), slog.New(mk(&buf)), e)
			out := buf.String()
			if strings.Count(out, "\n") != 1 || !strings.HasSuffix(out, "\n") {
				t.Errorf("not exactly one line: %q", out)
			}
			for _, c := range []string{"\r", "\x1b", "\x00", "\xff"} {
				if strings.Contains(out, c) {
					t.Errorf("raw %q reached the line: %q", c, out)
				}
			}
			if name == "text" {
				// The whole value is one quoted, escaped string: the
				// forged "host=" sits inside it, never as an attribute.
				want := ` host="" target="evil\r\nlevel=ERROR host=forged\x1b[31m\x00\xff:443`
				if !strings.Contains(out, want) {
					t.Errorf("line %q lacks the escaped target %q", out, want)
				}
				return
			}
			var m map[string]any
			if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
				t.Fatal(err)
			}
			if m["host"] != "" {
				t.Errorf("host = %q, want empty", m["host"])
			}
			if s, _ := m["target"].(string); !strings.HasPrefix(s, "evil\r\nlevel=ERROR host=forged\x1b") {
				t.Errorf("target decodes to %q, want the raw prefix", s)
			}
		})
	}
}

// TestTunnelAuditHostTextGrep: under the text handler the operator's
// grep for the bare host matches an IP-literal refusal.
func TestTunnelAuditHostTextGrep(t *testing.T) {
	_, view, rf := judgeTarget(netprofile.Profile{}, netprofile.DenyContext{}, "1.1.1.1:443")
	if rf == nil {
		t.Fatal("1.1.1.1 admitted")
	}
	var buf bytes.Buffer
	auditTunnel(context.Background(), slog.New(slog.NewTextHandler(&buf, nil)),
		tunnelEntry{host: view.host, target: view.target, outcome: rf.outcome})
	for _, want := range []string{" host=1.1.1.1 ", " target=1.1.1.1:443 "} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("line %q lacks %q", buf.String(), want)
		}
	}
}

// TestTunnelRefusalMetricHost: a refused CONNECT whose bare host part is
// allowlisted (wrong port, suspended, HTTP/1.0) never puts that host on
// the tunnels metric, because the host was not validated: the metric
// labels a host only after it passed judgement.
func TestTunnelRefusalMetricHost(t *testing.T) {
	testMetrics()
	tests := []struct {
		name, line string
		cfg        Config
		outcome    string
	}{
		{"port 8443", "CONNECT pkg.registry.dev:8443 HTTP/1.1", Config{}, OutcomePort},
		{"suspended", "CONNECT pkg.registry.dev:443 HTTP/1.1", Config{Suspended: true}, OutcomeSuspended},
		{"HTTP/1.0", "CONNECT pkg.registry.dev:443 HTTP/1.0", Config{}, OutcomeBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			labelled := map[string]string{"profile": "web", "outcome": tt.outcome, "host": "pkg.registry.dev"}
			bare := map[string]string{"profile": "web", "outcome": tt.outcome, "host": ""}
			before := [2]int64{sum(t, "maul.egress_proxy.tunnels", labelled),
				sum(t, "maul.egress_proxy.tunnels", bare)}
			h := newTunnelHarness(t, tt.cfg)
			c, br := h.connect(t, "web", tt.line)
			_ = readRaw(t, br)
			_ = c.Close()
			if line := h.closeLine(t, 1); line["outcome"] != tt.outcome || line["host"] != "pkg.registry.dev" {
				t.Fatalf("close line %v, want outcome %s and host pkg.registry.dev", line, tt.outcome)
			}
			// The count follows the close line; wait for it.
			deadline := time.Now().Add(10 * time.Second)
			for sum(t, "maul.egress_proxy.tunnels", bare)-before[1] < 1 {
				if time.Now().After(deadline) {
					t.Fatalf("no %s tunnel counted with host=\"\"", tt.outcome)
				}
				time.Sleep(5 * time.Millisecond)
			}
			if got := sum(t, "maul.egress_proxy.tunnels", labelled) - before[0]; got != 0 {
				t.Errorf("an unvalidated host became a metric attribute: +%d with host=pkg.registry.dev", got)
			}
		})
	}
}
