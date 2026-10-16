// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package usagerollup

import (
	"context"
	"time"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/stats"
)

// settled is a terminal run phase.
func settled(p v1alpha1.RunPhase) bool {
	return p == v1alpha1.RunComplete || p == v1alpha1.RunFailed
}

// completedAt is the time a run is bucketed by: when it finished, else (a
// run deleted before it finished, or one that never reported a stage) when
// it was created, which never moves between retries.
func completedAt(finished *metav1.Time, created metav1.Time) time.Time {
	if finished != nil && !finished.IsZero() {
		return finished.Time
	}
	return created.Time
}

// stageRun describes an Investigation or Remediation: its usage is its
// stage's, absent when the run never reported one.
func stageRun(kind string, uid types.UID, attr *v1alpha1.Attribution, phase v1alpha1.RunPhase,
	stage *v1alpha1.StageResult, created metav1.Time) Run {
	run := Run{Kind: kind, UID: uid, Attribution: attr, Settled: settled(phase)}
	var finished *metav1.Time
	if stage != nil {
		run.Usage, finished = &stage.Usage, stage.FinishedAt
	}
	run.CompletedAt = completedAt(finished, created)
	return run
}

// Investigations adapts Investigation runs: the org's (investigation
// controller stamps no person), the stage's usage.
var Investigations = Kind[*v1alpha1.Investigation]{
	New: func() *v1alpha1.Investigation { return &v1alpha1.Investigation{} },
	Run: func(_ context.Context, inv *v1alpha1.Investigation) (Run, error) {
		return stageRun(stats.UsageKindInvestigation, inv.UID, inv.Spec.AttributedTo, inv.Status.Phase,
			inv.Status.Stage, inv.CreationTimestamp), nil
	},
	Conditions: func(inv *v1alpha1.Investigation) *[]metav1.Condition { return &inv.Status.Conditions },
}

// Remediations adapts Remediation runs.
var Remediations = Kind[*v1alpha1.Remediation]{
	New: func() *v1alpha1.Remediation { return &v1alpha1.Remediation{} },
	Run: func(_ context.Context, rem *v1alpha1.Remediation) (Run, error) {
		return stageRun(stats.UsageKindRemediation, rem.UID, rem.Spec.AttributedTo, rem.Status.Phase,
			rem.Status.Stage, rem.CreationTimestamp), nil
	},
	Conditions: func(rem *v1alpha1.Remediation) *[]metav1.Condition { return &rem.Status.Conditions },
}

// EvaluationUnits adapts EvaluationUnit runs, by their submitter.
var EvaluationUnits = Kind[*v1alpha1.EvaluationUnit]{
	New: func() *v1alpha1.EvaluationUnit { return &v1alpha1.EvaluationUnit{} },
	Run: func(_ context.Context, unit *v1alpha1.EvaluationUnit) (Run, error) {
		return Run{Kind: stats.UsageKindEvaluation, UID: unit.UID, Attribution: unit.Spec.AttributedTo,
			Settled: settled(unit.Status.Phase), Usage: &unit.Status.Usage,
			CompletedAt: completedAt(unit.Status.FinishedAt, unit.CreationTimestamp)}, nil
	},
	Conditions: func(unit *v1alpha1.EvaluationUnit) *[]metav1.Condition { return &unit.Status.Conditions },
}

// IntentRunKind is the usage kind of an intent run: its stage, a checks
// round of revise being a check-fix.
func IntentRunKind(spec v1alpha1.IntentRunSpec) string {
	switch spec.Stage {
	case v1alpha1.IntentStagePlan:
		return stats.UsageKindPlan
	case v1alpha1.IntentStageBuild:
		return stats.UsageKindBuild
	}
	if spec.Trigger == v1alpha1.IntentRunTriggerChecks {
		return stats.UsageKindCheckFix
	}
	return stats.UsageKindRevise
}

// IntentRuns adapts IntentRuns. The Project is the owning Intent's, read
// through reader and held to the run's intentRef UID; when the Intent is
// gone (deleted in the background, ahead of its runs) the run still counts
// for its user, with no project.
func IntentRuns(reader client.Reader) Kind[*v1alpha1.IntentRun] {
	return Kind[*v1alpha1.IntentRun]{
		New: func() *v1alpha1.IntentRun { return &v1alpha1.IntentRun{} },
		Run: func(ctx context.Context, ir *v1alpha1.IntentRun) (Run, error) {
			run := Run{Kind: IntentRunKind(ir.Spec), UID: ir.UID, Attribution: ir.Spec.AttributedTo,
				Settled: settled(ir.Status.Phase), Usage: &ir.Status.Usage, Intent: ir.Spec.IntentRef.Name,
				CompletedAt: completedAt(ir.Status.FinishedAt, ir.CreationTimestamp)}
			var in v1alpha1.Intent
			err := reader.Get(ctx, types.NamespacedName{Namespace: ir.Namespace, Name: ir.Spec.IntentRef.Name}, &in)
			switch {
			case kerrors.IsNotFound(err):
			case err != nil:
				return run, err
			case ir.Spec.IntentRef.UID == "" || in.UID == ir.Spec.IntentRef.UID:
				run.Project = in.Spec.Project
			}
			return run, nil
		},
		Conditions: func(ir *v1alpha1.IntentRun) *[]metav1.Condition { return &ir.Status.Conditions },
	}
}
