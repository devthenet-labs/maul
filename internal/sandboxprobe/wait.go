// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package sandboxprobe

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// Mode is how the probe acts on its verdict.
type Mode string

const (
	// ModeStrict is the repository-image probe: every default target,
	// ExitUnenforced when one is still open as the window closes. It is
	// what the subcommand runs with no arguments at all.
	ModeStrict Mode = "strict"
	// ModeWait is the default-image probe: it holds the prepare init until
	// its targets have been blocked for the confirming rounds, so the agent
	// does not start inside the window before a late-attaching
	// NetworkPolicy applies. When the window closes with a target still
	// open it logs one loud verdict line and exits 0 all the same: a
	// default-image run never depended on the probe, and its breaker and
	// attempt semantics stay exactly as they were.
	ModeWait Mode = "wait"
)

// The target keys --targets takes.
const (
	// TargetInternet is the public address 1.1.1.1:443. A chart posture
	// that keeps TCP 443 to anywhere (broadEgress) opens it on purpose, so
	// only the strict probe of a repository-image Job, whose posture forbids
	// that rule, checks it.
	TargetInternet = "internet"
	// TargetKubernetes is the API server's ClusterIP, from the
	// KUBERNETES_SERVICE_HOST/PORT the kubelet injects into every container.
	TargetKubernetes = "k8s"
	// TargetMetadata is the cloud metadata service, 169.254.169.254:80.
	TargetMetadata = "imds"
	// TargetDNS is the cluster's resolver on TCP 53, at the address the
	// controller passes in DNSEnv. Only the evaluator's strict probe names
	// it; DefaultTargets never does, since a repository-image build under
	// the default DNS rule reaches the resolver on purpose.
	TargetDNS = "dns"
)

// DNSEnv names the environment variable the dns target's address comes
// from, as <ip>:53: the controller's own resolver, because under dns: none
// the pod's own resolver configuration names nothing to probe.
const DNSEnv = "MAUL_SANDBOX_PROBE_DNS"

// Targets by key, the public ones fixed. The Kubernetes API's address comes
// from the environment (kubernetesTarget).
var (
	internetTarget = Target{Name: "public internet", Addr: "1.1.1.1:443"}
	metadataTarget = Target{Name: "cloud metadata", Addr: "169.254.169.254:80"}
)

// kubernetesTarget is the API server as the kubelet announced it, and false
// when it did not.
func kubernetesTarget(getenv func(string) string) (Target, bool) {
	host := getenv("KUBERNETES_SERVICE_HOST")
	if host == "" {
		return Target{}, false
	}
	port := getenv("KUBERNETES_SERVICE_PORT")
	if port == "" {
		port = "443"
	}
	return Target{Name: "kubernetes API", Addr: net.JoinHostPort(host, port)}, true
}

// dnsTarget is the cluster resolver DNSEnv names. Unset, or anything but
// an IP literal on port 53, is an error: the value is controller-written,
// and a probe of the wrong address would read as a clean verdict.
func dnsTarget(getenv func(string) string) (Target, error) {
	raw := getenv(DNSEnv)
	if raw == "" {
		return Target{}, fmt.Errorf("%s is unset; the probe cannot check the cluster DNS", DNSEnv)
	}
	host, port, err := net.SplitHostPort(raw)
	if err != nil || port != "53" || net.ParseIP(host) == nil {
		return Target{}, fmt.Errorf("%s=%q: want <ip>:53", DNSEnv, raw)
	}
	return Target{Name: "cluster DNS", Addr: net.JoinHostPort(host, port)}, nil
}

// TargetsFor resolves a comma-separated --targets value into the targets
// it names, in the order given. An unknown or repeated key, an empty list,
// and k8s without KUBERNETES_SERVICE_HOST are errors: the value is
// controller-written, so each means a bug, and a probe that silently
// checked less than it was asked to would read as a clean verdict. So is
// dns without a valid DNSEnv (dnsTarget).
func TargetsFor(keys string, getenv func(string) string) ([]Target, error) {
	if strings.TrimSpace(keys) == "" {
		return nil, errors.New("--targets names no target")
	}
	seen := map[string]bool{}
	var targets []Target
	for key := range strings.SplitSeq(keys, ",") {
		key = strings.TrimSpace(key)
		if seen[key] {
			return nil, fmt.Errorf("--targets names %q twice", key)
		}
		seen[key] = true
		switch key {
		case TargetInternet:
			targets = append(targets, internetTarget)
		case TargetKubernetes:
			t, ok := kubernetesTarget(getenv)
			if !ok {
				return nil, errors.New("KUBERNETES_SERVICE_HOST is unset; the probe cannot check the Kubernetes API")
			}
			targets = append(targets, t)
		case TargetMetadata:
			targets = append(targets, metadataTarget)
		case TargetDNS:
			t, err := dnsTarget(getenv)
			if err != nil {
				return nil, err
			}
			targets = append(targets, t)
		default:
			return nil, fmt.Errorf("--targets: unknown target %q (want %s, %s, %s or %s)",
				key, TargetInternet, TargetKubernetes, TargetMetadata, TargetDNS)
		}
	}
	return targets, nil
}

// Parse reads the subcommand's arguments and environment into a Prober and
// the mode it runs in. No arguments is the strict probe, exactly as
// FromEnv builds it, so a repository-image Job's bare `sandbox-probe` keeps
// its meaning. --targets defaults to DefaultTargets in the strict mode and
// to k8s,imds in the wait mode; named, it replaces them in either (the
// evaluator's strict probe adds dns). Both forms of a flag are accepted
// (--mode wait and --mode=wait); a positional argument is an error.
func Parse(args []string, getenv func(string) string) (Prober, Mode, error) {
	fs := flag.NewFlagSet(Command, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	mode := fs.String("mode", string(ModeStrict), "strict or wait")
	keys := fs.String("targets", "", "comma-separated targets")
	if err := fs.Parse(args); err != nil {
		return Prober{}, "", err
	}
	if fs.NArg() > 0 {
		return Prober{}, "", fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	targetsSet := false
	fs.Visit(func(f *flag.Flag) { targetsSet = targetsSet || f.Name == "targets" })

	switch Mode(*mode) {
	case ModeStrict:
		if !targetsSet {
			p, err := FromEnv(getenv)
			return p, ModeStrict, err
		}
	case ModeWait:
		if !targetsSet {
			*keys = TargetKubernetes + "," + TargetMetadata
		}
	default:
		return Prober{}, "", fmt.Errorf("--mode %q: want %s or %s", *mode, ModeStrict, ModeWait)
	}
	targets, err := TargetsFor(*keys, getenv)
	if err != nil {
		return Prober{}, "", err
	}
	timeout, err := timeoutFromEnv(getenv)
	if err != nil {
		return Prober{}, "", err
	}
	return Prober{Targets: targets, Timeout: timeout}, Mode(*mode), nil
}

// WaitVerdict is the one line the wait mode prints. It names only the
// targets (fixed addresses and the API server's ClusterIP, which every
// container is told), the round count and the elapsed time: no dial error,
// no environment value, nothing from the workload.
func (r Result) WaitVerdict() string {
	if r.Enforced {
		return fmt.Sprintf("sandbox probe (wait): egress blocked after %d round(s) in %s; "+
			"the agent's NetworkPolicy is in place", r.Rounds, r.Elapsed.Round(time.Millisecond))
	}
	names := make([]string, 0, len(r.Open))
	for _, t := range r.Open {
		names = append(names, t.String())
	}
	return fmt.Sprintf("SANDBOX NOT CONFIRMED: sandbox probe (wait): egress still open after %d round(s) in %s "+
		"to %s; the agent's NetworkPolicy has not attached or is not enforced, and this default-image agent "+
		"starts anyway", r.Rounds, r.Elapsed.Round(time.Millisecond), strings.Join(names, ", "))
}
