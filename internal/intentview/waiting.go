// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intentview

import (
	"fmt"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// WaitingView is a Pending intent's wait on the issues its issue is blocked
// by, as the board shows it: a fixed public wording of why it waits, the
// intent repository's blockers as "#N", and how many it does not name.
type WaitingView struct {
	Reason string
	// Refs are the open-or-not-completed blockers in the intent's own
	// repository, "#N". A blocker in any other repository is only counted in
	// Others, so the board never names a repository the issue does not.
	Refs   []string
	Others int
}

// WaitingText is the public wording of each WaitingOnDependencies reason
// while it is True. It is pinned to the controller's reasons
// (internal/controller/intent/intentview_pin_test.go).
var WaitingText = map[string]string{
	v1alpha1.ReasonOpenBlockers:         "waiting for the issues it is blocked by",
	v1alpha1.ReasonBlockerNotCompleted:  "a blocker was closed without being completed",
	v1alpha1.ReasonBlockerUnreadable:    "a blocker cannot be read",
	v1alpha1.ReasonDependencyCycle:      "dependency cycle",
	v1alpha1.ReasonCheckingDependencies: "checking dependencies",
	v1alpha1.ReasonNoActiveSlot:         "waiting for an active slot",
}

// Waiting is in's wait on its dependencies, nil unless it waits now
// (v1alpha1.Intent.Waiting). An unknown reason shows as the generic wait.
func Waiting(in *v1alpha1.Intent) *WaitingView {
	if !in.Waiting() {
		return nil
	}
	v := &WaitingView{Reason: WaitingText[v1alpha1.ReasonOpenBlockers]}
	for _, c := range in.Status.Conditions {
		if c.Type == v1alpha1.ConditionWaitingOnDependencies {
			if t, ok := WaitingText[c.Reason]; ok {
				v.Reason = t
			}
		}
	}
	d := in.Status.Dependencies
	if d == nil {
		return v
	}
	for _, b := range d.Blockers {
		if sameRepo(b.Repository, in.Spec.Issue.Repository) && b.Number > 0 {
			v.Refs = append(v.Refs, fmt.Sprintf("#%d", b.Number))
		} else {
			v.Others++
		}
	}
	v.Others += int(d.More) + int(d.Unreadable)
	return v
}
