// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"fmt"
	"slices"
	"testing"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/jobs"
)

// buildSubagents sets --intent-build-subagents apart on the reconciler that
// creates runs (the intent reconciler, which records the grant) and the one
// that launches them (the run reconciler), so a test can turn the flag off
// between a run's creation and its launch, as an operator rolling back does.
func (e *env) buildSubagents(record, launch bool) {
	e.project.Settings.BuildSubagents = record
	e.intent.Settings.BuildSubagents = record
	e.runs.Settings.BuildSubagents = launch
}

// TestGrantRecordsSubagents: --intent-build-subagents records subagents on
// the grant of a build and a revise run (every check-fix and verify-fix
// round is a revise run) of a claude Project, and on no other: never a plan,
// evaluate or checkreview run, never a codex Project's, and nothing at all
// with the flag off.
func TestGrantRecordsSubagents(t *testing.T) {
	for _, harness := range []v1alpha1.ProjectHarness{"", v1alpha1.ProjectHarnessClaude, v1alpha1.ProjectHarnessCodex} {
		p := testProject()
		p.Spec.Harness = harness
		for _, stage := range v1alpha1.IntentStages() {
			for _, on := range []bool{false, true} {
				s := testSettings()
				s.BuildSubagents = on
				want := on && !harness.IsCodex() &&
					(stage == v1alpha1.IntentStageBuild || stage == v1alpha1.IntentStageRevise)
				if got := s.grant(p, stage).Subagents; got != want {
					t.Errorf("harness %q, stage %s, flag %v: grant.subagents = %v, want %v", harness, stage, on, got, want)
				}
			}
		}
	}
}

// approvedBuild drives a fresh intent of e's Project through its plan and
// approval to its pull request, and returns the intent's name.
func (e *env) approvedBuild() string {
	e.t.Helper()
	name := e.awaiting()
	e.gh.label(1, "maul:approved", approver)
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	return name
}

// jobOf is the spec the run's Job was created with.
func (e *env) jobOf(run v1alpha1.IntentRun) jobs.Spec {
	e.t.Helper()
	if run.Status.JobRef == nil {
		e.t.Fatalf("run %s launched no Job", run.Name)
	}
	spec, ok := e.jobs.specs[run.Status.JobRef.Name]
	if !ok {
		e.t.Fatalf("run %s's Job %s was not created", run.Name, run.Status.JobRef.Name)
	}
	return spec
}

// TestBuildSubagentsLaunchAsGranted: a build and a revise run recorded with
// subagents launch with them while the flag is on; a run recorded with them
// launches without them once the flag is off (owner decision 6: turning the
// flag off narrows every run, at once); and a run recorded without them is
// never launched with them, whatever the flag says at launch. A plan run
// never records or launches with them.
func TestBuildSubagentsLaunchAsGranted(t *testing.T) {
	for _, tt := range []struct {
		name           string
		record, launch bool
		wantRecorded   bool
		wantLaunched   bool
	}{
		{"flag on throughout", true, true, true, true},
		{"flag turned off before the launch", true, false, true, false},
		{"flag turned on before the launch", false, true, false, false},
		{"flag off throughout", false, false, false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, testProject())
			e.buildSubagents(tt.record, tt.launch)
			name := e.approvedBuild()
			in := e.get(name)
			if len(in.Status.PullRequests) != 1 {
				t.Fatal("build opened no pull request")
			}
			e.requestChanges(in.Status.PullRequests[0].Number, 876, "Please add a regression test.")
			e.drive(name, v1alpha1.IntentRevising, repoImage)
			e.drive(name, v1alpha1.IntentInReview, repoImage)

			for _, plan := range e.runsOf(name, v1alpha1.IntentStagePlan) {
				if plan.Spec.Grant.Subagents || e.jobOf(plan).BuildSubagents {
					t.Errorf("plan run %s recorded or launched with subagents", plan.Name)
				}
			}
			for _, stage := range []v1alpha1.IntentStage{v1alpha1.IntentStageBuild, v1alpha1.IntentStageRevise} {
				runs := e.runsOf(name, stage)
				if len(runs) != 1 {
					t.Fatalf("%s runs = %d, want one", stage, len(runs))
				}
				if got := runs[0].Spec.Grant.Subagents; got != tt.wantRecorded {
					t.Errorf("%s run grant.subagents = %v, want %v", stage, got, tt.wantRecorded)
				}
				if got := e.jobOf(runs[0]).BuildSubagents; got != tt.wantLaunched {
					t.Errorf("%s Job BuildSubagents = %v, want %v", stage, got, tt.wantLaunched)
				}
			}
		})
	}
}

// TestCodexRunsNeverRecordSubagents: a codex Project's build is never
// recorded with subagents, flag on or not: codex renders no subagents
// posture (and the schema refuses the record).
func TestCodexRunsNeverRecordSubagents(t *testing.T) {
	p := testProject()
	p.Spec.Harness = v1alpha1.ProjectHarnessCodex
	s := testSettings()
	s.BuildSubagents = true
	for _, stage := range []v1alpha1.IntentStage{v1alpha1.IntentStageBuild, v1alpha1.IntentStageRevise} {
		if s.grant(p, stage).Subagents {
			t.Errorf("codex %s run recorded subagents", stage)
		}
	}
}

// TestRefusedSubagentsSettleTheLaunch: a Job create the jobs package
// refuses for its subagents grant settles the run launch_refused, a counted
// attempt, instead of holding its slot retrying a launch that cannot
// succeed.
func TestRefusedSubagentsSettleTheLaunch(t *testing.T) {
	e := newEnv(t, testProject())
	e.buildSubagents(true, true)
	name := e.awaiting()
	e.jobs.mu.Lock()
	e.jobs.createErr = fmt.Errorf("%w: only an intent build on claude may be granted subagents",
		jobs.ErrSubagentsRefused)
	e.jobs.mu.Unlock()
	e.gh.label(1, "maul:approved", approver)
	e.drive(name, v1alpha1.IntentFailed, repoImage)
	builds := e.runsOf(name, v1alpha1.IntentStageBuild)
	if len(builds) == 0 || slices.ContainsFunc(builds, func(r v1alpha1.IntentRun) bool {
		return r.Status.Outcome != OutcomeLaunchRefused
	}) {
		t.Errorf("build runs = %+v, want every attempt launch_refused", builds)
	}
}

// TestReviseLeaseAdoptedWhateverItsSubagents: a revise round's lease created
// by a pass under the other setting of --intent-build-subagents (the flag
// was turned off, or on, and the controller restarted between the create
// and the pass that finds it) is the same round, so it is adopted with its
// own record, never refused as another's; and it launches no wider than the
// flag now allows.
func TestReviseLeaseAdoptedWhateverItsSubagents(t *testing.T) {
	for _, tc := range []struct {
		name     string
		flag     bool // the flag of the pass that finds the lease, and of the launch
		recorded bool // the existing lease's record
	}{
		{"leased with the flag on, found with it off", false, true},
		{"leased with the flag off, found with it on", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, testProject())
			e.buildSubagents(tc.flag, tc.flag)
			name := e.approvedBuild()
			in := e.get(name)

			wc, ok := e.c.(client.WithWatch)
			if !ok {
				t.Fatal("the env's client does not watch")
			}
			leased := false
			e.intent.Client = interceptor.NewClient(wc, interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object,
					opts ...client.CreateOption) error {
					run, ok := obj.(*v1alpha1.IntentRun)
					if !ok || run.Spec.Stage != v1alpha1.IntentStageRevise || leased {
						return c.Create(ctx, obj, opts...)
					}
					leased = true
					other := run.DeepCopy()
					other.Spec.Grant.Subagents = tc.recorded
					if err := c.Create(ctx, other, opts...); err != nil {
						return err
					}
					return kerrors.NewAlreadyExists(schema.GroupResource{Resource: "intentruns"}, run.Name)
				},
			})

			e.requestChanges(in.Status.PullRequests[0].Number, 901, "Please add a test.")
			e.drive(name, v1alpha1.IntentRevising, repoImage)
			e.drive(name, v1alpha1.IntentInReview, repoImage)
			runs := e.runsOf(name, v1alpha1.IntentStageRevise)
			if !leased || len(runs) != 1 || runs[0].Status.Phase != v1alpha1.RunComplete {
				t.Fatalf("leased %v, revise runs %+v; want the one existing lease adopted and run", leased, runs)
			}
			if got := runs[0].Spec.Grant.Subagents; got != tc.recorded {
				t.Errorf("the adopted round records subagents %v, want its own record %v", got, tc.recorded)
			}
			if e.jobOf(runs[0]).BuildSubagents {
				t.Error("the adopted round launched with subagents, wider than its record or the flag")
			}
		})
	}
}

// TestBuildSubagentsOnlyOnBrokeredClaude: a run recorded with subagents is
// launched with them only on brokered claude, the one harness that renders
// the posture: never on the fake harness, and never at a stage that does
// not run the build phase, whatever its record says.
func TestBuildSubagentsOnlyOnBrokeredClaude(t *testing.T) {
	s := testSettings()
	s.BuildSubagents = true
	for _, tt := range []struct {
		harness string
		stage   v1alpha1.IntentStage
		want    bool
	}{
		{"claude", v1alpha1.IntentStageBuild, true},
		{"claude", v1alpha1.IntentStageRevise, true},
		{"fake", v1alpha1.IntentStageBuild, false},
		{"claude", v1alpha1.IntentStagePlan, false},
		{"claude", v1alpha1.IntentStageEvaluate, false},
	} {
		r := &RunReconciler{Settings: s, Harness: tt.harness}
		run := &v1alpha1.IntentRun{Spec: v1alpha1.IntentRunSpec{Stage: tt.stage,
			Grant: v1alpha1.IntentRunGrant{Subagents: true}}}
		if got := r.buildSubagents(run); got != tt.want {
			t.Errorf("%s %s run recorded with subagents: launched with them %v, want %v", tt.harness, tt.stage,
				got, tt.want)
		}
	}
}
