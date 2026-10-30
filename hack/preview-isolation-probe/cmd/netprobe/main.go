// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// netprobe is a one-shot cold-start network-isolation test. It has no
// listener, except as the ingress-direction run's slot Pod (ingress.go:
// `listen`, and `connect` for its clients).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"sync"
	"syscall"
	"time"
)

type target struct {
	name string
	addr string
	imds bool
	// refusedBlocks counts ECONNREFUSED as blocked (outcomeRefused): only
	// the ingress run's connect --refused-is-blocked sets it.
	refusedBlocks bool
}

type result struct {
	Name      string `json:"name"`
	Worker    int    `json:"worker,omitempty"`
	Attempt   int    `json:"attempt,omitempty"`
	ElapsedMS int64  `json:"elapsed_ms"`
	Outcome   string `json:"outcome"`
	Status    int    `json:"status,omitempty"`
}

type envTarget struct{ name, key, port string }

// requiredTargets are always probed; siblingTargets are probed together or
// not at all. A multi-component Preview runs several components in one slot,
// so the slot policy must also keep a component from its sibling's Pod and
// Service, and from a Pod in another slot: a page reaches a sibling only from
// the browser, through the load balancer.
var (
	requiredTargets = []envTarget{
		{"kubernetes-api", "PROBE_KUBERNETES_API", "443"},
		{"maul-egress-broker", "PROBE_EGRESS_BROKER", "8080"},
		{"maul-integration-controller", "PROBE_INTEGRATION_CONTROLLER", "8080"},
		{"maul-source-controller", "PROBE_SOURCE_CONTROLLER", "9790"},
		{"maul-status-server", "PROBE_STATUS_SERVER", "8080"},
	}
	siblingTargets = []envTarget{
		{"same-slot-sibling-pod", "PROBE_SIBLING_POD", "8080"},
		{"same-slot-sibling-service", "PROBE_SIBLING_SERVICE", "80"},
		{"other-slot-pod", "PROBE_OTHER_SLOT_POD", "8080"},
	}
)

// The script discovers these service IPs immediately before deployment and
// passes them as ordinary, non-secret environment variables. Missing values
// make the test inconclusive, not a misleading pass. Link-local and internet
// targets are constant. No target is ever accessed by hostname.
func targets() ([]target, bool) { return targetsFrom(os.Getenv) }

func targetsFrom(getenv func(string) string) ([]target, bool) {
	out := []target{
		{name: "imds-v2-token-put", addr: "169.254.169.254:80", imds: true},
		{name: "pod-identity-agent", addr: "169.254.170.23:80"},
		{name: "internet", addr: "1.1.1.1:443"},
	}
	ipv4 := func(item envTarget) (target, bool) { return envIPv4(getenv, item) }
	for _, item := range requiredTargets {
		t, ok := ipv4(item)
		if !ok {
			return nil, false
		}
		out = append(out, t)
	}
	// The sibling set is all or nothing: one silently missing would drop a
	// forbidden target from a run that then reports a pass.
	given := 0
	for _, item := range siblingTargets {
		if getenv(item.key) != "" {
			given++
		}
	}
	if given == 0 {
		return out, true
	}
	if given != len(siblingTargets) {
		return nil, false
	}
	for _, item := range siblingTargets {
		t, ok := ipv4(item)
		if !ok {
			return nil, false
		}
		out = append(out, t)
	}
	return out, true
}

// envIPv4 is item's target at the IPv4 address its variable holds; false
// when it holds anything else, a name included.
func envIPv4(getenv func(string) string, item envTarget) (target, bool) {
	ip := net.ParseIP(getenv(item.key))
	if ip == nil || ip.To4() == nil {
		return target{}, false
	}
	return target{name: item.name, addr: net.JoinHostPort(ip.String(), item.port)}, true
}

// The evaluator run (`--as evaluator`, run.sh evaluator): the agent
// evaluator's row of the security model (DESIGN 4.2), probed from a Pod
// labelled as an evaluator is. It must reach the egress broker, and nothing
// else: not the internet, the cluster's resolver (it runs with no DNS, and
// kube-dns :53 must be closed as well), the API server, the instance
// metadata service, the artifact server or a slot component. Its own tool
// pod on :8090 is S1's to probe.
const (
	evaluatorRole = "evaluator"

	outcomePass            = "PASS"
	outcomeSecurityFailure = "SECURITY_FAILURE"
	outcomeNotReached      = "NOT_REACHED"

	// evaluatorReachWithin bounds the wait for the broker: a policy
	// controller programs a new Pod's policy a moment after it starts.
	evaluatorReachWithin = time.Minute
)

var (
	evaluatorReach = []envTarget{
		{"maul-egress-broker", "PROBE_EGRESS_BROKER", "8080"},
	}
	evaluatorDeny = []envTarget{
		{"kubernetes-api", "PROBE_KUBERNETES_API", "443"},
		{"cluster-dns", "PROBE_CLUSTER_DNS", "53"},
		{"maul-source-controller", "PROBE_SOURCE_CONTROLLER", "9790"}, // the artifact server
		{"slot-component", "PROBE_SLOT_COMPONENT", "8080"},
	}
)

// evaluatorTargetsFrom is the evaluator run's targets: those it must reach
// and those it must not. Every address is required; one missing makes the
// run inconclusive, never a pass without that target.
func evaluatorTargetsFrom(getenv func(string) string) (reach, deny []target, ok bool) {
	deny = []target{
		{name: "imds-v2-token-put", addr: "169.254.169.254:80", imds: true},
		{name: "internet", addr: "1.1.1.1:443"},
	}
	for _, item := range evaluatorReach {
		t, ok := envIPv4(getenv, item)
		if !ok {
			return nil, nil, false
		}
		reach = append(reach, t)
	}
	for _, item := range evaluatorDeny {
		t, ok := envIPv4(getenv, item)
		if !ok {
			return nil, nil, false
		}
		deny = append(deny, t)
	}
	// Its own tool pod (reach) and another evaluation's, another slot's
	// (deny): the matrix run's addresses (roles.go).
	extraReach, extraDeny, ok := extraTargets(getenv)
	if !ok {
		return nil, nil, false
	}
	return append(reach, extraReach...), append(deny, extraDeny...), true
}

// asMode runs `--as <role> [--refused-is-blocked]`; false when args do not
// start with --as, so main runs another mode.
func asMode(args []string) (handled bool, code int) {
	if len(args) == 0 || args[0] != "--as" {
		return false, 0
	}
	rest := args[1:]
	if len(rest) == 0 {
		return true, usageAs()
	}
	role, flags := rest[0], rest[1:]
	switch role {
	case evaluatorRole, toolsRole, peersRole:
		if len(flags) > 1 || (len(flags) == 1 && flags[0] != refusedIsBlocked) {
			return true, usageAs()
		}
		return true, runRole(role, len(flags) == 1)
	case sandboxRole:
		if len(flags) != 0 {
			return true, usageAs()
		}
		specs, ok := sandboxSpecsFrom(os.Getenv)
		if !ok {
			emit(result{Name: "summary", Outcome: outcomeInconclusive})
			return true, 3
		}
		_, code = runSandbox(specs, httpWithin, emit)
		return true, code
	case pageRole:
		if len(flags) != 0 {
			return true, usageAs()
		}
		return true, pageMode()
	}
	return true, usageAs()
}

// runEvaluator waits up to within for every reach target to connect, and
// only then bursts at the deny targets: before its policy is programmed a
// Pod's connections may be refused for no reason of policy (kube-router
// REJECTs them), so a refusal then would prove nothing. refusedBlocks counts
// a refusal as blocked (k3s, k3d), sound once the reached broker shows the
// policy is in force and the targets are known to answer.
func runEvaluator(reach, deny []target, refusedBlocks bool, within time.Duration,
	emit func(result)) (string, int) {
	start := time.Now()
	finish := func(outcome string, code int) (string, int) {
		emit(result{Name: "summary", Outcome: outcome, ElapsedMS: time.Since(start).Milliseconds()})
		return outcome, code
	}
	for _, t := range reach {
		outcome := awaitReach(t, start.Add(within))
		emit(result{Name: t.name, Outcome: outcome, ElapsedMS: time.Since(start).Milliseconds()})
		if outcome != outcomeReached {
			return finish(outcomeNotReached, 4)
		}
	}
	for i := range deny {
		deny[i].refusedBlocks = refusedBlocks
	}
	reachable, inconclusive := burst(start, deny, emit)
	switch {
	case reachable:
		return finish(outcomeSecurityFailure, 2)
	case inconclusive:
		return finish(outcomeInconclusive, 3)
	}
	return finish(outcomePass, 0)
}

// awaitReach dials t until it connects or deadline passes.
func awaitReach(t target, deadline time.Time) string {
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
		conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", t.addr)
		cancel()
		if err == nil {
			_ = conn.Close()
			return outcomeReached
		}
		if !time.Now().Add(250 * time.Millisecond).Before(deadline) {
			return outcomeNotReached
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func classify(err error) string {
	if err == nil {
		return "REACHABLE"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "blocked"
	}
	if errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
		return "blocked"
	}
	// ECONNREFUSED is not evidence of policy: the listener might be down.
	return "inconclusive"
}

// connectOutcome is a TCP attempt's outcome: classify's, except that a
// refusal is outcomeRefused when refusedBlocks says the listener is known to
// answer, so only the policy can have refused it.
func connectOutcome(err error, refusedBlocks bool) string {
	if refusedBlocks && errors.Is(err, syscall.ECONNREFUSED) {
		return outcomeRefused
	}
	return classify(err)
}

func probe(ctx context.Context, dialer *net.Dialer, t target, worker, attempt int, start time.Time) result {
	ctx, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
	defer cancel()
	r := result{Name: t.name, Worker: worker, Attempt: attempt}
	if t.imds {
		status, connected, err := imdsTokenPUT(ctx, dialer, t.addr)
		r.Status = status
		if connected {
			r.Outcome = "REACHABLE"
		} else {
			r.Outcome = connectOutcome(err, t.refusedBlocks)
		}
	} else {
		conn, err := dialer.DialContext(ctx, "tcp", t.addr)
		if conn != nil {
			_ = conn.Close()
		}
		r.Outcome = connectOutcome(err, t.refusedBlocks)
	}
	r.ElapsedMS = time.Since(start).Milliseconds()
	return r
}

func burst(start time.Time, targets []target, emit func(result)) (reachable, inconclusive bool) {
	const workers, attempts = 4, 4
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	begin := make(chan struct{})
	results := make(chan result, len(targets)*workers*attempts)
	var wg sync.WaitGroup
	for _, t := range targets {
		for worker := 1; worker <= workers; worker++ {
			wg.Add(1)
			go func(t target, worker int) {
				defer wg.Done()
				<-begin
				for attempt := 1; attempt <= attempts; attempt++ {
					if ctx.Err() != nil {
						return
					}
					r := probe(ctx, &net.Dialer{}, t, worker, attempt, start)
					results <- r
					if r.Outcome == "REACHABLE" {
						return
					}
				}
			}(t, worker)
		}
	}
	// The first network-related operation of main happens at this release.
	close(begin)
	go func() { wg.Wait(); close(results) }()
	for r := range results {
		emit(r)
		switch r.Outcome {
		case "REACHABLE":
			reachable = true
			cancel()
		case "inconclusive":
			inconclusive = true
		}
	}
	return reachable, inconclusive
}

func emit(r result) { _ = json.NewEncoder(os.Stdout).Encode(r) }

func main() {
	if handled, code := ingressMode(os.Args[1:]); handled {
		os.Exit(code)
	}
	if handled, code := asMode(os.Args[1:]); handled {
		os.Exit(code)
	}
	start := time.Now()
	all, ok := targets()
	if !ok {
		emit(result{Name: "summary", Outcome: "INCONCLUSIVE", ElapsedMS: time.Since(start).Milliseconds()})
		os.Exit(3)
	}
	reachable, inconclusive := burst(start, all, emit)
	if reachable || inconclusive {
		outcome, code := "SECURITY_FAILURE", 2
		if !reachable {
			outcome, code = "INCONCLUSIVE", 3
		}
		emit(result{Name: "summary", Outcome: outcome, ElapsedMS: time.Since(start).Milliseconds()})
		time.Sleep(time.Minute) // observer has time to delete the Deployment
		os.Exit(code)
	}
	// DNS may become available after policy programming. Forbidden targets
	// are never retried after the initial burst.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := net.DefaultResolver.LookupHost(ctx, "kubernetes.default.svc.cluster.local")
		cancel()
		if err == nil {
			emit(result{Name: "dns", Outcome: "ok", ElapsedMS: time.Since(start).Milliseconds()})
			emit(result{Name: "summary", Outcome: "PASS", ElapsedMS: time.Since(start).Milliseconds()})
			time.Sleep(time.Minute)
			return
		}
		time.Sleep(time.Second)
	}
	emit(result{Name: "summary", Outcome: "DNS_FAILURE", ElapsedMS: time.Since(start).Milliseconds()})
	time.Sleep(time.Minute)
	os.Exit(4)
}
