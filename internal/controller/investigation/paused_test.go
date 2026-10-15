// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package investigation

import (
	"reflect"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

// TestPausedMarksHeldRuns: while the agents are paused, the scheduler marks
// every run the pause holds (pending, or granted with no Job yet) with
// AgentsPaused, and nothing else; a second pass writes nothing; once the
// pause is lifted the mark is removed. A run that never waited on a pause is
// never written.
func TestPausedMarksHeldRuns(t *testing.T) {
	launched := pendingInv("inv-launched", "f0", "low")
	launched.Status.Phase = v1alpha1.RunRunning
	launched.Status.JobRef = &v1alpha1.JobReference{Namespace: "patchy-agents", Name: "job-0"}
	granted := pendingInv("inv-granted", "f1", "low")
	granted.Status.Phase = v1alpha1.RunRunning
	done := pendingInv("inv-done", "f2", "low")
	done.Status.Phase = v1alpha1.RunComplete
	r, c := invWith(t, &fakeRunner{}, nil, launched, granted, done, pendingInv("inv-pending", "f3", "high"))
	r.MaxConcurrent = 1
	get := func(name string) *v1alpha1.Investigation {
		t.Helper()
		var inv v1alpha1.Investigation
		if err := c.Get(t.Context(), types.NamespacedName{Namespace: "patchy", Name: name}, &inv); err != nil {
			t.Fatal(err)
		}
		return &inv
	}
	versions := func() map[string]string {
		out := map[string]string{}
		for _, n := range []string{"inv-launched", "inv-granted", "inv-done", "inv-pending"} {
			out[n] = get(n).ResourceVersion
		}
		return out
	}

	r.Paused = true
	if _, err := scheduleOnce(t, r); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	for name, held := range map[string]bool{
		"inv-launched": false, "inv-granted": true, "inv-done": false, "inv-pending": true,
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
	before := versions()
	if _, err := scheduleOnce(t, r); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if after := versions(); !reflect.DeepEqual(before, after) {
		t.Errorf("a second paused pass wrote status: %v -> %v", before, after)
	}

	r.Paused = false
	if _, err := scheduleOnce(t, r); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	for _, name := range []string{"inv-launched", "inv-granted", "inv-done", "inv-pending"} {
		if cond := meta.FindStatusCondition(get(name).Status.Conditions, v1alpha1.ConditionAgentsPaused); cond != nil {
			t.Errorf("%s AgentsPaused = %+v after the pause lifted, want none", name, cond)
		}
	}
}

// TestUnpauseGrantsOnTheNextPass: the pass that removes AgentsPaused once
// the pause lifts grants nothing (a grant would re-read the runs it just
// wrote through a possibly stale cache) and asks to run again shortly; that
// next pass grants.
func TestUnpauseGrantsOnTheNextPass(t *testing.T) {
	r, c := invWith(t, &fakeRunner{}, nil, pendingInv("inv-pending", "f0", "high"))
	get := func() *v1alpha1.Investigation {
		t.Helper()
		var inv v1alpha1.Investigation
		if err := c.Get(t.Context(), types.NamespacedName{Namespace: "patchy", Name: "inv-pending"}, &inv); err != nil {
			t.Fatal(err)
		}
		return &inv
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
	inv := get()
	if cond := meta.FindStatusCondition(inv.Status.Conditions, v1alpha1.ConditionAgentsPaused); cond != nil {
		t.Errorf("AgentsPaused = %+v after the pause lifted, want none", cond)
	}
	if inv.Status.Phase == v1alpha1.RunRunning {
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

// TestUnpausedSchedulerWritesNoPauseMark: with the agents not paused, one
// scheduler pass leaves runs it does not grant byte-identical.
func TestUnpausedSchedulerWritesNoPauseMark(t *testing.T) {
	launched := pendingInv("inv-launched", "f0", "low")
	launched.Status.Phase = v1alpha1.RunRunning
	launched.Status.JobRef = &v1alpha1.JobReference{Namespace: "patchy-agents", Name: "job-0"}
	done := pendingInv("inv-done", "f2", "low")
	done.Status.Phase = v1alpha1.RunComplete
	objs := []*v1alpha1.Investigation{launched, done}
	r, c := invWith(t, &fakeRunner{}, nil, launched, done)
	snapshot := func() map[string]v1alpha1.Investigation {
		out := map[string]v1alpha1.Investigation{}
		for _, obj := range objs {
			var cur v1alpha1.Investigation
			if err := c.Get(t.Context(), client.ObjectKeyFromObject(obj), &cur); err != nil {
				t.Fatal(err)
			}
			out[obj.Name] = cur
		}
		return out
	}
	before := snapshot()
	if _, err := scheduleOnce(t, r); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	for name, after := range snapshot() {
		if !reflect.DeepEqual(before[name], after) {
			t.Errorf("%s changed while not paused:\n%+v\n%+v", name, before[name].Status, after.Status)
		}
	}
}
