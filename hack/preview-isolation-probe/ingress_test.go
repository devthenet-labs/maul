// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package probe_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

const ingressImage = "registry.example/maul/previews/netprobe:sha-" + headSHA

// kubectlStub stands in for kubectl in ingress.sh's run. It logs each call's
// argv (after --request-timeout), keeps what each create was sent, and
// answers: the slot namespace's policies as STUB_POLICY and, if set, STUB_EXTRA_POLICY; a Pod's existence check as not
// found; the listener's IP; the listener's phase from STUB_LISTENER_PHASE
// (Running by default); every client's phase as Succeeded; and each client's
// log from STUB_LOG_<name>, the name's dashes as underscores.
const kubectlStub = `#!/bin/sh
case "$1" in --request-timeout=*) shift ;; esac
{ printf 'kubectl'; for arg in "$@"; do printf '\t%s' "$arg"; done; printf '\n'; } >>"$STUB_LOG"
case "$1 $2" in
"get networkpolicy") printf '{"items":[%s%s]}\n' "$STUB_POLICY" "${STUB_EXTRA_POLICY:+,$STUB_EXTRA_POLICY}" ;;
"get pod")
    case "$*" in
    *podIP*) printf '10.42.0.9' ;;
    *preview-ingress-listener*phase*) printf '%s' "${STUB_LISTENER_PHASE:-Running}" ;;
    *phase*) printf 'Succeeded' ;;
    *) exit 1 ;;
    esac ;;
"create -f") cat >>"$STUB_CREATED"; printf -- '---\n' >>"$STUB_CREATED" ;;
"wait "*|"delete "*) ;;
"logs "*) eval "printf '%s\n' \"\$STUB_LOG_$(printf '%s' "$2" | tr - _)\"" ;;
*) exit 1 ;;
esac
`

// gatewayOnlyPolicy is a slot policy admitting the gateway's label alone,
// as one off the legacy path is.
const gatewayOnlyPolicy = `{"metadata":{"name":"preview-isolation"},"spec":{"ingress":[{"from":[` +
	`{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"maul"}},` +
	`"podSelector":{"matchLabels":{"maul.run/preview-gateway":"true"}}}]}]}}`

// gatewayPolicy is a slot policy admitting the gateway's label beside the
// ALB's subnets, as one on the legacy path is.
const gatewayPolicy = `{"metadata":{"name":"preview-isolation"},` +
	`"spec":{"ingress":[{"from":[{"ipBlock":{"cidr":"10.40.128.0/24"}},` +
	`{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"maul"}},` +
	`"podSelector":{"matchLabels":{"maul.run/preview-gateway":"true"}}}]}]}}`

// summary is a client's log ending with outcome.
func summary(outcome string) string {
	return `{"name":"slot-pod","worker":1,"attempt":1,"outcome":"blocked"}` + "\n" +
		`{"name":"summary","outcome":"` + outcome + `"}`
}

type ingressRun struct {
	calls          [][]string
	created        string
	stdout, stderr string
	code           int
}

// runIngress runs ingress.sh with args against the kubectl stub; logs maps
// each client to its summary outcome.
func runIngress(t *testing.T, policy string, logs map[string]string, args ...string) ingressRun {
	t.Helper()
	return runIngressEnv(t, policy, logs, nil, args...)
}

// runIngressEnv is runIngress with extra environment for the stub.
func runIngressEnv(t *testing.T, policy string, logs map[string]string, extra []string,
	args ...string,
) ingressRun {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not on PATH")
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH; ingress.sh requires it")
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte(kubectlStub), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	log, created := filepath.Join(dir, "calls"), filepath.Join(dir, "created")
	env := []string{
		"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"STUB_LOG=" + log, "STUB_CREATED=" + created, "STUB_POLICY=" + policy,
	}
	env = append(env, extra...)
	for name, outcome := range logs {
		env = append(env, "STUB_LOG_"+strings.ReplaceAll(name, "-", "_")+"="+summary(outcome))
	}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "PATH=") && !strings.HasPrefix(kv, "PROBE_") && !strings.HasPrefix(kv, "STUB_") {
			env = append(env, kv)
		}
	}
	cmd := exec.Command(bash, append([]string{"ingress.sh"}, args...)...)
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	var run ingressRun
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("ingress.sh: %v", err)
		}
		run.code = exit.ExitCode()
	}
	run.stdout, run.stderr = stdout.String(), stderr.String()
	if b, err := os.ReadFile(created); err == nil {
		run.created = string(b)
	}
	if b, err := os.ReadFile(log); err == nil {
		for line := range strings.SplitSeq(strings.TrimSuffix(string(b), "\n"), "\n") {
			if line != "" {
				run.calls = append(run.calls, strings.Split(line, "\t"))
			}
		}
	}
	return run
}

func ingressArgs(extra ...string) []string {
	return slices.Concat([]string{"--image", ingressImage, "--node-selector", "maul.example.com/previews=true",
		"--taint-key", "maul.example.com/preview-only", "--timeout", "10"}, extra)
}

// expected is every client's outcome on a correct install.
func expected() map[string]string {
	return map[string]string{"preview-ingress-agent": "BLOCKED", "preview-ingress-release": "BLOCKED",
		"preview-ingress-gateway": "REACHED"}
}

// TestIngressPasses: the listener goes in the slot on the preview nodes,
// the three clients connect to its address, and the run passes when the two
// that must be refused were and the gateway's label was admitted.
func TestIngressPasses(t *testing.T) {
	run := runIngress(t, gatewayPolicy, expected(), ingressArgs()...)
	if run.code != 0 || !strings.Contains(run.stderr, "Ingress isolation passed") {
		t.Fatalf("exit %d, stderr:\n%s", run.code, run.stderr)
	}
	docs := strings.Split(strings.TrimSuffix(run.created, "---\n"), "---\n")
	if len(docs) != 4 {
		t.Fatalf("created %d objects, want the listener and three clients:\n%s", len(docs), run.created)
	}
	if !strings.Contains(docs[0], "namespace: maul-preview-0") || !strings.Contains(docs[0], "args: [listen]") ||
		!strings.Contains(docs[0], `maul.example.com/previews: "true"`) ||
		!strings.Contains(docs[0], "key: maul.example.com/preview-only") {
		t.Errorf("listener:\n%s", docs[0])
	}
	for i, ns := range []string{"maul-agents", "maul", "maul"} {
		doc := docs[i+1]
		if !strings.Contains(doc, "namespace: "+ns) || !strings.Contains(doc, `args: [connect, "10.42.0.9:8080"]`) ||
			strings.Contains(doc, "tolerations") || strings.Contains(doc, "nodeSelector") {
			t.Errorf("client %d:\n%s", i, doc)
		}
		if gw := strings.Contains(doc, `maul.run/preview-gateway: "true"`); gw != (i == 2) {
			t.Errorf("client %d carries the gateway label: %v", i, gw)
		}
	}
	deletes := 0
	for _, call := range run.calls {
		if call[1] == "delete" {
			deletes++
		}
	}
	if deletes == 0 {
		t.Error("the probe Pods were not deleted")
	}
}

// TestIngressFails: any client with another outcome fails the run.
func TestIngressFails(t *testing.T) {
	for name, outcome := range map[string]string{
		"preview-ingress-agent":   "REACHED",
		"preview-ingress-release": "REACHED",
		"preview-ingress-gateway": "BLOCKED",
	} {
		t.Run(name, func(t *testing.T) {
			logs := expected()
			logs[name] = outcome
			run := runIngress(t, gatewayPolicy, logs, ingressArgs()...)
			if run.code != 1 || !strings.Contains(run.stderr, "FAIL ") || !strings.Contains(run.stderr, name) {
				t.Errorf("exit %d, stderr:\n%s", run.code, run.stderr)
			}
		})
	}
	logs := expected()
	logs["preview-ingress-release"] = "INCONCLUSIVE"
	if run := runIngress(t, gatewayPolicy, logs, ingressArgs()...); run.code != 1 {
		t.Errorf("an inconclusive refusal passed: stderr:\n%s", run.stderr)
	}
}

// TestIngressNeedsTheGatewayPeer: a slot policy that admits no gateway Pod
// stops the run before anything is created.
func TestIngressNeedsTheGatewayPeer(t *testing.T) {
	run := runIngress(t, `{"metadata":{"name":"preview-isolation"},`+
		`"spec":{"ingress":[{"from":[{"ipBlock":{"cidr":"10.40.128.0/24"}}]}]}}`, expected(), ingressArgs()...)
	if run.code != 1 || !strings.Contains(run.stderr, "previewAuth.gateway.enabled") || run.created != "" {
		t.Errorf("exit %d, created %q, stderr:\n%s", run.code, run.created, run.stderr)
	}
}

// TestIngressValidatesAndDryRunCallsNothing: every value is checked before
// anything runs, and --dry-run and --print-manifests never call kubectl.
func TestIngressValidatesAndDryRunCallsNothing(t *testing.T) {
	for _, args := range [][]string{
		{"--image", "registry.example/maul/previews/netprobe:latest", "--no-node-isolation"},
		{"--image", ingressImage},
		{"--image", ingressImage, "--node-selector", "a=b"},
		{"--image", ingressImage, "--node-selector", "a=b c", "--taint-key", "k"},
		{"--image", ingressImage, "--no-node-isolation", "--taint-key", "k"},
		{"--image", ingressImage, "--no-node-isolation", "--slot", "x"},
		{"--image", ingressImage, "--no-node-isolation", "--release-namespace", "Maul"},
	} {
		run := runIngress(t, gatewayPolicy, expected(), args...)
		if run.code != 2 || len(run.calls) != 0 {
			t.Errorf("%q: exit %d, %d calls; want a usage error", args, run.code, len(run.calls))
		}
	}
	run := runIngress(t, gatewayPolicy, expected(), ingressArgs("--dry-run")...)
	if run.code != 0 || len(run.calls) != 0 || !strings.Contains(run.stdout, "nothing was called") {
		t.Errorf("--dry-run: exit %d, %d calls, stdout:\n%s", run.code, len(run.calls), run.stdout)
	}
	run = runIngress(t, gatewayPolicy, expected(), "--image", ingressImage, "--no-node-isolation",
		"--print-manifests")
	if run.code != 0 || len(run.calls) != 0 || strings.Count(run.stdout, "kind: Pod") != 4 ||
		strings.Contains(run.stdout, "nodeSelector") {
		t.Errorf("--print-manifests: exit %d, %d calls, stdout:\n%s", run.code, len(run.calls), run.stdout)
	}
}

// TestIngressWarnsOfAddressRanges: a slot policy that also admits an
// ipBlock (the ALB's subnets on the legacy path) lets every address in it
// past the gateway, which no client of this run tests, so the run says so;
// a policy admitting the gateway alone draws no warning. Neither changes
// the verdict.
func TestIngressWarnsOfAddressRanges(t *testing.T) {
	run := runIngress(t, gatewayPolicy, expected(), ingressArgs()...)
	if run.code != 0 || !strings.Contains(run.stderr, "WARNING: maul-preview-0/preview-isolation also admits "+
		"the address ranges 10.40.128.0/24") {
		t.Errorf("with an ipBlock: exit %d, stderr:\n%s", run.code, run.stderr)
	}
	run = runIngress(t, gatewayOnlyPolicy, expected(), ingressArgs()...)
	if run.code != 0 || strings.Contains(run.stderr, "WARNING") {
		t.Errorf("gateway alone: exit %d, stderr:\n%s", run.code, run.stderr)
	}
}

// createdDocs splits what the stub was sent to create, in order.
func (r ingressRun) createdDocs() []string {
	if r.created == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(r.created, "---\n"), "---\n")
}

// TestIngressRefusedIsBlocked: under --refused-is-blocked the gateway
// client goes first, the two clients that must be refused count a refusal
// as blocked, and the run passes when the listener still runs after them.
func TestIngressRefusedIsBlocked(t *testing.T) {
	run := runIngress(t, gatewayOnlyPolicy, expected(), ingressArgs("--refused-is-blocked")...)
	if run.code != 0 || !strings.Contains(run.stderr, "Ingress isolation passed") {
		t.Fatalf("exit %d, stderr:\n%s", run.code, run.stderr)
	}
	docs := run.createdDocs()
	if len(docs) != 4 {
		t.Fatalf("created %d objects, want the listener and three clients:\n%s", len(docs), run.created)
	}
	for i, want := range []struct{ name, args string }{
		{"preview-ingress-listener", "args: [listen]"},
		{"preview-ingress-gateway", `args: [connect, "10.42.0.9:8080"]`},
		{"preview-ingress-agent", `args: [connect, --refused-is-blocked, "10.42.0.9:8080"]`},
		{"preview-ingress-release", `args: [connect, --refused-is-blocked, "10.42.0.9:8080"]`},
	} {
		if !strings.Contains(docs[i], "name: "+want.name+"\n") || !strings.Contains(docs[i], want.args) {
			t.Errorf("object %d, want %s with %s:\n%s", i, want.name, want.args, docs[i])
		}
	}
	// Without the flag nothing changes: every client is told to call a
	// refusal inconclusive.
	run = runIngress(t, gatewayOnlyPolicy, expected(), ingressArgs()...)
	if run.code != 0 || strings.Contains(run.created, "--refused-is-blocked") {
		t.Errorf("without the flag: exit %d, created:\n%s", run.code, run.created)
	}
}

// TestIngressRefusedNeedsTheGateway: under --refused-is-blocked, a gateway
// client that does not reach the listener stops the run before the refused
// clients exist, since a refusal could then be the listener's own.
func TestIngressRefusedNeedsTheGateway(t *testing.T) {
	logs := expected()
	logs["preview-ingress-gateway"] = "BLOCKED"
	run := runIngress(t, gatewayOnlyPolicy, logs, ingressArgs("--refused-is-blocked")...)
	if run.code != 1 || !strings.Contains(run.stderr, "cannot be judged") {
		t.Fatalf("exit %d, stderr:\n%s", run.code, run.stderr)
	}
	if docs := run.createdDocs(); len(docs) != 2 || strings.Contains(run.created, "preview-ingress-agent") {
		t.Errorf("created %d objects, want the listener and the gateway client only:\n%s", len(docs), run.created)
	}
}

// TestIngressRefusedNeedsALiveListener: under --refused-is-blocked, a
// listener that stopped before the refused clients finished fails the run,
// though both clients were BLOCKED; a client that connected fails it too.
func TestIngressRefusedNeedsALiveListener(t *testing.T) {
	run := runIngressEnv(t, gatewayOnlyPolicy, expected(), []string{"STUB_LISTENER_PHASE=Failed"},
		ingressArgs("--refused-is-blocked")...)
	if run.code != 1 || !strings.Contains(run.stderr, "FAIL maul-preview-0/preview-ingress-listener: Failed") {
		t.Errorf("a stopped listener: exit %d, stderr:\n%s", run.code, run.stderr)
	}
	for _, name := range []string{"preview-ingress-agent", "preview-ingress-release"} {
		logs := expected()
		logs[name] = "REACHED"
		run = runIngress(t, gatewayOnlyPolicy, logs, ingressArgs("--refused-is-blocked")...)
		if run.code != 1 || !strings.Contains(run.stderr, "FAIL ") || !strings.Contains(run.stderr, name) {
			t.Errorf("%s reached: exit %d, stderr:\n%s", name, run.code, run.stderr)
		}
	}
	run = runIngress(t, gatewayOnlyPolicy, expected(), ingressArgs("--refused-is-blocked", "--dry-run")...)
	if run.code != 0 || len(run.calls) != 0 ||
		!strings.Contains(run.stdout, "blocked, once the gateway client has reached the listener") {
		t.Errorf("--dry-run: exit %d, %d calls, stdout:\n%s", run.code, len(run.calls), run.stdout)
	}
}

// TestIngressRefusesExtraPeers: the agents' client is held back by their
// own namespace's default-deny too, so it cannot show the slot policy
// refuses them; the script reads every policy in the slot namespace and
// refuses, before creating anything, any peer but the gateway's exact one
// and ipBlocks, a rule from anywhere, and a slot policy that leaves Pods or
// ingress ungoverned.
func TestIngressRefusesExtraPeers(t *testing.T) {
	const gw = `{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"maul"}},` +
		`"podSelector":{"matchLabels":{"maul.run/preview-gateway":"true"}}}`
	slot := func(spec string) string { return `{"metadata":{"name":"preview-isolation"},"spec":` + spec + `}` }
	for name, tc := range map[string]struct{ policy, extra, want string }{
		"the agents' namespace": {policy: slot(`{"ingress":[{"from":[` + gw + `,` +
			`{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"maul-agents"}}}]}]}`),
			want: "admits an ingress peer other than the preview gateway's"},
		"a rule from anywhere": {policy: slot(`{"ingress":[{"from":[` + gw + `]},{"ports":[{"port":8080}]}]}`),
			want: "has an ingress rule with no from, which admits every source"},
		"the gateway's label in any namespace": {policy: slot(`{"ingress":[{"from":[` +
			`{"podSelector":{"matchLabels":{"maul.run/preview-gateway":"true"}}}]}]}`),
			want: "admits an ingress peer other than the preview gateway's"},
		"a match expression": {policy: slot(`{"ingress":[{"from":[{"namespaceSelector":{"matchLabels":` +
			`{"kubernetes.io/metadata.name":"maul"}},"podSelector":{"matchLabels":` +
			`{"maul.run/preview-gateway":"true"},"matchExpressions":[{"key":"a","operator":"Exists"}]}}]}]}`),
			want: "admits an ingress peer other than the preview gateway's"},
		"some Pods only": {policy: slot(`{"podSelector":{"matchLabels":{"app":"web"}},"ingress":[{"from":[` + gw +
			`]}]}`), want: "selects only some of the slot's Pods"},
		"no ingress governed": {policy: slot(`{"policyTypes":["Egress"],"ingress":[{"from":[` + gw + `]}]}`),
			want: "does not govern ingress"},
		"another policy": {policy: gatewayOnlyPolicy, extra: `{"metadata":{"name":"allow-agents"},"spec":` +
			`{"ingress":[{"from":[{"namespaceSelector":{}}]}]}}`, want: "allow-agents admits an ingress peer"},
	} {
		t.Run(name, func(t *testing.T) {
			var env []string
			if tc.extra != "" {
				env = append(env, "STUB_EXTRA_POLICY="+tc.extra)
			}
			run := runIngressEnv(t, tc.policy, expected(), env, ingressArgs("--refused-is-blocked")...)
			if run.code != 1 || !strings.Contains(run.stderr, "admit more than the preview gateway") ||
				!strings.Contains(run.stderr, tc.want) || run.created != "" {
				t.Errorf("exit %d, created %q, stderr:\n%s", run.code, run.created, run.stderr)
			}
		})
	}
}

// TestIngressAcceptsTheChartPolicy: the slot policy the chart renders for
// a gateway-only install passes the script's static check, so the check
// is no stricter than the chart.
func TestIngressAcceptsTheChartPolicy(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	out, err := exec.CommandContext(t.Context(), "helm", "template", "maul", "../../charts/maul", "-n", "maul",
		"-f", "../testdata/chart-render/intent-controller.yaml",
		"-f", "../../cmd/maul/internal/previewsetup/testdata/golden/cloudflare-tunnel-k3s/maul-previews.values.yaml",
		"--set", "previewAuth.keys.renderOffline=true",
		"--set", "previewController.config.apiServerCIDR=10.43.0.1/32").Output()
	if err != nil {
		t.Fatalf("helm template: %v", err)
	}
	var items []string
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(out), 4096)
	for {
		var obj map[string]any
		if err := dec.Decode(&obj); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		meta, _ := obj["metadata"].(map[string]any)
		if obj["kind"] != "NetworkPolicy" || meta["namespace"] != "maul-preview-0" {
			continue
		}
		b, err := json.Marshal(obj)
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, string(b))
	}
	if len(items) == 0 {
		t.Fatal("the render has no NetworkPolicy in maul-preview-0")
	}
	run := runIngress(t, strings.Join(items, ","), expected(), ingressArgs("--refused-is-blocked")...)
	if run.code != 0 || !strings.Contains(run.stderr, "Ingress isolation passed") {
		t.Errorf("the chart's slot policy: exit %d, stderr:\n%s", run.code, run.stderr)
	}
}

// TestIngressOldImageFails: a netprobe image older than the script prints
// its usage instead of a JSON summary. The verdict skips the non-JSON line,
// says there is no summary and quotes it, and the run fails, never passes.
func TestIngressOldImageFails(t *testing.T) {
	const usage = "usage: probe connect <ipv4>:<port>"
	old := func(names ...string) []string {
		env := make([]string, 0, len(names))
		for _, n := range names {
			env = append(env, "STUB_LOG_"+strings.ReplaceAll(n, "-", "_")+"="+usage)
		}
		return env
	}
	for name, tc := range map[string]struct {
		env  []string
		args []string
		fail string
	}{
		"every client": {old("preview-ingress-agent", "preview-ingress-release", "preview-ingress-gateway"),
			ingressArgs(), "FAIL maul-agents/preview-ingress-agent: NO_SUMMARY"},
		"a refused client": {old("preview-ingress-agent"), ingressArgs("--refused-is-blocked"),
			"FAIL maul-agents/preview-ingress-agent: NO_SUMMARY"},
		"the gateway client": {old("preview-ingress-gateway"), ingressArgs("--refused-is-blocked"),
			"FAIL maul/preview-ingress-gateway: NO_SUMMARY"},
	} {
		t.Run(name, func(t *testing.T) {
			// The old clients' logs come from tc.env alone, verbatim.
			logs := expected()
			for _, kv := range tc.env {
				name, _, _ := strings.Cut(strings.TrimPrefix(kv, "STUB_LOG_"), "=")
				delete(logs, strings.ReplaceAll(name, "_", "-"))
			}
			run := runIngressEnv(t, gatewayOnlyPolicy, logs, tc.env, tc.args...)
			if run.code != 1 || strings.Contains(run.stderr, "Ingress isolation passed") {
				t.Fatalf("exit %d, stderr:\n%s", run.code, run.stderr)
			}
			for _, want := range []string{tc.fail, `first log line: "` + usage + `"`, "rebuild it from this commit"} {
				if !strings.Contains(run.stderr, want) {
					t.Errorf("stderr lacks %q:\n%s", want, run.stderr)
				}
			}
			if strings.Contains(run.stderr, "parse error") {
				t.Errorf("jq choked on the log:\n%s", run.stderr)
			}
		})
	}
}
