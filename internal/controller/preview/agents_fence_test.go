// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"context"
	"reflect"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// agentsPV is an outcome verification placed in the agents namespace, the
// slot one's twin in every other field, so only the fence keeps it out.
func (e *verifyEnv) agentsPV(phase v1alpha1.PreviewVerificationPhase) *v1alpha1.PreviewVerification {
	e.t.Helper()
	pv := e.toolPV(func(pv *v1alpha1.PreviewVerification) {
		pv.Name = "agents-" + pv.Name
		pv.UID = "pv-agents"
		pv.Spec.Placement = v1alpha1.PlacementAgents
	})
	pv.Status.Phase = phase
	e.mustStatus(pv)
	return pv
}

func TestSlotPlacementPredicate(t *testing.T) {
	for _, tt := range []struct {
		name      string
		placement v1alpha1.VerificationPlacement
		want      bool
	}{
		{"unset is slot", "", true},
		{"slot", v1alpha1.PlacementSlot, true},
		{"agents", v1alpha1.PlacementAgents, false},
	} {
		pv := &v1alpha1.PreviewVerification{Spec: v1alpha1.PreviewVerificationSpec{Placement: tt.placement}}
		if got := slotPlacement(pv); got != tt.want {
			t.Errorf("%s: slotPlacement = %v, want %v", tt.name, got, tt.want)
		}
	}
	if slotPlacement(&v1alpha1.Preview{}) {
		t.Error("slotPlacement admitted a Preview")
	}
}

func TestAgentsVerificationIsNeverReconciled(t *testing.T) {
	for _, tt := range []struct {
		name string
		mut  func(*v1alpha1.PreviewVerification)
	}{
		{"a live preview it could start on", func(*v1alpha1.PreviewVerification) {}},
		{"a preview that is not there (would be Stale)", func(pv *v1alpha1.PreviewVerification) {
			pv.Spec.PreviewRef = v1alpha1.ObjectReference{Name: "gone", UID: types.UID("gone")}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newToolEnv(t)
			pv := e.toolPV(func(pv *v1alpha1.PreviewVerification) {
				pv.Name, pv.UID = "agents-"+pv.Name, "pv-agents"
				pv.Spec.Placement = v1alpha1.PlacementAgents
				tt.mut(pv)
			})
			before := pv.DeepCopy()
			res, err := e.v.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pv)})
			if err != nil || res != (ctrl.Result{}) {
				t.Fatalf("reconcile = %+v, %v; want nothing", res, err)
			}
			var got v1alpha1.PreviewVerification
			if err := e.c.Get(context.Background(), client.ObjectKeyFromObject(pv), &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Status, before.Status) || got.Status.Phase != "" {
				t.Errorf("status changed: %+v", got.Status)
			}
			var jobs batchv1.JobList
			if err := e.c.List(context.Background(), &jobs); err != nil || len(jobs.Items) != 0 {
				t.Errorf("jobs = %d, %v; want none", len(jobs.Items), err)
			}
		})
	}
}

func TestAgentsVerificationIsNotCountedForRoom(t *testing.T) {
	e := newToolEnv(t)
	e.v.Settings.MaxEvaluations = 1
	e.agentsPV(v1alpha1.VerificationRunning)
	mine := e.pv
	room, err := e.v.evaluationRoom(context.Background(), mine)
	if err != nil || !room {
		t.Fatalf("room = %v, %v; an agents verification took a slot tool pod's room", room, err)
	}
	other := e.toolPV(func(pv *v1alpha1.PreviewVerification) {
		pv.Name += "-other"
		pv.UID = "pv-slot-running"
	})
	other.Status.Phase = v1alpha1.VerificationRunning
	e.mustStatus(other)
	if room, err := e.v.evaluationRoom(context.Background(), mine); err != nil || room {
		t.Fatalf("room = %v, %v; a running slot verification must still count", room, err)
	}
}

func TestAgentsVerificationIsNotCaptured(t *testing.T) {
	e := newToolEnv(t)
	pv := e.agentsPV(v1alpha1.VerificationRunning)
	if err := e.v.BeforeRelease(context.Background(), e.p); err != nil {
		t.Fatal(err)
	}
	var got v1alpha1.PreviewVerification
	if err := e.c.Get(context.Background(), client.ObjectKeyFromObject(pv), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Status, pv.Status) {
		t.Errorf("BeforeRelease wrote an agents verification: %+v", got.Status)
	}
}

func TestSweepDoesNotKnowAnAgentsVerification(t *testing.T) {
	e := newToolEnv(t)
	agents := e.agentsPV(v1alpha1.VerificationRunning)
	slot := e.pv
	sw := &Sweeper{Client: e.c, Settings: verifySettings(), Now: func() time.Time { return e.now }}
	live, err := sw.liveVerifications(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if live[string(agents.UID)] || !live[string(slot.UID)] {
		t.Errorf("live = %v; want the slot verification only", live)
	}
}
