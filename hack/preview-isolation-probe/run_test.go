// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package probe_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// The site these tests name: an image in account 222222222222, which is not
// the account of any credentials the run has (it has none; aws is a stub).
const (
	imageAccount = "222222222222"
	imageRepo    = "maul/previews/hello-web"
	image        = imageAccount + ".dkr.ecr.us-east-1.amazonaws.com/" + imageRepo
	headSHA      = "0123456789abcdef0123456789abcdef01234567"
	imageDigest  = "sha256:89abcdef0123456789abcdef0123456789abcdef0123456789abcdef01234567"
)

// stub stands in for gh, aws and kubectl. It logs each call's argv, one
// tab-separated line per call, and answers the two GitHub reads and the ECR
// read from the environment. Every kubectl call fails, so a run that gets
// past the GitHub and ECR checks stops at its first cluster read, the
// NodeClass check, having created nothing.
const stub = `#!/bin/sh
name=${0##*/}
{ printf '%s' "$name"; for arg in "$@"; do printf '\t%s' "$arg"; done; printf '\n'; } >>"$STUB_LOG"
case "$name $1 $2" in
"gh repo view") printf '%s\n' "$STUB_REPO_JSON" ;;
"gh pr view") printf '%s\n' "$STUB_PR_JSON" ;;
"aws ecr describe-images") printf '%s\n' "$STUB_DIGEST" ;;
*) exit 1 ;;
esac
`

// probeRun is one run.sh run against the stubs.
type probeRun struct {
	calls          [][]string // each stub call's argv, the command name first
	stdout, stderr string
	code           int
}

// called returns the argv of every call to name with first argument verb.
func (r probeRun) called(name, verb string) [][]string {
	var out [][]string
	for _, call := range r.calls {
		if call[0] == name && len(call) > 1 && call[1] == verb {
			out = append(out, call)
		}
	}
	return out
}

// runProbe runs run.sh with args, gh answering repoJSON for the repository
// and prJSON for the PR. The caller's PROBE_* variables are dropped so they
// cannot leak into the run.
func runProbe(t *testing.T, repoJSON, prJSON string, args ...string) probeRun {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not on PATH")
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH; run.sh requires it")
	}
	bin := t.TempDir()
	for _, name := range []string{"gh", "aws", "kubectl"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(stub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	log := filepath.Join(t.TempDir(), "calls")
	env := []string{
		"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"STUB_LOG=" + log,
		"STUB_REPO_JSON=" + repoJSON,
		"STUB_PR_JSON=" + prJSON,
		"STUB_DIGEST=" + imageDigest,
	}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "PATH=") && !strings.HasPrefix(kv, "PROBE_") && !strings.HasPrefix(kv, "STUB_") {
			env = append(env, kv)
		}
	}
	cmd := exec.Command(bash, append([]string{"run.sh"}, args...)...)
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	var run probeRun
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("run.sh: %v", err)
		}
		run.code = exit.ExitCode()
	}
	run.stdout, run.stderr = stdout.String(), stderr.String()
	logged, err := os.ReadFile(log)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(strings.TrimSuffix(string(logged), "\n"), "\n") {
		if line != "" {
			run.calls = append(run.calls, strings.Split(line, "\t"))
		}
	}
	return run
}

// siteArgs names the test site; the PR number comes last.
func siteArgs(extra ...string) []string {
	site := []string{"--repository", "acme/hello-web", "--image", image, "--taint-key", "preview.example.com/preview-only"}
	return slices.Concat(site, extra, []string{"7"})
}

func repoJSON(defaultBranch string) string {
	return `{"defaultBranchRef":{"name":"` + defaultBranch + `"}}`
}

func prJSON(base string) string {
	return `{"state":"OPEN","isCrossRepository":false,"headRefName":"test/preview-isolation",` +
		`"headRefOid":"` + headSHA + `","baseRefName":"` + base + `"}`
}

// flag returns the value after name in argv, or "" when it is absent.
func flag(argv []string, name string) string {
	if i := slices.Index(argv, name); i >= 0 && i+1 < len(argv) {
		return argv[i+1]
	}
	return ""
}

// TestImageLookupIsInTheImageAccount: the ECR check must look in the account
// named in --image, which the probe Pod pulls from. Regression: it passed
// only --region, so it read the default registry of whichever account the
// caller's credentials were in, and the gate could pass or fail against a
// registry the cluster never pulls from.
func TestImageLookupIsInTheImageAccount(t *testing.T) {
	run := runProbe(t, repoJSON("main"), prJSON("main"), siteArgs()...)
	lookups := run.called("aws", "ecr")
	if len(lookups) != 1 {
		t.Fatalf("aws ecr calls = %q, want one describe-images; stderr:\n%s", lookups, run.stderr)
	}
	got := lookups[0]
	for name, want := range map[string]string{
		"--registry-id":     imageAccount,
		"--region":          "us-east-1",
		"--repository-name": imageRepo,
		"--image-ids":       "imageTag=sha-" + headSHA,
	} {
		if v := flag(got, name); v != want {
			t.Errorf("describe-images %s = %q, want %q (argv %q)", name, v, want, got)
		}
	}
	// The digest was accepted and the run went on to its first cluster read.
	if !strings.Contains(run.stderr, "Using disposable PR #7 head "+headSHA+" ("+imageDigest+")") ||
		!strings.Contains(run.stderr, "preview NodeClass is not DefaultDeny") || run.code != 1 {
		t.Errorf("run did not reach the NodeClass check after the lookup: exit %d, stderr:\n%s", run.code, run.stderr)
	}
}

// TestPRMustBeIntoTheDefaultBranch: the disposable PR must be open into the
// repository's own default branch, read from GitHub. Regression: the guard
// required a base of "main", so a repository whose default branch is another
// name could never be probed, and nothing could get past that.
func TestPRMustBeIntoTheDefaultBranch(t *testing.T) {
	for _, tc := range []struct {
		name, defaultBranch, base string
		want                      string // "" when the guard admits the PR
	}{
		{"main into main", "main", "main", ""},
		{"trunk into trunk", "trunk", "trunk", ""},
		{"master into master", "master", "master", ""},
		{"trunk default, into main", "trunk", "main", "test/preview-* PR into trunk"},
		{"main default, into a release branch", "main", "release/1.x", "test/preview-* PR into main"},
		{"no default branch", "", "main", "could not read the default branch of acme/hello-web"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := runProbe(t, repoJSON(tc.defaultBranch), prJSON(tc.base), siteArgs()...)
			if repos := run.called("gh", "repo"); len(repos) != 1 || flag(repos[0], "view") != "acme/hello-web" {
				t.Errorf("gh repo calls = %q, want one view of acme/hello-web", repos)
			}
			lookups := run.called("aws", "ecr")
			if tc.want == "" {
				if len(lookups) != 1 {
					t.Errorf("PR into %s with default %s was refused: exit %d, stderr:\n%s",
						tc.base, tc.defaultBranch, run.code, run.stderr)
				}
				return
			}
			if run.code != 1 || !strings.Contains(run.stderr, tc.want) {
				t.Errorf("exit %d, stderr:\n%s\nwant exit 1 with %q", run.code, run.stderr, tc.want)
			}
			if len(lookups) != 0 {
				t.Errorf("a refused PR still reached ECR: %q", lookups)
			}
		})
	}
}

// TestDryRunNamesTheImageAccountAndCallsNothing: the dry run prints the
// account the lookup will be bound to and calls none of gh, aws or kubectl.
func TestDryRunNamesTheImageAccountAndCallsNothing(t *testing.T) {
	run := runProbe(t, repoJSON("main"), prJSON("main"), siteArgs("--dry-run")...)
	if run.code != 0 {
		t.Fatalf("dry run exit %d, stderr:\n%s", run.code, run.stderr)
	}
	if want := "(account " + imageAccount + ", region us-east-1)"; !strings.Contains(run.stdout, want) {
		t.Errorf("dry run printed:\n%s\nwant the registry line to say %q", run.stdout, want)
	}
	if len(run.calls) != 0 {
		t.Errorf("dry run called %q", run.calls)
	}
}

// edgesStub stands in for kubectl in run.sh's edges run. It logs each call's
// argv (after --request-timeout), keeps what each create from stdin was
// sent, and answers a cluster where every probe object starts: the slot
// policy exists, no Deployment is in either slot (STUB_DEPLOYMENTS is what
// a slot holds), every Service and Pod has the address
// STUB_IP_<svc|pod>_<name> names (dashes as underscores), the fixture's
// policies read back as STUB_EDGES_JSON, and each exec'd connect prints
// the summary STUB_RULES gives "<app> <target>" (a line "<app> <target>
// <outcome>"): REACHED for a listener's own loopback and BLOCKED
// otherwise. STUB_RESTART_APP's Pod reads as restarted from its second
// read on.
const edgesStub = `#!/bin/sh
case "$1" in --request-timeout=*) shift ;; esac
{ printf 'kubectl'; for arg in "$@"; do printf '\t%s' "$arg"; done; printf '\n'; } >>"$STUB_LOG"
ip() { eval "printf '%s' \"\${STUB_IP_$(printf '%s' "$1" | tr -- '-' '_')}\""; }
case "$1 $2" in
"get networkpolicy")
    case "$*" in
    *maul-isolation-probe*) printf '%s\n' "$STUB_EDGES_JSON" ;;
    *preview-isolation*) printf 'networkpolicy.networking.k8s.io/preview-isolation\n' ;;
    *) exit 1 ;;
    esac ;;
"get deployments") printf '%s' "$STUB_DEPLOYMENTS" ;;
"get service") ip "svc-$3" ;;
"get pods")
    for a in "$@"; do case "$a" in app=*) app=${a#app=} ;; esac; done
    n=$(cat "$STUB_DIR/reads-$app" 2>/dev/null || echo 0)
    n=$((n + 1))
    echo "$n" >"$STUB_DIR/reads-$app"
    restarts=0
    if [ "$app" = "$STUB_RESTART_APP" ] && [ "$n" -gt 1 ]; then restarts=1; fi
    printf '%s-p %s %s\n' "$app" "$(ip "pod-$app")" "$restarts" ;;
"get nodes") ;;
"get nodeclaims") printf '{"items":[]}\n' ;;
"create -n" | "create -f")
    for last in "$@"; do :; done
    if [ "$last" = - ]; then cat >>"$STUB_CREATED"; printf -- '---\n' >>"$STUB_CREATED"; fi ;;
"exec -n")
    pod=$4
    for target in "$@"; do :; done
    outcome=BLOCKED
    case "$target" in
    127.0.0.1:*) outcome=REACHED ;;
    *) rule=$(grep -- "^${pod%-p} $target " "$STUB_RULES" | tail -1)
       if [ -n "$rule" ]; then outcome=${rule##* }; fi ;;
    esac
    printf '{"name":"slot-pod","outcome":"blocked"}\n{"name":"summary","outcome":"%s"}\n' "$outcome" ;;
"wait "* | "delete "*) ;;
*) exit 1 ;;
esac
`

// failStub stands in for gh and aws, which the edges run never calls.
const failStub = `#!/bin/sh
{ printf '%s' "${0##*/}"; for arg in "$@"; do printf '\t%s' "$arg"; done; printf '\n'; } >>"$STUB_LOG"
exit 1
`

// edgesIPs are the edges site's addresses: each fixture part's Pod and
// Service, the other slot's Pod and the release's Services.
var edgesIPs = map[string]string{
	"pod-preview-edges-tester": "10.42.0.10", "pod-preview-edges-web": "10.42.0.11",
	"pod-preview-edges-api": "10.42.0.12", "pod-preview-edges-render": "10.42.0.13",
	"pod-preview-edges-other": "10.42.1.20",
	"svc-preview-edges-web":   "10.43.0.11", "svc-preview-edges-api": "10.43.0.12",
	"svc-preview-edges-render": "10.43.0.13", "svc-kubernetes": "10.43.1.1",
	"svc-maul-egress-broker": "10.43.1.2", "svc-maul-integration-controller": "10.43.1.3",
	"svc-maul-source-controller": "10.43.1.4", "svc-maul-status-server": "10.43.1.5",
}

// declaredRules are a correct cluster's: the tester reaches each
// component's Service, and nothing else connects.
func declaredRules() []string {
	return []string{"preview-edges-tester 10.43.0.11:80 REACHED", "preview-edges-tester 10.43.0.12:80 REACHED",
		"preview-edges-tester 10.43.0.13:80 REACHED"}
}

// edgesJSON is edges.yaml as the API server lists it.
func edgesJSON(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("edges.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var items []map[string]any
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(b), 4096)
	for {
		var obj map[string]any
		if err := dec.Decode(&obj); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		items = append(items, obj)
	}
	out, err := json.Marshal(map[string]any{"items": items})
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// edgesRun is one run.sh edges run against the stubs.
type edgesRun struct {
	probeRun
	created string
}

// runEdges runs run.sh edges with args against edgesStub, its connects
// answered by rules; extra is more environment for the stub.
func runEdges(t *testing.T, rules, extra []string, args ...string) edgesRun {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not on PATH")
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH; run.sh requires it")
	}
	bin, dir := t.TempDir(), t.TempDir()
	for name, script := range map[string]string{"kubectl": edgesStub, "gh": failStub, "aws": failStub} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	rulesFile := filepath.Join(dir, "rules")
	if err := os.WriteFile(rulesFile, []byte(strings.Join(rules, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	log, created := filepath.Join(dir, "calls"), filepath.Join(dir, "created")
	env := []string{
		"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"STUB_LOG=" + log, "STUB_CREATED=" + created, "STUB_DIR=" + dir, "STUB_RULES=" + rulesFile,
		"STUB_EDGES_JSON=" + edgesJSON(t),
	}
	for name, ip := range edgesIPs {
		env = append(env, "STUB_IP_"+strings.ReplaceAll(name, "-", "_")+"="+ip)
	}
	env = append(env, extra...)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "PATH=") && !strings.HasPrefix(kv, "PROBE_") && !strings.HasPrefix(kv, "STUB_") {
			env = append(env, kv)
		}
	}
	cmd := exec.Command(bash, append([]string{"run.sh", "edges"}, args...)...)
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	var run edgesRun
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("run.sh edges: %v", err)
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

const edgesImage = "registry.example/maul/previews/netprobe:sha-" + headSHA

func edgesArgs(extra ...string) []string {
	return slices.Concat([]string{"--image", edgesImage, "--node-selector", "maul.example.com/previews=true",
		"--taint-key", "maul.example.com/preview-only", "--timeout", "2"}, extra)
}

// connects is every connect the run exec'd, as "<namespace> <app> <flags
// and target>", the Pod's suffix stripped.
func (r edgesRun) connects() []string {
	var out []string
	for _, call := range r.calls {
		if call[0] != "kubectl" || call[1] != "exec" {
			continue
		}
		i := slices.Index(call, "connect")
		out = append(out, call[3]+" "+strings.TrimSuffix(call[4], "-p")+" "+strings.Join(call[i+1:], " "))
	}
	return out
}

// wantConnects is the connects one run in slot makes, with other the other
// slot: each listener's loopback before and after, the declared edges
// through each component's Service, every other pair of parts and every
// part to the other slot's Pod, and the tester to the API server, maul's
// Services and the instance metadata service.
func wantConnects(slot, other string, refused bool) []string {
	flag := ""
	if refused {
		flag = "--refused-is-blocked "
	}
	parts := []string{"api", "render", "tester", "web"}
	var out []string
	for range 2 {
		for _, p := range parts {
			out = append(out, slot+" preview-edges-"+p+" 127.0.0.1:8080")
		}
		out = append(out, other+" preview-edges-other 127.0.0.1:8080")
	}
	for _, to := range []string{"api", "render", "web"} {
		out = append(out, slot+" preview-edges-tester "+edgesIPs["svc-preview-edges-"+to]+":80")
	}
	for _, from := range parts {
		for _, to := range parts {
			if from != to && from != "tester" {
				out = append(out, slot+" preview-edges-"+from+" "+flag+edgesIPs["pod-preview-edges-"+to]+":8080")
			}
		}
		out = append(out, slot+" preview-edges-"+from+" "+flag+"10.42.1.20:8080")
	}
	for _, target := range []string{"10.43.1.1:443", "10.43.1.2:8080", "10.43.1.3:8080", "10.43.1.4:9790",
		"10.43.1.5:8080", "169.254.169.254:80"} {
		out = append(out, slot+" preview-edges-tester "+flag+target)
	}
	return out
}

// TestEdgesPasses: three runs, in slots 0, 1 and 0, each creating the
// fixture's policies, Services and stand-in Pods, then requiring each
// declared edge to connect through its Service and every other pair to be
// refused; gh and aws are never called.
func TestEdgesPasses(t *testing.T) {
	for _, refused := range []bool{false, true} {
		t.Run(fmt.Sprintf("refused-is-blocked=%v", refused), func(t *testing.T) {
			var args []string
			if refused {
				args = append(args, "--refused-is-blocked")
			}
			run := runEdges(t, declaredRules(), nil, edgesArgs(args...)...)
			if run.code != 0 || !strings.Contains(run.stderr, "Edges isolation passed") {
				t.Fatalf("exit %d, stderr:\n%s", run.code, run.stderr)
			}
			for _, call := range run.calls {
				if call[0] != "kubectl" {
					t.Errorf("the edges run called %q", call)
				}
			}
			got := run.connects()
			want := make([]string, 0, len(got))
			for _, pair := range [][2]string{{"0", "1"}, {"1", "0"}, {"0", "1"}} {
				want = append(want, wantConnects("maul-preview-"+pair[0], "maul-preview-"+pair[1], refused)...)
			}
			slices.Sort(got)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("connects:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
			fixture := 0
			for _, call := range run.calls {
				if call[1] == "create" && call[len(call)-1] != "-" {
					fixture++
					if call[len(call)-1] != mustAbs(t, "edges.yaml") {
						t.Errorf("created %q, want the fixture", call)
					}
				}
			}
			if fixture != 3 {
				t.Errorf("the fixture was created %d times, want once a run", fixture)
			}
			if c := strings.Count(run.created, "kind: Deployment"); c != 15 {
				t.Errorf("created %d Deployments, want five a run", c)
			}
			if c := strings.Count(run.created, "kind: Service\n"); c != 9 {
				t.Errorf("created %d Services, want three a run", c)
			}
		})
	}
}

func mustAbs(t *testing.T, path string) string {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

// TestEdgesFails: an undeclared pair that connects, a declared edge that
// never does, a refusal without --refused-is-blocked and a stand-in that
// restarted each fail the run, which cleans up after itself.
func TestEdgesFails(t *testing.T) {
	for name, tc := range map[string]struct {
		rules, extra, args []string
		want               string
	}{
		"a reverse edge": {rules: append(declaredRules(), "preview-edges-web 10.42.0.10:8080 REACHED"),
			want: "FAIL maul-preview-0 web -> tester (10.42.0.10:8080): REACHED, want BLOCKED"},
		"a component pair": {rules: append(declaredRules(), "preview-edges-api 10.42.0.13:8080 REACHED"),
			want: "FAIL maul-preview-0 api -> render (10.42.0.13:8080): REACHED, want BLOCKED"},
		"the other slot": {rules: append(declaredRules(), "preview-edges-web 10.42.1.20:8080 REACHED"),
			want: "FAIL maul-preview-0 web -> other-slot (10.42.1.20:8080): REACHED, want BLOCKED"},
		"the API server": {rules: append(declaredRules(), "preview-edges-tester 10.43.1.1:443 REACHED"),
			want: "FAIL maul-preview-0 tester -> kubernetes-api (10.43.1.1:443): REACHED, want BLOCKED"},
		"a closed edge": {rules: append(declaredRules(), "preview-edges-tester 10.43.0.12:80 BLOCKED"),
			want: "FAIL maul-preview-0 tester -> api (10.43.0.12:80): BLOCKED, want REACHED"},
		"an inconclusive refusal": {rules: append(declaredRules(),
			"preview-edges-web 10.42.0.12:8080 INCONCLUSIVE"),
			want: "FAIL maul-preview-0 web -> api (10.42.0.12:8080): INCONCLUSIVE, want BLOCKED"},
		"a restarted stand-in": {rules: declaredRules(), extra: []string{"STUB_RESTART_APP=preview-edges-api"},
			args: []string{"--refused-is-blocked"},
			want: "FAIL maul-preview-0 api: its Pod restarted or was replaced during the run"},
	} {
		t.Run(name, func(t *testing.T) {
			run := runEdges(t, tc.rules, tc.extra, edgesArgs(tc.args...)...)
			if run.code != 1 || !strings.Contains(run.stderr, tc.want) ||
				strings.Contains(run.stderr, "Edges isolation passed") {
				t.Errorf("exit %d, stderr:\n%s\nwant %q", run.code, run.stderr, tc.want)
			}
			deleted := false
			for _, call := range run.calls {
				deleted = deleted || (call[1] == "delete" && slices.Contains(call, "networkpolicy"))
			}
			if !deleted {
				t.Error("the fixture's policies were not deleted")
			}
		})
	}
}

// TestEdgesRefusesABusySlot: a slot holding any Deployment (a Preview's or
// a probe's) stops the run before anything is created.
func TestEdgesRefusesABusySlot(t *testing.T) {
	run := runEdges(t, declaredRules(), []string{"STUB_DEPLOYMENTS=deployment.apps/preview-shop\n"}, edgesArgs()...)
	if run.code != 1 || !strings.Contains(run.stderr, "is not free") || run.created != "" {
		t.Errorf("exit %d, created %q, stderr:\n%s", run.code, run.created, run.stderr)
	}
}

// TestEdgesColdPool: with --cold-node-pool every run first waits for that
// pool to have no node or claim.
func TestEdgesColdPool(t *testing.T) {
	run := runEdges(t, declaredRules(), nil, edgesArgs("--cold-node-pool", "maul-preview")...)
	if run.code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", run.code, run.stderr)
	}
	waits := 0
	for _, call := range run.calls {
		if call[1] == "get" && call[2] == "nodes" && slices.Contains(call, "karpenter.sh/nodepool=maul-preview") {
			waits++
		}
	}
	if waits != 3 {
		t.Errorf("waited for an empty pool %d times, want before each of the three runs", waits)
	}
}

// TestEdgesValidatesAndDryRunCallsNothing: every value is checked before
// anything runs, and --dry-run and --print-manifests call nothing.
func TestEdgesValidatesAndDryRunCallsNothing(t *testing.T) {
	for _, args := range [][]string{
		{"--image", "registry.example/maul/previews/netprobe:latest", "--no-node-isolation"},
		{"--image", edgesImage},
		{"--image", edgesImage, "--no-node-isolation", "--slots", "0,0,0"},
		{"--image", edgesImage, "--no-node-isolation", "--slots", "0,1,2"},
		{"--image", edgesImage, "--no-node-isolation", "--slots", "0;1"},
		{"--image", edgesImage, "--no-node-isolation", "--cold-node-pool", "Maul"},
		{"--image", edgesImage, "--no-node-isolation", "--release-namespace", "Maul"},
		{"--image", edgesImage, "--no-node-isolation", "--timeout", "0"},
		{"--image", edgesImage, "--no-node-isolation", "7"},
	} {
		run := runEdges(t, nil, nil, args...)
		if run.code != 2 || len(run.calls) != 0 {
			t.Errorf("%q: exit %d, %d calls; want a usage error", args, run.code, len(run.calls))
		}
	}
	run := runEdges(t, nil, nil, edgesArgs("--dry-run")...)
	if run.code != 0 || len(run.calls) != 0 || !strings.Contains(run.stdout, "nothing was called") ||
		!strings.Contains(run.stdout, "maul-preview-0, maul-preview-1, maul-preview-0") {
		t.Errorf("--dry-run: exit %d, %d calls, stdout:\n%s", run.code, len(run.calls), run.stdout)
	}
	run = runEdges(t, nil, nil, edgesArgs("--print-manifests")...)
	if run.code != 0 || len(run.calls) != 0 || strings.Count(run.stdout, "kind: Deployment") != 5 ||
		strings.Count(run.stdout, "kind: Service\n") != 3 || !strings.Contains(run.stdout, "namespace: maul-preview-1") {
		t.Errorf("--print-manifests: exit %d, %d calls, stdout:\n%s", run.code, len(run.calls), run.stdout)
	}
}

// TestEdgesStandInsCarryTheFixtureLabels: each part's stand-in carries the
// fixture's Preview UID and its part, the labels the policies select, and
// listens on the port every edge names.
func TestEdgesStandInsCarryTheFixtureLabels(t *testing.T) {
	var list struct{ Items []map[string]any }
	if err := json.Unmarshal([]byte(edgesJSON(t)), &list); err != nil {
		t.Fatal(err)
	}
	uids := map[string]bool{}
	for _, item := range list.Items {
		spec, _ := item["spec"].(map[string]any)
		sel, _ := spec["podSelector"].(map[string]any)
		labels, _ := sel["matchLabels"].(map[string]any)
		uid, _ := labels["maul.run/preview-uid"].(string)
		uids[uid] = true
	}
	if len(uids) != 1 || uids[""] {
		t.Fatalf("edges.yaml selects Preview UIDs %v, want one", uids)
	}
	uid := slices.Collect(maps.Keys(uids))[0]
	run := runEdges(t, nil, nil, edgesArgs("--print-manifests")...)
	parts := map[string]bool{}
	for doc := range strings.SplitSeq(run.stdout, "---\n") {
		if !strings.Contains(doc, "kind: Deployment") || strings.Contains(doc, "preview-edges-other") {
			continue
		}
		var d struct {
			Spec struct {
				Template struct {
					Metadata struct {
						Labels map[string]string `json:"labels"`
					} `json:"metadata"`
				} `json:"template"`
			} `json:"spec"`
		}
		if err := utilyaml.Unmarshal([]byte(doc), &d); err != nil {
			t.Fatal(err)
		}
		l := d.Spec.Template.Metadata.Labels
		if l["maul.run/preview-uid"] != uid || l["app"] != "preview-edges-"+l["maul.run/preview-part"] {
			t.Errorf("stand-in labels %v, want the fixture UID %s and app preview-edges-<part>", l, uid)
		}
		parts[l["maul.run/preview-part"]] = true
		if !strings.Contains(doc, "containerPort: 8080") || !strings.Contains(doc, "args: [listen]") {
			t.Errorf("stand-in does not listen on 8080:\n%s", doc)
		}
	}
	if got := slices.Sorted(maps.Keys(parts)); !slices.Equal(got, []string{"api", "render", "tester", "web"}) {
		t.Errorf("stand-ins for %q, want one per part", got)
	}
}
