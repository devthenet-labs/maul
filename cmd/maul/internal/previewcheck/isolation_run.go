// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewcheck

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/devthenet-labs/maul/cmd/maul/internal/checkreport"
)

// cleanupTimeout bounds deleting the probe Pods, which runs even after the
// run's own context ended (an interrupt, or the deadline).
const cleanupTimeout = time.Minute

// defaultPoll is how often a waited-on Pod is read.
const defaultPoll = 2 * time.Second

// isolation runs preview-netpol: plan, run, judge.
func (c *Checker) isolation(ctx context.Context, s Settings) {
	iso := c.cfg.Isolation
	plan, err := c.planIsolation(ctx, s, iso)
	if err != nil {
		c.add(CheckPreviewNetpol, checkreport.Fail, "the probe could not run: %v; no probe Pod was created", err)
		return
	}
	v := judgeIsolation(runIsolation(ctx, iso, plan))
	c.add(CheckPreviewNetpol, v.status, "%s", v.reason)
}

// runIsolation runs the probe in ingress.sh's --refused-is-blocked order and
// records what it saw: the listener in the slot; the gateway-labelled client
// alone, which must reach it; only then the agents' client and the
// unlabelled release client, which count a refusal as blocked; then the
// listener again, which must still be the same running Pod. Every Pod it
// created is deleted before it returns, whatever happened, with a context
// of its own so an interrupt still cleans up.
func runIsolation(ctx context.Context, iso *Isolation, plan probePlan) (r isolationRun) {
	timeout, poll := iso.Timeout, iso.Poll
	if timeout <= 0 {
		timeout = DefaultIsolationTimeout
	}
	if poll <= 0 {
		poll = defaultPoll
	}
	r.ipBlocks = plan.ipBlocks
	r.gatewayWhere = plan.releaseNS + "/" + probeGatewayClient
	r.agentWhere = plan.agentsNS + "/" + probeAgentClient
	r.releaseWhere = plan.releaseNS + "/" + probeReleaseClient
	run, err := newRunID()
	if err != nil {
		r.setup = err.Error()
		return r
	}
	var created []*corev1.Pod
	defer func() { r.cleanup = deleteProbes(context.WithoutCancel(ctx), iso.Client, created, run) }()
	create := func(p *corev1.Pod) error {
		if p.Annotations == nil {
			p.Annotations = map[string]string{}
		}
		p.Annotations[probeRunAnnotation] = run
		err := iso.Client.Create(ctx, p)
		// A Pod that already exists is another's (the plan saw none); any
		// other failure may still have created it, so cleanup looks for it,
		// and deletes it only when it carries this run's marker.
		if !apierrors.IsAlreadyExists(err) {
			created = append(created, p)
		}
		return err
	}

	listener := plan.listener()
	if err := create(listener); err != nil {
		r.setup = fmt.Sprintf("the listener %s/%s was not created: %v", plan.slotNS, probeListener, err)
		return r
	}
	ready, err := waitPod(ctx, iso.Client, client.ObjectKeyFromObject(listener), timeout, poll, listenerReady)
	if err != nil {
		r.setup = fmt.Sprintf("the listener %s/%s %v", plan.slotNS, probeListener, err)
		return r
	}
	ip := ready.Status.PodIP
	r.listener = net.JoinHostPort(ip, strconv.Itoa(probePort))

	r.gateway = runClient(ctx, iso, create, plan.client(probeGatewayClient, ip), timeout, poll)
	if r.gateway.outcome != outcomeReached {
		return r
	}
	agent, release := plan.client(probeAgentClient, ip), plan.client(probeReleaseClient, ip)
	r.agent.refusedBlocks, r.release.refusedBlocks = true, true
	agentErr, releaseErr := create(agent), create(release)
	r.agent = finishClient(ctx, iso, agent, agentErr, timeout, poll)
	r.release = finishClient(ctx, iso, release, releaseErr, timeout, poll)

	var after corev1.Pod
	if err := iso.Client.Get(ctx, client.ObjectKeyFromObject(listener), &after); err != nil {
		r.listenerState = "unreadable: " + err.Error()
		return r
	}
	r.listenerChecked = true
	r.listenerState, r.listenerSurvived = listenerAfter(&after, ready.UID)
	return r
}

// runClient creates one client Pod and waits for its summary.
func runClient(ctx context.Context, iso *Isolation, create func(*corev1.Pod) error, pod *corev1.Pod,
	timeout, poll time.Duration) clientResult {
	return finishClient(ctx, iso, pod, create(pod), timeout, poll)
}

// finishClient waits for a created client Pod to finish and reads its
// summary; createErr is its Create's outcome.
func finishClient(ctx context.Context, iso *Isolation, pod *corev1.Pod, createErr error,
	timeout, poll time.Duration) clientResult {
	res := clientResult{refusedBlocks: slices.Contains(pod.Spec.Containers[0].Args, refusedIsBlocked)}
	if createErr != nil {
		res.detail = "not created: " + createErr.Error()
		return res
	}
	res.ran = true
	done, err := waitPod(ctx, iso.Client, client.ObjectKeyFromObject(pod), timeout, poll, podFinished)
	if err != nil {
		res.detail = err.Error()
		return res
	}
	log, err := iso.Logs(ctx, pod.Namespace, pod.Name)
	if err != nil {
		res.detail = fmt.Sprintf("%s, and its log is unreadable: %v", done.Status.Phase, err)
		return res
	}
	res.outcome, res.detail = summaryOf(log)
	if done.Status.Phase != corev1.PodSucceeded && res.detail == "" {
		res.detail = "the Pod " + string(done.Status.Phase)
	}
	return res
}

// summaryOf is a netprobe client's last summary outcome from its log, or
// why there is none.
func summaryOf(log []byte) (outcome, detail string) {
	sc := bufio.NewScanner(bytes.NewReader(log))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	found, first := "", ""
	for sc.Scan() {
		if first == "" {
			first = string(bytes.TrimSpace(sc.Bytes()))
		}
		var line struct {
			Name    string `json:"name"`
			Outcome string `json:"outcome"`
		}
		if json.Unmarshal(sc.Bytes(), &line) == nil && line.Name == "summary" {
			found = line.Outcome
		}
	}
	switch found {
	case outcomeReached, outcomeBlocked, outcomeInconclusive:
		return found, ""
	case "":
		if len(first) > 200 {
			first = first[:200] + "..."
		}
		return "", fmt.Sprintf("no summary in its log (its first line: %q): the probe image may predate this "+
			"CLI's netprobe; rebuild it from hack/preview-isolation-probe at this CLI's commit", first)
	}
	return "", fmt.Sprintf("an unknown summary %q", found)
}

// listenerReady is done once the listener is Ready with an IPv4 address,
// and an error once it can never be.
func listenerReady(p *corev1.Pod) (bool, error) {
	switch p.Status.Phase {
	case corev1.PodSucceeded, corev1.PodFailed:
		return false, fmt.Errorf("ended %s before it was Ready", p.Status.Phase)
	case corev1.PodRunning:
	default:
		return false, nil
	}
	ready := slices.ContainsFunc(p.Status.Conditions, func(c corev1.PodCondition) bool {
		return c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue
	})
	if !ready {
		return false, nil
	}
	if ip := net.ParseIP(p.Status.PodIP); ip == nil || ip.To4() == nil {
		return false, fmt.Errorf("has no IPv4 address (%q)", p.Status.PodIP)
	}
	return true, nil
}

// podFinished is done once a client Pod has ended either way.
func podFinished(p *corev1.Pod) (bool, error) {
	return p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed, nil
}

// listenerAfter reports whether the listener survived the refused clients:
// still Running, the same Pod (uid) and its container never restarted
// (restartPolicy Never, so a listener that exited is no longer Running).
func listenerAfter(p *corev1.Pod, uid types.UID) (state string, survived bool) {
	if p.UID != uid {
		return "a different Pod of the same name", false
	}
	if p.Status.Phase != corev1.PodRunning {
		return string(p.Status.Phase), false
	}
	for _, cs := range p.Status.ContainerStatuses {
		if cs.RestartCount > 0 || cs.State.Running == nil {
			return "Running but its container restarted or stopped", false
		}
	}
	return "Running", true
}

// waitPod reads key every poll until done says so, done fails, or timeout
// (or ctx) ends; the error then says what the Pod was last doing.
func waitPod(ctx context.Context, r client.Reader, key types.NamespacedName, timeout, poll time.Duration,
	done func(*corev1.Pod) (bool, error)) (*corev1.Pod, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	last := "not yet read"
	for {
		var p corev1.Pod
		err := r.Get(ctx, key, &p)
		switch {
		case err == nil:
			ok, derr := done(&p)
			if derr != nil {
				return nil, derr
			}
			if ok {
				return &p, nil
			}
			last = podState(&p)
		case ctx.Err() == nil:
			last = "unreadable: " + err.Error()
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, fmt.Errorf("did not get there within %s (%s)", timeout, last)
			}
			return nil, fmt.Errorf("was abandoned: %w (%s)", ctx.Err(), last)
		case <-time.After(poll):
		}
	}
}

// podState is a Pod's phase and, while it waits, why (ErrImagePull, an
// unschedulable Pod's condition).
func podState(p *corev1.Pod) string {
	state := string(p.Status.Phase)
	if state == "" {
		state = "no phase yet"
	}
	for _, cs := range p.Status.ContainerStatuses {
		if w := cs.State.Waiting; w != nil && w.Reason != "" {
			return state + ", " + w.Reason
		}
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Reason != "" {
			return state + ", " + c.Reason
		}
	}
	return state
}

// probeRunAnnotation marks every Pod one probe run creates with that run's
// random ID, so cleanup never deletes a Pod of the same name it did not
// make. The slot admission policies leave Pod metadata alone.
const probeRunAnnotation = "maul.run/isolation-probe-run"

// newRunID is a random probe run ID.
func newRunID() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("draw a probe run ID: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// deleteProbes deletes every Pod the run created, each held to its UID by
// a delete precondition, and lists those it could not delete. A Pod whose
// Create failed has no UID from the API server: it is read first, and
// deleted (held to the UID read) only when it carries this run's marker.
func deleteProbes(ctx context.Context, c client.Client, pods []*corev1.Pod, run string) []string {
	ctx, cancel := context.WithTimeout(ctx, cleanupTimeout)
	defer cancel()
	var failed []string
	for _, p := range pods {
		uid := p.UID
		if uid == "" {
			var got corev1.Pod
			switch err := c.Get(ctx, client.ObjectKeyFromObject(p), &got); {
			case apierrors.IsNotFound(err):
				continue
			case err != nil:
				failed = append(failed, fmt.Sprintf("Pod %s/%s, if this run made it (%v)", p.Namespace, p.Name, err))
				continue
			case run == "" || got.Annotations[probeRunAnnotation] != run:
				continue // not this run's Pod
			}
			uid = got.UID
		}
		opts := []client.DeleteOption{client.GracePeriodSeconds(0),
			client.PropagationPolicy(metav1.DeletePropagationBackground)}
		if uid != "" {
			opts = append(opts, client.Preconditions{UID: &uid})
		}
		victim := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: p.Namespace, Name: p.Name}}
		if err := c.Delete(ctx, victim, opts...); err != nil && !apierrors.IsNotFound(err) {
			failed = append(failed, fmt.Sprintf("Pod %s/%s (%v)", p.Namespace, p.Name, err))
		}
	}
	return failed
}
