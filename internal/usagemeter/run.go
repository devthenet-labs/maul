// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package usagemeter

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/jobs"
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

// PendingDeleted reports whether a run being deleted still waits for its
// usage to be settled before it is folded into the rollups: the meter is
// on, the run launched a Job (one that never did has nothing metered) and
// its usage is not final (u nil when it never reported a stage). A run
// cancelled mid-flight never settles, so without this its broker-metered
// spend would be counted only as the pod reported it. The wait is Settle's,
// bounded by Wait like a settled run's.
func (m *Meter) PendingDeleted(ref *v1alpha1.JobReference, u *v1alpha1.UsageSummary) bool {
	return m.On() && ref != nil && ref.Name != "" && (u == nil || !u.Final)
}
