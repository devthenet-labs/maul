// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package e2e

import (
	"context"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/stats"
	"github.com/bitwise-media-group/patchy/internal/templates"
)

// TestIntentAttribution runs one intent through the shipped intent-controller
// started as the chart renders attribution.mode attribute. The approver who
// filed it is the plan's requester and, having approved it, the build's
// approver, each by GitHub id on the run's spec; the plan and build Jobs and
// their pods carry that id in the attributed-to annotation; and the issue's
// status comment names the approver, as code, beside the cost.
func TestIntentAttribution(t *testing.T) {
	e := startIntents(t, "--attribution-mode", "attribute")
	number, name := e.fileIntent(t)
	in := e.waitPhase(t, name, v1alpha1.IntentAwaitingApproval)
	want := "github-id:" + strconv.FormatInt(approver.ID, 10)

	planRun := v1alpha1.IntentRunName(name, v1alpha1.IntentStagePlan, 1, "", 1)
	checkAttributed(t, e.kubelet.waitRun(t, "the plan job to run", phaseIs("plan")), e.run(t, planRun),
		v1alpha1.AttributionRequester, want)

	afterSecond(in.Status.Plan.PostedAt.Time)
	e.gh.LabelIssue(number, approveLabel, approver)
	in = e.waitPhase(t, name, v1alpha1.IntentInReview)
	if ap := in.Status.Approval; ap == nil || ap.ByID != approver.ID {
		t.Errorf("approval = %+v, want the approver's GitHub id %d", ap, approver.ID)
	}
	buildRun := v1alpha1.IntentRunName(name, v1alpha1.IntentStageBuild, 1, appRepo, 1)
	checkAttributed(t, e.kubelet.waitRun(t, "the build job to run", phaseIs("build")), e.run(t, buildRun),
		v1alpha1.AttributionApprover, want)

	// Each settled run's usage is folded, by intent-controller itself, into
	// the approver's durable rollup, once, and the run then lets go of its
	// usage finalizer.
	scope := v1alpha1.UsageScope{Type: v1alpha1.UsageScopeUser, User: want}
	eventually(t, "the plan and the build to be rolled up for the approver", func() bool {
		var r v1alpha1.UsageRollup
		key := types.NamespacedName{Namespace: namespace, Name: stats.UsageObjectName(scope)}
		if err := e.cl.client.Get(context.Background(), key, &r); err != nil {
			return false
		}
		kinds := map[string]int64{}
		for _, m := range r.Status.Months {
			for k, v := range m.Kinds {
				kinds[k] += v.Runs
			}
		}
		return kinds[stats.UsageKindPlan] == 1 && kinds[stats.UsageKindBuild] == 1 && r.Status.Login == approver.Login
	})
	for _, run := range []string{planRun, buildRun} {
		eventually(t, "run "+run+" to release its usage finalizer", func() bool {
			return !slices.Contains(e.run(t, run).Finalizers, v1alpha1.FinalizerUsageRollup)
		})
	}

	line := "**Attributed to:** `@" + approver.Login + "` (`approver`), for the latest run"
	eventually(t, "the status comment to name the build's approver", func() bool {
		st := e.own(number, templates.IntentStatusMarker(namespace, name))
		return len(st) == 1 && strings.Contains(st[0].Body, line)
	})
}

// checkAttributed asserts that run is attributed to the approver for reason,
// and that its Job and the Job's pod template both carry value.
func checkAttributed(t *testing.T, r agentRun, run *v1alpha1.IntentRun, reason v1alpha1.AttributionReason,
	value string) {
	t.Helper()
	want := &v1alpha1.Attribution{User: &v1alpha1.UserRef{Login: approver.Login, ID: approver.ID}, Reason: reason}
	if !reflect.DeepEqual(run.Spec.AttributedTo, want) {
		t.Errorf("run %s attributed to %+v, want %+v", run.Name, run.Spec.AttributedTo, want)
	}
	if got := r.Job.Annotations[v1alpha1.AnnotationAttributedTo]; got != value {
		t.Errorf("job %s attributed to %q, want %q", r.Job.Name, got, value)
	}
	if got := r.Job.Spec.Template.Annotations[v1alpha1.AnnotationAttributedTo]; got != value {
		t.Errorf("job %s pod attributed to %q, want %q", r.Job.Name, got, value)
	}
}

// checkUnattributed asserts that, with attribution off, run carries no stamp
// and neither its Job nor the Job's pod template carries the annotation.
func checkUnattributed(t *testing.T, r agentRun, run *v1alpha1.IntentRun) {
	t.Helper()
	if run.Spec.AttributedTo != nil {
		t.Errorf("run %s attributed to %+v with attribution off", run.Name, run.Spec.AttributedTo)
	}
	if _, ok := r.Job.Annotations[v1alpha1.AnnotationAttributedTo]; ok {
		t.Errorf("job %s carries the attributed-to annotation with attribution off", r.Job.Name)
	}
	if _, ok := r.Job.Spec.Template.Annotations[v1alpha1.AnnotationAttributedTo]; ok {
		t.Errorf("job %s pod carries the attributed-to annotation with attribution off", r.Job.Name)
	}
}
