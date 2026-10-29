// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

func blocker(n int64, state, reason string) v1alpha1.IntentBlocker {
	return v1alpha1.IntentBlocker{Repository: intentRepoURL, Number: n, State: state, StateReason: reason}
}

// TestJudgeDependencies pins every verdict a read can reach.
func TestJudgeDependencies(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	recent, old := now.Add(-10*time.Minute), now.Add(-31*time.Minute)
	ok := func(bs ...v1alpha1.IntentBlocker) depRead { return depRead{kind: depReadOK, blockers: bs} }
	tests := []struct {
		name        string
		read        depRead
		since       *time.Time
		cycle       bool
		wantRelease bool
		wantReason  string
		wantPending int
	}{
		{"no blockers", ok(), nil, false, true, v1alpha1.ReasonNoBlockers, 0},
		{"all closed completed", ok(blocker(2, "closed", "completed"), blocker(3, "closed", "")), nil, false, true,
			v1alpha1.ReasonBlockersClosed, 0},
		{"one open", ok(blocker(2, "open", ""), blocker(3, "closed", "completed")), nil, false, false,
			v1alpha1.ReasonOpenBlockers, 1},
		{"open beats not completed", ok(blocker(2, "closed", "not_planned"), blocker(3, "open", "")), nil, false,
			false, v1alpha1.ReasonOpenBlockers, 2},
		{"not planned", ok(blocker(2, "closed", "not_planned")), nil, false, false,
			v1alpha1.ReasonBlockerNotCompleted, 1},
		{"duplicate", ok(blocker(2, "closed", "duplicate")), nil, false, false, v1alpha1.ReasonBlockerNotCompleted, 1},
		{"unknown reason", ok(blocker(2, "closed", "something_new")), nil, false, false,
			v1alpha1.ReasonBlockerNotCompleted, 1},
		{"unreadable only", depRead{kind: depReadOK, unreadable: 1}, nil, false, false,
			v1alpha1.ReasonBlockerUnreadable, 0},
		{"unreadable beside closed", depRead{kind: depReadOK, unreadable: 1,
			blockers: []v1alpha1.IntentBlocker{blocker(2, "closed", "completed")}}, nil, false, false,
			v1alpha1.ReasonBlockerUnreadable, 0},
		{"cycle", ok(blocker(2, "open", "")), nil, true, false, v1alpha1.ReasonDependencyCycle, 1},
		{"cycle needs an open blocker", ok(blocker(2, "closed", "not_planned")), nil, true, false,
			v1alpha1.ReasonBlockerNotCompleted, 1},
		{"transient first", depRead{kind: depReadTransient}, nil, false, false,
			v1alpha1.ReasonCheckingDependencies, 0},
		{"transient within the bound", depRead{kind: depReadTransient}, &recent, false, false,
			v1alpha1.ReasonCheckingDependencies, 0},
		{"transient past the bound", depRead{kind: depReadTransient}, &old, false, true,
			v1alpha1.ReasonDependenciesUnanswered, 0},
		{"unsupported", depRead{kind: depReadUnsupported}, nil, false, true, v1alpha1.ReasonDependenciesUnsupported, 0},
		{"forbidden", depRead{kind: depReadForbidden}, &recent, false, true, v1alpha1.ReasonDependenciesForbidden, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := judgeDependencies(tt.read, tt.since, now, DefaultDependencyGiveUp, tt.cycle)
			if v.release != tt.wantRelease || v.reason != tt.wantReason || len(v.pending) != tt.wantPending {
				t.Errorf("verdict = %+v, want release %v reason %s and %d pending", v, tt.wantRelease, tt.wantReason,
					tt.wantPending)
			}
		})
	}
	v := judgeDependencies(depRead{kind: depReadTransient}, &recent, now, DefaultDependencyGiveUp, false)
	if v.unavailableSince == nil || !v.unavailableSince.Equal(recent) {
		t.Errorf("a failing streak's start = %v, want the recorded %v kept", v.unavailableSince, recent)
	}
	v = judgeDependencies(depRead{kind: depReadTransient}, nil, now, DefaultDependencyGiveUp, false)
	if v.unavailableSince == nil || !v.unavailableSince.Equal(now) {
		t.Errorf("a first failure's streak start = %v, want now", v.unavailableSince)
	}
}

// TestPendingBlockersOrderAndCap: open blockers first, the rest counted
// past MaxIntentBlockers.
func TestPendingBlockersOrderAndCap(t *testing.T) {
	all := make([]v1alpha1.IntentBlocker, 0, 21)
	for i := range 20 {
		state, reason := "open", ""
		if i%2 == 0 {
			state, reason = "closed", "not_planned"
		}
		all = append(all, blocker(int64(i+1), state, reason))
	}
	all = append(all, blocker(99, "closed", "completed"))
	got, more := pendingBlockers(all)
	if len(got) != v1alpha1.MaxIntentBlockers || more != 4 {
		t.Fatalf("pending %d, more %d; want %d and 4", len(got), more, v1alpha1.MaxIntentBlockers)
	}
	for i := range 10 {
		if got[i].State != "open" {
			t.Fatalf("blocker %d is %s: open ones come first", i, got[i].State)
		}
	}
}

// TestJudgeReleaseProperty is a seeded property: a read that succeeded
// releases exactly when every blocker was closed as completed (or with no
// reason) and none is unreadable.
func TestJudgeReleaseProperty(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	states := []struct{ state, reason string }{
		{"open", ""}, {"closed", "completed"}, {"closed", ""}, {"closed", "not_planned"}, {"closed", "duplicate"},
		{"closed", "reopened"},
	}
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	for range 2000 {
		read := depRead{kind: depReadOK, unreadable: int32(r.IntN(3) / 2)}
		want := read.unreadable == 0
		for i := range r.IntN(6) {
			s := states[r.IntN(len(states))]
			read.blockers = append(read.blockers, blocker(int64(i+2), s.state, s.reason))
			if s.state != "closed" || s.reason != "completed" && s.reason != "" {
				want = false
			}
		}
		v := judgeDependencies(read, nil, now, DefaultDependencyGiveUp, r.IntN(2) == 0)
		if v.release != want {
			t.Fatalf("read %+v: release = %v, want %v", read, v.release, want)
		}
	}
}

// TestJudgeGiveUpProperty is a seeded property: a failing streak older than
// the bound always starts the intent, and a younger one never does.
func TestJudgeGiveUpProperty(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	for range 2000 {
		bound := time.Duration(1+r.IntN(120)) * time.Minute
		age := time.Duration(r.IntN(240)) * time.Minute
		since := now.Add(-age)
		v := judgeDependencies(depRead{kind: depReadTransient}, &since, now, bound, false)
		if v.release != (age >= bound) {
			t.Fatalf("streak %v, bound %v: release = %v", age, bound, v.release)
		}
	}
}

// TestFindCycle: none, a two-cycle, a three-cycle, a cycle that does not
// pass through the intent, and a self-block.
func TestFindCycle(t *testing.T) {
	tests := []struct {
		name  string
		edges map[int64][]int64
		want  []int64
	}{
		{"none", map[int64][]int64{1: {2}, 2: {3}}, nil},
		{"two", map[int64][]int64{1: {2}, 2: {1}}, []int64{2, 1}},
		{"three", map[int64][]int64{1: {2}, 2: {3}, 3: {1}}, []int64{2, 3, 1}},
		{"not through this one", map[int64][]int64{1: {2}, 2: {3}, 3: {2}}, nil},
		{"self", map[int64][]int64{1: {1}}, []int64{1}},
		{"first of several", map[int64][]int64{1: {5, 2}, 2: {1}, 5: {1}}, []int64{2, 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := findCycle(1, tt.edges); !slices.Equal(got, tt.want) {
				t.Errorf("findCycle = %v, want %v", got, tt.want)
			}
		})
	}
}

func slotIntent(name string, at int, phase v1alpha1.IntentPhase, reason string) v1alpha1.Intent {
	in := v1alpha1.Intent{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.IntentSpec{Project: "target", RequestedBy: v1alpha1.IntentRequest{
			At: metav1.NewTime(time.Date(2026, 10, 28, 12, at, 0, 0, time.UTC))}},
		Status: v1alpha1.IntentStatus{Phase: phase},
	}
	if reason != "" {
		in.Status.Conditions = []metav1.Condition{{Type: v1alpha1.ConditionWaitingOnDependencies,
			Status: metav1.ConditionTrue, Reason: reason}}
	}
	return in
}

// TestSlotRules: what holds a slot for discovery, and when a released
// intent may start, oldest released waiter first.
func TestSlotRules(t *testing.T) {
	active := slotIntent("a", 0, v1alpha1.IntentPlanning, "")
	waiting := slotIntent("w", 1, v1alpha1.IntentPending, v1alpha1.ReasonOpenBlockers)
	queuedOld := slotIntent("q1", 2, v1alpha1.IntentPending, v1alpha1.ReasonNoActiveSlot)
	queuedNew := slotIntent("q2", 4, v1alpha1.IntentPending, v1alpha1.ReasonNoActiveSlot)
	undecided := slotIntent("u", 3, v1alpha1.IntentPending, "")
	merged := slotIntent("m", 0, v1alpha1.IntentMerged, "")

	for _, tc := range []struct {
		in   v1alpha1.Intent
		want bool
	}{{active, true}, {waiting, false}, {queuedOld, true}, {undecided, true}, {merged, false}} {
		if got := holdsActiveSlot(&tc.in); got != tc.want {
			t.Errorf("holdsActiveSlot(%s) = %v, want %v", tc.in.Name, got, tc.want)
		}
	}
	list := &v1alpha1.IntentList{Items: []v1alpha1.Intent{active, waiting, queuedOld, queuedNew, undecided, merged}}
	if got := activeIntents(list, "target"); got != 4 {
		t.Errorf("activeIntents = %d, want 4: the started, the two queued and the undecided", got)
	}

	all := list.Items
	for _, tc := range []struct {
		self  v1alpha1.Intent
		limit int32
		want  bool
	}{
		// q2 has the started intent and the older q1 ahead of it.
		{queuedNew, 2, false},
		{queuedNew, 3, true},
		// q1 has only the started one ahead.
		{queuedOld, 2, true},
		{queuedOld, 1, false},
		// A waiter decided anew counts the queued ones older than it.
		{undecided, 2, false},
		{undecided, 3, true},
	} {
		if got := slotFree(&tc.self, all, tc.limit); got != tc.want {
			t.Errorf("slotFree(%s, limit %d) = %v, want %v", tc.self.Name, tc.limit, got, tc.want)
		}
	}
}
