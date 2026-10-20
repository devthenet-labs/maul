// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package usagerollup

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/kube"
	"github.com/devthenet-labs/maul/internal/stats"
)

const ns = "maul"

var (
	clock    = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	finished = metav1.NewTime(time.Date(2026, 10, 15, 23, 30, 0, 0, time.UTC))
	alice    = &v1alpha1.Attribution{User: &v1alpha1.UserRef{Login: "alice", ID: 42},
		Reason: v1alpha1.AttributionApprover}
)

func newClient(objs ...client.Object) client.WithWatch {
	return newClientWith(interceptor.Funcs{}, objs...)
}

func newClientWith(funcs interceptor.Funcs, objs ...client.Object) client.WithWatch {
	return fake.NewClientBuilder().WithScheme(kube.Scheme()).
		WithStatusSubresource(&v1alpha1.IntentRun{}, &v1alpha1.Investigation{}, &v1alpha1.Remediation{},
			&v1alpha1.EvaluationUnit{}, &v1alpha1.UsageRollup{}).
		WithObjects(objs...).WithInterceptorFuncs(funcs).Build()
}

func aggregator(c client.Client, measuring bool) *Aggregator {
	return &Aggregator{Client: c, Reader: c, Namespace: ns, Measuring: measuring,
		Now: func() time.Time { return clock }}
}

func measured(final bool) v1alpha1.UsageSummary {
	return v1alpha1.UsageSummary{
		InputTokens: 1, CostUSD: "9", // the pod's own figure
		Source: v1alpha1.UsageSourceMeasured, Final: final,
		Measured: &v1alpha1.MeasuredUsage{InputTokens: 100, OutputTokens: 20, Models: []v1alpha1.ModelUsage{{
			Model: "claude-sonnet-5", InputTokens: 100, OutputTokens: 20, CostUSD: "0.5",
			Pricing: v1alpha1.UsagePricingList,
		}}},
	}
}

func intent() *v1alpha1.Intent {
	return &v1alpha1.Intent{
		ObjectMeta: metav1.ObjectMeta{Name: "web-7", Namespace: ns, UID: "intent-uid"},
		Spec:       v1alpha1.IntentSpec{Project: "web"},
	}
}

func intentRun(attr *v1alpha1.Attribution, phase v1alpha1.RunPhase, usage v1alpha1.UsageSummary) *v1alpha1.IntentRun {
	return &v1alpha1.IntentRun{
		ObjectMeta: metav1.ObjectMeta{
			Name: "web-7-build-r1-a1", Namespace: ns, UID: "run-uid",
			Finalizers: Finalizers(attr, v1alpha1.FinalizerJobs),
		},
		Spec: v1alpha1.IntentRunSpec{
			IntentRef: v1alpha1.ObjectReference{Name: "web-7", UID: "intent-uid"},
			Stage:     v1alpha1.IntentStageBuild, Round: 1, Attempt: 1, AttributedTo: attr,
		},
		Status: v1alpha1.IntentRunStatus{Phase: phase, Usage: usage, FinishedAt: &finished},
	}
}

func settle(t *testing.T, a *Aggregator, c client.Client, obj *v1alpha1.IntentRun) {
	t.Helper()
	if err := Settle(context.Background(), a, client.ObjectKeyFromObject(obj), IntentRuns(c)); err != nil {
		t.Fatalf("Settle: %v", err)
	}
}

func rollups(t *testing.T, c client.Client) map[v1alpha1.UsageScopeType]v1alpha1.UsageRollup {
	t.Helper()
	var list v1alpha1.UsageRollupList
	if err := c.List(context.Background(), &list); err != nil {
		t.Fatal(err)
	}
	out := map[v1alpha1.UsageScopeType]v1alpha1.UsageRollup{}
	for _, r := range list.Items {
		if _, dup := out[r.Spec.Scope.Type]; dup {
			t.Fatalf("two %s rollups", r.Spec.Scope.Type)
		}
		out[r.Spec.Scope.Type] = r
	}
	return out
}

func getRun(t *testing.T, c client.Client, name string) *v1alpha1.IntentRun {
	t.Helper()
	var run v1alpha1.IntentRun
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &run); err != nil {
		t.Fatalf("get run: %v", err)
	}
	return &run
}

func TestFinalizers(t *testing.T) {
	if got := Finalizers(nil, v1alpha1.FinalizerJobs); !slices.Equal(got, []string{v1alpha1.FinalizerJobs}) {
		t.Errorf("unattributed run: %v", got)
	}
	got := Finalizers(&v1alpha1.Attribution{Reason: v1alpha1.AttributionOrg}, v1alpha1.FinalizerJobs)
	if !slices.Equal(got, []string{v1alpha1.FinalizerJobs, v1alpha1.FinalizerUsageRollup}) {
		t.Errorf("attributed run: %v", got)
	}
}

// TestSettleIntentRun: a settled build with final usage lands once in the
// user, user x project and project scopes, by its UTC completion day, with
// its intent in the user's top intents; the run is marked and released.
func TestSettleIntentRun(t *testing.T) {
	run := intentRun(alice, v1alpha1.RunComplete, measured(true))
	c := newClient(intent(), run)
	a := aggregator(c, true)
	settle(t, a, c, run)

	user := checkBuildRollups(t, rollups(t, c))

	after := getRun(t, c, run.Name)
	if !meta.IsStatusConditionTrue(after.Status.Conditions, v1alpha1.ConditionUsageRolledUp) {
		t.Errorf("run not marked: %+v", after.Status.Conditions)
	}
	if slices.Contains(after.Finalizers, v1alpha1.FinalizerUsageRollup) ||
		!slices.Contains(after.Finalizers, v1alpha1.FinalizerJobs) {
		t.Errorf("finalizers = %v", after.Finalizers)
	}

	// Settling again is a no-op.
	settle(t, a, c, run)
	if again := rollups(t, c)[v1alpha1.UsageScopeUser]; again.Status.Days["2026-10-15"].Runs != 1 ||
		again.ResourceVersion != user.ResourceVersion {
		t.Errorf("a second Settle changed the user rollup: %+v", again.Status.Days)
	}
}

// TestSettleWaitsForFinalUsage: while the meter is on, a settled run waits
// for its usage to be final; an unsettled run always waits; with the meter
// off a settled run is due at once and counts as reported.
func TestSettleWaitsForFinalUsage(t *testing.T) {
	for _, tt := range []struct {
		name      string
		phase     v1alpha1.RunPhase
		usage     v1alpha1.UsageSummary
		measuring bool
		due       bool
		reported  bool
	}{
		{"running", v1alpha1.RunRunning, measured(true), true, false, false},
		{"settled, not final", v1alpha1.RunComplete, measured(false), true, false, false},
		{"settled, final", v1alpha1.RunFailed, measured(true), true, true, false},
		{"settled, meter off", v1alpha1.RunComplete, v1alpha1.UsageSummary{InputTokens: 5, CostUSD: "0.1"},
			false, true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			run := intentRun(alice, tt.phase, tt.usage)
			c := newClient(intent(), run)
			settle(t, aggregator(c, tt.measuring), c, run)
			got := rollups(t, c)
			if (len(got) > 0) != tt.due {
				t.Fatalf("aggregated = %v, want %v", len(got) > 0, tt.due)
			}
			marked := meta.IsStatusConditionTrue(getRun(t, c, run.Name).Status.Conditions,
				v1alpha1.ConditionUsageRolledUp)
			if marked != tt.due {
				t.Errorf("marked = %v, want %v", marked, tt.due)
			}
			if tt.due {
				day := got[v1alpha1.UsageScopeUser].Status.Days["2026-10-15"]
				if (day.ReportedRuns == 1) != tt.reported {
					t.Errorf("day = %+v, reported want %v", day, tt.reported)
				}
				if tt.reported && (day.ReportedTokens != 5 || day.ReportedCostMicroUSD != 100_000 ||
					day.CostMicroUSD != 0) {
					t.Errorf("reported day = %+v", day)
				}
			}
		})
	}
}

// TestSettleNotApplicable: a run from before the feature (no attribution,
// so no usage finalizer) is never aggregated, never marked, never held.
func TestSettleNotApplicable(t *testing.T) {
	run := intentRun(nil, v1alpha1.RunComplete, measured(true))
	if slices.Contains(run.Finalizers, v1alpha1.FinalizerUsageRollup) {
		t.Fatal("an unattributed run was given the usage finalizer")
	}
	c := newClient(intent(), run)
	before := getRun(t, c, run.Name).ResourceVersion
	settle(t, aggregator(c, true), c, run)
	if got := rollups(t, c); len(got) != 0 {
		t.Errorf("rollups = %v", got)
	}
	after := getRun(t, c, run.Name)
	if meta.FindStatusCondition(after.Status.Conditions, v1alpha1.ConditionUsageRolledUp) != nil {
		t.Error("an unattributed run was marked")
	}
	if after.ResourceVersion != before {
		t.Error("an unattributed run was written")
	}
}

// TestSettleOnDelete: a run deleted before it settled (an Intent cancelled,
// a TTL cascade) is aggregated at once with what it has and released, so the
// deletion completes; nothing is counted twice when the controller sees the
// deletion again.
func TestSettleOnDelete(t *testing.T) {
	run := intentRun(alice, v1alpha1.RunRunning, v1alpha1.UsageSummary{OutputTokens: 7})
	run.Status.FinishedAt = nil
	run.CreationTimestamp = metav1.NewTime(time.Date(2026, 10, 14, 8, 0, 0, 0, time.UTC))
	run.Finalizers = Finalizers(alice) // the jobs finalizer already released
	c := newClient(intent(), run)
	ctx := context.Background()
	if err := c.Delete(ctx, getRun(t, c, run.Name)); err != nil {
		t.Fatal(err)
	}
	a := aggregator(c, true)
	settle(t, a, c, run)
	var gone v1alpha1.IntentRun
	if err := c.Get(ctx, client.ObjectKeyFromObject(run), &gone); !kerrors.IsNotFound(err) {
		t.Fatalf("run still present (err %v, finalizers %v)", err, gone.Finalizers)
	}
	user := rollups(t, c)[v1alpha1.UsageScopeUser]
	// Bucketed by creation, the only stable time a deleted unfinished run has.
	if d := user.Status.Days["2026-10-14"]; d.Runs != 1 || d.ReportedRuns != 1 || d.ReportedTokens != 7 {
		t.Errorf("day = %+v", d)
	}
	settle(t, a, c, run) // the run is gone: a no-op
	if user2 := rollups(t, c)[v1alpha1.UsageScopeUser]; user2.ResourceVersion != user.ResourceVersion {
		t.Error("a settle after deletion wrote the rollup")
	}
}

// TestSettleExactlyOnceAcrossCrash: a crash after the rollups were written
// but before the run was marked replays without counting the run twice, in
// any scope; so does a conflict on the run's status write.
func TestSettleExactlyOnceAcrossCrash(t *testing.T) {
	run := intentRun(alice, v1alpha1.RunComplete, measured(true))
	failRunStatus := 2 // the first attempt crashes; the next conflicts
	c := newClientWith(interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object,
			opts ...client.SubResourceUpdateOption) error {
			if _, ok := obj.(*v1alpha1.IntentRun); ok && failRunStatus > 0 {
				failRunStatus--
				if failRunStatus == 1 {
					return errors.New("crash")
				}
				return kerrors.NewConflict(v1alpha1.GroupVersion.WithResource("intentruns").GroupResource(),
					obj.GetName(), errors.New("stale"))
			}
			return cl.SubResource(sub).Update(ctx, obj, opts...)
		},
	}, intent(), run)
	a := aggregator(c, true)
	if err := Settle(context.Background(), a, client.ObjectKeyFromObject(run), IntentRuns(c)); err == nil {
		t.Fatal("the crash was not reported")
	}
	settle(t, a, c, run) // replays through the conflict
	for scope, r := range rollups(t, c) {
		if r.Status.Months["2026-10"].Runs != 1 || len(r.Status.Recent) != 1 {
			t.Errorf("%s: %d runs, ledger %v", scope, r.Status.Months["2026-10"].Runs, r.Status.Recent)
		}
	}
	if !meta.IsStatusConditionTrue(getRun(t, c, run.Name).Status.Conditions, v1alpha1.ConditionUsageRolledUp) {
		t.Error("run not marked after the replay")
	}
}

// TestSettleWithoutIntent: a run whose Intent is gone, or is another
// Intent of the same name, counts for its user only.
func TestSettleWithoutIntent(t *testing.T) {
	for name, objs := range map[string][]client.Object{
		"gone": nil,
		"another intent of the name": {&v1alpha1.Intent{
			ObjectMeta: metav1.ObjectMeta{Name: "web-7", Namespace: ns, UID: "other"},
			Spec:       v1alpha1.IntentSpec{Project: "elsewhere"},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			run := intentRun(alice, v1alpha1.RunComplete, measured(true))
			c := newClient(append(objs, run)...)
			settle(t, aggregator(c, true), c, run)
			got := rollups(t, c)
			if len(got) != 1 || got[v1alpha1.UsageScopeUser].Status.Months["2026-10"].TopIntents[0].Project != "" {
				t.Errorf("rollups = %+v", got)
			}
		})
	}
}

// TestKinds: each kind describes its run: the run kind, the person or the
// org, the usage, and the completion it is bucketed by.
func TestKinds(t *testing.T) {
	ctx := context.Background()
	stage := &v1alpha1.StageResult{Outcome: "ok", Usage: measured(true), FinishedAt: &finished}
	inv := &v1alpha1.Investigation{
		ObjectMeta: metav1.ObjectMeta{UID: "i"},
		Spec:       v1alpha1.InvestigationSpec{AttributedTo: &v1alpha1.Attribution{Reason: v1alpha1.AttributionOrg}},
		Status:     v1alpha1.InvestigationStatus{Phase: v1alpha1.RunComplete, Stage: stage},
	}
	run, _ := Investigations.Run(ctx, inv)
	if run.Kind != stats.UsageKindInvestigation || !run.Settled || run.Usage == nil ||
		!run.CompletedAt.Equal(finished.Time) || stats.UsageUser(run.Attribution) != "org" {
		t.Errorf("investigation = %+v", run)
	}
	inv.Status.Stage = nil
	inv.CreationTimestamp = metav1.NewTime(clock)
	if run, _ := Investigations.Run(ctx, inv); run.Usage != nil || !run.CompletedAt.Equal(clock) {
		t.Errorf("investigation without a stage = %+v", run)
	}
	rem := &v1alpha1.Remediation{
		Spec:   v1alpha1.RemediationSpec{AttributedTo: alice},
		Status: v1alpha1.RemediationStatus{Phase: v1alpha1.RunFailed, Stage: stage},
	}
	if run, _ := Remediations.Run(ctx, rem); run.Kind != stats.UsageKindRemediation || !run.Settled ||
		stats.UsageUser(run.Attribution) != "github-id:42" {
		t.Errorf("remediation = %+v", run)
	}
	unit := &v1alpha1.EvaluationUnit{
		Spec:   v1alpha1.EvaluationUnitSpec{AttributedTo: alice},
		Status: v1alpha1.EvaluationUnitStatus{Phase: v1alpha1.RunRunning, FinishedAt: &finished},
	}
	if run, _ := EvaluationUnits.Run(ctx, unit); run.Kind != stats.UsageKindEvaluation || run.Settled ||
		run.Usage == nil {
		t.Errorf("evaluation unit = %+v", run)
	}
	for _, tt := range []struct {
		spec v1alpha1.IntentRunSpec
		want string
	}{
		{v1alpha1.IntentRunSpec{Stage: v1alpha1.IntentStagePlan}, stats.UsageKindPlan},
		{v1alpha1.IntentRunSpec{Stage: v1alpha1.IntentStageBuild}, stats.UsageKindBuild},
		{v1alpha1.IntentRunSpec{Stage: v1alpha1.IntentStageRevise, Trigger: v1alpha1.IntentRunTriggerReview},
			stats.UsageKindRevise},
		{v1alpha1.IntentRunSpec{Stage: v1alpha1.IntentStageRevise, Trigger: v1alpha1.IntentRunTriggerCommand},
			stats.UsageKindRevise},
		{v1alpha1.IntentRunSpec{Stage: v1alpha1.IntentStageRevise, Trigger: v1alpha1.IntentRunTriggerChecks},
			stats.UsageKindCheckFix},
	} {
		if got := IntentRunKind(tt.spec); got != tt.want {
			t.Errorf("IntentRunKind(%s/%s) = %s, want %s", tt.spec.Stage, tt.spec.Trigger, got, tt.want)
		}
	}
}

// TestSettleNilAggregator: a controller with no aggregator configured
// settles nothing.
func TestSettleNilAggregator(t *testing.T) {
	run := intentRun(alice, v1alpha1.RunComplete, measured(true))
	c := newClient(intent(), run)
	if err := Settle(context.Background(), nil, client.ObjectKeyFromObject(run), IntentRuns(c)); err != nil {
		t.Fatal(err)
	}
	if len(rollups(t, c)) != 0 {
		t.Error("a nil aggregator aggregated")
	}
}

// checkBuildRollups holds the three rollups of alice's one build of web-7 in
// Project web, finished 2026-10-15 UTC, and returns her user rollup.
func checkBuildRollups(t *testing.T, got map[v1alpha1.UsageScopeType]v1alpha1.UsageRollup) v1alpha1.UsageRollup {
	t.Helper()
	if len(got) != 3 {
		t.Fatalf("rollups = %d, want 3", len(got))
	}
	user := got[v1alpha1.UsageScopeUser]
	if user.Spec.Scope.User != "github-id:42" || user.Status.Login != "alice" ||
		user.Name != stats.UsageObjectName(user.Spec.Scope) ||
		user.Labels[v1alpha1.LabelUsageScope] != "user" {
		t.Errorf("user rollup = %+v", user.ObjectMeta)
	}
	day := user.Status.Days["2026-10-15"]
	if day.Runs != 1 || day.InputTokens != 100 || day.CostMicroUSD != 500_000 || day.ReportedRuns != 0 ||
		day.Kinds[stats.UsageKindBuild].Runs != 1 {
		t.Errorf("user day = %+v", day)
	}
	top := user.Status.Months["2026-10"].TopIntents
	if len(top) != 1 || top[0].Intent != "web-7" || top[0].Project != "web" {
		t.Errorf("top intents = %+v", top)
	}
	if up := got[v1alpha1.UsageScopeUserProject]; up.Spec.Scope.Project != "web" ||
		up.Status.Months["2026-10"].Runs != 1 || len(up.Status.Months["2026-10"].TopIntents) != 0 {
		t.Errorf("user-project rollup = %+v", up)
	}
	if p := got[v1alpha1.UsageScopeProject]; p.Spec.Scope.User != "" || p.Status.Login != "" ||
		p.Status.Days["2026-10-15"].CostMicroUSD != 500_000 {
		t.Errorf("project rollup = %+v", p)
	}
	return user
}

// TestSettleForbidden: a controller no longer granted UsageRollups (the
// chart grants them only under attribution.mode attribute) lets an owed run
// go, marked RollupForbidden, rather than holding its deletion forever; any
// other rollup error keeps the run held, to be retried.
func TestSettleForbidden(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		released bool
	}{
		{"forbidden", kerrors.NewForbidden(v1alpha1.GroupVersion.WithResource("usagerollups").GroupResource(),
			"x", errors.New("no grant")), true},
		{"unavailable", kerrors.NewServiceUnavailable("etcd"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := intentRun(alice, v1alpha1.RunComplete, measured(true))
			c := newClientWith(interceptor.Funcs{
				Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object,
					opts ...client.GetOption) error {
					if _, ok := obj.(*v1alpha1.UsageRollup); ok {
						return tc.err
					}
					return cl.Get(ctx, key, obj, opts...)
				},
			}, intent(), run)
			err := Settle(context.Background(), aggregator(c, true), client.ObjectKeyFromObject(run), IntentRuns(c))
			if (err == nil) != tc.released {
				t.Fatalf("Settle = %v, want released %v", err, tc.released)
			}
			after := getRun(t, c, run.Name)
			if slices.Contains(after.Finalizers, v1alpha1.FinalizerUsageRollup) == tc.released {
				t.Errorf("finalizers = %v", after.Finalizers)
			}
			cond := meta.FindStatusCondition(after.Status.Conditions, v1alpha1.ConditionUsageRolledUp)
			if tc.released && (cond == nil || cond.Reason != ReasonRollupForbidden) {
				t.Errorf("condition = %+v", cond)
			}
			if !tc.released && cond != nil {
				t.Errorf("a held run was marked: %+v", cond)
			}
		})
	}
}
