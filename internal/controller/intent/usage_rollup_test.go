// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"maps"
	"slices"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/ghclient"
	"github.com/devthenet-labs/maul/internal/jobs"
	"github.com/devthenet-labs/maul/internal/stats"
	"github.com/devthenet-labs/maul/internal/usagemeter/usagemetertest"
	"github.com/devthenet-labs/maul/internal/usagerollup"
	"github.com/devthenet-labs/maul/internal/usagewire"
)

// TestUsageRollups: under mode attribute every intent run is created with
// the usage finalizer, and once settled is folded by the run controller
// into its person's user, user x project and project rollups (the plan the
// requester's, the build the approver's), each exactly once, with the
// intent in each person's top intents; the finalizer is then released.
// Under mode off no run carries it and nothing is aggregated.
func TestUsageRollups(t *testing.T) {
	for _, attributed := range []bool{true, false} {
		name := "off"
		if attributed {
			name = "attribute"
		}
		t.Run(name, func(t *testing.T) {
			p := testProject()
			p.Spec.Approvers.Logins = append(p.Spec.Approvers.Logins, reviewer)
			e := newEnv(t, p)
			e.gh.perms[reviewer] = ghclient.PermissionWrite
			if attributed {
				e.attributing()
			}
			e.runs.Usage = usagerollup.New(e.c, e.c, e.runs.Settings.Namespace, false, nil)
			intent := e.discovered()
			e.drive(intent, v1alpha1.IntentAwaitingApproval, repoImage)
			plan := e.runsOf(intent, v1alpha1.IntentStagePlan)
			if got := slices.Contains(plan[0].Finalizers, v1alpha1.FinalizerUsageRollup) ||
				meta.IsStatusConditionTrue(plan[0].Status.Conditions, v1alpha1.ConditionUsageRolledUp); got != attributed {
				t.Fatalf("plan run: finalizers %v, conditions %+v", plan[0].Finalizers, plan[0].Status.Conditions)
			}
			e.clock.Advance(time.Minute)
			e.gh.label(1, "maul:approved", reviewer)
			e.drive(intent, v1alpha1.IntentInReview, repoImage)
			e.runRuns()
			e.runRuns() // nothing is counted twice

			var list v1alpha1.UsageRollupList
			if err := e.c.List(t.Context(), &list, client.InNamespace(e.runs.Settings.Namespace)); err != nil {
				t.Fatal(err)
			}
			if !attributed {
				if len(list.Items) != 0 {
					t.Fatalf("aggregated under mode off: %d rollups", len(list.Items))
				}
				return
			}
			checkIntentRollups(t, list.Items, intent, p.Name)
			for _, run := range e.intentRuns(intent) {
				if slices.Contains(run.Finalizers, v1alpha1.FinalizerUsageRollup) {
					t.Errorf("run %s still holds the usage finalizer", run.Name)
				}
			}
		})
	}
}

// checkIntentRollups holds the rollups of one intent's plan (the
// requester's) and build (the approver's) to the design's scopes: each
// person's user and user x project scopes, the project's, the intent in the
// user scopes' top intents only.
func checkIntentRollups(t *testing.T, items []v1alpha1.UsageRollup, intent, project string) {
	t.Helper()
	byName := map[string]v1alpha1.UsageRollup{}
	for _, r := range items {
		byName[r.Name] = r
	}
	requester, approverKey := annotationOf(approver), annotationOf(reviewer)
	plan, build := map[string]int64{stats.UsageKindPlan: 1}, map[string]int64{stats.UsageKindBuild: 1}
	want := map[v1alpha1.UsageScope]map[string]int64{
		{Type: v1alpha1.UsageScopeUser, User: requester}:                            plan,
		{Type: v1alpha1.UsageScopeUser, User: approverKey}:                          build,
		{Type: v1alpha1.UsageScopeUserProject, User: requester, Project: project}:   plan,
		{Type: v1alpha1.UsageScopeUserProject, User: approverKey, Project: project}: build,
		{Type: v1alpha1.UsageScopeProject, Project: project}: {
			stats.UsageKindPlan: 1, stats.UsageKindBuild: 1},
	}
	if len(items) != len(want) {
		t.Errorf("rollups = %d, want %d", len(items), len(want))
	}
	for scope, kinds := range want {
		r, ok := byName[stats.UsageObjectName(scope)]
		if !ok {
			t.Errorf("no rollup for %+v", scope)
			continue
		}
		got, top := monthKinds(r)
		if !maps.Equal(got, kinds) {
			t.Errorf("%+v kinds = %v, want %v", scope, got, kinds)
		}
		if (scope.Type == v1alpha1.UsageScopeUser) != (len(top) == 1) ||
			(len(top) == 1 && (top[0].Intent != intent || top[0].Project != project)) {
			t.Errorf("%+v top intents = %+v", scope, top)
		}
	}
}

// monthKinds sums a rollup's months' runs by kind, and lists their top
// intents.
func monthKinds(r v1alpha1.UsageRollup) (map[string]int64, []v1alpha1.UsageTopIntent) {
	got := map[string]int64{}
	var top []v1alpha1.UsageTopIntent
	for _, m := range r.Status.Months {
		for k, v := range m.Kinds {
			got[k] += v.Runs
		}
		top = append(top, m.TopIntents...)
	}
	return got, top
}

// TestDeletedRunMeasured: a plan run deleted while its Job runs is measured
// against the broker before it is folded in, so the spend its Job already
// ran up reaches its requester's trusted totals instead of a zero reported
// run.
func TestDeletedRunMeasured(t *testing.T) {
	e := newEnv(t, testProject())
	e.attributing()
	start := e.clock.Now()
	e.jobs.statusFn = func(jobs.Spec) (jobs.Status, bool) { return jobs.Status{Created: start}, true }
	broker, meter := usagemetertest.New(t, e.clock.Now)
	broker.SetDefault(func(job string) usagewire.Response {
		return usagemetertest.Answer(job, start.Add(-time.Hour), start.Add(time.Minute), 0,
			usagemetertest.Sonnet(0))
	})
	e.runs.Meter = meter
	e.runs.Usage = usagerollup.New(e.c, e.c, e.runs.Settings.Namespace, true, nil)
	name := e.discovered()
	var plan v1alpha1.IntentRun
	for range 20 {
		e.mustIntent(name)
		e.readyRepositories(repoImage)
		e.runRuns()
		if runs := e.intentRuns(name); len(runs) == 1 && runs[0].Status.JobRef != nil {
			plan = runs[0]
			break
		}
		e.clock.Advance(time.Second)
	}
	if plan.Status.JobRef == nil || plan.Status.Phase != v1alpha1.RunRunning {
		t.Fatalf("no running plan run: %+v", plan.Status)
	}
	if err := e.c.Delete(t.Context(), &plan); err != nil {
		t.Fatal(err)
	}
	if _, err := e.runs.Reconcile(t.Context(), req(plan.Name)); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(broker.Asked(), plan.Status.JobRef.Name) {
		t.Errorf("the broker was not asked for %s: %v", plan.Status.JobRef.Name, broker.Asked())
	}
	var rollup v1alpha1.UsageRollup
	key := client.ObjectKey{Namespace: e.runs.Settings.Namespace,
		Name: stats.UsageObjectName(v1alpha1.UsageScope{Type: v1alpha1.UsageScopeUser, User: annotationOf(approver)})}
	if err := e.c.Get(t.Context(), key, &rollup); err != nil {
		t.Fatalf("requester's rollup: %v", err)
	}
	var runs, reported, cost int64
	for _, m := range rollup.Status.Months {
		runs, reported, cost = runs+m.Runs, reported+m.ReportedRuns, cost+m.CostMicroUSD
	}
	if runs != 1 || reported != 0 || cost != 3_000_000 {
		t.Errorf("runs %d, reported %d, cost %d; want one measured run of $3", runs, reported, cost)
	}
}
