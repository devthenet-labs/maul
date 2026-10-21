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
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/kube"
	"github.com/devthenet-labs/maul/internal/rbactest"
)

// The attribution policies' refusals, each distinct so a case cannot pass
// on the wrong rule.
const (
	attrLauncherDenied  = "is launched by a maul controller and must carry"
	attrOutsiderDenied  = "only the maul controllers that launch agent Jobs may set it"
	attrJobChangeDenied = "annotation on job"
	attrPodDenied       = "only the Kubernetes Job controller may create a pod carrying it"
	attrPodChangeDenied = "annotation on pod"
	attrStampDenied     = "must name the requesting user"
	attrSubmitterDenied = "must be the requesting user"
	attrRunStampDenied  = "may only be stamped by the controller that runs it"
	attrRequesterDenied = "may only be recorded by intent-controller"
)

const (
	saEvaluation = "system:serviceaccount:maul:maul-evaluation-controller"
	saIntegrate  = "system:serviceaccount:maul:maul-integration-controller"
	attrUser     = "alice@example.com"
)

// attributionArgs renders every component the attribution policies name,
// with attribution on.
var attributionArgs = []string{
	"-f", "../../hack/testdata/chart-render/intent-controller.yaml",
	"-f", "../../hack/testdata/chart-render/evaluation-controller.yaml",
	"--set", "attribution.mode=attribute",
}

// TestAttributionPoliciesAgainstAPIServer applies the chart rendered with
// attribution on to a real API server, then drives each attribution policy
// as the identities it judges: a forged annotation is refused and the
// launching controller admitted; a pod is admitted only from the Job
// controller for its Job; nobody may change either later; a Finding stamp
// and an Evaluation submitter name their writer unless a maul controller
// writes them.
func TestAttributionPoliciesAgainstAPIServer(t *testing.T) {
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
	applyAttributionRender(t, admin)
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

	t.Run("jobs", func(t *testing.T) { checkAttributionJobs(t, admin, as) })
	t.Run("pods", func(t *testing.T) { checkAttributionPods(t, admin, as) })
	t.Run("finding stamps", func(t *testing.T) { checkFindingStamps(t, as) })
	t.Run("evaluation submitter", func(t *testing.T) { checkEvaluationSubmitter(t, as) })
	t.Run("run stamps", func(t *testing.T) { checkRunStamps(t, as) })
	t.Run("intent requester", func(t *testing.T) { checkIntentRequester(t, as) })
}

func checkAttributionJobs(t *testing.T, admin client.Client, as func(string) client.Client) {
	ctx := t.Context()
	waitFor(t, func() error {
		err := admin.Create(ctx, attributedJob("probe", "remediation", "org"), client.DryRunAll)
		if deniedWith(err, attrOutsiderDenied) {
			return nil
		}
		return &waitError{err: err, want: attrOutsiderDenied}
	})
	controllers := []string{saInvestigation, saRemediation, saIntent, saEvaluation}
	granted := make([]authorizationv1.SubjectAccessReviewSpec, 0, len(controllers))
	for _, sa := range controllers {
		granted = append(granted, rbactest.Review(sa, nil, authorizationv1.ResourceAttributes{
			Namespace: egressAgents, Group: "batch", Resource: "jobs", Verb: "create",
		}))
	}
	rbactest.WaitAllowed(t, admin, granted...)

	podDiffers := attributedJob("i5", "intent", "github-id:5")
	podDiffers.Spec.Template.Annotations[v1alpha1.AnnotationAttributedTo] = "github-id:6"
	podOnly := attributedJob("a3", "remediation", "")
	podOnly.Spec.Template.Annotations = map[string]string{v1alpha1.AnnotationAttributedTo: "org"}
	for _, tc := range []struct {
		name string
		user string
		job  *batchv1.Job
		want string // "" admits
	}{
		{"investigation-controller, org", saInvestigation, attributedJob("v1", "investigation", "org"), ""},
		{"investigation-controller naming a person", saInvestigation,
			attributedJob("v2", "investigation", "github-id:5"), attrLauncherDenied},
		{"investigation-controller without the annotation", saInvestigation,
			attributedJob("v3", "investigation", ""), attrLauncherDenied},
		{"remediation-controller, a person", saRemediation, attributedJob("r1", "remediation", "github-id:5"), ""},
		{"remediation-controller, org", saRemediation, attributedJob("r2", "remediation", "org"), ""},
		{"remediation-controller launching kind intent", saRemediation,
			attributedJob("r3", "intent", "github-id:5"), attrLauncherDenied},
		{"intent-controller, a person", saIntent, attributedJob("i1", "intent", "github-id:42"), ""},
		{"intent-controller, a login instead of an id", saIntent,
			attributedJob("i2", "intent", "github:alice"), attrLauncherDenied},
		{"intent-controller, a leading zero", saIntent, attributedJob("i3", "intent", "github-id:05"),
			attrLauncherDenied},
		{"intent-controller, Job and pod disagree", saIntent, podDiffers, attrLauncherDenied},
		{"evaluation-controller, org", saEvaluation, attributedJob("e1", "evaluation", "org"), ""},
		{"evaluation-controller launching kind remediation", saEvaluation,
			attributedJob("e2", "remediation", "org"), attrLauncherDenied},
		{"cluster-admin forging a person", "", attributedJob("a1", "remediation", "github-id:5"), attrOutsiderDenied},
		{"cluster-admin forging the pod template only", "", podOnly, attrOutsiderDenied},
		{"cluster-admin, no annotation", "", attributedJob("a2", "remediation", ""), ""},
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

	// UPDATE: an unrelated change passes; any change to the annotation is
	// refused, even from the controller that set it.
	job := attributedJob("live", "remediation", "github-id:5")
	if err := as(saRemediation).Create(ctx, job); err != nil {
		t.Fatalf("create as remediation-controller: %v", err)
	}
	job.Annotations["example.com/note"] = "unrelated"
	if err := admin.Update(ctx, job); err != nil {
		t.Errorf("an update leaving the annotation alone was refused: %v", err)
	}
	rebill := job.DeepCopy()
	rebill.Annotations[v1alpha1.AnnotationAttributedTo] = "github-id:6"
	if err := admin.Update(ctx, rebill); !deniedWith(err, attrJobChangeDenied) {
		t.Errorf("re-attributing a Job: got %v, want refused", err)
	}
	cleared := job.DeepCopy()
	delete(cleared.Annotations, v1alpha1.AnnotationAttributedTo)
	if err := admin.Update(ctx, cleared); !deniedWith(err, attrJobChangeDenied) {
		t.Errorf("removing the Job's annotation: got %v, want refused", err)
	}
}

func checkAttributionPods(t *testing.T, admin client.Client, as func(string) client.Client) {
	ctx := t.Context()
	waitFor(t, func() error {
		err := admin.Create(ctx, attributedPod("probe", "org", nil), client.DryRunAll)
		if deniedWith(err, attrPodDenied) {
			return nil
		}
		return &waitError{err: err, want: attrPodDenied}
	})
	rbactest.WaitAllowed(t, admin,
		rbactest.Review(saJobController, nil, authorizationv1.ResourceAttributes{
			Namespace: egressAgents, Resource: "pods", Verb: "create",
		}),
		rbactest.Review(saReplicaSet, nil, authorizationv1.ResourceAttributes{
			Namespace: egressAgents, Resource: "pods", Verb: "create",
		}),
	)
	job := attributedJob("pod-owner", "remediation", "github-id:5")
	if err := as(saRemediation).Create(ctx, job); err != nil {
		t.Fatalf("create the owning Job as remediation-controller: %v", err)
	}
	yes, no := true, false
	jobRef := []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID,
		Controller: &yes, BlockOwnerDeletion: &yes}}
	notController := []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID,
		Controller: &no}}
	for _, tc := range []struct {
		name string
		user string
		pod  *corev1.Pod
		want string
	}{
		{"the Job controller, for its Job", saJobController, attributedPod("jc", "github-id:5", jobRef), ""},
		{"a bare pod as cluster-admin", "", attributedPod("bare", "github-id:5", nil), attrPodDenied},
		{"a forged Job reference as cluster-admin", "", attributedPod("forged", "github-id:5", jobRef),
			attrPodDenied},
		{"the Job controller with no controlling reference", saJobController,
			attributedPod("jc-noctl", "org", notController), attrPodDenied},
		{"the ReplicaSet controller claiming the Job", saReplicaSet, attributedPod("rs", "org", jobRef),
			attrPodDenied},
		{"an unannotated bare pod", "", attributedPod("plain", "", nil), ""},
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

	live := attributedPod("live", "github-id:5", jobRef)
	if err := as(saJobController).Create(ctx, live); err != nil {
		t.Fatalf("create as the Job controller: %v", err)
	}
	live.Labels = map[string]string{"example.com/note": "unrelated"}
	if err := admin.Update(ctx, live); err != nil {
		t.Errorf("an update leaving the annotation alone was refused: %v", err)
	}
	rebill := live.DeepCopy()
	rebill.Annotations[v1alpha1.AnnotationAttributedTo] = "github-id:6"
	if err := admin.Update(ctx, rebill); !deniedWith(err, attrPodChangeDenied) {
		t.Errorf("re-attributing a live pod: got %v, want refused", err)
	}
	cleared := live.DeepCopy()
	delete(cleared.Annotations, v1alpha1.AnnotationAttributedTo)
	if err := admin.Update(ctx, cleared); !deniedWith(err, attrPodChangeDenied) {
		t.Errorf("removing a live pod's annotation: got %v, want refused", err)
	}
	plain := attributedPod("plain-live", "", nil)
	if err := admin.Create(ctx, plain); err != nil {
		t.Fatalf("create an unannotated pod: %v", err)
	}
	// A status write keeps the metadata it is sent, so it is judged too.
	viaStatus := []byte(`{"metadata":{"annotations":{"` + v1alpha1.AnnotationAttributedTo + `":"github-id:5"}}}`)
	if err := admin.Status().Patch(ctx, plain, client.RawPatch(types.MergePatchType, viaStatus)); !deniedWith(err,
		attrPodChangeDenied) {
		t.Errorf("annotating a pod through pods/status: got %v, want refused", err)
	}
}

func checkFindingStamps(t *testing.T, as func(string) client.Client) {
	ctx := t.Context()
	user, integration := as(attrUser), as(saIntegrate)
	at := metav1.NewTime(time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC))
	waitFor(t, func() error {
		f := attributedFinding("probe")
		f.Spec.Approval = &v1alpha1.Approval{By: "bob", At: at}
		err := user.Create(ctx, f, client.DryRunAll)
		if deniedWith(err, attrStampDenied) {
			return nil
		}
		return &waitError{err: err, want: attrStampDenied}
	})
	for i, tc := range []struct {
		name   string
		c      client.Client
		mutate func(*v1alpha1.Finding)
		want   string
	}{
		{"a user approving as themselves", user, func(f *v1alpha1.Finding) {
			f.Spec.Approval = &v1alpha1.Approval{By: attrUser, At: at}
		}, ""},
		{"a user approving as someone else", user, func(f *v1alpha1.Finding) {
			f.Spec.Approval = &v1alpha1.Approval{By: "bob", At: at}
		}, attrStampDenied},
		{"a user claiming a GitHub id", user, func(f *v1alpha1.Finding) {
			f.Spec.Approval = &v1alpha1.Approval{By: attrUser, ByID: 5, At: at}
		}, attrStampDenied},
		{"a user retrying as someone else", user, func(f *v1alpha1.Finding) {
			f.Spec.Retry = &v1alpha1.ActionRequest{By: "bob", At: at}
		}, attrStampDenied},
		{"a user expediting with an id", user, func(f *v1alpha1.Finding) {
			f.Spec.Expedite = &v1alpha1.ActionRequest{By: attrUser, ByID: 9, At: at}
		}, attrStampDenied},
		{"integration-controller recording a commenter's id", integration, func(f *v1alpha1.Finding) {
			f.Spec.Approval = &v1alpha1.Approval{By: "bob", ByID: 5, At: at}
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := attributedFinding("f-" + strconv.Itoa(i))
			tc.mutate(f)
			err := tc.c.Create(ctx, f, client.DryRunAll)
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("refused, want admitted: %v", err)
			case tc.want != "" && !deniedWith(err, tc.want):
				t.Errorf("got %v, want refused with %q", err, tc.want)
			}
		})
	}

	// A stamp the controllers wrote stands through a user's later write.
	f := attributedFinding("stamped")
	f.Spec.Approval = &v1alpha1.Approval{By: "bob", ByID: 5, At: at}
	if err := integration.Create(ctx, f); err != nil {
		t.Fatalf("create as integration-controller: %v", err)
	}
	f.Spec.Related = []v1alpha1.RelatedFinding{{From: f.Name, To: "other", Relationship: v1alpha1.RelationshipRelatedTo}}
	if err := user.Update(ctx, f); err != nil {
		t.Errorf("a user's write leaving the stamp alone was refused: %v", err)
	}

	// UPDATE: a user who changes a stamp already there writes as
	// themselves too, whoever wrote it first.
	own := attributedFinding("own-stamp")
	own.Spec.Approval = &v1alpha1.Approval{By: attrUser, At: at}
	own.Spec.Retry = &v1alpha1.ActionRequest{By: attrUser, At: at}
	if err := user.Create(ctx, own); err != nil {
		t.Fatalf("create as the user: %v", err)
	}
	for _, tc := range []struct {
		name   string
		target *v1alpha1.Finding
		mutate func(*v1alpha1.Finding)
		want   string
	}{
		{"re-billing the controller's approval to someone else", f, func(f *v1alpha1.Finding) {
			f.Spec.Approval.By = "carol"
		}, attrStampDenied},
		{"re-billing the controller's approval to another id", f, func(f *v1alpha1.Finding) {
			f.Spec.Approval.ByID = 6
		}, attrStampDenied},
		{"adding a GitHub id to their own approval", own, func(f *v1alpha1.Finding) {
			f.Spec.Approval.ByID = 5
		}, attrStampDenied},
		{"handing their own retry to someone else", own, func(f *v1alpha1.Finding) {
			f.Spec.Retry.By = "bob"
		}, attrStampDenied},
		{"adding an expedite as someone else", own, func(f *v1alpha1.Finding) {
			f.Spec.Expedite = &v1alpha1.ActionRequest{By: "bob", At: at}
		}, attrStampDenied},
		{"replacing the controller's approval with their own", f, func(f *v1alpha1.Finding) {
			f.Spec.Approval = &v1alpha1.Approval{By: attrUser, At: at}
		}, ""},
	} {
		t.Run("update: "+tc.name, func(t *testing.T) {
			cur := &v1alpha1.Finding{}
			if err := user.Get(ctx, client.ObjectKeyFromObject(tc.target), cur); err != nil {
				t.Fatalf("get: %v", err)
			}
			tc.mutate(cur)
			err := user.Update(ctx, cur, client.DryRunAll)
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("refused, want admitted: %v", err)
			case tc.want != "" && !deniedWith(err, tc.want):
				t.Errorf("got %v, want refused with %q", err, tc.want)
			}
		})
	}
}

// checkRunStamps: a run's spec.attributedTo is admitted on CREATE only from
// the controller of the run's own kind; a run without one is anyone's to
// create (as RBAC allows), and another controller stamping a kind it does
// not run is refused like anyone else.
func checkRunStamps(t *testing.T, as func(string) client.Client) {
	ctx := t.Context()
	user := as(attrUser)
	person := &v1alpha1.Attribution{User: &v1alpha1.UserRef{Login: "bob", ID: 5}, Reason: v1alpha1.AttributionApprover}
	waitFor(t, func() error {
		err := user.Create(ctx, attributedRemediation("probe", person), client.DryRunAll)
		if deniedWith(err, attrRunStampDenied) {
			return nil
		}
		return &waitError{err: err, want: attrRunStampDenied}
	})
	org := &v1alpha1.Attribution{Reason: v1alpha1.AttributionOrg}
	for _, tc := range []struct {
		name string
		user string
		run  client.Object
		want string
	}{
		{"remediation-controller stamping a remediation", saRemediation, attributedRemediation("r1", person), ""},
		{"a user stamping a remediation", attrUser, attributedRemediation("r2", person), attrRunStampDenied},
		{"a user stamping a remediation org", attrUser, attributedRemediation("r3", org), attrRunStampDenied},
		{"a user creating an unstamped remediation", attrUser, attributedRemediation("r4", nil), ""},
		{"intent-controller stamping a remediation", saIntent, attributedRemediation("r5", person),
			attrRunStampDenied},
		{"investigation-controller stamping an investigation", saInvestigation,
			attributedInvestigation("v1", org), ""},
		{"a user stamping an investigation", attrUser, attributedInvestigation("v2", org), attrRunStampDenied},
		{"intent-controller stamping an intent run", saIntent, attributedIntentRun("i1", person), ""},
		{"a user stamping an intent run", attrUser, attributedIntentRun("i2", person), attrRunStampDenied},
		{"remediation-controller stamping an intent run", saRemediation, attributedIntentRun("i3", person),
			attrRunStampDenied},
		{"evaluation-controller stamping a unit", saEvaluation, attributedUnit("u1", person), ""},
		{"a user stamping a unit", attrUser, attributedUnit("u2", person), attrRunStampDenied},
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

// checkIntentRequester: only intent-controller records the GitHub id of an
// Intent's requester.
func checkIntentRequester(t *testing.T, as func(string) client.Client) {
	ctx := t.Context()
	user := as(attrUser)
	waitFor(t, func() error {
		err := user.Create(ctx, attributedIntent(1, 5), client.DryRunAll)
		if deniedWith(err, attrRequesterDenied) {
			return nil
		}
		return &waitError{err: err, want: attrRequesterDenied}
	})
	if err := as(saIntent).Create(ctx, attributedIntent(2, 5), client.DryRunAll); err != nil {
		t.Errorf("intent-controller recording the requester's id was refused: %v", err)
	}
	if err := user.Create(ctx, attributedIntent(3, 0), client.DryRunAll); err != nil {
		t.Errorf("a user creating an Intent with no requester id was refused: %v", err)
	}
	if err := as(saRemediation).Create(ctx, attributedIntent(4, 5), client.DryRunAll); !deniedWith(err,
		attrRequesterDenied) {
		t.Errorf("another controller recording a requester id: got %v, want refused", err)
	}
}

func checkEvaluationSubmitter(t *testing.T, as func(string) client.Client) {
	ctx := t.Context()
	user := as(attrUser)
	waitFor(t, func() error {
		err := user.Create(ctx, attributedEvaluation("probe", "bob"), client.DryRunAll)
		if deniedWith(err, attrSubmitterDenied) {
			return nil
		}
		return &waitError{err: err, want: attrSubmitterDenied}
	})
	if err := user.Create(ctx, attributedEvaluation("own", attrUser), client.DryRunAll); err != nil {
		t.Errorf("a user submitting as themselves was refused: %v", err)
	}
	if err := as(saEvaluation).Create(ctx, attributedEvaluation("api", "github:bob"), client.DryRunAll); err != nil {
		t.Errorf("the evaluation API stamping the verified submitter was refused: %v", err)
	}
}

// applyAttributionRender renders the chart with attributionArgs and creates
// every object it holds: namespaces, then CRDs (waiting until they are
// served), then the rest.
func applyAttributionRender(t *testing.T, admin client.Client) {
	t.Helper()
	seen := applyChartRender(t, admin, attributionArgs)
	for _, name := range []string{"attribution-jobs", "attribution-pods", "attribution-finding-stamps",
		"attribution-evaluation-submitter", "attribution-run-stamps", "attribution-intent-requester"} {
		for _, kind := range []string{"ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding"} {
			if !seen[kind+"/maul-"+name] {
				t.Errorf("the attribution render has no %s maul-%s", kind, name)
			}
		}
	}
}

// applyChartRender creates every object of the chart rendered with extra
// args, namespaces then CRDs (waiting until established) then the rest, and
// returns what it created as kind/name.
func applyChartRender(t *testing.T, admin client.Client, extra []string) map[string]bool {
	t.Helper()
	ctx := t.Context()
	args := append([]string{"template", "maul", ".", "--namespace", egressRelease}, extra...)
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
		case "":
		case "Namespace":
			namespaces = append(namespaces, u)
		case "CustomResourceDefinition":
			crds = append(crds, u)
		default:
			others = append(others, u)
		}
	}
	seen := map[string]bool{}
	for _, group := range [][]*unstructured.Unstructured{namespaces, crds, others} {
		for _, u := range group {
			if err := admin.Create(ctx, u); err != nil {
				t.Fatalf("the API server refused %s %s/%s: %v", u.GetKind(), u.GetNamespace(), u.GetName(), err)
			}
			seen[u.GetKind()+"/"+u.GetName()] = true
		}
		if len(group) > 0 && group[0].GetKind() == "CustomResourceDefinition" {
			waitCRDsEstablished(t, admin, group)
		}
	}
	return seen
}

func waitCRDsEstablished(t *testing.T, admin client.Client, crds []*unstructured.Unstructured) {
	t.Helper()
	waitFor(t, func() error {
		for _, u := range crds {
			crd := &unstructured.Unstructured{}
			crd.SetGroupVersionKind(u.GroupVersionKind())
			if err := admin.Get(t.Context(), client.ObjectKey{Name: u.GetName()}, crd); err != nil {
				return err
			}
			conds, _, _ := unstructured.NestedSlice(crd.Object, "status", "conditions")
			established := false
			for _, c := range conds {
				m, _ := c.(map[string]any)
				if m["type"] == "Established" && m["status"] == "True" {
					established = true
				}
			}
			if !established {
				return &waitError{want: "CRD " + u.GetName() + " established"}
			}
		}
		return nil
	})
}

// grantAttributionTesters lets the test's user, integration-controller and
// evaluation-controller write findings and evaluations in the release
// namespace with every finding verb, so each case is decided by admission,
// never by RBAC.
func grantAttributionTesters(t *testing.T, admin client.Client) {
	t.Helper()
	ctx := t.Context()
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "attribution-tester", Namespace: egressRelease},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{v1alpha1.GroupVersion.Group},
			Resources: []string{"findings", "evaluations", "investigations", "remediations", "intentruns",
				"evaluationunits", "intents"},
			Verbs: []string{"get", "create", "update", "approve", "retry", "expedite", "suspend", "resume"},
		}},
	}
	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "attribution-tester", Namespace: egressRelease},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: role.Name},
		Subjects: []rbacv1.Subject{
			{Kind: "User", APIGroup: "rbac.authorization.k8s.io", Name: attrUser},
			{Kind: "ServiceAccount", Name: "maul-integration-controller", Namespace: egressRelease},
			{Kind: "ServiceAccount", Name: "maul-evaluation-controller", Namespace: egressRelease},
			{Kind: "ServiceAccount", Name: "maul-investigation-controller", Namespace: egressRelease},
			{Kind: "ServiceAccount", Name: "maul-remediation-controller", Namespace: egressRelease},
			{Kind: "ServiceAccount", Name: "maul-intent-controller", Namespace: egressRelease},
		},
	}
	for _, obj := range []client.Object{role, binding} {
		if err := admin.Create(ctx, obj); err != nil {
			t.Fatalf("create %T: %v", obj, err)
		}
	}
	reviews := make([]authorizationv1.SubjectAccessReviewSpec, 0, 18)
	for _, user := range []string{attrUser, saIntegrate, saEvaluation} {
		for _, resource := range []string{"findings", "evaluations"} {
			reviews = append(reviews, rbactest.Verbs(user, nil, authorizationv1.ResourceAttributes{
				Namespace: egressRelease, Group: v1alpha1.GroupVersion.Group, Resource: resource,
			}, "create", "update", "approve")...)
		}
	}
	for _, user := range []string{attrUser, saInvestigation, saRemediation, saIntent, saEvaluation} {
		for _, resource := range []string{"investigations", "remediations", "intentruns", "evaluationunits",
			"intents"} {
			reviews = append(reviews, rbactest.Review(user, nil, authorizationv1.ResourceAttributes{
				Namespace: egressRelease, Group: v1alpha1.GroupVersion.Group, Resource: resource, Verb: "create",
			}))
		}
	}
	rbactest.WaitAllowed(t, admin, reviews...)
}

// attributedJob is an agent Job of run kind, carrying value on the Job and
// its pod template (none when "").
func attributedJob(name, kind, value string) *batchv1.Job {
	labels := map[string]string{
		"app.kubernetes.io/name":       "maul-agent",
		"app.kubernetes.io/managed-by": "maul",
		egressKind:                     kind,
	}
	ann, podAnn := map[string]string{}, map[string]string{}
	if value != "" {
		ann[v1alpha1.AnnotationAttributedTo] = value
		podAnn[v1alpha1.AnnotationAttributedTo] = value
	}
	podLabels := map[string]string{}
	for k, v := range labels {
		podLabels[k] = v
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: egressAgents, Labels: labels, Annotations: ann},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: podLabels, Annotations: podAnn},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers:    []corev1.Container{{Name: "agent", Image: "example.com/agent:v1"}},
				},
			},
		},
	}
}

// attributedPod is an agent pod as the Job controller stamps it, carrying
// value (none when "") and the given owner references; restricted, as the
// agent namespace's Pod Security level requires.
func attributedPod(name, value string, owners []metav1.OwnerReference) *corev1.Pod {
	p := unlabelledPod(profilePod(name, owners))
	p.Annotations = map[string]string{}
	if value != "" {
		p.Annotations[v1alpha1.AnnotationAttributedTo] = value
	}
	return p
}

func attributedFinding(name string) *v1alpha1.Finding {
	return &v1alpha1.Finding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: egressRelease},
		Spec: v1alpha1.FindingSpec{
			IntegrationRef: v1alpha1.LocalObjectReference{Name: "gh"},
			Source:         "github-code-scanning",
			Advisories:     []string{"CVE-2026-0001"},
		},
	}
}

func attributedEvaluation(name, submitter string) *v1alpha1.Evaluation {
	return &v1alpha1.Evaluation{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: egressRelease},
		Spec: v1alpha1.EvaluationSpec{
			Submitter: submitter,
			Units: []v1alpha1.UnitPlan{{
				Skill: "workflow-commit", Tier: 2, Model: "anthropic/claude-sonnet-5",
				Harnesses: []v1alpha1.HarnessOption{{Harness: "claude", ModelID: "claude-sonnet-5"}},
				Workspace: v1alpha1.WorkspaceRef{
					Digest:    "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
					SizeBytes: 1024,
				},
			}},
		},
	}
}

func attributedRemediation(name string, a *v1alpha1.Attribution) *v1alpha1.Remediation {
	return &v1alpha1.Remediation{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: egressRelease},
		Spec: v1alpha1.RemediationSpec{
			FindingRef:       v1alpha1.ObjectReference{Name: "attr"},
			InvestigationRef: v1alpha1.ObjectReference{Name: "attr-inv-1"},
			RepositoryRef:    v1alpha1.LocalObjectReference{Name: "attr-src"},
			Attempt:          1, AttributedTo: a,
		},
	}
}

func attributedInvestigation(name string, a *v1alpha1.Attribution) *v1alpha1.Investigation {
	return &v1alpha1.Investigation{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: egressRelease},
		Spec: v1alpha1.InvestigationSpec{FindingRef: v1alpha1.ObjectReference{Name: "attr"}, Attempt: 1,
			AttributedTo: a},
	}
}

func attributedIntentRun(name string, a *v1alpha1.Attribution) *v1alpha1.IntentRun {
	digest := "sha256:" + strings.Repeat("a", 64)
	return &v1alpha1.IntentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "target-1-plan-r1-" + name, Namespace: egressRelease},
		Spec: v1alpha1.IntentRunSpec{
			IntentRef: v1alpha1.ObjectReference{Name: "target-1", UID: "u-intent"},
			Stage:     v1alpha1.IntentStagePlan,
			Repository: v1alpha1.IntentRunRepository{
				URL:           "https://github.com/acme/shop",
				RepositoryRef: v1alpha1.LocalObjectReference{Name: name + "-src"},
			},
			Round:        1,
			Attempt:      1,
			Inputs:       v1alpha1.IntentRunInputs{ConfigMap: name + "-input", InputDigest: digest},
			Grant:        v1alpha1.IntentRunGrant{MaxTurns: 40, TokenBudget: 200000, TimeoutMilliseconds: 1200000},
			AttributedTo: a,
		},
	}
}

func attributedUnit(name string, a *v1alpha1.Attribution) *v1alpha1.EvaluationUnit {
	return &v1alpha1.EvaluationUnit{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: egressRelease},
		Spec: v1alpha1.EvaluationUnitSpec{
			EvaluationRef: v1alpha1.ObjectReference{Name: "attr"},
			Unit:          attributedEvaluation("unit", "x").Spec.Units[0],
			AttributedTo:  a,
		},
	}
}

// attributedIntent is the Intent for issue n of Project target, its
// requester carrying GitHub id id (none when 0).
func attributedIntent(n, id int64) *v1alpha1.Intent {
	return &v1alpha1.Intent{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.IntentName("target", n), Namespace: egressRelease},
		Spec: v1alpha1.IntentSpec{
			Project: "target",
			Issue: v1alpha1.IntentIssue{Repository: "https://github.com/acme/intents", Number: n,
				URL: "https://github.com/acme/intents/issues/" + strconv.FormatInt(n, 10)},
			RequestedBy: v1alpha1.IntentRequest{Login: "octocat", UserID: id, EventID: 42,
				At: metav1.NewTime(time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC))},
		},
	}
}
