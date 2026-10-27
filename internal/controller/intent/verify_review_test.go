// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/envelope"
	"github.com/devthenet-labs/maul/internal/jobs"
)

// TestVerifyFixAfterNoDefectAndRearm (review of #286, 1): a fix round
// concludes no defect; the approver disagrees and comments /maul verify
// fix; the verification fails the same way again. That is a real fix
// round, not a RepeatedFailure halt: a round at or before the re-arm is no
// prior fix to compare with.
func TestVerifyFixAfterNoDefectAndRearm(t *testing.T) {
	e := newVerifyEnv(t, acceptancePlan)
	e.onAcceptanceFix(func(spec jobs.Spec) jobs.RunOutput { return reportOnly(spec, noDefectReport) })
	name, _ := e.failingVerification()
	e.review(name, 1)
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	if a := e.verification(name).AutoFix; a.Reason != v1alpha1.AutoFixFixFoundNoDefect {
		t.Fatalf("auto-fix = %+v, want halted for no defect", a)
	}
	first := e.verification(name).Ref.Name
	e.gh.comment(approver, "/maul verify fix")
	e.settleActions(name)
	if p := e.prVerifyPass(name); p.noDefectView() == nil {
		t.Error("the note went with the re-arm, at the same heads, before any new round")
	}
	e.review(name, 4)
	again := e.verification(name)
	if again.Ref == nil || again.Ref.Name == first {
		t.Fatalf("no second verification: %+v", again)
	}
	if p := e.prVerifyPass(name); p.noDefectView() == nil {
		t.Error("the note went before a new round was leased")
	}
	e.observe(again.Ref.Name, v1alpha1.VerificationObserved, answering(500))
	e.review(name, 3)
	if n, v := len(e.verifyRuns(name)), e.verification(name); n != 2 || v.AutoFix.State != v1alpha1.AutoFixArmed {
		t.Fatalf("verify runs %d, verification %+v; want a second fix round after the re-arm", n, v)
	}
	if p := e.prVerifyPass(name); p.noDefectView() != nil {
		t.Error("the note outlived the new fix round")
	}
}

// TestNoDefectAfterAStop (review of #286, 3): an approver stops automatic
// fixing while a fix runs; the fix then concludes no defect. The latch
// keeps the approver's HumanStop, the VerifyFailing condition does not
// claim another reason, and the agent's conclusion is still shown, as the
// pull request notice promises.
func TestNoDefectAfterAStop(t *testing.T) {
	e := newVerifyEnv(t, acceptancePlan)
	e.onAcceptanceFix(func(spec jobs.Spec) jobs.RunOutput { return reportOnly(spec, noDefectReport) })
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
	e.gh.comment(approver, "/maul verify stop")
	e.settleActions(name)
	hold = false
	in := e.drive(name, v1alpha1.IntentInReview, repoImage)
	if r := e.verifyRuns(name)[0]; r.Status.Outcome != OutcomeNoDefect {
		t.Fatalf("fix = %s %s, want no_defect", r.Status.Phase, r.Status.Outcome)
	}
	v := in.Status.Verification
	if v.AutoFix.Reason != v1alpha1.AutoFixHumanStop || v.NoDefect == nil || v.NoDefect.Run != fix.Name ||
		!strings.Contains(v.NoDefect.Reason, "network policy") {
		t.Fatalf("verification = %+v (no defect %+v), want the stop kept and the conclusion recorded", v, v.NoDefect)
	}
	if c := meta.FindStatusCondition(in.Status.Conditions, v1alpha1.ConditionVerifyFailing); c != nil &&
		c.Reason == string(v1alpha1.AutoFixFixFoundNoDefect) {
		t.Errorf("VerifyFailing = %+v, inconsistent with the HumanStop latch", c)
	}
	e.review(name, 2)
	if body := e.everyBody(); !strings.Contains(body, "found no defect") ||
		!strings.Contains(body, "network policy was not ready") {
		t.Errorf("the no-defect conclusion is not shown:\n%s", body)
	}
}

// TestVerifyFixRoundWhoseAgentNeverRan (review of #286, 4): a fix round
// whose every attempt ended before any agent ran (the repository image
// could not run the agent) is no fix: the same failure afterwards starts a
// real round instead of halting as RepeatedFailure, and the empty round is
// not held against maxVerifyFixes.
func TestVerifyFixRoundWhoseAgentNeverRan(t *testing.T) {
	e := newVerifyEnv(t, acceptancePlan)
	e.onAcceptanceFix(func(spec jobs.Spec) jobs.RunOutput {
		if spec.Attempt <= 2 {
			out := reportOnly(spec, buildReport)
			out.Events[0].Remediation.Outcome = envelope.OutcomeImageIncompatible
			return out
		}
		return defaultOutput(spec)
	})
	name, _ := e.failingVerification()
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	before := len(e.verifyRuns(name))
	if got := e.prVerifyPass(name).verifyFixRounds(); got != 0 {
		t.Errorf("verifyFixRounds = %d, want a round no agent ran in uncounted", got)
	}
	e.onAcceptanceFix(defaultOutput)
	v := e.reverifyFailing(name, answering(500))
	if n := len(e.verifyRuns(name)); n <= before || v.AutoFix.State != v1alpha1.AutoFixArmed {
		t.Fatalf("verify runs %d (were %d), verification %+v; want a new round", n, before, v)
	}
}

// TestStoppedRetryStillCounts (review of #286, 5): with maxVerifyFixes 1,
// a fix round's first attempt runs and fails, its retry is cancelled by
// /maul verify stop. The round's agent ran, so it is spent: after a
// re-arm, a later, different failure gets no second round.
func TestStoppedRetryStillCounts(t *testing.T) {
	e := newVerifyEnv(t, twoCheckPlan, func(p *v1alpha1.Project) {
		one := int32(1)
		p.Spec.Limits.MaxVerifyFixes = &one
	})
	e.failEveryFix()
	name := e.reviewing()
	e.readyPreview(name)
	e.review(name, 3)
	e.observe(e.onlyVerification(name).Name, v1alpha1.VerificationObserved, byPath(500, 200))
	e.review(name, 1)
	for range 12 {
		runs := e.verifyRuns(name)
		if len(runs) == 2 {
			break
		}
		e.mustIntent(name)
		e.readyRepositories(repoImage)
		e.runRuns()
		e.clock.Advance(time.Minute)
	}
	runs := e.verifyRuns(name)
	if len(runs) != 2 || runs[1].Status.JobRef != nil {
		t.Fatalf("verify runs = %+v, want a1 failed and a2 not launched; verification %+v", runs, e.verification(name))
	}
	e.gh.comment(approver, "/maul verify stop")
	e.settleActions(name)
	e.readyRepositories(repoImage)
	e.runRuns()
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	if got := e.prVerifyPass(name).verifyFixRounds(); got != 1 {
		t.Fatalf("verifyFixRounds = %d, want the round whose a1 ran counted", got)
	}
	first := e.verification(name).Ref.Name
	e.gh.comment(approver, "/maul verify fix")
	e.settleActions(name)
	e.review(name, 4)
	again := e.verification(name)
	if again.Ref == nil || again.Ref.Name == first {
		t.Fatalf("no second verification: %+v", again)
	}
	e.observe(again.Ref.Name, v1alpha1.VerificationObserved, byPath(200, 500))
	e.review(name, 3)
	if n, v := len(e.verifyRuns(name)), e.verification(name); n != 2 ||
		v.AutoFix.Reason != v1alpha1.AutoFixMaxVerifyFixes {
		t.Fatalf("verify runs %d, verification %+v; want no second round, halted MaxVerifyFixes", n, v)
	}
}

// TestCrashingFixRoundIsBounded (re-review of #286, A): a fix round whose
// agent ran and crashed every attempt (runtime_error after a full
// session) is a tried fix: it counts toward maxVerifyFixes, so with a limit
// of one the next failure starts no round and automatic fixes halt.
func TestCrashingFixRoundIsBounded(t *testing.T) {
	e := newVerifyEnv(t, twoCheckPlan, func(p *v1alpha1.Project) {
		one := int32(1)
		p.Spec.Limits.MaxVerifyFixes = &one
	})
	e.onAcceptanceFix(func(spec jobs.Spec) jobs.RunOutput {
		out := reportOnly(spec, buildReport)
		out.Events[0].Remediation.Outcome = envelope.OutcomeRuntimeError
		out.Events[0].Remediation.Detail = "claude exited 1 after 40 turns"
		return out
	})
	name := e.reviewing()
	e.readyPreview(name)
	e.review(name, 3)
	e.observe(e.onlyVerification(name).Name, v1alpha1.VerificationObserved, byPath(500, 200))
	e.review(name, 1)
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	runs := e.verifyRuns(name)
	if len(runs) < 2 {
		t.Fatalf("verify runs = %d, want both attempts", len(runs))
	}
	for _, r := range runs {
		if r.Status.Outcome != string(envelope.OutcomeRuntimeError) {
			t.Fatalf("run %s = %s, want runtime_error", r.Name, r.Status.Outcome)
		}
	}
	if got := e.prVerifyPass(name).verifyFixRounds(); got != 1 {
		t.Fatalf("verifyFixRounds = %d, want the crashing round counted", got)
	}
	v := e.reverifyFailing(name, byPath(200, 500))
	if n := len(e.verifyRuns(name)); n != len(runs) || v.AutoFix.Reason != v1alpha1.AutoFixMaxVerifyFixes {
		t.Fatalf("verify runs %d (were %d), verification %+v; want halted MaxVerifyFixes", n, len(runs), v)
	}
}

// TestNoDefectNoteFollowsTheHeads (re-review of #286, B): the no-defect
// conclusion is about the code it judged. Once a human's round pushes new
// heads, the next verification's comment no longer shows it.
func TestNoDefectNoteFollowsTheHeads(t *testing.T) {
	e := newVerifyEnv(t, acceptancePlan)
	e.onAcceptanceFix(func(spec jobs.Spec) jobs.RunOutput { return reportOnly(spec, noDefectReport) })
	name, _ := e.failingVerification()
	e.review(name, 1)
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	if p := e.prVerifyPass(name); p.noDefectView() == nil {
		t.Fatal("no note at the heads it was concluded at")
	}
	e.reviewOn(e.prNumber(name), 1901, "Rename the handler.")
	e.clock.Advance(3 * time.Minute)
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	e.readyPreview(name)
	e.review(name, 4)
	v := e.verification(name)
	if v.Ref == nil || v.Round != 2 {
		t.Fatalf("no verification at the new heads: %+v", v)
	}
	e.observe(v.Ref.Name, v1alpha1.VerificationObserved, answering(500))
	e.review(name, 3)
	if p := e.prVerifyPass(name); p.noDefectView() != nil {
		t.Errorf("the note outlived the heads it judged: %+v", e.verification(name).NoDefect)
	}
	if cs := e.gh.withMarker("verify-r2"); len(cs) != 1 || strings.Contains(cs[0].Body, "found no defect") {
		t.Errorf("verification 2 comment = %+v, want one without the note", cs)
	}
}
