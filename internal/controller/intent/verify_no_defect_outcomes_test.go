// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/envelope"
	"github.com/devthenet-labs/maul/internal/jobs"
)

// The outcome twins of verify_no_defect_test.go (S4): an acceptance-fix
// round of an outcome round that concludes, or fails to conclude, no
// defect. TestHumanRoundNoDefectIsNotHonoured needs no twin: its intent
// verifies nothing, so it states neither criteria nor outcomes.

// TestOutcomeVerifyFixNoDefect twins TestVerifyFixNoDefect.
func TestOutcomeVerifyFixNoDefect(t *testing.T) {
	e, name, _ := failingEvaluation(t)
	e.onAcceptanceFix(func(spec jobs.Spec) jobs.RunOutput { return reportOnly(spec, noDefectReport) })
	e.review(name, 1)
	in := e.drive(name, v1alpha1.IntentInReview, repoImage)
	runs := e.verifyRuns(name)
	if len(runs) != 1 {
		t.Fatalf("verify runs = %d, want 1: a round that found no defect is not retried", len(runs))
	}
	run := runs[0]
	if run.Status.Phase != v1alpha1.RunComplete || run.Status.Outcome != OutcomeNoDefect ||
		run.Status.PushedCommit != "" || !strings.Contains(run.Status.Detail, "network policy") {
		t.Fatalf("verify run = %s %s %q pushed %q", run.Status.Phase, run.Status.Outcome, run.Status.Detail,
			run.Status.PushedCommit)
	}
	fixJob := false
	for _, spec := range e.jobs.launched() {
		fixJob = fixJob || spec.AcceptanceFix
	}
	if !fixJob {
		t.Error("no Job was told it is an acceptance-fix round")
	}
	v := in.Status.Verification
	if v.AutoFix.State != v1alpha1.AutoFixHalted || v.AutoFix.Reason != v1alpha1.AutoFixFixFoundNoDefect ||
		v.State != v1alpha1.VerificationFailing {
		t.Fatalf("verification = %+v, want failing with automatic fixes halted for no defect", v)
	}
	if in.Status.VerifyFixes != 1 || in.Status.Revisions != 0 {
		t.Errorf("counters verify %d revisions %d, want 1 and 0", in.Status.VerifyFixes, in.Status.Revisions)
	}
	if got := e.prVerifyPass(name).verifyFixRounds(); got != 1 {
		t.Errorf("verifyFixRounds = %d, want the round counted", got)
	}
	if c := meta.FindStatusCondition(in.Status.Conditions, v1alpha1.ConditionVerifyFailing); c == nil ||
		c.Status != metav1.ConditionTrue || c.Reason != string(v1alpha1.AutoFixFixFoundNoDefect) {
		t.Errorf("VerifyFailing = %+v, want True FixFoundNoDefect", c)
	}
	// One pass more than the criteria original: the issue's comment shows
	// the conclusion on the third pass after the fix settles.
	e.review(name, 1)
	e.checkNoDefectReported(name)
}

// TestOutcomeVerifyFixWithoutAChangeStillFails twins
// TestVerifyFixWithoutAChangeStillFails.
func TestOutcomeVerifyFixWithoutAChangeStillFails(t *testing.T) {
	for _, tc := range []struct {
		name    string
		out     func(jobs.Spec) jobs.RunOutput
		outcome string
	}{
		{"commit_failed", func(spec jobs.Spec) jobs.RunOutput {
			out := reportOnly(spec, buildReport)
			out.Events[0].Remediation.Outcome = envelope.OutcomeCommitFailed
			out.Events[0].Remediation.Detail = "commit.sh missing despite success report"
			return out
		}, string(envelope.OutcomeCommitFailed)},
		{"not built", func(spec jobs.Spec) jobs.RunOutput { return reportOnly(spec, nothingBuiltReport) },
			OutcomeNotBuilt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, name, _ := failingEvaluation(t)
			e.onAcceptanceFix(tc.out)
			e.review(name, 1)
			for range 12 {
				e.mustIntent(name)
				e.readyRepositories(repoImage)
				e.runRuns()
			}
			runs := e.verifyRuns(name)
			if len(runs) < 2 {
				t.Fatalf("verify runs = %d, want the failed attempt retried", len(runs))
			}
			for _, r := range runs {
				if r.Status.Phase == v1alpha1.RunComplete {
					t.Errorf("run %s completed (%s): a round that changed nothing without saying so failed",
						r.Name, r.Status.Outcome)
				}
			}
			if r := runs[0]; r.Status.Phase != v1alpha1.RunFailed || r.Status.Outcome != tc.outcome {
				t.Errorf("first attempt = %s %s, want Failed %s", r.Status.Phase, r.Status.Outcome, tc.outcome)
			}
			if v := e.verification(name); v.AutoFix.Reason == v1alpha1.AutoFixFixFoundNoDefect {
				t.Errorf("verification = %+v, halted for no defect", v)
			}
		})
	}
}
