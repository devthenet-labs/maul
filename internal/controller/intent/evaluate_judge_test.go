// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/envelope"
	"github.com/devthenet-labs/maul/internal/jobs"
	"github.com/devthenet-labs/maul/internal/templates"
)

// outcomesPlan's outcomes, as Decide answers for them.
const (
	oVersion = "version-shown"
	oHealthy = "still-healthy"
	oManual  = "looks-right"
)

// judged is Decide's result for one outcome.
func judged(id string, r acceptance.Result, reason, basis string) acceptance.OutcomeResult {
	return acceptance.OutcomeResult{ID: id, Result: r, Reason: reason, Basis: basis}
}

// judgeRoute is one route of TestJudgementRoutes: a decision, what the
// humans did, and what each view must say.
type judgeRoute struct {
	name                        string
	version                     acceptance.OutcomeResult
	healthy                     acceptance.OutcomeResult
	fix, replay, anomaly        bool
	env                         acceptance.EnvClass
	noDefect, acceptManual      bool
	acceptVersion               bool
	state                       v1alpha1.IntentVerificationState
	progress, status, held, why string
	disputed                    int
	versionResult, manualResult v1alpha1.OutcomeResultValue
}

// TestJudgementRoutes: every route a round's decision can take, to the
// state the Intent records, the issue's progress clause, the maul/acceptance
// status and the hand-off: ready only with every outcome confirmed or
// accepted (a manual one by an approver); a fix request Failing with the
// link held; a fix at heads a fix round found no defect in Exhausted
// (D3); a dispute's replay round held and Unconfirmed; an anomaly, a
// failing environment, a quarantined or a model-only outcome released as
// "couldn't confirm" with maul's reason, its maul/acceptance a final error
// (S1-29); an approver's acceptance of the failing outcome closes it.
func TestJudgementRoutes(t *testing.T) {
	check := savedCheck(t, versionCheck)
	pass := func(id string) acceptance.OutcomeResult {
		return judged(id, acceptance.ResultPassed, acceptance.ReasonConfirmed, check.Digest)
	}
	fail := judged(oVersion, acceptance.ResultFailed, acceptance.ReasonReproduced, "")
	for _, tc := range []judgeRoute{
		{name: "ready", version: pass(oVersion), healthy: pass(oHealthy), acceptManual: true,
			state: v1alpha1.VerificationPassed, progress: templates.VerifyPassed, status: statusSuccess,
			held: templates.HandoffReady, versionResult: v1alpha1.OutcomePassed, manualResult: v1alpha1.OutcomeAccepted},
		{name: "manual open", version: pass(oVersion), healthy: pass(oHealthy),
			state: v1alpha1.VerificationUnconfirmed, progress: templates.VerifyUnconfirmed, status: statusError,
			held: templates.HandoffUnconfirmed, why: templates.HandoffWhyUnsure, versionResult: v1alpha1.OutcomePassed,
			manualResult: v1alpha1.OutcomeUnsure},
		{name: "fix", version: fail, healthy: pass(oHealthy), fix: true, acceptManual: true,
			state: v1alpha1.VerificationFailing, progress: templates.VerifyFailing, status: statusFailure,
			held: templates.HandoffHeld, versionResult: v1alpha1.OutcomeFailed, manualResult: v1alpha1.OutcomeAccepted},
		{name: "fix after no defect", version: fail, healthy: pass(oHealthy), fix: true, noDefect: true,
			state: v1alpha1.VerificationExhausted, progress: templates.VerifyExhausted, status: statusFailure,
			held: templates.HandoffUnconfirmed, why: templates.HandoffWhyRounds, versionResult: v1alpha1.OutcomeFailed,
			manualResult: v1alpha1.OutcomeUnsure},
		{name: "fix accepted", version: fail, healthy: pass(oHealthy), fix: true, acceptManual: true,
			acceptVersion: true, state: v1alpha1.VerificationPassed, progress: templates.VerifyPassed,
			status: statusSuccess, held: templates.HandoffReady, versionResult: v1alpha1.OutcomeAccepted,
			manualResult: v1alpha1.OutcomeAccepted},
		{name: "dispute replays", version: judged(oVersion, acceptance.ResultUnsure, acceptance.ReasonDisputed, ""),
			healthy: pass(oHealthy), replay: true, acceptManual: true, state: v1alpha1.VerificationUnconfirmed,
			progress: templates.VerifyUnconfirmed, status: statusPending, held: templates.HandoffHeld, disputed: 1,
			versionResult: v1alpha1.OutcomeUnsure, manualResult: v1alpha1.OutcomeAccepted},
		{name: "anomaly", version: pass(oVersion), healthy: pass(oHealthy), anomaly: true, acceptManual: true,
			state: v1alpha1.VerificationUnconfirmed, progress: templates.VerifyUnconfirmed, status: statusError,
			held: templates.HandoffUnconfirmed, why: templates.HandoffWhyAnomaly, versionResult: v1alpha1.OutcomePassed,
			manualResult: v1alpha1.OutcomeAccepted},
		{name: "environment failed",
			version: judged(oVersion, acceptance.ResultUnsure, acceptance.ReasonEnvFailed, ""),
			healthy: pass(oHealthy), env: acceptance.EnvAppFailure, acceptManual: true,
			state: v1alpha1.VerificationUnconfirmed, progress: templates.VerifyUnconfirmed, status: statusError,
			held: templates.HandoffUnconfirmed, why: templates.HandoffWhyEnvironment,
			versionResult: v1alpha1.OutcomeUnsure, manualResult: v1alpha1.OutcomeAccepted},
		{name: "quarantined",
			version: judged(oVersion, acceptance.ResultQuarantined, acceptance.ReasonQuarantined, ""),
			healthy: pass(oHealthy), acceptManual: true, state: v1alpha1.VerificationUnconfirmed,
			progress: templates.VerifyUnconfirmed, status: statusError, held: templates.HandoffUnconfirmed,
			why: templates.HandoffWhyUnsure, versionResult: v1alpha1.OutcomeQuarantined,
			manualResult: v1alpha1.OutcomeAccepted},
		{name: "model only",
			version: judged(oVersion, acceptance.ResultUnsure, acceptance.ReasonModelOnlyPass, ""),
			healthy: pass(oHealthy), acceptManual: true, state: v1alpha1.VerificationUnconfirmed,
			progress: templates.VerifyUnconfirmed, status: statusError, held: templates.HandoffUnconfirmed,
			why: templates.HandoffWhyUnsure, versionResult: v1alpha1.OutcomeUnsure,
			manualResult: v1alpha1.OutcomeAccepted},
	} {
		t.Run(tc.name, func(t *testing.T) { checkJudgeRoute(t, tc, check) })
	}
}

// checkJudgeRoute drives one route: an evaluation in flight, the decision
// written as a judgement is, then every view of it.
func checkJudgeRoute(t *testing.T, tc judgeRoute, check v1alpha1.SavedCheck) {
	t.Helper()
	e, name := newEvaluatorEnv(t, 0.4)
	e.review(name, 3)
	if v := e.verification(name); v.State != v1alpha1.VerificationEvaluating || v.Handoff == nil {
		t.Fatalf("verification = %+v, want an evaluation in flight with its hand-off recorded", v)
	}
	j := routeJudgement(tc, check)
	p := e.prVerifyPass(name)
	e.setStatus(name, func(in *v1alpha1.Intent) {
		routeRecords(in, tc, e.clock.Now())
		applyJudgement(in, j)
		p.releaseIfDue(in)
	})
	v := e.verification(name)
	if v.State != tc.state || len(v.Disputed) != tc.disputed {
		t.Fatalf("state = %s, disputed %v; want %s, %d", v.State, v.Disputed, tc.state, tc.disputed)
	}
	if r := v.Outcomes[0]; r.Result != tc.versionResult || (r.Result == v1alpha1.OutcomePassed &&
		!strings.HasPrefix(r.Line, "http to app: POST /notes, GET /version")) {
		t.Errorf("version outcome = %+v, want %s with maul's check line", r, tc.versionResult)
	}
	if r := v.Outcomes[2]; r.Result != tc.manualResult {
		t.Errorf("manual outcome = %+v, want %s", r, tc.manualResult)
	}
	checkRouteViews(t, e.prVerifyPass(name), tc)
}

// routeJudgement is the judgement tc's decision makes.
func routeJudgement(tc judgeRoute, check v1alpha1.SavedCheck) roundJudgement {
	d := acceptance.Decision{PerOutcome: []acceptance.OutcomeResult{tc.version, tc.healthy,
		judged(oManual, acceptance.ResultUnsure, acceptance.ReasonManual, "")}, Env: acceptance.EnvOK}
	if tc.env != "" {
		d.Env = tc.env
	}
	if tc.fix {
		d.Fix = &acceptance.FixRequest{Failures: []acceptance.FixFailure{{Outcome: oVersion,
			CheckDigest: check.Digest, Seq: 2}}, Fingerprint: "sha256:" + strings.Repeat("f", 64)}
	}
	if tc.replay {
		d.ReplayRound, d.Retire = true, []string{check.Digest}
	}
	j := roundJudgement{decision: d, checks: []v1alpha1.SavedCheck{check}}
	if tc.anomaly {
		j.anomalies = []v1alpha1.AnomalyRecord{{Summary: "a console error on /", Ledger: []int32{4}}}
	}
	return j
}

// routeRecords writes what tc's humans did onto in: their acceptances at
// the heads, and a fix round's no-defect conclusion there.
func routeRecords(in *v1alpha1.Intent, tc judgeRoute, now time.Time) {
	v := in.Status.Verification
	accept := func(id string) {
		in.Status.Acceptance.Accepted = append(in.Status.Acceptance.Accepted, v1alpha1.AcceptanceRecord{
			ID: id, By: approver, ByID: 7, At: metav1.NewTime(now), CommentID: 99,
			HeadSetDigest: v.HeadSetDigest, Heads: []string{"abcdef1"}})
	}
	if tc.acceptManual {
		accept(oManual)
	}
	if tc.acceptVersion {
		accept(oVersion)
	}
	if tc.noDefect {
		v.NoDefect = &v1alpha1.NoDefectRecord{Run: "x-rev-r2-a1", Reason: "fine", At: metav1.NewTime(now),
			HeadSetDigest: v.HeadSetDigest}
	}
}

// checkRouteViews holds p's views of the judgement to tc: the progress
// clause, the maul/acceptance status, the hand-off and the comment.
func checkRouteViews(t *testing.T, p *pass, tc judgeRoute) {
	t.Helper()
	if prog := p.verifyProgress(); prog == nil || prog.State != tc.progress || !prog.Outcomes {
		t.Errorf("progress = %+v, want %s", prog, tc.progress)
	}
	st, _, ok := p.acceptanceStatus()
	if !ok || st.state != tc.status || len(st.description) > 140 {
		t.Errorf("maul/acceptance = %+v (posted %v), want %s within 140 bytes", st, ok, tc.status)
	}
	// S1-29: maul's own words while it is still checking (a replay round
	// due) and once it could not confirm an outcome (final, never pending).
	switch {
	case tc.state == v1alpha1.VerificationUnconfirmed && tc.held == templates.HandoffHeld:
		if !strings.HasPrefix(st.description, "maul is checking this") {
			t.Errorf("maul/acceptance description %q, want maul is checking this", st.description)
		}
	case tc.state == v1alpha1.VerificationUnconfirmed:
		if !strings.HasPrefix(st.description, "maul couldn't confirm this") {
			t.Errorf("maul/acceptance description %q, want maul couldn't confirm this", st.description)
		}
	}
	if h := p.handoffView(); h == nil || h.State != tc.held || h.Why != tc.why {
		t.Errorf("hand-off = %+v, want %s %s", h, tc.held, tc.why)
	}
	v := p.in.Status.Verification
	section := p.verifySection(context.Background(), nil)
	body, err := templates.RenderVerifyComment(templates.VerifyComment{Namespace: testNS, Intent: p.in.Name,
		Round: v.Round, Attempt: v.Attempt, Section: *section})
	if err != nil || len(section.Outcomes) != 3 || !strings.Contains(body, "`"+oVersion+"`") {
		t.Errorf("verification comment = %q, %v", body, err)
	}
}

// failingReport is an evaluator's report that the version outcome fails,
// citing the failing replay.
const failingReport = `---
verdicts:
  - id: version-shown
    verdict: fail
    ledger: [2]
  - id: still-healthy
    verdict: pass
    ledger: [3]
---
The version endpoint answers with the wrong note.
`

// storeChecks gives intent name a check store holding cs, each reviewed
// proves for its outcome's text.
func (e *env) storeChecks(name string, cs ...v1alpha1.SavedCheck) {
	e.t.Helper()
	for range 3 {
		if e.get(name).Status.Acceptance != nil {
			break
		}
		e.mustIntent(name)
	}
	p := e.prVerifyPass(name)
	if err := p.writeCheckStore(context.Background(), cs); err != nil {
		e.t.Fatal(err)
	}
	in := e.get(name)
	e.setStatus(name, func(cur *v1alpha1.Intent) {
		cur.Status.Acceptance.Checks = in.Status.Acceptance.Checks
		for _, c := range cs {
			for _, o := range cur.Status.Acceptance.Outcomes {
				if o.ID == c.Outcome {
					cur.Status.Acceptance.Reviews = append(cur.Status.Acceptance.Reviews, v1alpha1.CheckReviewRecord{
						CheckDigest: c.Digest, OutcomeDigest: outcomeDigest(o), Answer: v1alpha1.ReviewProves,
						Run: name + "-cr-r0-a1"})
				}
			}
		}
	})
}

// evaluateReporting makes e's evaluate Jobs report report.
func (e *env) evaluateReporting(report string) {
	prev := e.jobs.output
	e.jobs.output = func(spec jobs.Spec) jobs.RunOutput {
		if spec.Phase == "evaluate" {
			return evaluationOutput(envelope.Stage{Outcome: envelope.OutcomeOK, Harness: "claude", Model: spec.Model,
				Usage: envelope.Usage{InputTokens: 10, OutputTokens: 20, CostUSD: 0.4}}, report)
		}
		return prev(spec)
	}
}

// replayed plays preview-controller collecting pv with runs as its replays.
func (e *env) replayed(pvName string, runs ...v1alpha1.CheckRun) {
	e.t.Helper()
	e.playTools(pvName, v1alpha1.VerificationObserved, toolPodIP, true)
	pv := e.pvNamed(pvName)
	pv.Status.Replay = runs
	if err := e.c.Update(context.Background(), pv); err != nil {
		e.t.Fatal(err)
	}
}

// TestEvaluationConfirmsAndHandsOver: a full round whose stored checks
// replay clean, reviewed proves, with the evaluator's cited passes, and
// the manual outcome accepted by an approver, is judged ready: Passed, every
// outcome resting on its check with maul's line, the link released ready in
// the judgement's own write, and the status comment shows it labelled.
func TestEvaluationConfirmsAndHandsOver(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	version, health := savedCheck(t, versionCheck), savedCheck(t, healthCheck)
	e.storeChecks(name, version, health)
	e.review(name, 3)
	pv := e.onlyVerification(name)
	if len(pv.Spec.Checks) != 2 || pv.Spec.Rung != v1alpha1.RungFull {
		t.Fatalf("verification checks = %+v, want the stored two on a full round", pv.Spec.Checks)
	}
	if body := e.statusBody(); strings.Contains(body, previewHostOf(name)) {
		t.Errorf("the link is shown while held:\n%s", body)
	}
	e.setStatus(name, func(in *v1alpha1.Intent) {
		v := in.Status.Verification
		in.Status.Acceptance.Accepted = append(in.Status.Acceptance.Accepted, v1alpha1.AcceptanceRecord{ID: oManual,
			By: approver, ByID: 7, At: metav1.NewTime(e.clock.Now()), CommentID: 99, HeadSetDigest: v.HeadSetDigest})
	})
	e.playTools(pv.Name, v1alpha1.VerificationRunning, toolPodIP, true)
	e.review(name, 1)
	e.runRuns()
	e.runRuns()
	e.replayed(pv.Name, v1alpha1.CheckRun{CheckDigest: version.Digest, Result: v1alpha1.CheckRunPass, Token: "t1",
		Seq: 1}, v1alpha1.CheckRun{CheckDigest: health.Digest, Result: v1alpha1.CheckRunPass, Token: "t2", Seq: 2})
	e.settleReviews(name)
	v := e.verification(name)
	if v.State != v1alpha1.VerificationPassed || v.Passed != 2 {
		t.Fatalf("verification = %s %+v, want Passed", v.State, v.Outcomes)
	}
	if v.Handoff == nil || !v.Handoff.Released || v.Handoff.Reason != v1alpha1.HandoffReasonReady {
		t.Errorf("hand-off = %+v, want released ready with the judgement", v.Handoff)
	}
	for _, r := range v.Outcomes[:2] {
		if r.Basis == "" || r.Line == "" {
			t.Errorf("outcome %+v rests on no check", r)
		}
	}
	e.reviewUntil(name, func() bool { return strings.Contains(e.statusBody(), "**Preview:** Ready") })
	if body := e.statusBody(); !strings.Contains(body, "Ready: maul checked the approved outcomes.") ||
		!strings.Contains(body, previewHostOf(name)) {
		t.Errorf("status comment does not hand over the link labelled ready:\n%s", body)
	}
}

// reviewUntil runs review passes, a minute apart, until done holds, at most
// a dozen.
func (e *env) reviewUntil(name string, done func() bool) {
	e.t.Helper()
	for range 12 {
		if done() {
			return
		}
		e.review(name, 1)
	}
	e.t.Fatalf("intent %s: the condition never held; phase %s, verification %+v", name, e.get(name).Status.Phase,
		e.verification(name))
}

// TestEvaluationFailureStartsAFix: a stored check failing its replay, with
// the evaluator's cited fail, is a reproduced failure: the verification is
// Failing and the kept machinery starts one acceptance-fix round on it,
// handed the failing outcome's text and the check by its id, never the
// check's digest or the store; the link stays held while it runs.
func TestEvaluationFailureStartsAFix(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.evaluateReporting(failingReport)
	version, health := savedCheck(t, versionCheck), savedCheck(t, healthCheck)
	e.storeChecks(name, version, health)
	e.review(name, 3)
	pv := e.onlyVerification(name)
	e.playTools(pv.Name, v1alpha1.VerificationRunning, toolPodIP, true)
	e.review(name, 1)
	e.runRuns()
	e.runRuns()
	e.replayed(pv.Name, v1alpha1.CheckRun{CheckDigest: version.Digest, Result: v1alpha1.CheckRunFail, Token: "t1",
		Seq: 2}, v1alpha1.CheckRun{CheckDigest: health.Digest, Result: v1alpha1.CheckRunPass, Token: "t2", Seq: 3})
	e.settleReviews(name)
	if v := e.verification(name); v.State != v1alpha1.VerificationFailing || v.Failed != 1 {
		t.Fatalf("verification = %s %+v, want Failing on the version outcome", v.State, v.Outcomes)
	}
	e.reviewUntil(name, func() bool { return len(e.runsOf(name, v1alpha1.IntentStageRevise)) > 0 })
	run := e.runsOf(name, v1alpha1.IntentStageRevise)[0]
	if run.Spec.Trigger != v1alpha1.IntentRunTriggerVerify || run.Spec.Inputs.VerificationRef == nil ||
		run.Spec.Inputs.VerificationRef.UID != pv.UID {
		t.Fatalf("revise run = %+v, want the acceptance fix of the round", run.Spec)
	}
	if v := e.verification(name); v.State != v1alpha1.VerificationFixing || v.Handoff.Released {
		t.Errorf("verification = %s, hand-off %+v; want Fixing with the link held", v.State, v.Handoff)
	}
	var in *corev1.ConfigMap
	for range 8 {
		var cm corev1.ConfigMap
		if err := e.c.Get(context.Background(), client.ObjectKey{Namespace: testNS, Name: run.Spec.Inputs.ConfigMap},
			&cm); err == nil {
			in = &cm
			break
		}
		e.readyRepositories(repoImage)
		e.runRuns()
		e.review(name, 1)
	}
	if in == nil {
		t.Fatal("the fix round has no input")
	}
	all := ""
	for _, s := range in.Data {
		all += s
	}
	if !strings.Contains(all, "GET /version answers with the commit it runs") || !strings.Contains(all,
		"`version-shown`") {
		t.Errorf("the fix round's input lacks the failing outcome:\n%s", all)
	}
	for _, f := range []string{version.Digest, health.Digest, "-checks-", version.Body, "wrong note"} {
		if strings.Contains(all, f) {
			t.Errorf("the fix round's input carries %q", f)
		}
	}
}

// TestCheckLine: maul's line for each form names what the check asks of
// the preview and how much it asserts, on one line within its bound; an
// unparseable body has none.
func TestCheckLine(t *testing.T) {
	exec := `{"id":"version-shown.cli","outcome":"version-shown","form":"exec","exec":{"argv":["node","t.js"],
	 "files":[{"path":"t.js","content":"console.log(1)"}],"expect":{"exit":0,"stdoutContains":"{{run}}"}}}`
	browser := `{"id":"looks-right.row","outcome":"looks-right","form":"browser","browser":{"steps":[
	 {"goto":"/board"},{"click":{"role":"button","name":"Rate"}},
	 {"expect":{"assert":"visible","locator":{"role":"row","hasText":"{{run}}-alice"}}}]}}`
	for _, tc := range []struct{ raw, want string }{
		{versionCheck, "http to app: POST /notes, GET /version?q={{run}}; 3 expectations"},
		{healthCheck, "http to app: GET /healthz; 2 expectations"},
		{exec, "exec: node; expects exit 0, stdout"},
		{browser, "browser: goto /board, click, expect visible"},
	} {
		if got := checkLine(savedCheck(t, tc.raw)); got != tc.want {
			t.Errorf("checkLine = %q, want %q", got, tc.want)
		}
	}
	if got := checkLine(v1alpha1.SavedCheck{Body: "{"}); got != "" {
		t.Errorf("checkLine of garbage = %q", got)
	}
}
