// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package chart_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/devthenet-labs/maul/internal/kube"
	"github.com/devthenet-labs/maul/internal/rbactest"
)

const (
	toolShapeDenied    = "preview tool pods run tools with at most a browser and a sandbox native sidecar"
	toolVolumesDenied  = "preview tool pods mount one preview- ConfigMap in tools alone"
	toolPodJobDenied   = "a preview tool pod belongs to its Job"
	toolDeployDenied   = "preview components never carry the tool pod's role"
	toolIngressDenied  = "the tool pod's ingress policy admits exactly its own evaluator"
	toolReachDenied    = "nothing in a slot reaches the tool pod"
	toolObjectsDenied  = "preview-controller writes only its own preview- Secrets and ConfigMaps in a slot"
	toolLedgersDenied  = "preview-controller writes only a PreviewVerification's own ledger and evidence ConfigMaps"
	toolKeySecret      = "preview-battles-tools-key"
	toolConfigMap      = "preview-battles-tools"
	toolVerification   = "battles-1-v1-a1"
	toolManagedByLabel = "app.kubernetes.io/managed-by"
)

// toolsEnvArgs renders the tool pod for the envtest suites: one evaluation
// round shorter than their slot Job deadline.
var toolsEnvArgs = append(append([]string{}, toolsArgs...),
	"--set", "intentController.verification.evaluateTimeout=5m")

// startToolsEnv starts an API server holding the chart rendered with the
// evaluator's tool pod (slot namespaces, CRDs, every Role and binding, the
// slot isolation policies and every admission policy and binding), lets
// every identity the cases act as write Jobs and Pods in the agents
// namespace and the first slot, and waits until the controllers' grants
// and the tool rules are in force. It returns the admin client and a
// client per identity.
func startToolsEnv(t *testing.T) (*envtest.Environment, client.Client, func(string) client.Client) {
	t.Helper()
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
	applySlotRender(t, admin, toolsEnvArgs)
	grantEvaluatorLabelTesters(t, admin)
	as := func(user string) client.Client {
		c := rest.CopyConfig(cfg)
		c.Impersonate = rest.ImpersonationConfig{UserName: user}
		cl, err := client.New(c, client.Options{Scheme: kube.Scheme()})
		if err != nil {
			t.Fatalf("client as %s: %v", user, err)
		}
		return cl
	}
	waitVerificationGrants(t, admin)
	slot := func(resource string) authorizationv1.ResourceAttributes {
		return authorizationv1.ResourceAttributes{Namespace: "maul-preview-0", Resource: resource}
	}
	reviews := rbactest.Verbs(saPreview, nil, slot("secrets"), "create", "delete")
	reviews = append(reviews, rbactest.Verbs(saPreview, nil, slot("configmaps"), "create", "update", "delete")...)
	reviews = append(reviews, rbactest.Verbs(saPreview, nil, slot("events"), "get", "list", "watch")...)
	reviews = append(reviews, rbactest.Verbs(saPreview, nil, authorizationv1.ResourceAttributes{
		Namespace: egressRelease, Resource: "configmaps"}, "create", "delete")...)
	rbactest.WaitAllowed(t, admin, reviews...)
	pc := as(saPreview)
	waitFor(t, func() error {
		for _, probe := range []struct {
			c    client.Client
			obj  client.Object
			want string
		}{
			{pc, mutateJob(toolJob("probe"), func(j *batchv1.Job) {
				j.Spec.Template.Spec.InitContainers[1].Image = toolsImage
			}), toolShapeDenied},
			{pc, mutateNP(toolIngress("probe"), func(np *networkingv1.NetworkPolicy) {
				np.Spec.Ingress[0].Ports[0] = tcpPort(8091)
			}), toolIngressDenied},
			{pc, slotSecret("maul-preview-oidc-g9"), toolObjectsDenied},
			{pc, ledger("probe-ledger", ""), toolLedgersDenied},
		} {
			err := probe.c.Create(t.Context(), probe.obj.DeepCopyObject().(client.Object), client.DryRunAll)
			if !deniedWith(err, probe.want) {
				return &waitError{err: err, want: probe.want}
			}
		}
		return nil
	})
	return env, admin, as
}

// TestPreviewToolPodPoliciesAgainstAPIServer drives the slot policies the
// agent evaluator's tool pod adds, as the identities they judge: only
// preview-controller's Job runs it, with exactly the three images in their
// own containers, its key Secret and tool ConfigMap mounted where they
// belong, and every other slot pod rule unchanged; its per-verification
// ingress policy admits exactly its own evaluator; nothing in the slot
// reaches it; preview-controller's new Secret and ConfigMap grants reach
// only its own objects; and the isolation probe's evaluator run is
// admitted under the fence it tests.
func TestPreviewToolPodPoliciesAgainstAPIServer(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via mise run envtest")
	}
	_, admin, as := startToolsEnv(t)
	pc := as(saPreview)
	t.Run("jobs", func(t *testing.T) { checkToolJobs(t, admin, pc) })
	t.Run("pods", func(t *testing.T) { checkToolPods(t, admin, as(saJobController)) })
	t.Run("networkpolicies", func(t *testing.T) { checkToolPolicies(t, admin, pc) })
	t.Run("secrets and configmaps", func(t *testing.T) { checkToolObjects(t, admin, pc) })
	t.Run("isolation probe evaluator run", func(t *testing.T) { checkEvaluatorProbeObjects(t, admin, as(saIntent)) })
}

func checkToolJobs(t *testing.T, admin, pc client.Client) {
	admittedAs(t, pc, toolJob("tools"))
	admittedAs(t, pc, mutateJob(toolJob("no-sidecars"), func(j *batchv1.Job) {
		j.Spec.Template.Spec.InitContainers = nil
	}))
	admittedAs(t, pc, mutateJob(toolJob("browser-only"), func(j *batchv1.Job) {
		j.Spec.Template.Spec.InitContainers = j.Spec.Template.Spec.InitContainers[:1]
	}))
	admittedAs(t, pc, testerJob("tester"))
	// maul-evaluator-labels refuses it too; either refusal will do.
	if err := admin.Create(t.Context(), toolJob("operator"), client.DryRunAll); !deniedWith(err, jobOnlyPC,
		evWriterDenied) {
		t.Errorf("a tool Job by anyone but preview-controller: %v, want refused", err)
	}
	yes := true
	always := corev1.ContainerRestartPolicyAlways
	foreign := "ghcr.io/devthenet-labs/maul/preview-extra@sha256:" + strings.Repeat("4", 64)
	for _, tc := range []struct {
		name   string
		mutate func(*batchv1.Job)
		want   string
	}{
		{"a fourth image", func(j *batchv1.Job) { j.Spec.Template.Spec.InitContainers[1].Image = foreign }, toolShapeDenied},
		{"the tools image by tag", func(j *batchv1.Job) {
			j.Spec.Template.Spec.Containers[0].Image = "ghcr.io/devthenet-labs/maul/preview-tools:v1"
		}, toolShapeDenied},
		{"the browser image in the sandbox", func(j *batchv1.Job) {
			j.Spec.Template.Spec.InitContainers[1].Image = browserImage
		}, toolShapeDenied},
		{"a third sidecar", func(j *batchv1.Job) {
			extra := j.Spec.Template.Spec.InitContainers[1]
			extra.Name = "extra"
			extra.VolumeMounts = nil
			j.Spec.Template.Spec.InitContainers = append(j.Spec.Template.Spec.InitContainers, extra)
		}, toolShapeDenied},
		{"a plain init container", func(j *batchv1.Job) { j.Spec.Template.Spec.InitContainers[1].RestartPolicy = nil },
			toolShapeDenied},
		{"a main container named otherwise", func(j *batchv1.Job) { j.Spec.Template.Spec.Containers[0].Name = "relay" },
			toolShapeDenied},
		{"two main containers", func(j *batchv1.Job) {
			c := j.Spec.Template.Spec.Containers[0]
			c.Name, c.VolumeMounts = "second", nil
			j.Spec.Template.Spec.Containers = append(j.Spec.Template.Spec.Containers, c)
		}, "exactly one container"},
		{"a hostPath", func(j *batchv1.Job) {
			j.Spec.Template.Spec.Volumes = append(j.Spec.Template.Spec.Volumes, corev1.Volume{Name: "host",
				VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/var/run"}}})
		}, toolVolumesDenied},
		{"a projected token", func(j *batchv1.Job) {
			j.Spec.Template.Spec.Volumes = append(j.Spec.Template.Spec.Volumes, corev1.Volume{Name: "token",
				VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
					Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
						Path: "token"}}}}}})
		}, toolVolumesDenied},
		{"a mounted API token", func(j *batchv1.Job) { j.Spec.Template.Spec.AutomountServiceAccountToken = &yes },
			"API token"},
		{"the key Secret in the sandbox", func(j *batchv1.Job) {
			s := &j.Spec.Template.Spec.InitContainers[1]
			s.VolumeMounts = append(s.VolumeMounts, corev1.VolumeMount{Name: "key", MountPath: "/key"})
		}, toolVolumesDenied},
		{"the tool ConfigMap in the browser", func(j *batchv1.Job) {
			b := &j.Spec.Template.Spec.InitContainers[0]
			b.VolumeMounts = append(b.VolumeMounts, corev1.VolumeMount{Name: "config", MountPath: "/config"})
		}, toolVolumesDenied},
		{"a second Secret", func(j *batchv1.Job) {
			j.Spec.Template.Spec.Volumes = append(j.Spec.Template.Spec.Volumes, corev1.Volume{Name: "key2",
				VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "preview-battles-2"}}})
		}, toolVolumesDenied},
		{"a sign-in Secret", func(j *batchv1.Job) {
			j.Spec.Template.Spec.Volumes[1].Secret.SecretName = "maul-preview-oidc-g1"
		}, toolVolumesDenied},
		{"the root CA ConfigMap", func(j *batchv1.Job) {
			j.Spec.Template.Spec.Volumes[0].ConfigMap.Name = "kube-root-ca.crt"
		}, toolVolumesDenied},
		{"no node selector", func(j *batchv1.Job) { j.Spec.Template.Spec.NodeSelector = nil }, "dedicated preview"},
		{"host network", func(j *batchv1.Job) { j.Spec.Template.Spec.HostNetwork = true }, "host namespaces"},
		{"no pod deadline", func(j *batchv1.Job) { j.Spec.Template.Spec.ActiveDeadlineSeconds = nil }, jobDeadline},
		// Every other role keeps today's rules.
		{"a tester with a sidecar", func(j *batchv1.Job) {
			toTester(j)
			j.Spec.Template.Spec.InitContainers[1].RestartPolicy = &always
		}, "no init or ephemeral"},
		{"a tester in the tools image", func(j *batchv1.Job) {
			toTester(j)
			j.Spec.Template.Spec.Containers[0].Image = toolsImage
			j.Spec.Template.Spec.InitContainers = nil
			j.Spec.Template.Spec.Volumes = nil
			j.Spec.Template.Spec.Containers[0].VolumeMounts = nil
		}, jobImage},
	} {
		refusedAs(t, pc, mutateJob(toolJob(slug(tc.name)), tc.mutate), tc.want, tc.name)
	}
}

// toTester relabels a tool Job as a tester Job in the tester's image.
func toTester(j *batchv1.Job) {
	for _, l := range []map[string]string{j.Labels, j.Spec.Template.Labels} {
		delete(l, evEvaluation)
		l[evPreviewRole] = "tester"
	}
	j.Spec.Template.Labels["maul.run/preview-part"] = "tester"
	j.Spec.Template.Spec.Containers[0].Image = testerImage
}

// checkToolPods: the pod the Job controller creates for a tool Job is
// admitted, and judged by the same rules; a Deployment never carries the
// role, and a bare Pod carrying it is refused.
func checkToolPods(t *testing.T, admin, jobController client.Client) {
	admittedAs(t, jobController, toolPodNamed("tools"))
	refusedAs(t, jobController, mutatePod(toolPodNamed("key-in-sandbox"), func(p *corev1.Pod) {
		s := &p.Spec.InitContainers[1]
		s.VolumeMounts = append(s.VolumeMounts, corev1.VolumeMount{Name: "key", MountPath: "/key"})
	}), toolVolumesDenied, "a tool pod with the key in its sandbox")
	refusedAs(t, jobController, mutatePod(toolPodNamed("foreign"), func(p *corev1.Pod) {
		p.Spec.InitContainers[0].Image = toolboxImage
	}), toolShapeDenied, "a tool pod with the toolbox image in its browser")
	// maul-evaluator-labels refuses it too (only the Job controller labels
	// a pod tools); either refusal will do.
	bare := toolPodNamed("bare")
	bare.OwnerReferences = nil
	if err := admin.Create(t.Context(), bare, client.DryRunAll); !deniedWith(err, toolPodJobDenied, evPodDenied) {
		t.Errorf("a bare tool pod: %v, want refused", err)
	}
	d := deployment("maul-preview-0", "component", previewImage)
	d.Spec.Template.Labels[evPreviewRole] = "tools"
	refusedAs(t, admin, d, toolDeployDenied, "a component labelled as the tool pod")
}

// checkToolPolicies: the tool pod's per-verification ingress policy admits
// exactly its own evaluator; its edge policy is an ordinary one for part
// tools, with egress only; nothing egresses to it; and the tool isolation
// policy is Helm's, like preview-isolation.
func checkToolPolicies(t *testing.T, admin, pc client.Client) {
	ctx := t.Context()
	admittedAs(t, pc, toolIngress("ingress"))
	toolEdge := mutateNP(edgePolicy("tools"), func(np *networkingv1.NetworkPolicy) {})
	admittedAs(t, pc, toolEdge)
	admittedAs(t, pc, mutateNP(componentPolicy("api"), func(np *networkingv1.NetworkPolicy) {
		np.Name = "preview-battles-np-api-from-tools"
		np.Spec.Ingress[0].From[0].PodSelector = ptrSelector(partSelector("tools"))
	}))
	for _, tc := range []struct {
		name   string
		mutate func(*networkingv1.NetworkPolicy)
		want   string
	}{
		{"another namespace", func(np *networkingv1.NetworkPolicy) {
			np.Spec.Ingress[0].From[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] = egressRelease
		}, toolIngressDenied},
		{"a namespace by another label", func(np *networkingv1.NetworkPolicy) {
			np.Spec.Ingress[0].From[0].NamespaceSelector.MatchLabels = map[string]string{"team": "agents"}
		}, toolIngressDenied},
		{"any namespace", func(np *networkingv1.NetworkPolicy) {
			np.Spec.Ingress[0].From[0].NamespaceSelector = &metav1.LabelSelector{}
		}, toolIngressDenied},
		{"no namespace", func(np *networkingv1.NetworkPolicy) { np.Spec.Ingress[0].From[0].NamespaceSelector = nil },
			toolIngressDenied},
		{"an extra peer label", func(np *networkingv1.NetworkPolicy) {
			np.Spec.Ingress[0].From[0].PodSelector.MatchLabels["app"] = "x"
		}, toolIngressDenied},
		{"a peer selected by expression", func(np *networkingv1.NetworkPolicy) {
			np.Spec.Ingress[0].From[0].PodSelector.MatchExpressions = []metav1.LabelSelectorRequirement{{
				Key: "app", Operator: metav1.LabelSelectorOpExists}}
		}, toolIngressDenied},
		{"another verification's evaluator", func(np *networkingv1.NetworkPolicy) {
			np.Spec.Ingress[0].From[0].PodSelector.MatchLabels[evEvaluation] = "other"
		}, toolIngressDenied},
		{"a check reviewer", func(np *networkingv1.NetworkPolicy) {
			np.Spec.Ingress[0].From[0].PodSelector.MatchLabels[evRole] = "checkreview"
		}, toolIngressDenied},
		{"an ipBlock beside it", func(np *networkingv1.NetworkPolicy) {
			np.Spec.Ingress[0].From = append(np.Spec.Ingress[0].From,
				networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: "10.0.0.0/8"}})
		}, toolIngressDenied},
		{"a second rule", func(np *networkingv1.NetworkPolicy) {
			np.Spec.Ingress = append(np.Spec.Ingress, np.Spec.Ingress[0])
		}, toolIngressDenied},
		{"port 8091", func(np *networkingv1.NetworkPolicy) { np.Spec.Ingress[0].Ports[0] = tcpPort(8091) },
			toolIngressDenied},
		{"a second port", func(np *networkingv1.NetworkPolicy) {
			np.Spec.Ingress[0].Ports = append(np.Spec.Ingress[0].Ports, tcpPort(9222))
		}, toolIngressDenied},
		{"egress too", func(np *networkingv1.NetworkPolicy) {
			np.Spec.PolicyTypes = append(np.Spec.PolicyTypes, networkingv1.PolicyTypeEgress)
			np.Spec.Egress = []networkingv1.NetworkPolicyEgressRule{{
				To:    []networkingv1.NetworkPolicyPeer{{PodSelector: ptrSelector(partSelector("api"))}},
				Ports: []networkingv1.NetworkPolicyPort{tcpPort(8080)}}}
		}, toolIngressDenied},
		{"an extra target label", func(np *networkingv1.NetworkPolicy) {
			np.Spec.PodSelector.MatchLabels["app"] = "x"
		}, npShapeDenied},
		{"a target with no verification", func(np *networkingv1.NetworkPolicy) {
			delete(np.Spec.PodSelector.MatchLabels, evEvaluation)
			np.Spec.PodSelector.MatchLabels["maul.run/preview-uid"] = verifyUID
		}, npShapeDenied},
		{"no managed-by label", func(np *networkingv1.NetworkPolicy) { np.Labels = nil }, npShapeDenied},
	} {
		refusedAs(t, pc, mutateNP(toolIngress(slug(tc.name)), tc.mutate), tc.want, "tool ingress: "+tc.name)
	}
	refusedAs(t, pc, mutateNP(componentPolicy("api"), func(np *networkingv1.NetworkPolicy) {
		np.Name = "preview-battles-np-api-to-tools"
		np.Spec.Egress = []networkingv1.NetworkPolicyEgressRule{{
			To:    []networkingv1.NetworkPolicyPeer{{PodSelector: ptrSelector(partSelector("tools"))}},
			Ports: []networkingv1.NetworkPolicyPort{tcpPort(8090)}}}
	}), toolReachDenied, "a component's egress to the tool pod")
	refusedAs(t, pc, mutateNP(componentPolicy("tools"), func(np *networkingv1.NetworkPolicy) {
		np.Spec.Ingress[0].From[0].PodSelector = ptrSelector(partSelector("api"))
	}), toolReachDenied, "the tool pod's edge policy admitting a component")
	refusedAs(t, pc, mutateNP(edgePolicy("x"), func(np *networkingv1.NetworkPolicy) {
		np.Name = "preview-tools-isolation"
	}), npIsolation, "creating preview-tools-isolation")

	iso := &networkingv1.NetworkPolicy{}
	key := client.ObjectKey{Namespace: "maul-preview-0", Name: "preview-tools-isolation"}
	if err := admin.Get(ctx, key, iso); err != nil {
		t.Fatalf("get preview-tools-isolation: %v", err)
	}
	if err := pc.Delete(ctx, iso, client.DryRunAll); !deniedWith(err, npIsolation) {
		t.Errorf("preview-controller deleting preview-tools-isolation: %v, want %q", err, npIsolation)
	}
}

// checkToolObjects: preview-controller creates, updates and deletes its own
// preview- Secrets and ConfigMaps in a slot and nothing else there, and in
// the release namespace only a PreviewVerification's own ledger and
// evidence ConfigMaps.
func checkToolObjects(t *testing.T, admin, pc client.Client) {
	ctx := t.Context()
	admittedAs(t, pc, slotSecret(toolKeySecret))
	admittedAs(t, pc, slotConfigMap(toolConfigMap))
	refusedAs(t, pc, slotSecret("maul-preview-oidc-g9"), toolObjectsDenied, "a sign-in Secret")
	unlabelled := slotSecret(toolKeySecret + "-2")
	unlabelled.Labels = nil
	refusedAs(t, pc, unlabelled, toolObjectsDenied, "an unlabelled Secret")
	refusedAs(t, pc, slotConfigMap("kube-root-ca-2"), toolObjectsDenied, "a ConfigMap not named preview-")

	// Others' objects: preview-controller may neither delete nor take over
	// one, whatever it would make it.
	oidc := slotSecret("maul-preview-oidc-g1")
	oidc.Labels = nil
	if err := admin.Create(ctx, oidc); err != nil {
		t.Fatalf("create the sign-in Secret: %v", err)
	}
	if err := pc.Delete(ctx, oidc.DeepCopy(), client.DryRunAll); !deniedWith(err, toolObjectsDenied) {
		t.Errorf("preview-controller deleting a sign-in Secret: %v, want %q", err, toolObjectsDenied)
	}
	own := slotConfigMap(toolConfigMap)
	if err := pc.Create(ctx, own); err != nil {
		t.Fatalf("create the tool ConfigMap: %v", err)
	}
	own.Data = map[string]string{"outcomes": "[]"}
	if err := pc.Update(ctx, own, client.DryRunAll); err != nil {
		t.Errorf("preview-controller updating its tool ConfigMap: %v", err)
	}
	if err := pc.Delete(ctx, own, client.DryRunAll); err != nil {
		t.Errorf("preview-controller deleting its tool ConfigMap: %v", err)
	}

	admittedAs(t, pc, ledger(toolVerification+"-ledger", toolVerification))
	admittedAs(t, pc, ledger(toolVerification+"-evidence", toolVerification))
	for name, cm := range map[string]*corev1.ConfigMap{
		"no owner":                 ledger(toolVerification+"-ledger", ""),
		"another verification's":   ledger(toolVerification+"-ledger", "battles-1-v2-a1"),
		"a transcript":             ledger("finding-1-transcript", toolVerification),
		"an Intent-owned record":   intentOwned(ledger(toolVerification+"-ledger", toolVerification)),
		"a non-controlling ledger": nonController(ledger(toolVerification+"-ledger", toolVerification)),
	} {
		refusedAs(t, pc, cm, toolLedgersDenied, "release ConfigMap: "+name)
	}
	transcript := ledger("finding-1-transcript", "")
	if err := admin.Create(ctx, transcript); err != nil {
		t.Fatalf("create a transcript: %v", err)
	}
	if err := pc.Delete(ctx, transcript, client.DryRunAll); !deniedWith(err, toolLedgersDenied) {
		t.Errorf("preview-controller deleting a transcript: %v, want %q", err, toolLedgersDenied)
	}
}

// checkEvaluatorProbeObjects: what the isolation probe's evaluator run
// creates (run.sh evaluator --print-manifests) is admitted: its component
// Deployment in a slot as the operator, its evaluator Job in the agents
// namespace as intent-controller under the maul-evaluator-labels fence.
func checkEvaluatorProbeObjects(t *testing.T, admin, ic client.Client) {
	image := previewRegistry + "/maul/previews/netprobe:sha-" + previewSHA
	out, err := exec.CommandContext(t.Context(), "bash", filepath.Join(probeDir, "run.sh"), "evaluator",
		"--print-manifests", "--image", image,
		"--node-selector", "karpenter.sh/nodepool=maul-preview,eks.amazonaws.com/nodeclass=maul-preview",
		"--taint-key", previewTaintKey).Output()
	if err != nil {
		t.Fatalf("run.sh evaluator --print-manifests: %v", err)
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
		c := admin
		if u.GetKind() == "Job" {
			c = ic
		}
		if err := c.Create(t.Context(), u, client.DryRunAll); err != nil {
			t.Errorf("run.sh evaluator's %s %s/%s is refused: %v", u.GetKind(), u.GetNamespace(), u.GetName(), err)
		}
	}
	if kinds["Deployment"] != 1 || kinds["Job"] != 1 {
		t.Errorf("run.sh evaluator printed %v, want one Deployment and one Job", kinds)
	}
}

// toolJob is the tool Job as S1-13 has preview-controller render it (Q4,
// SP-2): tools the main container, browser and sandbox native sidecars, the
// key Secret in tools and browser, the tool ConfigMap in tools, emptyDirs
// for the browser's shared memory and the sandbox socket, labelled with the
// tools role and the verification on the Job and its template alike.
func toolJob(name string) *batchv1.Job {
	j := testerJob(name)
	j.Name = "preview-battles-tl-" + name
	spec := &j.Spec.Template.Spec
	spec.Containers[0].Name, spec.Containers[0].Image = "tools", toolsImage
	spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "config", MountPath: "/etc/maul-tools"},
		{Name: "key", MountPath: "/etc/maul-key"}, {Name: "sock", MountPath: "/run/maul-sandbox"}}
	always := corev1.ContainerRestartPolicyAlways
	sidecar := func(name, image string, mounts ...corev1.VolumeMount) corev1.Container {
		c := spec.Containers[0]
		c.Name, c.Image, c.RestartPolicy, c.VolumeMounts = name, image, &always, mounts
		return c
	}
	spec.InitContainers = []corev1.Container{
		sidecar("browser", browserImage, corev1.VolumeMount{Name: "key", MountPath: "/etc/maul-key"},
			corev1.VolumeMount{Name: "shm", MountPath: "/dev/shm"}),
		sidecar("sandbox", toolboxImage, corev1.VolumeMount{Name: "sock", MountPath: "/run/maul-sandbox"}),
	}
	spec.Volumes = []corev1.Volume{
		{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: toolConfigMap}}}},
		{Name: "key", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: toolKeySecret}}},
		{Name: "shm", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
			Medium: corev1.StorageMediumMemory}}},
		{Name: "sock", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}
	tools := map[string]string{evPreviewRole: "tools", evEvaluation: evUID}
	j.Labels = copyLabels(tools)
	for k, v := range tools {
		j.Spec.Template.Labels[k] = v
	}
	j.Spec.Template.Labels["maul.run/preview-part"] = "tools"
	return j
}

// toolPodNamed is the tool Job's pod as the Job controller creates it.
func toolPodNamed(name string) *corev1.Pod {
	j := toolJob(name)
	yes := true
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "maul-preview-0",
		Labels: j.Spec.Template.Labels, OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1",
			Kind: "Job", Name: j.Name, UID: "6c1f1b8e-0000-4000-8000-000000000001", Controller: &yes}}},
		Spec: j.Spec.Template.Spec}
}

// toolIngress is the tool pod's per-verification ingress policy (DESIGN
// 4.2): the tool pod by role and verification, from exactly the evaluator
// of the same verification in the agents namespace, on TCP 8090.
func toolIngress(name string) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "preview-battles-np-tools-" + name, Namespace: "maul-preview-0",
			Labels: map[string]string{"maul.run/preview": "battles", "maul.run/preview-uid": verifyUID,
				toolManagedByLabel: "maul-preview-controller"}},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{evPreviewRole: "tools", evEvaluation: evUID}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
						"kubernetes.io/metadata.name": egressAgents}},
					PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
						evRole: "evaluator", evEvaluation: evUID}},
				}},
				Ports: []networkingv1.NetworkPolicyPort{tcpPort(8090)},
			}},
		},
	}
}

// slotSecret is a Secret in the first slot as preview-controller labels its
// own.
func slotSecret(name string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "maul-preview-0",
		Labels: map[string]string{toolManagedByLabel: "maul-preview-controller"}},
		Data: map[string][]byte{"key": []byte("k")}}
}

// slotConfigMap is a ConfigMap in the first slot as preview-controller
// labels its own.
func slotConfigMap(name string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "maul-preview-0",
		Labels: map[string]string{toolManagedByLabel: "maul-preview-controller"}}}
}

// ledger is a release-namespace ConfigMap controlled by the
// PreviewVerification named owner ("" for none).
func ledger(name, owner string) *corev1.ConfigMap {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: egressRelease}}
	if owner != "" {
		yes := true
		cm.OwnerReferences = []metav1.OwnerReference{{APIVersion: "maul.run/v1alpha1", Kind: "PreviewVerification",
			Name: owner, UID: "6c1f1b8e-0000-4000-8000-000000000002", Controller: &yes}}
	}
	return cm
}

func intentOwned(cm *corev1.ConfigMap) *corev1.ConfigMap {
	cm.OwnerReferences[0].Kind = "Intent"
	return cm
}

func nonController(cm *corev1.ConfigMap) *corev1.ConfigMap {
	cm.OwnerReferences[0].Controller = nil
	return cm
}
