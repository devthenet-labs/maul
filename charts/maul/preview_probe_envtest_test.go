// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package chart_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/devthenet-labs/maul/internal/kube"
	"github.com/devthenet-labs/maul/internal/rbactest"
)

// probeDir is the live isolation probe (hack/preview-isolation-probe), whose
// sibling run creates a stand-in component Service in slot 0.
const probeDir = "../../hack/preview-isolation-probe"

// testIsolationProbeService runs the isolation probe's own command for its
// sibling Service, word for word from run.sh, through kubectl against the
// rendered slot policies. Regression: the probe client-side applied that
// Service, and apply's last-applied annotation is refused by the slot Service
// policy, which admits no annotation but a health-check path, so the sibling
// run could never pass. The same Service applied is refused here too, which
// shows the check can fail.
func testIsolationProbeService(t *testing.T, env *envtest.Environment) {
	t.Helper()
	user, err := env.AddUser(envtest.User{Name: "probe-operator", Groups: []string{"system:masters"}}, nil)
	if err != nil {
		t.Fatalf("add kubectl user: %v", err)
	}
	kubectl, err := user.Kubectl()
	if err != nil {
		t.Fatalf("kubectl: %v", err)
	}
	dir, err := filepath.Abs(probeDir)
	if err != nil {
		t.Fatal(err)
	}
	args := probeServiceCommand(t, dir)
	// A server-side dry run: the policies decide, and nothing is stored.
	if _, stderr, err := kubectl.Run(append(args, "--dry-run=server")...); err != nil {
		t.Errorf("run.sh's sibling Service command (kubectl %s) is refused: %v: %s",
			strings.Join(args, " "), err, readAll(t, stderr))
	}
	_, stderr, err := kubectl.Run("apply", "-n", "maul-preview-0", "-f",
		filepath.Join(dir, "sibling-service.yaml"), "--dry-run=server")
	if out := readAll(t, stderr); err == nil || !strings.Contains(out, "restricted to a safe healthcheck path") {
		t.Errorf("the sibling Service client-side applied: %v: %s; want refused for its last-applied annotation",
			err, out)
	}
}

// probeServiceCommand is the kubectl invocation run.sh creates the sibling
// Service with: its one line naming sibling-service.yaml, split into words,
// with $probe_dir expanded to dir. A line with any other shell syntax fails
// the test rather than being run differently from how bash would run it.
func probeServiceCommand(t *testing.T, dir string) []string {
	t.Helper()
	script, err := os.ReadFile(filepath.Join(dir, "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	var commands []string
	for line := range strings.SplitSeq(string(script), "\n") {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "sibling-service.yaml") && !strings.HasPrefix(line, "#") {
			commands = append(commands, line)
		}
	}
	if len(commands) != 1 {
		t.Fatalf("run.sh names sibling-service.yaml on %d command lines, want 1: %q", len(commands), commands)
	}
	words := strings.Fields(commands[0])
	if words[0] != "kubectl" {
		t.Fatalf("run.sh's sibling Service command %q is not a kubectl command", commands[0])
	}
	args := make([]string, 0, len(words)-1)
	for _, word := range words[1:] {
		word = strings.Trim(word, `"`)
		if strings.ContainsAny(strings.ReplaceAll(word, "$probe_dir", ""), "$`'\"\\|;&<>(){}*?") {
			t.Fatalf("run.sh's sibling Service command %q has shell syntax this test cannot reproduce", commands[0])
		}
		args = append(args, strings.ReplaceAll(word, "$probe_dir", dir))
	}
	return args
}

func readAll(t *testing.T, r io.Reader) string {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// testIngressProbePods creates the ingress-direction run's four Pods
// (ingress.sh --print-manifests with placement, a preview image and the
// clients in an ordinary namespace) with a server-side dry run against the
// rendered slot policies: the listener must be admissible in a slot, and
// the clients outside one.
func testIngressProbePods(t *testing.T, env *envtest.Environment, placement ...string) {
	t.Helper()
	user, err := env.AddUser(envtest.User{Name: "ingress-probe-operator", Groups: []string{"system:masters"}}, nil)
	if err != nil {
		t.Fatalf("add kubectl user: %v", err)
	}
	kubectl, err := user.Kubectl()
	if err != nil {
		t.Fatalf("kubectl: %v", err)
	}
	dir, err := filepath.Abs(probeDir)
	if err != nil {
		t.Fatal(err)
	}
	args := append([]string{filepath.Join(dir, "ingress.sh"), "--print-manifests", "--image", previewImage,
		"--release-namespace", "ordinary", "--agents-namespace", "ordinary"}, placement...)
	out, err := exec.CommandContext(t.Context(), "bash", args...).Output()
	if err != nil {
		t.Fatalf("ingress.sh --print-manifests: %v", err)
	}
	docs := strings.Split(string(out), "---\n")
	if len(docs) != 4 {
		t.Fatalf("ingress.sh printed %d manifests, want 4:\n%s", len(docs), out)
	}
	for i, doc := range docs {
		file := filepath.Join(t.TempDir(), "pod.yaml")
		if err := os.WriteFile(file, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, stderr, err := kubectl.Run("create", "-f", file, "--dry-run=server"); err != nil {
			t.Errorf("ingress.sh's Pod %d is refused: %v: %s\n%s", i, err, readAll(t, stderr), doc)
		}
	}
}

// probeArgs render the chart as a gateway-only k3s install with acceptance
// verification on: the shape the desktop live gate runs the probes on.
var probeArgs = []string{
	"-f", "../../hack/testdata/chart-render/intent-controller.yaml",
	"-f", "../../cmd/maul/internal/previewsetup/testdata/golden/cloudflare-tunnel-k3s/maul-previews.values.yaml",
	"--set", "previewAuth.keys.renderOffline=true",
	"--set", "previewController.config.apiServerCIDR=10.43.0.1/32",
	"--set", "preview.verification.enabled=true",
	"--set", "preview.verification.testerImage=" + testerImage,
}

// probeImage is a netprobe image under that install's preview prefix.
const probeImage = "ghcr.io/acme/maul/previews/netprobe:sha-" + previewSHA

// TestIsolationProbeAgainstVerificationPolicies runs the live isolation
// probe's own pieces against the chart's rendered slot policies with
// acceptance verification on. The edges run's fixture (edges.yaml, which
// probe_edges_test.go holds to preview-controller's render) is admitted as
// preview-controller writes it, and its Services and stand-ins are
// admitted in the slots. ingress.sh's static check (--check-policies, its
// own kubectl against this API server) passes a slot holding those
// policies, and refuses each misshapen one the admission policy refuses
// preview-controller: the operator may write such a policy, so the script
// must judge it by the same predicate.
func TestIsolationProbeAgainstVerificationPolicies(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via mise run envtest")
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH; ingress.sh requires it")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})
	admin, err := client.New(cfg, client.Options{Scheme: kube.Scheme()})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	release := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: egressRelease}}
	if err := admin.Create(t.Context(), release); err != nil {
		t.Fatalf("release namespace: %v", err)
	}
	applyProbeRender(t, admin)
	pcConfig := rest.CopyConfig(cfg)
	pcConfig.Impersonate = rest.ImpersonationConfig{UserName: saPreview}
	pc, err := client.New(pcConfig, client.Options{Scheme: kube.Scheme()})
	if err != nil {
		t.Fatalf("client as preview-controller: %v", err)
	}
	rbactest.WaitAllowed(t, admin, rbactest.Verbs(saPreview, nil, authorizationv1.ResourceAttributes{
		Namespace: "maul-preview-0", Group: "networking.k8s.io", Resource: "networkpolicies"}, "create")...)
	fixture := probeFixture(t)
	waitFor(t, func() error {
		for _, probe := range []struct {
			c    client.Client
			obj  client.Object
			want string
		}{
			{pc, mutateNP(fixture[0].DeepCopy(), func(np *networkingv1.NetworkPolicy) {
				np.Labels["app.kubernetes.io/managed-by"] = "maul-preview-controller"
				np.Name = "preview-edges-np-wait"
				np.Spec.PolicyTypes = []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}
			}), npTypesDenied},
			{admin, deployment("maul-preview-0", "probe", "nginx:latest"), "only immutable"},
			{admin, service("maul-preview-0", "probe", corev1.ServiceTypeLoadBalancer), "internal ClusterIP"},
		} {
			err := probe.c.Create(t.Context(), probe.obj.DeepCopyObject().(client.Object), client.DryRunAll)
			if !deniedWith(err, probe.want) {
				return &waitError{err: err, want: probe.want}
			}
		}
		return nil
	})

	kubeconfig, kubectlDir := probeKubeconfig(t, env)
	t.Run("edges objects", func(t *testing.T) { checkEdgesObjects(t, admin, fixture) })
	t.Run("ingress pre-check", func(t *testing.T) {
		checkIngressPreCheck(t, admin, pc, fixture, kubeconfig, kubectlDir)
	})
}

// applyProbeRender creates what the probe's pieces meet from the chart
// rendered with probeArgs: the slot namespaces, the Roles and bindings,
// the slot isolation policies and every admission policy and binding.
func applyProbeRender(t *testing.T, admin client.Client) {
	t.Helper()
	args := append([]string{"template", "maul", ".", "--namespace", egressRelease}, probeArgs...)
	output, err := exec.CommandContext(t.Context(), "helm", args...).Output()
	if err != nil {
		t.Fatalf("render chart: %v", err)
	}
	dec := yaml.NewYAMLOrJSONDecoder(strings.NewReader(string(output)), 4096)
	var namespaces, others []*unstructured.Unstructured
	for {
		u := &unstructured.Unstructured{}
		if err := dec.Decode(u); err != nil {
			break
		}
		switch u.GetKind() {
		case "Namespace":
			namespaces = append(namespaces, u)
		case "Role", "RoleBinding", "ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding":
			others = append(others, u)
		case "NetworkPolicy":
			if u.GetName() == "preview-isolation" {
				others = append(others, u)
			}
		}
	}
	for _, u := range append(namespaces, others...) {
		if err := admin.Create(t.Context(), u); err != nil {
			t.Fatalf("the API server refused %s %s/%s: %v", u.GetKind(), u.GetNamespace(), u.GetName(), err)
		}
	}
}

// probeFixture is edges.yaml's policies, in the first slot.
func probeFixture(t *testing.T) []*networkingv1.NetworkPolicy {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(probeDir, "edges.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var out []*networkingv1.NetworkPolicy
	dec := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(b), 4096)
	for {
		np := &networkingv1.NetworkPolicy{}
		if err := dec.Decode(np); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		np.Namespace = "maul-preview-0"
		out = append(out, np)
	}
	if len(out) == 0 {
		t.Fatal("edges.yaml holds no policy")
	}
	return out
}

// probeKubeconfig is a kubeconfig for a cluster admin of env, and the
// directory of the kubectl that suits it, for the probe scripts.
func probeKubeconfig(t *testing.T, env *envtest.Environment) (string, string) {
	t.Helper()
	user, err := env.AddUser(envtest.User{Name: "probe-scripts", Groups: []string{"system:masters"}}, nil)
	if err != nil {
		t.Fatalf("add probe user: %v", err)
	}
	kc, err := user.KubeConfig()
	if err != nil {
		t.Fatalf("kubeconfig: %v", err)
	}
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, kc, 0o600); err != nil {
		t.Fatal(err)
	}
	kubectl, err := user.Kubectl()
	if err != nil {
		t.Fatalf("kubectl: %v", err)
	}
	return path, filepath.Dir(kubectl.Path)
}

// checkEdgesObjects: every object the edges run creates is admitted in
// the slots: the fixture's policies as the operator creates them, and also
// as preview-controller would write them (its managed-by label), which the
// admission policy holds to its shape; and the Services and stand-ins
// run.sh edges --print-manifests prints.
func checkEdgesObjects(t *testing.T, admin client.Client, fixture []*networkingv1.NetworkPolicy) {
	for _, np := range fixture {
		admittedAs(t, admin, np.DeepCopy())
	}
	out, err := exec.CommandContext(t.Context(), "bash", filepath.Join(probeDir, "run.sh"), "edges",
		"--print-manifests", "--image", probeImage, "--node-selector", "maul.run/previews=true",
		"--taint-key", "maul.run/preview-only").Output()
	if err != nil {
		t.Fatalf("run.sh edges --print-manifests: %v", err)
	}
	kinds := map[string]int{}
	dec := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(out), 4096)
	for {
		u := &unstructured.Unstructured{}
		if err := dec.Decode(u); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		kinds[u.GetKind()]++
		if err := admin.Create(t.Context(), u, client.DryRunAll); err != nil {
			t.Errorf("run.sh edges' %s %s/%s is refused: %v", u.GetKind(), u.GetNamespace(), u.GetName(), err)
		}
	}
	if kinds["Deployment"] != 5 || kinds["Service"] != 3 {
		t.Errorf("run.sh edges printed %v, want five Deployments and three Services", kinds)
	}
}

// checkIngressPreCheck: preview-controller writes the fixture's policies
// into the slot and ingress.sh --check-policies passes it; then each
// misshapen edge policy, refused to preview-controller but the operator's
// to write, fails the script's check.
func checkIngressPreCheck(t *testing.T, admin, pc client.Client, fixture []*networkingv1.NetworkPolicy,
	kubeconfig, kubectlDir string) {
	ctx := t.Context()
	asController := func(np *networkingv1.NetworkPolicy) *networkingv1.NetworkPolicy {
		np = np.DeepCopy()
		np.Labels["app.kubernetes.io/managed-by"] = "maul-preview-controller"
		return np
	}
	for _, np := range fixture {
		if err := pc.Create(ctx, asController(np)); err != nil {
			t.Fatalf("preview-controller writing %s: %v", np.Name, err)
		}
	}
	check := func() (int, string) {
		cmd := exec.CommandContext(ctx, "bash", filepath.Join(probeDir, "ingress.sh"), "--check-policies",
			"--image", probeImage, "--no-node-isolation")
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfig,
			"PATH="+kubectlDir+string(os.PathListSeparator)+os.Getenv("PATH"))
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		err := cmd.Run()
		var exit *exec.ExitError
		switch {
		case err == nil:
			return 0, stderr.String()
		case errors.As(err, &exit):
			return exit.ExitCode(), stderr.String()
		}
		t.Fatalf("ingress.sh: %v", err)
		return 0, ""
	}
	if code, stderr := check(); code != 0 || !strings.Contains(stderr, "admit only the preview gateway") {
		t.Fatalf("ingress.sh --check-policies on preview-controller's policies: exit %d:\n%s", code, stderr)
	}
	tester := func() *networkingv1.NetworkPolicy {
		for _, np := range fixture {
			if np.Spec.PodSelector.MatchLabels["maul.run/preview-part"] == "tester" {
				return asController(np)
			}
		}
		t.Fatal("edges.yaml has no tester policy")
		return nil
	}
	named := intstr.FromString("http")
	for name, mutate := range map[string]func(*networkingv1.NetworkPolicy){
		"an egress ipBlock": func(np *networkingv1.NetworkPolicy) {
			np.Spec.Egress[0].To[0] = networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0"}}
		},
		"an egress namespaceSelector": func(np *networkingv1.NetworkPolicy) {
			np.Spec.Egress[0].To[0].NamespaceSelector = &metav1.LabelSelector{}
		},
		"an ingress from another namespace": func(np *networkingv1.NetworkPolicy) {
			np.Spec.Ingress = []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"kubernetes.io/metadata.name": "maul-agents"}}}},
				Ports: []networkingv1.NetworkPolicyPort{tcpPort(8080)},
			}}
		},
		"a named port": func(np *networkingv1.NetworkPolicy) { np.Spec.Egress[0].Ports[0].Port = &named },
		"a foreign UID": func(np *networkingv1.NetworkPolicy) {
			np.Spec.Egress[0].To[0].PodSelector.MatchLabels["maul.run/preview-uid"] = "99999999-2222-3333-4444-555555555555"
		},
		"one policy type": func(np *networkingv1.NetworkPolicy) {
			np.Spec.PolicyTypes = []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}
		},
		"an extra label":    func(np *networkingv1.NetworkPolicy) { np.Spec.PodSelector.MatchLabels["app"] = "x" },
		"no egress port":    func(np *networkingv1.NetworkPolicy) { np.Spec.Egress[0].Ports = nil },
		"an empty peer set": func(np *networkingv1.NetworkPolicy) { np.Spec.Egress[0].To = nil },
	} {
		t.Run(name, func(t *testing.T) {
			np := mutateNP(tester(), mutate)
			np.Name = "preview-edges-np-" + slug(name)
			if err := pc.Create(ctx, np.DeepCopy(), client.DryRunAll); !deniedWith(err, npDenied, npShapeDenied,
				npTypesDenied) {
				t.Errorf("preview-controller writing %s: %v, want the edge policy admission's refusal", name, err)
			}
			if err := admin.Create(ctx, np); err != nil {
				t.Fatalf("the operator writing %s: %v", name, err)
			}
			t.Cleanup(func() {
				if err := admin.Delete(context.Background(), np); err != nil {
					t.Errorf("delete %s: %v", np.Name, err)
				}
			})
			if code, stderr := check(); code != 1 || !strings.Contains(stderr, "admit more than the preview gateway") ||
				!strings.Contains(stderr, np.Name) {
				t.Errorf("ingress.sh --check-policies with %s: exit %d:\n%s", name, code, stderr)
			}
		})
	}
}
