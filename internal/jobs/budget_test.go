// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package jobs

import (
	"context"
	"reflect"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/bitwise-media-group/patchy/internal/provider"
)

// TestBudgetAnnotation: a run's cost budget rides on the Job and its pod
// template alike, in the grammar the broker reads back; none leaves both
// exactly as before budgets existed; a value the broker could not read is
// refused before anything is created.
func TestBudgetAnnotation(t *testing.T) {
	for _, microUSD := range []int64{1, 60000000, 9999999999999999} {
		spec := testSpec()
		spec.BudgetMicroUSD = microUSD
		job := buildJobForTest(t, testConfig(), spec)
		for where, ann := range map[string]map[string]string{
			"Job": job.Annotations, "pod template": job.Spec.Template.Annotations,
		} {
			got, ok := provider.ParseBudget(ann[provider.BudgetAnnotation])
			if !ok || got != microUSD {
				t.Errorf("%s budget annotation = %q, want %d", where, ann[provider.BudgetAnnotation], microUSD)
			}
		}
	}

	off := buildJobForTest(t, testConfig(), testSpec())
	if _, ok := off.Annotations[provider.BudgetAnnotation]; ok {
		t.Error("a Job with no budget carries the annotation")
	}
	if _, ok := off.Spec.Template.Annotations[provider.BudgetAnnotation]; ok {
		t.Error("a pod template with no budget carries the annotation")
	}
	stamped := testSpec()
	stamped.BudgetMicroUSD = 5
	on := buildJobForTest(t, testConfig(), stamped)
	delete(on.Annotations, provider.BudgetAnnotation)
	delete(on.Spec.Template.Annotations, provider.BudgetAnnotation)
	if !reflect.DeepEqual(on, off) {
		t.Error("the budget changed something else on the Job")
	}

	for _, bad := range []int64{-1, 10000000000000000} {
		cs := fake.NewClientset()
		spec := testSpec()
		spec.BudgetMicroUSD = bad
		if _, _, err := New(cs, testConfig(), nil).Create(context.Background(), spec); err == nil {
			t.Errorf("Create(budget %d) succeeded, want a refusal", bad)
		}
		if acts := cs.Actions(); len(acts) != 0 {
			t.Errorf("Create(budget %d) made %d API calls, want none", bad, len(acts))
		}
	}
}

// TestSpecDeadline: a Spec's deadline replaces the Config's for that one Job,
// on activeDeadlineSeconds and on the broker caller token's lifetime alike;
// none keeps the Config's, exactly as before.
func TestSpecDeadline(t *testing.T) {
	cfg := testConfig()
	cfg.Runners["claude"] = Runner{Image: cfg.Runners["claude"].Image, Brokered: true}
	tokenTTL := func(t *testing.T, spec Spec) int64 {
		t.Helper()
		job := buildJobForTest(t, cfg, spec)
		for _, v := range job.Spec.Template.Spec.Volumes {
			if v.Projected != nil {
				return *v.Projected.Sources[0].ServiceAccountToken.ExpirationSeconds
			}
		}
		t.Fatal("no projected caller token on a brokered Job")
		return 0
	}

	def := buildJobForTest(t, cfg, testSpec())
	if got := *def.Spec.ActiveDeadlineSeconds; got != int64(time.Hour.Seconds()) {
		t.Errorf("default deadline = %ds, want the Config's 3600s", got)
	}
	if got := tokenTTL(t, testSpec()); got != findingTokenTTL(time.Hour) {
		t.Errorf("default token TTL = %d, want %d", got, findingTokenTTL(time.Hour))
	}

	spec := testSpec()
	spec.Deadline = 4*time.Hour + 15*time.Minute
	long := buildJobForTest(t, cfg, spec)
	if got := *long.Spec.ActiveDeadlineSeconds; got != int64(spec.Deadline.Seconds()) {
		t.Errorf("per-Job deadline = %ds, want %ds", got, int64(spec.Deadline.Seconds()))
	}
	if got, want := tokenTTL(t, spec), int64((spec.Deadline + 15*time.Minute).Seconds()); got != want {
		t.Errorf("per-Job token TTL = %d, want %d (the deadline plus 15m)", got, want)
	}
}
