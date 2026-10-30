// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intentview

import (
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

const waitRepo = "https://github.com/acme/intents"

func waitingIntent(phase v1alpha1.IntentPhase, status metav1.ConditionStatus, reason string,
	bs ...v1alpha1.IntentBlocker) *v1alpha1.Intent {
	in := &v1alpha1.Intent{
		Spec:   v1alpha1.IntentSpec{Issue: v1alpha1.IntentIssue{Repository: waitRepo, Number: 1}},
		Status: v1alpha1.IntentStatus{Phase: phase, Dependencies: &v1alpha1.IntentDependencies{Blockers: bs}},
	}
	if reason != "" {
		in.Status.Conditions = []metav1.Condition{{Type: v1alpha1.ConditionWaitingOnDependencies, Status: status,
			Reason: reason}}
	}
	return in
}

func TestWaiting(t *testing.T) {
	own := v1alpha1.IntentBlocker{Repository: waitRepo, Number: 12, State: "open"}
	other := v1alpha1.IntentBlocker{Repository: "https://github.com/secret/repo", Number: 3, State: "open"}
	tests := []struct {
		name string
		in   *v1alpha1.Intent
		want *WaitingView
	}{
		{"not waiting", waitingIntent(v1alpha1.IntentPending, metav1.ConditionFalse, v1alpha1.ReasonBlockersClosed),
			nil},
		{"started", waitingIntent(v1alpha1.IntentPlanning, metav1.ConditionTrue, v1alpha1.ReasonOpenBlockers), nil},
		{"open", waitingIntent(v1alpha1.IntentPending, metav1.ConditionTrue, v1alpha1.ReasonOpenBlockers, own, other),
			&WaitingView{Reason: "waiting for the issues it is blocked by", Refs: []string{"#12"}, Others: 1}},
		{"slot", waitingIntent(v1alpha1.IntentPending, metav1.ConditionTrue, v1alpha1.ReasonNoActiveSlot),
			&WaitingView{Reason: "waiting for an active slot"}},
		{"unknown reason", waitingIntent(v1alpha1.IntentPending, metav1.ConditionTrue, "SomethingNew"),
			&WaitingView{Reason: "waiting for the issues it is blocked by"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Waiting(tt.in)
			if (got == nil) != (tt.want == nil) || got != nil && (got.Reason != tt.want.Reason ||
				!slices.Equal(got.Refs, tt.want.Refs) || got.Others != tt.want.Others) {
				t.Errorf("Waiting = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestWaitingNeverNamesAnotherRepository is a seeded property: Refs hold
// only the intent repository's numbers, and every other blocker is counted.
func TestWaitingNeverNamesAnotherRepository(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 6))
	repos := []string{waitRepo, waitRepo + "/", "https://github.com/ACME/intents", "https://github.com/secret/repo",
		"https://github.com/acme/app"}
	for range 1000 {
		n := r.IntN(6)
		bs := make([]v1alpha1.IntentBlocker, 0, n)
		wantOthers := 0
		for i := range n {
			repo := repos[r.IntN(len(repos))]
			bs = append(bs, v1alpha1.IntentBlocker{Repository: repo, Number: int64(100 + i), State: "open"})
			if !sameRepo(repo, waitRepo) {
				wantOthers++
			}
		}
		v := Waiting(waitingIntent(v1alpha1.IntentPending, metav1.ConditionTrue, v1alpha1.ReasonOpenBlockers, bs...))
		if v.Others != wantOthers || len(v.Refs)+v.Others != len(bs) {
			t.Fatalf("blockers %+v: view %+v", bs, v)
		}
		for _, ref := range v.Refs {
			if !strings.HasPrefix(ref, "#") || strings.Contains(ref, "/") {
				t.Fatalf("ref %q names a repository", ref)
			}
		}
	}
}
