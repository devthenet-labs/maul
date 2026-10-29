// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package probe_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// evaluatorStub stands in for kubectl in run.sh's evaluator run. It logs
// each call's argv (after --request-timeout), keeps what each create from
// stdin was sent, and answers a cluster where the evaluator's egress policy
// (unless STUB_NO_EVALUATOR_POLICY) and the slot policy exist, the slot holds
// STUB_DEPLOYMENTS, the probe Job does not exist yet (STUB_JOB_EXISTS) and
// ends with STUB_JOB_STATUS ("<succeeded>/<failed>"), every Service and Pod
// has the address STUB_IP_<svc|pod>_<name> names, the Job's log is
// STUB_EVALUATOR_LOG, and a connect to a listener's own loopback is REACHED.
// STUB_RESTART_APP's Pod reads as restarted from its second read on.
const evaluatorStub = `#!/bin/sh
case "$1" in --request-timeout=*) shift ;; esac
{ printf 'kubectl'; for arg in "$@"; do printf '\t%s' "$arg"; done; printf '\n'; } >>"$STUB_LOG"
ip() { eval "printf '%s' \"\${STUB_IP_$(printf '%s' "$1" | tr -- '-' '_')}\""; }
case "$1 $2" in
"get networkpolicy")
    case "$*" in
    *maul-agents-evaluator*)
        if [ -n "$STUB_NO_EVALUATOR_POLICY" ]; then exit 1; fi
        printf 'networkpolicy.networking.k8s.io/maul-agents-evaluator\n' ;;
    *preview-isolation*) printf 'networkpolicy.networking.k8s.io/preview-isolation\n' ;;
    *) exit 1 ;;
    esac ;;
"get deployments") printf '%s' "$STUB_DEPLOYMENTS" ;;
"get job")
    case "$*" in
    *jsonpath*) printf '%s' "${STUB_JOB_STATUS-1/}" ;;
    *) printf '%s' "$STUB_JOB_EXISTS" ;;
    esac ;;
"get service") ip "svc-$3" ;;
"get pods")
    for a in "$@"; do case "$a" in app=*) app=${a#app=} ;; esac; done
    n=$(cat "$STUB_DIR/reads-$app" 2>/dev/null || echo 0)
    n=$((n + 1))
    echo "$n" >"$STUB_DIR/reads-$app"
    restarts=0
    if [ "$app" = "$STUB_RESTART_APP" ] && [ "$n" -gt 1 ]; then restarts=1; fi
    printf '%s-p %s %s\n' "$app" "$(ip "pod-$app")" "$restarts" ;;
"create "*)
    for last in "$@"; do :; done
    if [ "$last" = - ]; then cat >>"$STUB_CREATED"; printf -- '---\n' >>"$STUB_CREATED"; fi ;;
"exec -n")
    for target in "$@"; do :; done
    outcome=BLOCKED
    case "$target" in 127.0.0.1:*) outcome=REACHED ;; esac
    printf '{"name":"summary","outcome":"%s"}\n' "$outcome" ;;
"logs "*) printf '%s\n' "$STUB_EVALUATOR_LOG" ;;
"wait "* | "delete "*) ;;
*) exit 1 ;;
esac
`

// evaluatorIPs are the evaluator site's addresses: the slot component's Pod
// and the Services the evaluator must reach or must not.
var evaluatorIPs = map[string]string{
	"pod-preview-isolation-component": "10.42.0.31",
	"svc-kubernetes":                  "10.43.0.1", "svc-kube-dns": "10.43.0.10",
	"svc-maul-egress-broker": "10.43.1.2", "svc-maul-source-controller": "10.43.1.4",
}

const passLog = `{"name":"maul-egress-broker","outcome":"REACHED"}
{"name":"cluster-dns","worker":1,"attempt":1,"outcome":"blocked"}
{"name":"summary","outcome":"PASS"}`

// runEvaluator runs run.sh evaluator with args against evaluatorStub; extra
// is more environment for the stub.
func runEvaluatorProbe(t *testing.T, extra []string, args ...string) edgesRun {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not on PATH")
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH; run.sh requires it")
	}
	bin, dir := t.TempDir(), t.TempDir()
	for name, script := range map[string]string{"kubectl": evaluatorStub, "gh": failStub, "aws": failStub} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	log, created := filepath.Join(dir, "calls"), filepath.Join(dir, "created")
	env := []string{
		"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"STUB_LOG=" + log, "STUB_CREATED=" + created, "STUB_DIR=" + dir, "STUB_EVALUATOR_LOG=" + passLog,
	}
	for name, ip := range evaluatorIPs {
		env = append(env, "STUB_IP_"+strings.ReplaceAll(name, "-", "_")+"="+ip)
	}
	env = append(env, extra...)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "PATH=") && !strings.HasPrefix(kv, "PROBE_") && !strings.HasPrefix(kv, "STUB_") {
			env = append(env, kv)
		}
	}
	cmd := exec.Command(bash, append([]string{"run.sh", "evaluator"}, args...)...)
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	var run edgesRun
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("run.sh evaluator: %v", err)
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

func evaluatorArgs(extra ...string) []string {
	return slices.Concat([]string{"--image", edgesImage, "--node-selector", "maul.example.com/previews=true",
		"--taint-key", "maul.example.com/preview-only", "--timeout", "2"}, extra)
}

// probeJob is the parts of the evaluator Job the run must get right.
type probeJob struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Namespace string            `json:"namespace"`
		Labels    map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		BackoffLimit *int `json:"backoffLimit"`
		Template     struct {
			Metadata struct {
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
			Spec struct {
				DNSPolicy                    string `json:"dnsPolicy"`
				AutomountServiceAccountToken *bool  `json:"automountServiceAccountToken"`
				RestartPolicy                string `json:"restartPolicy"`
				Containers                   []struct {
					Image string   `json:"image"`
					Args  []string `json:"args"`
					Env   []struct {
						Name  string `json:"name"`
						Value string `json:"value"`
					} `json:"env"`
				} `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
}

// jobsIn returns every Job document in manifests.
func jobsIn(t *testing.T, manifests string) []probeJob {
	t.Helper()
	var out []probeJob
	for doc := range strings.SplitSeq(manifests, "---\n") {
		if !strings.Contains(doc, "kind: Job") {
			continue
		}
		var j probeJob
		if err := utilyaml.Unmarshal([]byte(doc), &j); err != nil {
			t.Fatal(err)
		}
		out = append(out, j)
	}
	return out
}

// TestEvaluatorPasses: the run creates a slot component and, as
// intent-controller, a Job labelled as an evaluator is, whose netprobe runs
// --as evaluator against the site's addresses; a PASS summary passes.
func TestEvaluatorPasses(t *testing.T) {
	for _, refused := range []bool{false, true} {
		t.Run(fmt.Sprintf("refused-is-blocked=%v", refused), func(t *testing.T) {
			var args []string
			if refused {
				args = append(args, "--refused-is-blocked")
			}
			run := runEvaluatorProbe(t, nil, evaluatorArgs(args...)...)
			if run.code != 0 || !strings.Contains(run.stderr, "Evaluator isolation passed") {
				t.Fatalf("exit %d, stderr:\n%s", run.code, run.stderr)
			}
			var impersonated [][]string
			for _, call := range run.calls {
				if call[0] != "kubectl" {
					t.Errorf("the evaluator run called %q", call)
				}
				if slices.ContainsFunc(call, func(a string) bool { return strings.HasPrefix(a, "--as") }) {
					impersonated = append(impersonated, call)
				}
			}
			if len(impersonated) != 1 || impersonated[0][1] != "create" ||
				!slices.Contains(impersonated[0], "--as=system:serviceaccount:maul:maul-intent-controller") {
				t.Errorf("impersonated calls %q, want only the Job's create, as intent-controller", impersonated)
			}
			jobs := jobsIn(t, run.created)
			if len(jobs) != 1 {
				t.Fatalf("created %d Jobs, want one:\n%s", len(jobs), run.created)
			}
			checkProbeJob(t, jobs[0], refused)
			if c := strings.Count(run.created, "kind: Deployment"); c != 1 ||
				!strings.Contains(run.created, "namespace: maul-preview-0") {
				t.Errorf("created %d Deployments, want the slot component in maul-preview-0", c)
			}
		})
	}
}

// checkProbeJob holds the probe Job to an evaluator's labels and shape, and
// its netprobe to the site's addresses.
func checkProbeJob(t *testing.T, j probeJob, refused bool) {
	t.Helper()
	for _, labels := range []map[string]string{j.Metadata.Labels, j.Spec.Template.Metadata.Labels} {
		for k, v := range map[string]string{
			"app.kubernetes.io/name": "maul-agent", "app.kubernetes.io/managed-by": "maul",
			"maul.run/kind": "intent", "maul.run/harness": "claude", "maul.run/role": "evaluator",
		} {
			if labels[k] != v {
				t.Errorf("label %s = %q, want %q", k, labels[k], v)
			}
		}
		if len(labels["maul.run/preview-verification"]) != 36 {
			t.Errorf("label maul.run/preview-verification = %q, want a UID", labels["maul.run/preview-verification"])
		}
	}
	spec := j.Spec.Template.Spec
	if j.Metadata.Namespace != "maul-agents" || spec.DNSPolicy != "None" || spec.RestartPolicy != "Never" ||
		spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken ||
		j.Spec.BackoffLimit == nil || *j.Spec.BackoffLimit != 0 || len(spec.Containers) != 1 {
		t.Errorf("Job shape %+v", j)
	}
	c := spec.Containers[0]
	wantArgs := []string{"--as", "evaluator"}
	if refused {
		wantArgs = append(wantArgs, "--refused-is-blocked")
	}
	if c.Image != edgesImage || !slices.Equal(c.Args, wantArgs) {
		t.Errorf("container image %s args %q, want %s %q", c.Image, c.Args, edgesImage, wantArgs)
	}
	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	want := map[string]string{
		"PROBE_EGRESS_BROKER": "10.43.1.2", "PROBE_KUBERNETES_API": "10.43.0.1",
		"PROBE_CLUSTER_DNS": "10.43.0.10", "PROBE_SOURCE_CONTROLLER": "10.43.1.4",
		"PROBE_SLOT_COMPONENT": "10.42.0.31",
	}
	if fmt.Sprint(env) != fmt.Sprint(want) {
		t.Errorf("probe env %v, want %v", env, want)
	}
}

// TestEvaluatorFails: any summary but PASS, a Job that never finishes, and
// a slot component that restarted each fail the run, which cleans up.
func TestEvaluatorFails(t *testing.T) {
	for name, tc := range map[string]struct {
		extra []string
		want  string
	}{
		"a forbidden target reached": {
			extra: []string{`STUB_EVALUATOR_LOG={"name":"slot-component","outcome":"REACHABLE"}` + "\n" +
				`{"name":"summary","outcome":"SECURITY_FAILURE"}`, "STUB_JOB_STATUS=/1"},
			want: "FAIL evaluator: SECURITY_FAILURE"},
		"the broker not reached": {
			extra: []string{`STUB_EVALUATOR_LOG={"name":"summary","outcome":"NOT_REACHED"}`, "STUB_JOB_STATUS=/1"},
			want:  "FAIL evaluator: NOT_REACHED"},
		"inconclusive": {
			extra: []string{`STUB_EVALUATOR_LOG={"name":"summary","outcome":"INCONCLUSIVE"}`, "STUB_JOB_STATUS=/1"},
			want:  "FAIL evaluator: INCONCLUSIVE"},
		"no summary": {extra: []string{"STUB_EVALUATOR_LOG=usage: probe", "STUB_JOB_STATUS=/1"},
			want: "FAIL evaluator: NO_SUMMARY"},
		"a pass from a failed Job": {extra: []string{"STUB_JOB_STATUS=/1"},
			want: "FAIL evaluator: the Job failed"},
		"a Job that never finishes": {extra: []string{"STUB_JOB_STATUS=/"},
			want: "did not finish within 2s"},
		"a restarted component": {extra: []string{"STUB_RESTART_APP=preview-isolation-component"},
			want: "FAIL maul-preview-0 slot component: its Pod restarted or was replaced during the run"},
	} {
		t.Run(name, func(t *testing.T) {
			run := runEvaluatorProbe(t, tc.extra, evaluatorArgs()...)
			if run.code != 1 || !strings.Contains(run.stderr, tc.want) ||
				strings.Contains(run.stderr, "Evaluator isolation passed") {
				t.Errorf("exit %d, stderr:\n%s\nwant %q", run.code, run.stderr, tc.want)
			}
			var deletedJob, deletedComponent bool
			for _, call := range run.calls {
				deletedJob = deletedJob || (call[1] == "delete" && slices.Contains(call, "preview-isolation-evaluator"))
				deletedComponent = deletedComponent ||
					(call[1] == "delete" && slices.Contains(call, "preview-isolation-component"))
			}
			if !deletedJob || !deletedComponent {
				t.Errorf("deleted the Job %v, the component %v; want both", deletedJob, deletedComponent)
			}
		})
	}
}

// TestEvaluatorRefusesToStart: without the evaluator's egress policy (the
// value off), in a busy slot, or with a probe Job left over, the run stops
// before anything is created.
func TestEvaluatorRefusesToStart(t *testing.T) {
	for name, tc := range map[string]struct {
		extra []string
		want  string
	}{
		"the evaluator off":  {[]string{"STUB_NO_EVALUATOR_POLICY=1"}, "intentController.verification.evaluator"},
		"a busy slot":        {[]string{"STUB_DEPLOYMENTS=deployment.apps/preview-shop\n"}, "is not free"},
		"a left-over probe":  {[]string{"STUB_JOB_EXISTS=job.batch/preview-isolation-evaluator\n"}, "already exists"},
		"no address for DNS": {[]string{"STUB_IP_svc_kube_dns=None"}, "could not discover"},
		"no slot component":  {[]string{"STUB_IP_pod_preview_isolation_component="}, "no one running Pod"},
		"a component with no IPv4 address": {[]string{"STUB_IP_pod_preview_isolation_component=x"},
			"no one running Pod"},
	} {
		t.Run(name, func(t *testing.T) {
			run := runEvaluatorProbe(t, tc.extra, evaluatorArgs()...)
			if run.code != 1 || !strings.Contains(run.stderr, tc.want) || len(jobsIn(t, run.created)) != 0 {
				t.Errorf("exit %d, created %q, stderr:\n%s\nwant %q", run.code, run.created, run.stderr, tc.want)
			}
		})
	}
}

// TestEvaluatorValidatesAndDryRunCallsNothing: every value is checked
// before anything runs, and --dry-run and --print-manifests call nothing.
func TestEvaluatorValidatesAndDryRunCallsNothing(t *testing.T) {
	for _, args := range [][]string{
		{"--image", "registry.example/maul/previews/netprobe:latest", "--no-node-isolation"},
		{"--image", edgesImage},
		{"--image", edgesImage, "--no-node-isolation", "--slot", "x"},
		{"--image", edgesImage, "--no-node-isolation", "--release-namespace", "Maul"},
		{"--image", edgesImage, "--no-node-isolation", "--agents-namespace", "Maul"},
		{"--image", edgesImage, "--no-node-isolation", "--intent-controller-sa", "a/b"},
		{"--image", edgesImage, "--no-node-isolation", "--timeout", "0"},
		{"--image", edgesImage, "--no-node-isolation", "7"},
	} {
		run := runEvaluatorProbe(t, nil, args...)
		if run.code != 2 || len(run.calls) != 0 {
			t.Errorf("%q: exit %d, %d calls; want a usage error", args, run.code, len(run.calls))
		}
	}
	run := runEvaluatorProbe(t, nil, evaluatorArgs("--dry-run")...)
	if run.code != 0 || len(run.calls) != 0 || !strings.Contains(run.stdout, "nothing was called") ||
		!strings.Contains(run.stdout, "maul-preview-0") || !strings.Contains(run.stdout, "maul-intent-controller") {
		t.Errorf("--dry-run: exit %d, %d calls, stdout:\n%s", run.code, len(run.calls), run.stdout)
	}
	run = runEvaluatorProbe(t, nil, evaluatorArgs("--print-manifests", "--slot", "1")...)
	if run.code != 0 || len(run.calls) != 0 || strings.Count(run.stdout, "kind: Deployment") != 1 ||
		len(jobsIn(t, run.stdout)) != 1 || !strings.Contains(run.stdout, "namespace: maul-preview-1") ||
		!strings.Contains(run.stdout, "namespace: maul-agents") {
		t.Errorf("--print-manifests: exit %d, %d calls, stdout:\n%s", run.code, len(run.calls), run.stdout)
	}
}
