// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package remediation

import (
	"reflect"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

// TestRemediationPausedMarksHeldRuns: while the agents are paused, the
// scheduler marks every remediation the pause holds (pending, or granted
// with no Job yet) with AgentsPaused, and nothing else; a second pass writes
// nothing; once the pause is lifted the mark is removed. A run that never
// waited on a pause is never written.
func TestRemediationPausedMarksHeldRuns(t *testing.T) {
	launched := pendingRem("rem-launched", "f0", 10)
	launched.Status.Phase = v1alpha1.RunRunning
	launched.Status.JobRef = &v1alpha1.JobReference{Namespace: "patchy-agents", Name: "job-0"}
	granted := pendingRem("rem-granted", "f1", 10)
	granted.Status.Phase = v1alpha1.RunRunning
	done := pendingRem("rem-done", "f2", 10)
	done.Status.Phase = v1alpha1.RunFailed
	names := []string{"rem-launched", "rem-granted", "rem-done", "rem-pending"}
	r, c := remWith(t, &fakeCRRunner{}, &fakeForge{}, nil, launched, granted, done, pendingRem("rem-pending", "f3", 50))
	r.MaxConcurrent = 1
	get := func(name string) *v1alpha1.Remediation {
		t.Helper()
		var rem v1alpha1.Remediation
		if err := c.Get(t.Context(), types.NamespacedName{Namespace: "patchy", Name: name}, &rem); err != nil {
			t.Fatal(err)
		}
		return &rem
	}
	statuses := func() map[string]v1alpha1.RemediationStatus {
		out := map[string]v1alpha1.RemediationStatus{}
		for _, n := range names {
			out[n] = get(n).Status
		}
		return out
	}

	unpaused := statuses()
	if _, err := scheduleOnce(t, r); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	for _, n := range []string{"rem-launched", "rem-done"} {
		if got := get(n).Status; !reflect.DeepEqual(got, unpaused[n]) {
			t.Errorf("%s status changed while not paused: %+v", n, got)
		}
	}

	r.Paused = true
	if _, err := scheduleOnce(t, r); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	// rem-launched holds the one slot, so rem-pending was never granted.
	for name, held := range map[string]bool{
		"rem-launched": false, "rem-granted": true, "rem-done": false, "rem-pending": true,
	} {
		cond := meta.FindStatusCondition(get(name).Status.Conditions, v1alpha1.ConditionAgentsPaused)
		switch {
		case held && (cond == nil || cond.Status != metav1.ConditionTrue ||
			cond.Reason != v1alpha1.ReasonAgentsPaused || cond.Message != v1alpha1.AgentsPausedMessage):
			t.Errorf("%s AgentsPaused = %+v, want True/Paused with the fixed message", name, cond)
		case !held && cond != nil:
			t.Errorf("%s AgentsPaused = %+v, want none: the pause does not hold it", name, cond)
		}
	}
	before := statuses()
	if _, err := scheduleOnce(t, r); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if after := statuses(); !reflect.DeepEqual(before, after) {
		t.Errorf("a second paused pass wrote status")
	}

	r.Paused = false
	if _, err := scheduleOnce(t, r); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	for _, name := range names {
		if cond := meta.FindStatusCondition(get(name).Status.Conditions, v1alpha1.ConditionAgentsPaused); cond != nil {
			t.Errorf("%s AgentsPaused = %+v after the pause lifted, want none", name, cond)
		}
	}
}

// TestRemediationUnpauseGrantsOnTheNextPass: the pass that removes
// AgentsPaused once the pause lifts grants nothing (a grant would re-read the
// runs it just wrote through a possibly stale cache) and asks to run again
// shortly; that next pass grants.
func TestRemediationUnpauseGrantsOnTheNextPass(t *testing.T) {
	fnd := queuedFinding(v1alpha1.PhaseQueued)
	r, c := remWith(t, &fakeCRRunner{}, &fakeForge{}, nil, fnd, pendingRem("rem-a", fnd.Name, 50))
	get := func() *v1alpha1.Remediation {
		t.Helper()
		var rem v1alpha1.Remediation
		if err := c.Get(t.Context(), types.NamespacedName{Namespace: "patchy", Name: "rem-a"}, &rem); err != nil {
			t.Fatal(err)
		}
		return &rem
	}
	r.Paused = true
	if _, err := scheduleOnce(t, r); err != nil {
		t.Fatalf("paused schedule: %v", err)
	}

	r.Paused = false
	res, err := scheduleOnce(t, r)
	if err != nil {
		t.Fatalf("unpause schedule: %v", err)
	}
	rem := get()
	if cond := meta.FindStatusCondition(rem.Status.Conditions, v1alpha1.ConditionAgentsPaused); cond != nil {
		t.Errorf("AgentsPaused = %+v after the pause lifted, want none", cond)
	}
	if rem.Status.Phase == v1alpha1.RunRunning {
		t.Errorf("phase = Running: the unpause pass granted a run it had just written")
	}
	if res.RequeueAfter <= 0 || res.RequeueAfter > time.Second {
		t.Errorf("requeue = %v, want a prompt follow-up pass", res.RequeueAfter)
	}

	if _, err := scheduleOnce(t, r); err != nil {
		t.Fatalf("next schedule: %v", err)
	}
	if got := get().Status.Phase; got != v1alpha1.RunRunning {
		t.Errorf("phase = %q after the next pass, want Running", got)
	}
}
