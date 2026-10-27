// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"strings"
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/envelope"
	"github.com/devthenet-labs/maul/internal/jobs"
)

// twoCheckPlan is acceptancePlan with a second automated criterion.
var twoCheckPlan = strings.Replace(acceptancePlan, "  - id: sounds-good\n",
	"  - id: version\n"+
		"    kind: http\n"+
		"    statement: the app serves its version\n"+
		"    target: app\n"+
		"    request: {method: GET, path: /version}\n"+
		"    expect: {status: 200}\n"+
		"  - id: sounds-good\n", 1)

// byPath answers /healthz with health and every other path with other.
func byPath(health, other int32) acceptance.Doer {
	return func(c acceptance.Call) acceptance.Response {
		if strings.HasPrefix(c.Request.Path, "/healthz") {
			return acceptance.Response{Status: health}
		}
		return acceptance.Response{Status: other}
	}
}

// failEveryFix makes every attempt of every acceptance-fix round fail, as
// beatbattle-44's rev1 did: commit_failed, nothing pushed.
func (e *env) failEveryFix() {
	e.onAcceptanceFix(func(spec jobs.Spec) jobs.RunOutput {
		out := reportOnly(spec, buildReport)
		out.Events[0].Remediation.Outcome = envelope.OutcomeCommitFailed
		return out
	})
}

// reverifyFailing has an approver ask for a verification of name at the
// same heads and the tester observe do there; it returns the new
// verification.
func (e *env) reverifyFailing(name string, do acceptance.Doer) *v1alpha1.IntentVerification {
	e.t.Helper()
	first := e.verification(name).Ref.Name
	e.gh.comment(approver, "/maul verify")
	e.settleActions(name)
	e.review(name, 4)
	again := e.verification(name)
	if again.Ref == nil || again.Ref.Name == first {
		e.t.Fatalf("no second verification: %+v", again)
	}
	e.observe(again.Ref.Name, v1alpha1.VerificationObserved, do)
	e.review(name, 3)
	return e.verification(name)
}

// TestVerifyFixRepeatedFailureAfterAnUnpushedRound (beatbattle-44, rough
// edge 9): an acceptance-fix round whose every attempt failed pushes
// nothing; an approver's /maul verify at the same heads fails exactly as
// before. That is a repeated failure: automatic fixes halt and no second
// round starts, whether or not the failed round completed.
func TestVerifyFixRepeatedFailureAfterAnUnpushedRound(t *testing.T) {
	e := newVerifyEnv(t, acceptancePlan)
	e.failEveryFix()
	name, _ := e.failingVerification()
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	runs := e.verifyRuns(name)
	if len(runs) < 2 {
		t.Fatalf("verify runs = %d, want the round's attempts", len(runs))
	}
	for _, r := range runs {
		if r.Status.Phase != v1alpha1.RunFailed {
			t.Fatalf("run %s = %s, want every attempt failed", r.Name, r.Status.Phase)
		}
	}
	v := e.reverifyFailing(name, answering(500))
	if n := len(e.verifyRuns(name)); n != len(runs) || v.AutoFix.State != v1alpha1.AutoFixHalted ||
		v.AutoFix.Reason != v1alpha1.AutoFixRepeatedFailure {
		t.Fatalf("verify runs %d (were %d), verification %+v; want halted RepeatedFailure, no new round", n,
			len(runs), v)
	}
}

// TestVerifyFixRepeatedFailureBesideANewOne: the criterion the last fix
// round was for fails again exactly as it did, beside one that newly fails
// (on a preview never reset, data left by an earlier verification can do
// that). The fix changed nothing of what it was for: a repeated failure.
func TestVerifyFixRepeatedFailureBesideANewOne(t *testing.T) {
	e := newVerifyEnv(t, twoCheckPlan)
	e.failEveryFix()
	name := e.reviewing()
	e.readyPreview(name)
	e.review(name, 3)
	pv := e.onlyVerification(name)
	e.observe(pv.Name, v1alpha1.VerificationObserved, byPath(500, 200))
	e.review(name, 1)
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	before := len(e.verifyRuns(name))
	v := e.reverifyFailing(name, byPath(500, 500))
	if n := len(e.verifyRuns(name)); n != before || v.AutoFix.Reason != v1alpha1.AutoFixRepeatedFailure {
		t.Fatalf("verify runs %d (were %d), verification %+v; want halted RepeatedFailure", n, before, v)
	}
}

// TestVerifyFixADifferentFailureIsFixed: when what the last fix round was
// for now passes and something else fails, that is no repeat: another
// round starts.
func TestVerifyFixADifferentFailureIsFixed(t *testing.T) {
	e := newVerifyEnv(t, twoCheckPlan)
	e.failEveryFix()
	name := e.reviewing()
	e.readyPreview(name)
	e.review(name, 3)
	pv := e.onlyVerification(name)
	e.observe(pv.Name, v1alpha1.VerificationObserved, byPath(500, 200))
	e.review(name, 1)
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	before := len(e.verifyRuns(name))
	e.reverifyFailing(name, byPath(200, 500))
	e.review(name, 1)
	if n := len(e.verifyRuns(name)); n <= before {
		t.Fatalf("verify runs %d (were %d), verification %+v; want a new round for the new failure", n, before,
			e.verification(name))
	}
}
