// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/jobs"
	"github.com/bitwise-media-group/patchy/internal/usagemeter/usagemetertest"
	"github.com/bitwise-media-group/patchy/internal/usagewire"
)

// codexHarness is the codex harness as intent-controller runs it under
// --intent-codex, with a build model other than the plan's so a test can
// tell the stages apart.
var codexHarness = RunHarness{ID: "codex", PlanModel: "openai/gpt-5.3-codex", BuildModel: "openai/gpt-5.6-sol"}

// codexProject is the test Project asking for codex.
func codexProject() *v1alpha1.Project {
	p := testProject()
	p.Spec.Harness = v1alpha1.ProjectHarnessCodex
	return p
}

// withCodex turns --intent-codex on (or, with the zero harness, off) on all
// three reconcilers, as the binary sets one Settings for them.
func (e *env) withCodex(h RunHarness) {
	e.project.Settings.Codex = h
	e.intent.Settings.Codex = h
	e.runs.Settings.Codex = h
}

// TestCodexProjectPlansAndBuildsOnCodex: a Project with spec.harness codex,
// under --intent-codex, plans and builds on the codex harness with the codex
// stage models, every run recording the harness it was created with, while
// the controller's claude harness and models are untouched.
func TestCodexProjectPlansAndBuildsOnCodex(t *testing.T) {
	e := newEnv(t, codexProject())
	e.withCodex(codexHarness)
	name := e.newIntent(approver)
	e.drive(name, v1alpha1.IntentAwaitingApproval, repoImage)
	plan := e.onlyLaunch(t, "plan")
	if plan.Harness != "codex" || plan.Model != codexHarness.PlanModel || plan.RunnerImage != "" {
		t.Errorf("plan launch: harness %q, model %q, image %q; want codex, %s, the default image",
			plan.Harness, plan.Model, plan.RunnerImage, codexHarness.PlanModel)
	}

	e.clock.Advance(time.Minute)
	e.gh.label(1, "patchy:approved", approver)
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	build := e.onlyLaunch(t, "build")
	if build.Harness != "codex" || build.Model != codexHarness.BuildModel || build.RunnerImage != repoImage {
		t.Errorf("build launch: harness %q, model %q, image %q; want codex, %s, the repository's image",
			build.Harness, build.Model, build.RunnerImage, codexHarness.BuildModel)
	}
	for _, run := range e.intentRuns(name) {
		if run.Spec.Harness != v1alpha1.ProjectHarnessCodex {
			t.Errorf("run %s records harness %q, want codex", run.Name, run.Spec.Harness)
		}
	}
}

// TestClaudeProjectBesideCodex: with --intent-codex on, a Project that does
// not ask for codex still runs on the controller's claude harness and
// models, and its runs record no harness, exactly as before.
func TestClaudeProjectBesideCodex(t *testing.T) {
	e := newEnv(t, testProject())
	e.withCodex(codexHarness)
	name := e.newIntent(approver)
	e.drive(name, v1alpha1.IntentAwaitingApproval, repoImage)
	plan := e.onlyLaunch(t, "plan")
	if plan.Harness != "claude" || plan.Model != "anthropic/claude-sonnet-5" {
		t.Errorf("plan launch: harness %q, model %q; want claude on the claude plan model", plan.Harness, plan.Model)
	}
	for _, run := range e.intentRuns(name) {
		if run.Spec.Harness != "" {
			t.Errorf("run %s records harness %q, want none", run.Name, run.Spec.Harness)
		}
	}
}

// TestCodexProjectNotReadyWhileOff: without --intent-codex a Project asking
// for codex is not Ready (HarnessNotEnabled), so none of its intents is
// discovered; turning the flag on makes it Ready.
func TestCodexProjectNotReadyWhileOff(t *testing.T) {
	e := newEnv(t, codexProject())
	e.gh.openIssue(1, "Add a version endpoint", "Please add GET /version.", approver)
	e.reconcileProject()
	c := meta.FindStatusCondition(e.getProject().Status.Conditions, v1alpha1.ConditionReady)
	if c == nil || c.Status != "False" || c.Reason != ReasonHarnessNotEnabled || !strings.Contains(c.Message,
		"--intent-codex") {
		t.Fatalf("Ready = %+v, want False with %s naming --intent-codex", c, ReasonHarnessNotEnabled)
	}
	var list v1alpha1.IntentList
	if err := e.c.List(context.Background(), &list); err != nil || len(list.Items) != 0 {
		t.Fatalf("intents = %d (%v), want none discovered for a project that is not Ready", len(list.Items), err)
	}

	e.withCodex(codexHarness)
	e.clock.Advance(time.Hour)
	e.reconcileProject()
	if c := meta.FindStatusCondition(e.getProject().Status.Conditions, v1alpha1.ConditionReady); c == nil ||
		c.Status != "True" {
		t.Fatalf("Ready = %+v once --intent-codex is on, want True", c)
	}
}

// TestCodexRunWaitsWhileOff: a codex run whose controller restarts without
// --intent-codex is not launched, on no harness, and spends no attempt: it
// waits Pending until the flag is on again, then launches on codex.
func TestCodexRunWaitsWhileOff(t *testing.T) {
	e := newEnv(t, codexProject())
	name := e.newIntent(approver)
	// The plan run is created under --intent-codex, then the flag goes off
	// before it launches.
	e.withCodex(codexHarness)
	for range 3 {
		e.mustIntent(name)
		e.readyRepositories(repoImage)
		if len(e.intentRuns(name)) > 0 {
			break
		}
	}
	runs := e.intentRuns(name)
	if len(runs) != 1 || runs[0].Spec.Harness != v1alpha1.ProjectHarnessCodex {
		t.Fatalf("runs = %+v, want one codex plan run", runs)
	}
	e.runs.Settings.Codex = RunHarness{}
	for range 3 {
		e.runRuns()
		e.clock.Advance(time.Minute)
	}
	if n := len(e.jobs.launched()); n != 0 {
		t.Fatalf("%d Jobs launched for a codex run while --intent-codex is off", n)
	}
	run := e.intentRuns(name)[0]
	if run.Status.Phase != "" && run.Status.Phase != v1alpha1.RunPending {
		t.Fatalf("run phase = %s, want Pending", run.Status.Phase)
	}

	e.runs.Settings.Codex = codexHarness
	e.drive(name, v1alpha1.IntentAwaitingApproval, repoImage)
	if plan := e.onlyLaunch(t, "plan"); plan.Harness != "codex" || run.Spec.Attempt != 1 {
		t.Errorf("plan launch on %q at attempt %d, want codex at attempt 1", plan.Harness, run.Spec.Attempt)
	}
}

// TestCodexIntentHoldsOnOpenAICredential: under enforce, a codex Project's
// run waits for its person's OpenAI credential, never an Anthropic one, even
// though the controller's own harness is claude.
func TestCodexIntentHoldsOnOpenAICredential(t *testing.T) {
	e := newEnv(t, codexProject())
	e.withCodex(codexHarness)
	creds := e.enforcing()
	name := e.discovered()
	in := e.drive(name, v1alpha1.IntentBlocked, repoImage)
	if c := credentialBlock(in); c == nil || !strings.Contains(c.Message, "no OpenAI credential") {
		t.Fatalf("CredentialRequired = %+v, want OpenAI named", c)
	}
	creds.mu.Lock()
	asked := append([]v1alpha1.ModelCredentialProvider(nil), creds.asked...)
	creds.mu.Unlock()
	if len(asked) == 0 {
		t.Fatal("no credential record was read")
	}
	for _, p := range asked {
		if p != v1alpha1.ModelCredentialProviderOpenAI {
			t.Errorf("read a %s record for a codex intent", p)
		}
	}
	creds.set(approver, v1alpha1.ModelCredentialValid)
	e.drive(name, v1alpha1.IntentAwaitingApproval, repoImage)
	if plan := e.onlyLaunch(t, "plan"); plan.Harness != "codex" {
		t.Errorf("plan launched on %q, want codex", plan.Harness)
	}
}

// TestMeasureCodexRuns: a codex run's usage is settled from the broker as
// measured, judged by the harness the run records: codex runs brokered.
func TestMeasureCodexRuns(t *testing.T) {
	e := newEnv(t, codexProject())
	e.withCodex(codexHarness)
	start := e.clock.Now()
	e.jobs.statusFn = func(jobs.Spec) (jobs.Status, bool) {
		return jobs.Status{Done: true, Succeeded: 1, Created: start}, true
	}
	broker, meter := usagemetertest.New(t, e.clock.Now)
	broker.SetDefault(func(job string) usagewire.Response {
		return usagemetertest.Answer(job, start.Add(-time.Hour), start.Add(time.Minute), 0,
			usagemetertest.Sonnet(250))
	})
	e.runs.Meter = meter
	name := e.newIntent(approver)
	e.drive(name, v1alpha1.IntentAwaitingApproval, repoImage)
	e.runRuns()
	runs := e.intentRuns(name)
	if len(runs) == 0 {
		t.Fatal("no runs")
	}
	for _, run := range runs {
		if u := run.Status.Usage; !u.Final || u.Source != v1alpha1.UsageSourceMeasured {
			t.Errorf("codex run %s usage = %+v, want measured", run.Name, u)
		}
	}
	if len(broker.Asked()) == 0 {
		t.Error("the broker was never asked for a codex run's usage")
	}
}
