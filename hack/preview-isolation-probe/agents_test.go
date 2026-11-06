// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package probe_test

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// agentsStub stands in for kubectl in run.sh's agents run. It logs each
// call's argv (after --request-timeout); answers a Pod as JSON from
// STUB_PODJSON_<name> or by its address STUB_POD_<name>, a Service by
// STUB_SVC_<name>, the probe Job as succeeded, its log with STUB_DRIVER_LOG
// and the tools container's with STUB_TOOLS_LOG; and keeps what it is asked
// to create in $STUB_DIR/created.
const agentsStub = `#!/bin/sh
case "$1" in --request-timeout=*) shift ;; esac
{ printf 'kubectl'; for arg in "$@"; do printf '\t%s' "$arg"; done; printf '\n'; } >>"$STUB_LOG"
var() { printf '%s' "$1" | tr -- '-' '_'; }
json=0; for a in "$@"; do [ "$a" = json ] && json=1; done
case "$1" in
get)
    case "$2" in
    pod)
        if [ $json -eq 1 ]; then eval "printf '%s' \"\${STUB_PODJSON_$(var "$3")}\""
        else eval "printf '%s' \"\${STUB_POD_$(var "$3")}\""; fi ;;
    service) eval "printf '%s' \"\${STUB_SVC_$(var "$3")}\"" ;;
    job) for a in "$@"; do [ "$a" = name ] && exit 0; done; printf '1/' ;;
    esac ;;
create) cat >"$STUB_DIR/created" ;;
logs)
    case "$2" in job/*) printf '%s\n' "$STUB_DRIVER_LOG" ;; *) printf '%s\n' "$STUB_TOOLS_LOG" ;; esac ;;
delete) ;;
*) exit 1 ;;
esac
`

// goStub stands in for go: `go -C <dir> run ./cmd/agentscheck <sub> ...`
// logs its argv, copies a ledger run's files, reads stdin, and prints
// STUB_CHECK_<sub> (by default a passing summary).
const goStub = `#!/bin/sh
{ printf 'go'; for arg in "$@"; do printf '\t%s' "$arg"; done; printf '\n'; } >>"$STUB_LOG"
sub=$5
if [ "$sub" = ledger ]; then
    shift 5
    while [ $# -gt 0 ]; do
        case "$1" in --termination) cp "$2" "$STUB_DIR/termination" ;; --log) cp "$2" "$STUB_DIR/tools.log" ;; esac
        shift
    done
else
    cat >"$STUB_DIR/pod.json"
fi
eval "out=\${STUB_CHECK_$sub}"
printf '%s\n' "${out:-{\"name\":\"x\",\"outcome\":\"held\"\}
{\"name\":\"summary\",\"outcome\":\"PASS\"\}}"
`

const (
	agentsToolPodName = "maul-tools-6f1c2d3e4b5a-x7k2p"
	agentsToolJob     = "maul-tools-6f1c2d3e4b5a"
)

// agentsToolPodJSON is the tool pod as the API server returns it: its role,
// its controlling Job, its address and, when finished, its tools
// container's termination message.
func agentsToolPodJSON(role string, finished bool) string {
	state := `{"running":{}}`
	if finished {
		state = `{"terminated":{"exitCode":0,"message":"{\"count\":3,\"sha256\":\"abc\"}"}}`
	}
	return `{"metadata":{"name":"` + agentsToolPodName + `","labels":{"maul.run/role":"` + role + `"},` +
		`"ownerReferences":[{"apiVersion":"batch/v1","kind":"Job","name":"` + agentsToolJob + `","controller":true}]},` +
		`"status":{"podIP":"10.42.1.10","containerStatuses":[{"name":"tools","state":` + state + `}]}}`
}

var agentsAddrs = map[string]string{
	"SVC_maul_egress_broker": "10.43.1.2", "SVC_kubernetes": "10.43.0.1", "SVC_kube_dns": "10.43.0.10",
	"SVC_maul_source_controller": "10.43.1.4", "SVC_maul_egress_proxy": "10.43.1.5", "POD_other_tool": "10.42.1.11",
}

func agentsArgs(extra ...string) []string {
	return slices.Concat([]string{"--image", edgesImage, "--timeout", "5", "--ledger-timeout", "5",
		"--tool-pod", "maul-agents/" + agentsToolPodName}, extra)
}

type agentsRunResult struct {
	edgesRun
	dir string
}

func (r agentsRunResult) file(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(r.dir, name))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return string(b)
}

func runAgentsProbe(t *testing.T, extra []string, args ...string) agentsRunResult {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not on PATH")
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH; run.sh requires it")
	}
	bin, dir := t.TempDir(), t.TempDir()
	for name, script := range map[string]string{"kubectl": agentsStub, "go": goStub, "gh": failStub,
		"aws": failStub} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	log := filepath.Join(dir, "calls")
	env := []string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"), "STUB_LOG=" + log,
		"STUB_DIR=" + dir, "STUB_PODJSON_" + strings.ReplaceAll(agentsToolPodName, "-", "_") + "=" +
			agentsToolPodJSON("tools", true),
		`STUB_DRIVER_LOG={"name":"tool-pod","outcome":"REACHED"}` + "\n" + `{"name":"summary","outcome":"PASS"}`,
		"STUB_TOOLS_LOG=MAUL-TOOLS: {}"}
	for name, ip := range agentsAddrs {
		env = append(env, "STUB_"+name+"="+ip)
	}
	env = append(env, extra...)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "PATH=") && !strings.HasPrefix(kv, "PROBE_") && !strings.HasPrefix(kv, "STUB_") {
			env = append(env, kv)
		}
	}
	cmd := exec.Command(bash, append([]string{"run.sh", "agents"}, args...)...)
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	run := agentsRunResult{dir: dir}
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("run.sh agents: %v", err)
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

// callsOf are the calls whose argv (after the command) starts with prefix.
func callsOf(run edgesRun, cmd string, prefix ...string) [][]string {
	var out [][]string
	for _, c := range run.calls {
		if c[0] == cmd && len(c) > len(prefix) && slices.Equal(c[1:1+len(prefix)], prefix) {
			out = append(out, c)
		}
	}
	return out
}

func TestAgentsRunsEveryRow(t *testing.T) {
	run := runAgentsProbe(t, nil, agentsArgs("--taint-key", "maul.example.com/preview-only")...)
	if run.code != 0 || !strings.Contains(run.stderr, "Agents tool pod isolation passed") {
		t.Fatalf("exit %d, stderr:\n%s", run.code, run.stderr)
	}
	for _, row := range []string{"PASS pod", "PASS agents", "PASS ledger"} {
		if !strings.Contains(run.stderr, row) {
			t.Errorf("no %q in:\n%s", row, run.stderr)
		}
	}
	// The probe Job: created as intent-controller, labelled as an evaluator
	// with the probe's own verification UID, mounting the tool Job's
	// Secret's key file and nothing else of it.
	creates := callsOf(run.edgesRun, "kubectl", "create")
	if len(creates) != 1 || !slices.Contains(creates[0],
		"--as=system:serviceaccount:maul:maul-intent-controller") {
		t.Fatalf("creates: %v", creates)
	}
	job := run.file(t, "created")
	for _, want := range []string{
		"maul.run/role: evaluator", "maul.run/preview-verification: 00000000-0000-4000-8000-0000000a6e75",
		"secretName: " + agentsToolJob + "-tools", "items: [{key: tools-key, path: tools-key}]",
		"args: [--as, agents]", `{name: PROBE_TOOL_POD, value: "10.42.1.10"}`,
		`{name: PROBE_REACH, value: "artifact-server=10.43.1.4:9790"}`,
		`{name: PROBE_DENY, value: "maul-egress-broker=10.43.1.2:8080;kubernetes-api=10.43.0.1:443;` +
			`cluster-dns=10.43.0.10:53;imds=169.254.169.254:80;internet=1.1.1.1:443"}`,
		`{name: PROBE_OTHER_TOOL_POD, value: ""}`, `{name: PROBE_GO, value: "go"}`,
		"automountServiceAccountToken: false", "dnsPolicy: None",
	} {
		if !strings.Contains(job, want) {
			t.Errorf("the probe Job lacks %q:\n%s", want, job)
		}
	}
	// The key is never read: no Secret is ever got.
	for _, c := range run.calls {
		if slices.Contains(c, "secret") || slices.Contains(c, "secrets") {
			t.Errorf("a Secret call: %v", c)
		}
	}
	checkAgentsChecks(t, run, job)
	// The probe Job is deleted before the run waits on the ledger.
	if len(callsOf(run.edgesRun, "kubectl", "delete", "job", "preview-isolation-agents")) == 0 {
		t.Error("the probe Job was never deleted")
	}
}

// checkAgentsChecks: agentscheck runs from this checkout, on the pod with
// the taint key, then on the ledger with the tools container's log, its
// termination message and the probe Job's nonce.
func checkAgentsChecks(t *testing.T, run agentsRunResult, job string) {
	t.Helper()
	pod := callsOf(run.edgesRun, "go", "-C")
	if len(pod) != 2 || !slices.Contains(pod[0], "pod") || !slices.Contains(pod[0], "maul.example.com/preview-only") ||
		!slices.Contains(pod[1], "ledger") {
		t.Fatalf("agentscheck calls: %v", pod)
	}
	if !strings.Contains(run.file(t, "pod.json"), `"maul.run/role":"tools"`) {
		t.Error("agentscheck pod was not handed the tool pod")
	}
	nonce := regexp.MustCompile(`PROBE_NONCE, value: "([0-9a-f]{32})"`).FindStringSubmatch(job)
	if nonce == nil || !slices.Contains(pod[1], nonce[1]) {
		t.Errorf("the ledger check's nonce is not the Job's: %v", pod[1])
	}
	if got := run.file(t, "termination"); got != `{"count":3,"sha256":"abc"}` {
		t.Errorf("termination message %q", got)
	}
	if got := run.file(t, "tools.log"); got != "MAUL-TOOLS: {}\n" {
		t.Errorf("tools log %q", got)
	}
}

func TestAgentsOptionalRows(t *testing.T) {
	run := runAgentsProbe(t, nil, agentsArgs("--other-tool-pod", "maul-agents/other-tool",
		"--profile-proxy-port", "8091", "--refused-is-blocked", "--go", "/usr/local/go/bin/go")...)
	if run.code != 0 {
		t.Fatalf("exit %d:\n%s", run.code, run.stderr)
	}
	job := run.file(t, "created")
	for _, want := range []string{
		`{name: PROBE_OTHER_TOOL_POD, value: "10.42.1.11"}`,
		`{name: PROBE_REACH, value: "artifact-server=10.43.1.4:9790;egress-proxy=10.43.1.5:8091"}`,
		"args: [--as, agents, --refused-is-blocked]", `{name: PROBE_GO, value: "/usr/local/go/bin/go"}`,
	} {
		if !strings.Contains(job, want) {
			t.Errorf("the probe Job lacks %q:\n%s", want, job)
		}
	}
}

func TestAgentsAFailingRowFailsTheRun(t *testing.T) {
	failed := `{"name":"summary","outcome":"SECURITY_FAILURE"}`
	for _, tc := range []struct{ row, env string }{
		{"agents", "STUB_DRIVER_LOG=" + `{"name":"base/dev-shm","outcome":"BROKEN"}` + "\n" + failed},
		{"agents", "STUB_DRIVER_LOG=no summary at all"},
		{"pod", "STUB_CHECK_pod=" + failed},
		{"ledger", "STUB_CHECK_ledger=" + failed},
	} {
		run := runAgentsProbe(t, []string{tc.env}, agentsArgs()...)
		if run.code != 1 || !strings.Contains(run.stderr, "FAIL "+tc.row) ||
			strings.Contains(run.stderr, "isolation passed") {
			t.Errorf("%s: exit %d, stderr:\n%s", tc.env, run.code, run.stderr)
		}
	}
}

func TestAgentsWaitsForTheLedger(t *testing.T) {
	run := runAgentsProbe(t, []string{"STUB_PODJSON_" + strings.ReplaceAll(agentsToolPodName, "-", "_") + "=" +
		agentsToolPodJSON("tools", false)}, agentsArgs("--ledger-timeout", "1")...)
	if run.code != 1 || !strings.Contains(run.stderr, "FAIL ledger") {
		t.Errorf("exit %d, stderr:\n%s", run.code, run.stderr)
	}
	for _, c := range callsOf(run.edgesRun, "go", "-C") {
		if slices.Contains(c, "ledger") {
			t.Errorf("the ledger was checked before the evaluation finished: %v", c)
		}
	}
}

func TestAgentsRefusesAnotherPod(t *testing.T) {
	run := runAgentsProbe(t, []string{"STUB_PODJSON_" + strings.ReplaceAll(agentsToolPodName, "-", "_") + "=" +
		agentsToolPodJSON("evaluator", true)}, agentsArgs()...)
	if run.code != 1 || len(callsOf(run.edgesRun, "kubectl", "create")) != 0 {
		t.Errorf("exit %d, stderr:\n%s", run.code, run.stderr)
	}
}

func TestAgentsValidatesAndDryRunCallsNothing(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"--image", edgesImage},
		{"--tool-pod", "maul-agents/x"},
		agentsArgs("--image", "registry.example/netprobe:latest"),
		agentsArgs("--tool-pod", "tool"),
		agentsArgs("--other-tool-pod", "maul-agents/"+agentsToolPodName),
		agentsArgs("--other-tool-pod", "maul-preview-0/tool"),
		agentsArgs("--taint-key", "bad key"),
		agentsArgs("--profile-proxy-port", "0"),
		agentsArgs("--go", "go run"),
		agentsArgs("--release-namespace", "Maul"),
		agentsArgs("--timeout", "0"),
		agentsArgs("--ledger-timeout", "x"),
		agentsArgs("--bogus"),
	} {
		run := runAgentsProbe(t, nil, args...)
		if run.code != 2 || len(run.calls) != 0 {
			t.Errorf("%q: exit %d, %d calls; want a usage error", args, run.code, len(run.calls))
		}
	}
	run := runAgentsProbe(t, nil, agentsArgs("--dry-run", "--other-tool-pod", "maul-agents/other-tool")...)
	if run.code != 0 || len(run.calls) != 0 || !strings.Contains(run.stdout, "dry run: nothing was called") {
		t.Fatalf("--dry-run: exit %d, %d calls, stdout:\n%s", run.code, len(run.calls), run.stdout)
	}
	for _, want := range []string{"go run probe.go --as sandbox-head", "--as sandbox-base", "--as tools-agents",
		"maul-agents/other-tool refusing", "agentscheck pod", "agentscheck ledger"} {
		if !strings.Contains(run.stdout, want) {
			t.Errorf("--dry-run lacks %q:\n%s", want, run.stdout)
		}
	}
	run = runAgentsProbe(t, nil, agentsArgs("--print-manifests")...)
	if run.code != 0 || len(run.calls) != 0 || !strings.Contains(run.stdout, "kind: Job") ||
		!strings.Contains(run.stdout, "192.0.2.1") {
		t.Errorf("--print-manifests: exit %d, %d calls, stdout:\n%s", run.code, len(run.calls), run.stdout)
	}
}
