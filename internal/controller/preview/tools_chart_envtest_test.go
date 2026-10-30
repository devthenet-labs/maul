// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/devthenet-labs/maul/internal/kube"
	"github.com/devthenet-labs/maul/internal/rbactest"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// The identities the chart's slot policies judge (SPEC W3, S1-24): the
// chart's preview-controller ServiceAccount, and kube-controller-manager's
// Job controller, which creates the tool Job's pod.
const (
	chartPreviewSA  = "system:serviceaccount:maul:maul-preview-controller"
	chartJobControl = "system:serviceaccount:kube-system:job-controller"
)

// chartToolValues render the chart with the evaluator's tool pod: the
// fixtures the chart's own render tests use, verification, the evaluator
// and its three images, and an evaluation round shorter than the slot Job
// deadline.
var chartToolValues = []string{
	"-f", "../../../hack/testdata/chart-render/preview-foundation.yaml",
	"-f", "../../../hack/testdata/chart-render/intent-controller.yaml",
	"-f", "../../../hack/testdata/chart-render/preview-controller.yaml",
	"--set", "preview.verification.enabled=true",
	"--set", "preview.verification.testerImage=ghcr.io/devthenet-labs/maul/preview-tester@sha256:" +
		strings.Repeat("4", 64),
	"--set", "intentController.verification.evaluator=true",
	"--set", "preview.verification.tools.image=" + testToolsImage,
	"--set", "preview.verification.tools.browserImage=" + testBrowserImage,
	"--set", "preview.verification.tools.toolboxImage=" + testToolboxImage,
	"--set", "intentController.verification.evaluateTimeout=5m",
}

// TestRenderedToolObjectsAdmittedByTheChart (SPEC W3): every object
// preview-controller itself renders for an outcome verification, as
// toolJob, toolKeySecret, toolConfigMap, evaluatorPolicy, networkPolicies,
// ledgerConfigMap and evidenceConfigMap render them, under Settings read
// from the chart's own preview-controller ConfigMap, is admitted by the
// chart's real admission policies, quota and LimitRange on a real API
// server, as the identity that writes it: the tool Job, its pod as the Job
// controller creates it, the key Secret, the tool ConfigMap and its capture
// refresh, the per-verification ingress policy, the edge policies, the
// ledger and evidence ConfigMaps, and the teardown's deletes. The
// chart-side suites build their objects by hand; this one would have caught
// a render that drifted from the policies (the tool pod's part, W3).
func TestRenderedToolObjectsAdmittedByTheChart(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via mise run envtest")
	}
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	as, cm := startChartTools(t)
	ctx := t.Context()
	s, deadlineMax := chartSettings(t, cm)
	if err := s.Validate(); err != nil {
		t.Fatalf("preview-controller refuses the chart's settings: %v", err)
	}
	pc, jc := as(chartPreviewSA), as(chartJobControl)

	_, p := verifyPreview(t, time.Now())
	pv := toolVerification(t, p)
	pv.Spec.Timeout = metav1.Duration{Duration: 5 * time.Minute}
	job, err := s.toolJob(p, pv, 0, deadlineMax)
	if err != nil {
		t.Fatal(err)
	}
	// The policies are in force once the same Job with a sidecar in the
	// wrong image is refused; only then does an admission mean anything.
	waitRefused(t, func() error {
		bad := job.DeepCopy()
		bad.Spec.Template.Spec.InitContainers[0].Image = s.ToolsImage
		return pc.Create(ctx, bad, client.DryRunAll)
	}, "preview tool pods run tools with at most a browser and a sandbox native sidecar")

	admit := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Errorf("the chart refuses preview-controller's rendered %s: %v", what, err)
		}
	}
	// The slot objects, in createTools' order: policy, Job, Secret,
	// ConfigMap.
	admit("tool ingress policy", pc.Create(ctx, s.evaluatorPolicy(p, pv, 0)))
	for _, np := range s.networkPolicies(p, 0) {
		admit("edge policy "+np.Name, pc.Create(ctx, np))
	}
	if err := pc.Create(ctx, job); err != nil {
		t.Fatalf("the chart refuses preview-controller's rendered tool Job: %v", err)
	}
	admit("tool pod", jc.Create(ctx, podOfJob(job), client.DryRunAll))
	admit("tool key Secret", pc.Create(ctx, s.toolKeySecret(p, pv, job, newCDPKey())))
	targets := map[string]string{}
	for i, c := range p.Spec.Components {
		targets[c.Name] = "10.96.0." + strconv.Itoa(10+i) + ":80"
	}
	cfg, err := s.toolConfig(p, pv, targets, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	data, err := toolConfigData(cfg, pv.Spec.Outcomes)
	if err != nil {
		t.Fatal(err)
	}
	cmObj := s.toolConfigMap(p, pv, job, data)
	admit("tool ConfigMap", pc.Create(ctx, cmObj))
	// A capture refresh is a whole update of the same ConfigMap.
	cmObj.Data[toolwire.CaptureLogKey(p.Spec.Components[0].Name)] = "listening on :8080\n"
	cmObj.Data[toolwire.CaptureEventsKey] = "[]"
	admit("tool ConfigMap capture refresh", pc.Update(ctx, cmObj))

	// The release namespace: the ledger and its evidence.
	shot := testPNG(t, 8, 8)
	log, msg := toolLedger(t, shot) // the ledger tests' full round
	out := verifiedLedger(t, log, msg)
	ledger, err := ledgerConfigMap(pv, out.ledger)
	if err != nil {
		t.Fatal(err)
	}
	admit("ledger ConfigMap", pc.Create(ctx, ledger))
	images, _ := keepEvidence(out.ledger.Entries, out.evidence)
	if ev := evidenceConfigMap(pv, images); ev == nil {
		t.Error("no evidence ConfigMap rendered for a cited screenshot")
	} else {
		admit("evidence ConfigMap", pc.Create(ctx, ev))
	}

	// The teardown deletes what it put in the slot beside the Job.
	for _, obj := range []client.Object{
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: toolName(p, pv, toolConfigInfix), Namespace: job.Namespace}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: toolName(p, pv, toolKeyInfix), Namespace: job.Namespace}},
		s.evaluatorPolicy(p, pv, 0),
	} {
		admit("teardown delete of "+obj.GetName(), pc.Delete(ctx, obj))
	}
	admit("tool Job delete", pc.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)))
}

// startChartTools starts an API server holding the chart rendered with the
// tool pod (its namespaces, CRDs, Roles, ClusterRoles and their bindings,
// slot quota, LimitRanges and isolation policies, and every admission
// policy and binding), waits for preview-controller's and the Job
// controller's grants, and returns a client per identity and the rendered
// preview-controller ConfigMap's data.
func startChartTools(t *testing.T) (func(string) client.Client, map[string]string) {
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
		t.Fatal(err)
	}
	ctx := t.Context()
	args := append([]string{"template", "maul", "../../../charts/maul", "--namespace", "maul"}, chartToolValues...)
	rendered, err := exec.CommandContext(ctx, "helm", args...).Output()
	if err != nil {
		t.Fatalf("render chart: %v", err)
	}
	if err := admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "maul"}}); err != nil {
		t.Fatal(err)
	}
	var cm map[string]string
	order := map[string]int{"Namespace": 0, "CustomResourceDefinition": 1}
	groups := make([][]*unstructured.Unstructured, 3)
	dec := yaml.NewYAMLOrJSONDecoder(strings.NewReader(string(rendered)), 4096)
	for {
		u := &unstructured.Unstructured{}
		if err := dec.Decode(u); err != nil {
			break
		}
		switch u.GetKind() {
		case "ConfigMap":
			if u.GetName() == "maul-preview-controller-config" {
				cm, _, _ = unstructured.NestedStringMap(u.Object, "data")
			}
			continue
		case "Namespace", "CustomResourceDefinition", "Role", "RoleBinding", "ClusterRole", "ClusterRoleBinding",
			"ResourceQuota", "LimitRange", "NetworkPolicy", "ValidatingAdmissionPolicy",
			"ValidatingAdmissionPolicyBinding":
		default:
			continue
		}
		g, ok := order[u.GetKind()]
		if !ok {
			g = 2
		}
		groups[g] = append(groups[g], u)
	}
	for _, group := range groups {
		for _, u := range group {
			if err := admin.Create(ctx, u); err != nil {
				t.Fatalf("install %s %s/%s: %v", u.GetKind(), u.GetNamespace(), u.GetName(), err)
			}
		}
	}
	if cm == nil {
		t.Fatal("the chart rendered no preview-controller ConfigMap")
	}
	slot := func(group, resource string) authorizationv1.ResourceAttributes {
		return authorizationv1.ResourceAttributes{Namespace: "maul-preview-0", Group: group, Resource: resource}
	}
	reviews := rbactest.Verbs(chartPreviewSA, nil, slot("batch", "jobs"), "create", "delete")
	reviews = append(reviews, rbactest.Verbs(chartPreviewSA, nil, slot("", "secrets"), "create", "delete")...)
	reviews = append(reviews, rbactest.Verbs(chartPreviewSA, nil, slot("", "configmaps"), "create", "update",
		"delete")...)
	reviews = append(reviews, rbactest.Verbs(chartPreviewSA, nil, slot("networking.k8s.io", "networkpolicies"),
		"create", "delete")...)
	reviews = append(reviews, rbactest.Verbs(chartPreviewSA, nil, authorizationv1.ResourceAttributes{
		Namespace: "maul", Resource: "configmaps"}, "create")...)
	reviews = append(reviews, rbactest.Verbs(chartJobControl, nil, slot("", "pods"), "create")...)
	rbactest.WaitAllowed(t, admin, reviews...)
	as := func(user string) client.Client {
		c := rest.CopyConfig(cfg)
		c.Impersonate = rest.ImpersonationConfig{UserName: user}
		cl, err := client.New(c, client.Options{Scheme: kube.Scheme()})
		if err != nil {
			t.Fatalf("client as %s: %v", user, err)
		}
		return cl
	}
	return as, cm
}

// chartSettings builds preview-controller's Settings from the chart's
// ConfigMap, as cmd/preview-controller does from the flags its keys bind,
// with the slot Job deadline bound it renders.
func chartSettings(t *testing.T, cm map[string]string) (Settings, time.Duration) {
	t.Helper()
	atoi := func(k string) int {
		n, err := strconv.Atoi(cm[k])
		if err != nil {
			t.Fatalf("%s: %v", k, err)
		}
		return n
	}
	dur := func(k string) time.Duration {
		d, err := time.ParseDuration(cm[k])
		if err != nil {
			t.Fatalf("%s: %v", k, err)
		}
		return d
	}
	s := Settings{
		Namespace: "maul", SlotCount: atoi("MAUL_PREVIEW_SLOT_COUNT"),
		ImagePrefix: cm["MAUL_PREVIEW_IMAGE_PREFIX"], HostSuffix: cm["MAUL_PREVIEW_HOST_SUFFIX"],
		NodePool: cm["MAUL_PREVIEW_NODE_POOL"], NodeClass: cm["MAUL_PREVIEW_NODE_CLASS"],
		TaintKey: cm["MAUL_PREVIEW_TAINT_KEY"], NodeIsolation: cm["MAUL_PREVIEW_NODE_ISOLATION"],
		RolloutTimeout: dur("MAUL_PREVIEW_ROLLOUT_TIMEOUT"), PollInterval: dur("MAUL_PREVIEW_POLL_INTERVAL"),
		MaxRetries:  int32(atoi("MAUL_PREVIEW_MAX_RETRIES")),
		TesterImage: cm["MAUL_PREVIEW_TESTER_IMAGE"], Evaluator: cm["MAUL_PREVIEW_EVALUATOR"] == "true",
		ToolsImage: cm["MAUL_PREVIEW_TOOLS_IMAGE"], BrowserImage: cm["MAUL_PREVIEW_BROWSER_IMAGE"],
		ToolboxImage: cm["MAUL_PREVIEW_TOOLBOX_IMAGE"], AgentsNamespace: cm["MAUL_PREVIEW_AGENTS_NAMESPACE"],
	}
	if s.NodeIsolation == "" {
		s.NodeIsolation = NodeIsolationRequired
	}
	selector, err := ParseNodeSelector(cm["MAUL_PREVIEW_NODE_SELECTOR"])
	if err != nil {
		t.Fatal(err)
	}
	s.NodeSelector = selector
	return s, dur("MAUL_PREVIEW_VERIFY_DEADLINE_MAX")
}

// podOfJob is the pod the Job controller creates for job: its template,
// controlled by the Job.
func podOfJob(job *batchv1.Job) *corev1.Pod {
	labels := map[string]string{"batch.kubernetes.io/job-name": job.Name,
		"batch.kubernetes.io/controller-uid": string(job.UID)}
	for k, v := range job.Spec.Template.Labels {
		labels[k] = v
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: job.Name + "-x7k2p", Namespace: job.Namespace, Labels: labels,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(job,
				batchv1.SchemeGroupVersion.WithKind("Job"))}},
		Spec: *job.Spec.Template.Spec.DeepCopy(),
	}
}

// waitRefused retries create until it is refused with a message containing
// want: the API server enforces a new admission policy a moment after it
// is stored.
func waitRefused(t *testing.T, create func() error, want string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		err := create()
		if err != nil && strings.Contains(err.Error(), want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the chart's policies never refused a probe with %q: last %v", want,
				errors.Join(err, errors.New("admitted")))
		}
		time.Sleep(200 * time.Millisecond)
	}
}
