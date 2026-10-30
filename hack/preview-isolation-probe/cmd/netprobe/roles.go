// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"fmt"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The evaluator-tool-pod matrix (DESIGN 4.2; spec S1-25). The same binary is
// exec'd into each vantage point of a live evaluation and told which
// addresses that vantage must reach (a control, so a refusal means policy and
// not a listener that is down) and which it must not:
//
//	--as evaluator  the evaluator pod: the broker and its own tool pod
//	--as tools      the tool pod's `tools` container: no DNS, API, IMDS or internet
//	--as peers      any other vantage (a slot component): PROBE_REACH / PROBE_DENY only
//	--as sandbox    the tool pod's `sandbox` container: HTTP refusals (sandbox.go)
//	--as page       a page served in the pod, fetched by its browser (page.go)
const (
	toolsRole   = "tools"
	peersRole   = "peers"
	sandboxRole = "sandbox"
	pageRole    = "page"
)

var specName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// parseSpecs splits "name=value,name=value" into pairs; false when any entry
// is malformed, so a typo makes a run inconclusive and never a smaller one.
func parseSpecs(s string) (pairs [][2]string, ok bool) {
	if s == "" {
		return nil, true
	}
	for entry := range strings.SplitSeq(s, ",") {
		name, value, found := strings.Cut(entry, "=")
		if !found || !specName.MatchString(name) || value == "" {
			return nil, false
		}
		pairs = append(pairs, [2]string{name, value})
	}
	return pairs, true
}

// ipv4Addr is addr when it is <ipv4>:<port> with a port in 1..65535.
func ipv4Addr(addr string) bool {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	p, err := strconv.Atoi(port)
	return ip != nil && ip.To4() != nil && err == nil && p >= 1 && p <= 65535
}

func addrTargets(s string) ([]target, bool) {
	pairs, ok := parseSpecs(s)
	if !ok {
		return nil, false
	}
	var out []target
	for _, p := range pairs {
		if !ipv4Addr(p[1]) {
			return nil, false
		}
		out = append(out, target{name: p[0], addr: p[1]})
	}
	return out, true
}

// extraTargets are PROBE_REACH and PROBE_DENY, "name=<ipv4>:<port>" lists the
// script adds for the run's own addresses (a tool pod, another evaluation's
// tool pod, a slot component).
func extraTargets(getenv func(string) string) (reach, deny []target, ok bool) {
	reach, ok = addrTargets(getenv("PROBE_REACH"))
	if !ok {
		return nil, nil, false
	}
	deny, ok = addrTargets(getenv("PROBE_DENY"))
	if !ok {
		return nil, nil, false
	}
	return reach, deny, true
}

// toolsTargetsFrom is the tool pod's row: no DNS, API server, instance
// metadata service or internet. Its components on declared edges are
// PROBE_REACH.
func toolsTargetsFrom(getenv func(string) string) (reach, deny []target, ok bool) {
	deny = []target{
		{name: "imds-v2-token-put", addr: "169.254.169.254:80", imds: true},
		{name: "internet", addr: "1.1.1.1:443"},
	}
	for _, item := range []envTarget{
		{"cluster-dns", "PROBE_CLUSTER_DNS", "53"},
		{"kubernetes-api", "PROBE_KUBERNETES_API", "443"},
	} {
		t, ok := envIPv4(getenv, item)
		if !ok {
			return nil, nil, false
		}
		deny = append(deny, t)
	}
	reach, extra, ok := extraTargets(getenv)
	return reach, append(deny, extra...), ok
}

// peersTargetsFrom needs a control to reach and something to deny.
func peersTargetsFrom(getenv func(string) string) (reach, deny []target, ok bool) {
	reach, deny, ok = extraTargets(getenv)
	if !ok || len(reach) == 0 || len(deny) == 0 {
		return nil, nil, false
	}
	return reach, deny, true
}

// runRole runs the egress roles that share runEvaluator.
func runRole(role string, refusedBlocks bool) int {
	targetsFor := map[string]func(func(string) string) ([]target, []target, bool){
		evaluatorRole: evaluatorTargetsFrom, toolsRole: toolsTargetsFrom, peersRole: peersTargetsFrom,
	}[role]
	reach, deny, ok := targetsFor(os.Getenv)
	if !ok {
		emit(result{Name: "summary", Outcome: outcomeInconclusive})
		return 3
	}
	_, code := runEvaluator(reach, deny, refusedBlocks, evaluatorReachWithin, emit)
	return code
}

// pageWait is how long the page run waits for the browser's report.
func pageWait(getenv func(string) string) time.Duration {
	if n, err := strconv.Atoi(getenv("PROBE_PAGE_WAIT_SECONDS")); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return 90 * time.Second
}

func usageAs() int {
	fmt.Fprintln(os.Stderr, "usage: probe --as {"+evaluatorRole+"|"+toolsRole+"|"+peersRole+"} ["+refusedIsBlocked+
		"] | --as {"+sandboxRole+"|"+pageRole+"}")
	return 2
}
