// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/previewtester"
)

// kubeletNode is the Node the fake kubelet registers and the pods it runs
// are bound to.
const kubeletNode = "e2e-kubelet"

// fakeAgentScript is the fake agent-runner the kubelet runs in place of the
// agent container's image: it speaks every stage's event stream.
const fakeAgentScript = "../hack/fake-agent/agent-runner"

// kubelet stands in for the node agent Jobs run on. envtest runs no
// kubelet, so a Job a controller creates would never run, and the leg a
// Job's output drives (collect, transcript, changeset push, pull request)
// could not be driven end to end. It plays the kubelet's part at the
// kubelet's own edge, and nothing inside the binaries changes:
//
//   - It registers a Node whose kubelet endpoint is its own TLS server, so
//     the real API server serves pods/log by proxying to it, as it proxies
//     to a real kubelet: the controllers read logs through the clientset,
//     unchanged.
//   - For each Job carrying its run kind, it does what the Job's pod would.
//     The prepare step fetches the artifact, checks its digest and unpacks
//     it as the working tree (repo/), and copies the handoff from the
//     per-Job Secret into the workspace's input/. On a plan Job with trees
//     (a multi-repository intent's), it then fetches each tree the Secret's
//     trees file lists, checks its digest, unpacks it under repos/<key>, and
//     stages the repositories manifest beside the handoff, as the trees
//     script does. The agent container is hack/fake-agent, run with the
//     container's own environment, plus whatever a test set with
//     setAgentEnv: what the stand-in agent decides in a model's place.
//   - It records the pod, bound to the Node, with its containers'
//     terminated statuses, then the Job's terminal status, as the kubelet
//     and the Job controller would.
//
// Jobs of any other run kind are left alone, never run, as without it.
type kubelet struct {
	t       *testing.T
	cl      *cluster
	runKind string
	script  string
	work    string
	srv     *httptest.Server

	mu       sync.Mutex
	logs     map[string][]byte // "<namespace>/<pod>/<container>"
	ran      map[string]bool   // Job names already run
	runs     []agentRun        // in the order they ran
	agentEnv map[string]string // set by setAgentEnv
	// runner is the real agent-runner binary, and runnerPhases the phases
	// whose Jobs it runs in hack/fake-agent's place (useAgentRunner).
	runner       string
	runnerPhases map[string]bool
	// tokens, when set (useBrokerTokens), mints each agent-runner Job's
	// caller token as the kubelet projects it: bound to the Job's pod,
	// which then exists while the agent runs, for the audience the Job's
	// projected volume names. Unset, the token is a placeholder no broker
	// would accept.
	tokens kubernetes.Interface
	// testers is the preview tester side (runTesters), nil until a test
	// turns it on.
	testers *testerSide
	// holds are the agent phases whose Jobs wait (hold), each until its
	// channel is closed; waiting are the Jobs waiting on one now.
	holds   map[string]chan struct{}
	waiting map[string]bool
}

// hold makes every Job of phase the kubelet reaches from now on wait before
// its Pod starts, so its run stays Running with no result, until release
// is called (or the test ends). The kubelet runs one Job at a time, so the
// Jobs after it wait too.
func (k *kubelet) hold(t *testing.T, phase string) (release func()) {
	t.Helper()
	ch := make(chan struct{})
	k.mu.Lock()
	if k.holds == nil {
		k.holds, k.waiting = map[string]chan struct{}{}, map[string]bool{}
	}
	k.holds[phase] = ch
	k.mu.Unlock()
	var once sync.Once
	release = func() {
		once.Do(func() {
			k.mu.Lock()
			delete(k.holds, phase)
			k.mu.Unlock()
			close(ch)
		})
	}
	t.Cleanup(release)
	return release
}

// held reports whether the Job named name is waiting on a hold.
func (k *kubelet) held(name string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.waiting[name]
}

// waitHeld waits while phase is held.
func (k *kubelet) waitHeld(ctx context.Context, name, phase string) error {
	k.mu.Lock()
	ch, ok := k.holds[phase]
	if ok {
		k.waiting[name] = true
	}
	k.mu.Unlock()
	if !ok {
		return nil
	}
	defer func() {
		k.mu.Lock()
		delete(k.waiting, name)
		k.mu.Unlock()
	}()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// fakeClaudeScript and fakeCodexScript are the scripted claude and codex
// CLIs the real agent-runner drives in the model's place (useAgentRunner).
const (
	fakeClaudeScript = "../hack/fake-agent/claude"
	fakeCodexScript  = "../hack/fake-agent/codex"
)

// useBrokerTokens makes the kubelet give every Job it runs on the real
// agent-runner a real caller token: the pod is created before its agent
// runs, and a token bound to it, for the audience the Job's projected
// volume names, is minted through cs, as the kubelet projects one. The real
// egress broker then admits the run's model requests by TokenReview.
func (k *kubelet) useBrokerTokens(cs kubernetes.Interface) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.tokens = cs
}

// brokerToken mints the caller token the kubelet would project into pod,
// or "" when the kubelet mints none (useBrokerTokens) or the pod projects
// none.
func (k *kubelet) brokerToken(ctx context.Context, pod *corev1.Pod) (string, error) {
	k.mu.Lock()
	cs := k.tokens
	k.mu.Unlock()
	if cs == nil {
		return "", nil
	}
	for _, v := range pod.Spec.Volumes {
		if v.Projected == nil {
			continue
		}
		for _, src := range v.Projected.Sources {
			if src.ServiceAccountToken == nil {
				continue
			}
			ttl := int64(3600)
			tr, err := cs.CoreV1().ServiceAccounts(pod.Namespace).CreateToken(ctx, pod.Spec.ServiceAccountName,
				&authnv1.TokenRequest{Spec: authnv1.TokenRequestSpec{
					Audiences:         []string{src.ServiceAccountToken.Audience},
					ExpirationSeconds: &ttl,
					BoundObjectRef: &authnv1.BoundObjectReference{
						Kind: "Pod", APIVersion: "v1", Name: pod.Name, UID: pod.UID},
				}}, metav1.CreateOptions{})
			if err != nil {
				return "", fmt.Errorf("mint the caller token of pod %s: %w", pod.Name, err)
			}
			return tr.Status.Token, nil
		}
	}
	return "", nil
}

// useAgentRunner makes the kubelet run, for every Job of the given phases
// from now on, the REAL agent-runner (cmd/agent-runner, built once) with
// hack/fake-agent/claude as its claude CLI, instead of hack/fake-agent
// whole: the stage flow around the CLI — the report check and its repair in
// the same session, the tree guard, commit.sh and the changeset — is then
// the product's own. setAgentEnv still reaches the agent, so a test chooses
// what the scripted CLI does.
func (k *kubelet) useAgentRunner(t *testing.T, phases ...string) {
	t.Helper()
	bin := build(t, "agent-runner")
	k.mu.Lock()
	defer k.mu.Unlock()
	k.runner = bin
	if k.runnerPhases == nil {
		k.runnerPhases = map[string]bool{}
	}
	for _, p := range phases {
		k.runnerPhases[p] = true
	}
}

// mintsTokens reports whether the kubelet mints real caller tokens
// (useBrokerTokens).
func (k *kubelet) mintsTokens() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.tokens != nil
}

// agentRunnerFor is the agent-runner binary to run a Job of phase with, or
// "" for hack/fake-agent.
func (k *kubelet) agentRunnerFor(phase string) string {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.runnerPhases[phase] {
		return k.runner
	}
	return ""
}

// agentRun is one Job as the kubelet ran it: the Job, the handoff its agent
// was given, and what the agent printed.
type agentRun struct {
	Job batchv1.Job
	// Image is the agent container's image.
	Image string
	// Env is the agent container's environment, literal values only.
	Env map[string]string
	// Issue and Investigation are the handoff files; HasInvestigation
	// says whether the Secret carried investigation.md at all.
	Issue            []byte
	Investigation    []byte
	HasInvestigation bool
	// Trees and Repositories are a plan Job's trees file and repositories
	// manifest, nil on any other Job.
	Trees        []byte
	Repositories []byte
	// Workspace is the directory the pod's /workspace stood for: the
	// working tree at repo/, any trees under repos/, the handoff in input/.
	Workspace string
	Stdout    []byte
	ExitCode  int
}

// setAgentEnv sets name to value in the environment of every agent the
// kubelet runs from now on, after the container's own: a test's way to
// make the fake agent decide what a model would (which repositories a plan
// changes, say).
func (k *kubelet) setAgentEnv(name, value string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.agentEnv == nil {
		k.agentEnv = map[string]string{}
	}
	k.agentEnv[name] = value
}

// startKubelet registers the Node and runs every Job of runKind the
// controllers create, until the test ends.
func startKubelet(t *testing.T, cl *cluster, runKind string) *kubelet {
	t.Helper()
	script, err := filepath.Abs(fakeAgentScript)
	if err != nil {
		t.Fatal(err)
	}
	k := &kubelet{
		t: t, cl: cl, runKind: runKind, script: script, work: t.TempDir(),
		logs: map[string][]byte{}, ran: map[string]bool{},
	}
	k.srv = httptest.NewTLSServer(http.HandlerFunc(k.serveLogs))
	t.Cleanup(k.srv.Close)
	k.registerNode(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go k.loop(ctx, done)
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return k
}

// registerNode creates the Node the API server proxies log reads to: its
// kubelet endpoint is the fake's TLS listener. With no kubelet CA
// configured, the API server does not verify the kubelet's certificate, as
// it would not verify a real one's.
func (k *kubelet) registerNode(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	_, portText, err := net.SplitHostPort(k.srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: kubeletNode}}
	if err := k.cl.client.Create(ctx, node); err != nil {
		t.Fatalf("register node: %v", err)
	}
	node.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "127.0.0.1"}}
	node.Status.DaemonEndpoints.KubeletEndpoint.Port = int32(port)
	if err := k.cl.client.Status().Update(ctx, node); err != nil {
		t.Fatalf("register node status: %v", err)
	}
}

// serveLogs is the kubelet's containerLogs endpoint, which the API server's
// pods/log proxies to.
func (k *kubelet) serveLogs(w http.ResponseWriter, r *http.Request) {
	parts := splitPath(r.URL.Path)
	if len(parts) != 4 || parts[0] != "containerLogs" {
		http.NotFound(w, r)
		return
	}
	ns, pod, container := parts[1], parts[2], parts[3]
	k.mu.Lock()
	body, ok := k.logs[ns+"/"+pod+"/"+container]
	k.mu.Unlock()
	if !ok {
		http.Error(w, "container "+container+" of "+pod+" has no log", http.StatusNotFound)
		return
	}
	_, _ = w.Write(body)
}

// splitPath splits a URL path into its non-empty segments.
func splitPath(p string) []string {
	return slices.DeleteFunc(strings.Split(p, "/"), func(s string) bool { return s == "" })
}

// loop runs each new Job of the kubelet's run kind, one at a time.
func (k *kubelet) loop(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		var list batchv1.JobList
		if err := k.cl.client.List(ctx, &list, client.InNamespace(agentsNS),
			client.MatchingLabels{v1alpha1.LabelRunKind: k.runKind}); err != nil {
			continue
		}
		for i := range list.Items {
			job := &list.Items[i]
			k.mu.Lock()
			seen := k.ran[job.Name]
			k.mu.Unlock()
			if seen || !job.DeletionTimestamp.IsZero() {
				continue
			}
			if err := k.run(ctx, job); err != nil {
				if ctx.Err() != nil {
					return
				}
				k.t.Errorf("kubelet: run job %s: %v", job.Name, err)
			}
			k.mu.Lock()
			k.ran[job.Name] = true
			k.mu.Unlock()
		}
	}
}

// run does what job's pod would, then records the pod and the Job's end.
func (k *kubelet) run(ctx context.Context, job *batchv1.Job) error {
	started := metav1.Now()
	spec := job.Spec.Template.Spec
	prepare, agent, err := jobContainers(&spec)
	if err != nil {
		return err
	}
	var secret corev1.Secret
	if err := k.cl.client.Get(ctx, types.NamespacedName{Namespace: job.Namespace, Name: job.Name}, &secret); err != nil {
		// A Job deleted since it was listed never gets a pod, and its
		// Secret goes with it.
		var cur batchv1.Job
		if apierrors.IsNotFound(err) &&
			(apierrors.IsNotFound(k.cl.client.Get(ctx, client.ObjectKeyFromObject(job), &cur)) ||
				!cur.DeletionTimestamp.IsZero()) {
			return nil
		}
		return fmt.Errorf("read the handoff secret: %w", err)
	}
	rec := agentRun{Job: *job.DeepCopy(), Image: agent.Image, Env: literalEnv(agent.Env)}
	rec.Issue = secret.Data["issue.md"]
	rec.Investigation, rec.HasInvestigation = secret.Data["investigation.md"]
	rec.Trees, rec.Repositories = secret.Data[secretTrees], secret.Data[secretRepositories]

	// A Job of a held phase waits, its Job created and no Pod yet, as one
	// whose Pod is still pulling its image would.
	if err := k.waitHeld(ctx, job.Name, rec.Env["MAUL_PHASE"]); err != nil {
		return err
	}

	// The prepare step: the digest-verified working tree, the handoff, then
	// any trees.
	workspace, err := os.MkdirTemp(k.work, job.Name+"-")
	if err != nil {
		return err
	}
	rec.Workspace = workspace
	prepareExit := int32(0)
	if err := prepareWorkspace(ctx, workspace, literalEnv(prepare.Env), &secret); err != nil {
		k.t.Errorf("kubelet: job %s: prepare: %v", job.Name, err)
		prepareExit = 1
	}

	// The agent container, when the prepare step let it start. A Job whose
	// agent gets a real caller token has its pod first, the token bound to it.
	var pod *corev1.Pod
	if prepareExit == 0 {
		if bin := k.agentRunnerFor(rec.Env["MAUL_PHASE"]); bin != "" {
			token := ""
			if k.mintsTokens() {
				if pod, err = k.createPod(ctx, job); err != nil {
					return err
				}
				if token, err = k.brokerToken(ctx, pod); err != nil {
					return err
				}
			}
			rec.Stdout, rec.ExitCode, err = k.runAgentRunner(ctx, bin, workspace, rec.Env, token)
		} else {
			rec.Stdout, rec.ExitCode, err = k.runAgent(ctx, workspace, rec.Env)
		}
		if err != nil {
			return err
		}
	}

	if pod == nil {
		if pod, err = k.createPod(ctx, job); err != nil {
			return err
		}
	}
	k.mu.Lock()
	k.logs[pod.Namespace+"/"+pod.Name+"/"+agent.Name] = rec.Stdout
	k.mu.Unlock()
	if err := k.finishPod(ctx, pod, prepare, agent, started, prepareExit, rec.ExitCode); err != nil {
		return err
	}
	if err := k.finishJob(ctx, job, started, prepareExit == 0 && rec.ExitCode == 0); err != nil {
		return err
	}
	k.mu.Lock()
	k.runs = append(k.runs, rec)
	k.mu.Unlock()
	return nil
}

// jobContainers returns the Job's prepare init container and its agent
// container.
func jobContainers(spec *corev1.PodSpec) (prepare, agent *corev1.Container, err error) {
	for i := range spec.InitContainers {
		if spec.InitContainers[i].Name == "prepare" {
			prepare = &spec.InitContainers[i]
		}
	}
	for i := range spec.Containers {
		if spec.Containers[i].Name == "agent" {
			agent = &spec.Containers[i]
		}
	}
	if prepare == nil || agent == nil {
		return nil, nil, errors.New("the pod template has no prepare init container or no agent container")
	}
	return prepare, agent, nil
}

// literalEnv is a container's environment, the literal values only (a
// secretKeyRef never reaches an intent Job; a Finding Job's is not run).
func literalEnv(env []corev1.EnvVar) map[string]string {
	out := make(map[string]string, len(env))
	for _, e := range env {
		if e.ValueFrom == nil {
			out[e.Name] = e.Value
		}
	}
	return out
}

// The per-Job Secret's keys a plan Job with trees carries beside its
// handoff (internal/jobs' trees.go): the trees file, one
// "<key> <sha256> <artifact URL>" line per tree, and the repositories
// manifest the prepare step stages for the agent.
const (
	secretTrees        = "trees"
	secretRepositories = "repositories"
)

// prepareWorkspace does the prepare init's work in workspace: the Job's own
// artifact, fetched and digest-verified, unpacked as the working tree
// (repo/); the handoff copied into input/; and on a Job with trees, each
// tree fetched, verified and unpacked into its own new directory under
// repos/, then the repositories manifest staged in input/. Like the init,
// it stops at the first failure, and the agent never starts.
func prepareWorkspace(ctx context.Context, workspace string, env map[string]string, secret *corev1.Secret) error {
	url, digest := env["MAUL_ARTIFACT_URL"], env["MAUL_ARTIFACT_DIGEST"]
	if url == "" || digest == "" {
		return errors.New("the Job names no artifact URL or digest")
	}
	raw, err := fetchArtifact(ctx, url, digest)
	if err != nil {
		return err
	}
	if err := untarGz(raw, filepath.Join(workspace, "repo")); err != nil {
		return fmt.Errorf("unpack the working tree: %w", err)
	}
	if err := writeHandoff(workspace, secret); err != nil {
		return err
	}
	trees, ok := secret.Data[secretTrees]
	if !ok {
		return nil
	}
	for line := range strings.Lines(string(trees)) {
		fields := strings.Fields(line)
		if len(fields) != 3 || !treeKey.MatchString(fields[0]) {
			return fmt.Errorf("trees line %q is not \"<key> <sha256> <url>\"", strings.TrimSpace(line))
		}
		key, digest, url := fields[0], fields[1], fields[2]
		dir := filepath.Join(workspace, "repos", key)
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return err
		}
		if err := os.Mkdir(dir, 0o755); err != nil {
			return fmt.Errorf("tree %s: %w", key, err) // a key listed twice
		}
		raw, err := fetchArtifact(ctx, url, digest)
		if err != nil {
			return fmt.Errorf("tree %s: %w", key, err)
		}
		if err := untarGz(raw, dir); err != nil {
			return fmt.Errorf("unpack tree %s: %w", key, err)
		}
	}
	manifest, ok := secret.Data[secretRepositories]
	if !ok {
		return errors.New("the Job lists trees but no repositories manifest")
	}
	return os.WriteFile(filepath.Join(workspace, "input", secretRepositories), manifest, 0o600)
}

// treeKey is a Project repository key, the only directory name a tree gets.
var treeKey = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// fetchArtifact is the prepare step's fetch: the artifact at url, refused
// unless its bytes hash to want, the hex digest the Job pins.
func fetchArtifact(ctx context.Context, url, want string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: status %d, %v", url, resp.StatusCode, err)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(raw)); got != want {
		return nil, fmt.Errorf("artifact %s hashes to %s, not the pinned %s", url, got, want)
	}
	return raw, nil
}

// untarGz unpacks a tarball into dir, its top-level directory stripped, as
// `tar -xz --strip-components=1` does: GitHub's archives hold the tree under
// one "<owner>-<repo>-<sha7>/" directory. Only directories and regular files
// are written, and nothing outside dir.
func untarGz(raw []byte, dir string) error {
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		_, rel, ok := strings.Cut(hdr.Name, "/")
		if !ok || rel == "" {
			continue // the top-level directory itself
		}
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if !strings.HasPrefix(path, filepath.Clean(dir)+string(filepath.Separator)) {
			return fmt.Errorf("tar entry %q leaves the tree", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			body, err := io.ReadAll(tr)
			if err != nil {
				return err
			}
			if err := os.WriteFile(path, body, 0o644); err != nil {
				return err
			}
		}
	}
}

// writeHandoff copies the per-Job Secret's files into the workspace's
// input/, as the prepare step does.
func writeHandoff(workspace string, secret *corev1.Secret) error {
	input := filepath.Join(workspace, "input")
	if err := os.MkdirAll(input, 0o755); err != nil {
		return err
	}
	for _, name := range []string{"issue.md", "investigation.md"} {
		body, ok := secret.Data[name]
		if !ok {
			continue
		}
		if err := os.WriteFile(filepath.Join(input, name), body, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// runAgent runs the fake agent with the agent container's environment,
// pointed at the workspace, and returns its stdout and exit code.
func (k *kubelet) runAgent(ctx context.Context, workspace string, env map[string]string) ([]byte, int, error) {
	cmd := exec.CommandContext(ctx, "sh", k.script)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	for name, value := range env {
		switch name {
		case "PATH", "HOME", "MAUL_WORKSPACE":
			continue // the pod's own; the workspace is this run's directory
		}
		cmd.Env = append(cmd.Env, name+"="+value)
	}
	k.mu.Lock()
	for name, value := range k.agentEnv {
		cmd.Env = append(cmd.Env, name+"="+value)
	}
	k.mu.Unlock()
	cmd.Env = append(cmd.Env, "HOME="+workspace, "MAUL_WORKSPACE="+workspace, "MAUL_FAKE_TURN_DELAY=0")
	return runContainer(cmd, "the fake agent")
}

// runContainer runs an agent container's process and returns its stdout and
// exit code; an exit status is the container's, not an error.
func runContainer(cmd *exec.Cmd, what string) ([]byte, int, error) {
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		return stdout.Bytes(), exit.ExitCode(), nil
	case err != nil:
		return nil, 0, fmt.Errorf("run %s: %w: %s", what, err, stderr.String())
	}
	return stdout.Bytes(), 0, nil
}

// runAgentRunner runs the real agent-runner (bin) as the agent container,
// pointed at the workspace, with the scripted claude CLI in the model's
// place. It first does the rest of the prepare init's work the fake agent
// never needed: the synthetic base commit over the working tree. The pod
// paths outside the workspace — the injected binaries' directory and the
// projected caller token — are stood in for by a directory beside it.
func (k *kubelet) runAgentRunner(ctx context.Context, bin, workspace string, env map[string]string,
	callerToken string) ([]byte, int, error) {
	repo := filepath.Join(workspace, "repo")
	gitEnv := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + workspace, "GIT_CONFIG_NOSYSTEM=1"}
	for _, args := range [][]string{
		{"init", "-q"},
		{"add", "-A"},
		{"-c", "user.name=maul", "-c", "user.email=maul@invalid", "-c", "commit.gpgsign=false",
			"commit", "-qm", "base " + env["MAUL_BASE_SHA"]},
		{"checkout", "-q", "--detach", "HEAD"},
	} {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir, cmd.Env = repo, gitEnv
		if out, err := cmd.CombinedOutput(); err != nil {
			return nil, 0, fmt.Errorf("the synthetic base commit: git %s: %w: %s", strings.Join(args, " "), err, out)
		}
	}

	pod := workspace + "-pod"
	binDir := filepath.Join(pod, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return nil, 0, err
	}
	for name, path := range map[string]string{"claude": fakeClaudeScript, "codex": fakeCodexScript} {
		script, err := os.ReadFile(path)
		if err != nil {
			return nil, 0, err
		}
		if err := os.WriteFile(filepath.Join(binDir, name), script, 0o755); err != nil {
			return nil, 0, err
		}
	}
	if callerToken == "" {
		callerToken = "e2e-projected-caller-token"
	}
	token := filepath.Join(pod, "token")
	if err := os.WriteFile(token, []byte(callerToken+"\n"), 0o600); err != nil {
		return nil, 0, err
	}

	cmd := exec.CommandContext(ctx, bin)
	cmd.Env = []string{"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH")}
	for name, value := range env {
		switch name {
		case "PATH", "HOME", "MAUL_WORKSPACE", "MAUL_BIN_DIR", "MAUL_BROKER_TOKEN_FILE":
			continue // the pod's own paths; stood in for below
		}
		cmd.Env = append(cmd.Env, name+"="+value)
	}
	if env["MAUL_BIN_DIR"] != "" {
		cmd.Env = append(cmd.Env, "MAUL_BIN_DIR="+binDir)
	}
	if env["MAUL_BROKER_TOKEN_FILE"] != "" {
		cmd.Env = append(cmd.Env, "MAUL_BROKER_TOKEN_FILE="+token)
	}
	k.mu.Lock()
	for name, value := range k.agentEnv {
		cmd.Env = append(cmd.Env, name+"="+value)
	}
	k.mu.Unlock()
	cmd.Env = append(cmd.Env, "HOME="+workspace, "MAUL_WORKSPACE="+workspace)
	return runContainer(cmd, "agent-runner")
}

// createPod records the Job's pod, bound to the Node, as the Job controller
// and the scheduler would.
func (k *kubelet) createPod(ctx context.Context, job *batchv1.Job) (*corev1.Pod, error) {
	labels := map[string]string{}
	for key, value := range job.Spec.Template.Labels {
		labels[key] = value
	}
	labels["batch.kubernetes.io/job-name"] = job.Name
	labels["batch.kubernetes.io/controller-uid"] = string(job.UID)
	spec := job.Spec.Template.Spec.DeepCopy()
	spec.NodeName = kubeletNode
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            job.Name + "-e2e",
			Namespace:       job.Namespace,
			Labels:          labels,
			Annotations:     job.Spec.Template.Annotations,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(job, batchv1.SchemeGroupVersion.WithKind("Job"))},
		},
		Spec: *spec,
	}
	if err := k.cl.client.Create(ctx, pod); err != nil {
		return nil, fmt.Errorf("create pod: %w", err)
	}
	return pod, nil
}

// finishPod records the pod's end: the prepare step terminated with its
// exit code, then the agent container with its own, or still waiting when
// the prepare step failed and it never started.
func (k *kubelet) finishPod(ctx context.Context, pod *corev1.Pod, prepare, agent *corev1.Container,
	started metav1.Time, prepareExit int32, agentExit int) error {
	now := metav1.Now()
	terminated := func(code int32) corev1.ContainerState {
		reason := "Completed"
		if code != 0 {
			reason = "Error"
		}
		return corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: code, Reason: reason, StartedAt: started, FinishedAt: now,
		}}
	}
	pod.Status = corev1.PodStatus{
		Phase:     corev1.PodSucceeded,
		StartTime: &started,
		InitContainerStatuses: []corev1.ContainerStatus{{
			Name: prepare.Name, Image: prepare.Image, ImageID: prepare.Image, State: terminated(prepareExit),
		}},
	}
	agentStatus := corev1.ContainerStatus{Name: agent.Name, Image: agent.Image, ImageID: agent.Image}
	switch {
	case prepareExit != 0:
		pod.Status.Phase = corev1.PodFailed
		agentStatus.State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}
	default:
		agentStatus.State = terminated(int32(agentExit))
		if agentExit != 0 {
			pod.Status.Phase = corev1.PodFailed
		}
	}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{agentStatus}
	if err := k.cl.client.Status().Update(ctx, pod); err != nil {
		return fmt.Errorf("record pod status: %w", err)
	}
	return nil
}

// finishJob records the Job's terminal status as the Job controller would.
func (k *kubelet) finishJob(ctx context.Context, job *batchv1.Job, started metav1.Time, succeeded bool) error {
	var cur batchv1.Job
	if err := k.cl.client.Get(ctx, client.ObjectKeyFromObject(job), &cur); err != nil {
		return fmt.Errorf("read job: %w", err)
	}
	now := metav1.Now()
	cur.Status.StartTime = &started
	cond := func(typ batchv1.JobConditionType, reason string) batchv1.JobCondition {
		return batchv1.JobCondition{
			Type: typ, Status: corev1.ConditionTrue, Reason: reason, LastProbeTime: now, LastTransitionTime: now,
		}
	}
	if succeeded {
		cur.Status.Succeeded = 1
		cur.Status.CompletionTime = &now
		cur.Status.Conditions = []batchv1.JobCondition{
			cond(batchv1.JobSuccessCriteriaMet, "CompletionsReached"),
			cond(batchv1.JobComplete, "CompletionsReached"),
		}
	} else {
		cur.Status.Failed = 1
		cur.Status.Conditions = []batchv1.JobCondition{
			cond(batchv1.JobFailureTarget, "BackoffLimitExceeded"),
			cond(batchv1.JobFailed, "BackoffLimitExceeded"),
		}
	}
	if err := k.cl.client.Status().Update(ctx, &cur); err != nil {
		return fmt.Errorf("record job status: %w", err)
	}
	return nil
}

// Runs returns every Job the kubelet ran so far, in order.
func (k *kubelet) Runs() []agentRun {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]agentRun(nil), k.runs...)
}

// waitRun waits for a run matching match and returns it.
func (k *kubelet) waitRun(t *testing.T, why string, match func(agentRun) bool) agentRun {
	t.Helper()
	var found agentRun
	eventually(t, why, func() bool {
		for _, r := range k.Runs() {
			if match(r) {
				found = r
				return true
			}
		}
		return false
	})
	return found
}

// phaseIs matches a run whose agent ran phase (its MAUL_PHASE).
func phaseIs(phase string) func(agentRun) bool {
	return func(r agentRun) bool { return r.Env["MAUL_PHASE"] == phase }
}

// tarGz is a repository archive as GitHub serves one: files under a single
// top-level "<top>/" directory, gzipped.
func tarGz(t *testing.T, top string, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, name := range slices.Sorted(maps.Keys(files)) {
		body := files[name]
		if err := tw.WriteHeader(&tar.Header{Name: top + "/" + name, Mode: 0o644, Size: int64(len(body)),
			Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// labelVerificationUID marks a preview tester Job, and its Pod, with the
// UID of the PreviewVerification it runs (preview-controller's render.go).
const labelVerificationUID = "maul.run/preview-verification-uid"

// testerSide is the kubelet's part for preview tester Jobs: in the slot
// namespaces, a Job preview-controller rendered for a PreviewVerification
// runs the real preview-tester, built with the e2edial tag, with the Job's
// own MAUL_VERIFY_SPEC. envtest has no kube-proxy or cluster DNS, so each
// slot Service address the spec targets is dialled at the loopback listener
// of the fake component the test serves under that component's key
// (MAUL_E2E_DIAL_MAP), and the termination message is a file of the run's
// own (MAUL_E2E_TERMINATION_PATH), read back into the container's
// terminated state as the kubelet reads /dev/termination-log. Its stdout is
// the container log pods/log serves preview-controller.
type testerSide struct {
	bin        string
	namespaces []string

	// components are the fake components' loopback addresses by key.
	components map[string]string
	// faults are the outcomes the next runs are forced to, in order
	// (testerNoSummary); an empty queue runs the tester.
	faults []string
	runs   []testerRun
}

// testerNoSummary is a forced outcome: the tester's Pod ran and exited 1
// having printed nothing, as one killed before its summary would.
const testerNoSummary = "no-summary"

// testerRun is one tester Job as the kubelet ran it.
type testerRun struct {
	Job batchv1.Job
	// Spec is the MAUL_VERIFY_SPEC the Job carried.
	Spec     string
	Stdout   []byte
	Message  string
	ExitCode int
}

// runTesters turns the tester side on for Jobs in the slot namespaces
// given, from now on, until the test ends.
func (k *kubelet) runTesters(t *testing.T, namespaces ...string) {
	t.Helper()
	bin := buildTagged(t, "preview-tester", "e2edial")
	k.mu.Lock()
	k.testers = &testerSide{bin: bin, namespaces: namespaces, components: map[string]string{}}
	k.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go k.testerLoop(ctx, done)
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

// serveComponent serves h as the preview component of key: every tester
// request to the Service of that component reaches it.
func (k *kubelet) serveComponent(t *testing.T, key string, h http.Handler) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	k.mu.Lock()
	defer k.mu.Unlock()
	k.testers.components[key] = srv.Listener.Addr().String()
}

// failNextTester forces the next tester run's outcome (testerNoSummary).
func (k *kubelet) failNextTester(outcome string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.testers.faults = append(k.testers.faults, outcome)
}

// TesterRuns returns every tester Job the kubelet ran so far, in order.
func (k *kubelet) TesterRuns() []testerRun {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.testers == nil {
		return nil
	}
	return slices.Clone(k.testers.runs)
}

// testerLoop runs each new tester Job, one at a time.
func (k *kubelet) testerLoop(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		k.mu.Lock()
		namespaces := slices.Clone(k.testers.namespaces)
		k.mu.Unlock()
		for _, ns := range namespaces {
			var list batchv1.JobList
			if err := k.cl.client.List(ctx, &list, client.InNamespace(ns),
				client.HasLabels{labelVerificationUID}); err != nil {
				continue
			}
			for i := range list.Items {
				job := &list.Items[i]
				k.mu.Lock()
				seen := k.ran[ns+"/"+job.Name]
				k.mu.Unlock()
				if seen || !job.DeletionTimestamp.IsZero() {
					continue
				}
				if err := k.runTester(ctx, job); err != nil {
					if ctx.Err() != nil {
						return
					}
					k.t.Errorf("kubelet: run tester job %s/%s: %v", ns, job.Name, err)
				}
				k.mu.Lock()
				k.ran[ns+"/"+job.Name] = true
				k.mu.Unlock()
			}
		}
	}
}

// runTester does what the tester Job's Pod would, then records the Pod and
// the Job's end.
func (k *kubelet) runTester(ctx context.Context, job *batchv1.Job) error {
	started := metav1.Now()
	spec := job.Spec.Template.Spec
	if len(spec.Containers) != 1 {
		return fmt.Errorf("the tester pod template has %d containers, want 1", len(spec.Containers))
	}
	container := spec.Containers[0]
	env := literalEnv(container.Env)
	rec := testerRun{Job: *job.DeepCopy(), Spec: env["MAUL_VERIFY_SPEC"]}

	k.mu.Lock()
	var fault string
	if len(k.testers.faults) > 0 {
		fault, k.testers.faults = k.testers.faults[0], k.testers.faults[1:]
	}
	k.mu.Unlock()
	if fault == testerNoSummary {
		rec.ExitCode = 1
	} else {
		var err error
		if rec.Stdout, rec.Message, rec.ExitCode, err = k.execTester(ctx, rec.Spec); err != nil {
			return err
		}
	}

	pod, err := k.createPod(ctx, job)
	if err != nil {
		return err
	}
	k.mu.Lock()
	k.logs[pod.Namespace+"/"+pod.Name+"/"+container.Name] = rec.Stdout
	k.mu.Unlock()
	reason, phase := "Completed", corev1.PodSucceeded
	if rec.ExitCode != 0 {
		reason, phase = "Error", corev1.PodFailed
	}
	pod.Status = corev1.PodStatus{Phase: phase, StartTime: &started,
		ContainerStatuses: []corev1.ContainerStatus{{Name: container.Name, Image: container.Image,
			ImageID: container.Image, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: int32(rec.ExitCode), Reason: reason, Message: rec.Message, StartedAt: started,
				FinishedAt: metav1.Now()}}}}}
	if err := k.cl.client.Status().Update(ctx, pod); err != nil {
		return fmt.Errorf("record tester pod status: %w", err)
	}
	if err := k.finishJob(ctx, job, started, rec.ExitCode == 0); err != nil {
		return err
	}
	k.mu.Lock()
	k.testers.runs = append(k.testers.runs, rec)
	k.mu.Unlock()
	return nil
}

// execTester runs the e2edial preview-tester on spec, each target the spec
// names dialled at its fake component (a target without one is refused by
// the tester's dial map, as an unreachable Service), and returns its stdout,
// termination message and exit code.
func (k *kubelet) execTester(ctx context.Context, spec string) ([]byte, string, int, error) {
	var parsed struct {
		Targets map[string]string `json:"targets"`
	}
	if err := json.Unmarshal([]byte(spec), &parsed); err != nil {
		return nil, "", 0, fmt.Errorf("decode the tester spec: %w", err)
	}
	dial := map[string]string{}
	k.mu.Lock()
	for key, addr := range parsed.Targets {
		if to, ok := k.testers.components[key]; ok {
			dial[addr] = to
		}
	}
	bin := k.testers.bin
	k.mu.Unlock()
	dialMap, err := json.Marshal(dial)
	if err != nil {
		return nil, "", 0, err
	}
	term := filepath.Join(k.work, fmt.Sprintf("termination-%d", time.Now().UnixNano()))
	cmd := exec.CommandContext(ctx, bin)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "MAUL_VERIFY_SPEC=" + spec,
		"MAUL_E2E_DIAL_MAP=" + string(dialMap), "MAUL_E2E_TERMINATION_PATH=" + term}
	stdout, code, err := runContainer(cmd, "preview-tester")
	if err != nil {
		return nil, "", 0, err
	}
	msg, err := os.ReadFile(term)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, "", 0, err
	}
	return stdout, string(msg), code, nil
}

// TestKubelet checks the stand-in itself against the real API server: a Job
// whose agent succeeds ends Complete and one whose agent fails ends Failed,
// each with its pod's terminated statuses, and either way the agent's log is
// what pods/log serves; a Job of another run kind is never run; and a plan
// Job with trees gets each tree, digest-verified, in its own directory and
// the repositories manifest beside its handoff, as the trees script stages
// them, so the agent's check of every listed tree passes.
func TestKubelet(t *testing.T) {
	const runKind = "intent"
	cl := startCluster(t)
	k := startKubelet(t, cl, runKind)
	ctx := context.Background()
	artifact := tarGz(t, "acme-shop-0000000", map[string]string{"README.md": "# acme/shop\n"})
	apiTree := tarGz(t, "acme-api-0000000",
		map[string]string{"README.md": "# acme/api\n", "cmd/api/main.go": "package main\n"})
	const manifest = "shop /workspace/repo https://github.example/acme/shop\n" +
		"api /workspace/repos/api https://github.example/acme/api\n"
	art := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api" {
			_, _ = w.Write(apiTree)
			return
		}
		_, _ = w.Write(artifact)
	}))
	t.Cleanup(art.Close)
	digest := func(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }
	cfg, err := clientcmd.BuildConfigFromFlags("", cl.kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name, kind, phase string
		handoff           map[string]string
		wantCondition     batchv1.JobConditionType
		wantPod           corev1.PodPhase
		wantLog           string
		// wantFiles are files the workspace holds once the Job ran.
		wantFiles map[string]string
	}{
		{"an agent that succeeds", runKind, "plan", map[string]string{"issue.md": "# a request\n"},
			batchv1.JobComplete, corev1.PodSucceeded, `"type":"plan"`,
			map[string]string{"repo/README.md": "# acme/shop\n", "input/issue.md": "# a request\n"}},
		// agent-runner refuses a build handed a request; so does the fake.
		{"an agent that fails", runKind, "build",
			map[string]string{"issue.md": "# a request\n", "investigation.md": "---\n---\n"},
			batchv1.JobFailed, corev1.PodFailed, `"type":"fatal"`, nil},
		{"another run kind", string(v1alpha1.RunKindInvestigation), "investigate",
			map[string]string{"issue.md": "# a finding\n"}, "", "", "", nil},
		{"a plan with trees", runKind, "plan", map[string]string{
			"issue.md":         "# a request\n",
			secretTrees:        "api " + digest(apiTree) + " " + art.URL + "/api\n",
			secretRepositories: manifest,
		}, batchv1.JobComplete, corev1.PodSucceeded, `"type":"plan"`, map[string]string{
			"repo/README.md":              "# acme/shop\n",
			"repos/api/README.md":         "# acme/api\n",
			"repos/api/cmd/api/main.go":   "package main\n",
			"input/" + secretRepositories: manifest,
		}},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			name := fmt.Sprintf("kubelet-%d", i)
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: agentsNS},
				StringData: tc.handoff}
			if err := cl.client.Create(ctx, secret); err != nil {
				t.Fatal(err)
			}
			job := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: agentsNS,
					Labels: map[string]string{v1alpha1.LabelRunKind: tc.kind}},
				Spec: batchv1.JobSpec{BackoffLimit: new(int32(0)), Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						RestartPolicy: corev1.RestartPolicyNever,
						InitContainers: []corev1.Container{{Name: "prepare", Image: "tools", Env: []corev1.EnvVar{
							{Name: "MAUL_ARTIFACT_URL", Value: art.URL},
							{Name: "MAUL_ARTIFACT_DIGEST", Value: digest(artifact)},
						}}},
						Containers: []corev1.Container{{Name: "agent", Image: "agent", Env: []corev1.EnvVar{
							{Name: "MAUL_PHASE", Value: tc.phase},
							{Name: "MAUL_REPO", Value: "acme/shop"},
						}}},
					},
				}},
			}
			if err := cl.client.Create(ctx, job); err != nil {
				t.Fatal(err)
			}

			if tc.wantCondition == "" {
				consistently(t, "a job of another run kind to be left alone", func() bool {
					var pods corev1.PodList
					return cl.client.List(ctx, &pods, client.InNamespace(agentsNS),
						client.MatchingLabels{"batch.kubernetes.io/job-name": name}) == nil && len(pods.Items) == 0
				})
				return
			}
			ran := k.waitRun(t, "the kubelet to run "+name, func(r agentRun) bool { return r.Job.Name == name })
			for path, want := range tc.wantFiles {
				if got, err := os.ReadFile(filepath.Join(ran.Workspace, filepath.FromSlash(path))); err != nil ||
					string(got) != want {
					t.Errorf("workspace %s = %q (%v), want %q", path, got, err, want)
				}
			}
			var cur batchv1.Job
			if err := cl.client.Get(ctx, client.ObjectKeyFromObject(job), &cur); err != nil {
				t.Fatal(err)
			}
			if !slices.ContainsFunc(cur.Status.Conditions, func(c batchv1.JobCondition) bool {
				return c.Type == tc.wantCondition && c.Status == corev1.ConditionTrue
			}) {
				t.Errorf("job conditions = %+v, want %s", cur.Status.Conditions, tc.wantCondition)
			}
			var pods corev1.PodList
			if err := cl.client.List(ctx, &pods, client.InNamespace(agentsNS),
				client.MatchingLabels{"batch.kubernetes.io/job-name": name}); err != nil || len(pods.Items) != 1 {
				t.Fatalf("pods of %s = %d (%v), want 1", name, len(pods.Items), err)
			}
			pod := pods.Items[0]
			if pod.Status.Phase != tc.wantPod || pod.Spec.NodeName != kubeletNode {
				t.Errorf("pod = %s on %q, want %s on %s", pod.Status.Phase, pod.Spec.NodeName, tc.wantPod, kubeletNode)
			}
			stream, err := cs.CoreV1().Pods(agentsNS).GetLogs(pod.Name, &corev1.PodLogOptions{Container: "agent"}).Stream(ctx)
			if err != nil {
				t.Fatalf("pods/log: %v", err)
			}
			defer func() { _ = stream.Close() }()
			logs, err := io.ReadAll(stream)
			if err != nil || !strings.Contains(string(logs), tc.wantLog) {
				t.Errorf("pods/log = %q (%v), want the agent's %s event", logs, err, tc.wantLog)
			}
		})
	}
}

// checkTesterJob asserts a tester Job the kubelet ran ended with the
// condition want, with one Pod labelled with the verification uid,
// controlled by the Job and bound to the Node, and returns that Pod.
func checkTesterJob(t *testing.T, cl *cluster, job *batchv1.Job, uid string,
	want batchv1.JobConditionType) corev1.Pod {
	t.Helper()
	ctx := context.Background()
	var cur batchv1.Job
	if err := cl.client.Get(ctx, client.ObjectKeyFromObject(job), &cur); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(cur.Status.Conditions, func(c batchv1.JobCondition) bool {
		return c.Type == want && c.Status == corev1.ConditionTrue
	}) {
		t.Errorf("job conditions = %+v, want %s", cur.Status.Conditions, want)
	}
	var pods corev1.PodList
	if err := cl.client.List(ctx, &pods, client.InNamespace(job.Namespace),
		client.MatchingLabels{labelVerificationUID: uid}); err != nil || len(pods.Items) != 1 {
		t.Fatalf("tester pods = %d (%v), want 1", len(pods.Items), err)
	}
	pod := pods.Items[0]
	if owner := metav1.GetControllerOf(&pod); owner == nil || owner.UID != cur.UID || pod.Spec.NodeName != kubeletNode {
		t.Errorf("tester pod owner %v on %q, want the Job on %s", owner, pod.Spec.NodeName, kubeletNode)
	}
	return pod
}

// TestKubeletTester checks the tester side of the stand-in: a tester Job in
// a slot runs the real e2edial preview-tester, whose request to the spec's
// Service address reaches the fake component served for its target; the
// Job ends Complete, its Pod (the Job's, bound to the Node, labelled with
// the verification) carries the termination message, and pods/log serves
// the tester's MAUL-VERIFY lines. A forced no-summary run prints nothing,
// exits 1 and fails the Job; a Job without the verification label is never
// run.
func TestKubeletTester(t *testing.T) {
	cl := startCluster(t)
	k := startKubelet(t, cl, "intent")
	const slot = "maul-preview-0"
	ctx := context.Background()
	if err := cl.client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: slot}}); err != nil {
		t.Fatal(err)
	}
	k.runTesters(t, slot)
	var hits sync.Map
	k.serveComponent(t, "shop", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Store(r.URL.Path, true)
		_, _ = io.WriteString(w, "0.1.0")
	}))
	spec, err := previewtester.Spec{Round: 1, Attempt: 1, Run: "a1b2c3", BudgetSeconds: 45,
		Criteria: acceptance.Normalize([]acceptance.Criterion{{ID: "version-served", Kind: acceptance.KindHTTP,
			Statement: "GET /version answers", Target: "shop",
			Steps: []acceptance.Step{{Request: acceptance.Request{Method: "GET", Path: "/version"},
				Expect: acceptance.Expect{Status: acceptance.StatusList{200}}}}}}),
		Targets: map[string]string{"shop": "preview-shop-1:80"}}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", cl.kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	testerJob := func(name string, labels map[string]string) *batchv1.Job {
		return &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: slot, Labels: labels},
			Spec: batchv1.JobSpec{BackoffLimit: new(int32(0)), Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{Name: "tester", Image: "tester@sha256:" + strings.Repeat("1", 64),
						Env: []corev1.EnvVar{{Name: "MAUL_VERIFY_SPEC", Value: string(spec)}}}}},
			}},
		}
	}

	tests := []struct {
		name, fault   string
		wantCondition batchv1.JobConditionType
		wantLog       string
		wantMessage   bool
	}{
		{"a run", "", batchv1.JobComplete, "MAUL-VERIFY:", true},
		{"a run killed before its summary", testerNoSummary, batchv1.JobFailed, "", false},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.fault != "" {
				k.failNextTester(tc.fault)
			}
			uid := fmt.Sprintf("uid-%d", i)
			job := testerJob(fmt.Sprintf("preview-shop-1-vt-%d", i), map[string]string{labelVerificationUID: uid})
			if err := cl.client.Create(ctx, job); err != nil {
				t.Fatal(err)
			}
			var ran testerRun
			eventually(t, "the tester job to run", func() bool {
				for _, r := range k.TesterRuns() {
					if r.Job.Name == job.Name {
						ran = r
						return true
					}
				}
				return false
			})
			if string(spec) != ran.Spec {
				t.Errorf("tester ran spec %q, want the Job's", ran.Spec)
			}
			pod := checkTesterJob(t, cl, job, uid, tc.wantCondition)
			term := pod.Status.ContainerStatuses[0].State.Terminated
			if term == nil || (term.Message != "") != tc.wantMessage {
				t.Errorf("tester termination = %+v, want a message %v", term, tc.wantMessage)
			}
			stream, err := cs.CoreV1().Pods(slot).GetLogs(pod.Name, &corev1.PodLogOptions{Container: "tester"}).
				Stream(ctx)
			if err != nil {
				t.Fatalf("pods/log: %v", err)
			}
			defer func() { _ = stream.Close() }()
			logs, err := io.ReadAll(stream)
			if err != nil || (tc.wantLog == "") != (len(logs) == 0) || !strings.Contains(string(logs), tc.wantLog) {
				t.Errorf("pods/log = %q (%v), want %q", logs, err, tc.wantLog)
			}
		})
	}
	if _, ok := hits.Load("/version"); !ok {
		t.Error("the tester's request never reached the component")
	}

	unlabelled := testerJob("preview-shop-1-other", map[string]string{"app": "other"})
	if err := cl.client.Create(ctx, unlabelled); err != nil {
		t.Fatal(err)
	}
	consistently(t, "a Job without the verification label left alone", func() bool {
		var pods corev1.PodList
		return cl.client.List(ctx, &pods, client.InNamespace(slot),
			client.MatchingLabels{"batch.kubernetes.io/job-name": unlabelled.Name}) == nil && len(pods.Items) == 0
	})
}
