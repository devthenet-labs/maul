// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package projectsettings

import (
	"slices"
	"strconv"

	"github.com/devthenet-labs/maul/api/v1alpha1"
)

// The kinds of confirmation.
const (
	// ConfirmApproverAdd: a login gains an approver's authority.
	ConfirmApproverAdd = "approver-add"
	// ConfirmApproverRemove: a login loses it.
	ConfirmApproverRemove = "approver-remove"
	// ConfirmEvaluatorOff: the Project's intents are no longer verified.
	ConfirmEvaluatorOff = "evaluator-off"
	// ConfirmHandoff: previews may be handed over before the evaluation.
	ConfirmHandoff = "handoff"
	// ConfirmBelowFloor: an evaluator limit goes under the operator's floor.
	ConfirmBelowFloor = "below-floor"
	// ConfirmChecksEmpty: no CI check is waited for or fixed any more.
	ConfirmChecksEmpty = "checks-empty"
)

// Confirmations are the weakening changes between old and updated that a
// person must confirm before they are saved: a change to who approves, and
// each way of disabling the evaluator or the CI checks. A confirmation is a
// guard against accidents, not against people; whether a change may be made
// at all is Check's to say, and the admission policy's.
//
// Each key carries the change's own value (the login, the limit's new
// value), so a confirmation given for one change never covers another.
func Confirmations(old, updated *v1alpha1.Project, pol Policy) []ProjectSettingsConfirmation {
	out := []ProjectSettingsConfirmation{}
	add := func(kind, field, key, val string) {
		out = append(out, ProjectSettingsConfirmation{Key: key, Kind: kind, Field: field, Value: val})
	}
	for _, c := range Changes(&old.Spec, &updated.Spec) {
		switch c.Field {
		case FieldApprovers:
			for _, l := range c.Added {
				add(ConfirmApproverAdd, c.Field, ConfirmApproverAdd+":"+l, l)
			}
			for _, l := range c.Removed {
				add(ConfirmApproverRemove, c.Field, ConfirmApproverRemove+":"+l, l)
			}
		case FieldVerificationEnabled:
			if !updated.Spec.VerificationEnabled() {
				add(ConfirmEvaluatorOff, c.Field, ConfirmEvaluatorOff, "")
			}
		case FieldHandoff:
			if handoffWeakened(c.From, c.To) {
				add(ConfirmHandoff, c.Field, ConfirmHandoff+":"+orWord(c.To, "default"), c.To)
			}
		case FieldChecksFix:
			if len(updated.Spec.Checks.Fix) == 0 {
				add(ConfirmChecksEmpty, c.Field, ConfirmChecksEmpty, "")
			}
		default:
			if n, _, under := belowFloor(pol, c.Field, &old.Spec, &updated.Spec); under {
				val := strconv.FormatInt(n, 10)
				add(ConfirmBelowFloor, c.Field, ConfirmBelowFloor+":"+c.Field+"="+val, val)
			}
		}
	}
	return out
}

func orWord(s, word string) string {
	if s == "" {
		return word
	}
	return s
}

// Unconfirmed are the confirmations of need whose key is not among given.
func Unconfirmed(need []ProjectSettingsConfirmation, given []string) []ProjectSettingsConfirmation {
	out := []ProjectSettingsConfirmation{}
	for _, c := range need {
		if !slices.Contains(given, c.Key) {
			out = append(out, c)
		}
	}
	return out
}

// handoffModes are the hand-off modes a Project may state.
var handoffModes = []string{string(v1alpha1.HandoffGated), string(v1alpha1.HandoffEarly)}

// handoffWeakened reports a changed hand-off that may hand previews over
// earlier than before. The install's default mode is not published, so an
// unset mode is taken for neither: leaving gated, or going to early from
// anything but early, may weaken; arriving at gated, or leaving early,
// cannot.
func handoffWeakened(from, to string) bool {
	return to != string(v1alpha1.HandoffGated) && from != string(v1alpha1.HandoffEarly)
}

// belowFloor reports a field whose value went under the operator's floor
// and lower than it was: where it lands, the floor, and whether it did. A
// value already under the floor may be kept or raised toward it.
func belowFloor(pol Policy, field string, old, updated *v1alpha1.ProjectSpec) (landing, floor int64, under bool) {
	floor, ok := pol.Floor(field)
	r, known := rowByID(field)
	if !ok || !known {
		return 0, 0, false
	}
	is, open := r.landing(r.read(updated))
	was, wasOpen := r.landing(r.read(old))
	return is, floor, !open && is < floor && (wasOpen || is < was)
}
