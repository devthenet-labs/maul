// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package chart_test

import (
	"os"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/devthenet-labs/maul/internal/kube"
	"github.com/devthenet-labs/maul/internal/provider"
	"github.com/devthenet-labs/maul/internal/rbactest"
)

// The budget policies' refusals, each distinct so a case cannot pass on the
// wrong rule.
const (
	budgetLauncherDenied  = "is launched by intent-controller and must be an intent run's"
	budgetOutsiderDenied  = "only intent-controller may set it"
	budgetJobChangeDenied = "budget-micro-usd annotation on job"
	budgetPodDenied       = "only the Kubernetes Job controller may create a pod carrying it"
	budgetPodChangeDenied = "budget-micro-usd annotation on pod"
	budgetRunDenied       = "may only be granted by intent-controller"
)

// budgetArgs renders the intent controller with the broker counting cost.
var budgetArgs = []string{
	"-f", "../../hack/testdata/chart-render/intent-controller.yaml",
	"--set", "egressBroker.limits.unit=cost",
	"--set", "egressBroker.limits.costPerPod=60000000",
}

// TestBudgetPoliciesAgainstAPIServer applies the chart rendered with the
// cost unit and the intent controller to a real API server, then drives each
// budget policy as the identities it judges: only intent-controller sets the
// annotation, always on its own Jobs and for kind intent; a pod carries it
// only from the Job controller for its Job; nobody changes either later; and
// only intent-controller grants an IntentRun a budget.
func TestBudgetPoliciesAgainstAPIServer(t *testing.T) {
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
	seen := applyChartRender(t, admin, budgetArgs)
	for _, name := range []string{"budget-jobs", "budget-pods", "budget-runs"} {
		for _, kind := range []string{"ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding"} {
			if !seen[kind+"/maul-"+name] {
				t.Errorf("the budget render has no %s maul-%s", kind, name)
			}
		}
	}
	grantAttributionTesters(t, admin)
	as := func(user string) client.Client {
		c := rest.CopyConfig(cfg)
		c.Impersonate = rest.ImpersonationConfig{UserName: user}
		cl, err := client.New(c, client.Options{Scheme: kube.Scheme()})
		if err != nil {
			t.Fatalf("client as %s: %v", user, err)
		}
		return cl
	}
	t.Run("jobs", func(t *testing.T) { checkBudgetJobs(t, admin, as) })
	t.Run("pods", func(t *testing.T) { checkBudgetPods(t, admin, as) })
	t.Run("runs", func(t *testing.T) { checkBudgetRuns(t, as) })
}

func checkBudgetJobs(t *testing.T, admin client.Client, as func(string) client.Client) {
	ctx := t.Context()
	waitFor(t, func() error {
		err := admin.Create(ctx, budgetJob("probe", "remediation", "5"), client.DryRunAll)
		if deniedWith(err, budgetOutsiderDenied) {
			return nil
		}
		return &waitError{err: err, want: budgetOutsiderDenied}
	})
	granted := make([]authorizationv1.SubjectAccessReviewSpec, 0, 2)
	for _, sa := range []string{saRemediation, saIntent} {
		granted = append(granted, rbactest.Review(sa, nil, authorizationv1.ResourceAttributes{
			Namespace: egressAgents, Group: "batch", Resource: "jobs", Verb: "create",
		}))
	}
	rbactest.WaitAllowed(t, admin, granted...)

	podDiffers := budgetJob("i5", "intent", "60000000")
	podDiffers.Spec.Template.Annotations[provider.BudgetAnnotation] = "90000000"
	podOnly := budgetJob("a3", "remediation", "")
	podOnly.Spec.Template.Annotations = map[string]string{provider.BudgetAnnotation: "5"}
	for _, tc := range []struct {
		name string
		user string
		job  *batchv1.Job
		want string // "" admits
	}{
		{"intent-controller, a budget", saIntent, budgetJob("i1", "intent", "60000000"), ""},
		{"intent-controller, the largest budget", saIntent, budgetJob("i2", "intent", "9999999999999999"), ""},
		{"intent-controller without a budget", saIntent, budgetJob("i3", "intent", ""), budgetLauncherDenied},
		{"intent-controller, a leading zero", saIntent, budgetJob("i4", "intent", "060000000"),
			budgetLauncherDenied},
		{"intent-controller, zero", saIntent, budgetJob("i6", "intent", "0"), budgetLauncherDenied},
		{"intent-controller, past sixteen digits", saIntent, budgetJob("i7", "intent", "10000000000000000"),
			budgetLauncherDenied},
		{"intent-controller, Job and pod disagree", saIntent, podDiffers, budgetLauncherDenied},
		{"intent-controller launching kind remediation", saIntent, budgetJob("i8", "remediation", "5"),
			budgetLauncherDenied},
		{"remediation-controller setting a budget", saRemediation, budgetJob("r1", "remediation", "5"),
			budgetOutsiderDenied},
		{"remediation-controller, no budget", saRemediation, budgetJob("r2", "remediation", ""), ""},
		{"cluster-admin forging a budget", "", budgetJob("a1", "intent", "999999999"), budgetOutsiderDenied},
		{"cluster-admin forging the pod template only", "", podOnly, budgetOutsiderDenied},
		{"cluster-admin, no budget", "", budgetJob("a2", "remediation", ""), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := admin
			if tc.user != "" {
				c = as(tc.user)
			}
			err := c.Create(ctx, tc.job, client.DryRunAll)
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("refused, want admitted: %v", err)
			case tc.want != "" && !deniedWith(err, tc.want):
				t.Errorf("got %v, want refused with %q", err, tc.want)
			}
		})
	}

	job := budgetJob("live", "intent", "60000000")
	if err := as(saIntent).Create(ctx, job); err != nil {
		t.Fatalf("create as intent-controller: %v", err)
	}
	job.Annotations["example.com/note"] = "unrelated"
	if err := admin.Update(ctx, job); err != nil {
		t.Errorf("an update leaving the budget alone was refused: %v", err)
	}
	raised := job.DeepCopy()
	raised.Annotations[provider.BudgetAnnotation] = "999999999"
	if err := admin.Update(ctx, raised); !deniedWith(err, budgetJobChangeDenied) {
		t.Errorf("raising a Job's budget: got %v, want refused", err)
	}
	cleared := job.DeepCopy()
	delete(cleared.Annotations, provider.BudgetAnnotation)
	if err := admin.Update(ctx, cleared); !deniedWith(err, budgetJobChangeDenied) {
		t.Errorf("removing the Job's budget: got %v, want refused", err)
	}
}

func checkBudgetPods(t *testing.T, admin client.Client, as func(string) client.Client) {
	ctx := t.Context()
	waitFor(t, func() error {
		err := admin.Create(ctx, budgetPod("probe", "5", nil), client.DryRunAll)
		if deniedWith(err, budgetPodDenied) {
			return nil
		}
		return &waitError{err: err, want: budgetPodDenied}
	})
	rbactest.WaitAllowed(t, admin, rbactest.Review(saJobController, nil, authorizationv1.ResourceAttributes{
		Namespace: egressAgents, Resource: "pods", Verb: "create",
	}))
	job := budgetJob("pod-owner", "intent", "60000000")
	if err := as(saIntent).Create(ctx, job); err != nil {
		t.Fatalf("create the owning Job as intent-controller: %v", err)
	}
	yes := true
	jobRef := []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID,
		Controller: &yes, BlockOwnerDeletion: &yes}}
	for _, tc := range []struct {
		name string
		user string
		pod  *corev1.Pod
		want string
	}{
		{"the Job controller, for its Job", saJobController, budgetPod("jc", "60000000", jobRef), ""},
		{"a bare pod as cluster-admin", "", budgetPod("bare", "60000000", nil), budgetPodDenied},
		{"a forged Job reference as cluster-admin", "", budgetPod("forged", "60000000", jobRef), budgetPodDenied},
		{"an unbudgeted bare pod", "", budgetPod("plain", "", nil), ""},
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

	live := budgetPod("live", "60000000", jobRef)
	if err := as(saJobController).Create(ctx, live); err != nil {
		t.Fatalf("create as the Job controller: %v", err)
	}
	raised := live.DeepCopy()
	raised.Annotations[provider.BudgetAnnotation] = "999999999"
	if err := admin.Update(ctx, raised); !deniedWith(err, budgetPodChangeDenied) {
		t.Errorf("raising a live pod's budget: got %v, want refused", err)
	}
	plain := budgetPod("plain-live", "", nil)
	if err := admin.Create(ctx, plain); err != nil {
		t.Fatalf("create an unbudgeted pod: %v", err)
	}
	viaStatus := []byte(`{"metadata":{"annotations":{"` + provider.BudgetAnnotation + `":"999999999"}}}`)
	if err := admin.Status().Patch(ctx, plain, client.RawPatch(types.MergePatchType, viaStatus)); !deniedWith(err,
		budgetPodChangeDenied) {
		t.Errorf("budgeting a pod through pods/status: got %v, want refused", err)
	}
}

func checkBudgetRuns(t *testing.T, as func(string) client.Client) {
	ctx := t.Context()
	budgeted := func(name string, microUSD int64) client.Object {
		run := attributedIntentRun(name, nil)
		run.Spec.Grant.BudgetMicroUSD = microUSD
		return run
	}
	waitFor(t, func() error {
		err := as(attrUser).Create(ctx, budgeted("probe", 5), client.DryRunAll)
		if deniedWith(err, budgetRunDenied) {
			return nil
		}
		return &waitError{err: err, want: budgetRunDenied}
	})
	for _, tc := range []struct {
		name string
		user string
		run  client.Object
		want string
	}{
		{"intent-controller granting a budget", saIntent, budgeted("i1", 60000000), ""},
		{"a user granting a budget", attrUser, budgeted("i2", 60000000), budgetRunDenied},
		{"remediation-controller granting a budget", saRemediation, budgeted("i3", 5), budgetRunDenied},
		{"a user creating an unbudgeted run", attrUser, budgeted("i4", 0), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := as(tc.user).Create(ctx, tc.run, client.DryRunAll)
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("refused, want admitted: %v", err)
			case tc.want != "" && !deniedWith(err, tc.want):
				t.Errorf("got %v, want refused with %q", err, tc.want)
			}
		})
	}
}

// budgetJob is an agent Job of run kind carrying value as its budget on the
// Job and its pod template (none when "").
func budgetJob(name, kind, value string) *batchv1.Job {
	job := attributedJob(name, kind, "")
	if value != "" {
		job.Annotations[provider.BudgetAnnotation] = value
		job.Spec.Template.Annotations[provider.BudgetAnnotation] = value
	}
	return job
}

// budgetPod is an agent pod as the Job controller stamps it, carrying value
// as its budget (none when "").
func budgetPod(name, value string, owners []metav1.OwnerReference) *corev1.Pod {
	p := attributedPod(name, "", owners)
	if value != "" {
		p.Annotations[provider.BudgetAnnotation] = value
	}
	return p
}
