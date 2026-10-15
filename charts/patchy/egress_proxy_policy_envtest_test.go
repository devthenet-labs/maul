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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/bitwise-media-group/patchy/internal/kube"
	"github.com/bitwise-media-group/patchy/internal/netprofile"
	"github.com/bitwise-media-group/patchy/internal/rbactest"
)

const (
	egressAgents   = "patchy-agents"
	egressRelease  = "patchy"
	egressDenied   = "carries a network profile label"
	egressKind     = "patchy.bitwisemedia.uk/kind"
	egressImageSrc = "patchy.bitwisemedia.uk/runner-image-source"
)

// The ServiceAccounts the chart's profile-label policy names, and one it
// must refuse although it holds jobs create in the agent namespace.
const (
	saRemediation   = "system:serviceaccount:patchy:patchy-remediation-controller"
	saIntent        = "system:serviceaccount:patchy:patchy-intent-controller"
	saInvestigation = "system:serviceaccount:patchy:patchy-investigation-controller"
)

// egressOnArgs renders the egress-proxy on-fixture: repository images, the
// intent controller, two profiles, dns none and attribution on, so every
// object the feature can render is in the set.
var egressOnArgs = []string{
	"-f", "../../hack/testdata/chart-render/repository-images.yaml",
	"-f", "../../hack/testdata/chart-render/intent-controller.yaml",
	"-f", "../../hack/testdata/chart-render/egress-proxy.yaml",
	"--set", "agent.networkPolicy.dns=none",
	"--set", "egressProxy.attribution.enabled=true",
	"--set-json", `egressProxy.attribution.apiServerCIDRs=["10.0.1.10/32"]`,
}

// TestEgressProxyAgainstAPIServer applies every object the chart renders
// with the egress proxy on to a real API server, which catches what a
// render cannot (an invalid ipBlock except, a bad port name, a CEL
// expression that does not compile), then drives the profile-label
// admission policy as the job controllers' ServiceAccounts, with the RBAC
// the chart itself grants them. envtest enforces no NetworkPolicy: the live
// gate's probe pods prove the policies' packets.
func TestEgressProxyAgainstAPIServer(t *testing.T) {
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
	ctx := t.Context()
	if err := admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: egressRelease}}); err != nil {
		t.Fatalf("release namespace: %v", err)
	}
	applyEgressRender(t, admin)

	// The policy enforces once a request it must refuse is refused.
	waitFor(t, func() error {
		err := admin.Create(ctx, profileJob("probe", "remediation", true), client.DryRunAll)
		if deniedWith(err, egressDenied) {
			return nil
		}
		return &waitError{err: err, want: egressDenied}
	})

	// The chart's RBAC was stored a moment ago; act as the controllers only
	// once the authorizer has observed it, or an admitted case fails on RBAC.
	controllers := []string{saIntent, saRemediation, saInvestigation}
	granted := make([]authorizationv1.SubjectAccessReviewSpec, 0, len(controllers))
	for _, sa := range controllers {
		granted = append(granted, rbactest.Review(sa, nil, authorizationv1.ResourceAttributes{
			Namespace: egressAgents, Group: "batch", Resource: "jobs", Verb: "create",
		}))
	}
	rbactest.WaitAllowed(t, admin, granted...)

	as := func(user string) client.Client {
		c := rest.CopyConfig(cfg)
		c.Impersonate = rest.ImpersonationConfig{UserName: user}
		cl, err := client.New(c, client.Options{Scheme: kube.Scheme()})
		if err != nil {
			t.Fatalf("client as %s: %v", user, err)
		}
		return cl
	}
	for _, tc := range []struct {
		name  string
		user  string
		job   *batchv1.Job
		admit bool
	}{
		{"intent-controller, kind intent, repository image", saIntent, profileJob("i1", "intent", true), true},
		{"remediation-controller, kind remediation, repository image", saRemediation,
			profileJob("r1", "remediation", true), true},
		{"intent-controller without the repository-image annotation", saIntent,
			profileJob("i2", "intent", false), false},
		{"investigation-controller", saInvestigation, profileJob("v1", "investigation", true), false},
		{"investigation-controller claiming kind remediation", saInvestigation,
			profileJob("v2", "remediation", true), false},
		{"remediation-controller with kind intent", saRemediation, profileJob("r2", "intent", true), false},
		{"intent-controller with kind remediation", saIntent, profileJob("i3", "remediation", true), false},
		{"the label on the pod template only", saInvestigation, podLabelOnly(profileJob("v3", "investigation", true)),
			false},
		{"an unlabelled Job from the investigation-controller", saInvestigation,
			unlabelled(profileJob("v4", "investigation", false)), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := as(tc.user).Create(ctx, tc.job, client.DryRunAll)
			switch {
			case tc.admit && err != nil:
				t.Errorf("refused, want admitted: %v", err)
			case !tc.admit && !deniedWith(err, egressDenied):
				t.Errorf("got %v, want refused with %q", err, egressDenied)
			}
		})
	}

	// UPDATE: only a change to the profile labels is judged.
	intent := as(saIntent)
	job := profileJob("i-live", "intent", true)
	if err := intent.Create(ctx, job); err != nil {
		t.Fatalf("create as intent-controller: %v", err)
	}
	job.Annotations["example.com/note"] = "unrelated"
	if err := admin.Update(ctx, job); err != nil {
		t.Errorf("an update leaving the profile labels alone was refused: %v", err)
	}
	relabel := job.DeepCopy()
	relabel.Labels[netprofile.LabelKey("npm")] = netprofile.LabelValue
	if err := admin.Update(ctx, relabel); !deniedWith(err, egressDenied) {
		t.Errorf("adding a profile label as another user: got %v, want refused", err)
	}
	cleared := job.DeepCopy()
	delete(cleared.Labels, netprofile.LabelKey("go"))
	// The pod template is immutable on a Job, so the label stays there; the
	// policy judges the change all the same and refuses it as the admin.
	if err := admin.Update(ctx, cleared); !deniedWith(err, egressDenied) {
		t.Errorf("removing the Job label but keeping the pod's as another user: got %v, want refused", err)
	}
}

// applyEgressRender renders the chart with egressOnArgs and creates every
// object it holds, namespaces first.
func applyEgressRender(t *testing.T, admin client.Client) {
	t.Helper()
	ctx := t.Context()
	args := append([]string{"template", "patchy", ".", "--namespace", egressRelease}, egressOnArgs...)
	output, err := exec.CommandContext(ctx, "helm", args...).Output()
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
		if u.GetKind() == "" {
			continue
		}
		if u.GetKind() == "Namespace" {
			namespaces = append(namespaces, u)
		} else {
			others = append(others, u)
		}
	}
	seen := map[string]bool{}
	for _, u := range append(namespaces, others...) {
		if err := admin.Create(ctx, u); err != nil {
			t.Fatalf("the API server refused %s %s/%s: %v", u.GetKind(), u.GetNamespace(), u.GetName(), err)
		}
		seen[u.GetKind()+"/"+u.GetName()] = true
	}
	for _, want := range []string{
		"Deployment/patchy-egress-proxy", "Service/patchy-egress-proxy", "PodDisruptionBudget/patchy-egress-proxy",
		"NetworkPolicy/patchy-egress-proxy", "NetworkPolicy/patchy-agents-network-go",
		"NetworkPolicy/patchy-agents-network-npm", "Role/patchy-egress-proxy-attribution",
		"ValidatingAdmissionPolicy/patchy-network-profiles",
		"ValidatingAdmissionPolicyBinding/patchy-network-profiles",
	} {
		if !seen[want] {
			t.Errorf("the on-fixture render has no %s", want)
		}
	}
}

// profileJob is an agent Job carrying profile go on the Job and its pod,
// of the given run kind, with the repository-image annotation when image.
func profileJob(name, kind string, image bool) *batchv1.Job {
	labels := map[string]string{
		"app.kubernetes.io/name":       "patchy-agent",
		"app.kubernetes.io/managed-by": "patchy",
		egressKind:                     kind,
		netprofile.LabelKey("go"):      netprofile.LabelValue,
	}
	ann := map[string]string{}
	if image {
		ann[egressImageSrc] = "repository"
	}
	podLabels := map[string]string{}
	for k, v := range labels {
		podLabels[k] = v
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: egressAgents, Labels: labels, Annotations: ann},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: podLabels},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers:    []corev1.Container{{Name: "agent", Image: "example.com/agent:v1"}},
				},
			},
		},
	}
}

// podLabelOnly keeps the profile label on the pod template alone.
func podLabelOnly(j *batchv1.Job) *batchv1.Job {
	delete(j.Labels, netprofile.LabelKey("go"))
	return j
}

// unlabelled drops the profile label from the Job and its pod.
func unlabelled(j *batchv1.Job) *batchv1.Job {
	delete(j.Labels, netprofile.LabelKey("go"))
	delete(j.Spec.Template.Labels, netprofile.LabelKey("go"))
	return j
}
