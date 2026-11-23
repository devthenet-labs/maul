// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"testing"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// buildWorkflows sets --intent-build-workflows apart on the reconcilers that
// create runs and the one that launches them, as buildSubagents does.
func (e *env) buildWorkflows(record, launch bool) {
	e.project.Settings.BuildWorkflows = record
	e.intent.Settings.BuildWorkflows = record
	e.runs.Settings.BuildWorkflows = launch
}

// TestGrantRecordsWorkflows: --intent-build-workflows records workflows on
// the grant of exactly the runs --intent-build-subagents records subagents
// on, and only beside them: never without the subagents flag, never on a
// plan, evaluate or checkreview run, never on a codex Project's.
func TestGrantRecordsWorkflows(t *testing.T) {
	for _, harness := range []v1alpha1.ProjectHarness{"", v1alpha1.ProjectHarnessClaude, v1alpha1.ProjectHarnessCodex} {
		p := testProject()
		p.Spec.Harness = harness
		for _, stage := range v1alpha1.IntentStages() {
			for _, flags := range [][2]bool{{false, false}, {false, true}, {true, false}, {true, true}} {
				s := testSettings()
				s.BuildSubagents, s.BuildWorkflows = flags[0], flags[1]
				g := s.grant(p, stage)
				if want := g.Subagents && flags[1]; g.Workflows != want {
					t.Errorf("harness %q, stage %s, flags %v: grant.workflows = %v, want %v", harness, stage, flags,
						g.Workflows, want)
				}
				if g.Workflows && !g.Subagents {
					t.Errorf("harness %q, stage %s: workflows recorded without subagents", harness, stage)
				}
			}
		}
	}
}

// TestBuildWorkflowsLaunchAsGranted: a build and a revise run recorded with
// workflows launch with them while both flags are on; turning either flag
// off narrows every run so recorded at once, the workflows flag to
// subagents alone and the subagents flag to neither; and a run recorded
// without them is never launched with them. A plan run never is.
func TestBuildWorkflowsLaunchAsGranted(t *testing.T) {
	for _, tt := range []struct {
		name                       string
		record                     bool // both flags at creation
		launchSubagents, launchWfs bool // the flags at launch
		wantSubagents, wantWfs     bool
	}{
		{"both flags on throughout", true, true, true, true, true},
		{"workflows turned off before the launch", true, true, false, true, false},
		{"subagents turned off before the launch", true, false, true, false, false},
		{"recorded without, both on at the launch", false, true, true, false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, testProject())
			e.buildSubagents(tt.record, tt.launchSubagents)
			e.buildWorkflows(tt.record, tt.launchWfs)
			name := e.approvedBuild()
			in := e.get(name)
			if len(in.Status.PullRequests) != 1 {
				t.Fatal("build opened no pull request")
			}
			e.requestChanges(in.Status.PullRequests[0].Number, 877, "Please add a regression test.")
			e.drive(name, v1alpha1.IntentRevising, repoImage)
			e.drive(name, v1alpha1.IntentInReview, repoImage)

			for _, plan := range e.runsOf(name, v1alpha1.IntentStagePlan) {
				if plan.Spec.Grant.Workflows || e.jobOf(plan).BuildWorkflows {
					t.Errorf("plan run %s recorded or launched with workflows", plan.Name)
				}
			}
			for _, stage := range []v1alpha1.IntentStage{v1alpha1.IntentStageBuild, v1alpha1.IntentStageRevise} {
				runs := e.runsOf(name, stage)
				if len(runs) != 1 {
					t.Fatalf("%s runs = %d, want one", stage, len(runs))
				}
				if got := runs[0].Spec.Grant.Workflows; got != tt.record {
					t.Errorf("%s run grant.workflows = %v, want %v", stage, got, tt.record)
				}
				job := e.jobOf(runs[0])
				if job.BuildSubagents != tt.wantSubagents || job.BuildWorkflows != tt.wantWfs {
					t.Errorf("%s Job subagents %v, workflows %v; want %v, %v", stage, job.BuildSubagents,
						job.BuildWorkflows, tt.wantSubagents, tt.wantWfs)
				}
			}
		})
	}
}

// TestReviseLeaseAdoptedWhateverItsWorkflows: a revise round's lease created
// under the other setting of --intent-build-workflows is the same round, so
// it is adopted with its own record, and launches no wider than the flags
// now allow.
func TestReviseLeaseAdoptedWhateverItsWorkflows(t *testing.T) {
	for _, tc := range []struct {
		name     string
		flag     bool // the workflows flag of the pass that finds the lease, and of the launch
		recorded bool // the existing lease's record
	}{
		{"leased with the flag on, found with it off", false, true},
		{"leased with the flag off, found with it on", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, testProject())
			e.buildSubagents(true, true)
			e.buildWorkflows(tc.flag, tc.flag)
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
					other.Spec.Grant.Workflows = tc.recorded
					if err := c.Create(ctx, other, opts...); err != nil {
						return err
					}
					return kerrors.NewAlreadyExists(schema.GroupResource{Resource: "intentruns"}, run.Name)
				},
			})

			e.requestChanges(in.Status.PullRequests[0].Number, 902, "Please add a test.")
			e.drive(name, v1alpha1.IntentRevising, repoImage)
			e.drive(name, v1alpha1.IntentInReview, repoImage)
			runs := e.runsOf(name, v1alpha1.IntentStageRevise)
			if !leased || len(runs) != 1 || runs[0].Status.Phase != v1alpha1.RunComplete {
				t.Fatalf("leased %v, revise runs %+v; want the one existing lease adopted and run", leased, runs)
			}
			if got := runs[0].Spec.Grant.Workflows; got != tc.recorded {
				t.Errorf("the adopted round records workflows %v, want its own record %v", got, tc.recorded)
			}
			if e.jobOf(runs[0]).BuildWorkflows {
				t.Error("the adopted round launched with workflows, wider than its record or the flag")
			}
		})
	}
}
