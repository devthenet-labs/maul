// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/acceptance/crdconv"
	"github.com/devthenet-labs/maul/internal/envelope"
	"github.com/devthenet-labs/maul/internal/ghclient"
	"github.com/devthenet-labs/maul/internal/jobs"
	"github.com/devthenet-labs/maul/internal/report"
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

// observe plays preview-controller and the tester: the verification named
// is set to phase with the observations do gives each of its criteria.
func (e *env) observe(pvName string, phase v1alpha1.PreviewVerificationPhase, do acceptance.Doer,
	env ...v1alpha1.PartFact) {
	e.t.Helper()
	var pv v1alpha1.PreviewVerification
	if err := e.c.Get(context.Background(), client.ObjectKey{Namespace: testNS, Name: pvName}, &pv); err != nil {
		e.t.Fatal(err)
	}
	pv.Status = v1alpha1.PreviewVerificationStatus{Phase: phase, Environment: env}
	if do != nil {
		cs, err := crdconv.CriteriaFromCRD(pv.Spec.Criteria)
		if err != nil {
			e.t.Fatal(err)
		}
		rt := acceptance.RunTokens{Round: pv.Spec.Round, Attempt: pv.Spec.Attempt, Run: pv.Spec.Run}
		for _, c := range cs {
			o := acceptance.Observe(c, rt, do)
			var out v1alpha1.CriterionObservation
			b, err := json.Marshal(o)
			if err != nil {
				e.t.Fatal(err)
			}
			if err := json.Unmarshal(b, &out); err != nil {
				e.t.Fatal(err)
			}
			pv.Status.Observations = append(pv.Status.Observations, out)
		}
	}
	if err := e.c.Update(context.Background(), &pv); err != nil {
		e.t.Fatal(err)
	}
}

// answering is a Doer answering every request with status.
func answering(status int32) acceptance.Doer {
	return func(acceptance.Call) acceptance.Response { return acceptance.Response{Status: status} }
}

// flaky answers the first try with 503 and the second with 200.
func flaky(c acceptance.Call) acceptance.Response {
	if c.Try == 1 {
		return acceptance.Response{Status: 503}
	}
	return acceptance.Response{Status: 200}
}

// readyPreview drives intent name's Preview to Ready at its spec.
func (e *env) readyPreview(name string) *v1alpha1.Preview {
	e.t.Helper()
	e.servePreview(name, v1alpha1.PreviewReady, "https://"+previewHostOf(name))
	return e.previewOf(name)
}

// TestVerificationOffWritesNothing: without --intent-verification an
// intent in review gets no acceptance, no verification state and no
// PreviewVerification, even on a verifying Project with a Ready Preview.
func TestVerificationOffWritesNothing(t *testing.T) {
	e := newVerifyEnv(t, acceptancePlan)
	e.intent.Settings.Verification = false
	name := e.reviewing()
	e.readyPreview(name)
	e.review(name, 4)
	in := e.get(name)
	if in.Status.Acceptance != nil || in.Status.Verification != nil || len(e.verifications(name)) != 0 {
		t.Fatalf("acceptance %+v verification %+v verifications %d", in.Status.Acceptance,
			in.Status.Verification, len(e.verifications(name)))
	}
}

// TestVerificationRecordsTheApprovedCriteria: the approved plan's criteria
// are recorded once review begins, normalised (the shorthand folded into one
// step, defaults explicit) and digested as the plan declared them.
func TestVerificationRecordsTheApprovedCriteria(t *testing.T) {
	e := newVerifyEnv(t, acceptancePlan)
	name := e.reviewing()
	e.review(name, 2)
	in := e.get(name)
	acc := in.Status.Acceptance
	if acc == nil {
		t.Fatal("no acceptance recorded")
	}
	parsed, err := report.ParsePlan([]byte(acceptancePlan))
	if err != nil {
		t.Fatal(err)
	}
	if acc.PlanRevision != in.Status.Approval.PlanRevision || acc.Digest != acceptance.Digest(parsed.Acceptance) {
		t.Fatalf("acceptance = r%d %s, want r%d %s", acc.PlanRevision, acc.Digest,
			in.Status.Approval.PlanRevision, acceptance.Digest(parsed.Acceptance))
	}
	if len(acc.Criteria) != 2 || acc.Criteria[0].ID != "health" || len(acc.Criteria[0].Steps) != 1 ||
		acc.Criteria[0].Steps[0].Request.Method != "GET" || acc.Criteria[0].Steps[0].Request.Path != "/healthz" ||
		acc.Criteria[1].Kind != v1alpha1.AcceptanceManual {
		t.Fatalf("criteria = %+v", acc.Criteria)
	}
	back, err := crdconv.CriteriaFromCRD(acc.Criteria)
	if err != nil || acceptance.Digest(back) != acc.Digest {
		t.Fatalf("stored criteria digest %s, err %v; want %s", acceptance.Digest(back), err, acc.Digest)
	}
	if v := in.Status.Verification; v == nil || v.AutoFix.State != v1alpha1.AutoFixArmed {
		t.Fatalf("verification = %+v, want armed", v)
	}
}

// TestVerificationOffOnAProjectThatDoesNotVerify: with the controller
// verifying, a Project without verification reports Off, and an approver's
// request there is consumed rather than left waiting.
func TestVerificationOffOnAProjectThatDoesNotVerify(t *testing.T) {
	e := newVerifyEnv(t, acceptancePlan, func(p *v1alpha1.Project) { p.Spec.Verification = nil })
	name := e.reviewing()
	e.setStatus(name, func(in *v1alpha1.Intent) {
		in.Status.Verification = &v1alpha1.IntentVerification{Requested: &v1alpha1.VerifyRequest{By: approver,
			ByID: 7, At: metav1.NewTime(e.clock.Now()), CommentID: 99}}
	})
	e.readyPreview(name)
	e.review(name, 3)
	v := e.verification(name)
	if v.State != v1alpha1.VerificationOff || v.Requested != nil || len(e.verifications(name)) != 0 {
		t.Fatalf("verification = %+v, verifications %d", v, len(e.verifications(name)))
	}
}

// TestVerificationNotDeclared: a plan declaring no criteria is NotDeclared
// and nothing runs.
func TestVerificationNotDeclared(t *testing.T) {
	e := newVerifyEnv(t, "")
	name := e.reviewing()
	e.readyPreview(name)
	e.review(name, 3)
	in := e.get(name)
	if in.Status.Acceptance == nil || len(in.Status.Acceptance.Criteria) != 0 ||
		in.Status.Verification.State != v1alpha1.VerificationNotDeclared || len(e.verifications(name)) != 0 {
		t.Fatalf("acceptance %+v verification %+v", in.Status.Acceptance, in.Status.Verification)
	}
}

// TestVerificationWithoutPreviews: a controller started with
// --intent-verification but not --intent-previews-enabled (serve warns
// rather than refusing to start) runs with verification Off. A verifying
// Project stays Ready and says why in its VerificationOff condition; an
// intent of it in review is Off, nothing is created, no maul/acceptance is
// posted, and the issue's status comment says why. With previews on there
// is no such condition.
func TestVerificationWithoutPreviews(t *testing.T) {
	e := newVerifyEnv(t, acceptancePlan)
	e.reconcileProject()
	if c := meta.FindStatusCondition(e.getProject().Status.Conditions, ConditionVerificationOff); c != nil {
		t.Fatalf("%s with previews on: %+v", ConditionVerificationOff, c)
	}
	e.intent.Settings.Previews = false
	e.project.Settings.Verification, e.project.Settings.Previews = true, false
	e.reconcileProject()
	conds := e.getProject().Status.Conditions
	c := meta.FindStatusCondition(conds, ConditionVerificationOff)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != ReasonPreviewsDisabled ||
		!strings.Contains(c.Message, "--intent-previews-enabled") || !meta.IsStatusConditionTrue(conds,
		v1alpha1.ConditionReady) {
		t.Fatalf("project conditions = %+v", conds)
	}
	name := e.reviewing()
	e.review(name, 6)
	v := e.verification(name)
	if v.State != v1alpha1.VerificationOff || len(e.verifications(name)) != 0 {
		t.Fatalf("verification = %+v, verifications %d", v, len(e.verifications(name)))
	}
	if ss := e.acceptanceStatuses(e.get(name).Status.PullRequests[0].HeadSHA); len(ss) != 0 {
		t.Fatalf("statuses posted with verification off: %+v", ss)
	}
	if body := e.statusBody(); !strings.Contains(body, "**Verification:** off") ||
		!strings.Contains(body, "`--intent-previews-enabled`") {
		t.Fatalf("status comment does not say why verification is off:\n%s", body)
	}
}

// TestVerificationWaitsForThePreview: nothing runs while the Preview is not
// live at the heads.
func TestVerificationWaitsForThePreview(t *testing.T) {
	e := newVerifyEnv(t, acceptancePlan)
	name := e.reviewing()
	e.previewOf(name)
	e.review(name, 3)
	if v := e.verification(name); v.State != v1alpha1.VerificationWaitingForPreview || len(e.verifications(name)) != 0 {
		t.Fatalf("verification = %+v", v)
	}
}

// TestVerificationGivesUpWaitingForThePreview: a Preview that never goes
// live at the heads (Failed, Expired, or never served: no preview-controller)
// holds the verification only for previewWaitMax. Then it is Unavailable
// (PreviewNotLive), maul/acceptance is an error saying why, and the issue's
// verification comment says so; nothing is created meanwhile. Once the
// Preview is live it verifies after all.
func TestVerificationGivesUpWaitingForThePreview(t *testing.T) {
	for _, tc := range []struct {
		name  string
		phase v1alpha1.PreviewPhase
	}{{"failed", v1alpha1.PreviewFailed}, {"expired", v1alpha1.PreviewExpired}, {"never served", ""}} {
		t.Run(tc.name, func(t *testing.T) {
			e := newVerifyEnv(t, acceptancePlan)
			name := e.reviewing()
			if tc.phase != "" {
				e.servePreview(name, tc.phase, "")
			} else {
				e.previewOf(name)
			}
			e.review(name, 6)
			v := e.verification(name)
			if v.State != v1alpha1.VerificationWaitingForPreview || v.PreviewWaitSince == nil {
				t.Fatalf("verification = %+v, want waiting with its start recorded", v)
			}
			head := e.get(name).Status.PullRequests[0].HeadSHA
			if st := e.latestAcceptance(head); st.State != statusPending {
				t.Fatalf("status while waiting = %+v", st)
			}
			e.clock.Advance(previewWaitMax - 10*time.Minute)
			e.review(name, 2)
			if v := e.verification(name); v.State != v1alpha1.VerificationWaitingForPreview {
				t.Fatalf("gave up before the bound: %+v", v)
			}
			e.review(name, 6)
			v = e.verification(name)
			if v.State != v1alpha1.VerificationUnavailable ||
				v.UnavailableReason != v1alpha1.VerificationUnavailablePreviewNotLive || len(e.verifications(name)) != 0 {
				t.Fatalf("verification after the bound = %+v, verifications %d", v, len(e.verifications(name)))
			}
			st := e.latestAcceptance(head)
			if st.State != statusError || !strings.Contains(st.Description, "preview") || len(st.Description) > 140 {
				t.Fatalf("status after the bound = %+v", st)
			}
			cs := e.verifyComments(name, 1)
			if len(cs) != 1 || !strings.Contains(cs[0].Body, "the preview did not become live") {
				t.Fatalf("verification comments = %+v", cs)
			}
			e.review(name, 3)
			if v := e.verification(name); v.State != v1alpha1.VerificationUnavailable {
				t.Fatalf("did not stay unavailable: %+v", v)
			}
			e.readyPreview(name)
			e.review(name, 3)
			v = e.verification(name)
			if v.State != v1alpha1.VerificationVerifying || v.UnavailableReason != "" || len(e.verifications(name)) != 1 {
				t.Fatalf("verification once the preview is live = %+v, verifications %d", v,
					len(e.verifications(name)))
			}
		})
	}
}

// TestVerificationWaitsForCI: a named check not yet green at the head holds
// the verification.
func TestVerificationWaitsForCI(t *testing.T) {
	e := newVerifyEnv(t, acceptancePlan, func(p *v1alpha1.Project) { p.Spec.Checks.Fix = []string{"test"} })
	name := e.reviewing()
	e.readyPreview(name)
	head := e.get(name).Status.PullRequests[0].HeadSHA
	e.gh.mu.Lock()
	e.gh.checks[head] = []ghclient.CheckRun{{ID: 5, Name: "test", HeadSHA: head, Status: "in_progress"}}
	e.gh.mu.Unlock()
	e.review(name, 3)
	if v := e.verification(name); v.State != v1alpha1.VerificationWaitingForCI || len(e.verifications(name)) != 0 {
		t.Fatalf("verification = %+v", v)
	}
	e.gh.mu.Lock()
	e.gh.checks[head] = []ghclient.CheckRun{{ID: 5, Name: "test", HeadSHA: head, Status: "completed",
		Conclusion: "success"}}
	e.gh.mu.Unlock()
	e.review(name, 2)
	if n := len(e.verifications(name)); n != 1 {
		t.Fatalf("verifications once green = %d, want 1", n)
	}
}

// TestVerificationCreatesTheRequest: once the Preview is live at the heads,
// one PreviewVerification is created, owned by the Intent, pinned to it and
// its Preview by UID, carrying the heads, the automatable criteria in plan
// order and the full list's digest, with the Project's timeout clamped.
func TestVerificationCreatesTheRequest(t *testing.T) {
	e := newVerifyEnv(t, acceptancePlan, func(p *v1alpha1.Project) {
		p.Spec.Verification.Timeout = &metav1.Duration{Duration: 45 * time.Minute}
	})
	name := e.reviewing()
	preview := e.readyPreview(name)
	e.review(name, 3)
	in := e.get(name)
	pv := e.onlyVerification(name)
	if pv.Name != v1alpha1.PreviewVerificationName(name, 1, 1) || !controlledBy(pv.OwnerReferences, in.UID) {
		t.Fatalf("verification %s owners %+v", pv.Name, pv.OwnerReferences)
	}
	checkRequestSpec(t, in, preview, pv.Spec)
	v := in.Status.Verification
	if v.State != v1alpha1.VerificationVerifying || v.Round != 1 || v.Attempt != 1 || v.Ref == nil ||
		v.Ref.UID != pv.UID || v.HeadSetDigest != pv.Spec.HeadSetDigest || v.Automatic != 1 || v.LastAutomaticAt == nil {
		t.Fatalf("verification = %+v", v)
	}
	e.review(name, 3)
	if n := len(e.verifications(name)); n != 1 {
		t.Fatalf("an in-flight verification was created again: %d", n)
	}
}

// checkRequestSpec holds a first verification's spec to in and its
// Preview.
func checkRequestSpec(t *testing.T, in *v1alpha1.Intent, preview *v1alpha1.Preview,
	s v1alpha1.PreviewVerificationSpec) {
	t.Helper()
	head := in.Status.PullRequests[0].HeadSHA
	wantHeads := []v1alpha1.VerificationHead{{Component: "app", Repository: appRepoURL, Revision: head}}
	if s.IntentRef.UID != in.UID || s.PreviewRef.UID != preview.UID || s.PreviewRef.Name != preview.Name ||
		s.PreviewGeneration != preview.Generation || !slices.Equal(s.Heads, wantHeads) ||
		s.HeadSetDigest != headSetDigest(wantHeads) || s.Round != 1 || s.Attempt != 1 ||
		!regexp.MustCompile(`^[0-9a-f]{6}$`).MatchString(s.Run) || s.Timeout.Duration != 20*time.Minute {
		t.Fatalf("spec = %+v", s)
	}
	if len(s.Criteria) != 1 || s.Criteria[0].ID != "health" || s.CriteriaDigest != in.Status.Acceptance.Digest {
		t.Fatalf("criteria = %+v digest %s", s.Criteria, s.CriteriaDigest)
	}
}

// TestVerificationJudgesTheObservations: maul judges what the tester saw:
// a pass, a failure (with its fingerprint), and a pass only on retry
// (quarantined, never counted as passed). A judged head set is not run again.
func TestVerificationJudgesTheObservations(t *testing.T) {
	for _, tc := range []struct {
		name  string
		do    acceptance.Doer
		state v1alpha1.IntentVerificationState
		check func(*testing.T, *v1alpha1.IntentVerification)
	}{
		{"pass", answering(200), v1alpha1.VerificationPassed, func(t *testing.T, v *v1alpha1.IntentVerification) {
			if v.Passed != 1 || v.Failed != 0 || v.Quarantined != 0 || len(v.Failing) != 0 || v.ManualOpen != 1 {
				t.Errorf("counts = %+v", v)
			}
		}},
		{"fail", answering(500), v1alpha1.VerificationFailing, func(t *testing.T, v *v1alpha1.IntentVerification) {
			if v.Failed != 1 || len(v.Failing) != 1 || v.Failing[0].ID != "health" ||
				v.Failing[0].Outcome != v1alpha1.VerdictFail ||
				!strings.HasPrefix(v.Failing[0].Fingerprint, acceptance.FingerprintVersion+":") {
				t.Errorf("failing = %+v", v)
			}
		}},
		{"quarantined", flaky, v1alpha1.VerificationPassed, func(t *testing.T, v *v1alpha1.IntentVerification) {
			if v.Passed != 0 || v.Quarantined != 1 || !slices.Equal(v.QuarantinedIDs, []string{"health"}) {
				t.Errorf("quarantine = %+v", v)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newVerifyEnv(t, acceptancePlan)
			name := e.reviewing()
			e.readyPreview(name)
			e.review(name, 3)
			pv := e.onlyVerification(name)
			e.observe(pv.Name, v1alpha1.VerificationObserved, tc.do)
			// The judgement is the pass's one write: a failure's fix round
			// (verify_round.go) starts on a later one.
			e.review(name, 1)
			v := e.verification(name)
			if v.State != tc.state || v.Ref == nil || v.Ref.UID != pv.UID {
				t.Fatalf("verification = %+v, want %s", v, tc.state)
			}
			tc.check(t, v)
			e.review(name, 2)
			if n := len(e.verifications(name)); n != 1 {
				t.Fatalf("a judged head set ran again: %d verifications", n)
			}
		})
	}
}

// TestVerificationInfraRetriesThenUnavailable: maul's own failures are
// retried as new attempts of the same round, uncounted, and after the last
// the verification is unavailable.
func TestVerificationInfraRetriesThenUnavailable(t *testing.T) {
	e := newVerifyEnv(t, acceptancePlan)
	name := e.reviewing()
	e.readyPreview(name)
	e.review(name, 3)
	for attempt := int32(1); attempt <= v1alpha1.MaxVerificationAttempts; attempt++ {
		pvName := v1alpha1.PreviewVerificationName(name, 1, attempt)
		e.observe(pvName, v1alpha1.VerificationInfra, nil)
		e.review(name, 3)
	}
	v := e.verification(name)
	if v.State != v1alpha1.VerificationUnavailable || v.Round != 1 || v.Attempt != 3 || v.Automatic != 1 {
		t.Fatalf("verification = %+v", v)
	}
	if n := len(e.verifications(name)); n != 3 {
		t.Fatalf("verifications = %d, want 3 attempts", n)
	}
}

// TestVerificationEnvFailedChargesTheIntent: a component that crashed is
// the change's failure: Failing, not retried.
func TestVerificationEnvFailedChargesTheIntent(t *testing.T) {
	e := newVerifyEnv(t, acceptancePlan)
	name := e.reviewing()
	e.readyPreview(name)
	e.review(name, 3)
	pv := e.onlyVerification(name)
	e.observe(pv.Name, v1alpha1.VerificationEnvFailed, nil, v1alpha1.PartFact{Name: "app",
		Role: v1alpha1.PreviewRoleComponent, Waiting: "CrashLoopBackOff", Restarts: 4})
	e.review(name, 1)
	if v := e.verification(name); v.State != v1alpha1.VerificationFailing || len(e.verifications(name)) != 1 {
		t.Fatalf("verification = %+v", v)
	}
	e.review(name, 2)
	if n := len(e.verifications(name)); n != 1 {
		t.Fatalf("a judged environment failure was retried: %d verifications", n)
	}
}

// TestVerificationSupersededByAMovedHead: when the combined change moves,
// the in-flight verification is deleted and the state starts over at the
// new heads; a head maul did not push halts automatic fixes.
func TestVerificationSupersededByAMovedHead(t *testing.T) {
	e := newVerifyEnv(t, acceptancePlan)
	name := e.reviewing()
	e.readyPreview(name)
	e.review(name, 3)
	pv := e.onlyVerification(name)
	n := e.get(name).Status.PullRequests[0].Number
	moved := strings.Repeat("f", 40)
	e.pushHead(n, moved)
	e.review(name, 3)
	var gone v1alpha1.PreviewVerification
	if err := e.c.Get(context.Background(), client.ObjectKeyFromObject(&pv), &gone); !kerrors.IsNotFound(err) {
		t.Fatalf("the superseded verification is still there: %v", err)
	}
	v := e.verification(name)
	want := headSetDigest([]v1alpha1.VerificationHead{{Component: "app", Repository: appRepoURL, Revision: moved}})
	if v.HeadSetDigest != want || v.Ref != nil || v.AutoFix.State != v1alpha1.AutoFixHalted ||
		v.AutoFix.Reason != v1alpha1.AutoFixHumanPush {
		t.Fatalf("verification = %+v", v)
	}
}

// TestVerificationAutomaticBounds: an automatic verification needs the
// Project's maxVerifications not spent, and while auto-fix is halted, five
// minutes since the last.
func TestVerificationAutomaticBounds(t *testing.T) {
	e := newVerifyEnv(t, acceptancePlan)
	name := e.reviewing()
	e.readyPreview(name)
	e.review(name, 2)
	e.setStatus(name, func(in *v1alpha1.Intent) {
		at := metav1.NewTime(e.clock.Now())
		in.Status.Verification.Automatic = 20
		in.Status.Verification.LastAutomaticAt = &at
	})
	e.review(name, 3)
	if n := len(e.verifications(name)); n != 0 {
		t.Fatalf("maxVerifications spent, yet %d verifications", n)
	}
	e.setStatus(name, func(in *v1alpha1.Intent) {
		at := metav1.NewTime(e.clock.Now())
		in.Status.Verification.Automatic = 1
		in.Status.Verification.LastAutomaticAt = &at
		in.Status.Verification.AutoFix = v1alpha1.AutoFix{State: v1alpha1.AutoFixHalted,
			Reason: v1alpha1.AutoFixHumanPush}
	})
	e.review(name, 3) // three minutes
	if n := len(e.verifications(name)); n != 0 {
		t.Fatalf("halted and within five minutes, yet %d verifications", n)
	}
	e.review(name, 3)
	if n := len(e.verifications(name)); n != 1 {
		t.Fatalf("halted past five minutes: %d verifications, want 1", n)
	}
}

// TestVerificationRequestRunsAgain: an approver's /maul verify runs a new
// round at a head set already judged, uncounted, and consumes the request;
// a second request within five minutes of the first waits.
func TestVerificationRequestRunsAgain(t *testing.T) {
	e := newVerifyEnv(t, acceptancePlan)
	name := e.reviewing()
	e.readyPreview(name)
	e.review(name, 3)
	e.observe(v1alpha1.PreviewVerificationName(name, 1, 1), v1alpha1.VerificationObserved, answering(200))
	e.review(name, 2)
	request := func(id int64) {
		e.setStatus(name, func(in *v1alpha1.Intent) {
			in.Status.Verification.Requested = &v1alpha1.VerifyRequest{By: approver, ByID: 7,
				At: metav1.NewTime(e.clock.Now()), CommentID: id}
		})
	}
	request(101)
	e.review(name, 1)
	v := e.verification(name)
	if v.Requested != nil || v.Round != 2 || v.Automatic != 1 || len(e.verifications(name)) != 2 {
		t.Fatalf("after a request: %+v, %d verifications", v, len(e.verifications(name)))
	}
	e.observe(v1alpha1.PreviewVerificationName(name, 2, 1), v1alpha1.VerificationObserved, answering(200))
	e.review(name, 1)
	request(102)
	e.review(name, 2)
	if v := e.verification(name); v.Requested == nil || len(e.verifications(name)) != 2 {
		t.Fatalf("a request within five minutes ran: %+v", v)
	}
	e.review(name, 3)
	if v := e.verification(name); v.Requested != nil || v.Round != 3 {
		t.Fatalf("the request did not run once five minutes passed: %+v", v)
	}
}

// TestVerificationNeedsReplan: a criterion whose target is no longer a
// previewed component cannot run as approved.
func TestVerificationNeedsReplan(t *testing.T) {
	e := newVerifyEnv(t, strings.Replace(acceptancePlan, "target: app", "target: web", 1))
	name := e.reviewing()
	e.readyPreview(name)
	e.review(name, 3)
	in := e.get(name)
	v := in.Status.Verification
	if v.State != v1alpha1.VerificationNeedsReplan || v.AutoFix.State != v1alpha1.AutoFixHalted ||
		v.AutoFix.Reason != v1alpha1.AutoFixCriterionError || len(e.verifications(name)) != 0 {
		t.Fatalf("verification = %+v", v)
	}
	c := meta.FindStatusCondition(in.Status.Conditions, v1alpha1.ConditionVerifyFailing)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != string(v1alpha1.AutoFixCriterionError) {
		t.Fatalf("condition = %+v", c)
	}
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

// TestCriteriaConversionIsStrict: criteria the CRD cannot hold as written
// (the shorthand left unfolded) are refused, never silently dropped.
func TestCriteriaConversionIsStrict(t *testing.T) {
	parsed, err := report.ParsePlan([]byte(acceptancePlan))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := crdconv.CriteriaToCRD(parsed.Acceptance); err == nil {
		t.Fatal("the unnormalised shorthand converted")
	}
	got, err := crdconv.CriteriaToCRD(acceptance.Normalize(parsed.Acceptance))
	if err != nil {
		t.Fatal(err)
	}
	back, err := crdconv.CriteriaFromCRD(got)
	if err != nil || acceptance.Digest(back) != acceptance.Digest(parsed.Acceptance) {
		t.Fatalf("round trip: %v", err)
	}
}

// TestVerifyTimeoutIsClamped: the Project's timeout, its default, clamped
// by the controller's maximum.
func TestVerifyTimeoutIsClamped(t *testing.T) {
	s := Settings{VerifyTimeoutMax: 15 * time.Minute}
	p := &v1alpha1.Project{}
	if got := s.verifyTimeout(p); got != 10*time.Minute {
		t.Errorf("default = %s", got)
	}
	p.Spec.Verification = &v1alpha1.ProjectVerification{Timeout: &metav1.Duration{Duration: time.Hour}}
	if got := s.verifyTimeout(p); got != 15*time.Minute {
		t.Errorf("clamped = %s", got)
	}
	if got := (Settings{}).verifyTimeout(p); got != DefaultVerifyTimeoutMax {
		t.Errorf("unset maximum = %s", got)
	}
}

// TestVerificationStopsWhenTheApprovedCriteriaCannotBeRead: when the
// approved plan revision's criteria cannot be read (its plan no longer
// matches the approval's digest) and those on record are another
// revision's, nothing is verified against criteria the approver no longer
// approved: the state is Unavailable, the Intent says why, and
// maul/acceptance is an error, never a verdict from the old criteria.
func TestVerificationStopsWhenTheApprovedCriteriaCannotBeRead(t *testing.T) {
	e := newVerifyEnv(t, acceptancePlan)
	name := e.reviewing()
	e.review(name, 2)
	in := e.get(name)
	var cm corev1.ConfigMap
	key := client.ObjectKey{Namespace: testNS, Name: planConfigMapName(name, in.Status.Approval.PlanRevision)}
	if err := e.c.Get(context.Background(), key, &cm); err != nil {
		t.Fatal(err)
	}
	cm.Data[keyPlan] += "\n"
	if err := e.c.Update(context.Background(), &cm); err != nil {
		t.Fatal(err)
	}
	e.setStatus(name, func(in *v1alpha1.Intent) {
		in.Status.Acceptance.PlanRevision = in.Status.Approval.PlanRevision + 1
	})
	e.readyPreview(name)
	e.review(name, 6)
	if n := len(e.verifications(name)); n != 0 {
		t.Fatalf("verified %d times against criteria the approver no longer approved", n)
	}
	in = e.get(name)
	if v := in.Status.Verification; v == nil || v.State != v1alpha1.VerificationUnavailable {
		t.Fatalf("verification = %+v, want Unavailable", v)
	}
	c := meta.FindStatusCondition(in.Status.Conditions, v1alpha1.ConditionVerifyFailing)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != reasonCriteriaUnreadable {
		t.Fatalf("condition = %+v", c)
	}
	s := e.latestAcceptance(in.Status.PullRequests[0].HeadSHA)
	if s.State != "error" || !strings.Contains(s.Description, "cannot be read") {
		t.Fatalf("status = %+v", s)
	}
}
