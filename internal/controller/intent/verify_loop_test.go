// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/envelope"
	"github.com/devthenet-labs/maul/internal/jobs"
)

// acceptancePlan is validPlan declaring one http criterion in the one-step
// shorthand against the app, and one manual criterion.
var acceptancePlan = strings.Replace(validPlan, "estimated_token_budget: 200000\n",
	"estimated_token_budget: 200000\n"+
		"acceptance:\n"+
		"  - id: health\n"+
		"    kind: http\n"+
		"    statement: the app answers its health check\n"+
		"    target: app\n"+
		"    request: {method: GET, path: /healthz}\n"+
		"    expect: {status: 200}\n"+
		"  - id: sounds-good\n"+
		"    kind: manual\n"+
		"    statement: it sounds good\n"+
		"    reason: only a human can judge it\n", 1)

// newVerifyEnv is an env over the one-repository Project previewing its app
// with verification on, in the Project and in the controller, whose plans
// carry plan (validPlan when empty).
func newVerifyEnv(t *testing.T, plan string, mutate ...func(*v1alpha1.Project)) *env {
	t.Helper()
	p := testProject()
	p.Spec.Preview = &appPreview
	p.Spec.Verification = &v1alpha1.ProjectVerification{Enabled: true}
	for _, m := range mutate {
		m(p)
	}
	e := newEnv(t, p)
	e.intent.Settings.Previews = true
	e.intent.Settings.Verification = true
	e.intent.Settings.VerifyTimeoutMax = 20 * time.Minute
	if plan != "" {
		e.jobs.output = func(spec jobs.Spec) jobs.RunOutput {
			if spec.Phase == "plan" {
				return planOutput(envelope.Stage{Outcome: envelope.OutcomeOK, Harness: "claude", Model: spec.Model,
					Usage: envelope.Usage{InputTokens: 10, OutputTokens: 20, CostUSD: 0.25}}, plan)
			}
			return defaultOutput(spec)
		}
	}
	return e
}

// reviewing drives a new intent to InReview and returns its name.
func (e *env) reviewing() string {
	e.t.Helper()
	name := e.awaiting()
	e.gh.label(1, "maul:approved", approver)
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	return name
}

// review runs review passes, each one poll interval after the last.
func (e *env) review(name string, passes int) {
	e.t.Helper()
	for range passes {
		e.clock.Advance(time.Minute)
		e.mustIntent(name)
	}
}

// verifications are the Intent's PreviewVerifications, by name.
func (e *env) verifications(name string) []v1alpha1.PreviewVerification {
	e.t.Helper()
	var list v1alpha1.PreviewVerificationList
	if err := e.c.List(context.Background(), &list, client.InNamespace(testNS),
		client.MatchingLabels{v1alpha1.LabelIntent: name}); err != nil {
		e.t.Fatal(err)
	}
	slices.SortFunc(list.Items, func(a, b v1alpha1.PreviewVerification) int { return strings.Compare(a.Name, b.Name) })
	return list.Items
}

// onlyVerification is the Intent's one PreviewVerification.
func (e *env) onlyVerification(name string) v1alpha1.PreviewVerification {
	e.t.Helper()
	vs := e.verifications(name)
	if len(vs) != 1 {
		e.t.Fatalf("verifications = %d, want one: %+v", len(vs), vs)
	}
	return vs[0]
}

// readyPreview drives intent name's Preview to Ready at its spec.
func (e *env) readyPreview(name string) *v1alpha1.Preview {
	e.t.Helper()
	e.servePreview(name, v1alpha1.PreviewReady, "https://"+previewHostOf(name))
	return e.previewOf(name)
}

// TestVerificationHumanRoundHalts: a review or command round of the
// approved plan revision halts automatic fixes, unless it is not later than
// the round an approver's /maul verify fix re-armed them at.
func TestVerificationHumanRoundHalts(t *testing.T) {
	p := &pass{in: &v1alpha1.Intent{Status: v1alpha1.IntentStatus{
		Approval: &v1alpha1.IntentApproval{PlanRevision: 2},
		Verification: &v1alpha1.IntentVerification{AutoFix: v1alpha1.AutoFix{
			State: v1alpha1.AutoFixArmed, Rearm: &v1alpha1.Rearm{Round: 3}}}}}}
	run := func(round, plan int32, trigger v1alpha1.IntentRunTrigger) *v1alpha1.IntentRun {
		return &v1alpha1.IntentRun{Spec: v1alpha1.IntentRunSpec{Stage: v1alpha1.IntentStageRevise, Round: round,
			Trigger: trigger, Inputs: v1alpha1.IntentRunInputs{PlanRevision: plan}}}
	}
	for _, tc := range []struct {
		runs []*v1alpha1.IntentRun
		want v1alpha1.AutoFixReason
	}{
		{nil, ""},
		{[]*v1alpha1.IntentRun{run(3, 2, v1alpha1.IntentRunTriggerReview)}, ""},
		{[]*v1alpha1.IntentRun{run(4, 1, v1alpha1.IntentRunTriggerReview)}, ""},
		{[]*v1alpha1.IntentRun{run(4, 2, v1alpha1.IntentRunTriggerChecks)}, ""},
		{[]*v1alpha1.IntentRun{run(4, 2, v1alpha1.IntentRunTriggerVerify)}, ""},
		{[]*v1alpha1.IntentRun{run(4, 2, v1alpha1.IntentRunTriggerReview)}, v1alpha1.AutoFixHumanReview},
		{[]*v1alpha1.IntentRun{run(5, 2, v1alpha1.IntentRunTriggerCommand)}, v1alpha1.AutoFixHumanCommand},
	} {
		p.runs = tc.runs
		if got := p.humanRoundHalt(); got != tc.want {
			t.Errorf("runs %+v: halt %q, want %q", tc.runs, got, tc.want)
		}
	}
}

// TestHeadSetDigestIsOrderFree: the head-set digest binds every
// (repository, revision) pair and nothing of their order.
func TestHeadSetDigestIsOrderFree(t *testing.T) {
	a := v1alpha1.VerificationHead{Component: "api", Repository: "https://github.com/acme/api",
		Revision: strings.Repeat("a", 40)}
	b := v1alpha1.VerificationHead{Component: "web", Repository: "https://github.com/acme/web",
		Revision: strings.Repeat("b", 40)}
	if headSetDigest([]v1alpha1.VerificationHead{a, b}) != headSetDigest([]v1alpha1.VerificationHead{b, a}) {
		t.Fatal("the digest depends on order")
	}
	c := b
	c.Revision = strings.Repeat("c", 40)
	if headSetDigest([]v1alpha1.VerificationHead{a, b}) == headSetDigest([]v1alpha1.VerificationHead{a, c}) {
		t.Fatal("the digest ignores a revision")
	}
	if !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(headSetDigest([]v1alpha1.VerificationHead{a})) {
		t.Fatal("the digest is not in its stored form")
	}
}
