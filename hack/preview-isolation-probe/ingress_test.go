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

const ingressImage = "registry.example/patchy/previews/netprobe:sha-" + headSHA

// kubectlStub stands in for kubectl in ingress.sh's run. It logs each call's
// argv (after --request-timeout), keeps what each create was sent, and
// answers: the slot policy from STUB_POLICY; a Pod's existence check as not
// found; the listener's IP; every client's phase as Succeeded; and each
// client's log from STUB_LOG_<name>, the name's dashes as underscores.
const kubectlStub = `#!/bin/sh
case "$1" in --request-timeout=*) shift ;; esac
{ printf 'kubectl'; for arg in "$@"; do printf '\t%s' "$arg"; done; printf '\n'; } >>"$STUB_LOG"
case "$1 $2" in
"get networkpolicy") printf '%s\n' "$STUB_POLICY" ;;
"get pod")
    case "$*" in
    *podIP*) printf '10.42.0.9' ;;
    *phase*) printf 'Succeeded' ;;
    *) exit 1 ;;
    esac ;;
"create -f") cat >>"$STUB_CREATED"; printf -- '---\n' >>"$STUB_CREATED" ;;
"wait "*|"delete "*) ;;
"logs "*) eval "printf '%s\n' \"\$STUB_LOG_$(printf '%s' "$2" | tr - _)\"" ;;
*) exit 1 ;;
esac
`

// gatewayPolicy is a slot policy admitting the gateway's label.
const gatewayPolicy = `{"spec":{"ingress":[{"from":[{"ipBlock":{"cidr":"10.40.128.0/24"}},` +
	`{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"patchy"}},` +
	`"podSelector":{"matchLabels":{"patchy.bitwisemedia.uk/preview-gateway":"true"}}}]}]}}`

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
	return slices.Concat([]string{"--image", ingressImage, "--node-selector", "patchy.example.com/previews=true",
		"--taint-key", "patchy.example.com/preview-only", "--timeout", "10"}, extra)
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
	if !strings.Contains(docs[0], "namespace: patchy-preview-0") || !strings.Contains(docs[0], "args: [listen]") ||
		!strings.Contains(docs[0], `patchy.example.com/previews: "true"`) ||
		!strings.Contains(docs[0], "key: patchy.example.com/preview-only") {
		t.Errorf("listener:\n%s", docs[0])
	}
	for i, ns := range []string{"patchy-agents", "patchy", "patchy"} {
		doc := docs[i+1]
		if !strings.Contains(doc, "namespace: "+ns) || !strings.Contains(doc, `args: [connect, "10.42.0.9:8080"]`) ||
			strings.Contains(doc, "tolerations") || strings.Contains(doc, "nodeSelector") {
			t.Errorf("client %d:\n%s", i, doc)
		}
		if gw := strings.Contains(doc, `patchy.bitwisemedia.uk/preview-gateway: "true"`); gw != (i == 2) {
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
	run := runIngress(t, `{"spec":{"ingress":[{"from":[{"ipBlock":{"cidr":"10.40.128.0/24"}}]}]}}`, expected(),
		ingressArgs()...)
	if run.code != 1 || !strings.Contains(run.stderr, "previewAuth.gateway.enabled") || run.created != "" {
		t.Errorf("exit %d, created %q, stderr:\n%s", run.code, run.created, run.stderr)
	}
}

// TestIngressValidatesAndDryRunCallsNothing: every value is checked before
// anything runs, and --dry-run and --print-manifests never call kubectl.
func TestIngressValidatesAndDryRunCallsNothing(t *testing.T) {
	for _, args := range [][]string{
		{"--image", "registry.example/patchy/previews/netprobe:latest", "--no-node-isolation"},
		{"--image", ingressImage},
		{"--image", ingressImage, "--node-selector", "a=b"},
		{"--image", ingressImage, "--node-selector", "a=b c", "--taint-key", "k"},
		{"--image", ingressImage, "--no-node-isolation", "--taint-key", "k"},
		{"--image", ingressImage, "--no-node-isolation", "--slot", "x"},
		{"--image", ingressImage, "--no-node-isolation", "--release-namespace", "Patchy"},
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
