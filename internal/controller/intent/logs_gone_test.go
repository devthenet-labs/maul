// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/jobs"
)

// evictionMessage is the kubelet's message on the live eviction this
// regression comes from (0.12.37, run pip-fixture-35-bld-r1-pip-fixture-a1).
const evictionMessage = "Pod ephemeral local storage usage exceeds the total limit of containers 8Gi."

// TestEvictedBuildWithLogGoneSettles: a build whose pod was evicted, and
// whose agent log the kubelet removed with it, settles as evicted in one
// pass, naming the eviction's cause on the run, rather than failing the
// collect on every pass until the Job's TTL deletes it.
func TestEvictedBuildWithLogGoneSettles(t *testing.T) {
	e := newEnv(t, testProject())
	e.jobs.statusFn = func(spec jobs.Spec) (jobs.Status, bool) {
		if spec.Phase != "build" {
			return jobs.Status{}, false
		}
		return jobs.Status{Done: true, Failed: 1, AgentStarted: true, AgentTerminated: "ContainerStatusUnknown",
			PodReason: "Evicted", PodMessage: evictionMessage, JobFailure: "BackoffLimitExceeded"}, true
	}
	e.jobs.resultErr = func(spec jobs.Spec) error {
		if spec.Phase != "build" {
			return nil
		}
		return fmt.Errorf("%w (the pod was evicted): jobs: read logs: container \"agent\" is terminated",
			jobs.ErrLogsGone)
	}
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	if errs := e.driveCollectingErrors(name, v1alpha1.IntentFailed); len(errs) != 0 {
		t.Fatalf("collecting an evicted build failed %d times, want none: %v", len(errs), errs)
	}
	runs := e.runsOf(name, v1alpha1.IntentStageBuild)
	if len(runs) == 0 {
		t.Fatal("no build runs")
	}
	for _, run := range runs {
		if run.Status.Outcome != OutcomeEvicted {
			t.Errorf("run %s outcome = %q, want %q", run.Name, run.Status.Outcome, OutcomeEvicted)
		}
		for _, want := range []string{"log", "evicted", "ephemeral local storage"} {
			if !strings.Contains(run.Status.Detail, want) {
				t.Errorf("run %s detail %q does not say %q", run.Name, run.Status.Detail, want)
			}
		}
	}
}

// TestPlanWithLogGoneSettles: a plan whose log is gone for another reason
// (the container removed, not an eviction) settles with the ordinary
// no-result outcome.
func TestPlanWithLogGoneSettles(t *testing.T) {
	e := newEnv(t, testProject())
	e.jobs.statusFn = func(jobs.Spec) (jobs.Status, bool) {
		return jobs.Status{Done: true, Failed: 1, AgentStarted: true, AgentTerminated: "Error"}, true
	}
	e.jobs.resultErr = func(jobs.Spec) error {
		return fmt.Errorf("%w (the terminated agent container was removed with its log)", jobs.ErrLogsGone)
	}
	name := e.newIntent(approver)
	errs := e.driveCollectingErrors(name, v1alpha1.IntentFailed)
	if len(errs) != 0 {
		t.Fatalf("collecting a plan whose log is gone failed %d times, want none: %v", len(errs), errs)
	}
	runs := e.runsOf(name, v1alpha1.IntentStagePlan)
	if len(runs) == 0 {
		t.Fatal("no plan runs")
	}
	for _, run := range runs {
		if run.Status.Outcome != OutcomeAborted || !strings.Contains(run.Status.Detail, jobs.LogsGoneDetail) {
			t.Errorf("run %s = %q %q, want aborted naming the lost log", run.Name, run.Status.Outcome,
				run.Status.Detail)
		}
	}
}

// TestTransientLogReadRetries: a log read that fails for a reason that may
// pass is retried, and the run settles from the log once it reads.
func TestTransientLogReadRetries(t *testing.T) {
	e := newEnv(t, testProject())
	errFlaky := errors.New("jobs: read logs: connection reset by peer")
	failures := 0
	e.jobs.resultErr = func(spec jobs.Spec) error {
		if spec.Phase == "build" && failures < 2 {
			failures++
			return errFlaky
		}
		return nil
	}
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	errs := e.driveCollectingErrors(name, v1alpha1.IntentInReview)
	if len(errs) != 2 || !errors.Is(errs[0], errFlaky) {
		t.Fatalf("errors = %v, want the two transient read failures returned for a retry", errs)
	}
	runs := e.runsOf(name, v1alpha1.IntentStageBuild)
	if len(runs) != 1 || runs[0].Status.Outcome != "ok" {
		t.Fatalf("build runs = %+v, want the one run, complete", runs)
	}
}
