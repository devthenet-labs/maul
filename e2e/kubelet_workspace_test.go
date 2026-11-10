// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/jobs"
	"github.com/devthenet-labs/maul/internal/previewtools"
	"github.com/devthenet-labs/maul/internal/previewtools/prepare"
	"github.com/devthenet-labs/maul/internal/toolexec"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// The kubelet's part for the agents-namespace tool pod (SPEC-S2 S2-22): the
// role tools Job intent-controller's agents host renders for a Project with
// no preview (a cli or library surface).
//
//   - The prepare init's work is the real engine's (internal/previewtools/
//     prepare): both tarballs fetched from the artifact server, verified
//     against their digests and extracted, each side's tree made readable.
//     Its strict probe does not run: envtest hosts have open egress, where
//     the probe rightly exits 78 (S2-21's live probe covers it).
//   - Each seed is the real prepare.Seed into that side's own work
//     directory; its UID check (the sandbox's) is the pod's, not run here.
//   - Each sandbox is the real internal/toolexec workspace server, in this
//     process, serving its side's exec socket. Two stand-ins only: its sweep
//     reads an empty proc directory (the host's /proc lists the suite's own
//     processes, which the sweep would kill: the pod's PID namespace is the
//     sandbox's alone), and a filter on its socket plays the sandbox's mount
//     namespace, mapping /work in every request (argv, files, environment)
//     to the side's directory and back in the output, and putting the
//     host's Go toolchain on the search path in place of the image's.
//   - The tools container is the REAL preview-tools built with e2edial, on
//     the tools Secret's data, with the trees and sockets in the kubelet's
//     directories (MAUL_E2E_TREES_DIR, MAUL_E2E_EXEC_DIR) and a loopback
//     listener; its pod is Ready, both sandboxes running and Ready, once its
//     readiness path answers. Once the relay exits, its stdout is the tools
//     container's log and its termination message the file it wrote.
//
// An evaluator reaches it as it reaches a slot tool pod (roleEnv): its
// hosts entry for maul-tools names the pod's address.

// workspaceSide is the kubelet's part for agents tool Jobs
// (runWorkspaceTools).
type workspaceSide struct {
	// short is a short directory for the exec sockets (a unix socket's
	// path is at most 108 bytes), goBin the host toolchain's directory and
	// goCache its build cache, which the sandboxes share.
	short, goBin, goCache string
	// procfs is the empty proc directory the sandboxes' sweeps read.
	procfs string
	// runs are the workspace tool Jobs finished so far, in order.
	runs []workspaceRun
}

// workspaceRun is one agents tool Job as the kubelet ran it.
type workspaceRun struct {
	toolRun
	// Trees are the pristine trees the relay read, by side; Requests every
	// program each side's sandbox was asked to run, in order.
	Trees    map[string]string
	Requests map[string][]toolexec.Request
}

// runWorkspaceTools turns the agents tool side on: every role tools Job in
// the agents namespace runs from now on, until the test ends.
func (k *kubelet) runWorkspaceTools(t *testing.T) {
	t.Helper()
	bin := buildTagged(t, "preview-tools", "e2edial")
	short, err := os.MkdirTemp("", "mx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(short) })
	ws := &workspaceSide{short: short, procfs: filepath.Join(short, "proc")}
	if err := os.Mkdir(ws.procfs, 0o755); err != nil {
		t.Fatal(err)
	}
	ws.goBin, ws.goCache = hostGo(t)
	k.mu.Lock()
	if k.tools == nil {
		k.tools = &toolSide{byIP: map[string]string{}}
	}
	k.tools.bin = bin
	k.workspace = ws
	k.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	done := make(chan struct{})
	go k.workspaceLoop(ctx, &wg, done)
	t.Cleanup(func() {
		cancel()
		<-done
		wg.Wait()
	})
}

// hostGo is the host toolchain's bin directory, standing in for the
// repository image's Go, and its build cache.
func hostGo(t *testing.T) (string, string) {
	t.Helper()
	out, err := exec.Command("go", "env", "GOROOT", "GOCACHE").Output()
	if err != nil {
		t.Fatalf("go env: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 2 {
		t.Fatalf("go env printed %q", out)
	}
	return filepath.Join(lines[0], "bin"), lines[1]
}

// WorkspaceRuns returns every agents tool Job the kubelet finished, in
// order.
func (k *kubelet) WorkspaceRuns() []workspaceRun {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.workspace == nil {
		return nil
	}
	return append([]workspaceRun(nil), k.workspace.runs...)
}

// toolPodIP is the address of the one tool pod running now, "" for none or
// several.
func toolPodIP(k *kubelet) string {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.tools == nil || len(k.tools.byIP) != 1 {
		return ""
	}
	for ip := range k.tools.byIP {
		return ip
	}
	return ""
}

// isWorkspaceToolJob reports an agents tool Job, which the agent loop
// leaves alone.
func isWorkspaceToolJob(job *batchv1.Job) bool {
	return job.Spec.Template.Labels[v1alpha1.LabelRole] == v1alpha1.RoleTools
}

// workspaceLoop starts each new agents tool Job in its own goroutine: it
// runs while the agent loop runs its evaluator.
func (k *kubelet) workspaceLoop(ctx context.Context, wg *sync.WaitGroup, done chan<- struct{}) {
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
			client.MatchingLabels{v1alpha1.LabelRole: v1alpha1.RoleTools}); err != nil {
			continue
		}
		for i := range list.Items {
			job := list.Items[i].DeepCopy()
			key := agentsNS + "/" + job.Name
			k.mu.Lock()
			seen := k.ran[key]
			k.ran[key] = true
			k.mu.Unlock()
			if seen || !job.DeletionTimestamp.IsZero() {
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := k.runWorkspacePod(ctx, job); err != nil && ctx.Err() == nil {
					k.t.Errorf("kubelet: run workspace tool job %s: %v", key, err)
				}
			}()
		}
	}
}

// wsPod is one agents tool pod while the kubelet runs it.
type wsPod struct {
	*toolPod
	// root holds the pod's volumes: config/, trees/<side>, work-<side>.
	root, exec string
	sandboxes  map[string]*wsSandbox
}

// runWorkspacePod does what the agents tool Job's pod would: prepare and
// the seeds, both sandboxes, then the relay until it exits; the pod and the
// Job end with it. A Job deleted while it runs stops it, as the pod's
// deletion would.
func (k *kubelet) runWorkspacePod(ctx context.Context, job *batchv1.Job) error {
	wp, err := k.mountWorkspace(ctx, job)
	if err != nil || wp == nil {
		return err
	}
	defer wp.stopSandboxes()
	trees, err := k.prepareWorkspaceTrees(ctx, wp)
	if err != nil {
		k.t.Logf("kubelet: workspace tool job %s: prepare: %v", job.Name, err)
		return k.failWorkspacePrepare(ctx, wp)
	}
	if err := k.startSandboxes(wp); err != nil {
		return err
	}
	if err := k.startRelay(wp.toolPod); err != nil {
		return err
	}
	if err := k.startToolPod(ctx, wp.toolPod); err != nil {
		wp.stop()
		return err
	}
	if exited, err := k.followToolPod(ctx, wp.toolPod); !exited || err != nil {
		wp.stop()
		return err
	}
	return k.endWorkspacePod(ctx, wp, trees)
}

// mountWorkspace waits for the tools Secret, as the kubelet starts a pod
// only once its Secret volume can be mounted, and writes its data into the
// pod's config directory. A Job deleted first never gets a pod: nil.
func (k *kubelet) mountWorkspace(ctx context.Context, job *batchv1.Job) (*wsPod, error) {
	var secret corev1.Secret
	key := types.NamespacedName{Namespace: job.Namespace, Name: job.Name + jobs.ToolsSecretSuffix}
	for {
		err := k.cl.client.Get(ctx, key, &secret)
		if err == nil {
			break
		}
		if !apierrors.IsNotFound(err) || ctx.Err() != nil || k.jobGone(ctx, job) {
			return nil, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	root, err := os.MkdirTemp(k.work, job.Name+"-tools-")
	if err != nil {
		return nil, err
	}
	k.mu.Lock()
	short := k.workspace.short
	k.mu.Unlock()
	execDir, err := os.MkdirTemp(short, "x")
	if err != nil {
		return nil, err
	}
	tp := &toolPod{job: job, started: metav1.Now(), dir: filepath.Join(root, "config")}
	tp.term = filepath.Join(root, "termination-log")
	if err := os.Mkdir(tp.dir, 0o755); err != nil {
		return nil, err
	}
	for name, value := range secret.Data {
		if err := os.WriteFile(filepath.Join(tp.dir, name), value, 0o444); err != nil {
			return nil, err
		}
	}
	if err := json.Unmarshal(secret.Data[previewtools.ConfigFile], &tp.cfg); err != nil {
		return nil, fmt.Errorf("decode the tool config: %w", err)
	}
	wp := &wsPod{toolPod: tp, root: root, exec: execDir, sandboxes: map[string]*wsSandbox{}}
	tp.env = []string{"MAUL_E2E_EXEC_DIR=" + execDir,
		"MAUL_E2E_TREES_DIR=" + filepath.Join(root, "trees"),
		"MAUL_E2E_SANDBOX_UID=" + strconv.Itoa(os.Getuid())}
	return wp, nil
}

// prepareArgs are the prepare init's arguments, by flag name.
func prepareArgs(job *batchv1.Job) (map[string]string, error) {
	for _, c := range job.Spec.Template.Spec.InitContainers {
		if c.Name != jobs.PrepareContainer {
			continue
		}
		if len(c.Args) == 0 || c.Args[0] != "prepare" {
			return nil, fmt.Errorf("the prepare init runs %q", c.Args)
		}
		fs := flag.NewFlagSet("prepare", flag.ContinueOnError)
		out := map[string]string{}
		for _, name := range []string{"head-url", "head-sha256", "base-url", "base-sha256", "trees", "bin"} {
			fs.Func(name, "", func(v string) error { out[name] = v; return nil })
		}
		if err := fs.Parse(c.Args[1:]); err != nil {
			return nil, err
		}
		return out, nil
	}
	return nil, errors.New("the tool pod has no prepare init")
}

// prepareWorkspaceTrees does prepare's and the seeds' work: each side's
// tarball fetched, verified and extracted by the real engine into the
// pod's trees, made readable, then copied into the side's work directory.
// It returns each side's pristine tree.
func (k *kubelet) prepareWorkspaceTrees(ctx context.Context, wp *wsPod) (map[string]string, error) {
	args, err := prepareArgs(wp.job)
	if err != nil {
		return nil, err
	}
	fetcher := prepare.Fetcher{Client: prepare.NewClient(), Attempts: 3}
	trees := map[string]string{}
	for _, side := range []string{jobs.ToolSideHead, jobs.ToolSideBase} {
		dst := filepath.Join(wp.root, "trees", side)
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return nil, err
		}
		if err := fetcher.Tree(ctx, args[side+"-url"], args[side+"-sha256"], wp.root, dst); err != nil {
			return nil, fmt.Errorf("tree %s: %w", side, err)
		}
		if err := prepare.Readable(dst); err != nil {
			return nil, err
		}
		work := filepath.Join(wp.root, "work-"+side)
		if err := os.Mkdir(work, 0o755); err != nil {
			return nil, err
		}
		if err := prepare.Seed(dst, work); err != nil {
			return nil, fmt.Errorf("seed %s: %w", side, err)
		}
		trees[side] = dst
	}
	return trees, nil
}

// failWorkspacePrepare records the pod with its prepare init failed and
// the Job failed, as a refused tree ends it.
func (k *kubelet) failWorkspacePrepare(ctx context.Context, wp *wsPod) error {
	pod, err := k.createPod(ctx, wp.job)
	if err != nil {
		return err
	}
	now := metav1.Now()
	pod.Status = corev1.PodStatus{Phase: corev1.PodFailed, StartTime: &wp.started,
		InitContainerStatuses: []corev1.ContainerStatus{{Name: jobs.PrepareContainer,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error",
				StartedAt: wp.started, FinishedAt: now}}}}}
	if err := k.cl.client.Status().Update(ctx, pod); err != nil {
		return err
	}
	return k.finishJob(ctx, wp.job, wp.started, false)
}

// endWorkspacePod records the relay's run as endToolPod does, with the
// trees and each sandbox's requests.
func (k *kubelet) endWorkspacePod(ctx context.Context, wp *wsPod, trees map[string]string) error {
	if err := k.endToolPod(ctx, wp.toolPod); err != nil {
		return err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	run := workspaceRun{Trees: trees, Requests: map[string][]toolexec.Request{}}
	for _, r := range k.tools.runs {
		if r.Job.UID == wp.job.UID {
			run.toolRun = r
		}
	}
	for side, sb := range wp.sandboxes {
		run.Requests[side] = sb.requests()
	}
	k.workspace.runs = append(k.workspace.runs, run)
	return nil
}

// startSandboxes serves each side's sandbox on its exec socket.
func (k *kubelet) startSandboxes(wp *wsPod) error {
	k.mu.Lock()
	ws := *k.workspace
	k.mu.Unlock()
	for _, side := range []string{jobs.ToolSideHead, jobs.ToolSideBase} {
		sb, err := startSandbox(ws, filepath.Join(wp.root, "work-"+side), filepath.Join(wp.exec, side))
		if err != nil {
			return fmt.Errorf("sandbox %s: %w", side, err)
		}
		wp.sandboxes[side] = sb
	}
	return nil
}

// stopSandboxes stops both sandboxes.
func (wp *wsPod) stopSandboxes() {
	for _, sb := range wp.sandboxes {
		_ = sb.hs.Close()
	}
}

// wsSandbox is one side's sandbox: the workspace shim over its work
// directory, behind the mount filter.
type wsSandbox struct {
	work, goBin string
	srv         *toolexec.Server
	hs          *http.Server

	mu   sync.Mutex
	reqs []toolexec.Request
}

// startSandbox serves the workspace shim of the side whose work directory
// (the pod's /work) is work, on <execDir>/sock.
func startSandbox(ws workspaceSide, work, execDir string) (*wsSandbox, error) {
	srv, err := toolexec.NewServer(toolexec.Options{Work: work, Home: filepath.Join(work, "home"),
		Cache: filepath.Join(work, "cache"), Tmp: filepath.Join(work, "tmp"), Procfs: ws.procfs})
	if err != nil {
		return nil, err
	}
	// The sandboxes share the host's build cache: content-addressed, it
	// holds nothing a side could pass the other but compiled packages.
	if ws.goCache != "" {
		if err := os.Symlink(ws.goCache, filepath.Join(work, "cache", "go-build")); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(execDir, 0o755); err != nil {
		return nil, err
	}
	ln, err := toolexec.Listen(filepath.Join(execDir, "sock"))
	if err != nil {
		return nil, err
	}
	sb := &wsSandbox{work: work, goBin: ws.goBin, srv: srv}
	mux := http.NewServeMux()
	mux.HandleFunc(toolexec.ExecPath, sb.serveExec)
	mux.Handle(toolexec.ReadyPath, srv.Handler())
	sb.hs = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = sb.hs.Serve(ln) }()
	return sb, nil
}

// serveExec is the shim's exec endpoint behind the mount filter: the
// request's /work is the side's directory, the image's Go the host's.
func (sb *wsSandbox) serveExec(w http.ResponseWriter, r *http.Request) {
	var req toolexec.Request
	if err := toolwire.DecodeRequest(r.Body, &req); err != nil {
		writeShimResult(w, http.StatusBadRequest, toolexec.Result{Refused: toolwire.RefusedInvalid,
			Reason: err.Error()})
		return
	}
	sb.mu.Lock()
	sb.reqs = append(sb.reqs, req)
	sb.mu.Unlock()
	mapped := sb.mount(req)
	res := sb.srv.Do(r.Context(), &mapped)
	res.Output = strings.ReplaceAll(res.Output, sb.work, toolexec.WorkDir)
	writeShimResult(w, http.StatusOK, res)
}

// mount maps req's /work paths to the side's directory, and puts the
// host's Go first on its search path.
func (sb *wsSandbox) mount(req toolexec.Request) toolexec.Request {
	rw := func(s string) string {
		if s == toolexec.WorkDir {
			return sb.work
		}
		return strings.ReplaceAll(s, toolexec.WorkDir+"/", sb.work+"/")
	}
	out := req
	out.Argv = make([]string, len(req.Argv))
	for i, a := range req.Argv {
		out.Argv[i] = rw(a)
	}
	out.Files = make([]toolwire.File, len(req.Files))
	for i, f := range req.Files {
		out.Files[i] = toolwire.File{Name: f.Name, Content: []byte(rw(string(f.Content)))}
	}
	out.Env = make([]toolexec.EnvVar, len(req.Env))
	for i, e := range req.Env {
		v := rw(e.Value)
		if e.Name == "PATH" {
			v = sb.goBin + ":" + v
		}
		out.Env[i] = toolexec.EnvVar{Name: e.Name, Value: v}
	}
	return out
}

// requests are the requests the sandbox was sent, as sent.
func (sb *wsSandbox) requests() []toolexec.Request {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return append([]toolexec.Request(nil), sb.reqs...)
}

func writeShimResult(w http.ResponseWriter, status int, res toolexec.Result) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(res)
}

// logWorkspaceRuns logs each agents tool pod's ledger and its sandboxes'
// requests, for a failing test.
func (k *kubelet) logWorkspaceRuns(t *testing.T) {
	t.Helper()
	for _, r := range k.WorkspaceRuns() {
		t.Logf("agents tool pod %s exited %d, message %q, ledger:\n%s", r.Job.Name, r.ExitCode, r.Message, r.Stdout)
		for side, reqs := range r.Requests {
			for _, req := range reqs {
				t.Logf("  %s sandbox: %s %q (run %s)", side, req.Kind, req.Argv, req.Run)
			}
		}
	}
}
