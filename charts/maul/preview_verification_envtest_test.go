// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package chart_test

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/kube"
	"github.com/devthenet-labs/maul/internal/rbactest"
)

const (
	saPreview = "system:serviceaccount:maul:maul-preview-controller"
	// verifyUID is the Preview every rendered edge policy belongs to.
	verifyUID = "11111111-2222-3333-4444-555555555555"

	npDenied       = "preview edges admit only pods of the same Preview"
	npShapeDenied  = "preview-controller policies select one part of one Preview"
	npTypesDenied  = "preview edge policies always cover both directions"
	npIsolation    = "preview-controller may never create, change or delete the slot isolation policy"
	jobOnlyPC      = "only preview-controller runs Jobs in a preview slot"
	jobOnce        = "preview Jobs run once, never retried, never self-deleting"
	jobDeadline    = "preview Jobs and their pods carry a bounded deadline"
	jobLabels      = "preview Job pods are labelled with their Preview, part and role"
	jobImage       = "only immutable"
	pvWriters      = "intent-controller writes verification requests; preview-controller writes their results"
	verifyDeadline = int64(600)
)

// TestPreviewVerificationPoliciesAgainstAPIServer applies the chart rendered
// with acceptance verification on to a real API server and drives its three
// fences as the identities they judge: preview-controller's edge policies
// admit exactly one Preview's pods by part on explicit TCP ports, in both
// directions, and never touch preview-isolation; only preview-controller
// runs a slot Job, once, bounded, labelled, in an admitted image; only
// intent-controller writes a verification request and only
// preview-controller its result. Each refusal is the policy's, never RBAC's:
// the controllers act through the chart's own Roles.
func TestPreviewVerificationPoliciesAgainstAPIServer(t *testing.T) {
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
	applyVerificationRender(t, admin)
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
	pc := as(saPreview)
	waitFor(t, func() error {
		for _, probe := range []struct {
			c    client.Client
			obj  client.Object
			want string
		}{
			{pc, mutateNP(edgePolicy("probe"), func(np *networkingv1.NetworkPolicy) {
				np.Spec.Egress[0].To[0] = networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0"}}
			}), npDenied},
			{admin, testerJob("probe"), jobOnlyPC},
			{admin, verification("probe"), pvWriters},
		} {
			err := probe.c.Create(t.Context(), probe.obj.DeepCopyObject().(client.Object), client.DryRunAll)
			if !deniedWith(err, probe.want) {
				return &waitError{err: err, want: probe.want}
			}
		}
		return nil
	})

	t.Run("networkpolicies", func(t *testing.T) { checkEdgePolicies(t, admin, pc) })
	t.Run("jobs", func(t *testing.T) { checkSlotJobs(t, admin, pc) })
	t.Run("verifications", func(t *testing.T) { checkVerificationWriters(t, admin, pc, as(saIntent)) })
}

// applyVerificationRender creates what the verification fences need from
// the chart's own render: the slot namespaces, the CRDs (established), the
// controllers' Roles and bindings, the slot isolation policies and every
// admission policy and binding. The rest (Deployments, the ALB's classes)
// needs objects envtest does not serve.
func applyVerificationRender(t *testing.T, admin client.Client) {
	t.Helper()
	ctx := t.Context()
	args := append([]string{"template", "maul", ".", "--namespace", egressRelease}, verificationArgs...)
	args = append(args, "--set", "preview.verification.deadlineMaxSeconds="+strconv.FormatInt(verifyDeadline, 10))
	output, err := exec.CommandContext(ctx, "helm", args...).Output()
	if err != nil {
		t.Fatalf("render chart: %v", err)
	}
	dec := yaml.NewYAMLOrJSONDecoder(strings.NewReader(string(output)), 4096)
	var namespaces, crds, others []*unstructured.Unstructured
	for {
		u := &unstructured.Unstructured{}
		if err := dec.Decode(u); err != nil {
			break
		}
		switch u.GetKind() {
		case "Namespace":
			namespaces = append(namespaces, u)
		case "CustomResourceDefinition":
			crds = append(crds, u)
		case "Role", "RoleBinding", "ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding":
			others = append(others, u)
		case "NetworkPolicy":
			if u.GetName() == "preview-isolation" {
				others = append(others, u)
			}
		}
	}
	for _, group := range [][]*unstructured.Unstructured{namespaces, crds, others} {
		for _, u := range group {
			if err := admin.Create(ctx, u); err != nil {
				t.Fatalf("the API server refused %s %s/%s: %v", u.GetKind(), u.GetNamespace(), u.GetName(), err)
			}
		}
		if len(group) > 0 && group[0].GetKind() == "CustomResourceDefinition" {
			waitCRDsEstablished(t, admin, group)
		}
	}
}

// waitVerificationGrants waits until RBAC has observed the controllers'
// verification grants, so every case below is decided by admission.
func waitVerificationGrants(t *testing.T, admin client.Client) {
	t.Helper()
	slot := func(group, resource, sub string) authorizationv1.ResourceAttributes {
		return authorizationv1.ResourceAttributes{Namespace: "maul-preview-0", Group: group, Resource: resource,
			Subresource: sub}
	}
	release := func(sub string) authorizationv1.ResourceAttributes {
		return authorizationv1.ResourceAttributes{Namespace: egressRelease, Group: v1alpha1.GroupVersion.Group,
			Resource: "previewverifications", Subresource: sub}
	}
	reviews := make([]authorizationv1.SubjectAccessReviewSpec, 0, 10)
	reviews = append(reviews, rbactest.Verbs(saPreview, nil, slot("batch", "jobs", ""), "create", "delete")...)
	reviews = append(reviews, rbactest.Verbs(saPreview, nil, slot("networking.k8s.io", "networkpolicies", ""),
		"create", "update", "delete")...)
	reviews = append(reviews, rbactest.Verbs(saPreview, nil, slot("", "pods", "log"), "get")...)
	reviews = append(reviews, rbactest.Verbs(saPreview, nil, slot("", "pods", ""), "delete")...)
	reviews = append(reviews, rbactest.Verbs(saPreview, nil, release("status"), "update")...)
	reviews = append(reviews, rbactest.Verbs(saIntent, nil, release(""), "create", "delete")...)
	rbactest.WaitAllowed(t, admin, reviews...)
}

func checkEdgePolicies(t *testing.T, admin, pc client.Client) {
	ctx := t.Context()
	admittedAs(t, pc, edgePolicy("tester"))
	admittedAs(t, pc, componentPolicy("api"))
	admittedAs(t, pc, mutateNP(edgePolicy("no-egress"), func(np *networkingv1.NetworkPolicy) { np.Spec.Egress = nil }))

	for _, dir := range []string{"egress", "ingress"} {
		base := edgePolicy
		peer := func(np *networkingv1.NetworkPolicy) *networkingv1.NetworkPolicyPeer { return &np.Spec.Egress[0].To[0] }
		rule := func(np *networkingv1.NetworkPolicy) *networkingv1.NetworkPolicyEgressRule { return &np.Spec.Egress[0] }
		ports := func(np *networkingv1.NetworkPolicy) *[]networkingv1.NetworkPolicyPort { return &rule(np).Ports }
		clearPeers := func(np *networkingv1.NetworkPolicy) { rule(np).To = nil }
		if dir == "ingress" {
			base = componentPolicy
			peer = func(np *networkingv1.NetworkPolicy) *networkingv1.NetworkPolicyPeer {
				return &np.Spec.Ingress[0].From[0]
			}
			ports = func(np *networkingv1.NetworkPolicy) *[]networkingv1.NetworkPolicyPort {
				return &np.Spec.Ingress[0].Ports
			}
			clearPeers = func(np *networkingv1.NetworkPolicy) { np.Spec.Ingress[0].From = nil }
		}
		endPort := int32(9000)
		udp := corev1.ProtocolUDP
		for _, tc := range []struct {
			name   string
			mutate func(*networkingv1.NetworkPolicy)
		}{
			{"ipBlock", func(np *networkingv1.NetworkPolicy) {
				*peer(np) = networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: "10.0.0.0/8"}}
			}},
			{"namespaceSelector", func(np *networkingv1.NetworkPolicy) {
				peer(np).NamespaceSelector = &metav1.LabelSelector{}
			}},
			{"no peers", clearPeers},
			{"no ports", func(np *networkingv1.NetworkPolicy) { *ports(np) = nil }},
			{"endPort", func(np *networkingv1.NetworkPolicy) { (*ports(np))[0].EndPort = &endPort }},
			{"UDP", func(np *networkingv1.NetworkPolicy) { (*ports(np))[0].Protocol = &udp }},
			{"named port", func(np *networkingv1.NetworkPolicy) {
				p := intstr.FromString("http")
				(*ports(np))[0].Port = &p
			}},
			{"no port", func(np *networkingv1.NetworkPolicy) { (*ports(np))[0].Port = nil }},
			{"foreign UID", func(np *networkingv1.NetworkPolicy) {
				peer(np).PodSelector.MatchLabels["maul.run/preview-uid"] = "99999999-2222-3333-4444-555555555555"
			}},
			{"matchExpressions", func(np *networkingv1.NetworkPolicy) {
				peer(np).PodSelector.MatchExpressions = []metav1.LabelSelectorRequirement{{
					Key: "app", Operator: metav1.LabelSelectorOpExists}}
			}},
			{"extra selector label", func(np *networkingv1.NetworkPolicy) {
				peer(np).PodSelector.MatchLabels["app"] = "x"
			}},
			{"no part", func(np *networkingv1.NetworkPolicy) {
				delete(peer(np).PodSelector.MatchLabels, "maul.run/preview-part")
				peer(np).PodSelector.MatchLabels["maul.run/preview-role"] = "component"
			}},
			{"any pod", func(np *networkingv1.NetworkPolicy) { peer(np).PodSelector = &metav1.LabelSelector{} }},
		} {
			refusedAs(t, pc, mutateNP(base(dir+"-"+slug(tc.name)), tc.mutate),
				npDenied, dir+" "+tc.name)
		}
	}
	refusedAs(t, pc, mutateNP(edgePolicy("one-type"), func(np *networkingv1.NetworkPolicy) {
		np.Spec.PolicyTypes = []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}
	}), npTypesDenied, "one policyType")
	refusedAs(t, pc, mutateNP(edgePolicy("unlabelled"), func(np *networkingv1.NetworkPolicy) { np.Labels = nil }),
		npShapeDenied, "no managed-by label")
	refusedAs(t, pc, mutateNP(edgePolicy("tester"), func(np *networkingv1.NetworkPolicy) {
		np.Name = "preview-battles-tester"
	}), npShapeDenied, "a name without -np-")
	refusedAs(t, pc, mutateNP(edgePolicy("all-pods"), func(np *networkingv1.NetworkPolicy) {
		np.Spec.PodSelector = metav1.LabelSelector{}
	}), npShapeDenied, "an empty pod selector")
	refusedAs(t, pc, mutateNP(edgePolicy("expr"), func(np *networkingv1.NetworkPolicy) {
		np.Spec.PodSelector.MatchExpressions = []metav1.LabelSelectorRequirement{{
			Key: "app", Operator: metav1.LabelSelectorOpExists}}
	}), npShapeDenied, "a pod selector with matchExpressions")
	refusedAs(t, pc, mutateNP(edgePolicy("isolation"), func(np *networkingv1.NetworkPolicy) {
		np.Name = "preview-isolation"
	}), npIsolation, "creating preview-isolation")

	// The kept isolation policy is Helm's: preview-controller may neither
	// change nor delete it, whatever it would make it.
	iso := &networkingv1.NetworkPolicy{}
	if err := admin.Get(ctx, client.ObjectKey{Namespace: "maul-preview-0", Name: "preview-isolation"}, iso); err != nil {
		t.Fatalf("get preview-isolation: %v", err)
	}
	replaced := edgePolicy("x")
	iso.Labels, iso.Spec = replaced.Labels, replaced.Spec
	if err := pc.Update(ctx, iso, client.DryRunAll); !deniedWith(err, npIsolation) {
		t.Errorf("preview-controller updating preview-isolation: %v, want %q", err, npIsolation)
	}
	if err := pc.Delete(ctx, iso, client.DryRunAll); !deniedWith(err, npIsolation) {
		t.Errorf("preview-controller deleting preview-isolation: %v, want %q", err, npIsolation)
	}

	// Its own policy it may change into another edge set and delete; it may
	// not widen it.
	own := edgePolicy("own")
	if err := pc.Create(ctx, own); err != nil {
		t.Fatalf("create own policy: %v", err)
	}
	own.Spec.Egress[0].Ports[0].Port = ptrPort(9090)
	if err := pc.Update(ctx, own, client.DryRunAll); err != nil {
		t.Errorf("preview-controller updating its own policy: %v", err)
	}
	own.Spec.Egress[0].To[0].NamespaceSelector = &metav1.LabelSelector{}
	if err := pc.Update(ctx, own, client.DryRunAll); !deniedWith(err, npDenied) {
		t.Errorf("preview-controller widening its own policy: %v, want %q", err, npDenied)
	}
	if err := pc.Delete(ctx, edgePolicy("own"), client.DryRunAll); err != nil {
		t.Errorf("preview-controller deleting its own policy: %v", err)
	}

	// Anyone else keeps today's posture: the operator may write any policy.
	admittedAs(t, admin, mutateNP(edgePolicy("operator"), func(np *networkingv1.NetworkPolicy) {
		np.Spec.Egress[0].To[0] = networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: "10.0.0.0/8"}}
		np.Labels = nil
	}))
	if err := admin.Delete(ctx, iso, client.DryRunAll); err != nil {
		t.Errorf("the operator deleting preview-isolation: %v", err)
	}
}

func checkSlotJobs(t *testing.T, admin, pc client.Client) {
	admittedAs(t, pc, testerJob("tester"))
	refusedAs(t, admin, testerJob("operator"), jobOnlyPC, "a Job by anyone but preview-controller")
	one, two := int32(1), int32(2)
	ttl := int32(60)
	tooLong := verifyDeadline + 1
	yes := true
	for _, tc := range []struct {
		name   string
		mutate func(*batchv1.Job)
		want   string
	}{
		{"backoffLimit 1", func(j *batchv1.Job) { j.Spec.BackoffLimit = &one }, jobOnce},
		{"backoffLimit unset", func(j *batchv1.Job) { j.Spec.BackoffLimit = nil }, jobOnce},
		{"parallelism 2", func(j *batchv1.Job) { j.Spec.Parallelism = &two }, jobOnce},
		{"completions 2", func(j *batchv1.Job) { j.Spec.Completions = &two }, jobOnce},
		{"ttl", func(j *batchv1.Job) { j.Spec.TTLSecondsAfterFinished = &ttl }, jobOnce},
		{"suspended", func(j *batchv1.Job) { j.Spec.Suspend = &yes }, jobOnce},
		{"pod failure policy", func(j *batchv1.Job) {
			j.Spec.PodFailurePolicy = &batchv1.PodFailurePolicy{Rules: []batchv1.PodFailurePolicyRule{{
				Action: batchv1.PodFailurePolicyActionIgnore,
				OnPodConditions: []batchv1.PodFailurePolicyOnPodConditionsPattern{{
					Type: corev1.DisruptionTarget, Status: corev1.ConditionTrue}},
			}}}
		}, jobOnce},
		{"no Job deadline", func(j *batchv1.Job) { j.Spec.ActiveDeadlineSeconds = nil }, jobDeadline},
		{"Job deadline past the maximum", func(j *batchv1.Job) {
			j.Spec.ActiveDeadlineSeconds = &tooLong
			j.Spec.Template.Spec.ActiveDeadlineSeconds = &tooLong
		}, jobDeadline},
		{"no pod deadline", func(j *batchv1.Job) { j.Spec.Template.Spec.ActiveDeadlineSeconds = nil }, jobDeadline},
		{"pod deadline past the Job's", func(j *batchv1.Job) {
			longer := *j.Spec.ActiveDeadlineSeconds + 1
			j.Spec.Template.Spec.ActiveDeadlineSeconds = &longer
		}, jobDeadline},
		{"restart OnFailure", func(j *batchv1.Job) {
			j.Spec.Template.Spec.RestartPolicy = corev1.RestartPolicyOnFailure
		}, jobDeadline},
		{"no role", func(j *batchv1.Job) { delete(j.Spec.Template.Labels, "maul.run/preview-role") }, jobLabels},
		{"component role", func(j *batchv1.Job) {
			j.Spec.Template.Labels["maul.run/preview-role"] = "component"
		}, jobLabels},
		{"no preview", func(j *batchv1.Job) { delete(j.Spec.Template.Labels, "maul.run/preview-uid") }, jobLabels},
		{"no part", func(j *batchv1.Job) { delete(j.Spec.Template.Labels, "maul.run/preview-part") }, jobLabels},
		{"postgres tag", func(j *batchv1.Job) { j.Spec.Template.Spec.Containers[0].Image = "postgres:16" }, jobImage},
		{"tester by tag", func(j *batchv1.Job) {
			j.Spec.Template.Spec.Containers[0].Image = "ghcr.io/devthenet-labs/maul/preview-tester:v1"
		}, jobImage},
		{"two containers", func(j *batchv1.Job) {
			c := j.Spec.Template.Spec.Containers[0]
			c.Name = "second"
			j.Spec.Template.Spec.Containers = append(j.Spec.Template.Spec.Containers, c)
		}, "exactly one container"},
		{"token mounted", func(j *batchv1.Job) { j.Spec.Template.Spec.AutomountServiceAccountToken = &yes }, "API token"},
		{"no node selector", func(j *batchv1.Job) { j.Spec.Template.Spec.NodeSelector = nil }, "dedicated preview"},
		{"init container", func(j *batchv1.Job) {
			init := j.Spec.Template.Spec.Containers[0]
			init.Name = "init"
			j.Spec.Template.Spec.InitContainers = []corev1.Container{init}
		}, "no init or ephemeral"},
		{"host network", func(j *batchv1.Job) { j.Spec.Template.Spec.HostNetwork = true }, "host namespaces"},
		{"manual selector", func(j *batchv1.Job) {
			j.Spec.ManualSelector = &yes
			j.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"maul.run/preview-part": "tester"}}
		}, jobOnce},
	} {
		refusedAs(t, pc, mutateJob(testerJob(slug(tc.name)), tc.mutate),
			tc.want, tc.name)
	}
	// The pod the Job controller creates for it is admitted by the slot pod
	// rules: the tester's exact image is one of them.
	j := testerJob("pod")
	admittedAs(t, admin, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "tester-pod", Namespace: "maul-preview-0",
		Labels: j.Spec.Template.Labels}, Spec: j.Spec.Template.Spec})
}

func checkVerificationWriters(t *testing.T, admin, pc, ic client.Client) {
	ctx := t.Context()
	refusedAs(t, admin, verification("by-operator"), pvWriters, "a request by the operator")
	v := verification("target-1-v1-a1")
	if err := ic.Create(ctx, v); err != nil {
		t.Fatalf("intent-controller creating a request: %v", err)
	}
	v.Status = v1alpha1.PreviewVerificationStatus{Phase: v1alpha1.VerificationRunning}
	if err := pc.Status().Update(ctx, v, client.DryRunAll); err != nil {
		t.Errorf("preview-controller writing the result: %v", err)
	}
	if err := admin.Status().Update(ctx, v.DeepCopy(), client.DryRunAll); !deniedWith(err, pvWriters) {
		t.Errorf("the operator writing the result: %v, want %q", err, pvWriters)
	}
	if err := ic.Status().Update(ctx, v.DeepCopy(), client.DryRunAll); !deniedWith(err, pvWriters, "forbidden") {
		t.Errorf("intent-controller writing the result: %v, want a refusal", err)
	}
	if err := admin.Delete(ctx, v.DeepCopy(), client.DryRunAll); !deniedWith(err, pvWriters) {
		t.Errorf("the operator deleting a request: %v, want %q", err, pvWriters)
	}
	if err := ic.Delete(ctx, v.DeepCopy(), client.DryRunAll); err != nil {
		t.Errorf("intent-controller deleting a request: %v", err)
	}
}

// edgePolicy is the tester's policy as preview-controller renders it: egress
// to two components of its own Preview, no ingress.
func edgePolicy(part string) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "preview-battles-np-" + part, Namespace: "maul-preview-0",
			Labels: map[string]string{"maul.run/preview": "battles", "maul.run/preview-uid": verifyUID,
				"app.kubernetes.io/managed-by": "maul-preview-controller"}},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: partSelector(part),
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{{
				To:    []networkingv1.NetworkPolicyPeer{{PodSelector: ptrSelector(partSelector("api"))}},
				Ports: []networkingv1.NetworkPolicyPort{tcpPort(8080)},
			}, {
				To:    []networkingv1.NetworkPolicyPeer{{PodSelector: ptrSelector(partSelector("web"))}},
				Ports: []networkingv1.NetworkPolicyPort{tcpPort(3000)},
			}},
		},
	}
}

// componentPolicy is a component's policy: ingress from the tester only.
func componentPolicy(part string) *networkingv1.NetworkPolicy {
	np := edgePolicy(part)
	np.Spec.Egress = nil
	np.Spec.Ingress = []networkingv1.NetworkPolicyIngressRule{{
		From:  []networkingv1.NetworkPolicyPeer{{PodSelector: ptrSelector(partSelector("tester"))}},
		Ports: []networkingv1.NetworkPolicyPort{tcpPort(8080)},
	}}
	return np
}

func partSelector(part string) metav1.LabelSelector {
	return metav1.LabelSelector{MatchLabels: map[string]string{"maul.run/preview-uid": verifyUID,
		"maul.run/preview-part": part}}
}

func ptrSelector(s metav1.LabelSelector) *metav1.LabelSelector { return &s }

func ptrPort(n int) *intstr.IntOrString {
	p := intstr.FromInt32(int32(n))
	return &p
}

func tcpPort(n int) networkingv1.NetworkPolicyPort {
	tcp := corev1.ProtocolTCP
	return networkingv1.NetworkPolicyPort{Protocol: &tcp, Port: ptrPort(n)}
}

func mutateNP(np *networkingv1.NetworkPolicy, f func(*networkingv1.NetworkPolicy)) *networkingv1.NetworkPolicy {
	f(np)
	return np
}

// testerJob is the tester Job as §5.6 has preview-controller render it.
func testerJob(name string) *batchv1.Job {
	zero, one := int32(0), int32(1)
	deadline := verifyDeadline
	p := pod("maul-preview-0", name, testerImage)
	p.Spec.RestartPolicy = corev1.RestartPolicyNever
	p.Spec.ActiveDeadlineSeconds = &deadline
	// The slots enforce PodSecurity restricted.
	yes, no := true, false
	p.Spec.SecurityContext = &corev1.PodSecurityContext{RunAsNonRoot: &yes,
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}}
	p.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{AllowPrivilegeEscalation: &no,
		Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "preview-battles-vt-" + name, Namespace: "maul-preview-0"},
		Spec: batchv1.JobSpec{
			BackoffLimit: &zero, Parallelism: &one, Completions: &one, ActiveDeadlineSeconds: &deadline,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
					"maul.run/preview": "battles", "maul.run/preview-uid": verifyUID,
					"maul.run/preview-part": "tester", "maul.run/preview-role": "tester",
				}},
				Spec: p.Spec,
			},
		},
	}
}

// slug is a case name as an object name: lowercase letters, digits and
// dashes.
func slug(name string) string {
	return strings.Trim(strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r + 'a' - 'A'
		}
		return '-'
	}, name), "-")
}

func mutateJob(j *batchv1.Job, f func(*batchv1.Job)) *batchv1.Job {
	f(j)
	return j
}

// verification is a valid verification request in the release namespace.
func verification(name string) *v1alpha1.PreviewVerification {
	return &v1alpha1.PreviewVerification{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: egressRelease},
		Spec: v1alpha1.PreviewVerificationSpec{
			IntentRef:         v1alpha1.ObjectReference{Name: "target-1", UID: "u-intent"},
			PreviewRef:        v1alpha1.ObjectReference{Name: "target-1", UID: "u-preview"},
			PreviewGeneration: 1,
			HeadSetDigest:     "sha256:" + strings.Repeat("d", 64),
			Heads: []v1alpha1.VerificationHead{{Component: "api", Repository: "https://github.com/acme/api",
				Revision: previewSHA}},
			Round: 1, Attempt: 1, Run: "0a1b2c",
			Criteria: []v1alpha1.AcceptanceCriterion{{
				ID: "home", Kind: v1alpha1.AcceptanceHTTP, Statement: "the home page answers", Target: "api",
				Steps: []v1alpha1.AcceptanceStep{{
					Request: v1alpha1.AcceptanceRequest{Method: "GET", Path: "/"},
					Expect:  v1alpha1.AcceptanceExpect{Status: &apiextensionsv1.JSON{Raw: []byte("200")}},
				}},
			}},
			CriteriaDigest: "sha256:" + strings.Repeat("d", 64),
			Timeout:        metav1.Duration{Duration: 10 * time.Minute},
		},
	}
}

// admittedAs: a dry-run create of obj as c succeeds.
func admittedAs(t *testing.T, c client.Client, obj client.Object) {
	t.Helper()
	if err := c.Create(t.Context(), obj, client.DryRunAll); err != nil {
		t.Errorf("%s/%s refused: %v", obj.GetNamespace(), obj.GetName(), err)
	}
}

// refusedAs: a dry-run create of obj as c fails with want in its message.
func refusedAs(t *testing.T, c client.Client, obj client.Object, want, what string) {
	t.Helper()
	err := c.Create(t.Context(), obj, client.DryRunAll)
	if !deniedWith(err, want) {
		t.Errorf("%s: %v, want a refusal containing %q", what, err, want)
	}
}
