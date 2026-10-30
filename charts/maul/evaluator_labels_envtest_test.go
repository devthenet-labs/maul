// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package chart_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/devthenet-labs/maul/internal/kube"
	"github.com/devthenet-labs/maul/internal/rbactest"
)

// The label keys the fence judges (DESIGN 4.2, SPEC Q2 and Q5), spelled
// out: the fence pins strings, not a Go constant.
const (
	evRole        = "maul.run/role"
	evEvaluation  = "maul.run/preview-verification"
	evPreviewRole = "maul.run/preview-role"
	evUID         = "0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"
	evSlot        = "maul-preview-0"
)

// The fence's refusals, each distinct so a case cannot pass on the wrong
// rule.
const (
	evWriterDenied    = "only intent-controller sets them in the agents namespace"
	evShapeDenied     = "must carry its evaluator labels on the Job and its pod template alike"
	evJobChangeDenied = "the evaluator labels on job"
	evPodDenied       = "only the Kubernetes Job controller may create a pod carrying them"
	evPodChangeDenied = "the evaluator labels on pod"
)

// evaluatorEnvArgs renders previews (so the slot names are bound), the
// intent and preview controllers, and the evaluator on.
var evaluatorEnvArgs = append(append([]string{}, previewRuntimeArgs...),
	"--set", "intentController.verification.evaluator=true")

// TestEvaluatorLabelsAgainstAPIServer applies the maul-evaluator-labels
// policies, as the chart renders them with the evaluator on, to a real API
// server and drives them as the identities they judge: only intent-controller
// labels an agents-namespace Job, only preview-controller a slot tool Job,
// on the Job and its template alike; a Pod carrying a label only from the Job
// controller for its Job; and nobody changes one later. The policies are
// applied alone and every writer holds the verbs, so each refusal is this
// fence's, never RBAC's or another policy's.
func TestEvaluatorLabelsAgainstAPIServer(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via mise run envtest")
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
	applyEvaluatorLabelsRender(t, admin)
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
	waitFor(t, func() error {
		for _, probe := range []struct {
			obj  client.Object
			want string
		}{
			{evJob(egressAgents, "probe", evLabels(evRole, "evaluator"), evLabels(evRole, "evaluator")),
				evWriterDenied},
			{evPod(egressAgents, "probe", evLabels(evRole, "evaluator"), nil), evPodDenied},
		} {
			err := admin.Create(t.Context(), probe.obj, client.DryRunAll)
			if !deniedWith(err, probe.want) {
				return &waitError{err: err, want: probe.want}
			}
		}
		return nil
	})

	t.Run("jobs", func(t *testing.T) { checkEvaluatorJobs(t, admin, as) })
	t.Run("job updates", func(t *testing.T) { checkEvaluatorJobUpdates(t, admin, as) })
	t.Run("pods", func(t *testing.T) { checkEvaluatorPods(t, admin, as) })
}

func checkEvaluatorJobs(t *testing.T, admin client.Client, as func(string) client.Client) {
	ctx := t.Context()
	evaluator := evLabels(evRole, "evaluator", evEvaluation, evUID)
	tools := evLabels(evPreviewRole, "tools", evEvaluation, evUID)
	for _, tc := range []struct {
		name     string
		user     string
		ns       string
		job, pod map[string]string
		want     string // "" admits
	}{
		{"intent-controller, an evaluator", saIntent, egressAgents, evaluator, evaluator, ""},
		{"intent-controller, a check reviewer", saIntent, egressAgents,
			evLabels(evRole, "checkreview"), evLabels(evRole, "checkreview"), ""},
		{"intent-controller, an agents-namespace tool pod", saIntent, egressAgents,
			evLabels(evRole, "tools", evEvaluation, evUID), evLabels(evRole, "tools", evEvaluation, evUID), ""},
		{"intent-controller, an unlabelled Job", saIntent, egressAgents, nil, nil, ""},
		{"intent-controller, an unknown role", saIntent, egressAgents,
			evLabels(evRole, "planner"), evLabels(evRole, "planner"), evShapeDenied},
		{"intent-controller, the role on the Job only", saIntent, egressAgents,
			evLabels(evRole, "evaluator"), nil, evShapeDenied},
		// A Job with no labels takes its template's at creation, so the Job
		// here carries another.
		{"intent-controller, the role on the template only", saIntent, egressAgents,
			evLabels("example.com/note", "x"), evLabels(evRole, "evaluator"), evShapeDenied},
		{"intent-controller, Job and template name other evaluations", saIntent, egressAgents,
			evaluator, evLabels(evRole, "evaluator", evEvaluation, "other"), evShapeDenied},
		{"intent-controller, an evaluation with no role", saIntent, egressAgents,
			evLabels(evEvaluation, evUID), evLabels(evEvaluation, evUID), evShapeDenied},
		{"intent-controller, a slot tool label in the agents namespace", saIntent, egressAgents,
			tools, tools, evShapeDenied},
		{"remediation-controller, an evaluator", saRemediation, egressAgents, evaluator, evaluator, evWriterDenied},
		{"preview-controller in the agents namespace", saPreview, egressAgents, evaluator, evaluator,
			evWriterDenied},
		{"cluster-admin, an evaluator", "", egressAgents, evaluator, evaluator, evWriterDenied},
		{"cluster-admin, the evaluation on the template only", "", egressAgents,
			evLabels("example.com/note", "x"), evLabels(evEvaluation, evUID), evWriterDenied},
		{"cluster-admin, an unlabelled Job", "", egressAgents, nil, nil, ""},
		// evaluation-controller's evolve Jobs carry maul.run/evaluation; the
		// fence must never judge that label (as the admin it would refuse a
		// fenced one, see "cluster-admin, an evaluator").
		{"cluster-admin, an evolve evaluation Job", "", egressAgents,
			evLabels("maul.run/evaluation", "eval-1"), evLabels("maul.run/evaluation", "eval-1"), ""},

		{"preview-controller, a slot tool Job", saPreview, evSlot, tools, tools, ""},
		{"preview-controller, a tester Job", saPreview, evSlot,
			evLabels(evPreviewRole, "tester"), evLabels(evPreviewRole, "tester"), ""},
		{"preview-controller, a tool Job with the template differing", saPreview, evSlot,
			tools, evLabels(evPreviewRole, "tools", evEvaluation, "other"), evShapeDenied},
		{"preview-controller, a tester Job naming an evaluation", saPreview, evSlot,
			evLabels(evPreviewRole, "tester", evEvaluation, evUID),
			evLabels(evPreviewRole, "tester", evEvaluation, evUID), evShapeDenied},
		{"preview-controller, an evaluator role in a slot", saPreview, evSlot,
			evLabels(evPreviewRole, "tools", evRole, "evaluator"),
			evLabels(evPreviewRole, "tools", evRole, "evaluator"), evShapeDenied},
		{"intent-controller, a slot tool Job", saIntent, evSlot, tools, tools, evWriterDenied},
		{"cluster-admin, a slot tool Job", "", evSlot, tools, tools, evWriterDenied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := admin
			if tc.user != "" {
				c = as(tc.user)
			}
			err := c.Create(ctx, evJob(tc.ns, slug(tc.name), tc.job, tc.pod), client.DryRunAll)
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("refused, want admitted: %v", err)
			case tc.want != "" && !deniedWith(err, tc.want):
				t.Errorf("got %v, want refused with %q", err, tc.want)
			}
		})
	}
}

func checkEvaluatorJobUpdates(t *testing.T, admin client.Client, as func(string) client.Client) {
	ctx := t.Context()
	evaluator := evLabels(evRole, "evaluator", evEvaluation, evUID)
	tools := evLabels(evPreviewRole, "tools", evEvaluation, evUID)
	live := evJob(egressAgents, "live", evaluator, evaluator)
	if err := as(saIntent).Create(ctx, live); err != nil {
		t.Fatalf("create as intent-controller: %v", err)
	}
	slotLive := evJob(evSlot, "live", tools, tools)
	if err := as(saPreview).Create(ctx, slotLive); err != nil {
		t.Fatalf("create as preview-controller: %v", err)
	}
	plain := evJob(egressAgents, "plain", nil, nil)
	if err := admin.Create(ctx, plain); err != nil {
		t.Fatalf("create an unlabelled Job: %v", err)
	}
	for _, tc := range []struct {
		name   string
		user   string
		job    *batchv1.Job
		mutate func(map[string]string)
		want   string
	}{
		{"an unrelated label", "", live, func(l map[string]string) { l["example.com/note"] = "x" }, ""},
		{"changing the role", "", live, func(l map[string]string) { l[evRole] = "checkreview" }, evJobChangeDenied},
		{"changing the role as intent-controller", saIntent, live,
			func(l map[string]string) { l[evRole] = "checkreview" }, evJobChangeDenied},
		{"moving to another evaluation", "", live,
			func(l map[string]string) { l[evEvaluation] = "other" }, evJobChangeDenied},
		{"removing the evaluation", "", live, func(l map[string]string) { delete(l, evEvaluation) },
			evJobChangeDenied},
		{"labelling a plain Job an evaluator", "", plain,
			func(l map[string]string) { l[evRole] = "evaluator" }, evJobChangeDenied},
		{"labelling a plain Job a slot tool", "", plain,
			func(l map[string]string) { l[evPreviewRole] = "tools" }, evJobChangeDenied},
		{"dropping a slot Job's tool role", saPreview, slotLive,
			func(l map[string]string) { l[evPreviewRole] = "tester" }, evJobChangeDenied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := admin
			if tc.user != "" {
				c = as(tc.user)
			}
			current := &batchv1.Job{}
			if err := admin.Get(ctx, client.ObjectKeyFromObject(tc.job), current); err != nil {
				t.Fatalf("get: %v", err)
			}
			if current.Labels == nil {
				current.Labels = map[string]string{}
			}
			tc.mutate(current.Labels)
			err := c.Update(ctx, current, client.DryRunAll)
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("refused, want admitted: %v", err)
			case tc.want != "" && !deniedWith(err, tc.want):
				t.Errorf("got %v, want refused with %q", err, tc.want)
			}
		})
	}
}

func checkEvaluatorPods(t *testing.T, admin client.Client, as func(string) client.Client) {
	ctx := t.Context()
	evaluator := evLabels(evRole, "evaluator", evEvaluation, evUID)
	tools := evLabels(evPreviewRole, "tools", evEvaluation, evUID)
	owner := evJob(egressAgents, "pod-owner", evaluator, evaluator)
	if err := as(saIntent).Create(ctx, owner); err != nil {
		t.Fatalf("create the owning Job as intent-controller: %v", err)
	}
	slotOwner := evJob(evSlot, "pod-owner", tools, tools)
	if err := as(saPreview).Create(ctx, slotOwner); err != nil {
		t.Fatalf("create the owning slot Job as preview-controller: %v", err)
	}
	yes, no := true, false
	ref := func(j *batchv1.Job, controller *bool) []metav1.OwnerReference {
		return []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: j.Name, UID: j.UID,
			Controller: controller, BlockOwnerDeletion: controller}}
	}
	for _, tc := range []struct {
		name string
		user string
		pod  *corev1.Pod
		want string
	}{
		{"the Job controller, for its Job", saJobController,
			evPod(egressAgents, "jc", evaluator, ref(owner, &yes)), ""},
		{"the Job controller, a slot tool pod", saJobController, evPod(evSlot, "jc", tools, ref(slotOwner, &yes)), ""},
		{"a bare evaluator pod as cluster-admin", "", evPod(egressAgents, "bare", evaluator, nil), evPodDenied},
		{"an evaluation label alone as cluster-admin", "",
			evPod(egressAgents, "eval-only", evLabels(evEvaluation, evUID), nil), evPodDenied},
		{"a forged Job reference as cluster-admin", "",
			evPod(egressAgents, "forged", evaluator, ref(owner, &yes)), evPodDenied},
		{"intent-controller creating the pod itself", saIntent,
			evPod(egressAgents, "ic", evaluator, ref(owner, &yes)), evPodDenied},
		{"the Job controller with no controlling reference", saJobController,
			evPod(egressAgents, "noctl", evaluator, ref(owner, &no)), evPodDenied},
		{"the ReplicaSet controller claiming the Job", saReplicaSet,
			evPod(egressAgents, "rs", evaluator, ref(owner, &yes)), evPodDenied},
		{"a bare slot tool pod as preview-controller", saPreview, evPod(evSlot, "bare", tools, nil), evPodDenied},
		{"an unlabelled bare pod", "", evPod(egressAgents, "plain", nil, nil), ""},
		{"a bare slot tester pod", "", evPod(evSlot, "tester", evLabels(evPreviewRole, "tester"), nil), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := admin
			if tc.user != "" {
				c = as(tc.user)
			}
			err := c.Create(ctx, tc.pod, client.DryRunAll)
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("refused, want admitted: %v", err)
			case tc.want != "" && !deniedWith(err, tc.want):
				t.Errorf("got %v, want refused with %q", err, tc.want)
			}
		})
	}

	live := evPod(egressAgents, "live", evaluator, ref(owner, &yes))
	if err := as(saJobController).Create(ctx, live); err != nil {
		t.Fatalf("create as the Job controller: %v", err)
	}
	live.Labels["example.com/note"] = "unrelated"
	if err := admin.Update(ctx, live); err != nil {
		t.Errorf("an update leaving the labels alone was refused: %v", err)
	}
	moved := live.DeepCopy()
	moved.Labels[evEvaluation] = "other"
	if err := admin.Update(ctx, moved); !deniedWith(err, evPodChangeDenied) {
		t.Errorf("moving a live pod to another evaluation: got %v, want refused", err)
	}
	cleared := live.DeepCopy()
	delete(cleared.Labels, evRole)
	if err := admin.Update(ctx, cleared); !deniedWith(err, evPodChangeDenied) {
		t.Errorf("removing a live pod's role: got %v, want refused", err)
	}
	plain := evPod(evSlot, "plain-live", evLabels(evPreviewRole, "tester"), nil)
	if err := admin.Create(ctx, plain); err != nil {
		t.Fatalf("create a plain slot pod: %v", err)
	}
	// A status write keeps the metadata it is sent, so it is judged too.
	viaStatus := []byte(`{"metadata":{"labels":{"` + evPreviewRole + `":"tools"}}}`)
	if err := admin.Status().Patch(ctx, plain, client.RawPatch(types.MergePatchType, viaStatus)); !deniedWith(err,
		evPodChangeDenied) {
		t.Errorf("making a pod a tool pod through pods/status: got %v, want refused", err)
	}
	relabel := []byte(`{"metadata":{"labels":{"` + evEvaluation + `":"` + evUID + `"}}}`)
	if err := admin.Patch(ctx, plain, client.RawPatch(types.MergePatchType, relabel)); !deniedWith(err,
		evPodChangeDenied) {
		t.Errorf("adding an evaluation to a live pod: got %v, want refused", err)
	}
}

// applyEvaluatorLabelsRender renders the chart with evaluatorEnvArgs and
// creates its namespaces and the maul-evaluator-labels policies alone.
func applyEvaluatorLabelsRender(t *testing.T, admin client.Client) {
	t.Helper()
	ctx := t.Context()
	args := append([]string{"template", "maul", ".", "--namespace", egressRelease}, evaluatorEnvArgs...)
	output, err := exec.CommandContext(ctx, "helm", args...).Output()
	if err != nil {
		t.Fatalf("render chart: %v", err)
	}
	dec := yaml.NewYAMLOrJSONDecoder(strings.NewReader(string(output)), 4096)
	var namespaces, policies []*unstructured.Unstructured
	for {
		u := &unstructured.Unstructured{}
		if err := dec.Decode(u); err != nil {
			break
		}
		switch {
		case u.GetKind() == "Namespace":
			namespaces = append(namespaces, u)
		case strings.HasPrefix(u.GetName(), "maul-evaluator-labels-"):
			policies = append(policies, u)
		}
	}
	if len(policies) != 4 {
		t.Fatalf("the render holds %d maul-evaluator-labels objects, want 4", len(policies))
	}
	for _, u := range append(namespaces, policies...) {
		if err := admin.Create(ctx, u); err != nil {
			t.Fatalf("the API server refused %s %s: %v", u.GetKind(), u.GetName(), err)
		}
	}
}

// grantEvaluatorLabelTesters lets every identity the cases act as write Jobs
// and Pods in the agents namespace and the first slot, so each case is
// decided by admission, never by RBAC.
func grantEvaluatorLabelTesters(t *testing.T, admin client.Client) {
	t.Helper()
	ctx := t.Context()
	role := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "evaluator-labels-tester"},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{"batch"}, Resources: []string{"jobs"}, Verbs: []string{"get", "create", "update"}},
			{APIGroups: []string{""}, Resources: []string{"pods", "pods/status"},
				Verbs: []string{"get", "create", "update", "patch"}},
		},
	}
	users := []string{saIntent, saPreview, saRemediation, saJobController, saReplicaSet}
	if err := admin.Create(ctx, role); err != nil {
		t.Fatalf("create %s: %v", role.Name, err)
	}
	reviews := make([]authorizationv1.SubjectAccessReviewSpec, 0, 2*2*len(users))
	for _, ns := range []string{egressAgents, evSlot} {
		subjects := make([]rbacv1.Subject, 0, len(users))
		for _, u := range users {
			subjects = append(subjects, rbacv1.Subject{Kind: "User", APIGroup: rbacv1.GroupName, Name: u})
			reviews = append(reviews,
				rbactest.Review(u, nil, authorizationv1.ResourceAttributes{
					Namespace: ns, Group: "batch", Resource: "jobs", Verb: "create"}),
				rbactest.Review(u, nil, authorizationv1.ResourceAttributes{
					Namespace: ns, Resource: "pods", Verb: "create"}))
		}
		binding := &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "evaluator-labels-tester", Namespace: ns},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name},
			Subjects:   subjects,
		}
		if err := admin.Create(ctx, binding); err != nil {
			t.Fatalf("bind %s: %v", ns, err)
		}
	}
	rbactest.WaitAllowed(t, admin, reviews...)
}

// evLabels is the pairs as a label map.
func evLabels(kv ...string) map[string]string {
	m := make(map[string]string, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

// evJob is a Job in ns labelled job on itself and pod on its template.
func evJob(ns, name string, job, pod map[string]string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: copyLabels(job)},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: copyLabels(pod)},
				Spec:       evPod(ns, name, nil, nil).Spec,
			},
		},
	}
}

// evPod is a Pod in ns carrying labels and owners, restricted as both
// namespaces' Pod Security level requires.
func evPod(ns, name string, labels map[string]string, owners []metav1.OwnerReference) *corev1.Pod {
	noToken, no, yes := false, false, true
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: copyLabels(labels),
			OwnerReferences: owners},
		Spec: corev1.PodSpec{
			AutomountServiceAccountToken: &noToken,
			RestartPolicy:                corev1.RestartPolicyNever,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   &yes,
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name: "agent", Image: "example.com/agent:v1",
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: &no,
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
			}},
		},
	}
}

func copyLabels(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
