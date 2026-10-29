// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package sandboxprobe

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// waitTargets are the targets a default-image Job's wait probe dials.
var waitTargets = []Target{
	{Name: "kubernetes API", Addr: "172.20.0.1:443"},
	{Name: "cloud metadata", Addr: "169.254.169.254:80"},
}

// k8sEnv is the environment the kubelet gives every container, plus a
// value no verdict may ever repeat.
func k8sEnv(extra map[string]string) func(string) string {
	env := map[string]string{
		"KUBERNETES_SERVICE_HOST": "172.20.0.1",
		"KUBERNETES_SERVICE_PORT": "443",
		"ANTHROPIC_AUTH_TOKEN":    "s3cr3t-placeholder",
	}
	for k, v := range extra {
		env[k] = v
	}
	return func(k string) string { return env[k] }
}

func addrs(targets []Target) []string {
	out := make([]string, len(targets))
	for i, t := range targets {
		out[i] = t.Addr
	}
	return out
}

func TestParse(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		env       map[string]string
		wantMode  Mode
		wantAddrs []string
		wantTime  time.Duration
		wantErr   string
	}{
		{name: "no arguments is today's strict probe", wantMode: ModeStrict,
			wantAddrs: []string{"1.1.1.1:443", "172.20.0.1:443", "169.254.169.254:80"}, wantTime: DefaultTimeout},
		{name: "strict named explicitly", args: []string{"--mode=strict"}, wantMode: ModeStrict,
			wantAddrs: []string{"1.1.1.1:443", "172.20.0.1:443", "169.254.169.254:80"}, wantTime: DefaultTimeout},
		{name: "wait defaults to the api server and metadata", args: []string{"--mode=wait"}, wantMode: ModeWait,
			wantAddrs: []string{"172.20.0.1:443", "169.254.169.254:80"}, wantTime: DefaultTimeout},
		{name: "wait with the jobs arguments", args: []string{"--mode=wait", "--targets=k8s,imds"},
			env: map[string]string{TimeoutEnv: "30s"}, wantMode: ModeWait,
			wantAddrs: []string{"172.20.0.1:443", "169.254.169.254:80"}, wantTime: 30 * time.Second},
		{name: "separate-value flags", args: []string{"--mode", "wait", "--targets", "imds"}, wantMode: ModeWait,
			wantAddrs: []string{"169.254.169.254:80"}, wantTime: DefaultTimeout},
		{name: "wait may name the internet explicitly", args: []string{"--mode=wait", "--targets=internet"},
			wantMode: ModeWait, wantAddrs: []string{"1.1.1.1:443"}, wantTime: DefaultTimeout},
		{name: "strict with the evaluator's targets", args: []string{"--mode=strict", "--targets=internet,k8s,imds,dns"},
			env: map[string]string{DNSEnv: "10.96.0.10:53", TimeoutEnv: "30s"}, wantMode: ModeStrict,
			wantAddrs: []string{"1.1.1.1:443", "172.20.0.1:443", "169.254.169.254:80", "10.96.0.10:53"},
			wantTime:  30 * time.Second},
		{name: "strict with targets and no mode flag", args: []string{"--targets=imds"}, wantMode: ModeStrict,
			wantAddrs: []string{"169.254.169.254:80"}, wantTime: DefaultTimeout},
		{name: "strict targets with a bad window", args: []string{"--mode=strict", "--targets=imds"},
			env: map[string]string{TimeoutEnv: "twenty"}, wantErr: "not a duration"},
		{name: "strict dns without its address", args: []string{"--mode=strict", "--targets=k8s,dns"},
			wantErr: DNSEnv},
		{name: "strict targets with an unknown key", args: []string{"--mode=strict", "--targets=k8s,dnz"},
			wantErr: `unknown target "dnz"`},
		{name: "wait may name dns", args: []string{"--mode=wait", "--targets=dns"},
			env: map[string]string{DNSEnv: "10.96.0.10:53"}, wantMode: ModeWait,
			wantAddrs: []string{"10.96.0.10:53"}, wantTime: DefaultTimeout},
		{name: "unknown mode", args: []string{"--mode=lenient"}, wantErr: `"lenient"`},
		{name: "unknown target", args: []string{"--mode=wait", "--targets=k8s,dnz"}, wantErr: `unknown target "dnz"`},
		{name: "repeated target", args: []string{"--mode=wait", "--targets=imds,imds"}, wantErr: "twice"},
		{name: "empty targets", args: []string{"--mode=wait", "--targets="}, wantErr: "names no target"},
		{name: "positional argument", args: []string{"wait"}, wantErr: `unexpected argument "wait"`},
		{name: "unknown flag", args: []string{"--window=5s"}, wantErr: "window"},
		{name: "bad window in wait mode", args: []string{"--mode=wait"}, env: map[string]string{TimeoutEnv: "twenty"},
			wantErr: "not a duration"},
		{name: "wait without an api server to check", args: []string{"--mode=wait"},
			env: map[string]string{"KUBERNETES_SERVICE_HOST": ""}, wantErr: "KUBERNETES_SERVICE_HOST"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, mode, err := Parse(tt.args, k8sEnv(tt.env))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Parse error = %v, want one mentioning %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if mode != tt.wantMode || !slices.Equal(addrs(p.Targets), tt.wantAddrs) || p.Timeout != tt.wantTime {
				t.Errorf("Parse = %s %v %s, want %s %v %s", mode, addrs(p.Targets), p.Timeout,
					tt.wantMode, tt.wantAddrs, tt.wantTime)
			}
		})
	}
}

// TestTargetsForDerivesTheAPIServer: the k8s target is the address the
// kubelet announced, in host:port form for both IP families, and refused
// rather than dropped when the kubelet announced none.
func TestTargetsForDerivesTheAPIServer(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		want    string
		wantErr bool
	}{
		{"host and port", map[string]string{"KUBERNETES_SERVICE_HOST": "10.96.0.1",
			"KUBERNETES_SERVICE_PORT": "6443"}, "10.96.0.1:6443", false},
		{"host only defaults to 443", map[string]string{"KUBERNETES_SERVICE_HOST": "10.96.0.1",
			"KUBERNETES_SERVICE_PORT": ""}, "10.96.0.1:443", false},
		{"ipv6", map[string]string{"KUBERNETES_SERVICE_HOST": "fd00::1"}, "[fd00::1]:443", false},
		{"no host", map[string]string{"KUBERNETES_SERVICE_HOST": ""}, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := TargetsFor(TargetKubernetes, k8sEnv(tt.env))
			if (err != nil) != tt.wantErr {
				t.Fatalf("TargetsFor error = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && (len(got) != 1 || got[0].Addr != tt.want || got[0].Name != "kubernetes API") {
				t.Errorf("TargetsFor = %+v, want the kubernetes API at %s", got, tt.want)
			}
		})
	}
}

// TestTargetsForDNS: the dns target is the resolver address the controller
// passes in DNSEnv, dialled over TCP 53. Unset or anything but an IP
// literal on port 53 is refused rather than dropped, since a probe that
// silently skipped the resolver would read as a clean verdict.
func TestTargetsForDNS(t *testing.T) {
	tests := []struct {
		name    string
		dns     string
		want    string
		wantErr string
	}{
		{name: "ipv4", dns: "10.96.0.10:53", want: "10.96.0.10:53"},
		{name: "ipv6", dns: "[fd00::a]:53", want: "[fd00::a]:53"},
		{name: "unset", dns: "", wantErr: DNSEnv + " is unset"},
		{name: "no port", dns: "10.96.0.10", wantErr: "want <ip>:53"},
		{name: "another port", dns: "10.96.0.10:5353", wantErr: "want <ip>:53"},
		{name: "a name, not an address", dns: "kube-dns.kube-system:53", wantErr: "want <ip>:53"},
		{name: "empty host", dns: ":53", wantErr: "want <ip>:53"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := TargetsFor(TargetDNS, k8sEnv(map[string]string{DNSEnv: tt.dns}))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("TargetsFor error = %v, want one mentioning %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("TargetsFor: %v", err)
			}
			if len(got) != 1 || got[0].Addr != tt.want || got[0].Name != "cluster DNS" {
				t.Errorf("TargetsFor = %+v, want the cluster DNS at %s", got, tt.want)
			}
		})
	}
}

// TestDefaultTargetsUnchanged: the strict probe with no --targets, the one
// every repository-image build runs, never probes DNS even when the
// address is in the environment. Adding it would turn every build under the
// default DNS rule into ExitUnenforced.
func TestDefaultTargetsUnchanged(t *testing.T) {
	getenv := k8sEnv(map[string]string{DNSEnv: "10.96.0.10:53"})
	want := []string{"1.1.1.1:443", "172.20.0.1:443", "169.254.169.254:80"}
	if got := addrs(DefaultTargets(getenv)); !slices.Equal(got, want) {
		t.Errorf("DefaultTargets = %v, want %v", got, want)
	}
	p, mode, err := Parse(nil, getenv)
	if err != nil || mode != ModeStrict || !slices.Equal(addrs(p.Targets), want) {
		t.Errorf("Parse(no arguments) = %v %s %v, want strict over %v", addrs(p.Targets), mode, err, want)
	}
}

// TestStrictExitsUnenforcedWhenDNSOpen: the evaluator's strict probe, with
// every other target blocked, exits ExitUnenforced when only the cluster's
// resolver still answers on TCP 53, naming it in the verdict; and 0 once
// it is blocked too.
func TestStrictExitsUnenforcedWhenDNSOpen(t *testing.T) {
	getenv := k8sEnv(map[string]string{DNSEnv: "10.96.0.10:53"})
	for _, dnsOpen := range []bool{true, false} {
		p, mode, err := Parse([]string{"--mode=strict", "--targets=internet,k8s,imds,dns"}, getenv)
		if err != nil || mode != ModeStrict {
			t.Fatalf("Parse = %s, %v", mode, err)
		}
		clock := newClock()
		n := &fakeNet{targets: len(p.Targets),
			open: func(_ int, addr string) bool { return dnsOpen && addr == "10.96.0.10:53" }}
		p.Dial, p.Now, p.Sleep = n.dial, clock.Now, clock.Sleep
		var buf bytes.Buffer
		code := p.Exit(context.Background(), slog.New(slog.NewTextHandler(&buf, nil)))
		want := 0
		if dnsOpen {
			want = ExitUnenforced
		}
		if code != want {
			t.Errorf("dns open=%v: Exit = %d, want %d (log %q)", dnsOpen, code, want, buf.String())
		}
		if dnsOpen && !strings.Contains(buf.String(), "cluster DNS (10.96.0.10:53)") {
			t.Errorf("verdict %q does not name the cluster DNS", buf.String())
		}
	}
}

// waitLog runs the wait probe and returns its exit status and log.
func waitLog(ctx context.Context, p Prober) (int, string) {
	var buf bytes.Buffer
	code := p.Wait(ctx, slog.New(slog.NewTextHandler(&buf, nil)))
	return code, buf.String()
}

func waitProber(clock *fakeClock, open func(round int, addr string) bool) Prober {
	n := &fakeNet{targets: len(waitTargets), open: open}
	return Prober{Targets: waitTargets, Timeout: DefaultTimeout, Dial: n.dial, Now: clock.Now, Sleep: clock.Sleep}
}

// TestWaitExitsZero: the wait mode's three outcomes. Blocked from the start
// hands over after the confirming rounds; open and then blocked waits for
// the policy and hands over once it is confirmed; open to the end of the
// window hands over all the same, with one loud line saying so. None of
// them is ExitUnenforced.
func TestWaitExitsZero(t *testing.T) {
	tests := []struct {
		name       string
		open       func(round int, addr string) bool
		wantRounds int
		wantLevel  string
		wantLine   string
	}{
		{"clean hands over at once", func(int, string) bool { return false }, confirmRounds, "level=INFO",
			"egress blocked after 3 round(s) in 2s"},
		{"open then closed waits for the policy", func(round int, _ string) bool { return round <= 5 },
			5 + confirmRounds, "level=INFO", "egress blocked after 8 round(s) in 7s"},
		{"one target open forever is loud and hands over",
			func(_ int, addr string) bool { return addr == "169.254.169.254:80" }, 21, "level=ERROR",
			"SANDBOX NOT CONFIRMED: sandbox probe (wait): egress still open after 21 round(s) in 20s to " +
				"cloud metadata (169.254.169.254:80);"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clock := newClock()
			code, log := waitLog(context.Background(), waitProber(clock, tt.open))
			if code != 0 {
				t.Errorf("Wait = %d, want 0 (log %q)", code, log)
			}
			if got := len(clock.sleeps) + 1; got != tt.wantRounds {
				t.Errorf("rounds = %d, want %d", got, tt.wantRounds)
			}
			if strings.Count(log, "\n") != 1 || !strings.Contains(log, tt.wantLevel) ||
				!strings.Contains(log, tt.wantLine) {
				t.Errorf("log = %q, want one %s line saying %q", log, tt.wantLevel, tt.wantLine)
			}
		})
	}
}

// TestWaitInterrupted: a wait the pod's termination cut short reaches no
// verdict and says so; it is neither a hand-over nor the unenforced exit.
func TestWaitInterrupted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	code, log := waitLog(ctx, waitProber(newClock(), func(int, string) bool { return true }))
	if code != exitInterrupted || !strings.Contains(log, "sandbox probe interrupted") {
		t.Errorf("Wait = %d (log %q), want %d", code, log, exitInterrupted)
	}
}

// TestMainWaitNeverExitsUnenforced: through the subcommand's own entry
// point, the wait mode against targets open to the end of the window exits
// 0, where the strict probe against the same network exits ExitUnenforced.
// Main builds the production Prober with the real dialer, so the "API
// server" is a loopback listener that accepts every connection and the
// window is shrunk to keep the test to about one retry interval.
func TestMainWaitNeverExitsUnenforced(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback listener:", err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	host, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	getenv := k8sEnv(map[string]string{"KUBERNETES_SERVICE_HOST": host, "KUBERNETES_SERVICE_PORT": port,
		TimeoutEnv: "10ms"})
	var buf bytes.Buffer
	code := Main(context.Background(), []string{"--mode=wait", "--targets=k8s"}, getenv,
		slog.New(slog.NewTextHandler(&buf, nil)))
	if code != 0 || !strings.Contains(buf.String(), "SANDBOX NOT CONFIRMED") {
		t.Errorf("Main(wait) = %d, log %q; want 0 and the loud verdict", code, buf.String())
	}
	if code == ExitUnenforced {
		t.Error("the wait mode exited ExitUnenforced")
	}
}

// droppingNet is a network whose blocked targets drop the SYN, the way a
// NetworkPolicy enforced by the VPC CNI or Cilium does: each such dial
// costs the full dialTimeout on the fake clock. Targets dial concurrently,
// so a round costs one dialTimeout however many of them drop. An open
// target answers at once.
type droppingNet struct {
	mu         sync.Mutex
	clock      *fakeClock
	targets    int
	calls      int
	roundStart time.Time
	open       func(round int, addr string) bool
}

func (n *droppingNet) dial(_ context.Context, addr string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.calls%n.targets == 0 {
		n.roundStart = n.clock.now
	}
	round := n.calls/n.targets + 1
	n.calls++
	if n.open(round, addr) {
		return nil
	}
	if end := n.roundStart.Add(dialTimeout); end.After(n.clock.now) {
		n.clock.now = end
	}
	return errors.New("i/o timeout")
}

// TestWaitStartLatency measures what the wait probe adds to a default-image
// Job's start, on the fake clock with a dialer that costs what a real one
// would. These are the figures the isolation guide documents:
//
//   - policy already attached, rejecting: 2 s (three rounds, one second
//     apart, each failing at once);
//   - policy already attached, dropping: 11 s (three rounds of a 3 s dial
//     timeout, one second apart): the worst case on a cluster that
//     enforces, and the common one (EKS Auto Mode's VPC CNI drops);
//   - policy attaching 5 s late, dropping: the open seconds plus the 11 s;
//   - never enforced, one target dropped by the network and the other open:
//     the 20 s window plus at most one round, 23 s, after which the agent
//     starts anyway.
func TestWaitStartLatency(t *testing.T) {
	tests := []struct {
		name string
		drop bool
		open func(round int, addr string) bool
		want time.Duration
	}{
		{"rejecting policy", false, func(int, string) bool { return false }, 2 * time.Second},
		{"dropping policy", true, func(int, string) bool { return false }, 11 * time.Second},
		{"dropping policy attaching after 5 rounds", true, func(round int, _ string) bool { return round <= 5 },
			5*retryInterval + 11*time.Second},
		{"never enforced", true, func(_ int, addr string) bool { return addr == "172.20.0.1:443" },
			23 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clock := newClock()
			p := Prober{Targets: waitTargets, Timeout: DefaultTimeout, Now: clock.Now, Sleep: clock.Sleep}
			if tt.drop {
				p.Dial = (&droppingNet{clock: clock, targets: len(waitTargets), open: tt.open}).dial
			} else {
				p.Dial = (&fakeNet{targets: len(waitTargets), open: tt.open}).dial
			}
			start := clock.Now()
			if code, log := waitLog(context.Background(), p); code != 0 {
				t.Fatalf("Wait = %d, log %q", code, log)
			}
			if got := clock.Now().Sub(start); got != tt.want {
				t.Errorf("added start latency = %s, want %s", got, tt.want)
			}
			if limit := DefaultTimeout + dialTimeout; clock.Now().Sub(start) > limit {
				t.Errorf("waited %s, past the %s bound", clock.Now().Sub(start), limit)
			}
		})
	}
}

// TestWaitVerdictLeaksNothing: the verdict line is the fixed wording, the
// round count, the elapsed time and the targets' own names and addresses.
// No dial error (which could carry resolver or proxy detail) and nothing
// from the environment beyond the API server's address reaches the log.
func TestWaitVerdictLeaksNothing(t *testing.T) {
	for _, open := range []bool{false, true} {
		clock := newClock()
		p := waitProber(clock, func(int, string) bool { return open })
		p.Dial = func(context.Context, string) error {
			if open {
				return nil
			}
			return errors.New("dial tcp: s3cr3t-placeholder via proxy user:pass@10.0.0.9")
		}
		_, log := waitLog(context.Background(), p)
		for _, leak := range []string{"s3cr3t", "user:pass", "10.0.0.9", "proxy"} {
			if strings.Contains(log, leak) {
				t.Errorf("open=%v: log %q leaks %q", open, log, leak)
			}
		}
	}
	open := Result{Rounds: 21, Elapsed: 20 * time.Second, Open: waitTargets}
	want := "SANDBOX NOT CONFIRMED: sandbox probe (wait): egress still open after 21 round(s) in 20s to " +
		"kubernetes API (172.20.0.1:443), cloud metadata (169.254.169.254:80); the agent's NetworkPolicy has " +
		"not attached or is not enforced, and this default-image agent starts anyway"
	if got := open.WaitVerdict(); got != want {
		t.Errorf("WaitVerdict =\n%q\nwant\n%q", got, want)
	}
}
