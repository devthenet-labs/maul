// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// The outcome twins of verify_repeated_test.go (S4 PR 1): RepeatedFailure
// on the evaluator's fingerprint, around an acceptance-fix round that
// pushed nothing.

// bothFailingReport is an evaluator's report citing both outcomes failing.
const bothFailingReport = `---
verdicts:
  - id: version-shown
    verdict: fail
    ledger: [2]
  - id: still-healthy
    verdict: fail
    ledger: [3]
---
Neither endpoint answers as it should.
`

// healthFailingReport cites the health outcome alone failing.
const healthFailingReport = `---
verdicts:
  - id: version-shown
    verdict: pass
    ledger: [2]
  - id: still-healthy
    verdict: fail
    ledger: [3]
---
The health endpoint no longer answers.
`

// reevaluateFailing has an approver ask for another round of name at the
// same heads, the evaluator reporting report and the saved checks replaying
// as runs; it returns the verification once judged.
func (e *env) reevaluateFailing(name, report string, runs ...v1alpha1.CheckRun) *v1alpha1.IntentVerification {
	e.t.Helper()
	first := e.verification(name).Ref.Name
	e.evaluateReporting(report)
	e.gh.comment(approver, "/maul verify")
	e.settleActions(name)
	e.review(name, 4)
	if again := e.verification(name); again.Ref == nil || again.Ref.Name == first {
		e.t.Fatalf("no second round: %+v", again)
	}
	e.evalRound(name, runs...)
	e.review(name, 3)
	return e.verification(name)
}

// unpushedFix drives a failing evaluation's fix round, every attempt of
// which fails, back to review; it returns how many attempts ran.
func (e *env) unpushedFix(name string) int {
	e.t.Helper()
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	runs := e.verifyRuns(name)
	if len(runs) < 2 {
		e.t.Fatalf("verify runs = %d, want the round's attempts", len(runs))
	}
	for _, r := range runs {
		if r.Status.Phase != v1alpha1.RunFailed {
			e.t.Fatalf("run %s = %s, want every attempt failed", r.Name, r.Status.Phase)
		}
	}
	return len(runs)
}

// TestOutcomeFixRepeatedFailureAfterAnUnpushedRound twins
// TestVerifyFixRepeatedFailureAfterAnUnpushedRound.
func TestOutcomeFixRepeatedFailureAfterAnUnpushedRound(t *testing.T) {
	e, name, _, _ := outcomeEnv(t)
	e.failEveryFix()
	version, health := savedCheck(t, versionCheck), savedCheck(t, healthCheck)
	e.evalRound(name, replays(version, health, v1alpha1.CheckRunFail, v1alpha1.CheckRunPass)...)
	before := e.unpushedFix(name)
	v := e.reevaluateFailing(name, failingReport, replays(version, health, v1alpha1.CheckRunFail,
		v1alpha1.CheckRunPass)...)
	if n := len(e.verifyRuns(name)); n != before || v.AutoFix.State != v1alpha1.AutoFixHalted ||
		v.AutoFix.Reason != v1alpha1.AutoFixRepeatedFailure {
		t.Fatalf("verify runs %d (were %d), verification %+v; want halted RepeatedFailure, no new round", n,
			before, v)
	}
}

// TestOutcomeFixRepeatedFailureBesideANewOne twins
// TestVerifyFixRepeatedFailureBesideANewOne: the outcome the last fix was
// for fails again on the same check, beside one that newly fails.
func TestOutcomeFixRepeatedFailureBesideANewOne(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.evaluateReporting(failingReport)
	// The health check writes its run's own record: its new failure is a
	// reproduced one beside the repeat, not a suspicion.
	version, health := savedCheck(t, versionCheck), savedCheck(t, healthWritesCheck)
	e.storeChecks(name, version, health)
	e.review(name, 3)
	e.failEveryFix()
	e.evalRound(name, replays(version, health, v1alpha1.CheckRunFail, v1alpha1.CheckRunPass)...)
	before := e.unpushedFix(name)
	v := e.reevaluateFailing(name, bothFailingReport, replays(version, health, v1alpha1.CheckRunFail,
		v1alpha1.CheckRunFail)...)
	if n := len(e.verifyRuns(name)); n != before || v.Failed != 2 ||
		v.AutoFix.Reason != v1alpha1.AutoFixRepeatedFailure {
		t.Fatalf("verify runs %d (were %d), verification %+v; want both failing, halted RepeatedFailure", n,
			before, v)
	}
}

// healthWritesCheck is a check of the keep outcome that writes its run's
// own record, so its failure once it passed is a regression maul may fix
// (#309 review), not a suspicion held for a person.
const healthWritesCheck = `{"id":"still-healthy.w","outcome":"still-healthy","form":"http","http":{"target":"app",
 "steps":[{"request":{"method":"POST","path":"/notes","json":{"name":"{{run}}-h"}},"expect":{"status":201}},
 {"request":{"path":"/healthz?q={{run}}"},"expect":{"json":{"/note":"{{run}}-h"}}}]}}`

// TestOutcomeFixADifferentFailureIsFixed twins
// TestVerifyFixADifferentFailureIsFixed: what the last fix was for passes
// now and another outcome fails: no repeat, another round starts.
func TestOutcomeFixADifferentFailureIsFixed(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.evaluateReporting(failingReport)
	version, health := savedCheck(t, versionCheck), savedCheck(t, healthWritesCheck)
	e.storeChecks(name, version, health)
	e.review(name, 3)
	e.failEveryFix()
	e.evalRound(name, replays(version, health, v1alpha1.CheckRunFail, v1alpha1.CheckRunPass)...)
	before := e.unpushedFix(name)
	v := e.reevaluateFailing(name, healthFailingReport, replays(version, health, v1alpha1.CheckRunPass,
		v1alpha1.CheckRunFail)...)
	if (v.State != v1alpha1.VerificationFailing && v.State != v1alpha1.VerificationFixing) ||
		v.AutoFix.Reason == v1alpha1.AutoFixRepeatedFailure {
		t.Fatalf("verification %+v: want the new failure reproduced, no repeat", v)
	}
	e.review(name, 1)
	if n := len(e.verifyRuns(name)); n <= before {
		t.Fatalf("verify runs %d (were %d), verification %+v; want a new round for the new failure", n, before,
			e.verification(name))
	}
}
