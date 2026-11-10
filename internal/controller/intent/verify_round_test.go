// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// verifyRuns are the Intent's acceptance-fix runs.
func (e *env) verifyRuns(name string) []v1alpha1.IntentRun {
	e.t.Helper()
	var out []v1alpha1.IntentRun
	for _, r := range e.runsOf(name, v1alpha1.IntentStageRevise) {
		if roundKindOf(r.Spec.Trigger) == roundVerify {
			out = append(out, r)
		}
	}
	return out
}

// runInput is run's input ConfigMap.
func (e *env) runInput(run v1alpha1.IntentRun) *corev1.ConfigMap {
	e.t.Helper()
	var cm corev1.ConfigMap
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: testNS,
		Name: run.Spec.Inputs.ConfigMap}, &cm); err != nil {
		e.t.Fatal(err)
	}
	return &cm
}

// prNumber is the intent's one pull request's number.
func (e *env) prNumber(name string) int64 {
	e.t.Helper()
	return e.get(name).Status.PullRequests[0].Number
}

// TestFixTargetPicksAnOpenPullRequest: the round targets the repository
// of the first failing criterion with an open pull request, in plan order;
// with none, no target.
func TestFixTargetPicksAnOpenPullRequest(t *testing.T) {
	const apiURL, webURL = "https://github.com/acme/api", "https://github.com/acme/web"
	heads := []v1alpha1.VerificationHead{{Component: "api", Repository: apiURL, Revision: "a"},
		{Component: "web", Repository: webURL, Revision: "b"}}
	proj := &v1alpha1.Project{Spec: v1alpha1.ProjectSpec{Repositories: []v1alpha1.ProjectRepository{
		{Name: "api", URL: apiURL}, {Name: "web", URL: webURL}}}}
	prs := func(apiState, webState string) *v1alpha1.Intent {
		return &v1alpha1.Intent{Status: v1alpha1.IntentStatus{
			Plan: &v1alpha1.IntentPlan{Repositories: []string{apiURL, webURL}},
			PullRequests: []v1alpha1.IntentPullRequest{
				{Repository: apiURL, Number: 1, State: apiState}, {Repository: webURL, Number: 2, State: webState}}}}
	}
	for _, tc := range []struct {
		name    string
		in      *v1alpha1.Intent
		targets []string
		want    string
	}{
		{"first failing", prs(prOpen, prOpen), []string{"web", "api"}, webURL},
		{"first merged, next open", prs(prOpen, prMerged), []string{"web", "api"}, apiURL},
		{"none open", prs(prMerged, prClosed), []string{"web", "api"}, ""},
		{"unknown component", prs(prOpen, prOpen), []string{"db"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &pass{in: tc.in, proj: proj}
			got := ""
			if pr := p.fixTarget(tc.targets, heads); pr != nil {
				got = pr.Repository
			}
			if got != tc.want {
				t.Errorf("target = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestVerifyLaunchRefusal: the run scheduler settles a verify fix the
// intent cancelled for a human as superseded, and refuses one whose plan
// revision has no recorded criteria or whose repository is not previewed;
// any other run is launched.
func TestVerifyLaunchRefusal(t *testing.T) {
	proj := testProject()
	proj.Spec.Preview = &appPreview
	run := reviseRunOf(v1alpha1.IntentRunTriggerVerify)
	run.Name = "fix-1"
	run.Spec.Repository.URL = appRepoURL
	in := func(mutate func(*v1alpha1.Intent)) *v1alpha1.Intent {
		i := &v1alpha1.Intent{Status: v1alpha1.IntentStatus{
			Acceptance:   &v1alpha1.IntentAcceptance{PlanRevision: 1},
			Verification: &v1alpha1.IntentVerification{}}}
		if mutate != nil {
			mutate(i)
		}
		return i
	}
	for _, tc := range []struct {
		name string
		in   *v1alpha1.Intent
		run  func() *v1alpha1.IntentRun
		want string
	}{
		{"launch", in(nil), nil, ""},
		{"cancelled", in(func(i *v1alpha1.Intent) { i.Status.Verification.CancelRun = "fix-1" }), nil,
			OutcomeSupersededByHuman},
		{"another run cancelled", in(func(i *v1alpha1.Intent) { i.Status.Verification.CancelRun = "fix-0" }),
			nil, ""},
		{"no criteria", in(func(i *v1alpha1.Intent) { i.Status.Acceptance = nil }), nil, OutcomeAborted},
		{"other plan revision", in(func(i *v1alpha1.Intent) { i.Status.Acceptance.PlanRevision = 2 }), nil,
			OutcomeAborted},
		{"not previewed", in(nil), func() *v1alpha1.IntentRun {
			r := run.DeepCopy()
			r.Spec.Repository.URL = "https://github.com/acme/other"
			return r
		}, OutcomeAborted},
		{"not a verify round", in(func(i *v1alpha1.Intent) { i.Status.Verification.CancelRun = "fix-1" }),
			func() *v1alpha1.IntentRun {
				r := run.DeepCopy()
				r.Spec.Trigger = v1alpha1.IntentRunTriggerReview
				return r
			}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := run
			if tc.run != nil {
				r = tc.run()
			}
			got := ""
			if res := verifyRunRefusal(tc.in, proj, r); res != nil {
				got = res.outcome
			}
			if got != tc.want {
				t.Errorf("refusal = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestVerifyWorkPending: humanPending is asked only while a verify fix or
// a verification could start.
func TestVerifyWorkPending(t *testing.T) {
	ref := &v1alpha1.ObjectReference{Name: "v", UID: "u"}
	armed, halted := v1alpha1.AutoFix{State: v1alpha1.AutoFixArmed}, v1alpha1.AutoFix{State: v1alpha1.AutoFixHalted}
	for _, tc := range []struct {
		name string
		v    *v1alpha1.IntentVerification
		want bool
	}{
		{"none", nil, false},
		{"a request", &v1alpha1.IntentVerification{State: v1alpha1.VerificationPassed, Ref: ref,
			Requested: &v1alpha1.VerifyRequest{CommentID: 1}}, true},
		{"not verified yet", &v1alpha1.IntentVerification{State: v1alpha1.VerificationWaitingForCI}, true},
		{"needs a replan", &v1alpha1.IntentVerification{State: v1alpha1.VerificationNeedsReplan}, false},
		{"in flight", &v1alpha1.IntentVerification{State: v1alpha1.VerificationVerifying, Ref: ref}, false},
		{"passed", &v1alpha1.IntentVerification{State: v1alpha1.VerificationPassed, Ref: ref}, false},
		{"failing, armed", &v1alpha1.IntentVerification{State: v1alpha1.VerificationFailing, Ref: ref,
			AutoFix: armed}, true},
		{"failing, halted", &v1alpha1.IntentVerification{State: v1alpha1.VerificationFailing, Ref: ref,
			AutoFix: halted}, false},
		{"fixing", &v1alpha1.IntentVerification{State: v1alpha1.VerificationFixing, Ref: ref, AutoFix: armed},
			false},
	} {
		if got := verifyWorkPending(tc.v); got != tc.want {
			t.Errorf("%s: verifyWorkPending = %v, want %v", tc.name, got, tc.want)
		}
	}
}
