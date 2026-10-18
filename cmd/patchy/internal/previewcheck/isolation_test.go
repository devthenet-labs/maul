// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewcheck

import (
	"context"
	"errors"
	"math/rand"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/quick"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"

	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/checkreport"
	"github.com/bitwise-media-group/patchy/internal/kube"
)

const (
	probeCommit    = "0123456789abcdef0123456789abcdef01234567"
	probePrefix    = "registry.acme.test/patchy/previews/"
	testProbeImage = probePrefix + "netprobe:sha-" + probeCommit
	ingressSh      = "../../../../hack/preview-isolation-probe/ingress.sh"
)

// testPlan is a probe in slot 1 with node isolation.
func testPlan() probePlan {
	return probePlan{image: testProbeImage, slotNS: "patchy-preview-1", releaseNS: "patchy", agentsNS: "patchy-agents",
		selector: map[string]string{iNodeLabel: "true", "karpenter.sh/nodepool": "patchy-preview"}, taintKey: iTaint}
}

// TestProbePodsMatchIngressSh: the Pods the Go probe creates are
// ingress.sh's own under --refused-is-blocked (whose --print-manifests the
// chart's preview policy envtest admits against the rendered slot
// policies), with and without node isolation.
func TestProbePodsMatchIngressSh(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	isolated := testPlan()
	none := testPlan()
	none.selector, none.taintKey = nil, ""
	for name, tc := range map[string]struct {
		plan  probePlan
		flags []string
	}{
		"isolated": {isolated, []string{"--node-selector",
			"karpenter.sh/nodepool=patchy-preview," + iNodeLabel + "=true", "--taint-key", iTaint}},
		"no isolation": {none, []string{"--no-node-isolation"}},
	} {
		t.Run(name, func(t *testing.T) {
			args := append([]string{ingressSh, "--print-manifests", "--refused-is-blocked", "--image", testProbeImage,
				"--slot", "1", "--release-namespace", "patchy", "--agents-namespace", "patchy-agents"}, tc.flags...)
			out, err := exec.CommandContext(t.Context(), "bash", args...).Output()
			if err != nil {
				t.Fatalf("ingress.sh --print-manifests: %v", err)
			}
			docs := strings.Split(string(out), "---\n")
			want := []*corev1.Pod{tc.plan.listener(), tc.plan.client(probeAgentClient, "192.0.2.1"),
				tc.plan.client(probeReleaseClient, "192.0.2.1"), tc.plan.client(probeGatewayClient, "192.0.2.1")}
			if len(docs) != len(want) {
				t.Fatalf("ingress.sh printed %d Pods, want %d", len(docs), len(want))
			}
			for i, doc := range docs {
				var got corev1.Pod
				if err := yaml.UnmarshalStrict([]byte(doc), &got); err != nil {
					t.Fatalf("Pod %d: %v\n%s", i, err, doc)
				}
				if !equality.Semantic.DeepEqual(&got, want[i]) {
					g, _ := yaml.Marshal(&got)
					w, _ := yaml.Marshal(want[i])
					t.Errorf("Pod %s differs from ingress.sh's:\ningress.sh:\n%s\ngo:\n%s", got.Name, g, w)
				}
			}
		})
	}
}

// TestJudgeIsolationNeverPassesUnproven (seeded): whatever the run
// observed, preview-netpol is a PASS or WARN only when the gateway's client
// REACHED the listener, both other clients ran and were BLOCKED, and the
// listener was read afterwards and had survived. A refusal therefore never
// counts as blocked unless the listener is proven to have answered before
// and to be alive after.
func TestJudgeIsolationNeverPassesUnproven(t *testing.T) {
	outcomes := []string{"", outcomeReached, outcomeBlocked, outcomeInconclusive}
	randClient := func(r *rand.Rand) clientResult {
		return clientResult{ran: r.Intn(4) > 0, refusedBlocks: r.Intn(2) == 0,
			outcome: outcomes[r.Intn(len(outcomes))], detail: []string{"", "timed out"}[r.Intn(2)]}
	}
	cfg := quickConfig(20000)
	cfg.Values = func(args []reflect.Value, r *rand.Rand) {
		run := isolationRun{listener: "10.42.0.9:8080", gatewayWhere: "patchy/gw", agentWhere: "agents/a",
			releaseWhere: "patchy/r", gateway: randClient(r), agent: randClient(r), release: randClient(r),
			listenerChecked: r.Intn(4) > 0, listenerSurvived: r.Intn(3) > 0, listenerState: "Running"}
		if r.Intn(8) == 0 {
			run.setup = "the listener never became Ready"
		}
		if r.Intn(4) == 0 {
			run.ipBlocks = []string{"10.40.128.0/24"}
		}
		if r.Intn(6) == 0 {
			run.cleanup = []string{"Pod patchy/x (gone)"}
		}
		args[0] = reflect.ValueOf(run)
	}
	prop := func(run isolationRun) bool {
		v := judgeIsolation(run)
		proven := run.setup == "" && run.gateway.ran && run.gateway.outcome == outcomeReached &&
			run.agent.ran && run.agent.outcome == outcomeBlocked &&
			run.release.ran && run.release.outcome == outcomeBlocked &&
			run.listenerChecked && run.listenerSurvived
		if passed := v.status != checkreport.Fail; passed != proven {
			t.Logf("%+v: %s %s, proven %v", run, v.status, v.reason, proven)
			return false
		}
		if v.status == checkreport.Pass && (len(run.ipBlocks) > 0 || len(run.cleanup) > 0) {
			t.Logf("PASS despite ipBlocks or leftover Pods: %+v", run)
			return false
		}
		return v.reason != ""
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}

// scenario is how a fake cluster answers one probe run.
type scenario struct {
	// listener: "ready", "ipv6" (Ready without IPv4), "pending" (never
	// starts), "failed" (ends before Ready).
	listener string
	// outcomes are each client's summary; "old" is an image that predates
	// --refused-is-blocked, "hang" a client that never finishes.
	gateway, agent, release string
	// dies: the listener is Failed once the refused clients exist;
	// replaced: it is another Pod of the same name by then.
	dies, replaced bool
	// failCreate names a Pod whose Create fails.
	failCreate string
}

// fakeProbeCluster is a fake API server that plays a scenario: Pods get
// their status as the scenario says when read, and every create is logged.
type fakeProbeCluster struct {
	mu      sync.Mutex
	sc      scenario
	client  client.WithWatch
	created []string
	secrets int
}

func newFakeProbeCluster(sc scenario, objs ...client.Object) *fakeProbeCluster {
	f := &fakeProbeCluster{sc: sc}
	f.client = fake.NewClientBuilder().WithScheme(kube.Scheme()).WithObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				f.mu.Lock()
				f.created = append(f.created, obj.GetName())
				f.mu.Unlock()
				if obj.GetName() == sc.failCreate {
					return apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, obj.GetName(),
						errors.New("admission refused"))
				}
				return c.Create(ctx, obj, opts...)
			},
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
				opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.Secret); ok {
					f.mu.Lock()
					f.secrets++
					f.mu.Unlock()
				}
				if err := c.Get(ctx, key, obj, opts...); err != nil {
					return err
				}
				if p, ok := obj.(*corev1.Pod); ok {
					f.status(p)
				}
				return nil
			},
		}).Build()
	return f
}

// refusedClientsExist reports whether the agent or release client was
// created.
func (f *fakeProbeCluster) refusedClientsExist() bool {
	return slices.Contains(f.created, probeAgentClient) || slices.Contains(f.created, probeReleaseClient)
}

// status gives a read Pod the scenario's status.
func (f *fakeProbeCluster) status(p *corev1.Pod) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ready := []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	running := []corev1.ContainerStatus{{Name: probeContainer,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
	if p.Name == probeListener {
		p.UID = "listener-1"
		switch f.sc.listener {
		case "pending":
			p.Status = corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{{
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ErrImagePull"}}}}}
		case "failed":
			p.Status = corev1.PodStatus{Phase: corev1.PodFailed}
		case "ipv6":
			p.Status = corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "fd00::9", Conditions: ready}
		default:
			p.Status = corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.42.0.9", Conditions: ready,
				ContainerStatuses: running}
		}
		if f.refusedClientsExist() {
			if f.sc.dies {
				p.Status = corev1.PodStatus{Phase: corev1.PodFailed}
			}
			if f.sc.replaced {
				p.UID = "listener-2"
			}
		}
		return
	}
	if f.outcome(p.Name) == "hang" {
		p.Status = corev1.PodStatus{Phase: corev1.PodRunning}
		return
	}
	p.Status = corev1.PodStatus{Phase: corev1.PodSucceeded}
}

func (f *fakeProbeCluster) outcome(name string) string {
	switch name {
	case probeGatewayClient:
		return f.sc.gateway
	case probeAgentClient:
		return f.sc.agent
	}
	return f.sc.release
}

// logs is each client's log as netprobe writes it.
func (f *fakeProbeCluster) logs(_ context.Context, _, pod string) ([]byte, error) {
	switch out := f.outcome(pod); out {
	case "old":
		return []byte("usage: probe connect <ipv4>:<port>\n"), nil
	case "":
		return []byte(`{"name":"slot-pod","worker":1,"attempt":1,"outcome":"blocked"}` + "\n"), nil
	default:
		return []byte(`{"name":"slot-pod","worker":1,"attempt":1,"outcome":"x"}` + "\n" +
			`{"name":"summary","outcome":"` + out + `"}` + "\n"), nil
	}
}

// remaining are the Pods left in the fake cluster.
func (f *fakeProbeCluster) remaining(t *testing.T) []string {
	t.Helper()
	var list corev1.PodList
	if err := f.client.List(context.Background(), &list); err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(list.Items))
	for _, p := range list.Items {
		out = append(out, p.Namespace+"/"+p.Name)
	}
	return out
}

func (f *fakeProbeCluster) iso() *Isolation {
	return &Isolation{Client: f.client, Logs: f.logs, Timeout: 50 * time.Millisecond, Poll: time.Millisecond}
}

// TestRunIsolationScenarios: the run's verdict for named scenarios, each
// leaving no probe Pod behind.
func TestRunIsolationScenarios(t *testing.T) {
	pass := scenario{listener: "ready", gateway: outcomeReached, agent: outcomeBlocked, release: outcomeBlocked}
	tests := map[string]struct {
		change func(*scenario)
		status checkreport.Status
		want   []string
	}{
		"isolated": {status: checkreport.Pass, want: []string{"patchy/preview-ingress-gateway REACHED",
			"patchy-agents/preview-ingress-agent and patchy/preview-ingress-release were BLOCKED",
			"every probe Pod was deleted"}},
		"agent leaks": {change: func(s *scenario) { s.agent = outcomeReached }, status: checkreport.Fail,
			want: []string{"patchy-agents/preview-ingress-agent REACHED the listener"}},
		"release inconclusive": {change: func(s *scenario) { s.release = outcomeInconclusive },
			status: checkreport.Fail, want: []string{"preview-ingress-release was not blocked (INCONCLUSIVE)"}},
		"gateway blocked": {change: func(s *scenario) { s.gateway = outcomeBlocked }, status: checkreport.Fail,
			want: []string{"did not reach the listener", "other clients were not run"}},
		"listener dies": {change: func(s *scenario) { s.dies = true }, status: checkreport.Fail,
			want: []string{"listener was Failed after the refused clients ran"}},
		"listener replaced": {change: func(s *scenario) { s.replaced = true }, status: checkreport.Fail,
			want: []string{"a different Pod of the same name"}},
		"listener never starts": {change: func(s *scenario) { s.listener = "pending" }, status: checkreport.Fail,
			want: []string{"did not get there", "ErrImagePull"}},
		"listener IPv6 only": {change: func(s *scenario) { s.listener = "ipv6" }, status: checkreport.Fail,
			want: []string{"no IPv4 address"}},
		"old image": {change: func(s *scenario) { s.gateway, s.agent = outcomeReached, "old" },
			status: checkreport.Fail, want: []string{"no summary in its log", "usage: probe connect",
				"rebuild it"}},
		"agent hangs": {change: func(s *scenario) { s.agent = "hang" }, status: checkreport.Fail,
			want: []string{"preview-ingress-agent was not blocked (did not get there"}},
		"agent refused admission": {change: func(s *scenario) { s.failCreate = probeAgentClient },
			status: checkreport.Fail, want: []string{"preview-ingress-agent did not run (not created"}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			sc := pass
			if tc.change != nil {
				tc.change(&sc)
			}
			f := newFakeProbeCluster(sc)
			v := judgeIsolation(runIsolation(context.Background(), f.iso(), testPlan()))
			if v.status != tc.status {
				t.Errorf("status = %s, want %s: %s", v.status, tc.status, v.reason)
			}
			for _, w := range tc.want {
				if !strings.Contains(v.reason, w) {
					t.Errorf("reason %q lacks %q", v.reason, w)
				}
			}
			if left := f.remaining(t); len(left) > 0 {
				t.Errorf("Pods left behind: %v", left)
			}
		})
	}
}

// TestRunIsolationNeverPassesALeak (seeded): over random cluster
// behaviour, the run passes only when the gateway's client reached the
// listener, both others were blocked and the listener survived; the
// refused clients are created only after the gateway's client reached it;
// and no probe Pod is ever left behind.
func TestRunIsolationNeverPassesALeak(t *testing.T) {
	listeners := []string{"ready", "ready", "ready", "ipv6", "pending", "failed"}
	outs := []string{outcomeReached, outcomeBlocked, outcomeInconclusive, "", "old", "hang"}
	fails := []string{"", "", "", probeListener, probeGatewayClient, probeAgentClient, probeReleaseClient}
	cfg := quickConfig(300)
	cfg.Values = func(args []reflect.Value, r *rand.Rand) {
		// Each client gives the passing outcome half the time, so runs
		// that get as far as the listener's last read are common.
		pick := func(likely string) string {
			if r.Intn(2) == 0 {
				return likely
			}
			return outs[r.Intn(len(outs))]
		}
		failCreate := ""
		if r.Intn(3) == 0 {
			failCreate = fails[r.Intn(len(fails))]
		}
		args[0] = reflect.ValueOf(scenario{
			listener: listeners[r.Intn(len(listeners))], gateway: pick(outcomeReached),
			agent: pick(outcomeBlocked), release: pick(outcomeBlocked),
			dies: r.Intn(4) == 0, replaced: r.Intn(6) == 0, failCreate: failCreate,
		})
	}
	prop := func(sc scenario) bool {
		f := newFakeProbeCluster(sc)
		v := judgeIsolation(runIsolation(context.Background(), f.iso(), testPlan()))
		listenerOK := sc.listener == "ready" && sc.failCreate != probeListener
		gatewayOK := listenerOK && sc.gateway == outcomeReached && sc.failCreate != probeGatewayClient
		want := gatewayOK && sc.agent == outcomeBlocked && sc.release == outcomeBlocked && !sc.dies &&
			!sc.replaced && sc.failCreate != probeAgentClient && sc.failCreate != probeReleaseClient
		if got := v.status != checkreport.Fail; got != want {
			t.Logf("%+v: %s %s, want pass %v", sc, v.status, v.reason, want)
			return false
		}
		if f.refusedClientsExist() && !gatewayOK {
			t.Logf("%+v: refused clients created before the gateway reached the listener (%v)", sc, f.created)
			return false
		}
		if left := f.remaining(t); len(left) > 0 || f.secrets > 0 {
			t.Logf("%+v: left %v, read %d Secrets", sc, left, f.secrets)
			return false
		}
		return true
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}

// TestRunIsolationInterrupted: a run whose context ends mid-way (an
// interrupt) still deletes every Pod it created, and fails.
func TestRunIsolationInterrupted(t *testing.T) {
	f := newFakeProbeCluster(scenario{listener: "ready", gateway: outcomeReached, agent: "hang",
		release: outcomeBlocked})
	ctx, cancel := context.WithCancel(context.Background())
	iso := f.iso()
	iso.Timeout = time.Hour
	go func() {
		for {
			f.mu.Lock()
			n := len(f.created)
			f.mu.Unlock()
			if n >= 4 {
				cancel()
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	v := judgeIsolation(runIsolation(ctx, iso, testPlan()))
	if v.status != checkreport.Fail || !strings.Contains(v.reason, "abandoned") {
		t.Errorf("interrupted run = %s %s", v.status, v.reason)
	}
	if left := f.remaining(t); len(left) > 0 {
		t.Errorf("Pods left behind after an interrupt: %v", left)
	}
}

// TestRunIsolationSparesAForeignPod: when a Create fails for another
// reason than AlreadyExists, the run cannot know whether it made the Pod,
// so cleanup deletes the Pod of that name only if it carries this run's
// marker: one that appeared there in the meantime (another probe's) is
// left alone, while one this run did create despite the error is deleted.
func TestRunIsolationSparesAForeignPod(t *testing.T) {
	foreign := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "patchy-preview-1", Name: probeListener}}
	f := newFakeProbeCluster(scenario{listener: "ready", failCreate: probeListener}, foreign)
	v := judgeIsolation(runIsolation(context.Background(), f.iso(), testPlan()))
	if v.status != checkreport.Fail {
		t.Errorf("status = %s: %s", v.status, v.reason)
	}
	if left := f.remaining(t); !reflect.DeepEqual(left, []string{"patchy-preview-1/" + probeListener}) {
		t.Errorf("the foreign Pod was not spared: left %v", left)
	}

	// A Create that made the Pod but answered with an error (a timeout).
	var ambiguous client.WithWatch
	ambiguous = fake.NewClientBuilder().WithScheme(kube.Scheme()).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if err := c.Create(ctx, obj, opts...); err != nil {
				return err
			}
			return apierrors.NewTimeoutError("the request timed out", 1)
		},
	}).Build()
	iso := &Isolation{Client: ambiguous, Logs: f.logs, Timeout: 50 * time.Millisecond, Poll: time.Millisecond}
	_ = runIsolation(context.Background(), iso, testPlan())
	var list corev1.PodList
	if err := ambiguous.List(context.Background(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 0 {
		t.Errorf("a Pod the run created despite the error was left behind: %s/%s", list.Items[0].Namespace,
			list.Items[0].Name)
	}
}

// TestDeleteProbesReportsFailures: a Pod that cannot be deleted is named,
// so the line can say to delete it by hand.
func TestDeleteProbesReportsFailures(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(kube.Scheme()).WithInterceptorFuncs(interceptor.Funcs{
		Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
			return errors.New("connection reset")
		},
	}).Build()
	pod := testPlan().listener()
	pod.UID = "listener-1"
	got := deleteProbes(context.Background(), c, []*corev1.Pod{pod}, "run")
	if len(got) != 1 || !strings.Contains(got[0], "patchy-preview-1/preview-ingress-listener") {
		t.Errorf("deleteProbes = %v", got)
	}
	v := judgeIsolation(isolationRun{setup: "x", cleanup: got})
	if !strings.Contains(v.reason, "delete it by hand") {
		t.Errorf("reason = %s", v.reason)
	}
}

// slotPolicyObject is the chart's slot policy in slot 0: the gateway's
// label, plus blocks when given.
func slotPolicyObject(gateway bool, blocks ...string) *networkingv1.NetworkPolicy {
	var peers []networkingv1.NetworkPolicyPeer
	if gateway {
		peers = append(peers, networkingv1.NetworkPolicyPeer{
			PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{gatewayLabel: "true"}}})
	}
	for _, b := range blocks {
		peers = append(peers, networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: b}})
	}
	return &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: "patchy-preview-0",
		Name: slotPolicyName}, Spec: networkingv1.NetworkPolicySpec{Ingress: []networkingv1.NetworkPolicyIngressRule{
		{From: peers}}}}
}

// TestInstallIsolation: preview-netpol through Install, from the
// settings: the default image from the prefix and the CLI's commit, the
// slot policy's ipBlocks as a WARN, and every reason the probe cannot run
// a FAIL with no Pod created. No other check writes, and none reads a
// Secret.
func TestInstallIsolation(t *testing.T) {
	pass := scenario{listener: "ready", gateway: outcomeReached, agent: outcomeBlocked, release: outcomeBlocked}
	tests := map[string]struct {
		change  func(*installWorld, *Isolation)
		policy  client.Object
		status  checkreport.Status
		want    []string
		created bool
	}{
		"passes": {policy: slotPolicyObject(true), status: checkreport.Pass, created: true,
			want: []string{"only the preview gateway's label reaches the slot"}},
		"legacy subnets admitted": {policy: slotPolicyObject(true, "10.40.128.0/24"), status: checkreport.Warn,
			created: true, want: []string{"also admits 10.40.128.0/24"}},
		"no slot policy": {status: checkreport.Fail, want: []string{"no slot NetworkPolicy"}},
		"policy without the gateway": {policy: slotPolicyObject(false), status: checkreport.Fail,
			want: []string{"admits no preview gateway Pod"}},
		"no commit": {policy: slotPolicyObject(true), status: checkreport.Fail,
			change: func(_ *installWorld, iso *Isolation) { iso.Commit = "none" },
			want:   []string{"built without a commit", "--isolation-image " + probePrefix + "netprobe:sha-"}},
		"image off the prefix": {policy: slotPolicyObject(true), status: checkreport.Fail,
			change: func(_ *installWorld, iso *Isolation) {
				iso.Image = "ghcr.io/other/netprobe:sha-" + probeCommit
			},
			want: []string{"not under preview-controller's image prefix"}},
		"slot out of range": {policy: slotPolicyObject(true), status: checkreport.Fail,
			change: func(_ *installWorld, iso *Isolation) { iso.Slot = 2 },
			want:   []string{"slot 2 is not one of the install's 2 slots"}},
		"a probe Pod exists": {policy: slotPolicyObject(true), status: checkreport.Fail,
			change: func(w *installWorld, _ *Isolation) {
				w.objs = append(w.objs, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "patchy",
					Name: probeGatewayClient}})
			},
			want: []string{"the Pod patchy/preview-ingress-gateway already exists"}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			w := newInstallWorld()
			w.setting("patchy-preview-controller", keyPreviewImagePrefix, probePrefix)
			w.objs = append(w.objs, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "patchy",
				Name: "patchy-preview-auth-keys"}})
			if tc.policy != nil {
				w.objs = append(w.objs, tc.policy)
			}
			iso := &Isolation{Commit: probeCommit, Timeout: 50 * time.Millisecond, Poll: time.Millisecond}
			if tc.change != nil {
				tc.change(w, iso)
			}
			f := newFakeProbeCluster(pass, w.objs...)
			iso.Client, iso.Logs = f.client, f.logs
			r := Install(context.Background(), Config{Reader: f.client, Namespace: "patchy", Resolver: w.resolver,
				DialTLS: iDial, HTTP: w.web, Isolation: iso})
			expect(t, r.Checks, CheckPreviewNetpol, tc.status, tc.want...)
			if r.Checks[len(r.Checks)-1].Name != CheckPreviewNetpol {
				t.Errorf("preview-netpol is not the last line:\n%s", dump(r.Checks))
			}
			if got := len(f.created) > 0; got != tc.created {
				t.Errorf("Pods created = %v, want %v", f.created, tc.created)
			}
			if f.secrets > 0 {
				t.Errorf("read %d Secrets", f.secrets)
			}
			for _, p := range f.remaining(t) {
				if p != "patchy/"+probeGatewayClient || name != "a probe Pod exists" {
					t.Errorf("Pod %s left behind", p)
				}
			}
		})
	}
}

// TestInstallWithoutIsolationWritesNothing: with no Isolation, Install
// creates nothing and reports no preview-netpol line.
func TestInstallWithoutIsolationWritesNothing(t *testing.T) {
	w := newInstallWorld()
	w.objs = append(w.objs, slotPolicyObject(true))
	writes := 0
	r := w.run(t, interceptor.Funcs{
		Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
			writes++
			return nil
		},
	})
	for _, l := range r.Checks {
		if l.Name == CheckPreviewNetpol {
			t.Errorf("a preview-netpol line without --isolation: %s", l.Reason)
		}
	}
	if writes > 0 {
		t.Errorf("%d writes without --isolation", writes)
	}
}
