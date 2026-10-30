// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
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
	"github.com/devthenet-labs/maul/internal/previewtools"
)

// The kubelet's part for the agent evaluator (SPEC S1-24): the slot tool
// pod preview-controller renders for an outcome verification, and the
// evaluator and check-reviewer Jobs intent-controller launches in the
// agents namespace.
//
//   - A tool Job (maul.run/preview-role tools) runs its tools container as
//     the REAL preview-tools, built with the e2edial tag: its tool
//     ConfigMap is mirrored into a directory of its own on every poll, as
//     the kubelet's ConfigMap volume is (the capture preview-controller
//     refreshes reaches `maul-tool logs`), each component Service address
//     the config names is dialled at the fake component the test serves
//     under that key (MAUL_E2E_DIAL_MAP), and it listens on a loopback
//     port (MAUL_E2E_LISTEN). Its pod gets an address of its own, and is
//     Ready once the relay's readiness path answers, as the probe the Job
//     renders would make it; once the relay exits, its stdout is the tools
//     container's log and its termination message the file it wrote. The
//     browser and sandbox sidecars do not run. In a pod with a browser the
//     relay runs its e2e-only scripted browser (MAUL_E2E_BROWSER_DIR,
//     internal/previewtools/browser_e2e.go) behind the real Playwright
//     seams: it loads each page through the relay's preview front, reads
//     the component's answer as a page model, and measures compiled
//     checks against it. An exec call is the relay's "unavailable" answer.
//   - An evaluator Job reaches its tool pod by the host entry its pod
//     template pins (maul-tools at the tool pod's address): the kubelet
//     points its MAUL_TOOLS_URL at the relay's loopback listener for that
//     address, and puts maul-tool (built with e2edial, its scratch
//     directory the run's own: MAUL_E2E_SCRATCH_DIR) on its PATH.

// labelPreviewRole is the preview role label (preview-controller's
// labelRole); a tool Job's is v1alpha1.PreviewRoleTools.
const labelPreviewRole = "maul.run/preview-role"

// toolSide is the kubelet's part for slot tool Jobs (runTools).
type toolSide struct {
	bin        string
	namespaces []string
	// byIP is each running tool pod's relay listener, by the pod address
	// the kubelet gave it.
	byIP   map[string]string
	nextIP int
	runs   []toolRun
	// readyHold, while set (holdToolsReady), keeps every tool pod's tools
	// container un-Ready, as a relay still replaying would be.
	readyHold chan struct{}
}

// toolRun is one tool Job as the kubelet ran it.
type toolRun struct {
	Job batchv1.Job
	// IP is the address its pod was given; Config the relay config it ran.
	IP     string
	Config previewtools.Config
	// Stdout is the tools container's log (the ledger), Message its
	// termination message, ExitCode the relay's exit status.
	Stdout   []byte
	Message  string
	ExitCode int
}

// runTools turns the tool side on for Jobs in the slot namespaces given,
// from now on, until the test ends. It needs the tester side (runTesters)
// for the fake components.
func (k *kubelet) runTools(t *testing.T, namespaces ...string) {
	t.Helper()
	bin := buildTagged(t, "preview-tools", "e2edial")
	k.mu.Lock()
	k.tools = &toolSide{bin: bin, namespaces: namespaces, byIP: map[string]string{}}
	k.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	done := make(chan struct{})
	go k.toolLoop(ctx, &wg, done)
	t.Cleanup(func() {
		cancel()
		<-done
		wg.Wait()
	})
}

// ToolRuns returns every tool Job the kubelet finished so far, in order.
func (k *kubelet) ToolRuns() []toolRun {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.tools == nil {
		return nil
	}
	return slices.Clone(k.tools.runs)
}

// holdToolsReady keeps every tool pod un-Ready until release is called (or
// the test ends): its relay runs, but its pod's readiness never turns.
func (k *kubelet) holdToolsReady(t *testing.T) (release func()) {
	t.Helper()
	ch := make(chan struct{})
	k.mu.Lock()
	k.tools.readyHold = ch
	k.mu.Unlock()
	var once sync.Once
	release = func() {
		once.Do(func() {
			k.mu.Lock()
			k.tools.readyHold = nil
			k.mu.Unlock()
			close(ch)
		})
	}
	t.Cleanup(release)
	return release
}

// readyHeld reports a hold on tool pod readiness.
func (k *kubelet) readyHeld() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.tools.readyHold != nil
}

// toolAddr is the relay listener of the tool pod at ip, "" for none.
func (k *kubelet) toolAddr(ip string) string {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.tools == nil {
		return ""
	}
	return k.tools.byIP[ip]
}

// isToolJob reports a slot tool Job, which the tester side leaves alone.
func isToolJob(job *batchv1.Job) bool {
	return job.Spec.Template.Labels[labelPreviewRole] == v1alpha1.PreviewRoleTools
}

// toolLoop starts each new tool Job, each in its own goroutine: a tool pod
// runs until its evaluator finishes it, while the agent loop runs that
// evaluator.
func (k *kubelet) toolLoop(ctx context.Context, wg *sync.WaitGroup, done chan<- struct{}) {
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
		namespaces := slices.Clone(k.tools.namespaces)
		k.mu.Unlock()
		for _, ns := range namespaces {
			var list batchv1.JobList
			if err := k.cl.client.List(ctx, &list, client.InNamespace(ns),
				client.MatchingLabels{labelPreviewRole: v1alpha1.PreviewRoleTools}); err != nil {
				continue
			}
			for i := range list.Items {
				job := list.Items[i].DeepCopy()
				key := ns + "/" + job.Name
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
					if err := k.runToolPod(ctx, job); err != nil && ctx.Err() == nil {
						k.t.Errorf("kubelet: run tool job %s: %v", key, err)
					}
				}()
			}
		}
	}
}

// toolConfigMapOf names the tool ConfigMap the Job's tools container
// mounts.
func toolConfigMapOf(job *batchv1.Job) string {
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.ConfigMap != nil {
			return v.ConfigMap.Name
		}
	}
	return ""
}

// runToolPod does what the tool Job's pod would: the relay runs against
// the mirrored tool ConfigMap until it exits, then the pod and the Job end.
// A Job deleted while it runs (its verification settled or dropped) stops
// the relay, as the pod's deletion would.
func (k *kubelet) runToolPod(ctx context.Context, job *batchv1.Job) error {
	tp, err := k.mountToolConfig(ctx, job)
	if err != nil || tp == nil {
		return err
	}
	if err := k.startRelay(tp); err != nil {
		return err
	}
	if err := k.startToolPod(ctx, tp); err != nil {
		tp.stop()
		return err
	}
	if exited, err := k.followToolPod(ctx, tp); !exited || err != nil {
		tp.stop()
		return err
	}
	return k.endToolPod(ctx, tp)
}

// toolPod is one tool Job's pod while the kubelet runs it.
type toolPod struct {
	job     *batchv1.Job
	pod     *corev1.Pod
	cm      corev1.ConfigMap
	cfg     previewtools.Config
	started metav1.Time
	// dir is the mirrored ConfigMap volume, term the termination message
	// file, listen the relay's loopback address, ip the pod's address.
	dir, term, listen, ip string
	cmd                   *exec.Cmd
	exited                chan struct{}
	stdout, stderr        syncBuffer
}

// stop kills the relay and waits for it.
func (tp *toolPod) stop() {
	_ = tp.cmd.Process.Kill()
	<-tp.exited
}

// mountToolConfig waits for the tool ConfigMap, as the kubelet starts a pod
// only once its ConfigMap volume can be mounted, and mirrors it into a
// directory of the pod's own. A Job deleted first never gets a pod: nil.
func (k *kubelet) mountToolConfig(ctx context.Context, job *batchv1.Job) (*toolPod, error) {
	tp := &toolPod{job: job, started: metav1.Now()}
	name := toolConfigMapOf(job)
	if name == "" {
		return nil, errors.New("the tool pod mounts no ConfigMap")
	}
	for {
		err := k.cl.client.Get(ctx, types.NamespacedName{Namespace: job.Namespace, Name: name}, &tp.cm)
		if err == nil {
			break
		}
		if !apierrors.IsNotFound(err) || ctx.Err() != nil || k.jobGone(ctx, job) {
			return nil, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	var err error
	if tp.dir, err = os.MkdirTemp(k.work, job.Name+"-config-"); err != nil {
		return nil, err
	}
	tp.term = tp.dir + ".termination"
	if err := mirrorConfigMap(tp.dir, &tp.cm); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(tp.cm.Data[previewtools.ConfigFile]), &tp.cfg); err != nil {
		return nil, fmt.Errorf("decode the tool config: %w", err)
	}
	return tp, nil
}

// startRelay starts the e2edial preview-tools on the mirrored config: each
// component address the config names dialled at the fake component served
// under its key, a loopback listener of its own, its termination message a
// file of its own.
func (k *kubelet) startRelay(tp *toolPod) error {
	port, err := ports.next()
	if err != nil {
		return err
	}
	tp.listen = net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	dial := map[string]string{}
	k.mu.Lock()
	bin := k.tools.bin
	k.tools.nextIP++
	tp.ip = fmt.Sprintf("10.244.7.%d", k.tools.nextIP)
	for _, c := range tp.cfg.Components {
		if k.testers != nil {
			if to, ok := k.testers.components[c.Key]; ok {
				dial[c.Addr] = to
			}
		}
	}
	k.mu.Unlock()
	dialMap, err := json.Marshal(dial)
	if err != nil {
		return err
	}
	tp.cmd = exec.Command(bin)
	// previewtools' e2edial seams (dial_e2e.go), named literally: an
	// untagged import has none of them.
	tp.cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + tp.dir,
		"MAUL_E2E_DIAL_MAP=" + string(dialMap), "MAUL_E2E_LISTEN=" + tp.listen,
		"MAUL_E2E_TOOLS_CONFIG_DIR=" + tp.dir, "MAUL_E2E_TERMINATION_PATH=" + tp.term}
	if tp.cfg.Browser {
		// The browser container's part: the relay's scripted browser
		// (browser_e2e.go), working beside the mirrored config.
		tp.cmd.Env = append(tp.cmd.Env, "MAUL_E2E_BROWSER_DIR="+tp.dir+".browser")
	}
	tp.cmd.Stdout, tp.cmd.Stderr = &tp.stdout, &tp.stderr
	if err := tp.cmd.Start(); err != nil {
		return fmt.Errorf("start preview-tools: %w", err)
	}
	tp.exited = make(chan struct{})
	go func() {
		_ = tp.cmd.Wait()
		close(tp.exited)
	}()
	return nil
}

// startToolPod records the pod running at its address, not yet Ready, and
// makes the relay reachable there.
func (k *kubelet) startToolPod(ctx context.Context, tp *toolPod) error {
	var err error
	if tp.pod, err = k.createPod(ctx, tp.job); err != nil {
		return err
	}
	if err := k.toolPodRunning(ctx, tp, false); err != nil {
		return err
	}
	k.mu.Lock()
	k.tools.byIP[tp.ip] = tp.listen
	k.mu.Unlock()
	return nil
}

// followToolPod runs the pod until the relay exits (true): Ready once the
// relay's readiness path answers (and no hold keeps it back), its ConfigMap
// volume refreshed on every poll. The test's end or the Job's deletion
// ends it first (false).
func (k *kubelet) followToolPod(ctx context.Context, tp *toolPod) (bool, error) {
	ready := false
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-tp.exited:
			return true, nil
		case <-ctx.Done():
			return false, nil
		case <-tick.C:
		}
		if k.jobGone(ctx, tp.job) {
			return false, nil
		}
		if !ready && !k.readyHeld() && relayReady(tp.listen) {
			ready = true
			if err := k.toolPodRunning(ctx, tp, true); err != nil {
				return false, err
			}
		}
		if err := k.cl.client.Get(ctx, client.ObjectKeyFromObject(&tp.cm), &tp.cm); err == nil {
			_ = mirrorConfigMap(tp.dir, &tp.cm)
		}
	}
}

// endToolPod records the exited relay's run: its stdout as the tools
// container's log, the pod terminated with its exit status and termination
// message, the Job's end. A Job deleted meanwhile is no error.
func (k *kubelet) endToolPod(ctx context.Context, tp *toolPod) error {
	code := tp.cmd.ProcessState.ExitCode()
	msg, _ := os.ReadFile(tp.term)
	rec := toolRun{Job: *tp.job.DeepCopy(), IP: tp.ip, Config: tp.cfg, Stdout: tp.stdout.Bytes(),
		Message: string(msg), ExitCode: code}
	k.mu.Lock()
	delete(k.tools.byIP, tp.ip)
	k.logs[tp.pod.Namespace+"/"+tp.pod.Name+"/tools"] = rec.Stdout
	k.mu.Unlock()
	if code != 0 && len(rec.Stdout) == 0 {
		k.t.Logf("kubelet: preview-tools of %s exited %d: %s", tp.job.Name, code, tp.stderr.Bytes())
	}
	err := k.toolPodEnded(ctx, tp, rec)
	if err == nil {
		err = k.finishJob(ctx, tp.job, tp.started, code == 0)
	}
	if err != nil && !k.jobGone(ctx, tp.job) {
		return err
	}
	k.mu.Lock()
	k.tools.runs = append(k.tools.runs, rec)
	k.mu.Unlock()
	return nil
}

// jobGone reports job deleted, or being deleted.
func (k *kubelet) jobGone(ctx context.Context, job *batchv1.Job) bool {
	var cur batchv1.Job
	err := k.cl.client.Get(ctx, client.ObjectKeyFromObject(job), &cur)
	return apierrors.IsNotFound(err) || (err == nil && (!cur.DeletionTimestamp.IsZero() || cur.UID != job.UID))
}

// relayReady reports the relay's readiness path answering 200.
func relayReady(addr string) bool {
	c := http.Client{Timeout: time.Second}
	res, err := c.Get((&url.URL{Scheme: "http", Host: addr, Path: previewtools.ReadyPath}).String())
	if err != nil {
		return false
	}
	_ = res.Body.Close()
	return res.StatusCode == http.StatusOK
}

// mirrorConfigMap writes cm's data into dir, one file per key, each
// replaced whole (renamed into place) as the kubelet's atomic writer swaps
// a ConfigMap volume's contents; a key gone from cm is removed.
func mirrorConfigMap(dir string, cm *corev1.ConfigMap) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if _, ok := cm.Data[e.Name()]; !ok {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
	for key, value := range cm.Data {
		path := filepath.Join(dir, key)
		if old, err := os.ReadFile(path); err == nil && string(old) == value {
			continue
		}
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, []byte(value), 0o444); err != nil {
			return err
		}
		if err := os.Rename(tmp, path); err != nil {
			return err
		}
	}
	return nil
}

// toolPodRunning records the tool pod running at ip, its tools container
// Ready or not yet.
func (k *kubelet) toolPodRunning(ctx context.Context, tp *toolPod, ready bool) error {
	cond := corev1.ConditionFalse
	if ready {
		cond = corev1.ConditionTrue
	}
	pod, image := tp.pod, tp.pod.Spec.Containers[0].Image
	pod.Status = corev1.PodStatus{Phase: corev1.PodRunning, StartTime: &tp.started, PodIP: tp.ip,
		PodIPs:     []corev1.PodIP{{IP: tp.ip}},
		Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: cond}},
		ContainerStatuses: []corev1.ContainerStatus{{Name: "tools", Image: image, ImageID: image, Ready: ready,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: tp.started}}}},
		InitContainerStatuses: sidecarsRunning(pod, tp.started)}
	if err := k.cl.client.Status().Update(ctx, pod); err != nil {
		return fmt.Errorf("record tool pod status: %w", err)
	}
	return nil
}

// sidecarsRunning reports each of pod's native sidecars (browser, sandbox)
// started, running and Ready, as a kubelet would once they came up: they
// do not run here, but preview-controller publishes a tool pod only once
// every sidecar its round asks for is.
func sidecarsRunning(pod *corev1.Pod, started metav1.Time) []corev1.ContainerStatus {
	var out []corev1.ContainerStatus
	for _, c := range pod.Spec.InitContainers {
		if c.RestartPolicy == nil || *c.RestartPolicy != corev1.ContainerRestartPolicyAlways {
			continue
		}
		up := true
		out = append(out, corev1.ContainerStatus{Name: c.Name, Image: c.Image, ImageID: c.Image, Ready: true,
			Started: &up, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: started}}})
	}
	return out
}

// toolPodEnded records the tool pod's end: its tools container terminated
// with the relay's exit status and termination message.
func (k *kubelet) toolPodEnded(ctx context.Context, tp *toolPod, rec toolRun) error {
	pod := tp.pod
	if err := k.cl.client.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil {
		return err
	}
	reason, phase := "Completed", corev1.PodSucceeded
	if rec.ExitCode != 0 {
		reason, phase = "Error", corev1.PodFailed
	}
	image := pod.Spec.Containers[0].Image
	pod.Status = corev1.PodStatus{Phase: phase, StartTime: &tp.started, PodIP: tp.ip,
		PodIPs:     []corev1.PodIP{{IP: tp.ip}},
		Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
		ContainerStatuses: []corev1.ContainerStatus{{Name: "tools", Image: image, ImageID: image,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: int32(rec.ExitCode), Reason: reason, Message: rec.Message, StartedAt: tp.started,
				FinishedAt: metav1.Now()}}}}}
	if err := k.cl.client.Status().Update(ctx, pod); err != nil {
		return fmt.Errorf("record tool pod status: %w", err)
	}
	return nil
}

// syncBuffer is a bytes.Buffer safe to write from a process's output
// goroutine and read from another.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.buf.Bytes())
}

// ---- the evaluator and the check reviewer ---------------------------------

// isRoleJob reports an evaluator's or check reviewer's Job, which reads no
// tree.
func isRoleJob(job *batchv1.Job) bool {
	return job.Spec.Template.Labels[v1alpha1.LabelRole] != ""
}

// prepareRole does a role Job's prepare init's work in workspace: the
// handoff staged in input/, the scratch directory made. Nothing is fetched.
func prepareRole(workspace string, secret *corev1.Secret) error {
	if err := writeHandoff(workspace, secret); err != nil {
		return err
	}
	return os.MkdirAll(filepath.Join(workspace, "scratch"), 0o755)
}

// roleEnv is what the kubelet gives a role Job's agent beside its own
// environment: an evaluator's MAUL_TOOLS_URL pointed at its tool pod's
// relay (the address its hosts entry pins), and maul-tool's scratch
// directory the run's own.
func (k *kubelet) roleEnv(job *batchv1.Job, workspace string) ([]string, error) {
	if job.Spec.Template.Labels[v1alpha1.LabelRole] != v1alpha1.RoleEvaluator {
		return nil, nil
	}
	env := []string{"MAUL_E2E_SCRATCH_DIR=" + filepath.Join(workspace, "scratch")}
	for _, a := range job.Spec.Template.Spec.HostAliases {
		if !slices.Contains(a.Hostnames, "maul-tools") {
			continue
		}
		addr := k.toolAddr(a.IP)
		if addr == "" {
			// The tool pod is gone (its relay ended on its own clock):
			// nothing answers at its address, as on a cluster.
			addr = "127.0.0.1:1"
		}
		return append(env, "MAUL_TOOLS_URL=http://"+addr), nil
	}
	return nil, errors.New("the evaluator's pod pins no maul-tools host")
}
