// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package projectsettings

import (
	"slices"

	"github.com/devthenet-labs/maul/api/v1alpha1"
)

// Why a change may not be made: fixed keys the page words.
const (
	// RefusedLocked: the operator locked the field's group.
	RefusedLocked = "locked"
	// RefusedOutOfRange: outside the Project schema's own range (Limit).
	RefusedOutOfRange = "out-of-range"
	// RefusedInvalid: not a value the field can hold at all (Value).
	RefusedInvalid = "invalid"
	// RefusedApproversNotAllowed: the operator did not let the page change
	// the approver list.
	RefusedApproversNotAllowed = "approvers-not-allowed"
	// RefusedApproversInvalid: the result is no approver list (Value is
	// the login at fault).
	RefusedApproversInvalid = "approvers-invalid"
	// RefusedApproversOneAtATime: more than one login added or removed.
	RefusedApproversOneAtATime = "approvers-one-at-a-time"
	// RefusedApproversSelfRemoval: the editor would no longer be on it.
	RefusedApproversSelfRemoval = "approvers-self-removal"
	// RefusedEvaluatorOffNotAllowed: verification turned off without the
	// operator's switch.
	RefusedEvaluatorOffNotAllowed = "evaluator-off-not-allowed"
	// RefusedHandoffNotAllowed: a hand-off that may come earlier, without
	// the operator's switch.
	RefusedHandoffNotAllowed = "handoff-early-not-allowed"
	// RefusedBelowFloor: under the operator's floor (Limit), and lower
	// than it was.
	RefusedBelowFloor = "below-floor"
	// RefusedAboveCeiling: above the operator's page ceiling (Limit), and
	// higher than it was.
	RefusedAboveCeiling = "above-ceiling"
	// RefusedAboveControllerCeiling: above what intent-controller grants
	// (Limit), and higher than it was.
	RefusedAboveControllerCeiling = "above-controller-ceiling"
	// RefusedAdmitsUnpublished: intent-controller has not published what
	// it admits, so the change cannot be judged.
	RefusedAdmitsUnpublished = "admits-unpublished"
	// RefusedModelNotAdmitted: a model intent-controller does not admit
	// for the Project (Value).
	RefusedModelNotAdmitted = "model-not-admitted"
	// RefusedClassNotAdmitted: a resource class the operator does not
	// define (Value).
	RefusedClassNotAdmitted = "class-not-admitted"
	// RefusedChecksEmptyNotAllowed: checks.fix emptied without the
	// operator's switch.
	RefusedChecksEmptyNotAllowed = "checks-empty-not-allowed"
	// RefusedRunBudgetOverHourly: a run's cost budget would pass half the
	// broker's hourly limit (Limit), which makes the Project not Ready.
	RefusedRunBudgetOverHourly = "run-budget-over-hourly"
	// RefusedDelegationNotAdmitted: a delegation switch turned on that
	// intent-controller does not allow for the Project (status.admits).
	RefusedDelegationNotAdmitted = "delegation-not-admitted"
	// RefusedWorkflowsNeedSubagents: workflows left on without subagents,
	// which the Project schema refuses.
	RefusedWorkflowsNeedSubagents = "workflows-need-subagents"
	// RefusedRunBudgetUnderReserve: an evaluator stage's cost budget would
	// be under what the broker reserves for its model's requests (Limit,
	// Value the model), which makes the Project not Ready.
	RefusedRunBudgetUnderReserve = "run-budget-under-reserve"
)

// Check judges the changes between old (the live Project, with the status
// intent-controller published) and updated (what Apply made of a copy)
// against the operator's Policy and what the controller admits. It returns
// the changes that may not be made, each with why; none means the save may
// be written. editor is the approver login the gate resolved the session
// to.
//
// Only what changed is judged, and a bound is passed only by moving the
// wrong way: a value an operator set outside the policy may be kept, or
// moved back toward it, and never locks the rest of the form. A cleared
// field is judged by the value it lands on. The rules are the admission
// policy's own (the chart renders both from one Policy), so a refusal here
// is a kindness; the policy is the containment.
func Check(old, updated *v1alpha1.Project, pol Policy, editor string) []ProjectSettingsRefusal {
	j := judge{old: &old.Spec, new: &updated.Spec, admits: old.Status.Admits, pol: pol, editor: editor}
	for _, c := range Changes(j.old, j.new) {
		j.changed = append(j.changed, c.Field)
		j.change(c)
	}
	j.delegation()
	j.budgets()
	return j.out
}

// judge is one Check.
type judge struct {
	old, new *v1alpha1.ProjectSpec
	admits   *v1alpha1.ProjectAdmits
	pol      Policy
	editor   string
	changed  []string
	out      []ProjectSettingsRefusal
}

// refuse records a refusal once.
func (j *judge) refuse(r ProjectSettingsRefusal) {
	for _, have := range j.out {
		if have.Field == r.Field && have.Item == r.Item && have.Reason == r.Reason {
			return
		}
	}
	j.out = append(j.out, r)
}

func (j *judge) change(c ProjectSettingsChange) {
	if j.pol.Locks(Group(c.Group)) {
		j.refuse(ProjectSettingsRefusal{Field: c.Field, Item: c.Item, Reason: RefusedLocked})
		return
	}
	if c.Field == FieldRepositoryClass {
		j.class(c)
		return
	}
	r, _ := rowByID(c.Field)
	was, is := r.read(j.old), r.read(j.new)
	if j.outOfRange(r.Field, is) {
		return
	}
	switch {
	case r.ID == FieldApprovers:
		j.approvers(c)
	case r.ID == FieldVerificationEnabled:
		if is.n == 0 && !j.pol.Allow.DisableEvaluator {
			j.refuse(ProjectSettingsRefusal{Field: r.ID, Reason: RefusedEvaluatorOffNotAllowed})
		}
	case r.ID == FieldHandoff:
		if handoffWeakened(was.s, is.s) && !j.pol.Allow.DisableEvaluator {
			j.refuse(ProjectSettingsRefusal{Field: r.ID, Reason: RefusedHandoffNotAllowed})
		}
	case r.ID == FieldChecksFix:
		if len(is.list) == 0 && !j.pol.Allow.EmptyChecks {
			j.refuse(ProjectSettingsRefusal{Field: r.ID, Reason: RefusedChecksEmptyNotAllowed})
		}
	case r.Group == GroupDelegation:
		j.switchedOn(r.ID, is)
	case r.Kind == KindModel:
		j.admitted(r.ID, "", is.s, RefusedModelNotAdmitted, func(a *v1alpha1.ProjectAdmits) []string { return a.Models })
	case r.Kind.numeric():
		j.number(r, was, is)
	}
}

// outOfRange refuses a value the schema itself would refuse.
func (j *judge) outOfRange(f Field, v value) bool {
	n, ranged := v.n, f.Kind.numeric() && !v.unset(f)
	if f.Kind.list() {
		n, ranged = int64(len(v.list)), true
	}
	switch {
	case f.Kind == KindHandoff && v.s != "" && !slices.Contains(handoffModes, v.s):
		j.refuse(ProjectSettingsRefusal{Field: f.ID, Reason: RefusedInvalid, Value: v.s})
	case ranged && n < f.Min:
		j.refuse(ProjectSettingsRefusal{Field: f.ID, Reason: RefusedOutOfRange, Limit: f.Min})
	case ranged && n > f.Max:
		j.refuse(ProjectSettingsRefusal{Field: f.ID, Reason: RefusedOutOfRange, Limit: f.Max})
	default:
		return false
	}
	return true
}

// approvers judges a changed approver list.
func (j *judge) approvers(c ProjectSettingsChange) {
	if !j.pol.Allow.Approvers {
		j.refuse(ProjectSettingsRefusal{Field: c.Field, Reason: RefusedApproversNotAllowed})
		return
	}
	if err := CheckApprovers(j.new.Approvers.Logins); err != nil {
		j.refuse(ProjectSettingsRefusal{Field: c.Field, Reason: RefusedApproversInvalid, Value: err.Login})
	}
	if len(c.Added)+len(c.Removed) > 1 {
		j.refuse(ProjectSettingsRefusal{Field: c.Field, Reason: RefusedApproversOneAtATime})
	}
	if !j.new.Approvers.Has(j.editor) {
		j.refuse(ProjectSettingsRefusal{Field: c.Field, Reason: RefusedApproversSelfRemoval, Value: j.editor})
	}
}

// class judges a repository's changed resource class.
func (j *judge) class(c ProjectSettingsChange) {
	j.admitted(c.Field, c.Item, c.To, RefusedClassNotAdmitted, func(a *v1alpha1.ProjectAdmits) []string {
		names := make([]string, 0, len(a.ResourceClasses))
		for _, class := range a.ResourceClasses {
			names = append(names, class.Name)
		}
		return names
	})
}

// switchedOn judges a changed delegation switch. Only turning one on is
// judged, against what intent-controller allows: turning it off is always
// a move the right way, so an opt-in the controller no longer honours can
// still be withdrawn.
func (j *judge) switchedOn(field string, is value) {
	switch {
	case is.n == 0:
	case j.admits == nil:
		j.refuse(ProjectSettingsRefusal{Field: field, Reason: RefusedAdmitsUnpublished})
	case !delegationAdmitted(j.admits, field):
		j.refuse(ProjectSettingsRefusal{Field: field, Reason: RefusedDelegationNotAdmitted})
	}
}

// delegationAdmitted reports whether admits lets the delegation switch
// field take effect.
func delegationAdmitted(admits *v1alpha1.ProjectAdmits, field string) bool {
	if field == FieldWorkflows {
		return admits.Workflows
	}
	return admits.Subagents
}

// delegation refuses a save that leaves workflows on without subagents, as
// the schema would: Apply never clears the one the person did not post.
func (j *judge) delegation() {
	moved := slices.Contains(j.changed, FieldSubagents) || slices.Contains(j.changed, FieldWorkflows)
	if moved && j.new.Delegation != nil && j.new.Delegation.Workflows && !j.new.Delegation.Subagents {
		j.refuse(ProjectSettingsRefusal{Field: FieldWorkflows, Reason: RefusedWorkflowsNeedSubagents})
	}
}

// admitted judges a new choice against what intent-controller publishes.
// No choice at all (the install's model, the default size) is always
// admitted; any other needs the published list, and to be on it.
func (j *judge) admitted(field, item, choice, reason string, list func(*v1alpha1.ProjectAdmits) []string) {
	switch {
	case choice == "":
	case j.admits == nil:
		j.refuse(ProjectSettingsRefusal{Field: field, Item: item, Reason: RefusedAdmitsUnpublished})
	case !slices.Contains(list(j.admits), choice):
		j.refuse(ProjectSettingsRefusal{Field: field, Item: item, Reason: reason, Value: choice})
	}
}

// number judges a changed limit by where it lands.
func (j *judge) number(r row, was, is value) {
	now, _ := r.landing(is)
	before, wasOpen := r.landing(was)
	if c, ok := j.pol.Ceiling(r.ID); ok && now > c && now > before {
		j.refuse(ProjectSettingsRefusal{Field: r.ID, Reason: RefusedAboveCeiling, Limit: c})
	}
	if _, floor, under := belowFloor(j.pol, r.ID, j.old, j.new); under && !j.pol.Allow.DisableEvaluator {
		j.refuse(ProjectSettingsRefusal{Field: r.ID, Reason: RefusedBelowFloor, Limit: floor})
	}
	// The controller's own ceiling. It clamps whatever the Project says,
	// so an unset limit takes effect at most at the ceiling and can never
	// pass it: only a stated value can, and only a raise is judged, one
	// stated above where the limit landed before.
	if !clamped(r.ID) || is.unset(r.Field) || !wasOpen && now <= before {
		return
	}
	switch c := controllerCeiling(j.admits, r.ID); {
	case j.admits == nil:
		j.refuse(ProjectSettingsRefusal{Field: r.ID, Reason: RefusedAdmitsUnpublished})
	case c > 0 && now > c:
		j.refuse(ProjectSettingsRefusal{Field: r.ID, Reason: RefusedAboveControllerCeiling, Limit: c})
	}
}

// RunBudget is the cost budget of one run of a stage (a StageNames entry)
// in micro-USD, as intent-controller reckons it: the stage's own
// maxCostMicroUSD, or the ceiling it is spent against when that is unset,
// and never more than that ceiling. The ceiling is the per-intent cost for
// plan, build and revise, and for the evaluator's two stages the evaluation
// cost, itself held to the controller's own maximum where it publishes one.
// Zero for a stage there is none of.
//
// It is the controller's Settings.stageBudget written from published data;
// a test in the controller's package holds the two together.
func RunBudget(spec *v1alpha1.ProjectSpec, admits *v1alpha1.ProjectAdmits, stage string) int64 {
	for _, st := range stages {
		if st.name == stage {
			return st.budget(spec, admits)
		}
	}
	return 0
}

func (st stage) budget(spec *v1alpha1.ProjectSpec, admits *v1alpha1.ProjectAdmits) int64 {
	ceiling := orDefault(max(spec.Limits.MaxCostMicroUSD, 0), v1alpha1.DefaultMaxCostMicroUSD)
	if st.role {
		ceiling = spec.Limits.MaxEvaluateCostLimit()
		if admits != nil && admits.MaxEvaluateCostMicroUSD > 0 && admits.MaxEvaluateCostMicroUSD < ceiling {
			ceiling = admits.MaxEvaluateCostMicroUSD
		}
	}
	if b := st.limits(&spec.Limits).MaxCostMicroUSD; b > 0 && b < ceiling {
		return b
	}
	return ceiling
}

// counted reports whether the controller judges the stage's run budget for
// this spec: always for plan, build and revise, and for the evaluator's
// stages while the evaluator is on and the Project asks to be verified.
// (The controller has further reasons not to verify a Project, which are
// not published; counting a stage it would not is the cautious side.)
func (st stage) counted(spec *v1alpha1.ProjectSpec, admits *v1alpha1.ProjectAdmits) bool {
	return !st.role || admits.Evaluator && spec.VerificationEnabled()
}

// model is the model the evaluator stage runs for this spec: the Project's
// choice, or the controller's own.
func (st stage) model(spec *v1alpha1.ProjectSpec, admits *v1alpha1.ProjectAdmits) string {
	chosen, def := "", admits.DefaultModels.Evaluate
	if spec.Models != nil {
		chosen = spec.Models.Evaluate
	}
	if st.name == stageCheckReview {
		def = admits.DefaultModels.CheckReview
		if spec.Models != nil {
			chosen = spec.Models.CheckReview
		}
	}
	if chosen != "" {
		return chosen
	}
	return def
}

// reserve is the least budget a run of the evaluator stage may have on a
// model: zero when the controller publishes none for it.
func (st stage) reserve(admits *v1alpha1.ProjectAdmits, model string) int64 {
	for _, f := range admits.RunBudgetFloors {
		if f.Model != model {
			continue
		}
		if st.name == stageCheckReview {
			return f.CheckReviewMicroUSD
		}
		return f.EvaluateMicroUSD
	}
	return 0
}

const stageCheckReview = "checkReview"

// budgets judges what the save does to each stage's run budget. Under the
// broker's cost unit intent-controller takes a whole Project not Ready when
// a run's budget is over half the hourly limit or, for an evaluator stage,
// under what its model's requests reserve; every open intent then waits.
// A budget moves with more than its own field (a stage with none follows
// the per-intent cost; the reserve follows the model), so it is judged
// here, after the fields, and refused on the field that moved it.
func (j *judge) budgets() {
	for _, st := range stages {
		was, is := st.budget(j.old, j.admits), st.budget(j.new, j.admits)
		field := j.blame(st)
		if j.admits == nil {
			// Whether the broker counts cost is not known: no raise.
			if is > was {
				j.refuse(ProjectSettingsRefusal{Field: field, Reason: RefusedAdmitsUnpublished})
			}
			continue
		}
		if !j.admits.CostBudgets {
			continue
		}
		if limit := j.admits.MaxRunCostMicroUSD; limit > 0 {
			over := st.counted(j.new, j.admits) && is > limit
			wasOver := st.counted(j.old, j.admits) && was > limit
			if worse := !wasOver || is > was; over && worse {
				j.refuse(ProjectSettingsRefusal{Field: field, Reason: RefusedRunBudgetOverHourly, Limit: limit})
			}
		}
		if !st.role {
			continue
		}
		model := st.model(j.new, j.admits)
		least, wasLeast := st.reserve(j.admits, model), st.reserve(j.admits, st.model(j.old, j.admits))
		short := j.new.VerificationEnabled() && is < least
		wasShort := j.old.VerificationEnabled() && was < wasLeast
		if worse := !wasShort || is < was || least > wasLeast; short && worse {
			j.refuse(ProjectSettingsRefusal{Field: field, Reason: RefusedRunBudgetUnderReserve, Limit: least,
				Value: model})
		}
	}
}

// blame is the changed field a stage's run budget moved with: its own cost
// budget, else the ceiling it follows, else its model, else verification
// being turned on.
func (j *judge) blame(st stage) string {
	own := StageField(st.name, leafCost)
	follows := []string{own, FieldMaxCost}
	if st.role {
		follows = []string{own, FieldMaxEvaluateCost, "models." + st.name, FieldVerificationEnabled}
	}
	for _, f := range follows {
		if slices.Contains(j.changed, f) {
			return f
		}
	}
	return own
}
