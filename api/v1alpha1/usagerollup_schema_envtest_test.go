// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1_test

import (
	"context"
	"fmt"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	patchyv1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

// testUsageRollupSchema: a scope carries a user exactly for the user scopes
// and a project exactly for the project scopes, the user in the
// attributed-to grammar; the scope is immutable; and the status's day and
// month maps are bounded.
func testUsageRollupSchema(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	n := 0
	rollup := func(scope patchyv1.UsageScope) *patchyv1.UsageRollup {
		n++
		return &patchyv1.UsageRollup{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("usage-schema-%d", n), Namespace: "default"},
			Spec:       patchyv1.UsageRollupSpec{Scope: scope},
		}
	}
	for _, s := range []patchyv1.UsageScope{
		{Type: patchyv1.UsageScopeUser, User: "org"},
		{Type: patchyv1.UsageScopeUser, User: "github-id:42"},
		{Type: patchyv1.UsageScopeUserProject, User: "github-id:42", Project: "web"},
		{Type: patchyv1.UsageScopeProject, Project: "web"},
	} {
		if err := c.Create(ctx, rollup(s)); err != nil {
			t.Errorf("Create(scope %+v) = %v, want nil", s, err)
		}
	}
	for _, s := range []patchyv1.UsageScope{
		{Type: patchyv1.UsageScopeUser},
		{Type: patchyv1.UsageScopeUser, User: "org", Project: "web"},
		{Type: patchyv1.UsageScopeUser, User: "github:alice"},
		{Type: patchyv1.UsageScopeUser, User: "github-id:042"},
		{Type: patchyv1.UsageScopeUserProject, User: "org"},
		{Type: patchyv1.UsageScopeProject, User: "org", Project: "web"},
		{Type: patchyv1.UsageScopeProject},
		{Type: "total"},
	} {
		if err := c.Create(ctx, rollup(s)); err == nil {
			t.Errorf("Create(scope %+v) = nil, want rejection", s)
		}
	}

	r := rollup(patchyv1.UsageScope{Type: patchyv1.UsageScopeUser, User: "github-id:7"})
	if err := c.Create(ctx, r); err != nil {
		t.Fatalf("Create = %v", err)
	}
	moved := r.DeepCopy()
	moved.Spec.Scope.User = "github-id:8"
	if err := c.Update(ctx, moved); err == nil {
		t.Error("Update of spec.scope = nil, want rejection (immutable)")
	}

	r.Status = patchyv1.UsageRollupStatus{
		SchemaVersion: patchyv1.UsageRollupSchemaVersion,
		Login:         "alice",
		Days: map[string]patchyv1.UsageBucket{"2026-10-16": {
			UsageAggregate: patchyv1.UsageAggregate{Runs: 2, CostMicroUSD: 5},
			Kinds:          map[string]patchyv1.UsageKindTotals{"build": {Runs: 2, CostMicroUSD: 5}},
		}},
		Months: map[string]patchyv1.UsageMonthBucket{"2026-10": {
			UsageBucket: patchyv1.UsageBucket{UsageAggregate: patchyv1.UsageAggregate{Runs: 2}},
			TopIntents:  []patchyv1.UsageTopIntent{{Intent: "web-1", Project: "web", Runs: 2}},
		}},
		Recent: []string{"u:a", "u:b"},
	}
	if err := c.Status().Update(ctx, r); err != nil {
		t.Fatalf("status update = %v", err)
	}
	var got patchyv1.UsageRollup
	if err := c.Get(ctx, client.ObjectKeyFromObject(r), &got); err != nil {
		t.Fatalf("Get = %v", err)
	}
	if got.Status.Days["2026-10-16"].Kinds["build"].CostMicroUSD != 5 ||
		got.Status.Months["2026-10"].TopIntents[0].Intent != "web-1" {
		t.Errorf("status round-tripped as %+v", got.Status)
	}

	tooMany := got.DeepCopy()
	for i := range patchyv1.UsageRetentionDays + 1 {
		tooMany.Status.Days[fmt.Sprintf("d%04d", i)] = patchyv1.UsageBucket{}
	}
	if err := c.Status().Update(ctx, tooMany); err == nil {
		t.Errorf("status with %d days = nil, want rejection", len(tooMany.Status.Days))
	}
	tooManyMonths := got.DeepCopy()
	for i := range patchyv1.UsageRetentionMonths + 1 {
		tooManyMonths.Status.Months[fmt.Sprintf("m%04d", i)] = patchyv1.UsageMonthBucket{}
	}
	if err := c.Status().Update(ctx, tooManyMonths); err == nil {
		t.Errorf("status with %d months = nil, want rejection", len(tooManyMonths.Status.Months))
	}
}
