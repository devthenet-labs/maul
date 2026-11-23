// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package projectsettings

import (
	"reflect"
	"testing"

	"github.com/devthenet-labs/maul/api/v1alpha1"
)

func TestConfirmations(t *testing.T) {
	gated := func(p *v1alpha1.Project) { p.Spec.Verification.Handoff = v1alpha1.HandoffGated }
	for _, tc := range []struct {
		name    string
		project func(*v1alpha1.Project)
		edit    func(*ProjectSettings)
		want    []ProjectSettingsConfirmation
	}{
		{name: "an approver added is named",
			edit: func(d *ProjectSettings) { d.Approvers = append(d.Approvers, "carol") },
			want: []ProjectSettingsConfirmation{{Key: "approver-add:carol", Kind: ConfirmApproverAdd,
				Field: FieldApprovers, Value: "carol"}}},
		{name: "an approver removed is named",
			edit: func(d *ProjectSettings) { d.Approvers = []string{"alice"} },
			want: []ProjectSettingsConfirmation{{Key: "approver-remove:bob", Kind: ConfirmApproverRemove,
				Field: FieldApprovers, Value: "bob"}}},
		{name: "verification off",
			edit: func(d *ProjectSettings) { d.Verification.Enabled = false },
			want: []ProjectSettingsConfirmation{{Key: "evaluator-off", Kind: ConfirmEvaluatorOff,
				Field: FieldVerificationEnabled}}},
		{name: "verification on needs none", project: func(p *v1alpha1.Project) { p.Spec.Verification = nil },
			edit: func(d *ProjectSettings) { d.Verification.Enabled = true }},
		{name: "hand-off to early",
			edit: func(d *ProjectSettings) { d.Verification.Handoff = "early" },
			want: []ProjectSettingsConfirmation{{Key: "handoff:early", Kind: ConfirmHandoff, Field: FieldHandoff,
				Value: "early"}}},
		{name: "hand-off left to the install's default", project: gated,
			edit: func(d *ProjectSettings) { d.Verification.Handoff = "" },
			want: []ProjectSettingsConfirmation{{Key: "handoff:default", Kind: ConfirmHandoff, Field: FieldHandoff}}},
		{name: "hand-off to gated needs none", edit: func(d *ProjectSettings) { d.Verification.Handoff = "gated" }},
		{name: "a limit lowered under its floor, with where it lands",
			edit: func(d *ProjectSettings) { d.Stages.Evaluate.MaxTurns = 5 },
			want: []ProjectSettingsConfirmation{{Key: "below-floor:limits.evaluate.maxTurns=5",
				Kind: ConfirmBelowFloor, Field: "limits.evaluate.maxTurns", Value: "5"}}},
		{name: "a limit lowered to its floor needs none",
			edit: func(d *ProjectSettings) { d.Stages.Evaluate.MaxTurns = 40 }},
		{name: "a limit already under its floor and raised needs none",
			project: func(p *v1alpha1.Project) { p.Spec.Limits.MaxVerifications = i32(1) },
			edit:    func(d *ProjectSettings) { d.Limits.MaxVerifications = 2 }},
		{name: "the checks emptied",
			edit: func(d *ProjectSettings) { d.Checks.Fix = nil },
			want: []ProjectSettingsConfirmation{{Key: "checks-empty", Kind: ConfirmChecksEmpty, Field: FieldChecksFix}}},
		{name: "a check dropped needs none", edit: func(d *ProjectSettings) { d.Checks.Fix = []string{"test"} }},
		{name: "an ordinary change needs none", edit: func(d *ProjectSettings) { d.Limits.MaxRevisions = i32(5) }},
		{name: "several at once, in the fields' order",
			edit: func(d *ProjectSettings) {
				d.Checks.Fix = nil
				d.Verification.Enabled = false
				d.Verification.HandoffMaxSeconds = 900
			},
			want: []ProjectSettingsConfirmation{
				{Key: "evaluator-off", Kind: ConfirmEvaluatorOff, Field: FieldVerificationEnabled},
				{Key: "below-floor:verification.handoffMax=900", Kind: ConfirmBelowFloor, Field: FieldHandoffMax,
					Value: "900"},
				{Key: "checks-empty", Kind: ConfirmChecksEmpty, Field: FieldChecksFix}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var muts []func(*v1alpha1.Project)
			if tc.project != nil {
				muts = append(muts, tc.project)
			}
			p := baseProject(muts...)
			out, _ := save(t, p, tc.edit)
			got := Confirmations(p, out, openPolicy())
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("confirmations =\n%+v\nwant\n%+v", got, tc.want)
			}
		})
	}
}

// A confirmation covers the change it was given for and no other: the key
// carries the change's value.
func TestAConfirmationCoversOnlyItsOwnChange(t *testing.T) {
	p := baseProject()
	carol, _ := save(t, p, func(d *ProjectSettings) { d.Approvers = append(d.Approvers, "carol") })
	dave, _ := save(t, p, func(d *ProjectSettings) { d.Approvers = append(d.Approvers, "dave") })
	five, _ := save(t, p, func(d *ProjectSettings) { d.Stages.Evaluate.MaxTurns = 5 })
	one, _ := save(t, p, func(d *ProjectSettings) { d.Stages.Evaluate.MaxTurns = 1 })
	pol := openPolicy()
	given := []string{"approver-add:carol", "below-floor:limits.evaluate.maxTurns=5", "stray"}
	for name, tc := range map[string]struct {
		out       *v1alpha1.Project
		confirmed bool
	}{
		"the login confirmed":   {carol, true},
		"another login":         {dave, false},
		"the value confirmed":   {five, true},
		"a lower value than it": {one, false},
	} {
		need := Confirmations(p, tc.out, pol)
		if len(need) != 1 {
			t.Fatalf("%s: confirmations %+v", name, need)
		}
		left := Unconfirmed(need, given)
		if tc.confirmed && len(left) != 0 {
			t.Errorf("%s: still unconfirmed %+v", name, left)
		}
		if !tc.confirmed && !reflect.DeepEqual(left, need) {
			t.Errorf("%s: unconfirmed %+v, want %+v", name, left, need)
		}
	}
	if left := Unconfirmed(nil, given); len(left) != 0 {
		t.Errorf("nothing to confirm leaves %+v", left)
	}
}

// A floor the operator set to zero asks for nothing, and a floor is asked
// about whether or not the switch is on: Check decides whether the change
// may be made at all.
func TestConfirmationsFollowTheFloors(t *testing.T) {
	p := baseProject()
	out, _ := save(t, p, func(d *ProjectSettings) { d.Verification.MaxToolCalls = 60 })
	pol := DefaultPolicy()
	if got := Confirmations(p, out, pol); len(got) != 1 || got[0].Key != "below-floor:verification.maxToolCalls=60" {
		t.Errorf("confirmations = %+v", got)
	}
	pol.Floors.MaxToolCalls = 0
	if got := Confirmations(p, out, pol); len(got) != 0 {
		t.Errorf("with no floor: confirmations = %+v", got)
	}
}
