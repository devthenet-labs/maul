// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"strings"
	"testing"
	"time"

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
