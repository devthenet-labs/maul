// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/action"
)

func depRef(n int64, state, reason string) IntentBlockerRef {
	return IntentBlockerRef{Number: n, URL: "https://github.com/acme/intents/issues/" + strconv.FormatInt(n, 10),
		State: state, StateReason: reason}
}

func waitingComment(d IntentDependencyStatus) func() (string, error) {
	d.Waiting = true
	return func() (string, error) {
		return RenderIntentStatusComment(IntentStatusComment{
			Namespace: "maul", Intent: "target-3", Phase: "Pending",
			Commands: []string{action.VerbStart, action.VerbCancel}, Dependencies: &d,
		})
	}
}

func startedComment(d IntentDependencyStatus) func() (string, error) {
	return func() (string, error) {
		return RenderIntentStatusComment(IntentStatusComment{
			Namespace: "maul", Intent: "target-3", Phase: "Planning", Dependencies: &d,
		})
	}
}

// TestIntentDependencyGoldens pins every wording of the status comment's
// dependency section and the late-blocker notice.
func TestIntentDependencyGoldens(t *testing.T) {
	released := time.Date(2026, 10, 28, 14, 5, 0, 0, time.UTC)
	other := IntentBlockerRef{Repository: "acme/app", Number: 4, URL: "https://github.com/acme/app/issues/4",
		State: "open"}
	hidden := IntentBlockerRef{Hidden: true, Number: 9, State: "open"}
	tests := []struct {
		name   string
		render func() (string, error)
	}{
		{"intent_status_waiting_open.md", waitingComment(IntentDependencyStatus{
			Reason:   v1alpha1.ReasonOpenBlockers,
			Blockers: []IntentBlockerRef{depRef(12, "open", ""), other, hidden},
		})},
		{"intent_status_waiting_not_completed.md", waitingComment(IntentDependencyStatus{
			Reason:   v1alpha1.ReasonBlockerNotCompleted,
			Blockers: []IntentBlockerRef{depRef(14, "closed", "not_planned")},
		})},
		{"intent_status_waiting_unreadable.md", waitingComment(IntentDependencyStatus{
			Reason: v1alpha1.ReasonBlockerUnreadable, Unreadable: 2,
		})},
		{"intent_status_waiting_cycle.md", waitingComment(IntentDependencyStatus{
			Reason: v1alpha1.ReasonDependencyCycle, Blockers: []IntentBlockerRef{depRef(12, "open", "")},
			Cycle: []IntentBlockerRef{depRef(12, "open", "")},
		})},
		{"intent_status_waiting_cycle_long.md", waitingComment(IntentDependencyStatus{
			Reason: v1alpha1.ReasonDependencyCycle, Blockers: []IntentBlockerRef{depRef(12, "open", "")},
			Cycle: []IntentBlockerRef{depRef(12, "open", ""), depRef(15, "open", "")},
		})},
		{"intent_status_waiting_checking.md", waitingComment(IntentDependencyStatus{
			Reason: v1alpha1.ReasonCheckingDependencies, GiveUpAt: released,
		})},
		{"intent_status_waiting_slot.md", waitingComment(IntentDependencyStatus{
			Reason: v1alpha1.ReasonNoActiveSlot, MaxActiveIntents: 2,
		})},
		{"intent_status_waiting_more.md", waitingComment(IntentDependencyStatus{
			Reason: v1alpha1.ReasonOpenBlockers, Blockers: []IntentBlockerRef{depRef(12, "open", "")}, More: 3,
		})},
		{"intent_status_released.md", startedComment(IntentDependencyStatus{
			Reason: v1alpha1.ReasonBlockersClosed, ReleasedAt: released,
			Blockers: []IntentBlockerRef{depRef(12, "open", ""), depRef(14, "open", "")},
		})},
		{"intent_status_started_by.md", startedComment(IntentDependencyStatus{
			Reason: v1alpha1.ReasonStartedByApprover, StartedBy: "peter", ReleasedAt: released,
			Blockers: []IntentBlockerRef{depRef(12, "open", "")},
		})},
		{"intent_status_degraded.md", startedComment(IntentDependencyStatus{
			Reason: v1alpha1.ReasonDependenciesUnsupported, ReleasedAt: released,
		})},
		{"intent_status_late.md", startedComment(IntentDependencyStatus{
			Reason: v1alpha1.ReasonNoBlockers, ReleasedAt: released,
			Late: []IntentBlockerRef{depRef(20, "open", "")},
		})},
		{"intent_notice_dependencies_late.md", func() (string, error) {
			return RenderLateDependenciesNotice(LateDependenciesNotice{
				Namespace: "maul", Intent: "target-3", Key: "dependencies-late",
				Blockers: []IntentBlockerRef{depRef(20, "open", ""), hidden},
			})
		}},
		{"intent_notice_done_start.md", func() (string, error) {
			return RenderCommandDoneNotice(CommandDoneNotice{
				Namespace: "maul", Intent: "target-3", Key: "comment-7", Actor: "peter", Verb: action.VerbStart,
			})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.render()
			if err != nil {
				t.Fatal(err)
			}
			golden(t, tt.name, got)
		})
	}
}

// TestNoDependencySectionUnchanged: an intent that never waited renders the
// comment exactly as without the section.
func TestNoDependencySectionUnchanged(t *testing.T) {
	base := IntentStatusComment{Namespace: "maul", Intent: "target-1", Phase: "Planning"}
	want, err := RenderIntentStatusComment(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []*IntentDependencyStatus{
		{Reason: v1alpha1.ReasonNoBlockers},
		{Reason: v1alpha1.ReasonBlockersClosed},
	} {
		c := base
		c.Dependencies = d
		got, err := RenderIntentStatusComment(c)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("Dependencies %+v changed the comment:\n%s", d, got)
		}
	}
}

// TestHiddenBlockersNeverNamed is a seeded property: a hidden blocker's
// number and URL never reach any rendering, whatever else it is shown with.
func TestHiddenBlockersNeverNamed(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 11))
	reasons := []string{v1alpha1.ReasonOpenBlockers, v1alpha1.ReasonBlockerNotCompleted,
		v1alpha1.ReasonBlockerUnreadable, v1alpha1.ReasonDependencyCycle, v1alpha1.ReasonBlockersClosed,
		v1alpha1.ReasonStartedByApprover}
	for range 500 {
		n := r.IntN(5)
		bs := make([]IntentBlockerRef, 0, n+1)
		for i := range n {
			bs = append(bs, depRef(int64(10+i), []string{"open", "closed"}[r.IntN(2)], ""))
		}
		secret := IntentBlockerRef{Hidden: true, Repository: "secret-org/private-repo", Number: 4242,
			URL: "https://github.com/secret-org/private-repo/issues/4242", State: []string{"open", "closed"}[r.IntN(2)]}
		bs = append(bs, secret)
		r.Shuffle(len(bs), func(i, j int) { bs[i], bs[j] = bs[j], bs[i] })
		d := IntentDependencyStatus{Waiting: r.IntN(2) == 0, Reason: reasons[r.IntN(len(reasons))], Blockers: bs,
			Late: bs, StartedBy: "peter"}
		got, err := RenderIntentStatusComment(IntentStatusComment{Namespace: "maul", Intent: "t-1", Phase: "Pending",
			Dependencies: &d})
		if err != nil {
			t.Fatal(err)
		}
		notice, err := RenderLateDependenciesNotice(LateDependenciesNotice{Namespace: "maul", Intent: "t-1",
			Key: "k", Blockers: bs})
		if err != nil {
			t.Fatal(err)
		}
		for _, text := range []string{got, notice} {
			if strings.Contains(text, "secret-org") || strings.Contains(text, "4242") {
				t.Fatalf("a hidden blocker was named:\n%s", text)
			}
		}
	}
}
