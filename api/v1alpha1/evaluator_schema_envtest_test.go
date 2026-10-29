// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	maulv1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// The evaluator's schema (SPEC S1-01): approved outcomes beside the kept
// criteria, saved checks by reference on the Intent and inline (each at most
// MaxSavedCheckBytes) on a PreviewVerification, the tool pod, ledger and
// capture summaries on its status, per-outcome results and the hand-off on
// the Intent, the evaluate run's record and the Project's evaluator settings.
// They run inside TestSchemaValidation's single envtest boot.

// schemaOutcomes is one outcome of each kind, every field set on the first.
func schemaOutcomes() []maulv1.AcceptanceOutcome {
	return []maulv1.AcceptanceOutcome{
		{ID: "winner-rises", Outcome: "Rating a battle moves the winner above the loser on the leaderboard.",
			From: "ratings should update after every battle", Kind: maulv1.OutcomeKindChange},
		{ID: "leaderboard-still-loads", Outcome: "The leaderboard page still lists every player.",
			Kind: maulv1.OutcomeKindKeep},
		{ID: "sounds-good", Outcome: "It sounds good.", Manual: true, Reason: "only a human can judge how it sounds"},
	}
}

// schemaSavedChecks is one saved check of each form.
func schemaSavedChecks() []maulv1.SavedCheck {
	digest := func(c string) string { return "sha256:" + strings.Repeat(c, 64) }
	return []maulv1.SavedCheck{
		{ID: "winner-rises.order", Outcome: "winner-rises", Form: maulv1.CheckFormHTTP,
			Body: `{"id":"winner-rises.order","outcome":"winner-rises","form":"http"}`, Digest: digest("1")},
		{ID: "leaderboard-still-loads", Outcome: "leaderboard-still-loads", Form: maulv1.CheckFormBrowser,
			Body: `{"form":"browser"}`, Digest: digest("2")},
		{ID: "winner-rises.cli", Outcome: "winner-rises", Form: maulv1.CheckFormExec,
			Body: `{"form":"exec"}`, Digest: digest("3")},
	}
}

// schemaOutcomeVerification is a valid outcome PreviewVerification.
func schemaOutcomeVerification(name string) *maulv1.PreviewVerification {
	v := schemaVerification(name)
	v.Spec.Criteria, v.Spec.CriteriaDigest = nil, ""
	v.Spec.Outcomes, v.Spec.OutcomesDigest = schemaOutcomes(), schemaDigest
	v.Spec.Checks, v.Spec.ChecksDigest = schemaSavedChecks(), schemaDigest
	v.Spec.Rung = maulv1.RungFull
	v.Spec.Tools = &maulv1.ToolsSpec{Browser: true, Sandbox: true, MaxToolCalls: 300, LookAroundRoutes: 8}
	return v
}

// fullEvaluatorStatus populates every PreviewVerification status field the
// evaluator adds.
func fullEvaluatorStatus() maulv1.PreviewVerificationStatus {
	sha := strings.Repeat("a", 64)
	check := "sha256:" + strings.Repeat("1", 64)
	return maulv1.PreviewVerificationStatus{
		Phase: maulv1.VerificationObserved, Reason: maulv1.VerificationReasonOK,
		StartedAt: schemaNow.DeepCopy(), CompletedAt: schemaNow.DeepCopy(),
		Tools: &maulv1.ToolsStatus{PodIP: "10.42.0.17", Port: 8090, Ready: true},
		Ledger: &maulv1.LedgerSummary{Count: 2, SHA256: sha, ConfigMap: "target-1-v1-a1-ledger",
			Evidence: "target-1-v1-a1-evidence",
			Entries: []maulv1.LedgerIndexEntry{
				{Seq: 1, Kind: maulv1.LedgerKindRelay, Verb: maulv1.LedgerVerbCheckRun, ResultSHA256: sha},
				{Seq: 2, Kind: maulv1.LedgerKindAgent, Verb: maulv1.LedgerVerbBrowser, Exit: 1, ResultSHA256: sha,
					Artifacts: []string{sha}, Refused: true},
			}},
		Replay: []maulv1.CheckRun{{CheckDigest: check, Result: maulv1.CheckRunPass, Token: "v1a1-0a1b2c", Seq: 1}},
		Confirmations: []maulv1.CheckRun{{CheckDigest: check, Result: maulv1.CheckRunQuarantined,
			Token: "c-9f8e7d", Seq: 3, DryRunSeq: 2}},
		Confirmed: schemaSavedChecks()[:1],
		Capture: &maulv1.CaptureSummary{Components: []maulv1.ComponentCapture{
			{Name: "api", Bytes: 65536, Truncated: true}, {Name: "web", Bytes: 12}}, Events: 7},
		Environment: []maulv1.PartFact{{Name: "tools", Role: maulv1.PreviewRoleToolPod, Ready: true}},
	}
}

// fullEvaluatorVerification populates every Intent verification field the
// evaluator adds.
func fullEvaluatorVerification() *maulv1.IntentVerification {
	check := "sha256:" + strings.Repeat("1", 64)
	return &maulv1.IntentVerification{
		State: maulv1.VerificationUnconfirmed, Round: 1, Attempt: 1,
		Outcomes: []maulv1.OutcomeResult{
			{ID: "winner-rises", Result: maulv1.OutcomePassed, Basis: check,
				Line: "Check: POST /api/battles, GET /api/leaderboard; asserts the winner is listed above the loser."},
			{ID: "leaderboard-still-loads", Result: maulv1.OutcomeUnsure, Reason: "the evaluator disputed the check"},
			{ID: "sounds-good", Result: maulv1.OutcomeAccepted},
		},
		Anomalies: []maulv1.AnomalyRecord{{Summary: "a console error on every load", Route: "/battle",
			Ledger: []int32{4, 7}}},
		Handoff: &maulv1.HandoffState{Mode: maulv1.HandoffGated, Released: true, ReleasedAt: schemaNow.DeepCopy(),
			Reason: maulv1.HandoffReasonUnconfirmed},
		FirstGreenAt:           schemaNow.DeepCopy(),
		EvaluationCostMicroUSD: 1500000,
		Disputed:               []string{check},
	}
}

func testEvaluatorSchema(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	testEvaluatorProjectSchema(ctx, t, c)
	testOutcomeAcceptanceSchema(ctx, t, c)
	testOutcomeVerificationSchema(ctx, t, c)
	testEvaluateRunSchema(ctx, t, c)
}

// testEvaluatorProjectSchema: the evaluator settings are optional, never
// defaulted into a stored Project, and bounded.
func testEvaluatorProjectSchema(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	t.Run("evaluator settings are not defaulted", func(t *testing.T) {
		p := schemaProject("proj-eval-unset")
		p.Spec.Verification = &maulv1.ProjectVerification{Enabled: true}
		if err := c.Create(ctx, p); err != nil {
			t.Fatalf("Create(project) = %v", err)
		}
		got := &maulv1.Project{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(p), got); err != nil {
			t.Fatal(err)
		}
		v := got.Spec.Verification
		if v.Handoff != "" || v.HandoffMax != nil || v.MaxToolCalls != nil || got.Spec.Surfaces != nil ||
			got.Spec.Limits.MaxEvaluateCostMicroUSD != nil {
			t.Errorf("verification %+v, surfaces %+v: want nothing defaulted", v, got.Spec.Surfaces)
		}
		if got.Spec.HandoffMax() != maulv1.DefaultHandoffMax || got.Spec.MaxToolCalls() != maulv1.DefaultMaxToolCalls ||
			got.Spec.Limits.MaxEvaluateCostLimit() != maulv1.DefaultMaxEvaluateCostMicroUSD {
			t.Error("unset evaluator settings do not read as their defaults")
		}
	})
	t.Run("evaluator settings round-trip", func(t *testing.T) {
		p := schemaProject("proj-eval-set")
		calls, cost := int32(300), int64(6000000)
		p.Spec.Verification = &maulv1.ProjectVerification{Enabled: true, Handoff: maulv1.HandoffEarly,
			HandoffMax: &metav1.Duration{Duration: 4 * time.Hour}, MaxToolCalls: &calls}
		p.Spec.Surfaces = &maulv1.ProjectSurfaces{Browser: &maulv1.BrowserSurface{Entry: "/battle"}}
		p.Spec.Limits.MaxEvaluateCostMicroUSD = &cost
		want := p.Spec.DeepCopy()
		if err := c.Create(ctx, p); err != nil {
			t.Fatalf("Create(project) = %v", err)
		}
		got := &maulv1.Project{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(p), got); err != nil {
			t.Fatal(err)
		}
		sameJSON(t, "verification", got.Spec.Verification, want.Verification)
		sameJSON(t, "surfaces", got.Spec.Surfaces, want.Surfaces)
		if got.Spec.HandoffMax() != 4*time.Hour || got.Spec.MaxToolCalls() != 300 ||
			got.Spec.Limits.MaxEvaluateCostLimit() != cost {
			t.Error("set evaluator settings do not read back")
		}
	})
	n := func(v int32) *int32 { return &v }
	d := func(v time.Duration) *metav1.Duration { return &metav1.Duration{Duration: v} }
	verify := func(s *maulv1.ProjectSpec) *maulv1.ProjectVerification {
		s.Verification = &maulv1.ProjectVerification{Enabled: true}
		return s.Verification
	}
	for _, tt := range []struct {
		name    string
		mutate  func(*maulv1.ProjectSpec)
		wantErr bool
	}{
		{"an unknown hand-off", func(s *maulv1.ProjectSpec) { verify(s).Handoff = "late" }, true},
		{"a gated hand-off", func(s *maulv1.ProjectSpec) { verify(s).Handoff = maulv1.HandoffGated }, false},
		{"a hand-off bound under 10m", func(s *maulv1.ProjectSpec) { verify(s).HandoffMax = d(9 * time.Minute) }, true},
		{"a hand-off bound of 10m", func(s *maulv1.ProjectSpec) { verify(s).HandoffMax = d(10 * time.Minute) }, false},
		{"a hand-off bound over 4h", func(s *maulv1.ProjectSpec) {
			verify(s).HandoffMax = d(4*time.Hour + time.Minute)
		}, true},
		{"49 tool calls", func(s *maulv1.ProjectSpec) { verify(s).MaxToolCalls = n(49) }, true},
		{"50 tool calls", func(s *maulv1.ProjectSpec) { verify(s).MaxToolCalls = n(50) }, false},
		{"301 tool calls", func(s *maulv1.ProjectSpec) { verify(s).MaxToolCalls = n(301) }, true},
		{"a zero evaluation ceiling", func(s *maulv1.ProjectSpec) {
			zero := int64(0)
			s.Limits.MaxEvaluateCostMicroUSD = &zero
		}, true},
		{"an evaluation ceiling over $1000", func(s *maulv1.ProjectSpec) {
			over := int64(1000000001)
			s.Limits.MaxEvaluateCostMicroUSD = &over
		}, true},
		{"a relative browser entry", func(s *maulv1.ProjectSpec) {
			s.Surfaces = &maulv1.ProjectSurfaces{Browser: &maulv1.BrowserSurface{Entry: "battle"}}
		}, true},
		{"a browser entry on another host", func(s *maulv1.ProjectSpec) {
			s.Surfaces = &maulv1.ProjectSurfaces{Browser: &maulv1.BrowserSurface{Entry: "//evil.example/"}}
		}, true},
		{"a browser surface at the root", func(s *maulv1.ProjectSpec) {
			s.Surfaces = &maulv1.ProjectSurfaces{Browser: &maulv1.BrowserSurface{Entry: "/"}}
		}, false},
	} {
		t.Run("create with "+tt.name, func(t *testing.T) {
			p := schemaProject("pe-" + shortHash(tt.name))
			tt.mutate(&p.Spec)
			err := c.Create(ctx, p)
			if (err != nil) != tt.wantErr {
				t.Errorf("Create(project with %s) = %v, wantErr %v", tt.name, err, tt.wantErr)
			}
		})
	}
}

// testOutcomeAcceptanceSchema: an Intent's approved outcomes, its check
// store and review cache, and the evaluator's results on its verification.
func testOutcomeAcceptanceSchema(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	key := client.ObjectKey{Name: "target-90", Namespace: "default"}
	if err := c.Create(ctx, schemaIntent(90)); err != nil {
		t.Fatalf("Create(intent) = %v", err)
	}
	fresh := func(t *testing.T) *maulv1.Intent {
		t.Helper()
		i := &maulv1.Intent{}
		if err := c.Get(ctx, key, i); err != nil {
			t.Fatal(err)
		}
		return i
	}
	full := func() maulv1.IntentStatus {
		s := fullIntentStatus()
		s.Acceptance = &maulv1.IntentAcceptance{PlanRevision: 1, Digest: schemaDigest,
			Outcomes: schemaOutcomes(), OutcomesDigest: schemaDigest,
			Checks: &maulv1.CheckStoreRef{ConfigMap: "target-90-checks-r1", Digest: schemaDigest, Count: 3},
			Reviews: []maulv1.CheckReviewRecord{{CheckDigest: schemaDigest, OutcomeDigest: schemaDigest,
				Answer: maulv1.ReviewProves, Run: "target-90-cr-r1-a1"}},
		}
		s.Verification = fullEvaluatorVerification()
		return s
	}
	t.Run("an outcome acceptance and evaluator results round-trip", func(t *testing.T) {
		i := fresh(t)
		i.Status = full()
		if err := c.Status().Update(ctx, i); err != nil {
			t.Fatalf("Status().Update(outcome acceptance) = %v", err)
		}
		sameJSON(t, "intent status", fresh(t).Status, full())
	})
	t.Run("the evaluating state is admitted", func(t *testing.T) {
		i := fresh(t)
		i.Status = full()
		i.Status.Verification.State = maulv1.VerificationEvaluating
		if err := c.Status().Update(ctx, i); err != nil {
			t.Errorf("Status().Update(Evaluating) = %v", err)
		}
	})
	for _, tt := range []struct {
		name   string
		mutate func(*maulv1.IntentStatus)
	}{
		{"both criteria and outcomes", func(s *maulv1.IntentStatus) { s.Acceptance.Criteria = schemaCriteria() }},
		{"outcomes without their digest", func(s *maulv1.IntentStatus) { s.Acceptance.OutcomesDigest = "" }},
		{"an outcomes digest without outcomes", func(s *maulv1.IntentStatus) { s.Acceptance.Outcomes = nil }},
		{"21 outcomes", func(s *maulv1.IntentStatus) {
			for i := len(s.Acceptance.Outcomes); i < 21; i++ {
				o := s.Acceptance.Outcomes[0]
				o.ID = fmt.Sprintf("o-%d", i)
				s.Acceptance.Outcomes = append(s.Acceptance.Outcomes, o)
			}
		}},
		{"a repeated outcome id", func(s *maulv1.IntentStatus) {
			s.Acceptance.Outcomes = append(s.Acceptance.Outcomes, s.Acceptance.Outcomes[0])
		}},
		{"an outcome over 500 characters", func(s *maulv1.IntentStatus) {
			s.Acceptance.Outcomes[0].Outcome = strings.Repeat("o", 501)
		}},
		{"an empty outcome", func(s *maulv1.IntentStatus) { s.Acceptance.Outcomes[0].Outcome = "" }},
		{"a two-line outcome", func(s *maulv1.IntentStatus) { s.Acceptance.Outcomes[0].Outcome = "a\nb" }},
		{"a from over 300 characters", func(s *maulv1.IntentStatus) {
			s.Acceptance.Outcomes[0].From = strings.Repeat("f", 301)
		}},
		{"an unknown outcome kind", func(s *maulv1.IntentStatus) { s.Acceptance.Outcomes[0].Kind = "remove" }},
		{"a malformed outcome id", func(s *maulv1.IntentStatus) { s.Acceptance.Outcomes[0].ID = "Winner" }},
		{"41 stored checks", func(s *maulv1.IntentStatus) { s.Acceptance.Checks.Count = 41 }},
		{"a check store without its digest", func(s *maulv1.IntentStatus) { s.Acceptance.Checks.Digest = "" }},
		{"an unknown review answer", func(s *maulv1.IntentStatus) { s.Acceptance.Reviews[0].Answer = "maybe" }},
		{"81 reviews", func(s *maulv1.IntentStatus) {
			for len(s.Acceptance.Reviews) < 81 {
				s.Acceptance.Reviews = append(s.Acceptance.Reviews, s.Acceptance.Reviews[0])
			}
		}},
		{"an unknown outcome result", func(s *maulv1.IntentStatus) { s.Verification.Outcomes[0].Result = "pass" }},
		{"a basis that is not a digest", func(s *maulv1.IntentStatus) { s.Verification.Outcomes[0].Basis = "e21" }},
		{"21 outcome results", func(s *maulv1.IntentStatus) {
			for len(s.Verification.Outcomes) < 21 {
				s.Verification.Outcomes = append(s.Verification.Outcomes, s.Verification.Outcomes[0])
			}
		}},
		{"11 anomalies", func(s *maulv1.IntentStatus) {
			for len(s.Verification.Anomalies) < 11 {
				s.Verification.Anomalies = append(s.Verification.Anomalies, s.Verification.Anomalies[0])
			}
		}},
		{"an anomaly route over 300 characters", func(s *maulv1.IntentStatus) {
			s.Verification.Anomalies[0].Route = strings.Repeat("r", 301)
		}},
		{"an unknown hand-off mode", func(s *maulv1.IntentStatus) { s.Verification.Handoff.Mode = "late" }},
		{"an unknown hand-off reason", func(s *maulv1.IntentStatus) { s.Verification.Handoff.Reason = "because" }},
		{"a release without its time", func(s *maulv1.IntentStatus) { s.Verification.Handoff.ReleasedAt = nil }},
		{"a release without its reason", func(s *maulv1.IntentStatus) { s.Verification.Handoff.Reason = "" }},
		{"a negative evaluation cost", func(s *maulv1.IntentStatus) { s.Verification.EvaluationCostMicroUSD = -1 }},
		{"a disputed entry that is not a digest", func(s *maulv1.IntentStatus) {
			s.Verification.Disputed = []string{"winner-rises.order"}
		}},
		{"an unknown verification state", func(s *maulv1.IntentStatus) { s.Verification.State = "Judging" }},
	} {
		t.Run("status with "+tt.name, func(t *testing.T) {
			i := fresh(t)
			i.Status = full()
			tt.mutate(&i.Status)
			if err := c.Status().Update(ctx, i); err == nil {
				t.Errorf("Status().Update(%s) = nil, want rejection", tt.name)
			}
		})
	}
}

// testOutcomeVerificationSchema: a PreviewVerification runs exactly one of
// criteria and outcomes, saved checks travel inline and bounded, and the
// evaluator's status is bounded and in a fixed vocabulary.
func testOutcomeVerificationSchema(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	const name = "target-1-v7-a1"
	t.Run("an outcome verification round-trips", func(t *testing.T) {
		v := schemaOutcomeVerification(name)
		want := v.Spec.DeepCopy()
		if err := c.Create(ctx, v); err != nil {
			t.Fatalf("Create(outcome verification) = %v", err)
		}
		got := &maulv1.PreviewVerification{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(v), got); err != nil {
			t.Fatal(err)
		}
		sameJSON(t, "outcome verification spec", got.Spec, *want)
		got.Status = fullEvaluatorStatus()
		if err := c.Status().Update(ctx, got); err != nil {
			t.Fatalf("Status().Update(evaluator status) = %v", err)
		}
		read := &maulv1.PreviewVerification{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(v), read); err != nil {
			t.Fatal(err)
		}
		sameJSON(t, "evaluator status", read.Status, fullEvaluatorStatus())
	})
	t.Run("spec checks are immutable", func(t *testing.T) {
		v := &maulv1.PreviewVerification{}
		if err := c.Get(ctx, client.ObjectKey{Name: name, Namespace: "default"}, v); err != nil {
			t.Fatal(err)
		}
		v.Spec.Checks[0].Body = `{"form":"http","changed":true}`
		if err := c.Update(ctx, v); err == nil {
			t.Error("Update(saved check body) = nil, want immutability rejection")
		}
	})

	bigCheck := func(n int, r string) func(*maulv1.PreviewVerification) {
		return func(v *maulv1.PreviewVerification) { v.Spec.Checks[0].Body = strings.Repeat(r, n) }
	}
	asCriteria := func(v *maulv1.PreviewVerification) {
		v.Spec.Outcomes, v.Spec.OutcomesDigest, v.Spec.Checks, v.Spec.ChecksDigest = nil, "", nil, ""
		v.Spec.Rung, v.Spec.Tools = "", nil
		v.Spec.Criteria, v.Spec.CriteriaDigest = schemaCriteria()[:1], schemaDigest
	}
	for _, tt := range []struct {
		name    string
		mutate  func(*maulv1.PreviewVerification)
		wantErr bool
	}{
		{"both criteria and outcomes", func(v *maulv1.PreviewVerification) {
			v.Spec.Criteria, v.Spec.CriteriaDigest = schemaCriteria()[:1], schemaDigest
		}, true},
		{"neither criteria nor outcomes", func(v *maulv1.PreviewVerification) {
			v.Spec.Outcomes, v.Spec.OutcomesDigest = nil, ""
		}, true},
		{"outcomes without their digest", func(v *maulv1.PreviewVerification) { v.Spec.OutcomesDigest = "" }, true},
		{"criteria alone", asCriteria, false},
		{"criteria without their digest", func(v *maulv1.PreviewVerification) {
			asCriteria(v)
			v.Spec.CriteriaDigest = ""
		}, true},
		{"criteria with a rung", func(v *maulv1.PreviewVerification) {
			asCriteria(v)
			v.Spec.Rung = maulv1.RungFull
		}, true},
		{"criteria with tools", func(v *maulv1.PreviewVerification) {
			asCriteria(v)
			v.Spec.Tools = &maulv1.ToolsSpec{MaxToolCalls: 300}
		}, true},
		{"outcomes without a rung", func(v *maulv1.PreviewVerification) { v.Spec.Rung = "" }, true},
		{"outcomes without tools", func(v *maulv1.PreviewVerification) { v.Spec.Tools = nil }, true},
		{"an unknown rung", func(v *maulv1.PreviewVerification) { v.Spec.Rung = "explore" }, true},
		{"a replay round with no checks", func(v *maulv1.PreviewVerification) {
			v.Spec.Rung, v.Spec.Checks, v.Spec.ChecksDigest = maulv1.RungReplay, nil, ""
		}, true},
		{"a replay round", func(v *maulv1.PreviewVerification) { v.Spec.Rung = maulv1.RungReplay }, false},
		{"a first round with no checks", func(v *maulv1.PreviewVerification) {
			v.Spec.Checks, v.Spec.ChecksDigest = nil, ""
		}, false},
		{"checks without their digest", func(v *maulv1.PreviewVerification) { v.Spec.ChecksDigest = "" }, true},
		{"21 outcomes", func(v *maulv1.PreviewVerification) {
			for i := len(v.Spec.Outcomes); i < 21; i++ {
				o := v.Spec.Outcomes[0]
				o.ID = fmt.Sprintf("o-%d", i)
				v.Spec.Outcomes = append(v.Spec.Outcomes, o)
			}
		}, true},
		{"41 checks", func(v *maulv1.PreviewVerification) {
			for len(v.Spec.Checks) < 41 {
				v.Spec.Checks = append(v.Spec.Checks, v.Spec.Checks[0])
			}
		}, true},
		{"40 checks", func(v *maulv1.PreviewVerification) {
			for len(v.Spec.Checks) < 40 {
				v.Spec.Checks = append(v.Spec.Checks, v.Spec.Checks[0])
			}
		}, false},
		{"40 checks of 8 KiB", func(v *maulv1.PreviewVerification) {
			for len(v.Spec.Checks) < 40 {
				v.Spec.Checks = append(v.Spec.Checks, v.Spec.Checks[0])
			}
			for i := range v.Spec.Checks {
				v.Spec.Checks[i].Body = strings.Repeat("x", maulv1.MaxSavedCheckBytes)
			}
		}, false},
		{"a check body of 8 KiB", bigCheck(maulv1.MaxSavedCheckBytes, "x"), false},
		{"a check body over 8 KiB", bigCheck(maulv1.MaxSavedCheckBytes+1, "x"), true},
		// The bound is on bytes: 2731 three-byte characters are 8193 bytes.
		{"a check body over 8 KiB in multi-byte text", bigCheck(2731, "€"), true},
		{"an empty check body", bigCheck(0, "x"), true},
		{"an unknown check form", func(v *maulv1.PreviewVerification) { v.Spec.Checks[0].Form = "grpc" }, true},
		{"a check digest that is not one", func(v *maulv1.PreviewVerification) {
			v.Spec.Checks[0].Digest = strings.Repeat("1", 64)
		}, true},
		{"a malformed check id", func(v *maulv1.PreviewVerification) { v.Spec.Checks[0].ID = "a..b" }, true},
		{"a check bound to a malformed outcome", func(v *maulv1.PreviewVerification) {
			v.Spec.Checks[0].Outcome = "winner-rises.order"
		}, true},
		{"301 tool calls", func(v *maulv1.PreviewVerification) { v.Spec.Tools.MaxToolCalls = 301 }, true},
		{"49 tool calls", func(v *maulv1.PreviewVerification) { v.Spec.Tools.MaxToolCalls = 49 }, true},
		{"nine look-around routes", func(v *maulv1.PreviewVerification) { v.Spec.Tools.LookAroundRoutes = 9 }, true},
		{"no look-around", func(v *maulv1.PreviewVerification) { v.Spec.Tools.LookAroundRoutes = 0 }, false},
	} {
		t.Run("create with "+tt.name, func(t *testing.T) {
			v := schemaOutcomeVerification("target-1-v8-" + shortHash(tt.name))
			tt.mutate(v)
			err := c.Create(ctx, v)
			if (err != nil) != tt.wantErr {
				t.Errorf("Create(outcome verification with %s) = %v, wantErr %v", tt.name, err, tt.wantErr)
			}
		})
	}
	testEvaluatorStatusSchema(ctx, t, c, name)
}

// testEvaluatorStatusSchema: the evaluator's status on the outcome
// verification `name` is bounded and in a fixed vocabulary.
func testEvaluatorStatusSchema(ctx context.Context, t *testing.T, c client.Client, name string) {
	t.Helper()
	t.Run("the evaluator's reasons are admitted", func(t *testing.T) {
		for _, r := range []maulv1.PreviewVerificationReason{
			maulv1.VerificationReasonToolsUnscheduled, maulv1.VerificationReasonToolsImagePull,
			maulv1.VerificationReasonToolsNotReady, maulv1.VerificationReasonLedgerMismatch,
			maulv1.VerificationReasonEvidenceMismatch,
		} {
			v := &maulv1.PreviewVerification{}
			if err := c.Get(ctx, client.ObjectKey{Name: name, Namespace: "default"}, v); err != nil {
				t.Fatal(err)
			}
			v.Status = fullEvaluatorStatus()
			v.Status.Phase, v.Status.Reason = maulv1.VerificationInfra, r
			if err := c.Status().Update(ctx, v); err != nil {
				t.Errorf("Status().Update(reason %s) = %v", r, err)
			}
		}
	})
	t.Run("a full ledger index fits", func(t *testing.T) {
		v := &maulv1.PreviewVerification{}
		if err := c.Get(ctx, client.ObjectKey{Name: name, Namespace: "default"}, v); err != nil {
			t.Fatal(err)
		}
		v.Status = fullEvaluatorStatus()
		for len(v.Status.Ledger.Entries) < maulv1.MaxLedgerIndexEntries {
			e := v.Status.Ledger.Entries[1]
			e.Seq = int32(len(v.Status.Ledger.Entries) + 1)
			v.Status.Ledger.Entries = append(v.Status.Ledger.Entries, e)
		}
		if err := c.Status().Update(ctx, v); err != nil {
			t.Errorf("Status().Update(%d ledger entries) = %v", maulv1.MaxLedgerIndexEntries, err)
		}
	})
	for _, tt := range []struct {
		name   string
		mutate func(*maulv1.PreviewVerificationStatus)
	}{
		{"a tool port of 0", func(s *maulv1.PreviewVerificationStatus) { s.Tools.Port = 0 }},
		{"a tool address that is not one", func(s *maulv1.PreviewVerificationStatus) { s.Tools.PodIP = "tools.svc" }},
		{"a ledger hash that is not one", func(s *maulv1.PreviewVerificationStatus) { s.Ledger.SHA256 = "abc" }},
		{"an unknown ledger kind", func(s *maulv1.PreviewVerificationStatus) { s.Ledger.Entries[0].Kind = "app" }},
		{"an unknown ledger verb", func(s *maulv1.PreviewVerificationStatus) { s.Ledger.Entries[0].Verb = "shell" }},
		{"a ledger seq of 0", func(s *maulv1.PreviewVerificationStatus) { s.Ledger.Entries[0].Seq = 0 }},
		{"more ledger entries than a ledger holds", func(s *maulv1.PreviewVerificationStatus) {
			for len(s.Ledger.Entries) <= maulv1.MaxLedgerIndexEntries {
				s.Ledger.Entries = append(s.Ledger.Entries, s.Ledger.Entries[0])
			}
		}},
		{"nine artifacts on an entry", func(s *maulv1.PreviewVerificationStatus) {
			for len(s.Ledger.Entries[1].Artifacts) < 9 {
				s.Ledger.Entries[1].Artifacts = append(s.Ledger.Entries[1].Artifacts, strings.Repeat("b", 64))
			}
		}},
		{"an artifact hash that is not one", func(s *maulv1.PreviewVerificationStatus) {
			s.Ledger.Entries[1].Artifacts = []string{"shot.png"}
		}},
		{"an unknown check result", func(s *maulv1.PreviewVerificationStatus) { s.Replay[0].Result = "flaky" }},
		{"a replay digest that is not one", func(s *maulv1.PreviewVerificationStatus) {
			s.Replay[0].CheckDigest = "winner-rises.order"
		}},
		{"41 confirmations", func(s *maulv1.PreviewVerificationStatus) {
			for len(s.Confirmations) < 41 {
				s.Confirmations = append(s.Confirmations, s.Confirmations[0])
			}
		}},
		{"41 confirmed checks", func(s *maulv1.PreviewVerificationStatus) {
			for len(s.Confirmed) < 41 {
				s.Confirmed = append(s.Confirmed, s.Confirmed[0])
			}
		}},
		{"a confirmed check over 8 KiB", func(s *maulv1.PreviewVerificationStatus) {
			s.Confirmed[0].Body = strings.Repeat("x", maulv1.MaxSavedCheckBytes+1)
		}},
		{"a capture over 64 KiB", func(s *maulv1.PreviewVerificationStatus) { s.Capture.Components[0].Bytes = 65537 }},
		{"a negative event count", func(s *maulv1.PreviewVerificationStatus) { s.Capture.Events = -1 }},
	} {
		t.Run("evaluator status with "+tt.name, func(t *testing.T) {
			v := &maulv1.PreviewVerification{}
			if err := c.Get(ctx, client.ObjectKey{Name: name, Namespace: "default"}, v); err != nil {
				t.Fatal(err)
			}
			v.Status = fullEvaluatorStatus()
			tt.mutate(&v.Status)
			if err := c.Status().Update(ctx, v); err == nil {
				t.Errorf("Status().Update(%s) = nil, want rejection", tt.name)
			}
		})
	}
}

// testEvaluateRunSchema: an evaluate run, and only one, records the
// verification it judges, pinned by UID, with its tool pod's address and
// its rung.
func testEvaluateRunSchema(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	for _, tt := range []struct {
		name    string
		stage   maulv1.IntentStage
		mutate  func(*maulv1.IntentRunSpec)
		wantErr bool
	}{
		{"an evaluate run without its verification", maulv1.IntentStageEvaluate,
			func(s *maulv1.IntentRunSpec) { s.Evaluation = nil }, true},
		{"an evaluate run without the verification's uid", maulv1.IntentStageEvaluate,
			func(s *maulv1.IntentRunSpec) { s.Evaluation.UID = "" }, true},
		{"an evaluate run with an unknown rung", maulv1.IntentStageEvaluate,
			func(s *maulv1.IntentRunSpec) { s.Evaluation.Rung = "explore" }, true},
		{"an evaluate run with a tool address that is not one", maulv1.IntentStageEvaluate,
			func(s *maulv1.IntentRunSpec) { s.Evaluation.ToolIP = "tools.slot.svc" }, true},
		{"an evaluate run with an IPv6 tool address", maulv1.IntentStageEvaluate,
			func(s *maulv1.IntentRunSpec) { s.Evaluation.ToolIP = "fd00:10:42::17" }, false},
		{"a check review run recording a verification", maulv1.IntentStageCheckReview,
			func(s *maulv1.IntentRunSpec) { s.Evaluation = schemaRunEvaluation() }, true},
		{"a build run recording a verification", maulv1.IntentStageBuild,
			func(s *maulv1.IntentRunSpec) { s.Evaluation = schemaRunEvaluation() }, true},
	} {
		t.Run("create with "+tt.name, func(t *testing.T) {
			r := schemaIntentRun("target-1-ev-"+shortHash(tt.name), tt.stage)
			tt.mutate(&r.Spec)
			err := c.Create(ctx, r)
			if (err != nil) != tt.wantErr {
				t.Errorf("Create(run with %s) = %v, wantErr %v", tt.name, err, tt.wantErr)
			}
		})
	}
	t.Run("an evaluate run's record is immutable", func(t *testing.T) {
		r := schemaIntentRun("target-1-ev-r9-a1", maulv1.IntentStageEvaluate)
		if err := c.Create(ctx, r); err != nil {
			t.Fatalf("Create(evaluate run) = %v", err)
		}
		r.Spec.Evaluation.ToolIP = "10.42.0.18"
		if err := c.Update(ctx, r); err == nil {
			t.Error("Update(evaluation tool address) = nil, want immutability rejection")
		}
	})
}
