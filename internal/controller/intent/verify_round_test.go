// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/jobs"
)

// failingVerification drives a new intent of e to InReview, verifies its
// combined change and has the tester observe the health check failing
// with a 500. It returns the intent and its PreviewVerification's name.
func (e *env) failingVerification() (string, string) {
	e.t.Helper()
	name := e.reviewing()
	e.readyPreview(name)
	e.review(name, 3)
	pv := e.onlyVerification(name)
	e.observe(pv.Name, v1alpha1.VerificationObserved, answering(500))
	e.review(name, 1) // judged: Failing
	if v := e.verification(name); v.State != v1alpha1.VerificationFailing {
		e.t.Fatalf("verification = %+v, want Failing", v)
	}
	return name, pv.Name
}

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

// TestVerifyFixRoundStartsAndPushes: a failed verification at maul's own
// heads starts one acceptance-fix round on the failing criterion's
// repository, consuming the verification by UID; its handoff carries the
// failure evidence under "Acceptance failures" and records the failure's
// signature; it pushes and is counted as a verify fix, not a revision.
func TestVerifyFixRoundStartsAndPushes(t *testing.T) {
	e := newVerifyEnv(t, acceptancePlan)
	name, pvName := e.failingVerification()
	e.review(name, 1)
	in := e.get(name)
	runs := e.verifyRuns(name)
	if in.Status.Phase != v1alpha1.IntentRevising || len(runs) != 1 {
		t.Fatalf("phase %s, verify runs %d; want Revising with one", in.Status.Phase, len(runs))
	}
	pv := e.onlyVerification(name)
	run := runs[0]
	if ref := run.Spec.Inputs.VerificationRef; ref == nil || ref.Name != pvName || ref.UID != pv.UID ||
		!sameRepo(run.Spec.Repository.URL, appRepoURL) || run.Spec.Round != 1 {
		t.Fatalf("verify run = %+v", run.Spec)
	}
	if v := in.Status.Verification; v.State != v1alpha1.VerificationFixing {
		t.Fatalf("verification = %+v, want Fixing", v)
	}
	in = e.drive(name, v1alpha1.IntentInReview, repoImage)
	if in.Status.VerifyFixes != 1 || in.Status.Revisions != 0 || in.Status.CheckFixes != 0 {
		t.Fatalf("counters = revisions %d checkFixes %d verifyFixes %d", in.Status.Revisions,
			in.Status.CheckFixes, in.Status.VerifyFixes)
	}
	run = e.verifyRuns(name)[0]
	if run.Status.Phase != v1alpha1.RunComplete || run.Status.PushedCommit == "" {
		t.Fatalf("verify run = %s %s %q", run.Status.Phase, run.Status.Outcome, run.Status.Detail)
	}
	cm := e.runInput(run)
	handoff := cm.Data[keyInvestigation]
	for _, want := range []string{"### Acceptance failures", "`health`", "the app answers its health check",
		"/healthz", "500", "make the failing acceptance criteria pass"} {
		if !strings.Contains(handoff, want) {
			t.Errorf("handoff omits %q:\n%s", want, handoff)
		}
	}
	if sig := cm.Data[keyVerifySignature]; !strings.HasPrefix(sig, acceptance.FingerprintVersion+":") {
		t.Errorf("signature = %q", sig)
	}
	if cm.Data[keyCheckSignature] != "" {
		t.Errorf("a verify round recorded a check signature %q", cm.Data[keyCheckSignature])
	}
}

// TestVerifyFixRepeatedFailureHalts: the re-verification after a fix
// fails exactly as before: automatic fixes halt (RepeatedFailure), the
// condition says so, and the intent stays in review.
func TestVerifyFixRepeatedFailureHalts(t *testing.T) {
	e := newVerifyEnv(t, acceptancePlan)
	name, _ := e.failingVerification()
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	e.readyPreview(name)
	e.review(name, 4)
	again := e.verification(name)
	if again.Ref == nil || again.Round != 2 {
		t.Fatalf("no re-verification at the fixed heads: %+v", again)
	}
	e.observe(again.Ref.Name, v1alpha1.VerificationObserved, answering(500))
	e.review(name, 3)
	in := e.get(name)
	v := in.Status.Verification
	if in.Status.Phase != v1alpha1.IntentInReview || len(e.verifyRuns(name)) != 1 ||
		v.AutoFix.State != v1alpha1.AutoFixHalted || v.AutoFix.Reason != v1alpha1.AutoFixRepeatedFailure {
		t.Fatalf("phase %s, verify runs %d, verification %+v", in.Status.Phase, len(e.verifyRuns(name)), v)
	}
	if c := meta.FindStatusCondition(in.Status.Conditions, v1alpha1.ConditionVerifyFailing); c == nil ||
		c.Status != metav1.ConditionTrue || c.Reason != string(v1alpha1.AutoFixRepeatedFailure) {
		t.Fatalf("condition = %+v", c)
	}
}

// TestVerifyFixGuardsHalt: each bound halts automatic fixes instead of
// starting a round, never blocking the intent.
func TestVerifyFixGuardsHalt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		project func(*v1alpha1.Project)
		reason  v1alpha1.AutoFixReason
		state   v1alpha1.IntentVerificationState
	}{
		{"max verify fixes", func(p *v1alpha1.Project) { p.Spec.Limits.MaxVerifyFixes = new(int32(0)) },
			v1alpha1.AutoFixMaxVerifyFixes, v1alpha1.VerificationExhausted},
		{"cost ceiling", func(p *v1alpha1.Project) { p.Spec.Limits.MaxCostMicroUSD = 1 },
			v1alpha1.AutoFixCostCeiling, v1alpha1.VerificationFailing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newVerifyEnv(t, acceptancePlan)
			name, _ := e.failingVerification()
			var proj v1alpha1.Project
			if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: "target"},
				&proj); err != nil {
				t.Fatal(err)
			}
			tc.project(&proj)
			if err := e.c.Update(context.Background(), &proj); err != nil {
				t.Fatal(err)
			}
			e.review(name, 2)
			in := e.get(name)
			v := in.Status.Verification
			if in.Status.Phase != v1alpha1.IntentInReview || len(e.verifyRuns(name)) != 0 ||
				v.AutoFix.State != v1alpha1.AutoFixHalted || v.AutoFix.Reason != tc.reason || v.State != tc.state {
				t.Fatalf("phase %s, verify runs %d, verification %+v", in.Status.Phase, len(e.verifyRuns(name)), v)
			}
		})
	}
}

// TestVerifyFixHumanPushHalts (H7): a head maul did not push supersedes the
// failed verification and halts automatic fixes: no round starts.
func TestVerifyFixHumanPushHalts(t *testing.T) {
	e := newVerifyEnv(t, acceptancePlan)
	name, _ := e.failingVerification()
	e.pushHead(e.prNumber(name), strings.Repeat("e", 40))
	e.review(name, 3)
	v := e.verification(name)
	if len(e.verifyRuns(name)) != 0 || v.AutoFix.Reason != v1alpha1.AutoFixHumanPush {
		t.Fatalf("verify runs %d, verification %+v", len(e.verifyRuns(name)), v)
	}
}

// TestVerifyFixEnvFailedTargetsTheCrashedComponent: a component that
// crashed is the change's failure: the round targets its repository, and
// its handoff names the part.
func TestVerifyFixEnvFailedTargetsTheCrashedComponent(t *testing.T) {
	e := newVerifyEnv(t, acceptancePlan)
	name := e.reviewing()
	e.readyPreview(name)
	e.review(name, 3)
	pv := e.onlyVerification(name)
	e.observe(pv.Name, v1alpha1.VerificationEnvFailed, nil, v1alpha1.PartFact{Name: "app",
		Role: v1alpha1.PreviewRoleComponent, Waiting: "CrashLoopBackOff", Restarts: 4,
		LogTail: "panic: nil map"})
	e.review(name, 2)
	runs := e.verifyRuns(name)
	if len(runs) != 1 || !sameRepo(runs[0].Spec.Repository.URL, appRepoURL) {
		t.Fatalf("verify runs = %+v", runs)
	}
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	handoff := e.runInput(e.verifyRuns(name)[0]).Data[keyInvestigation]
	for _, want := range []string{"CrashLoopBackOff", "panic: nil map", "`app`"} {
		if !strings.Contains(handoff, want) {
			t.Errorf("handoff omits %q:\n%s", want, handoff)
		}
	}
}

// TestHumanWinsOverAVerifyFix (H1): a review due on the pull request and
// a verify fix due: the human's round starts, and automatic fixes halt in
// the same write.
func TestHumanWinsOverAVerifyFix(t *testing.T) {
	e := newVerifyEnv(t, acceptancePlan)
	name := e.reviewing()
	e.readyPreview(name)
	e.review(name, 3)
	pv := e.onlyVerification(name)
	e.reviewOn(e.prNumber(name), 1501, "Return JSON, please.")
	e.observe(pv.Name, v1alpha1.VerificationObserved, answering(500))
	e.review(name, 3)
	in := e.get(name)
	runs := e.runsOf(name, v1alpha1.IntentStageRevise)
	if in.Status.Phase != v1alpha1.IntentRevising || len(runs) != 1 ||
		runs[0].Spec.Trigger != v1alpha1.IntentRunTriggerReview {
		t.Fatalf("phase %s, runs %+v; want the review's round", in.Status.Phase, runs)
	}
	if a := in.Status.Verification.AutoFix; a.State != v1alpha1.AutoFixHalted || a.Reason != v1alpha1.AutoFixHumanReview {
		t.Fatalf("auto-fix = %+v", a)
	}
}

// TestHumanPendingHoldsAVerifyFix (H3): a review inside its quiet window
// holds a due verify fix: nothing starts until the human's round can.
func TestHumanPendingHoldsAVerifyFix(t *testing.T) {
	e := newVerifyEnv(t, acceptancePlan)
	name, _ := e.failingVerification()
	e.reviewOn(e.prNumber(name), 1601, "Hold on, the route is wrong.")
	e.review(name, 1)
	if in := e.get(name); in.Status.Phase != v1alpha1.IntentInReview ||
		len(e.runsOf(name, v1alpha1.IntentStageRevise)) != 0 {
		t.Fatalf("phase %s, runs %d; want nothing started inside the quiet window", in.Status.Phase,
			len(e.runsOf(name, v1alpha1.IntentStageRevise)))
	}
	e.review(name, 2)
	runs := e.runsOf(name, v1alpha1.IntentStageRevise)
	if len(runs) != 1 || runs[0].Spec.Trigger != v1alpha1.IntentRunTriggerReview {
		t.Fatalf("runs = %+v, want the review's round", runs)
	}
}

// TestHumanCancelsAPendingVerifyFix (H5): a review arriving while a verify
// fix waits unlaunched cancels it: the run settles superseded with no Job,
// the round is not counted or retried, the pull request is told, and the
// human's round runs next.
func TestHumanCancelsAPendingVerifyFix(t *testing.T) {
	e := newVerifyEnv(t, acceptancePlan)
	name, _ := e.failingVerification()
	e.review(name, 1)
	runs := e.verifyRuns(name)
	if len(runs) != 1 || e.get(name).Status.Phase != v1alpha1.IntentRevising {
		t.Fatalf("verify runs %d, want one leased", len(runs))
	}
	n := e.prNumber(name)
	e.reviewOn(n, 1701, "Not like that.")
	e.review(name, 1)
	v := e.verification(name)
	if v.CancelRun != runs[0].Name || v.AutoFix.State != v1alpha1.AutoFixHalted ||
		v.AutoFix.Reason != v1alpha1.AutoFixHumanReview {
		t.Fatalf("verification = %+v, want the run cancelled", v)
	}
	e.readyRepositories(repoImage)
	e.runRuns()
	run := e.verifyRuns(name)[0]
	if run.Status.Phase != v1alpha1.RunFailed || run.Status.Outcome != OutcomeSupersededByHuman ||
		run.Status.JobRef != nil || len(e.jobs.order) != 2 {
		t.Fatalf("verify run = %s %s job %+v, jobs %v", run.Status.Phase, run.Status.Outcome, run.Status.JobRef,
			e.jobs.order)
	}
	e.review(name, 1)
	in := e.get(name)
	if in.Status.Phase != v1alpha1.IntentInReview || in.Status.VerifyFixes != 0 || in.Status.Revisions != 0 ||
		in.Status.Verification.CancelRun != "" || len(e.verifyRuns(name)) != 1 {
		t.Fatalf("after the cancel: phase %s, fixes %d, revisions %d, verification %+v, verify runs %d",
			in.Status.Phase, in.Status.VerifyFixes, in.Status.Revisions, in.Status.Verification,
			len(e.verifyRuns(name)))
	}
	if !strings.Contains(e.everyBody(), "cancelled for your review") {
		t.Error("the pull request was not told the automatic fix was cancelled")
	}
	in = e.drive(name, v1alpha1.IntentRevising, repoImage)
	if r := e.reviseRun(name, in.Status.Rounds); r == nil || r.Spec.Trigger != v1alpha1.IntentRunTriggerReview {
		t.Fatalf("round %d = %+v, want the review's", in.Status.Rounds, r)
	}
}

// TestHumanWaitsForARunningVerifyFix (H6): a review arriving while a verify
// fix's agent runs lets it finish and push, halts automatic fixes, and the
// human's round runs next, told that the fix landed after their review.
func TestHumanWaitsForARunningVerifyFix(t *testing.T) {
	e := newVerifyEnv(t, acceptancePlan)
	name, _ := e.failingVerification()
	e.review(name, 1)
	fix := e.verifyRuns(name)[0]
	hold := true
	e.jobs.statusFn = func(spec jobs.Spec) (jobs.Status, bool) {
		if hold && spec.Finding == fix.Name {
			return jobs.Status{}, true
		}
		return jobs.Status{}, false
	}
	for range 4 {
		e.mustIntent(name)
		e.readyRepositories(repoImage)
		e.runRuns()
	}
	if r := e.verifyRuns(name)[0]; r.Status.JobRef == nil {
		t.Fatalf("the verify fix did not launch: %s %s %q", r.Status.Phase, r.Status.Outcome, r.Status.Detail)
	}
	e.reviewOn(e.prNumber(name), 1801, "Also rename the handler.")
	e.review(name, 1)
	v := e.verification(name)
	if v.CancelRun != "" || v.AutoFix.State != v1alpha1.AutoFixHalted || v.AutoFix.Reason != v1alpha1.AutoFixHumanReview {
		t.Fatalf("verification = %+v, want halted and nothing cancelled", v)
	}
	hold = false
	in := e.drive(name, v1alpha1.IntentInReview, repoImage)
	fix = e.verifyRuns(name)[0]
	if fix.Status.Phase != v1alpha1.RunComplete || fix.Status.PushedCommit == "" || in.Status.VerifyFixes != 1 {
		t.Fatalf("the running fix did not push: %s %s", fix.Status.Phase, fix.Status.Outcome)
	}
	e.clock.Advance(2 * time.Minute)
	in = e.drive(name, v1alpha1.IntentRevising, repoImage)
	human := e.reviseRun(name, in.Status.Rounds)
	if human == nil || human.Spec.Trigger != v1alpha1.IntentRunTriggerReview {
		t.Fatalf("round %d = %+v, want the review's", in.Status.Rounds, human)
	}
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	handoff := e.runInput(*e.reviseRun(name, human.Spec.Round)).Data[keyInvestigation]
	if !strings.Contains(handoff, "An automatic acceptance fix landed after your review") ||
		!strings.Contains(handoff, fix.Status.PushedCommit) {
		t.Errorf("the human's round was not told of the fix:\n%s", handoff)
	}
}

// TestVerifyCommandOnAPullRequestIsSettled: an approver's /maul verify on
// an intent pull request is recorded and answered there by the review poll.
func TestVerifyCommandOnAPullRequestIsSettled(t *testing.T) {
	e := newVerifyEnv(t, acceptancePlan)
	name := e.reviewing()
	e.review(name, 2)
	n := e.prNumber(name)
	id := e.prCommand(n, approver, "/maul verify")
	e.review(name, 1)
	if r := e.verification(name).Requested; r == nil || r.CommentID != id {
		t.Fatalf("requested = %+v", r)
	}
	if !strings.Contains(e.everyBody(), "comment-"+itoa(id)) {
		t.Error("the command was not answered on the pull request")
	}
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
