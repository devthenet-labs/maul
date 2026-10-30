// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"fmt"
	"testing"
	"time"
)

func getenvOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestExtraTargets(t *testing.T) {
	for _, tc := range []struct {
		name  string
		env   map[string]string
		reach []string
		deny  []string
		ok    bool
	}{
		{"none is fine", nil, nil, nil, true},
		{"both", map[string]string{"PROBE_REACH": "tool-pod=10.0.0.5:8090", "PROBE_DENY": "a=10.0.0.6:8090,b=10.0.0.7:80"},
			[]string{"10.0.0.5:8090"}, []string{"10.0.0.6:8090", "10.0.0.7:80"}, true},
		{"a name is not an address", map[string]string{"PROBE_DENY": "a=svc.example:80"}, nil, nil, false},
		{"a bad port", map[string]string{"PROBE_DENY": "a=10.0.0.6:0"}, nil, nil, false},
		{"a bad name", map[string]string{"PROBE_REACH": "A_B=10.0.0.6:80"}, nil, nil, false},
		{"no equals sign", map[string]string{"PROBE_REACH": "10.0.0.6:80"}, nil, nil, false},
		{"an empty entry", map[string]string{"PROBE_REACH": "a=10.0.0.6:80,"}, nil, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reach, deny, ok := extraTargets(getenvOf(tc.env))
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if fmt.Sprint(addrs(reach)) != fmt.Sprint(tc.reach) || fmt.Sprint(addrs(deny)) != fmt.Sprint(tc.deny) {
				t.Errorf("got %v, %v; want %v, %v", addrs(reach), addrs(deny), tc.reach, tc.deny)
			}
		})
	}
}

func TestToolsTargets(t *testing.T) {
	full := map[string]string{"PROBE_CLUSTER_DNS": "10.43.0.10", "PROBE_KUBERNETES_API": "10.43.0.1"}
	reach, deny, ok := toolsTargetsFrom(getenvOf(full))
	if !ok || len(reach) != 0 {
		t.Fatalf("ok = %v, reach = %v", ok, reach)
	}
	want := []string{"169.254.169.254:80", "1.1.1.1:443", "10.43.0.10:53", "10.43.0.1:443"}
	if fmt.Sprint(addrs(deny)) != fmt.Sprint(want) {
		t.Errorf("deny = %v, want %v", addrs(deny), want)
	}
	// A tool pod's DNS and API server targets are required: one missing is
	// inconclusive, never a pass without it.
	for _, drop := range []string{"PROBE_CLUSTER_DNS", "PROBE_KUBERNETES_API"} {
		env := map[string]string{}
		for k, v := range full {
			if k != drop {
				env[k] = v
			}
		}
		if _, _, ok := toolsTargetsFrom(getenvOf(env)); ok {
			t.Errorf("without %s: ok", drop)
		}
	}
}

func TestPeersTargetsNeedAControlAndADenyTarget(t *testing.T) {
	// Without a reached control a refusal proves nothing; without a deny
	// target there is nothing to prove.
	for _, env := range []map[string]string{
		nil, {"PROBE_REACH": "dns=10.0.0.2:53"}, {"PROBE_DENY": "tool=10.0.0.5:8090"},
	} {
		if _, _, ok := peersTargetsFrom(getenvOf(env)); ok {
			t.Errorf("accepted %v", env)
		}
	}
	reach, deny, ok := peersTargetsFrom(getenvOf(map[string]string{
		"PROBE_REACH": "dns=10.0.0.2:53", "PROBE_DENY": "tool=10.0.0.5:8090"}))
	if !ok || fmt.Sprint(addrs(reach)) != "[10.0.0.2:53]" || fmt.Sprint(addrs(deny)) != "[10.0.0.5:8090]" {
		t.Errorf("reach = %v, deny = %v, ok = %v", addrs(reach), addrs(deny), ok)
	}
}

func TestEvaluatorTargetsTakeTheExtras(t *testing.T) {
	env := map[string]string{
		"PROBE_EGRESS_BROKER": "10.1.0.1", "PROBE_KUBERNETES_API": "10.1.0.2", "PROBE_CLUSTER_DNS": "10.1.0.3",
		"PROBE_SOURCE_CONTROLLER": "10.1.0.4", "PROBE_SLOT_COMPONENT": "10.1.0.5",
		"PROBE_REACH": "own-tool-pod=10.1.0.6:8090", "PROBE_DENY": "other-tool-pod=10.1.0.7:8090",
	}
	reach, deny, ok := evaluatorTargetsFrom(getenvOf(env))
	if !ok {
		t.Fatal("not ok")
	}
	if fmt.Sprint(addrs(reach)) != "[10.1.0.1:8080 10.1.0.6:8090]" {
		t.Errorf("reach = %v", addrs(reach))
	}
	if got := addrs(deny); got[len(got)-1] != "10.1.0.7:8090" {
		t.Errorf("deny = %v", got)
	}
}

func TestAsModeRoles(t *testing.T) {
	for _, args := range [][]string{
		{"--as", "tools", "x"}, {"--as", "sandbox", refusedIsBlocked}, {"--as", "page", "x"}, {"--as", "peers", "x"},
	} {
		if handled, code := asMode(args); !handled || code != 2 {
			t.Errorf("asMode(%q) = %v, %d; want a usage error", args, handled, code)
		}
	}
	// Without their sites these runs are inconclusive, never a pass.
	t.Setenv("PROBE_CLUSTER_DNS", "")
	t.Setenv("PROBE_KUBERNETES_API", "")
	t.Setenv("PROBE_POD_IP", "")
	t.Setenv("PROBE_DENY", "")
	for _, role := range []string{"tools", "sandbox", "peers", "page"} {
		if role == "page" {
			continue // page's defaults need no site; runPage is tested directly
		}
		if handled, code := asMode([]string{"--as", role}); !handled || code != 3 {
			t.Errorf("asMode(%s) without a site = %v, %d; want inconclusive (3)", role, handled, code)
		}
	}
}

func TestRunPeers(t *testing.T) {
	// The component's row: a refusal counts only against a reached control.
	control := listening(t, "control")
	up := listening(t, "tool-pod")
	down := closed(t, "tool-pod")
	emitTo := func(result) {}
	out, code := runEvaluator([]target{control}, []target{down}, true, time.Second, emitTo)
	if out != outcomePass || code != 0 {
		t.Fatalf("refused deny = %s, %d", out, code)
	}
	out, code = runEvaluator([]target{control}, []target{up}, true, time.Second, emitTo)
	if out != outcomeSecurityFailure || code != 2 {
		t.Fatalf("answering deny = %s, %d", out, code)
	}
}

func addrs(ts []target) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.addr)
	}
	return out
}
