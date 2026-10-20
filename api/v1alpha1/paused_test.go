// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1

import (
	"reflect"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestHeldByPause(t *testing.T) {
	job := &JobReference{Namespace: "maul-agents", Name: "j"}
	tests := []struct {
		paused bool
		phase  RunPhase
		job    *JobReference
		want   bool
	}{
		{true, "", nil, true},
		{true, RunPending, nil, true},
		{true, RunRunning, nil, true},
		{true, RunRunning, job, false},
		{true, RunComplete, job, false},
		{true, RunFailed, nil, false},
		{false, "", nil, false},
		{false, RunRunning, nil, false},
	}
	for _, tt := range tests {
		if got := HeldByPause(tt.paused, tt.phase, tt.job); got != tt.want {
			t.Errorf("HeldByPause(%v, %q, job=%v) = %v, want %v", tt.paused, tt.phase, tt.job != nil, got, tt.want)
		}
	}
}

// TestSyncAgentsPaused: held sets the condition once and then reports no
// change; not held removes it, and on conditions that never carried it
// changes nothing at all.
func TestSyncAgentsPaused(t *testing.T) {
	t0 := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	other := metav1.Condition{Type: ConditionComplete, Status: metav1.ConditionTrue, Reason: "x",
		LastTransitionTime: metav1.NewTime(t0)}
	conds := []metav1.Condition{other}
	before := append([]metav1.Condition(nil), conds...)
	if SyncAgentsPaused(&conds, false, t0) || !reflect.DeepEqual(conds, before) {
		t.Fatalf("not held, never paused: changed to %+v", conds)
	}
	if !SyncAgentsPaused(&conds, true, t0) {
		t.Fatal("held: want a change")
	}
	if len(conds) != 2 || conds[1].Type != ConditionAgentsPaused || conds[1].Status != metav1.ConditionTrue ||
		conds[1].Reason != ReasonAgentsPaused || conds[1].Message != AgentsPausedMessage ||
		!conds[1].LastTransitionTime.Time.Equal(t0) {
		t.Fatalf("held: conditions = %+v", conds)
	}
	if SyncAgentsPaused(&conds, true, t0.Add(time.Hour)) {
		t.Errorf("held again: want no change, got %+v", conds)
	}
	if !SyncAgentsPaused(&conds, false, t0) || !reflect.DeepEqual(conds, before) {
		t.Errorf("lifted: conditions = %+v, want %+v", conds, before)
	}
}
