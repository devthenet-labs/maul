// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package remediation

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/envelope"
	"github.com/devthenet-labs/maul/internal/runnerguard"
)

// investigatedThen is a finding's phase history from ingest through its
// verdict, followed by more.
func investigatedThen(more ...v1alpha1.Phase) []v1alpha1.Phase {
	return append([]v1alpha1.Phase{v1alpha1.PhaseOpened, v1alpha1.PhaseEnhanced, v1alpha1.PhaseInvestigating},
		more...)
}

// TestRemediationHoldsDefaultImageFallback: a repository that declares an
// accepted agent image never has a remediation silently fall back to the
// default image, which carries none of its toolchain. Whatever kept the
// launch off the repository's image (the kill switch, the sandbox breaker,
// a revival), no Job is created: the finding is handed to a human with the
// reason, and the held attempt is marked so it is not counted.
func TestRemediationHoldsDefaultImageFallback(t *testing.T) {
	tripped := runnerguard.NewBreaker("test", nil)
	tripped.Trip(t.Context(), "job-x", "finding-x")
	tests := []struct {
		name    string
		guard   runnerguard.Guard
		history []v1alpha1.Phase
		skipped string
	}{
		{"kill switch off", runnerguard.Guard{},
			investigatedThen(v1alpha1.PhaseQueued, v1alpha1.PhaseRemediating), runnerguard.SkipDisabled},
		{"sandbox breaker tripped", runnerguard.Guard{Enabled: true, Breaker: tripped},
			investigatedThen(v1alpha1.PhaseQueued, v1alpha1.PhaseRemediating), runnerguard.SkipBreaker},
		{"revived out of HandedOff", runnerguard.Guard{Enabled: true},
			investigatedThen(v1alpha1.PhaseHandedOff, v1alpha1.PhaseQueued, v1alpha1.PhaseRemediating),
			runnerguard.SkipRevived},
		{"retried out of Failed", runnerguard.Guard{Enabled: true},
			investigatedThen(v1alpha1.PhaseQueued, v1alpha1.PhaseRemediating, v1alpha1.PhaseFailed,
				v1alpha1.PhaseQueued, v1alpha1.PhaseRemediating),
			runnerguard.SkipRevived},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := withImage(acceptedImage(), "", tt.history...)
			objs[1].(*v1alpha1.Remediation).Status.JobRef = nil // granted, not launched
			runner := &fakeCRRunner{}
			fw := &fakeForge{}
			r, c := newRemediation(t, runner, fw, objs...)
			r.Images = tt.guard
			remReconcile(t, r)

			if len(runner.created) != 0 {
				t.Fatalf("jobs = %d (image %q), want none: the run would fall back to the default image",
					len(runner.created), runner.created[0].RunnerImage)
			}
			f := findingNow(t, c)
			if f.Status.Phase != v1alpha1.PhaseHandedOff {
				t.Errorf("phase = %q, want HandedOff", f.Status.Phase)
			}
			if !strings.Contains(f.Status.LastFailureReason, tt.skipped) ||
				!strings.Contains(f.Status.LastFailureReason, "not be built or tested") {
				t.Errorf("lastFailureReason = %q, want the skip reason %q and the untested consequence",
					f.Status.LastFailureReason, tt.skipped)
			}
			if f.Status.Remediation == nil || f.Status.Remediation.Outcome != v1alpha1.OutcomeDefaultImageHeld {
				t.Errorf("remediation summary = %+v, want outcome %q", f.Status.Remediation,
					v1alpha1.OutcomeDefaultImageHeld)
			}
			if f.Status.ActiveRun != nil {
				t.Errorf("activeRun = %+v, want cleared", f.Status.ActiveRun)
			}
			rem := getRem(t, c)
			if rem.Status.Phase != v1alpha1.RunFailed || !runnerguard.Held(rem.Status.Conditions) ||
				rem.Status.JobRef != nil {
				t.Errorf("run phase %q, held %v, jobRef %+v: want Failed, held, no Job",
					rem.Status.Phase, runnerguard.Held(rem.Status.Conditions), rem.Status.JobRef)
			}
			if len(fw.pushed) != 0 || fw.prCalls != 0 {
				t.Errorf("pushed/prs = %v/%d, want no forge call", fw.pushed, fw.prCalls)
			}
		})
	}
}

// TestRemediationRunsDefaultImageOnceHoldApproved: approving the hold is a
// human accepting an untested run. The next attempt launches on the default
// image, the held attempt does not count toward MaxAttempts, and the pull
// request the run opens says plainly that maul did not build or test it.
func TestRemediationRunsDefaultImageOnceHoldApproved(t *testing.T) {
	// Attempt 1 was held; the approval revived the finding into attempt 2.
	held := runningRemediation()[1].(*v1alpha1.Remediation)
	held.Status = v1alpha1.RemediationStatus{Phase: v1alpha1.RunFailed,
		Conditions: []metav1.Condition{runnerguard.HeldCondition(1, runnerguard.SkipDisabled)}}

	objs := withImage(acceptedImage(), "", investigatedThen(v1alpha1.PhaseQueued, v1alpha1.PhaseRemediating,
		v1alpha1.PhaseHandedOff, v1alpha1.PhaseQueued, v1alpha1.PhaseRemediating)...)
	objs[0].(*v1alpha1.Finding).Status.Attempts.Remediation = 2
	rem2 := objs[1].(*v1alpha1.Remediation)
	rem2.Name, rem2.Spec.Attempt, rem2.Status.JobRef = "finding-aa-1-rem-2", 2, nil
	objs = append(objs, held)

	runner := &fakeCRRunner{}
	fw := &fakeForge{}
	r, c := newRemediation(t, runner, fw, objs...)
	r.Images = runnerguard.Guard{Enabled: true}
	remOnce(t, r, "finding-aa-1-rem-2")

	if len(runner.created) != 1 || runner.created[0].RunnerImage != "" {
		t.Fatalf("jobs = %+v, want one default-image Job", runner.created)
	}

	// The default-image run succeeds: its pull request is marked untested.
	runner.done, runner.events = true, crdRemediationEvent(true)
	remOnce(t, r, "finding-aa-1-rem-2")
	if len(fw.bodies) != 1 || !strings.Contains(fw.bodies[0], untestedMarker) {
		t.Errorf("PR bodies = %q, want the untested warning", fw.bodies)
	}
	if f := findingNow(t, c); f.Status.Phase != v1alpha1.PhaseInReview {
		t.Errorf("phase = %q, want InReview", f.Status.Phase)
	}
}

// TestRemediationHeldAttemptConsumesNoAttempt: a held attempt never ran an
// agent, so an ordinary failure of the approved run after it is the first
// real attempt and is retried, not exhausted.
func TestRemediationHeldAttemptConsumesNoAttempt(t *testing.T) {
	held := runningRemediation()[1].(*v1alpha1.Remediation)
	held.Status = v1alpha1.RemediationStatus{Phase: v1alpha1.RunFailed,
		Conditions: []metav1.Condition{runnerguard.HeldCondition(1, runnerguard.SkipBreaker)}}
	objs := withImage(acceptedImage(), v1alpha1.RunnerImageSourceDefault)
	objs[0].(*v1alpha1.Finding).Status.Attempts.Remediation = 2
	rem2 := objs[1].(*v1alpha1.Remediation)
	rem2.Name, rem2.Spec.Attempt = "finding-aa-1-rem-2", 2
	objs = append(objs, held)

	runner := &fakeCRRunner{done: true, events: crdFatal()}
	r, c := newRemediation(t, runner, &fakeForge{}, objs...)
	remOnce(t, r, "finding-aa-1-rem-2")
	if f := findingNow(t, c); f.Status.Phase != v1alpha1.PhaseQueued {
		t.Errorf("phase = %q (%s), want Queued: the held attempt must not count", f.Status.Phase,
			f.Status.LastFailureReason)
	}
}

// TestRemediationPRBodySaysWhetherTested: a run on maul's default image —
// here a repository that declares nothing, which keeps today's behaviour of
// remediating there — opens a pull request that says it was not built or
// tested; a run on the repository's own image does not.
func TestRemediationPRBodySaysWhetherTested(t *testing.T) {
	for _, tt := range []struct {
		name         string
		ri           *v1alpha1.RunnerImage
		source       string
		wantUntested bool
	}{
		{"nothing declared", nil, v1alpha1.RunnerImageSourceDefault, true},
		{"repository image", acceptedImage(), v1alpha1.RunnerImageSourceRepository, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			runner := &fakeCRRunner{done: true, events: crdRemediationEvent(true)}
			fw := &fakeForge{}
			r, _ := newRemediation(t, runner, fw, withImage(tt.ri, tt.source)...)
			remReconcile(t, r)
			if len(fw.bodies) != 1 {
				t.Fatalf("PR bodies = %d, want 1", len(fw.bodies))
			}
			if got := strings.Contains(fw.bodies[0], untestedMarker); got != tt.wantUntested {
				t.Errorf("untested warning present = %v, want %v:\n%s", got, tt.wantUntested, fw.bodies[0])
			}
		})
	}
}

// TestRemediationNoDeclarationLaunchesDefaultImage: a repository that
// declares no image is not held; it remediates on the default image as it
// always has.
func TestRemediationNoDeclarationLaunchesDefaultImage(t *testing.T) {
	for _, tt := range []struct {
		name string
		ri   *v1alpha1.RunnerImage
	}{
		{"nothing declared", nil},
		{"rejected under onReject default", &v1alpha1.RunnerImage{
			Declared: "ghcr.io/acme/go-env:1.26", Manifest: ".maul/agent.yaml", Rejected: "NotAllowlisted",
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			objs := withImage(tt.ri, "", investigatedThen(v1alpha1.PhaseQueued, v1alpha1.PhaseRemediating)...)
			objs[1].(*v1alpha1.Remediation).Status.JobRef = nil
			runner := &fakeCRRunner{}
			r, c := newRemediation(t, runner, &fakeForge{}, objs...)
			r.Images = runnerguard.Guard{}
			remReconcile(t, r)
			if len(runner.created) != 1 {
				t.Fatalf("jobs = %d, want 1", len(runner.created))
			}
			if f := findingNow(t, c); f.Status.Phase != v1alpha1.PhaseRemediating {
				t.Errorf("phase = %q, want Remediating", f.Status.Phase)
			}
		})
	}
}

// untestedMarker is the sentence the pull request body opens its untested
// warning with.
const untestedMarker = "maul did not build or test this change"

// crdFatal is a run that ended in a fatal event: an ordinary failure.
func crdFatal() []envelope.Event {
	return []envelope.Event{{V: envelope.Version, Type: envelope.TypeFatal, Error: "claude exited 1"}}
}
