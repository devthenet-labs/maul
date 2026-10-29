// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"connected", nil, "REACHABLE"},
		{"timeout", os.ErrDeadlineExceeded, "blocked"},
		{"no-route", syscall.ENETUNREACH, "blocked"},
		{"permission", syscall.EPERM, "blocked"},
		{"refused-is-not-proof", syscall.ECONNREFUSED, "inconclusive"},
		{"wrapped-refusal", fmt.Errorf("dial: %w", syscall.ECONNREFUSED), "inconclusive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classify(tc.err); got != tc.want {
				t.Fatalf("classify(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func TestTargets(t *testing.T) {
	required := map[string]string{
		"PROBE_KUBERNETES_API": "172.20.0.1", "PROBE_EGRESS_BROKER": "172.20.10.1",
		"PROBE_INTEGRATION_CONTROLLER": "172.20.10.2", "PROBE_SOURCE_CONTROLLER": "172.20.10.3",
		"PROBE_STATUS_SERVER": "172.20.10.4",
	}
	siblings := map[string]string{
		"PROBE_SIBLING_POD": "10.40.32.7", "PROBE_SIBLING_SERVICE": "172.20.99.9", "PROBE_OTHER_SLOT_POD": "10.40.33.8",
	}
	env := func(sets ...map[string]string) func(string) string {
		merged := map[string]string{}
		for _, s := range sets {
			for k, v := range s {
				merged[k] = v
			}
		}
		return func(k string) string { return merged[k] }
	}
	without := func(m map[string]string, key string) map[string]string {
		out := map[string]string{}
		for k, v := range m {
			if k != key {
				out[k] = v
			}
		}
		return out
	}
	for _, tc := range []struct {
		name   string
		getenv func(string) string
		want   []string // target addresses, or nil when inconclusive
	}{
		{"the eight slice-2 targets", env(required), []string{
			"169.254.169.254:80", "169.254.170.23:80", "1.1.1.1:443", "172.20.0.1:443", "172.20.10.1:8080",
			"172.20.10.2:8080", "172.20.10.3:9790", "172.20.10.4:8080",
		}},
		{"with the sibling targets", env(required, siblings), []string{
			"169.254.169.254:80", "169.254.170.23:80", "1.1.1.1:443", "172.20.0.1:443", "172.20.10.1:8080",
			"172.20.10.2:8080", "172.20.10.3:9790", "172.20.10.4:8080",
			"10.40.32.7:8080", "172.20.99.9:80", "10.40.33.8:8080",
		}},
		{"a required target missing", env(without(required, "PROBE_STATUS_SERVER"), siblings), nil},
		{"one sibling target missing", env(required, without(siblings, "PROBE_OTHER_SLOT_POD")), nil},
		{"a sibling target that is not an IPv4 address", env(required, siblings,
			map[string]string{"PROBE_SIBLING_POD": "sibling.maul-preview-0"}), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := targetsFrom(tc.getenv)
			if ok != (tc.want != nil) {
				t.Fatalf("ok = %v, want %v", ok, tc.want != nil)
			}
			var addrs []string
			for _, target := range got {
				addrs = append(addrs, target.addr)
			}
			if fmt.Sprint(addrs) != fmt.Sprint(tc.want) {
				t.Errorf("targets = %v, want %v", addrs, tc.want)
			}
		})
	}
}

func TestReadStatusCodeDoesNotReadTokenBody(t *testing.T) {
	const statusLine = "HTTP/1.1 200 OK\r\n"
	const rest = "Content-Length: 12\r\n\r\nsecret-token"
	r := bytes.NewReader([]byte(statusLine + rest))
	code, err := readStatusCode(r)
	if err != nil || code != 200 || r.Len() != len(rest) {
		t.Fatalf("readStatusCode = %d, %v; %d unread bytes, want %d", code, err, r.Len(), len(rest))
	}
}

func TestReadStatusCodeRejectsMalformed(t *testing.T) {
	for _, line := range []string{"oops\n", "HTTP/1.1 nope\n", strings.Repeat("x", 129)} {
		if _, err := readStatusCode(strings.NewReader(line)); err == nil {
			t.Fatalf("accepted malformed status %q", line)
		}
	}
}

// evaluatorEnv is an evaluator run's site: the broker it must reach and the
// cluster addresses it must not.
var evaluatorEnv = map[string]string{
	"PROBE_EGRESS_BROKER": "172.20.10.1", "PROBE_KUBERNETES_API": "172.20.0.1", "PROBE_CLUSTER_DNS": "172.20.0.10",
	"PROBE_SOURCE_CONTROLLER": "172.20.10.3", "PROBE_SLOT_COMPONENT": "10.40.32.7",
}

func TestEvaluatorTargets(t *testing.T) {
	with := func(key, value string) func(string) string {
		return func(k string) string {
			if k == key {
				return value
			}
			return evaluatorEnv[k]
		}
	}
	addrs := func(ts []target) []string {
		out := make([]string, 0, len(ts))
		for _, t := range ts {
			out = append(out, t.addr)
		}
		return out
	}
	for _, tc := range []struct {
		name        string
		getenv      func(string) string
		reach, deny []string // target addresses, nil when inconclusive
	}{
		{"the evaluator's row", with("", ""), []string{"172.20.10.1:8080"}, []string{
			"169.254.169.254:80", "1.1.1.1:443", "172.20.0.1:443", "172.20.0.10:53", "172.20.10.3:9790",
			"10.40.32.7:8080",
		}},
		{"the broker missing", with("PROBE_EGRESS_BROKER", ""), nil, nil},
		{"the API server missing", with("PROBE_KUBERNETES_API", ""), nil, nil},
		{"the cluster resolver missing", with("PROBE_CLUSTER_DNS", ""), nil, nil},
		{"the artifact server missing", with("PROBE_SOURCE_CONTROLLER", ""), nil, nil},
		{"the slot component missing", with("PROBE_SLOT_COMPONENT", ""), nil, nil},
		{"a name, not an address", with("PROBE_SLOT_COMPONENT", "web.maul-preview-0"), nil, nil},
		{"an IPv6 address", with("PROBE_CLUSTER_DNS", "fd00::a"), nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reach, deny, ok := evaluatorTargetsFrom(tc.getenv)
			if ok != (tc.reach != nil) {
				t.Fatalf("ok = %v, want %v", ok, tc.reach != nil)
			}
			if fmt.Sprint(addrs(reach)) != fmt.Sprint(tc.reach) || fmt.Sprint(addrs(deny)) != fmt.Sprint(tc.deny) {
				t.Errorf("targets = %v, %v; want %v, %v", addrs(reach), addrs(deny), tc.reach, tc.deny)
			}
			for _, d := range deny {
				if d.imds != (d.addr == "169.254.169.254:80") {
					t.Errorf("%s: imds = %v", d.addr, d.imds)
				}
			}
		})
	}
}

func TestAsModeArgs(t *testing.T) {
	for _, args := range [][]string{nil, {}, {"listen"}, {"connect", "10.1.2.3:80"}} {
		if handled, _ := asMode(args); handled {
			t.Errorf("asMode(%q) handled another mode's arguments", args)
		}
	}
	for _, args := range [][]string{
		{"--as"}, {"--as", "checkreview"}, {"--as", "evaluator", "x"},
		{"--as", "evaluator", "--refused-is-allowed"}, {"--as", "evaluator", refusedIsBlocked, "x"},
	} {
		if handled, code := asMode(args); !handled || code != 2 {
			t.Errorf("asMode(%q) = %v, %d; want a usage error", args, handled, code)
		}
	}
	// Without its site an evaluator run is inconclusive, never a pass.
	t.Setenv("PROBE_EGRESS_BROKER", "")
	if handled, code := asMode([]string{"--as", "evaluator"}); !handled || code != 3 {
		t.Errorf("asMode without a site = %v, %d; want inconclusive (3)", handled, code)
	}
}

// listening is a loopback target something answers on; closed is one
// nothing does (a refusal).
func listening(t *testing.T, name string) target {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	return target{name: name, addr: l.Addr().String()}
}

func closed(t *testing.T, name string) target {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return target{name: name, addr: addr}
}

func TestRunEvaluator(t *testing.T) {
	for _, tc := range []struct {
		name          string
		reachUp       bool
		denyUp        bool
		refusedBlocks bool
		want          string
		code          int
	}{
		// On loopback a refusal is the only closed answer, so a pass needs
		// --refused-is-blocked here.
		{"broker reached, the rest refused as blocked", true, false, true, outcomePass, 0},
		{"a refusal without the flag", true, false, false, outcomeInconclusive, 3},
		{"a forbidden target answers", true, true, true, outcomeSecurityFailure, 2},
		{"the broker never answers", false, false, true, outcomeNotReached, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reach := closed(t, "maul-egress-broker")
			if tc.reachUp {
				reach = listening(t, "maul-egress-broker")
			}
			deny := closed(t, "slot-component")
			if tc.denyUp {
				deny = listening(t, "slot-component")
			}
			var lines []result
			got, code := runEvaluator([]target{reach}, []target{deny}, tc.refusedBlocks, time.Second,
				func(r result) { lines = append(lines, r) })
			if got != tc.want || code != tc.code {
				t.Fatalf("runEvaluator = %s, %d; want %s, %d (lines %+v)", got, code, tc.want, tc.code, lines)
			}
			if last := lines[len(lines)-1]; last.Name != "summary" || last.Outcome != tc.want {
				t.Errorf("last line %+v, want the summary %s", last, tc.want)
			}
			wantReach := outcomeReached
			if !tc.reachUp {
				wantReach = outcomeNotReached
			}
			if lines[0].Name != "maul-egress-broker" || lines[0].Outcome != wantReach {
				t.Errorf("first line %+v, want the broker %s", lines[0], wantReach)
			}
			for _, r := range lines {
				// Nothing forbidden is dialled before the broker answers: a
				// policy not yet programmed could refuse it for the wrong
				// reason.
				if !tc.reachUp && r.Name == "slot-component" {
					t.Errorf("dialled %s although the broker never answered", r.Name)
				}
			}
		})
	}
}
