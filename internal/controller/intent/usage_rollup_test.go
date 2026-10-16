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

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
	"github.com/bitwise-media-group/patchy/internal/stats"
	"github.com/bitwise-media-group/patchy/internal/usagerollup"
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
			e.gh.label(1, "patchy:approved", reviewer)
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
