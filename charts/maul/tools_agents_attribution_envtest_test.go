// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package chart_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	"github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/kube"
	"github.com/devthenet-labs/maul/internal/rbactest"
)

// agentsAttributedArgs render the agents-namespace tool pod with every
// admission policy that judges intent-controller's Jobs in the agents
// namespace: attribution on (as per-user credentials need) and the broker
// counting cost (the run budgets). The desktop live gate's S2-B1 ran the
// first; either alone refused every tool Job.
var agentsAttributedArgs = slices.Concat(agentsEnvArgs, []string{
	"--set", "attribution.mode=attribute",
	"--set", "egressBroker.limits.unit=cost",
	"--set", "egressBroker.limits.costPerPod=60000000",
})

// budgetToolsDenied is the budget policy's refusal of a budgeted tool Job.
const budgetToolsDenied = "is an agents tool pod and may carry no"

// TestAgentsToolPodUnderAttributionAgainstAPIServer (S2-B1) applies the chart
// rendered with the agents tool pod, attribution and cost budgets on to a
// real API server and creates what intent-controller creates in the agents
// namespace, as intent-controller: the tool Job jobs.CreateToolPod renders
// under --attribution-mode attribute (its golden) is admitted by every
// policy at once, the Job controller's pod of it too, and the one rendered
// with attribution off is refused by the attribution policy, which shows
// the check can fail. The isolation probe's own Jobs (run.sh agents and
// run.sh evaluator, created as intent-controller) are admitted as well.
func TestAgentsToolPodUnderAttributionAgainstAPIServer(t *testing.T) {
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
	applySlotRender(t, admin, agentsAttributedArgs)
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
	rbactest.WaitAllowed(t, admin, rbactest.Review(saIntent, nil, authorizationv1.ResourceAttributes{
		Namespace: egressAgents, Group: "batch", Resource: "jobs", Verb: "create"}))
	ic := as(saIntent)
	unattributed := builderToolJob(t, "toolpod_default")
	// Wait until every policy judges: the attribution policy refuses the
	// tool Job rendered with attribution off, and the budget policy an
	// ordinary intent Job without a budget.
	waitFor(t, func() error {
		if err := ic.Create(t.Context(), unattributed.DeepCopy(), client.DryRunAll); !deniedWith(err,
			attrLauncherDenied) {
			return &waitError{err: err, want: attrLauncherDenied}
		}
		if err := ic.Create(t.Context(), attributedJob("probe", "intent", "org"), client.DryRunAll); !deniedWith(err,
			budgetLauncherDenied) {
			return &waitError{err: err, want: budgetLauncherDenied}
		}
		return nil
	})

	t.Run("tool job", func(t *testing.T) {
		admittedAs(t, ic, builderToolJob(t, "toolpod_attributed"))
		budgeted := builderToolJob(t, "toolpod_attributed")
		for _, ann := range []map[string]string{budgeted.Annotations, budgeted.Spec.Template.Annotations} {
			ann["maul.run/budget-micro-usd"] = "1000000"
		}
		refusedAs(t, ic, budgeted, budgetToolsDenied, "a tool Job carrying a budget")
	})
	t.Run("tool pod", func(t *testing.T) {
		job := builderToolJob(t, "toolpod_attributed")
		if err := ic.Create(t.Context(), job); err != nil {
			t.Fatalf("create the tool Job: %v", err)
		}
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: job.Name + "-x7k2p", Namespace: job.Namespace,
				Labels: job.Spec.Template.Labels, Annotations: job.Spec.Template.Annotations,
				OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Name,
					UID: job.UID, Controller: ptrTo(true)}}},
			Spec: job.Spec.Template.Spec,
		}
		admittedAs(t, as(saJobController), pod)
	})
	t.Run("isolation probe jobs", func(t *testing.T) {
		for _, run := range [][]string{
			{"agents", "--tool-pod", egressAgents + "/maul-tools-6f1c2d3e4b5a-x7k2p"},
			{"evaluator", "--no-node-isolation", "--agents-namespace", egressAgents},
		} {
			for _, job := range probeJobs(t, run...) {
				if job.Annotations[v1alpha1.AnnotationAttributedTo] != "org" {
					t.Errorf("run.sh %s's Job %s is not attributed to the org: %v", run[0], job.Name,
						job.Annotations)
				}
				admittedAs(t, ic, job)
			}
		}
	})
}

// probeJobs are the Jobs `run.sh <run> --print-manifests` prints.
func probeJobs(t *testing.T, args ...string) []*batchv1.Job {
	t.Helper()
	dir, err := filepath.Abs(probeDir)
	if err != nil {
		t.Fatal(err)
	}
	cmd := slices.Concat([]string{filepath.Join(dir, "run.sh"), args[0], "--print-manifests", "--image",
		probeImage}, args[1:])
	out, err := exec.CommandContext(t.Context(), "bash", cmd...).Output()
	if err != nil {
		t.Fatalf("run.sh %s --print-manifests: %v", args[0], err)
	}
	var jobs []*batchv1.Job
	for doc := range strings.SplitSeq(string(out), "---\n") {
		if !strings.Contains(doc, "kind: Job\n") {
			continue
		}
		var j batchv1.Job
		if err := yaml.UnmarshalStrict([]byte(doc), &j); err != nil {
			t.Fatalf("run.sh %s's Job: %v\n%s", args[0], err, doc)
		}
		jobs = append(jobs, &j)
	}
	if len(jobs) != 1 {
		t.Fatalf("run.sh %s printed %d Jobs, want 1:\n%s", args[0], len(jobs), out)
	}
	return jobs
}
