// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/acceptance/crdconv"
	"github.com/devthenet-labs/maul/internal/checks"
	"github.com/devthenet-labs/maul/internal/previewtools"
)

const (
	testToolsImage = "ghcr.io/devthenet-labs/maul-preview-tools@sha256:" +
		"1111111111111111111111111111111111111111111111111111111111111111"
	testBrowserImage = "ghcr.io/devthenet-labs/maul-preview-browser@sha256:" +
		"2222222222222222222222222222222222222222222222222222222222222222"
	testToolboxImage = "ghcr.io/devthenet-labs/maul-preview-toolbox@sha256:" +
		"3333333333333333333333333333333333333333333333333333333333333333"
	testCheckBody = `{"id":"items.body","outcome":"items","form":"http","http":{"target":"api","steps":[` +
		`{"request":{"method":"POST","path":"/api/echo","json":{"n":"{{run}}"}},"expect":{"bodyContains":"{{run}}"}}]}}`
)

// evalSettings are verifySettings with the evaluator on.
func evalSettings() Settings {
	s := verifySettings()
	s.Evaluator = true
	s.ToolsImage, s.BrowserImage, s.ToolboxImage = testToolsImage, testBrowserImage, testToolboxImage
	s.AgentsNamespace = "maul-agents"
	return s
}

// testOutcomes are an approved plan's outcomes, a manual one included.
func testOutcomes() []v1alpha1.AcceptanceOutcome {
	return []v1alpha1.AcceptanceOutcome{
		{ID: "items", Outcome: "the items list shows new items", Kind: v1alpha1.OutcomeKindChange},
		{ID: "feel", Outcome: "it feels right", Kind: v1alpha1.OutcomeKindChange, Manual: true, Reason: "taste"},
	}
}

// outcomesDigest is acceptance.OutcomesDigest of stored outcomes.
func outcomesDigest(t *testing.T, outcomes []v1alpha1.AcceptanceOutcome) string {
	t.Helper()
	plan, ok := planOutcomes(outcomes)
	if !ok {
		t.Fatal("outcomes do not convert")
	}
	if _, err := crdconv.OutcomesToCRD(plan); err != nil {
		t.Fatal(err)
	}
	return acceptance.OutcomesDigest(plan)
}

// savedCheck is a saved check of body.
func savedCheck(t *testing.T, body string) v1alpha1.SavedCheck {
	t.Helper()
	c, err := checks.Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return v1alpha1.SavedCheck{ID: c.ID, Outcome: c.Outcome, Form: v1alpha1.CheckForm(c.Form),
		Body: string(checks.Canonical(c)), Digest: checks.Digest(c)}
}

// toolVerification is testVerification as an outcome verification with
// both sidecars and one saved check.
func toolVerification(t *testing.T, p *v1alpha1.Preview) *v1alpha1.PreviewVerification {
	t.Helper()
	pv := testVerification(p)
	pv.Spec.Outcomes = testOutcomes()
	pv.Spec.OutcomesDigest = outcomesDigest(t, pv.Spec.Outcomes)
	pv.Spec.Checks = []v1alpha1.SavedCheck{savedCheck(t, testCheckBody)}
	pv.Spec.ChecksDigest = "sha256:" + strings.Repeat("c", 64)
	pv.Spec.Rung = v1alpha1.RungFull
	pv.Spec.Tools = &v1alpha1.ToolsSpec{Browser: true, Sandbox: true, MaxToolCalls: 120}
	pv.Spec.Timeout = metav1.Duration{Duration: 20 * time.Minute}
	return pv
}

func TestToolDeadlines(t *testing.T) {
	for _, tc := range []struct {
		timeout, max, job, pod, wall time.Duration
	}{
		{20 * time.Minute, 30 * time.Minute, 26 * time.Minute, 23 * time.Minute, 20 * time.Minute},
		{20 * time.Minute, 22 * time.Minute, 22 * time.Minute, 19 * time.Minute, 16 * time.Minute},
		{time.Hour, time.Hour, time.Hour, 57 * time.Minute, 54 * time.Minute},
		{time.Minute, time.Minute, time.Minute, 45 * time.Second, 30 * time.Second},
	} {
		job, pod, wall := toolDeadlines(tc.timeout, tc.max)
		if job != tc.job || pod != tc.pod || wall != tc.wall {
			t.Errorf("toolDeadlines(%s, %s) = %s %s %s, want %s %s %s", tc.timeout, tc.max, job, pod, wall,
				tc.job, tc.pod, tc.wall)
		}
	}
	// Every timeout and bound the schema and the chart admit keep the
	// order the ledger's survival rests on: wall < pod < job <= max.
	for timeout := time.Minute; timeout <= time.Hour; timeout += 7 * time.Minute {
		for max := time.Minute; max <= time.Hour; max += 5 * time.Minute {
			job, pod, wall := toolDeadlines(timeout, max)
			if wall <= 0 || wall > timeout || wall >= pod || pod >= job || job > max {
				t.Errorf("toolDeadlines(%s, %s) = %s %s %s out of order", timeout, max, job, pod, wall)
			}
		}
	}
}

func TestEvaluatorSettings(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*Settings)
		ok   bool
	}{
		{"on", func(*Settings) {}, true},
		{"off needs nothing", func(s *Settings) {
			s.Evaluator, s.ToolsImage, s.BrowserImage, s.ToolboxImage, s.AgentsNamespace = false, "", "", "", ""
		}, true},
		{"no verification", func(s *Settings) { s.TesterImage = "" }, false},
		{"no browser image", func(s *Settings) { s.BrowserImage = "" }, false},
		{"tag only", func(s *Settings) { s.ToolsImage = "ghcr.io/devthenet-labs/maul-preview-tools:v1" }, false},
		{"under the prefix", func(s *Settings) {
			s.ToolboxImage = s.ImagePrefix + "toolbox@sha256:" + strings.Repeat("3", 64)
		}, false},
		{"no agents namespace", func(s *Settings) { s.AgentsNamespace = "" }, false},
		{"negative max", func(s *Settings) { s.MaxEvaluations = -1 }, false},
	} {
		s := evalSettings()
		tc.mut(&s)
		if err := s.Validate(); (err == nil) != tc.ok {
			t.Errorf("%s: Validate = %v, want ok %v", tc.name, err, tc.ok)
		}
	}
}

func TestToolJob(t *testing.T) {
	_, p := verifyPreview(t, time.Now())
	s := evalSettings()
	pv := toolVerification(t, p)
	job, err := s.toolJob(p, pv, 1, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range slices.Concat(toolJobChecks(s, p, pv, job), toolContainerChecks(job)) {
		if !check.ok {
			t.Errorf("tool Job %s wrong", check.name)
		}
	}
	checkToolPosture(t, job)
	checkToolVolumes(t, p, pv, job)
}

// toolJobChecks: the Job's name, run-once shape, deadlines, pod posture,
// placement and labels.
func toolJobChecks(s Settings, p *v1alpha1.Preview, pv *v1alpha1.PreviewVerification,
	job *batchv1.Job) []namedCheck {
	spec := job.Spec.Template.Spec
	l, jl := job.Spec.Template.Labels, job.Labels
	return []namedCheck{
		{"named as a tester Job", job.Name == testerJobName(p, pv) && job.Namespace == "maul-preview-1"},
		{"run once", *job.Spec.BackoffLimit == 0 && *job.Spec.Parallelism == 1 && *job.Spec.Completions == 1 &&
			job.Spec.TTLSecondsAfterFinished == nil},
		{"pod deadline under the Job's", *spec.ActiveDeadlineSeconds == 23*60 &&
			*job.Spec.ActiveDeadlineSeconds == 26*60},
		{"short grace", *spec.TerminationGracePeriodSeconds == toolsGrace},
		{"no token", spec.ServiceAccountName == "default" && !*spec.AutomountServiceAccountToken},
		{"no service links", !*spec.EnableServiceLinks},
		{"placement", spec.NodeSelector["karpenter.sh/nodepool"] == s.NodePool && len(spec.Tolerations) == 1},
		{"no host namespaces", !spec.HostNetwork && !spec.HostPID && !spec.HostIPC && len(spec.HostAliases) == 0},
		{"pod labels", l[labelRole] == v1alpha1.PreviewRoleTools && l[labelPart] == v1alpha1.PreviewPartTester &&
			l[v1alpha1.LabelPreviewVerification] == string(pv.UID) && l[labelVerificationUID] == string(pv.UID) &&
			l[labelPreviewUID] == string(p.UID)},
		{"Job labels as its pod's, without the part", jl[labelRole] == l[labelRole] &&
			jl[v1alpha1.LabelPreviewVerification] == l[v1alpha1.LabelPreviewVerification] && jl[labelPart] == "" &&
			ownedBy(job, p)},
	}
}

// toolContainerChecks: the three containers, their images and what each
// one mounts.
func toolContainerChecks(job *batchv1.Job) []namedCheck {
	spec := job.Spec.Template.Spec
	mounts := map[string][]string{}
	byName := map[string]corev1.Container{}
	for _, c := range slices.Concat(spec.InitContainers, spec.Containers) {
		byName[c.Name] = c
		for _, m := range c.VolumeMounts {
			mounts[c.Name] = append(mounts[c.Name], m.Name)
		}
	}
	only := func(volume string, containers ...string) bool {
		for _, name := range []string{toolsContainer, browserContainer, sandboxContainer} {
			if slices.Contains(mounts[name], volume) != slices.Contains(containers, name) {
				return false
			}
		}
		return true
	}
	tools, browser, sandbox := byName[toolsContainer], byName[browserContainer], byName[sandboxContainer]
	return []namedCheck{
		{"tools is the main container", len(spec.Containers) == 1 && spec.Containers[0].Name == toolsContainer &&
			tools.Image == testToolsImage},
		{"native sidecars", len(spec.InitContainers) == 2 && spec.InitContainers[0].Name == browserContainer &&
			spec.InitContainers[1].Name == sandboxContainer && browser.Image == testBrowserImage &&
			sandbox.Image == testToolboxImage &&
			*browser.RestartPolicy == corev1.ContainerRestartPolicyAlways &&
			*sandbox.RestartPolicy == corev1.ContainerRestartPolicyAlways},
		{"key only in tools and browser", only("cdp-key", toolsContainer, browserContainer)},
		{"config only in tools", only("tool-config", toolsContainer)},
		{"socket shared by tools and sandbox only", only("exec-socket", toolsContainer, sandboxContainer)},
		{"readiness on the relay", tools.ReadinessProbe.HTTPGet.Path == previewtools.ReadyPath &&
			tools.ReadinessProbe.HTTPGet.Port.IntVal == previewtools.Port},
		{"termination message", tools.TerminationMessagePolicy == corev1.TerminationMessageReadFile},
	}
}

// checkToolPosture holds every container to the restricted posture, each
// under a UID of its own.
func checkToolPosture(t *testing.T, job *batchv1.Job) {
	t.Helper()
	spec := job.Spec.Template.Spec
	uids := map[int64]bool{}
	for _, c := range slices.Concat(spec.InitContainers, spec.Containers) {
		sc := c.SecurityContext
		if sc == nil || *sc.AllowPrivilegeEscalation || !*sc.ReadOnlyRootFilesystem || !*sc.RunAsNonRoot ||
			len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" || sc.RunAsUser == nil {
			t.Errorf("container %s posture %+v", c.Name, sc)
			continue
		}
		uids[*sc.RunAsUser] = true
	}
	if len(uids) != 3 {
		t.Errorf("containers share UIDs: %v", uids)
	}
}

// checkToolVolumes: bounded emptyDirs, the one ConfigMap and the one
// Secret of this verification, and nothing else.
func checkToolVolumes(t *testing.T, p *v1alpha1.Preview, pv *v1alpha1.PreviewVerification, job *batchv1.Job) {
	t.Helper()
	for _, v := range job.Spec.Template.Spec.Volumes {
		switch {
		case v.EmptyDir != nil:
			if v.EmptyDir.SizeLimit == nil {
				t.Errorf("volume %s unbounded", v.Name)
			}
		case v.ConfigMap != nil:
			if v.ConfigMap.Name != toolName(p, pv, toolConfigInfix) {
				t.Errorf("config volume %s", v.ConfigMap.Name)
			}
		case v.Secret != nil:
			if v.Secret.SecretName != toolName(p, pv, toolKeyInfix) {
				t.Errorf("key volume %s", v.Secret.SecretName)
			}
		default:
			t.Errorf("volume %s of another kind", v.Name)
		}
	}
}

// Without a browser or a sandbox the pod runs neither sidecar, and holds
// no key at all.
func TestToolJobWithoutSidecars(t *testing.T) {
	_, p := verifyPreview(t, time.Now())
	pv := toolVerification(t, p)
	pv.Spec.Tools = &v1alpha1.ToolsSpec{MaxToolCalls: 50}
	job, err := evalSettings().toolJob(p, pv, 0, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	spec := job.Spec.Template.Spec
	if len(spec.InitContainers) != 0 || len(spec.Containers) != 1 {
		t.Errorf("containers %d+%d", len(spec.InitContainers), len(spec.Containers))
	}
	for _, v := range spec.Volumes {
		if v.Secret != nil || v.Name == "exec-socket" {
			t.Errorf("volume %s rendered without its sidecar", v.Name)
		}
	}
}

func TestToolJobRefusals(t *testing.T) {
	_, p := verifyPreview(t, time.Now())
	for _, tc := range []struct {
		name string
		mut  func(*Settings, *v1alpha1.PreviewVerification)
	}{
		{"evaluator off", func(s *Settings, _ *v1alpha1.PreviewVerification) { s.Evaluator = false }},
		{"criteria verification", func(_ *Settings, pv *v1alpha1.PreviewVerification) { pv.Spec.Outcomes = nil }},
		{"no tools", func(_ *Settings, pv *v1alpha1.PreviewVerification) { pv.Spec.Tools = nil }},
		{"another preview", func(_ *Settings, pv *v1alpha1.PreviewVerification) { pv.Spec.PreviewRef.UID = "x" }},
		{"timeout", func(_ *Settings, pv *v1alpha1.PreviewVerification) {
			pv.Spec.Timeout = metav1.Duration{Duration: 2 * time.Hour}
		}},
	} {
		s, pv := evalSettings(), toolVerification(t, p)
		tc.mut(&s, pv)
		if _, err := s.toolJob(p, pv, 0, 30*time.Minute); err == nil {
			t.Errorf("%s: rendered", tc.name)
		}
	}
}

// The tool pod fits the slot as SP-2 sized it: no container over the
// LimitRange's per-container maximum, and the whole pod within what the
// quota keeps beside a full application.
func TestToolPodFitsTheSlot(t *testing.T) {
	_, p := verifyPreview(t, time.Now())
	job, err := evalSettings().toolJob(p, toolVerification(t, p), 0, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	maxCPU, maxMem := resource.MustParse("1500m"), resource.MustParse("1536Mi")
	podCPU, podMem := resource.MustParse("2500m"), resource.MustParse("2Gi")
	var cpu, mem resource.Quantity
	spec := job.Spec.Template.Spec
	for _, c := range slices.Concat(spec.InitContainers, spec.Containers) {
		req, lim := c.Resources.Requests, c.Resources.Limits
		if req.Cpu().Cmp(*lim.Cpu()) != 0 || req.Memory().Cmp(*lim.Memory()) != 0 {
			t.Errorf("%s requests %v are not its limits %v", c.Name, req, lim)
		}
		if lim.Cpu().Cmp(maxCPU) > 0 || lim.Memory().Cmp(maxMem) > 0 {
			t.Errorf("%s limits %v over the LimitRange maximum", c.Name, lim)
		}
		cpu.Add(*lim.Cpu())
		mem.Add(*lim.Memory())
	}
	if cpu.Cmp(podCPU) > 0 || mem.Cmp(podMem) > 0 {
		t.Errorf("tool pod %s / %s over the quota's room of %s / %s", cpu.String(), mem.String(), podCPU.String(),
			podMem.String())
	}
}

func TestToolConfig(t *testing.T) {
	_, p := verifyPreview(t, time.Now())
	s := evalSettings()
	pv := toolVerification(t, p)
	targets := map[string]string{"web": "10.96.0.10:80", "api": "10.96.0.11:80"}
	cfg, err := s.toolConfig(p, pv, targets, 20*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "shop-1.preview.maul.example.com" || cfg.MaxToolCalls != 120 || cfg.TimeoutSeconds != 1200 ||
		cfg.ReplayOnly || !cfg.Browser || !cfg.Sandbox || len(cfg.Checks) != 1 || cfg.Checks[0].ID != "items.body" {
		t.Errorf("config %+v", cfg)
	}
	if len(cfg.Components) != 2 || cfg.Components[0] != (previewtools.Component{Key: "web", Path: "/",
		Addr: "10.96.0.10:80"}) || cfg.Components[1] != (previewtools.Component{Key: "api", Path: "/api",
		Addr: "10.96.0.11:80"}) {
		t.Errorf("components %+v", cfg.Components)
	}
	data, err := toolConfigData(cfg, pv.Spec.Outcomes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := previewtools.ParseConfig([]byte(data[previewtools.ConfigFile])); err != nil {
		t.Errorf("the relay refuses the config: %v", err)
	}
	var outcomes []v1alpha1.AcceptanceOutcome
	if err := json.Unmarshal([]byte(data[toolOutcomesKey]), &outcomes); err != nil || len(outcomes) != 2 {
		t.Errorf("outcomes %s: %v", data[toolOutcomesKey], err)
	}
	pv.Spec.Rung = v1alpha1.RungReplay
	if cfg, err := s.toolConfig(p, pv, targets, time.Minute); err != nil || !cfg.ReplayOnly {
		t.Errorf("replay rung: %+v, %v", cfg, err)
	}
	pv.Spec.Checks[0].Digest = "sha256:" + strings.Repeat("0", 64)
	if _, err := s.toolConfig(p, pv, targets, time.Minute); err == nil {
		t.Error("a check whose digest is not its body's was configured")
	}
}

// The per-verification ingress policy admits exactly one evaluator: the
// agents namespace by name, the pod by its role and this verification's
// UID, on the relay's port, ingress only.
func TestEvaluatorPolicy(t *testing.T) {
	_, p := verifyPreview(t, time.Now())
	pv := toolVerification(t, p)
	np := evalSettings().evaluatorPolicy(p, pv, 1)
	if np.Name != "preview-shop-1-np-ev-"+strings.TrimPrefix(testerJobName(p, pv), "preview-shop-1-vt-") ||
		np.Namespace != "maul-preview-1" || !ownedBy(np, p) || !renderedName(np) {
		t.Errorf("policy %s/%s labels %v", np.Namespace, np.Name, np.Labels)
	}
	if !isEvaluatorPolicy(p, np.Name) || isEvaluatorPolicy(p, policyName(p, "ev-api")) ||
		isEvaluatorPolicy(p, policyName(p, "tester")) {
		t.Error("isEvaluatorPolicy misjudges a name")
	}
	tcp := corev1.ProtocolTCP
	want := networkingv1.NetworkPolicySpec{
		PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{
			labelRole: v1alpha1.PreviewRoleTools, v1alpha1.LabelPreviewVerification: string(pv.UID)}},
		PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
		Ingress: []networkingv1.NetworkPolicyIngressRule{{
			From: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
					corev1.LabelMetadataName: "maul-agents"}},
				PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
					v1alpha1.LabelRole: v1alpha1.RoleEvaluator, v1alpha1.LabelPreviewVerification: string(pv.UID)}},
			}},
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: ptr(intstr.FromInt32(previewtools.Port))}},
		}},
	}
	if !equality.Semantic.DeepEqual(np.Spec, want) {
		t.Errorf("policy spec\n got %+v\nwant %+v", np.Spec, want)
	}
}

// The Preview's own edge reconcile keeps its parts' policies in line and
// never deletes a verification's ingress policy beside them.
func TestEdgeReconcileKeepsTheEvaluatorPolicy(t *testing.T) {
	at := time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC)
	in, p := verifyPreview(t, at)
	e := newTestEnv(t, verifyProject(), in, p)
	e.r.Settings = evalSettings()
	p = e.untilSlot(p.Name)
	np := e.r.Settings.evaluatorPolicy(p, toolVerification(t, p), 0)
	if err := e.c.Create(context.Background(), np); err != nil {
		t.Fatal(err)
	}
	if err := e.r.ensureNetworkPolicies(context.Background(), p, 0); err != nil {
		t.Fatal(err)
	}
	if err := e.slotObject(np.Name, &networkingv1.NetworkPolicy{}); err != nil {
		t.Errorf("the evaluator's ingress policy was deleted: %v", err)
	}
}

// TestGoldenToolPod pins every object an outcome verification renders into
// its slot: the ingress policy, the Job, the key Secret and the ConfigMap.
func TestGoldenToolPod(t *testing.T) {
	_, p := verifyPreview(t, time.Date(2026, 10, 3, 7, 0, 0, 0, time.UTC))
	s := evalSettings()
	pv := toolVerification(t, p)
	job, err := s.toolJob(p, pv, 0, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	job.UID = types.UID("job-uid")
	cfg, err := s.toolConfig(p, pv, map[string]string{"web": "10.96.0.10:80", "api": "10.96.0.11:80"}, 20*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	data, err := toolConfigData(cfg, pv.Spec.Outcomes)
	if err != nil {
		t.Fatal(err)
	}
	objects := []client.Object{s.evaluatorPolicy(p, pv, 0), job,
		s.toolKeySecret(p, pv, job, strings.Repeat("k", 64)), s.toolConfigMap(p, pv, job, data)}
	docs := make([]string, 0, len(objects))
	for _, obj := range objects {
		docs = append(docs, renderYAML(t, obj))
	}
	got := strings.Join(docs, "---\n")
	path := filepath.Join("testdata", "tool_pod.yaml")
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("update golden %s: %v", path, err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s (run with -update to create): %v", path, err)
	}
	if got != string(want) {
		t.Errorf("tool pod render differs from %s:\n--- want\n%s\n--- got\n%s", path, want, got)
	}
}

// The key Secret and the ConfigMap are the Job's: deleting it (the
// settle, the cleanup, the sweep) removes them, and no read is needed.
func TestToolObjectsAreTheJobs(t *testing.T) {
	_, p := verifyPreview(t, time.Now())
	s := evalSettings()
	pv := toolVerification(t, p)
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: testerJobName(p, pv), Namespace: "maul-preview-0",
		UID: "job-uid"}}
	key := newCDPKey()
	if len(key) != 2*cdpKeyBytes || key == newCDPKey() {
		t.Errorf("key %q is not a fresh 256-bit hex key", key)
	}
	sec := s.toolKeySecret(p, pv, job, key)
	cm := s.toolConfigMap(p, pv, job, map[string]string{"a": "b"})
	for _, obj := range []client.Object{sec, cm} {
		refs := obj.GetOwnerReferences()
		if len(refs) != 1 || refs[0].Kind != "Job" || refs[0].UID != "job-uid" || refs[0].Name != job.Name ||
			obj.GetNamespace() != job.Namespace {
			t.Errorf("%T owner %+v", obj, refs)
		}
	}
	if !*sec.Immutable || string(sec.Data[cdpKeyFile]) != key {
		t.Error("key Secret")
	}
}

// The sweep removes a tool pod whose Job is gone, as it does a tester's.
func TestSweepToolPodOrphans(t *testing.T) {
	ctx := context.Background()
	_, live := verifyPreview(t, time.Now())
	slot := int32(0)
	live.Status.Slot = &slot
	s := evalSettings()
	pv := toolVerification(t, live)
	job, err := s.toolJob(live, pv, 0, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	job.UID = "job-ghost"
	ghost := testerPod(job, job.Name+"-b")
	policy := s.evaluatorPolicy(live, pv, 0)
	gonePV := pv.DeepCopy()
	gonePV.UID = "pv-gone"
	orphanJob, err := s.toolJob(live, gonePV, 0, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	e := newTestEnv(t, live, pv, ghost, policy, orphanJob)
	if err := (&Sweeper{Client: e.c, Settings: s}).SweepOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Get(ctx, client.ObjectKeyFromObject(ghost), &corev1.Pod{}); err == nil {
		t.Error("a tool pod whose Job is gone survived the sweep")
	}
	if err := e.c.Get(ctx, client.ObjectKeyFromObject(orphanJob), &batchv1.Job{}); err == nil {
		t.Error("a tool Job whose verification is gone survived the sweep")
	}
	if err := e.c.Get(ctx, client.ObjectKeyFromObject(policy), &networkingv1.NetworkPolicy{}); err != nil {
		t.Errorf("a live Preview's verification policy was swept: %v", err)
	}
}
