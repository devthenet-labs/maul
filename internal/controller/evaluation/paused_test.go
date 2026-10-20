// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package evaluation

import (
	"reflect"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/devthenet-labs/maul/api/v1alpha1"
)

// TestPausedMarksHeldUnits: while the agents are paused, the scheduler marks
// every unit the pause holds (pending, or granted with no Job yet) with
// AgentsPaused, and nothing else; a second pass writes nothing; once the
// pause is lifted the mark is removed. A unit that never waited on a pause is
// never written.
func TestPausedMarksHeldUnits(t *testing.T) {
	launched := pendingUnit(0)
	launched.Status.Phase = v1alpha1.RunRunning
	launched.Status.JobRef = &v1alpha1.JobReference{Namespace: "maul-agents", Name: "job-0"}
	granted := pendingUnit(1)
	granted.Status.Phase = v1alpha1.RunRunning
	done := pendingUnit(2)
	done.Status.Phase = v1alpha1.RunComplete
	pending := pendingUnit(3)
	held := map[string]bool{launched.Name: false, granted.Name: true, done.Name: false, pending.Name: true}
	c := newClient(t, launched, granted, done, pending)
	r := newUnitReconciler(c, &fakeRunner{}, present)
	statuses := func() map[string]v1alpha1.EvaluationUnitStatus {
		out := map[string]v1alpha1.EvaluationUnitStatus{}
		for n := range held {
			out[n] = getUnit(t, c, n).Status
		}
		return out
	}

	unpaused := statuses()
	if _, err := r.Reconcile(t.Context(), unitReq(schedulerRequest)); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	for _, n := range []string{launched.Name, done.Name} {
		if got := getUnit(t, c, n).Status; !reflect.DeepEqual(got, unpaused[n]) {
			t.Errorf("%s status changed while not paused: %+v", n, got)
		}
	}

	r.Paused = true
	if _, err := r.Reconcile(t.Context(), unitReq(schedulerRequest)); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	for name, want := range held {
		cond := meta.FindStatusCondition(getUnit(t, c, name).Status.Conditions, v1alpha1.ConditionAgentsPaused)
		switch {
		case want && (cond == nil || cond.Status != metav1.ConditionTrue ||
			cond.Reason != v1alpha1.ReasonAgentsPaused || cond.Message != v1alpha1.AgentsPausedMessage):
			t.Errorf("%s AgentsPaused = %+v, want True/Paused with the fixed message", name, cond)
		case !want && cond != nil:
			t.Errorf("%s AgentsPaused = %+v, want none: the pause does not hold it", name, cond)
		}
	}
	before := statuses()
	if _, err := r.Reconcile(t.Context(), unitReq(schedulerRequest)); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if after := statuses(); !reflect.DeepEqual(before, after) {
		t.Errorf("a second paused pass wrote status")
	}

	r.Paused = false
	if _, err := r.Reconcile(t.Context(), unitReq(schedulerRequest)); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	for name := range held {
		conds := getUnit(t, c, name).Status.Conditions
		if cond := meta.FindStatusCondition(conds, v1alpha1.ConditionAgentsPaused); cond != nil {
			t.Errorf("%s AgentsPaused = %+v after the pause lifted, want none", name, cond)
		}
	}
}

// TestUnpauseGrantsUnitOnTheNextPass: the pass that removes AgentsPaused
// once the pause lifts grants nothing (a grant would re-read the units it
// just wrote through a possibly stale cache) and asks to run again shortly;
// that next pass grants.
func TestUnpauseGrantsUnitOnTheNextPass(t *testing.T) {
	unit := pendingUnit(0)
	c := newClient(t, unit)
	r := newUnitReconciler(c, &fakeRunner{}, present)
	r.Paused = true
	if _, err := r.Reconcile(t.Context(), unitReq(schedulerRequest)); err != nil {
		t.Fatalf("paused schedule: %v", err)
	}

	r.Paused = false
	res, err := r.Reconcile(t.Context(), unitReq(schedulerRequest))
	if err != nil {
		t.Fatalf("unpause schedule: %v", err)
	}
	got := getUnit(t, c, unit.Name)
	if cond := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionAgentsPaused); cond != nil {
		t.Errorf("AgentsPaused = %+v after the pause lifted, want none", cond)
	}
	if got.Status.Phase == v1alpha1.RunRunning {
		t.Errorf("phase = Running: the unpause pass granted a unit it had just written")
	}
	if res.RequeueAfter <= 0 || res.RequeueAfter > time.Second {
		t.Errorf("requeue = %v, want a prompt follow-up pass", res.RequeueAfter)
	}

	if _, err := r.Reconcile(t.Context(), unitReq(schedulerRequest)); err != nil {
		t.Fatalf("next schedule: %v", err)
	}
	if phase := getUnit(t, c, unit.Name).Status.Phase; phase != v1alpha1.RunRunning {
		t.Errorf("phase = %q after the next pass, want Running", phase)
	}
}
