// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package usagemeter

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/jobs"
)

// JobStatus is the slice of a controller's jobs client RunOf reads a Job's
// creation time through.
type JobStatus interface {
	Status(ctx context.Context, jobName string) (jobs.Status, error)
}

// RunOf describes a settled run for Settle: its Job from the run's status
// (none when it never launched), the Job's creation time from the cluster,
// or the run's own creation when the Job is gone (earlier, so the boot-time
// check only gets stricter), and the harness its controller launched it on.
func RunOf(ctx context.Context, js JobStatus, uid types.UID, ref *v1alpha1.JobReference, runCreated time.Time,
	harness string) Run {
	run := Run{UID: uid, Created: runCreated, Harness: harness}
	if ref == nil || ref.Name == "" {
		return run
	}
	run.Job = ref.Name
	if st, err := js.Status(ctx, ref.Name); err == nil && !st.Created.IsZero() {
		run.Created = st.Created
	}
	return run
}

// Pending reports whether a run's usage still waits to be settled: the
// meter is on, the run is terminal and its usage is not final.
func (m *Meter) Pending(phase v1alpha1.RunPhase, u *v1alpha1.UsageSummary) bool {
	return m.On() && (phase == v1alpha1.RunComplete || phase == v1alpha1.RunFailed) && u != nil && !u.Final
}
