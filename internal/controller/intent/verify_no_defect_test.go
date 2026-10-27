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
	"github.com/devthenet-labs/maul/internal/envelope"
	"github.com/devthenet-labs/maul/internal/jobs"
)

// noDefectReason is the agent's one-line account of why the verification
// failed, as an acceptance-fix round that found no defect writes it.
const noDefectReason = "each criterion passes when replayed by hand; the preview's network policy was not " +
	"ready when the tester ran"

// noDefectReport is an acceptance-fix round's report that found no defect.
const noDefectReport = "---\nsuccess: false\nno_defect: true\nsummary: \"No defect found\"\ntests:\n" +
	"  ran: true\n  passed: true\n  command: \"go test ./...\"\nnotes: []\nreason: \"" + noDefectReason +
	"\"\n---\n\nReplayed each criterion against the code.\n"

// nothingBuiltReport is a build that changed nothing and does not say why the
// code needs no change: a failure.
const nothingBuiltReport = "---\nsuccess: false\nsummary: \"Nothing to change\"\ntests:\n" +
	"  ran: false\n  passed: false\nnotes: []\nreason: \"the code looks right already\"\n---\n\nNothing.\n"

// reportOnly is a build stage that ran well and wrote report, with no
// changeset.
func reportOnly(spec jobs.Spec, report string) jobs.RunOutput {
	stage := envelope.Stage{Outcome: envelope.OutcomeOK, Harness: "claude", Model: spec.Model,
		Usage: envelope.Usage{InputTokens: 10, OutputTokens: 20, CostUSD: 0.25}}
	return jobs.RunOutput{Events: []envelope.Event{{V: envelope.Version, Type: envelope.TypeRemediation,
		Remediation: &envelope.Remediation{Stage: stage, ReportMarkdown: report}}}}
}

// onAcceptanceFix answers an acceptance-fix round's Job of e with out, and
// every other Job as e did.
func (e *env) onAcceptanceFix(out func(jobs.Spec) jobs.RunOutput) {
	before := e.jobs.output
	e.jobs.output = func(spec jobs.Spec) jobs.RunOutput {
		if spec.AcceptanceFix {
			return out(spec)
		}
		return before(spec)
	}
}

// TestVerifyFixNoDefect: an acceptance-fix round whose agent concludes,
// explicitly, that the code has no defect completes as no_defect: no
// retry, no push, automatic fixes halted (FixFoundNoDefect), the round
// counted against maxVerifyFixes, and the issue's verification comment
// shows the agent's reason, says it may be wrong and how to verify again.
// The round's Job was told it is an acceptance fix.
func TestVerifyFixNoDefect(t *testing.T) {
	e := newVerifyEnv(t, acceptancePlan)
	e.onAcceptanceFix(func(spec jobs.Spec) jobs.RunOutput { return reportOnly(spec, noDefectReport) })
	name, _ := e.failingVerification()
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
	p := e.prVerifyPass(name)
	if got := p.verifyFixRounds(); got != 1 {
		t.Errorf("verifyFixRounds = %d, want the round counted", got)
	}
	if c := meta.FindStatusCondition(in.Status.Conditions, v1alpha1.ConditionVerifyFailing); c == nil ||
		c.Status != metav1.ConditionTrue || c.Reason != string(v1alpha1.AutoFixFixFoundNoDefect) {
		t.Errorf("VerifyFailing = %+v, want True FixFoundNoDefect", c)
	}
	e.checkNoDefectReported(name)
}

// checkNoDefectReported: the comments show the agent's reason, that it may
// be wrong and how to verify again, never a push, and more passes start no
// round.
func (e *env) checkNoDefectReported(name string) {
	e.t.Helper()
	t := e.t
	e.review(name, 2)
	bodies := e.everyBody()
	for _, want := range []string{"found no defect", "network policy was not ready", "may be wrong",
		"/maul verify"} {
		if !strings.Contains(bodies, want) {
			t.Errorf("comments omit %q:\n%s", want, bodies)
		}
	}
	if strings.Contains(bodies, "pushed commit ``") {
		t.Errorf("a round that pushed nothing was announced as a push:\n%s", bodies)
	}
	e.review(name, 3)
	if n := len(e.verifyRuns(name)); n != 1 {
		t.Errorf("verify runs = %d after more passes, want still 1", n)
	}
}

// TestVerifyFixWithoutAChangeStillFails: an acceptance-fix round that
// changed nothing without saying no_defect fails and is retried, as
// before: the live round that wrote no commit.sh (commit_failed), and one
// that reported the plan not built.
func TestVerifyFixWithoutAChangeStillFails(t *testing.T) {
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
			e := newVerifyEnv(t, acceptancePlan)
			e.onAcceptanceFix(tc.out)
			name, _ := e.failingVerification()
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

// TestHumanRoundNoDefectIsNotHonoured: an approver's round is never an
// acceptance fix: its Job is not told it may conclude no defect, and a
// report claiming it anyway fails as not built and is retried, exactly as
// a round that built nothing did before.
func TestHumanRoundNoDefectIsNotHonoured(t *testing.T) {
	e := newEnv(t, testProject())
	name := e.awaiting()
	e.gh.label(1, "maul:approved", approver)
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	e.jobs.output = func(spec jobs.Spec) jobs.RunOutput { return reportOnly(spec, noDefectReport) }
	e.reviewOn(e.prNumber(name), 901, "Rename the handler.")
	e.clock.Advance(3 * time.Minute)
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	for range 12 {
		e.mustIntent(name)
		e.readyRepositories(repoImage)
		e.runRuns()
	}
	runs := e.runsOf(name, v1alpha1.IntentStageRevise)
	if len(runs) < 2 {
		t.Fatalf("revise runs = %d, want the failed attempt retried", len(runs))
	}
	for _, r := range runs {
		if r.Status.Phase == v1alpha1.RunComplete || r.Status.Outcome == OutcomeNoDefect {
			t.Errorf("run %s = %s %s, want failed as not built", r.Name, r.Status.Phase, r.Status.Outcome)
		}
	}
	if r := runs[0]; r.Status.Outcome != OutcomeNotBuilt {
		t.Errorf("first attempt = %s, want %s", r.Status.Outcome, OutcomeNotBuilt)
	}
	for _, spec := range e.jobs.launched() {
		if spec.AcceptanceFix {
			t.Errorf("Job %s was told it is an acceptance fix", spec.Finding)
		}
	}
}
