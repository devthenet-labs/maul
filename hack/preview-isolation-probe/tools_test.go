// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package probe_test

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// toolsStub stands in for kubectl in run.sh's tools run. It logs each call's
// argv (after --request-timeout), answers every Service and Pod with the
// address STUB_SVC_<name> / STUB_POD_<name> holds, and answers a probe
// container's log with STUB_LOG_<row> (the row is the container name's
// middle part), by default a passing summary.
const toolsStub = `#!/bin/sh
case "$1" in --request-timeout=*) shift ;; esac
{ printf 'kubectl'; for arg in "$@"; do printf '\t%s' "$arg"; done; printf '\n'; } >>"$STUB_LOG"
var() { printf '%s' "$1" | tr -- '-' '_'; }
case "$1" in
get)
    case "$2" in service) kind=SVC ;; *) kind=POD ;; esac
    eval "printf '%s' \"\${STUB_${kind}_$(var "$3")}\"" ;;
debug)
    for a in "$@"; do case "$a" in probe-*) container=$a ;; esac; done
    row=${container#probe-}; row=${row%-*}
    eval "fail=\${STUB_DEBUG_FAIL_$(var "$row")}"
    exit "${fail:-0}" ;;
exec) ;;
logs)
    for a in "$@"; do case "$a" in probe-*) container=$a ;; esac; done
    row=${container#probe-}; row=${row%-*}
    eval "log=\${STUB_LOG_$(var "$row")}"
    if [ -z "$log" ]; then log='{"name":"x","outcome":"REACHED"}
{"name":"summary","outcome":"PASS"}'; fi
    printf '%s\n' "$log" ;;
*) exit 1 ;;
esac
`

var toolsAddrs = map[string]string{
	"SVC_maul_egress_broker": "10.43.1.2", "SVC_kubernetes": "10.43.0.1", "SVC_kube_dns": "10.43.0.10",
	"SVC_maul_source_controller": "10.43.1.4",
	"POD_tool":                   "10.42.0.5", "POD_evaluator": "10.42.1.6", "POD_component": "10.42.0.7",
	"POD_other_tool": "10.42.2.8", "POD_other_slot": "10.42.3.9", "POD_agents_tool": "10.42.1.10",
}

func toolsArgs(extra ...string) []string {
	return slices.Concat([]string{"--image", edgesImage, "--timeout", "2",
		"--tool-pod", "maul-preview-0/tool", "--evaluator-pod", "maul-agents/evaluator",
		"--component-pod", "maul-preview-0/component", "--other-tool-pod", "maul-preview-1/other-tool",
		"--other-slot-pod", "maul-preview-1/other-slot"}, extra)
}

func runToolsProbe(t *testing.T, extra []string, args ...string) edgesRun {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not on PATH")
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH; run.sh requires it")
	}
	bin, dir := t.TempDir(), t.TempDir()
	for name, script := range map[string]string{"kubectl": toolsStub, "gh": failStub, "aws": failStub} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	log := filepath.Join(dir, "calls")
	env := []string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"), "STUB_LOG=" + log}
	for name, ip := range toolsAddrs {
		env = append(env, "STUB_"+name+"="+ip)
	}
	env = append(env, extra...)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "PATH=") && !strings.HasPrefix(kv, "PROBE_") && !strings.HasPrefix(kv, "STUB_") {
			env = append(env, kv)
		}
	}
	cmd := exec.Command(bash, append([]string{"run.sh", "tools"}, args...)...)
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	var run edgesRun
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("run.sh tools: %v", err)
		}
		run.code = exit.ExitCode()
	}
	run.stdout, run.stderr = stdout.String(), stderr.String()
	if b, err := os.ReadFile(log); err == nil {
		for line := range strings.SplitSeq(strings.TrimSuffix(string(b), "\n"), "\n") {
			if line != "" {
				run.calls = append(run.calls, strings.Split(line, "\t"))
			}
		}
	}
	return run
}

// debugCalls are the ephemeral-container calls, by the role they run.
func debugCalls(run edgesRun) map[string][]string {
	out := map[string][]string{}
	for _, c := range run.calls {
		if len(c) > 1 && c[1] == "debug" {
			if i := slices.Index(c, "--as"); i >= 0 {
				out[c[i+1]+"@"+c[2]] = c
			}
		}
	}
	return out
}

func TestToolsRunsEveryRow(t *testing.T) {
	run := runToolsProbe(t, nil, toolsArgs("--agents-tool-pod", "maul-agents/agents-tool")...)
	if run.code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", run.code, run.stderr)
	}
	calls := debugCalls(run)
	for _, want := range []string{
		"evaluator@evaluator", "peers@component", "tools@tool", "sandbox@tool", "page@tool", "tools@agents-tool",
	} {
		if _, ok := calls[want]; !ok {
			t.Errorf("no ephemeral container for %s; have %v", want, slices.Collect(mapsKeys(calls)))
		}
	}
	if len(calls) != 6 {
		t.Errorf("%d probe containers, want 6", len(calls))
	}
	ev := strings.Join(calls["evaluator@evaluator"], " ")
	for _, want := range []string{
		"-n maul-agents", "--env=PROBE_REACH=own-tool-pod=10.42.0.5:8090",
		"--env=PROBE_DENY=other-tool-pod=10.42.2.8:8090;other-slot-pod=10.42.3.9:8080",
		"--env=PROBE_EGRESS_BROKER=10.43.1.2", "--env=PROBE_SLOT_COMPONENT=10.42.0.7", "--profile=restricted",
	} {
		if !strings.Contains(ev, want) {
			t.Errorf("evaluator row lacks %q in %s", want, ev)
		}
	}
	peers := strings.Join(calls["peers@component"], " ")
	for _, want := range []string{
		"--env=PROBE_REACH=cluster-dns=10.43.0.10:53",
		"tool-pod=10.42.0.5:8090;evaluator=10.42.1.6:8080;maul-egress-broker=10.43.1.2:8080",
	} {
		if !strings.Contains(peers, want) {
			t.Errorf("component row lacks %q in %s", want, peers)
		}
	}
	if sb := strings.Join(calls["sandbox@tool"], " "); !strings.Contains(sb, "--env=PROBE_POD_IP=10.42.0.5") {
		t.Errorf("sandbox row: %s", sb)
	}
	agents := strings.Join(calls["tools@agents-tool"], " ")
	if !strings.Contains(agents, "PROBE_REACH=artifact-server=10.43.1.4:9790") ||
		!strings.Contains(agents, "maul-egress-broker=10.43.1.2:8080") {
		t.Errorf("agents row: %s", agents)
	}
	checkBrowsed(t, run)
	if !strings.Contains(run.stderr, "Tool-pod isolation passed") {
		t.Errorf("stderr:\n%s", run.stderr)
	}
}

// checkBrowsed requires that the browser container's Chromium loaded the page
// and that nothing but kubectl was called.
func checkBrowsed(t *testing.T, run edgesRun) {
	t.Helper()
	var browsed bool
	for _, c := range run.calls {
		if len(c) > 1 && c[1] == "exec" && slices.Contains(c, "browser") &&
			slices.Contains(c, "/usr/lib/chromium/chrome") && slices.Contains(c, "http://127.0.0.1:8099/") {
			browsed = true
		}
	}
	if !browsed {
		t.Errorf("Chromium never loaded the page in the browser container: %v", run.calls)
	}
	for _, c := range run.calls {
		if c[0] != "kubectl" {
			t.Errorf("called %s", c[0])
		}
	}
}

func mapsKeys(m map[string][]string) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

func TestToolsAgentsRowOnlyWhenAsked(t *testing.T) {
	run := runToolsProbe(t, nil, toolsArgs()...)
	if run.code != 0 || len(debugCalls(run)) != 5 {
		t.Errorf("exit %d with %d probe containers; want 0 and 5\n%s", run.code, len(debugCalls(run)), run.stderr)
	}
}

func TestToolsAFailingRowFailsTheRunAfterTheRest(t *testing.T) {
	for _, tc := range []struct{ row, log string }{
		{"sandbox", `{"name":"tool-api-loopback","outcome":"REACHED"}` + "\n" +
			`{"name":"summary","outcome":"SECURITY_FAILURE"}`},
		{"page", `{"name":"summary","outcome":"INCONCLUSIVE"}`},
		{"component", `{"name":"summary","outcome":"NOT_REACHED"}`},
	} {
		run := runToolsProbe(t, []string{"STUB_LOG_" + tc.row + "=" + tc.log}, toolsArgs()...)
		if run.code != 1 || !strings.Contains(run.stderr, "FAIL "+tc.row) ||
			strings.Contains(run.stderr, "Tool-pod isolation passed") {
			t.Errorf("%s: exit %d, stderr:\n%s", tc.row, run.code, run.stderr)
		}
		// The matrix is complete even when one row fails.
		if n := len(debugCalls(run)); n != 5 {
			t.Errorf("%s: %d probe containers ran, want all 5", tc.row, n)
		}
	}
}

// A probe that exits non-zero makes kubectl debug exit non-zero: the row is
// recorded as failed and the remaining rows still run, the run exiting 1.
func TestToolsARefusedRowDoesNotAbortTheMatrix(t *testing.T) {
	for _, row := range []string{"evaluator", "component", "tools", "sandbox"} {
		run := runToolsProbe(t, []string{"STUB_DEBUG_FAIL_" + row + "=1",
			"STUB_LOG_" + row + "=" + `{"name":"summary","outcome":"SECURITY_FAILURE"}`}, toolsArgs()...)
		if run.code != 1 || !strings.Contains(run.stderr, "FAIL "+row) {
			t.Errorf("%s: exit %d, stderr:\n%s", row, run.code, run.stderr)
		}
		if n := len(debugCalls(run)); n != 5 {
			t.Errorf("%s: %d probe containers ran, want all 5\n%s", row, n, run.stderr)
		}
		if strings.Contains(run.stderr, "Tool-pod isolation passed") {
			t.Errorf("%s: passed", row)
		}
	}
}

// A row whose container could not be started at all (no summary to read) is
// a failure of the run too, not a pass and not an abort.
func TestToolsARowThatCouldNotRunFailsTheRun(t *testing.T) {
	run := runToolsProbe(t, []string{"STUB_DEBUG_FAIL_tools=1", "STUB_LOG_tools=none"}, toolsArgs()...)
	if run.code != 1 || !strings.Contains(run.stderr, "FAIL tools") || len(debugCalls(run)) != 5 {
		t.Errorf("exit %d, %d probe containers, stderr:\n%s", run.code, len(debugCalls(run)), run.stderr)
	}
}

// kubectl debug --env splits its value on commas, so a list must reach the
// probe whole: no comma in any PROBE_ list the script passes.
func TestToolsListsSurviveKubectlDebugEnv(t *testing.T) {
	run := runToolsProbe(t, nil, toolsArgs()...)
	c := debugCalls(run)["evaluator@evaluator"]
	var deny string
	for _, a := range c {
		if v, ok := strings.CutPrefix(a, "--env=PROBE_DENY="); ok {
			deny = v
		}
	}
	if !strings.Contains(deny, "other-tool-pod=10.42.2.8:8090") || !strings.Contains(deny, "other-slot-pod=") {
		t.Fatalf("PROBE_DENY = %q, want both the other tool pod and the other slot pod", deny)
	}
	for _, calls := range debugCalls(run) {
		for _, a := range calls {
			if strings.HasPrefix(a, "--env=PROBE_") && strings.Contains(a, ",") {
				t.Errorf("%q carries a comma, which kubectl debug --env would split on", a)
			}
		}
	}
}

func TestToolsMatrixLinesAreNamedReachedAndBlocked(t *testing.T) {
	log := `{"name":"maul-egress-broker","outcome":"REACHED"}` + "\n" +
		`{"name":"internet","worker":1,"attempt":1,"outcome":"blocked"}` + "\n" + `{"name":"summary","outcome":"PASS"}`
	run := runToolsProbe(t, []string{"STUB_LOG_evaluator=" + log}, toolsArgs()...)
	if !strings.Contains(run.stderr, "maul-egress-broker REACHED") || !strings.Contains(run.stderr, "internet BLOCKED") {
		t.Errorf("stderr:\n%s", run.stderr)
	}
}

func TestToolsRefusedIsBlockedOnlyForTheEgressRows(t *testing.T) {
	run := runToolsProbe(t, nil, toolsArgs("--refused-is-blocked")...)
	calls := debugCalls(run)
	for key, c := range calls {
		want := !strings.HasPrefix(key, "sandbox@") && !strings.HasPrefix(key, "page@")
		if got := slices.Contains(c, "--refused-is-blocked"); got != want {
			t.Errorf("%s: --refused-is-blocked = %v, want %v", key, got, want)
		}
	}
	run = runToolsProbe(t, nil, toolsArgs()...)
	for key, c := range debugCalls(run) {
		if slices.Contains(c, "--refused-is-blocked") {
			t.Errorf("%s: --refused-is-blocked without the flag", key)
		}
	}
}

func TestToolsValidatesAndDryRunCallsNothing(t *testing.T) {
	without := func(flag string) []string {
		args := toolsArgs()
		i := slices.Index(args, flag)
		return slices.Delete(slices.Clone(args), i, i+2)
	}
	for _, args := range [][]string{
		nil,
		without("--tool-pod"), without("--evaluator-pod"), without("--component-pod"),
		without("--other-tool-pod"), without("--other-slot-pod"), without("--image"),
		toolsArgs("--tool-pod", "tool"),
		toolsArgs("--evaluator-pod", "maul-agents/Evaluator"),
		toolsArgs("--agents-tool-pod", "x;y/z"),
		toolsArgs("--component-port", "0"),
		toolsArgs("--component-port", "70000"),
		toolsArgs("--chromium", "chrome"),
		toolsArgs("--release-namespace", "Maul"),
		toolsArgs("--timeout", "0"),
		toolsArgs("--other-tool-pod", "maul-preview-0/tool"),
		toolsArgs("--bogus"),
		{"--image", "registry.example/netprobe:latest"},
	} {
		run := runToolsProbe(t, nil, args...)
		if run.code != 2 || len(run.calls) != 0 {
			t.Errorf("%q: exit %d, %d calls; want a usage error", args, run.code, len(run.calls))
		}
	}
	run := runToolsProbe(t, nil, toolsArgs("--dry-run", "--agents-tool-pod", "maul-agents/agents-tool")...)
	if run.code != 0 || len(run.calls) != 0 || !strings.Contains(run.stdout, "dry run: nothing was called") {
		t.Fatalf("--dry-run: exit %d, %d calls, stdout:\n%s", run.code, len(run.calls), run.stdout)
	}
	for _, want := range []string{
		"netprobe --as evaluator in maul-agents/evaluator", "netprobe --as peers in maul-preview-0/component",
		"netprobe --as tools in maul-preview-0/tool", "netprobe --as sandbox in maul-preview-0/tool",
		"netprobe --as page in maul-preview-0/tool", "netprobe --as tools in maul-agents/agents-tool",
		"REACHED: broker :8080, its own tool pod :8090", "BLOCKED: page JS fetching :8090 and the CDP shim",
	} {
		if !strings.Contains(run.stdout, want) {
			t.Errorf("--dry-run lacks %q:\n%s", want, run.stdout)
		}
	}
	run = runToolsProbe(t, nil, toolsArgs("--dry-run")...)
	if strings.Contains(run.stdout, "in maul-agents/agents-tool") {
		t.Errorf("--dry-run shows an agents row nobody asked for:\n%s", run.stdout)
	}
}

func TestToolsStopsOnAnAddressItCannotDiscover(t *testing.T) {
	run := runToolsProbe(t, []string{"STUB_POD_evaluator="}, toolsArgs()...)
	if run.code != 1 || len(debugCalls(run)) != 0 {
		t.Errorf("exit %d with %d probe containers; want a stop before any row\n%s",
			run.code, len(debugCalls(run)), run.stderr)
	}
}
