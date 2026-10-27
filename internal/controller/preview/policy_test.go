// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

const testTesterImage = "ghcr.io/devthenet-labs/maul-preview-tester@sha256:" +
	"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// verifySettings are testSettings with verification on: a tester image.
func verifySettings() Settings {
	s := testSettings()
	s.TesterImage = testTesterImage
	return s
}

// verifyProject is shopProject with verification on.
func verifyProject() *v1alpha1.Project {
	p := shopProject()
	p.Spec.Verification = &v1alpha1.ProjectVerification{Enabled: true}
	return p
}

// verifyPreview is multiPreview made verification-capable, its environment
// derived as the writer derives it.
func verifyPreview(t *testing.T, at time.Time) (*v1alpha1.Intent, *v1alpha1.Preview) {
	t.Helper()
	in, p := multiPreview(t, at)
	env, ok := v1alpha1.DesiredPreviewEnvironment(verifyProject(), in)
	if !ok {
		t.Fatal("no environment derived")
	}
	p.Spec.Environment = env
	return in, p
}

// testVerification is a PreviewVerification of Preview p.
func testVerification(p *v1alpha1.Preview) *v1alpha1.PreviewVerification {
	return &v1alpha1.PreviewVerification{
		ObjectMeta: metav1.ObjectMeta{Name: p.Name + "-v1-a1", Namespace: "maul", UID: types.UID("pv-" + p.Name)},
		Spec: v1alpha1.PreviewVerificationSpec{
			IntentRef:  p.Spec.IntentRef,
			PreviewRef: v1alpha1.ObjectReference{Name: p.Name, UID: p.UID}, PreviewGeneration: 1,
			Round: 1, Attempt: 1, Run: "a1b2c3", Timeout: metav1.Duration{Duration: 5 * time.Minute},
		},
	}
}

// vapShape is the Go mirror of the maul-preview-networkpolicies admission
// policy's checks on a policy preview-controller writes (spec §5.5): a
// rendered name, the managed-by label, a selector of exactly the Preview's
// UID and one part, both policy types, and rules that each name at least one
// peer and one explicit TCP port, every peer a pod of the same Preview by
// part, by labels only.
func vapShape(np *networkingv1.NetworkPolicy) error {
	sel := np.Spec.PodSelector
	uid := sel.MatchLabels[labelPreviewUID]
	if !strings.HasPrefix(np.Name, "preview-") || !strings.Contains(np.Name, "-np-") ||
		np.Labels[labelManagedBy] != managedBy || !partOnly(&sel, uid) {
		return errors.New("selector or name")
	}
	kinds := np.Spec.PolicyTypes
	if len(kinds) != 2 || !slices.Contains(kinds, networkingv1.PolicyTypeIngress) ||
		!slices.Contains(kinds, networkingv1.PolicyTypeEgress) {
		return errors.New("policy types")
	}
	for _, r := range np.Spec.Ingress {
		if !ruleShape(r.From, r.Ports, uid) {
			return errors.New("ingress rule")
		}
	}
	for _, r := range np.Spec.Egress {
		if !ruleShape(r.To, r.Ports, uid) {
			return errors.New("egress rule")
		}
	}
	return nil
}

// partOnly: a selector of exactly the Preview's UID (non-empty) and a part.
func partOnly(sel *metav1.LabelSelector, uid string) bool {
	return sel != nil && uid != "" && len(sel.MatchExpressions) == 0 && len(sel.MatchLabels) == 2 &&
		sel.MatchLabels[labelPreviewUID] == uid && sel.MatchLabels[labelPart] != ""
}

// ruleShape: at least one peer, each a pod of the same Preview by part, by
// labels only; at least one port, each an explicit numeric TCP port.
func ruleShape(peers []networkingv1.NetworkPolicyPeer, ports []networkingv1.NetworkPolicyPort, uid string) bool {
	badPeer := func(p networkingv1.NetworkPolicyPeer) bool {
		return p.IPBlock != nil || p.NamespaceSelector != nil || !partOnly(p.PodSelector, uid)
	}
	badPort := func(p networkingv1.NetworkPolicyPort) bool {
		return p.Protocol == nil || *p.Protocol != corev1.ProtocolTCP || p.Port == nil || p.EndPort != nil ||
			p.Port.IntVal < 1
	}
	return len(peers) > 0 && len(ports) > 0 && !slices.ContainsFunc(peers, badPeer) &&
		!slices.ContainsFunc(ports, badPort)
}

func TestNetworkPoliciesRender(t *testing.T) {
	_, p := verifyPreview(t, time.Now())
	s := verifySettings()
	if err := s.validatePreview(p); err != nil {
		t.Fatal(err)
	}
	nps := s.networkPolicies(p, 0)
	names := make([]string, 0, len(nps))
	for _, np := range nps {
		names = append(names, np.Name)
		if err := vapShape(np); err != nil {
			t.Errorf("%s breaks the admission policy's shape: %v", np.Name, err)
		}
		if np.Namespace != "maul-preview-0" || !maps.Equal(np.Labels, labelsFor(p)) {
			t.Errorf("%s namespace %s labels %v", np.Name, np.Namespace, np.Labels)
		}
	}
	want := []string{"preview-shop-1-np-api", "preview-shop-1-np-tester", "preview-shop-1-np-web"}
	if !slices.Equal(names, want) {
		t.Fatalf("policies = %q, want %q", names, want)
	}
	tester := nps[1]
	if tester.Spec.Ingress != nil || len(tester.Spec.Egress) != 2 {
		t.Errorf("tester policy: nothing may call it, it calls each component: %+v", tester.Spec)
	}
	web := nps[2]
	if web.Spec.Egress != nil || len(web.Spec.Ingress) != 1 ||
		web.Spec.Ingress[0].From[0].PodSelector.MatchLabels[labelPart] != v1alpha1.PreviewPartTester ||
		web.Spec.Ingress[0].Ports[0].Port.IntVal != 8080 {
		t.Errorf("component policy: %+v", web.Spec)
	}
	// Without an environment, or with verification off, nothing renders.
	_, plain := multiPreview(t, time.Now())
	if got := s.networkPolicies(plain, 0); len(got) != 0 {
		t.Errorf("a Preview without an environment rendered %d policies", len(got))
	}
	if got := testSettings().networkPolicies(p, 0); len(got) != 0 {
		t.Errorf("verification off rendered %d policies", len(got))
	}
}

// The verification-capable Preview's component pods carry their part and
// role, its Deployment and Service selectors carry the component even with
// one component, and no Service selects the tester's pod.
func TestVerificationLabels(t *testing.T) {
	in, p := verifyPreview(t, time.Now())
	_ = in
	p.Spec.Components = p.Spec.Components[:1]
	p.Spec.Environment.Edges = p.Spec.Environment.Edges[:1]
	s := verifySettings()
	d := s.deployment(p, 0, 0)
	want := labelsFor(p)
	want[labelComponent] = "web"
	if !maps.Equal(d.Spec.Selector.MatchLabels, want) {
		t.Errorf("selector = %v, want %v", d.Spec.Selector.MatchLabels, want)
	}
	tmpl := d.Spec.Template.Labels
	if tmpl[labelPart] != "web" || tmpl[labelRole] != string(v1alpha1.PreviewRoleComponent) ||
		!labels.SelectorFromSet(d.Spec.Selector.MatchLabels).Matches(labels.Set(tmpl)) {
		t.Errorf("template labels = %v", tmpl)
	}
	job, err := s.testerJob(p, testVerification(p), 0, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	svc := s.service(p, 0, 0)
	if labels.SelectorFromSet(svc.Spec.Selector).Matches(labels.Set(job.Spec.Template.Labels)) {
		t.Errorf("Service selector %v selects the tester %v", svc.Spec.Selector, job.Spec.Template.Labels)
	}
}

// A Preview without an environment renders byte for byte what it did before
// verification existed, whether or not the controller verifies.
func TestNoEnvironmentRendersAsBefore(t *testing.T) {
	_, single := testPreview("demo-1", time.Now())
	_, multi := multiPreview(t, time.Now())
	for _, p := range []*v1alpha1.Preview{single, multi} {
		for i := range p.Spec.Components {
			if a, b := testSettings().deployment(p, i, 0), verifySettings().deployment(p, i, 0); !equalYAML(t, a, b) {
				t.Errorf("%s component %d Deployment changed with verification on", p.Name, i)
			}
			if a, b := testSettings().service(p, i, 0), verifySettings().service(p, i, 0); !equalYAML(t, a, b) {
				t.Errorf("%s component %d Service changed with verification on", p.Name, i)
			}
			if _, ok := testSettings().deployment(p, i, 0).Spec.Template.Labels[labelPart]; ok {
				t.Errorf("%s component %d carries a part label without an environment", p.Name, i)
			}
		}
	}
}

func TestDeployRendersPolicies(t *testing.T) {
	at := time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC)
	in, p := verifyPreview(t, at)
	e := newTestEnv(t, verifyProject(), in, p)
	e.r.Settings = verifySettings()
	p = e.untilSlot(p.Name)
	e.step(p.Name)
	ctx := context.Background()
	var nps networkingv1.NetworkPolicyList
	if err := e.c.List(ctx, &nps, client.InNamespace("maul-preview-0")); err != nil {
		t.Fatal(err)
	}
	if len(nps.Items) != 3 {
		t.Fatalf("rendered %d policies, want 3", len(nps.Items))
	}
	if ok, err := e.r.policiesInPlace(ctx, p, 0); !ok || err != nil {
		t.Errorf("policies in place = %v, %v after a deploy", ok, err)
	}
	// A policy edited by hand is put back; a stale owned one is pruned; a
	// foreign one is left alone.
	np := &nps.Items[0]
	np.Spec.Ingress = nil
	np.Spec.Egress = nil
	if err := e.c.Update(ctx, np); err != nil {
		t.Fatal(err)
	}
	if ok, err := e.r.policiesInPlace(ctx, p, 0); ok || err != nil {
		t.Errorf("policies in place = %v, %v with one edited", ok, err)
	}
	stale := verifySettings().networkPolicies(p, 0)[0]
	stale.Name = "preview-shop-1-np-gone"
	foreign := stale.DeepCopy()
	foreign.Name = "someone-elses"
	foreign.Labels = nil
	for _, obj := range []client.Object{stale, foreign} {
		if err := e.c.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	e.step(p.Name)
	var fixed networkingv1.NetworkPolicy
	if err := e.slotObject(np.Name, &fixed); err != nil {
		t.Fatal(err)
	}
	if fixed.Spec.Ingress == nil && fixed.Spec.Egress == nil {
		t.Errorf("edited policy %s was not restored", np.Name)
	}
	if err := e.slotObject(stale.Name, &networkingv1.NetworkPolicy{}); !kerrors.IsNotFound(err) {
		t.Errorf("stale policy survived: %v", err)
	}
	if err := e.slotObject(foreign.Name, &networkingv1.NetworkPolicy{}); err != nil {
		t.Errorf("foreign policy deleted: %v", err)
	}
}

// A policy write refused at admission never holds the Preview up or spends a
// retry: the slot stays closed, the Preview becomes Ready, and the policies
// are not in place, which a verification waits for.
func TestRefusedPolicyLeavesSlotClosed(t *testing.T) {
	at := time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC)
	in, p := verifyPreview(t, at)
	funcs := interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object,
		opts ...client.CreateOption) error {
		if _, ok := obj.(*networkingv1.NetworkPolicy); ok {
			return kerrors.NewForbidden(schema.GroupResource{Group: "networking.k8s.io", Resource: "networkpolicies"},
				obj.GetName(), errors.New("ValidatingAdmissionPolicy 'maul-preview-networkpolicies' denied"))
		}
		return c.Create(ctx, obj, opts...)
	}}
	e := newTestEnvWith(t, funcs, verifyProject(), in, p)
	e.r.Settings = verifySettings()
	p = e.untilSlot(p.Name)
	e.step(p.Name)
	e.markReady(p, 0)
	e.markReady(p, 1)
	if p = e.step(p.Name); p.Status.Phase != v1alpha1.PreviewReady || p.Status.Retries != 0 {
		t.Fatalf("status = %+v, want Ready without a retry", p.Status)
	}
	if ok, err := e.r.policiesInPlace(context.Background(), p, 0); ok || err != nil {
		t.Errorf("policies in place = %v, %v though every write was refused", ok, err)
	}
}

// With verification off the controller never lists, writes or deletes a
// NetworkPolicy or a Job: the chart grants it neither then.
func TestVerificationOffTouchesNoPoliciesOrJobs(t *testing.T) {
	at := time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC)
	in, p := verifyPreview(t, at)
	forbidden := func(obj any) error {
		switch obj.(type) {
		case *networkingv1.NetworkPolicy, *networkingv1.NetworkPolicyList, *batchv1.Job, *batchv1.JobList,
			*v1alpha1.PreviewVerificationList:
			return kerrors.NewForbidden(schema.GroupResource{}, "", errors.New("no RBAC"))
		}
		return nil
	}
	funcs := interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := forbidden(list); err != nil {
				return err
			}
			return c.List(ctx, list, opts...)
		},
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if err := forbidden(obj); err != nil {
				return err
			}
			return c.Create(ctx, obj, opts...)
		},
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
			opts ...client.GetOption) error {
			if err := forbidden(obj); err != nil {
				return err
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}
	e := newTestEnvWith(t, funcs, verifyProject(), in, p)
	p = e.untilSlot(p.Name)
	for range 3 {
		p = e.step(p.Name)
	}
	e.markReady(p, 0)
	e.markReady(p, 1)
	if p = e.step(p.Name); p.Status.Phase != v1alpha1.PreviewReady {
		t.Fatalf("phase = %s (%s), want Ready without policies", p.Status.Phase, p.Status.Message)
	}
	if err := (&Sweeper{Client: e.c, Settings: testSettings()}).SweepOnce(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if err := e.c.Delete(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	e.step(p.Name)
	e.collectGarbage()
	if _, err := e.r.Reconcile(context.Background(), reconcileRequest(p.Name)); err != nil {
		t.Fatalf("finalize: %v", err)
	}
}

// The Preview's environment and reset generation are re-derived from the
// Project and the Intent: a forged edge, a missing or extra environment, or a
// reset generation ahead of the Intent's renders nothing.
func TestMatchesApprovedEnvironment(t *testing.T) {
	in, p := verifyPreview(t, time.Now())
	project := verifyProject()
	if !matchesApprovedPreview(p, in, project) {
		t.Fatal("the derived Preview does not match")
	}
	in.Status.Verification = &v1alpha1.IntentVerification{Reset: v1alpha1.ResetState{Generation: 2}}
	for _, tc := range []struct {
		name   string
		mutate func(*v1alpha1.Preview)
		want   bool
	}{
		{"reset behind the Intent's, the writer lagging", func(p *v1alpha1.Preview) { p.Spec.ResetGeneration = 1 }, true},
		{"reset at the Intent's", func(p *v1alpha1.Preview) { p.Spec.ResetGeneration = 2 }, true},
		{"reset ahead of the Intent's", func(p *v1alpha1.Preview) { p.Spec.ResetGeneration = 3 }, false},
		{"forged edge", func(p *v1alpha1.Preview) {
			p.Spec.Environment.Edges = append(p.Spec.Environment.Edges,
				v1alpha1.PreviewEdge{From: "web", To: "api", Port: 8080})
		}, false},
		{"widened port", func(p *v1alpha1.Preview) { p.Spec.Environment.Edges[0].Port = 22 }, false},
		{"environment dropped", func(p *v1alpha1.Preview) { p.Spec.Environment = nil }, false},
	} {
		c := p.DeepCopy()
		tc.mutate(c)
		if got := matchesApprovedPreview(c, in, project); got != tc.want {
			t.Errorf("%s: matches = %v, want %v", tc.name, got, tc.want)
		}
	}
	// A Project that does not verify admits no environment, and no reset.
	plain := shopProject()
	if matchesApprovedPreview(p, in, plain) {
		t.Error("an environment matched a Project that does not verify")
	}
	_, bare := multiPreview(t, time.Now())
	bare.Spec.ResetGeneration = 1
	if matchesApprovedPreview(bare, in, plain) {
		t.Error("a reset generation matched a Preview without an environment")
	}
}

func TestEnvironmentValidation(t *testing.T) {
	_, p := verifyPreview(t, time.Now())
	s := verifySettings()
	for _, tc := range []struct {
		name   string
		mutate func(*v1alpha1.Preview)
	}{
		{"edge to an unknown part", func(p *v1alpha1.Preview) { p.Spec.Environment.Edges[0].To = "db" }},
		{"edge from an unknown part", func(p *v1alpha1.Preview) { p.Spec.Environment.Edges[0].From = "db" }},
		{"edge to the tester", func(p *v1alpha1.Preview) {
			p.Spec.Environment.Edges[0] = v1alpha1.PreviewEdge{From: "web", To: v1alpha1.PreviewPartTester, Port: 80}
		}},
		{"port zero", func(p *v1alpha1.Preview) { p.Spec.Environment.Edges[0].Port = 0 }},
		{"no edges", func(p *v1alpha1.Preview) { p.Spec.Environment.Edges = nil }},
		{"not verification", func(p *v1alpha1.Preview) { p.Spec.Environment.Verification = false }},
		{"component named tester", func(p *v1alpha1.Preview) {
			p.Spec.Components[0].Name = v1alpha1.PreviewPartTester
			p.Spec.Environment.Edges[0].To = v1alpha1.PreviewPartTester
		}},
	} {
		c := p.DeepCopy()
		tc.mutate(c)
		if err := s.validatePreview(c); err == nil {
			t.Errorf("%s: accepted", tc.name)
		}
	}
}

// A reset restarts every component: the Pod template carries the reset
// generation, a Pod of the previous one never counts Ready, and the status
// records the generation once the restarted components are.
func TestResetGenerationRestartsComponents(t *testing.T) {
	at := time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC)
	in, p := verifyPreview(t, at)
	e := newTestEnv(t, verifyProject(), in, p)
	e.r.Settings = verifySettings()
	p = e.untilSlot(p.Name)
	e.step(p.Name)
	e.markReady(p, 0)
	e.markReady(p, 1)
	if p = e.step(p.Name); p.Status.Phase != v1alpha1.PreviewReady || p.Status.ObservedResetGeneration != 0 {
		t.Fatalf("status = %+v, want Ready at reset 0", p.Status)
	}
	ctx := context.Background()
	in.Status.Verification = &v1alpha1.IntentVerification{Reset: v1alpha1.ResetState{Generation: 1}}
	var live v1alpha1.Intent
	if err := e.c.Get(ctx, client.ObjectKeyFromObject(in), &live); err != nil {
		t.Fatal(err)
	}
	live.Status.Verification = in.Status.Verification
	if err := e.c.Status().Update(ctx, &live); err != nil {
		t.Fatal(err)
	}
	p.Spec.ResetGeneration = 1
	p.Generation++
	if err := e.c.Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		p = e.step(p.Name)
	}
	var dep appsv1.Deployment
	if err := e.slotObject(componentName(p, 0), &dep); err != nil {
		t.Fatal(err)
	}
	if got := dep.Spec.Template.Annotations[annotationResetGeneration]; got != "1" {
		t.Fatalf("template reset annotation = %q, want 1", got)
	}
	if p.Status.Phase == v1alpha1.PreviewReady {
		t.Fatal("Ready on the previous reset generation's Pods")
	}
	e.markReadyNamed(p, 0, "-reset")
	e.markReadyNamed(p, 1, "-reset")
	if p = e.step(p.Name); p.Status.Phase != v1alpha1.PreviewReady || p.Status.ObservedResetGeneration != 1 {
		t.Fatalf("status = %+v, want Ready at reset 1", p.Status)
	}
}

func reconcileRequest(name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "maul", Name: name}}
}

// equalYAML compares two objects as their YAML renders.
func equalYAML(t *testing.T, a, b client.Object) bool {
	t.Helper()
	return renderYAML(t, a) == renderYAML(t, b)
}

func renderYAML(t *testing.T, obj client.Object) string {
	t.Helper()
	raw, err := yaml.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// markReadyNamed is markReady for a further Pod of component i, named with
// suffix: the restarted one beside the Pods of an earlier template.
func (e *testEnv) markReadyNamed(p *v1alpha1.Preview, i int, suffix string) {
	e.t.Helper()
	ctx := context.Background()
	var dep appsv1.Deployment
	if err := e.slotObject(componentName(p, i), &dep); err != nil {
		e.t.Fatalf("component %d Deployment: %v", i, err)
	}
	dep.Status.ObservedGeneration = dep.Generation
	dep.Status.AvailableReplicas = 1
	if err := e.c.Status().Update(ctx, &dep); err != nil {
		e.t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: dep.Name + "-pod" + suffix, Namespace: dep.Namespace, Labels: maps.Clone(dep.Spec.Template.Labels),
		Annotations: maps.Clone(dep.Spec.Template.Annotations),
	}, Spec: dep.Spec.Template.Spec}
	if err := e.c.Create(ctx, pod); err != nil {
		e.t.Fatal(err)
	}
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: p.Spec.Components[i].Name, Ready: true,
		ImageID: "repo@sha256:" + p.Spec.Components[i].Name}}
	if err := e.c.Status().Update(ctx, pod); err != nil {
		e.t.Fatal(err)
	}
}
