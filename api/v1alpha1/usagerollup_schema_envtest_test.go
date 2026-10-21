// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1_test

import (
	"context"
	"fmt"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	maulv1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// testUsageRollupSchema: a scope carries a user exactly for the user scopes
// and a project exactly for the project scopes, the user in the
// attributed-to grammar; the scope is immutable; and the status's day and
// month maps are bounded.
func testUsageRollupSchema(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	n := 0
	rollup := func(scope maulv1.UsageScope) *maulv1.UsageRollup {
		n++
		return &maulv1.UsageRollup{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("usage-schema-%d", n), Namespace: "default"},
			Spec:       maulv1.UsageRollupSpec{Scope: scope},
		}
	}
	for _, s := range []maulv1.UsageScope{
		{Type: maulv1.UsageScopeUser, User: "org"},
		{Type: maulv1.UsageScopeUser, User: "github-id:42"},
		{Type: maulv1.UsageScopeUserProject, User: "github-id:42", Project: "web"},
		{Type: maulv1.UsageScopeProject, Project: "web"},
	} {
		if err := c.Create(ctx, rollup(s)); err != nil {
			t.Errorf("Create(scope %+v) = %v, want nil", s, err)
		}
	}
	for _, s := range []maulv1.UsageScope{
		{Type: maulv1.UsageScopeUser},
		{Type: maulv1.UsageScopeUser, User: "org", Project: "web"},
		{Type: maulv1.UsageScopeUser, User: "github:alice"},
		{Type: maulv1.UsageScopeUser, User: "github-id:042"},
		{Type: maulv1.UsageScopeUserProject, User: "org"},
		{Type: maulv1.UsageScopeProject, User: "org", Project: "web"},
		{Type: maulv1.UsageScopeProject},
		{Type: "total"},
	} {
		if err := c.Create(ctx, rollup(s)); err == nil {
			t.Errorf("Create(scope %+v) = nil, want rejection", s)
		}
	}

	r := rollup(maulv1.UsageScope{Type: maulv1.UsageScopeUser, User: "github-id:7"})
	if err := c.Create(ctx, r); err != nil {
		t.Fatalf("Create = %v", err)
	}
	moved := r.DeepCopy()
	moved.Spec.Scope.User = "github-id:8"
	if err := c.Update(ctx, moved); err == nil {
		t.Error("Update of spec.scope = nil, want rejection (immutable)")
	}

	r.Status = maulv1.UsageRollupStatus{
		SchemaVersion: maulv1.UsageRollupSchemaVersion,
		Login:         "alice",
		Days: map[string]maulv1.UsageBucket{"2026-10-16": {
			UsageAggregate: maulv1.UsageAggregate{Runs: 2, CostMicroUSD: 5},
			Kinds:          map[string]maulv1.UsageKindTotals{"build": {Runs: 2, CostMicroUSD: 5}},
		}},
		Months: map[string]maulv1.UsageMonthBucket{"2026-10": {
			UsageBucket: maulv1.UsageBucket{UsageAggregate: maulv1.UsageAggregate{Runs: 2}},
			TopIntents:  []maulv1.UsageTopIntent{{Intent: "web-1", Project: "web", Runs: 2}},
		}},
		Recent: []string{"u:a", "u:b"},
	}
	if err := c.Status().Update(ctx, r); err != nil {
		t.Fatalf("status update = %v", err)
	}
	var got maulv1.UsageRollup
	if err := c.Get(ctx, client.ObjectKeyFromObject(r), &got); err != nil {
		t.Fatalf("Get = %v", err)
	}
	if got.Status.Days["2026-10-16"].Kinds["build"].CostMicroUSD != 5 ||
		got.Status.Months["2026-10"].TopIntents[0].Intent != "web-1" {
		t.Errorf("status round-tripped as %+v", got.Status)
	}

	tooMany := got.DeepCopy()
	for i := range maulv1.UsageRetentionDays + 1 {
		tooMany.Status.Days[fmt.Sprintf("d%04d", i)] = maulv1.UsageBucket{}
	}
	if err := c.Status().Update(ctx, tooMany); err == nil {
		t.Errorf("status with %d days = nil, want rejection", len(tooMany.Status.Days))
	}
	tooManyMonths := got.DeepCopy()
	for i := range maulv1.UsageRetentionMonths + 1 {
		tooManyMonths.Status.Months[fmt.Sprintf("m%04d", i)] = maulv1.UsageMonthBucket{}
	}
	if err := c.Status().Update(ctx, tooManyMonths); err == nil {
		t.Errorf("status with %d months = nil, want rejection", len(tooManyMonths.Status.Months))
	}
}
