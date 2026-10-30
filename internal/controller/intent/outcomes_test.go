// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/envelope"
	"github.com/devthenet-labs/maul/internal/jobs"
	"github.com/devthenet-labs/maul/internal/report"
	"github.com/devthenet-labs/maul/internal/templates"
)

// outcomesPlan is validPlan stating three outcomes for the agent
// evaluator: a change answering one issue line, a keep, and a manual one.
var outcomesPlan = strings.Replace(validPlan, "estimated_token_budget: 200000\n",
	"estimated_token_budget: 200000\n"+
		"acceptance:\n"+
		"  - id: version-shown\n"+
		"    outcome: \"GET /version answers with the commit it runs\"\n"+
		"    from: \"Show the version\"\n"+
		"  - id: still-healthy\n"+
		"    outcome: the app still answers its health check\n"+
		"    kind: keep\n"+
		"  - id: looks-right\n"+
		"    outcome: the version page looks right\n"+
		"    manual: true\n"+
		"    reason: only a human can judge it\n", 1)

// outcomesIssue is a request whose list items outcomesPlan answers one of.
const outcomesIssue = "Please:\n\n- Show the version\n- Show the build time\n"

// evaluating switches the agent evaluator on or off in every reconciler.
func (e *env) evaluating(on bool) {
	for _, s := range []*Settings{&e.project.Settings, &e.intent.Settings, &e.runs.Settings} {
		s.Evaluator = on
	}
}

// awaitingWith drives a new intent whose issue body is body to
// AwaitingApproval.
func (e *env) awaitingWith(body string) string {
	e.t.Helper()
	name := e.newIntent(approver)
	e.gh.mu.Lock()
	e.gh.issues[1].body = body
	e.gh.mu.Unlock()
	e.drive(name, v1alpha1.IntentAwaitingApproval, repoImage)
	return name
}

// TestEvaluatorAsksThePlanForOutcomes: under the evaluator a verifying
// Project's plan Job is told to state outcomes; without it the handoff is
// slice 1's, naming none.
func TestEvaluatorAsksThePlanForOutcomes(t *testing.T) {
	for _, on := range []bool{true, false} {
		e := newVerifyEnv(t, outcomesPlan)
		e.verifying(true, true)
		e.evaluating(on)
		e.awaiting()
		plans := e.specsOf("plan")
		if len(plans) == 0 {
			t.Fatal("no plan Job launched")
		}
		got, err := templates.DecodePlanVerification(plans[0].PlanVerification)
		if err != nil || got == nil || got.Outcomes != on {
			t.Errorf("evaluator %t: plan Job handed %q (%v), want outcomes %t", on, plans[0].PlanVerification, err, on)
		}
	}
}

// TestEvaluatorPlanCommentListsOutcomes: the plan comment shows each
// outcome with the issue line it answers and lists the issue lines none
// answers, in place of slice 1's acceptance line. Evaluator off, the same
// plan's comment is slice 1's, with no outcome section.
func TestEvaluatorPlanCommentListsOutcomes(t *testing.T) {
	for _, on := range []bool{true, false} {
		e := newVerifyEnv(t, outcomesPlan)
		e.verifying(true, true)
		e.evaluating(on)
		e.awaitingWith(outcomesIssue)
		plans := e.gh.withMarker("maul:plan")
		if len(plans) != 1 {
			t.Fatalf("plan comments = %d, want 1", len(plans))
		}
		body := plans[0].Body
		if !on {
			if strings.Contains(body, "Acceptance outcomes") || !strings.Contains(body, noneLine) {
				t.Errorf("evaluator off: comment is not slice 1's:\n%s", body)
			}
			continue
		}
		for _, want := range []string{
			"### Acceptance outcomes",
			"- `version-shown`: GET /version answers with the commit it runs. It answers the issue's line: Show the version",
			"- `still-healthy` (must keep holding): the app still answers its health check",
			"- `looks-right` (manual: only a human can judge it): the version page looks right",
			"No outcome answers these lines of the issue",
			"- Show the build time",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("plan comment lacks %q:\n%s", want, body)
			}
		}
		if strings.Contains(body, "**Acceptance:**") || strings.Contains(body, "- Show the version\n") {
			t.Errorf("plan comment carries slice 1's line or a covered issue line:\n%s", body)
		}
	}
}

// TestEvaluatorRecordsTheApprovedOutcomes: once in review, the approved
// plan's outcomes are recorded, normalised (every kind explicit) and
// digested as declared, with no criteria; evaluator off, the same plan
// records neither.
func TestEvaluatorRecordsTheApprovedOutcomes(t *testing.T) {
	e := newVerifyEnv(t, outcomesPlan)
	e.evaluating(true)
	name := e.reviewing()
	e.review(name, 2)
	in := e.get(name)
	acc := in.Status.Acceptance
	if acc == nil {
		t.Fatal("no acceptance recorded")
	}
	parsed, err := report.ParsePlan([]byte(outcomesPlan))
	if err != nil {
		t.Fatal(err)
	}
	if acc.PlanRevision != in.Status.Approval.PlanRevision ||
		acc.OutcomesDigest != acceptance.OutcomesDigest(parsed.Outcomes) || len(acc.Criteria) != 0 {
		t.Fatalf("acceptance = %+v", acc)
	}
	want := []v1alpha1.AcceptanceOutcome{
		{ID: "version-shown", Outcome: "GET /version answers with the commit it runs", From: "Show the version",
			Kind: v1alpha1.OutcomeKindChange},
		{ID: "still-healthy", Outcome: "the app still answers its health check", Kind: v1alpha1.OutcomeKindKeep},
		{ID: "looks-right", Outcome: "the version page looks right", Kind: v1alpha1.OutcomeKindChange, Manual: true,
			Reason: "only a human can judge it"},
	}
	if len(acc.Outcomes) != len(want) {
		t.Fatalf("outcomes = %+v", acc.Outcomes)
	}
	for i := range want {
		if acc.Outcomes[i] != want[i] {
			t.Errorf("outcome %d = %+v, want %+v", i, acc.Outcomes[i], want[i])
		}
	}

	off := newVerifyEnv(t, outcomesPlan)
	name = off.reviewing()
	off.review(name, 2)
	if acc := off.get(name).Status.Acceptance; acc == nil || len(acc.Outcomes) != 0 || acc.OutcomesDigest != "" {
		t.Fatalf("evaluator off: acceptance = %+v, want slice 1's record", acc)
	}
}

// TestEvaluatorRefusesACriteriaPlan: under the evaluator a plan stating
// slice-1 criteria is refused when its run is collected, and the retry is
// told to state outcomes; nothing is posted for approval.
func TestEvaluatorRefusesACriteriaPlan(t *testing.T) {
	e := newVerifyEnv(t, "")
	e.verifying(true, true)
	e.evaluating(true)
	e.jobs.output = func(spec jobs.Spec) jobs.RunOutput {
		if spec.Phase == "plan" {
			return planOutput(envelope.Stage{Outcome: envelope.OutcomeOK}, acceptancePlan)
		}
		return defaultOutput(spec)
	}
	name := e.newIntent(approver)
	e.drive(name, v1alpha1.IntentFailed, "")
	runs := e.runsOf(name, v1alpha1.IntentStagePlan)
	if len(runs) < 2 {
		t.Fatalf("plan runs = %d, want a refused plan retried", len(runs))
	}
	for _, r := range runs {
		if r.Status.Outcome != "report_invalid" || !strings.Contains(r.Status.Detail, "slice-1 criteria") {
			t.Errorf("run %s = %s: %s, want report_invalid naming the criteria", r.Name, r.Status.Outcome,
				r.Status.Detail)
		}
	}
	if prev := runs[1].Spec.PreviousAttempt; prev == nil || !strings.Contains(prev.Detail, "outcomes") {
		t.Errorf("the retry was not told why: %+v", prev)
	}
	if n := len(e.gh.withMarker("maul:plan")); n != 0 {
		t.Errorf("a criteria plan was posted for approval (%d)", n)
	}
}

// checkStoppedForReplan asserts an intent stopped for a replan under the
// evaluator: one notice, nothing verified, no criteria on record, fixes
// halted, the condition, the status and the status comment saying why. It
// returns the notice's key.
func (e *env) checkStoppedForReplan(name string) string {
	t := e.t
	t.Helper()
	in := e.get(name)
	key := templates.CriteriaReplanKey(in.Status.Approval.PlanRevision)
	if n := len(e.gh.withMarker(key)); n != 1 {
		t.Fatalf("replan notices = %d, want 1", n)
	}
	if n := len(e.verifications(name)); n != 0 {
		t.Fatalf("verified %d times against slice-1 criteria under the evaluator", n)
	}
	if in.Status.Acceptance != nil {
		t.Errorf("criteria still on record: %+v", in.Status.Acceptance)
	}
	if v := in.Status.Verification; v == nil || v.State != v1alpha1.VerificationUnavailable ||
		v.AutoFix.State != v1alpha1.AutoFixHalted {
		t.Fatalf("verification = %+v, want Unavailable, fixes halted", v)
	}
	c := meta.FindStatusCondition(in.Status.Conditions, v1alpha1.ConditionVerifyFailing)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != reasonCriteriaReplan {
		t.Fatalf("condition = %+v", c)
	}
	s := e.latestAcceptance(in.Status.PullRequests[0].HeadSHA)
	if s.State != "error" || !strings.Contains(s.Description, "nothing is verified") {
		t.Fatalf("status = %+v", s)
	}
	if status := e.gh.withMarker("maul:intent"); len(status) == 0 ||
		!strings.Contains(status[len(status)-1].Body, "the approved plan states acceptance criteria") {
		t.Errorf("status comment does not say why nothing is verified")
	}
	return key
}

// TestEvaluatorMigratesAnApprovedCriteriaPlan: an intent in review whose
// approved plan states slice-1 criteria, when the evaluator is switched on,
// gets one replan notice, however many passes follow; nothing is verified
// against its criteria, which are no longer on record, the Intent says why
// and maul/acceptance is an error. Switched off again, slice 1 reads the
// criteria afresh and verification resumes.
func TestEvaluatorMigratesAnApprovedCriteriaPlan(t *testing.T) {
	e := newVerifyEnv(t, acceptancePlan)
	name := e.reviewing()
	e.review(name, 2)
	if acc := e.get(name).Status.Acceptance; acc == nil || len(acc.Criteria) != 2 {
		t.Fatalf("slice 1 recorded %+v", acc)
	}
	e.evaluating(true)
	e.readyPreview(name)
	e.review(name, 6)

	key := e.checkStoppedForReplan(name)

	// A request made meanwhile is consumed, and still only one notice.
	e.setStatus(name, func(in *v1alpha1.Intent) {
		in.Status.Verification.Requested = &v1alpha1.VerifyRequest{By: approver, ByID: 7,
			At: metav1.NewTime(e.clock.Now()), CommentID: 99}
	})
	e.review(name, 3)
	if v := e.verification(name); v.Requested != nil {
		t.Errorf("request left waiting: %+v", v.Requested)
	}
	if n := len(e.gh.withMarker(key)); n != 1 {
		t.Fatalf("replan notices = %d after more passes, want 1", n)
	}

	// Switched off: slice 1 runs unchanged.
	e.evaluating(false)
	e.review(name, 3)
	in := e.get(name)
	if acc := in.Status.Acceptance; acc == nil || len(acc.Criteria) != 2 {
		t.Fatalf("evaluator off again: acceptance = %+v", acc)
	}
	if c := meta.FindStatusCondition(in.Status.Conditions, v1alpha1.ConditionVerifyFailing); c != nil &&
		c.Reason == reasonCriteriaReplan {
		t.Errorf("condition kept: %+v", c)
	}
}

// TestEvaluatorReplanToOutcomes: a criteria plan waiting for approval when
// the evaluator is switched on is replanned; the new plan Job is asked for
// outcomes, its plan lists them for approval, and once approved and in
// review its outcomes are what is recorded, with no replan notice.
func TestEvaluatorReplanToOutcomes(t *testing.T) {
	e := newVerifyEnv(t, "")
	e.verifying(true, true)
	plan := acceptancePlan
	e.jobs.output = func(spec jobs.Spec) jobs.RunOutput {
		if spec.Phase == "plan" {
			return planOutput(envelope.Stage{Outcome: envelope.OutcomeOK}, plan)
		}
		return defaultOutput(spec)
	}
	name := e.awaitingWith(outcomesIssue)
	if body := e.gh.withMarker("maul:plan")[0].Body; !strings.Contains(body, splitLine) {
		t.Fatalf("the criteria plan was not posted as slice 1's:\n%s", body)
	}

	e.evaluating(true)
	plan = outcomesPlan
	e.gh.comment(approver, "/maul replan")
	e.settleActions(name)
	e.drive(name, v1alpha1.IntentAwaitingApproval, repoImage)
	plans := e.gh.withMarker("maul:plan")
	if len(plans) != 2 || !strings.Contains(plans[1].Body, "### Acceptance outcomes") {
		t.Fatalf("plan comments = %d, the new one without outcomes", len(plans))
	}
	asked := 0
	for _, spec := range e.specsOf("plan") {
		v, err := templates.DecodePlanVerification(spec.PlanVerification)
		if err != nil || v == nil {
			t.Fatalf("plan Job %s handed %q (%v)", spec.Finding, spec.PlanVerification, err)
		}
		if v.Outcomes {
			asked++
		}
	}
	if asked == 0 {
		t.Fatal("the replan's Job was not asked for outcomes")
	}

	e.clock.Advance(time.Minute)
	e.gh.label(1, "maul:approved", approver)
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	e.review(name, 2)
	in := e.get(name)
	if acc := in.Status.Acceptance; acc == nil || len(acc.Outcomes) != 3 || len(acc.Criteria) != 0 {
		t.Fatalf("acceptance = %+v, want the approved outcomes", acc)
	}
	if n := len(e.gh.withMarker("verify-replan")); n != 0 {
		t.Errorf("replan notices = %d on an outcomes plan", n)
	}
}
