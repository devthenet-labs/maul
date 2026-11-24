// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package projectsettings

import (
	"math"
	"math/rand"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/quick"
	"time"

	"github.com/devthenet-labs/maul/api/v1alpha1"
)

// checkCase is one save judged: the Project and policy it is made under,
// the edit, who makes it (alice unless said), and the refusals expected
// (none: the save may be made).
type checkCase struct {
	name    string
	project func(*v1alpha1.Project)
	policy  func(*Policy)
	edit    func(*ProjectSettings)
	editor  string
	want    []ProjectSettingsRefusal
}

func runChecks(t *testing.T, cases []checkCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var muts []func(*v1alpha1.Project)
			if tc.project != nil {
				muts = append(muts, tc.project)
			}
			p := baseProject(muts...)
			pol := DefaultPolicy()
			if tc.policy != nil {
				tc.policy(&pol)
			}
			editor := tc.editor
			if editor == "" {
				editor = "alice"
			}
			out, changes := save(t, p, tc.edit)
			if len(changes) == 0 {
				t.Fatal("the edit changes nothing: the case tests nothing")
			}
			got := Check(p, out, pol, editor)
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("refusals =\n%+v\nwant\n%+v", got, tc.want)
			}
		})
	}
}

func refused(field, reason string) ProjectSettingsRefusal {
	return ProjectSettingsRefusal{Field: field, Reason: reason}
}

func bounded(field, reason string, limit int64) ProjectSettingsRefusal {
	return ProjectSettingsRefusal{Field: field, Reason: reason, Limit: limit}
}

func TestCheckLocks(t *testing.T) {
	// One edit per group: with the group locked it is refused, and with
	// every other group locked it is not.
	edits := map[Group]struct {
		field string
		edit  func(*ProjectSettings)
	}{
		GroupApprovers:     {FieldApprovers, func(d *ProjectSettings) { d.Approvers = append(d.Approvers, "carol") }},
		GroupEvaluator:     {FieldMaxToolCalls, func(d *ProjectSettings) { d.Verification.MaxToolCalls = 200 }},
		GroupHandoff:       {FieldHandoffMax, func(d *ProjectSettings) { d.Verification.HandoffMaxSeconds = 3000 }},
		GroupModels:        {"models.build", func(d *ProjectSettings) { d.Models.Build = "anthropic/claude-b" }},
		GroupCost:          {FieldMaxCost, func(d *ProjectSettings) { d.Limits.MaxCostMicroUSD = 5_000_000 }},
		GroupRounds:        {FieldMaxRevisions, func(d *ProjectSettings) { d.Limits.MaxRevisions = i32(1) }},
		GroupActiveIntents: {FieldMaxActiveIntents, func(d *ProjectSettings) { d.Limits.MaxActiveIntents = 1 }},
		GroupStages:        {"limits.plan.maxTurns", func(d *ProjectSettings) { d.Stages.Plan.MaxTurns = 10 }},
		GroupDelegation:    {FieldSubagents, func(d *ProjectSettings) { d.Delegation.Subagents = true }},
		GroupChecks:        {FieldChecksRerunFailed, func(d *ProjectSettings) { d.Checks.RerunFailed = true }},
		GroupResources: {FieldRepositoryClass, func(d *ProjectSettings) {
			d.Repositories[1].AgentResourceClass = "large"
		}},
	}
	if len(edits) != len(Groups()) {
		t.Fatalf("%d groups have an edit here, of %d", len(edits), len(Groups()))
	}
	for _, g := range Groups() {
		e := edits[g]
		p := baseProject()
		out, _ := save(t, p, e.edit)
		locked, others := openPolicy(), openPolicy()
		locked.Locked = []Group{g}
		for _, o := range Groups() {
			if o != g {
				others.Locked = append(others.Locked, o)
			}
		}
		got := Check(p, out, locked, "alice")
		if len(got) != 1 || got[0].Field != e.field || got[0].Reason != RefusedLocked {
			t.Errorf("%s locked: refusals %+v, want one %s on %s", g, got, RefusedLocked, e.field)
		}
		if got := Check(p, out, others, "alice"); len(got) != 0 {
			t.Errorf("%s with every other group locked: refusals %+v, want none", g, got)
		}
	}
}

func TestCheckSwitches(t *testing.T) {
	allow := func(set func(*ProjectSettingsAllow)) func(*Policy) {
		return func(p *Policy) { set(&p.Allow) }
	}
	approvers := allow(func(a *ProjectSettingsAllow) { a.Approvers = true })
	evaluator := allow(func(a *ProjectSettingsAllow) { a.DisableEvaluator = true })
	gated := func(p *v1alpha1.Project) { p.Spec.Verification.Handoff = v1alpha1.HandoffGated }
	early := func(p *v1alpha1.Project) { p.Spec.Verification.Handoff = v1alpha1.HandoffEarly }
	runChecks(t, []checkCase{
		{name: "approvers: off by default",
			edit: func(d *ProjectSettings) { d.Approvers = append(d.Approvers, "carol") },
			want: []ProjectSettingsRefusal{refused(FieldApprovers, RefusedApproversNotAllowed)}},
		{name: "approvers: one added", policy: approvers,
			edit: func(d *ProjectSettings) { d.Approvers = append(d.Approvers, "carol") }},
		{name: "approvers: one removed", policy: approvers,
			edit: func(d *ProjectSettings) { d.Approvers = []string{"alice"} }},
		{name: "approvers: a swap is two changes", policy: approvers,
			edit: func(d *ProjectSettings) { d.Approvers = []string{"alice", "carol"} },
			want: []ProjectSettingsRefusal{refused(FieldApprovers, RefusedApproversOneAtATime)}},
		{name: "approvers: two added", policy: approvers,
			edit: func(d *ProjectSettings) { d.Approvers = append(d.Approvers, "carol", "dave") },
			want: []ProjectSettingsRefusal{refused(FieldApprovers, RefusedApproversOneAtATime)}},
		{name: "approvers: the editor removes themselves", policy: approvers,
			edit: func(d *ProjectSettings) { d.Approvers = []string{"bob"} },
			want: []ProjectSettingsRefusal{{Field: FieldApprovers, Reason: RefusedApproversSelfRemoval,
				Value: "alice"}}},
		{name: "approvers: the editor is matched as the list spells them", policy: approvers, editor: "ALICE",
			edit: func(d *ProjectSettings) { d.Approvers = []string{"bob"} },
			want: []ProjectSettingsRefusal{{Field: FieldApprovers, Reason: RefusedApproversSelfRemoval,
				Value: "ALICE"}}},
		{name: "approvers: a login differing only in case", policy: approvers,
			edit: func(d *ProjectSettings) { d.Approvers = append(d.Approvers, "Bob") },
			want: []ProjectSettingsRefusal{{Field: FieldApprovers, Reason: RefusedApproversInvalid,
				Value: "Bob"}}},
		{name: "approvers: a Bot is no login", policy: approvers,
			edit: func(d *ProjectSettings) { d.Approvers = append(d.Approvers, "maul[bot]") },
			want: []ProjectSettingsRefusal{{Field: FieldApprovers, Reason: RefusedApproversInvalid,
				Value: "maul[bot]"}}},
		{name: "approvers: the list emptied", policy: approvers, project: logins("alice"),
			edit: func(d *ProjectSettings) { d.Approvers = nil },
			want: []ProjectSettingsRefusal{
				{Field: FieldApprovers, Reason: RefusedOutOfRange, Limit: 1}}},

		{name: "evaluator: off is off by default",
			edit: func(d *ProjectSettings) { d.Verification.Enabled = false },
			want: []ProjectSettingsRefusal{refused(FieldVerificationEnabled, RefusedEvaluatorOffNotAllowed)}},
		{name: "evaluator: off where the operator allows it", policy: evaluator,
			edit: func(d *ProjectSettings) { d.Verification.Enabled = false }},
		{name: "evaluator: on needs no switch",
			project: func(p *v1alpha1.Project) { p.Spec.Verification = nil },
			edit:    func(d *ProjectSettings) { d.Verification.Enabled = true }},

		{name: "handoff: early is the evaluator's switch",
			edit: func(d *ProjectSettings) { d.Verification.Handoff = "early" },
			want: []ProjectSettingsRefusal{refused(FieldHandoff, RefusedHandoffNotAllowed)}},
		{name: "handoff: gated to early", project: gated,
			edit: func(d *ProjectSettings) { d.Verification.Handoff = "early" },
			want: []ProjectSettingsRefusal{refused(FieldHandoff, RefusedHandoffNotAllowed)}},
		{name: "handoff: clearing gated lands on an install default nobody published", project: gated,
			edit: func(d *ProjectSettings) { d.Verification.Handoff = "" },
			want: []ProjectSettingsRefusal{refused(FieldHandoff, RefusedHandoffNotAllowed)}},
		{name: "handoff: early where the operator allows it", policy: evaluator,
			edit: func(d *ProjectSettings) { d.Verification.Handoff = "early" }},
		{name: "handoff: to gated needs no switch",
			edit: func(d *ProjectSettings) { d.Verification.Handoff = "gated" }},
		{name: "handoff: early to gated", project: early,
			edit: func(d *ProjectSettings) { d.Verification.Handoff = "gated" }},
		{name: "handoff: clearing early can only strengthen it", project: early,
			edit: func(d *ProjectSettings) { d.Verification.Handoff = "" }},
		{name: "handoff: a mode that does not exist", policy: evaluator,
			edit: func(d *ProjectSettings) { d.Verification.Handoff = "never" },
			want: []ProjectSettingsRefusal{{Field: FieldHandoff, Reason: RefusedInvalid, Value: "never"}}},

		{name: "checks: emptying the list is off by default",
			edit: func(d *ProjectSettings) { d.Checks.Fix = nil },
			want: []ProjectSettingsRefusal{refused(FieldChecksFix, RefusedChecksEmptyNotAllowed)}},
		{name: "checks: emptying where the operator allows it",
			policy: allow(func(a *ProjectSettingsAllow) { a.EmptyChecks = true }),
			edit:   func(d *ProjectSettings) { d.Checks.Fix = []string{} }},
		{name: "checks: down to one", edit: func(d *ProjectSettings) { d.Checks.Fix = []string{"test"} }},
		{name: "checks: more of them", edit: func(d *ProjectSettings) { d.Checks.Fix = append(d.Checks.Fix, "e2e") }},
	})
}

// A delegation switch may be turned on only where status.admits allows
// it; keeping it on or turning it off always passes, the usual "moved the
// wrong way" rule. Workflows never stay on without subagents.
func TestCheckDelegation(t *testing.T) {
	notAdmitted := func(p *v1alpha1.Project) { p.Status.Admits.Subagents, p.Status.Admits.Workflows = false, false }
	noWorkflows := func(p *v1alpha1.Project) { p.Status.Admits.Workflows = false }
	unpublished := func(p *v1alpha1.Project) { p.Status.Admits = nil }
	on := func(p *v1alpha1.Project) { p.Spec.Delegation = &v1alpha1.ProjectDelegation{Subagents: true} }
	both := func(p *v1alpha1.Project) {
		p.Spec.Delegation = &v1alpha1.ProjectDelegation{Subagents: true, Workflows: true}
	}
	both0 := func(p *v1alpha1.Project) { both(p); notAdmitted(p) }
	subagentsOn := func(d *ProjectSettings) { d.Delegation.Subagents = true }
	runChecks(t, []checkCase{
		{name: "subagents on, allowed", edit: subagentsOn},
		{name: "subagents on, not allowed", project: notAdmitted, edit: subagentsOn,
			want: []ProjectSettingsRefusal{refused(FieldSubagents, RefusedDelegationNotAdmitted)}},
		{name: "subagents on, nothing published", project: unpublished, edit: subagentsOn,
			want: []ProjectSettingsRefusal{refused(FieldSubagents, RefusedAdmitsUnpublished)}},
		{name: "both on together, allowed",
			edit: func(d *ProjectSettings) { d.Delegation = ProjectSettingsDelegation{Subagents: true, Workflows: true} }},
		{name: "workflows on beside subagents, workflows not allowed", project: func(p *v1alpha1.Project) {
			on(p)
			noWorkflows(p)
		}, edit: func(d *ProjectSettings) { d.Delegation.Workflows = true },
			want: []ProjectSettingsRefusal{refused(FieldWorkflows, RefusedDelegationNotAdmitted)}},
		{name: "workflows on without subagents",
			edit: func(d *ProjectSettings) { d.Delegation.Workflows = true },
			want: []ProjectSettingsRefusal{refused(FieldWorkflows, RefusedWorkflowsNeedSubagents)}},
		{name: "subagents off with workflows left on", project: both,
			edit: func(d *ProjectSettings) { d.Delegation.Subagents = false },
			want: []ProjectSettingsRefusal{refused(FieldWorkflows, RefusedWorkflowsNeedSubagents)}},
		{name: "kept on while not allowed: another field changed", project: both0,
			edit: func(d *ProjectSettings) { d.Limits.MaxRevisions = i32(1) }},
		{name: "workflows off while not allowed", project: both0,
			edit: func(d *ProjectSettings) { d.Delegation.Workflows = false }},
		{name: "both off while not allowed", project: both0,
			edit: func(d *ProjectSettings) { d.Delegation = ProjectSettingsDelegation{} }},
		{name: "both off while nothing is published", project: func(p *v1alpha1.Project) { both(p); unpublished(p) },
			edit: func(d *ProjectSettings) { d.Delegation = ProjectSettingsDelegation{} }},
		{name: "locked", policy: func(p *Policy) { p.Locked = []Group{GroupDelegation} }, project: both,
			edit: func(d *ProjectSettings) { d.Delegation = ProjectSettingsDelegation{} },
			want: []ProjectSettingsRefusal{refused(FieldSubagents, RefusedLocked),
				refused(FieldWorkflows, RefusedLocked)}},
	})
}

func TestCheckCeilingsAndFloors(t *testing.T) {
	evaluator := func(p *Policy) { p.Allow.DisableEvaluator = true }
	const evaluateTurns, evaluateTimeout, evaluateTokens, evaluateCost = "limits.evaluate.maxTurns",
		"limits.evaluate.timeout", "limits.evaluate.tokenBudget", "limits.evaluate.maxCostMicroUSD"
	runChecks(t, []checkCase{
		// The operator's page ceilings: at most the ceiling, or at most the
		// old value.
		{name: "cost: up to the ceiling", edit: func(d *ProjectSettings) { d.Limits.MaxCostMicroUSD = 25_000_000 }},
		{name: "cost: past the ceiling",
			edit: func(d *ProjectSettings) { d.Limits.MaxCostMicroUSD = 25_000_001 },
			want: []ProjectSettingsRefusal{bounded(FieldMaxCost, RefusedAboveCeiling, 25_000_000)}},
		{name: "cost: an operator's higher value may be lowered, still above the ceiling",
			project: func(p *v1alpha1.Project) { p.Spec.Limits.MaxCostMicroUSD = 60_000_000 },
			edit:    func(d *ProjectSettings) { d.Limits.MaxCostMicroUSD = 40_000_000 }},
		{name: "cost: an operator's higher value may not be raised",
			project: func(p *v1alpha1.Project) { p.Spec.Limits.MaxCostMicroUSD = 60_000_000 },
			edit:    func(d *ProjectSettings) { d.Limits.MaxCostMicroUSD = 60_000_001 },
			want:    []ProjectSettingsRefusal{bounded(FieldMaxCost, RefusedAboveCeiling, 25_000_000)}},
		{name: "revisions: past the ceiling", edit: func(d *ProjectSettings) { d.Limits.MaxRevisions = i32(11) },
			want: []ProjectSettingsRefusal{bounded(FieldMaxRevisions, RefusedAboveCeiling, 10)}},
		{name: "check fixes: past the ceiling", edit: func(d *ProjectSettings) { d.Limits.MaxCheckFixes = i32(11) },
			want: []ProjectSettingsRefusal{bounded(FieldMaxCheckFixes, RefusedAboveCeiling, 10)}},
		{name: "verify fixes: past the ceiling", edit: func(d *ProjectSettings) { d.Limits.MaxVerifyFixes = i32(6) },
			want: []ProjectSettingsRefusal{bounded(FieldMaxVerifyFixes, RefusedAboveCeiling, 5)}},
		{name: "verifications: past the ceiling", edit: func(d *ProjectSettings) { d.Limits.MaxVerifications = 51 },
			want: []ProjectSettingsRefusal{bounded(FieldMaxVerifications, RefusedAboveCeiling, 50)}},
		{name: "active intents: past the ceiling", edit: func(d *ProjectSettings) { d.Limits.MaxActiveIntents = 6 },
			want: []ProjectSettingsRefusal{bounded(FieldMaxActiveIntents, RefusedAboveCeiling, 5)}},
		{name: "a zero ceiling still lets a value come down",
			policy: func(p *Policy) { p.Ceilings.MaxRevisions = 0 },
			edit:   func(d *ProjectSettings) { d.Limits.MaxRevisions = i32(1) }},
		{name: "a zero ceiling lets nothing go up",
			policy: func(p *Policy) { p.Ceilings.MaxRevisions = 0 },
			edit:   func(d *ProjectSettings) { d.Limits.MaxRevisions = i32(4) },
			want:   []ProjectSettingsRefusal{bounded(FieldMaxRevisions, RefusedAboveCeiling, 0)}},
		{name: "past the schema's own range",
			policy: func(p *Policy) { p.Ceilings.MaxRevisions = 20 },
			edit:   func(d *ProjectSettings) { d.Limits.MaxRevisions = i32(21) },
			want:   []ProjectSettingsRefusal{bounded(FieldMaxRevisions, RefusedOutOfRange, 20)}},
		{name: "under the schema's own range",
			edit: func(d *ProjectSettings) { d.Checks.TimeoutSeconds = 30 },
			want: []ProjectSettingsRefusal{bounded(FieldChecksTimeout, RefusedOutOfRange, 60)}},

		// A clear is judged by where the value lands.
		{name: "clear: revisions land on the default, past a lower ceiling",
			project: func(p *v1alpha1.Project) { p.Spec.Limits.MaxRevisions = i32(1) },
			policy:  func(p *Policy) { p.Ceilings.MaxRevisions = 2 },
			edit:    func(d *ProjectSettings) { d.Limits.MaxRevisions = nil },
			want:    []ProjectSettingsRefusal{bounded(FieldMaxRevisions, RefusedAboveCeiling, 2)}},
		{name: "clear: verify fixes land on the code's default, past a lower ceiling",
			project: func(p *v1alpha1.Project) { p.Spec.Limits.MaxVerifyFixes = i32(0) },
			policy:  func(p *Policy) { p.Ceilings.MaxVerifyFixes = 1 },
			edit:    func(d *ProjectSettings) { d.Limits.MaxVerifyFixes = nil },
			want:    []ProjectSettingsRefusal{bounded(FieldMaxVerifyFixes, RefusedAboveCeiling, 1)}},
		{name: "clear: active intents land on the default, past a lower ceiling",
			project: func(p *v1alpha1.Project) { p.Spec.Limits.MaxActiveIntents = 1 },
			policy:  func(p *Policy) { p.Ceilings.MaxActiveIntents = 1 },
			edit:    func(d *ProjectSettings) { d.Limits.MaxActiveIntents = 0 },
			want:    []ProjectSettingsRefusal{bounded(FieldMaxActiveIntents, RefusedAboveCeiling, 1)}},
		{name: "clear: verifications land on the default, above the floor",
			project: func(p *v1alpha1.Project) { p.Spec.Limits.MaxVerifications = i32(2) },
			edit:    func(d *ProjectSettings) { d.Limits.MaxVerifications = 0 }},
		{name: "clear: a pinned default cleared lands where it was",
			project: func(p *v1alpha1.Project) { p.Spec.Limits.MaxVerifyFixes = i32(2) },
			policy:  func(p *Policy) { p.Ceilings.MaxVerifyFixes = 0 },
			edit:    func(d *ProjectSettings) { d.Limits.MaxVerifyFixes = nil }},
		{name: "clear: evaluation cost unset is as much as the controller grants, never past its ceiling",
			project: func(p *v1alpha1.Project) {
				p.Spec.Limits.MaxEvaluateCostMicroUSD = i64(3_000_000)
				p.Status.Admits.MaxEvaluateCostMicroUSD = 5_000_000
			},
			edit: func(d *ProjectSettings) { d.Limits.MaxEvaluateCostMicroUSD = 0 }},
		{name: "clear: tool calls unset, though the code's default is above the controller's ceiling",
			project: func(p *v1alpha1.Project) { p.Spec.Verification.MaxToolCalls = i32(120) },
			edit:    func(d *ProjectSettings) { d.Verification.MaxToolCalls = 0 }},
		{name: "clear: an evaluate limit lands on the controller's ceiling, the most there is",
			project: func(p *v1alpha1.Project) { p.Spec.Limits.Evaluate.MaxTurns = 20 },
			edit:    func(d *ProjectSettings) { d.Stages.Evaluate.MaxTurns = 0 }},

		// The floors under the evaluator's limits: at least the floor, or
		// at least the old value, unless the operator allows the evaluator
		// to be disabled.
		{name: "floor: evaluation cost", edit: func(d *ProjectSettings) { d.Limits.MaxEvaluateCostMicroUSD = 1_999_999 },
			want: []ProjectSettingsRefusal{bounded(FieldMaxEvaluateCost, RefusedBelowFloor, 2_000_000)}},
		{name: "floor: evaluation cost at the floor",
			edit: func(d *ProjectSettings) { d.Limits.MaxEvaluateCostMicroUSD = 2_000_000 }},
		{name: "floor: evaluate turns", edit: func(d *ProjectSettings) { d.Stages.Evaluate.MaxTurns = 39 },
			want: []ProjectSettingsRefusal{bounded(evaluateTurns, RefusedBelowFloor, 40)}},
		{name: "floor: evaluate timeout", edit: func(d *ProjectSettings) { d.Stages.Evaluate.TimeoutSeconds = 599 },
			want: []ProjectSettingsRefusal{bounded(evaluateTimeout, RefusedBelowFloor, 600)}},
		{name: "floor: evaluate tokens", edit: func(d *ProjectSettings) { d.Stages.Evaluate.TokenBudget = 99_999 },
			want: []ProjectSettingsRefusal{bounded(evaluateTokens, RefusedBelowFloor, 100_000)}},
		{name: "floor: tool calls", edit: func(d *ProjectSettings) { d.Verification.MaxToolCalls = 99 },
			want: []ProjectSettingsRefusal{bounded(FieldMaxToolCalls, RefusedBelowFloor, 100)}},
		{name: "floor: verifications", edit: func(d *ProjectSettings) { d.Limits.MaxVerifications = 2 },
			want: []ProjectSettingsRefusal{bounded(FieldMaxVerifications, RefusedBelowFloor, 3)}},
		{name: "floor: hand-off wait", edit: func(d *ProjectSettings) { d.Verification.HandoffMaxSeconds = 1799 },
			want: []ProjectSettingsRefusal{bounded(FieldHandoffMax, RefusedBelowFloor, 1800)}},
		// The evaluate stage's cost has the evaluation cost's floor: its run
		// budget is the lesser of the two, so either could starve it.
		{name: "floor: evaluate stage cost", edit: func(d *ProjectSettings) { d.Stages.Evaluate.MaxCostMicroUSD = 1_999_999 },
			want: []ProjectSettingsRefusal{bounded(evaluateCost, RefusedBelowFloor, 2_000_000)}},
		{name: "floor: evaluate stage cost at the floor",
			edit: func(d *ProjectSettings) { d.Stages.Evaluate.MaxCostMicroUSD = 2_000_000 }},
		{name: "floor: evaluate stage cost cleared lands on the evaluation cost",
			project: func(p *v1alpha1.Project) { p.Spec.Limits.Evaluate.MaxCostMicroUSD = 3_000_000 },
			edit:    func(d *ProjectSettings) { d.Stages.Evaluate.MaxCostMicroUSD = 0 }},
		{name: "floor: evaluate stage cost under it, raised toward it",
			project: func(p *v1alpha1.Project) { p.Spec.Limits.Evaluate.MaxCostMicroUSD = 500_000 },
			edit:    func(d *ProjectSettings) { d.Stages.Evaluate.MaxCostMicroUSD = 1_000_000 }},
		{name: "floor: evaluate stage cost under it, lowered",
			project: func(p *v1alpha1.Project) { p.Spec.Limits.Evaluate.MaxCostMicroUSD = 500_000 },
			edit:    func(d *ProjectSettings) { d.Stages.Evaluate.MaxCostMicroUSD = 400_000 },
			want:    []ProjectSettingsRefusal{bounded(evaluateCost, RefusedBelowFloor, 2_000_000)}},
		{name: "floor: evaluate stage cost under it where the evaluator may be disabled", policy: evaluator,
			edit: func(d *ProjectSettings) { d.Stages.Evaluate.MaxCostMicroUSD = 1_000_000 }},
		{name: "floor: evaluate stage cost follows the evaluation cost's floor",
			policy: func(p *Policy) { p.Floors.MaxEvaluateCostMicroUSD = 0 },
			edit:   func(d *ProjectSettings) { d.Stages.Evaluate.MaxCostMicroUSD = 1_000_000 }},
		{name: "floor: a value already under it may be raised toward it",
			project: func(p *v1alpha1.Project) { p.Spec.Limits.Evaluate.MaxTurns = 10 },
			edit:    func(d *ProjectSettings) { d.Stages.Evaluate.MaxTurns = 20 }},
		{name: "floor: a value already under it may not be lowered",
			project: func(p *v1alpha1.Project) { p.Spec.Limits.Evaluate.MaxTurns = 10 },
			edit:    func(d *ProjectSettings) { d.Stages.Evaluate.MaxTurns = 9 },
			want:    []ProjectSettingsRefusal{bounded(evaluateTurns, RefusedBelowFloor, 40)}},
		{name: "floor: under it where the operator allows the evaluator to be disabled", policy: evaluator,
			edit: func(d *ProjectSettings) { d.Stages.Evaluate.MaxTurns = 5 }},
		{name: "floor: no floor set", policy: func(p *Policy) { p.Floors.MaxToolCalls = 0 },
			edit: func(d *ProjectSettings) { d.Verification.MaxToolCalls = 50 }},
		{name: "floor: another stage's turns have none", edit: func(d *ProjectSettings) { d.Stages.Plan.MaxTurns = 1 }},

		// The controller's own ceilings, as status.admits publishes them.
		{name: "controller: evaluation cost up to its ceiling",
			edit: func(d *ProjectSettings) { d.Limits.MaxEvaluateCostMicroUSD = 8_000_000 }},
		{name: "controller: evaluation cost past its ceiling",
			edit: func(d *ProjectSettings) { d.Limits.MaxEvaluateCostMicroUSD = 8_000_001 },
			want: []ProjectSettingsRefusal{bounded(FieldMaxEvaluateCost, RefusedAboveControllerCeiling, 8_000_000)}},
		{name: "controller: no evaluation cost ceiling published",
			project: func(p *v1alpha1.Project) { p.Status.Admits.MaxEvaluateCostMicroUSD = 0 },
			edit:    func(d *ProjectSettings) { d.Limits.MaxEvaluateCostMicroUSD = 900_000_000 }},
		{name: "controller: tool calls raised past its ceiling",
			project: func(p *v1alpha1.Project) { p.Spec.Verification.MaxToolCalls = i32(120) },
			edit:    func(d *ProjectSettings) { d.Verification.MaxToolCalls = 251 },
			want:    []ProjectSettingsRefusal{bounded(FieldMaxToolCalls, RefusedAboveControllerCeiling, 250)}},
		{name: "controller: tool calls raised up to its ceiling",
			project: func(p *v1alpha1.Project) { p.Spec.Verification.MaxToolCalls = i32(120) },
			edit:    func(d *ProjectSettings) { d.Verification.MaxToolCalls = 250 }},
		{name: "controller: unset tool calls are the code's default, so a value under that is a lowering",
			edit: func(d *ProjectSettings) { d.Verification.MaxToolCalls = 251 }},
		{name: "controller: stage turns at the ceiling", edit: func(d *ProjectSettings) { d.Stages.Build.MaxTurns = 150 }},
		{name: "controller: stage turns past the ceiling", edit: func(d *ProjectSettings) { d.Stages.Build.MaxTurns = 151 },
			want: []ProjectSettingsRefusal{bounded("limits.build.maxTurns", RefusedAboveControllerCeiling, 150)}},
		{name: "controller: stage tokens past the ceiling",
			edit: func(d *ProjectSettings) { d.Stages.Revise.TokenBudget = 400_001 },
			want: []ProjectSettingsRefusal{bounded("limits.revise.tokenBudget", RefusedAboveControllerCeiling, 400_000)}},
		{name: "controller: stage timeout past the ceiling",
			edit: func(d *ProjectSettings) { d.Stages.Plan.TimeoutSeconds = 1201 },
			want: []ProjectSettingsRefusal{bounded("limits.plan.timeout", RefusedAboveControllerCeiling, 1200)}},
		{name: "controller: a value already past the ceiling may come down",
			project: func(p *v1alpha1.Project) { p.Spec.Limits.Build.MaxTurns = 500 },
			edit:    func(d *ProjectSettings) { d.Stages.Build.MaxTurns = 300 }},
		{name: "controller: a stage it does not run has no ceiling to pass",
			project: func(p *v1alpha1.Project) { p.Status.Admits.Stages.Evaluate = v1alpha1.ProjectStageCeiling{} },
			edit:    func(d *ProjectSettings) { d.Stages.Evaluate.MaxTurns = 90 }},
	})
}

// With nothing published the page cannot know what the controller admits,
// so it changes no model, picks no class and raises nothing the controller
// bounds. Everything else, and every lowering, still works.
func TestCheckFailsClosedWithoutAdmits(t *testing.T) {
	unpublished := func(p *v1alpha1.Project) { p.Status.Admits = nil }
	withModel := func(p *v1alpha1.Project) {
		p.Status.Admits = nil
		p.Spec.Models = &v1alpha1.ProjectModels{Build: "anthropic/claude-b"}
		p.Spec.Limits.Build.MaxTurns = 100
		p.Spec.Limits.Build.MaxCostMicroUSD = 4_000_000
	}
	no := func(field string) []ProjectSettingsRefusal {
		return []ProjectSettingsRefusal{refused(field, RefusedAdmitsUnpublished)}
	}
	runChecks(t, []checkCase{
		{name: "a model", project: unpublished, edit: func(d *ProjectSettings) { d.Models.Plan = "anthropic/claude-a" },
			want: no("models.plan")},
		{name: "a model cleared runs the install's, which is admitted by definition", project: withModel,
			edit: func(d *ProjectSettings) { d.Models.Build = "" }},
		{name: "a class", project: unpublished,
			edit: func(d *ProjectSettings) { d.Repositories[1].AgentResourceClass = "large" },
			want: []ProjectSettingsRefusal{{Field: FieldRepositoryClass, Item: "lib", Reason: RefusedAdmitsUnpublished}}},
		{name: "a class cleared", project: unpublished,
			edit: func(d *ProjectSettings) { d.Repositories[0].AgentResourceClass = "" }},
		{name: "a stage limit set where there was none", project: unpublished,
			edit: func(d *ProjectSettings) { d.Stages.Plan.MaxTurns = 30 }, want: no("limits.plan.maxTurns")},
		{name: "a stage limit raised", project: withModel,
			edit: func(d *ProjectSettings) { d.Stages.Build.MaxTurns = 101 }, want: no("limits.build.maxTurns")},
		{name: "a stage limit lowered", project: withModel,
			edit: func(d *ProjectSettings) { d.Stages.Build.MaxTurns = 99 }},
		{name: "a stage limit cleared is the controller's own ceiling", project: withModel,
			edit: func(d *ProjectSettings) { d.Stages.Build.MaxTurns = 0 }},
		{name: "evaluation cost raised", project: unpublished,
			edit: func(d *ProjectSettings) { d.Limits.MaxEvaluateCostMicroUSD = 7_000_000 },
			want: no(FieldMaxEvaluateCost)},
		{name: "evaluation cost lowered", project: unpublished,
			edit: func(d *ProjectSettings) { d.Limits.MaxEvaluateCostMicroUSD = 5_000_000 }},
		{name: "tool calls lowered", project: unpublished,
			edit: func(d *ProjectSettings) { d.Verification.MaxToolCalls = 200 }},
		{name: "the per-intent cost raised: a run's budget may follow it", project: unpublished,
			edit: func(d *ProjectSettings) { d.Limits.MaxCostMicroUSD = 12_000_000 }, want: no(FieldMaxCost)},
		{name: "the per-intent cost lowered", project: unpublished,
			edit: func(d *ProjectSettings) { d.Limits.MaxCostMicroUSD = 8_000_000 }},
		{name: "a stage's cost budget cleared is a raise", project: withModel,
			edit: func(d *ProjectSettings) { d.Stages.Build.MaxCostMicroUSD = 0 }, want: no("limits.build.maxCostMicroUSD")},
		{name: "a stage's cost budget lowered", project: withModel,
			edit: func(d *ProjectSettings) { d.Stages.Build.MaxCostMicroUSD = 3_000_000 }},
		{name: "a round limit the controller does not bound", project: unpublished,
			edit: func(d *ProjectSettings) { d.Limits.MaxRevisions = i32(8) }},
		{name: "the evaluator turned on", project: func(p *v1alpha1.Project) {
			p.Status.Admits = nil
			p.Spec.Verification = nil
		}, edit: func(d *ProjectSettings) { d.Verification.Enabled = true }},
	})
}

func TestCheckModelsAndClasses(t *testing.T) {
	runChecks(t, []checkCase{
		{name: "an admitted model", edit: func(d *ProjectSettings) { d.Models.Evaluate = "anthropic/claude-a" }},
		{name: "a model the controller does not admit", edit: func(d *ProjectSettings) { d.Models.Plan = "openai/gpt-x" },
			want: []ProjectSettingsRefusal{{Field: "models.plan", Reason: RefusedModelNotAdmitted,
				Value: "openai/gpt-x"}}},
		{name: "an unadmitted model already there does not lock the others",
			project: func(p *v1alpha1.Project) { p.Spec.Models = &v1alpha1.ProjectModels{Plan: "openai/gpt-x"} },
			edit:    func(d *ProjectSettings) { d.Models.Build = "anthropic/claude-b" }},
		{name: "none admitted", project: func(p *v1alpha1.Project) { p.Status.Admits.Models = nil },
			edit: func(d *ProjectSettings) { d.Models.Plan = "anthropic/claude-a" },
			want: []ProjectSettingsRefusal{{Field: "models.plan", Reason: RefusedModelNotAdmitted,
				Value: "anthropic/claude-a"}}},
		{name: "a class the operator defines",
			edit: func(d *ProjectSettings) { d.Repositories[0].AgentResourceClass = "large" }},
		{name: "a class nobody defines",
			edit: func(d *ProjectSettings) { d.Repositories[1].AgentResourceClass = "huge" },
			want: []ProjectSettingsRefusal{{Field: FieldRepositoryClass, Item: "lib", Reason: RefusedClassNotAdmitted,
				Value: "huge"}}},
		{name: "a class cleared is the default size",
			edit: func(d *ProjectSettings) { d.Repositories[0].AgentResourceClass = "" }},
		{name: "no classes defined at all",
			project: func(p *v1alpha1.Project) { p.Status.Admits.ResourceClasses = nil },
			edit:    func(d *ProjectSettings) { d.Repositories[1].AgentResourceClass = "small" },
			want: []ProjectSettingsRefusal{{Field: FieldRepositoryClass, Item: "lib", Reason: RefusedClassNotAdmitted,
				Value: "small"}}},
		{name: "an undefined class already there does not lock the other repository",
			project: func(p *v1alpha1.Project) { p.Spec.Repositories[0].AgentResourceClass = "huge" },
			edit:    func(d *ProjectSettings) { d.Repositories[1].AgentResourceClass = "large" }},
	})
}

// Under the broker's cost unit a run's budget has bounds of its own, which
// the controller reports by taking the whole Project not Ready. A save that
// would walk into one is refused instead, on the field that causes it.
func TestCheckRunBudgets(t *testing.T) {
	cost := func(p *v1alpha1.Project) {
		a := p.Status.Admits
		a.CostBudgets, a.MaxRunCostMicroUSD = true, 12_000_000
		a.RunBudgetFloors = []v1alpha1.ProjectRunBudgetFloor{
			{Model: "anthropic/claude-b", EvaluateMicroUSD: 1_600_000},
			{Model: "anthropic/claude-dear", EvaluateMicroUSD: 4_800_000},
		}
	}
	and := func(muts ...func(*v1alpha1.Project)) func(*v1alpha1.Project) {
		return func(p *v1alpha1.Project) {
			for _, m := range muts {
				m(p)
			}
		}
	}
	evaluator := func(p *Policy) { p.Allow.DisableEvaluator = true }
	const evaluateCost = "limits.evaluate.maxCostMicroUSD"
	runChecks(t, []checkCase{
		{name: "hourly: the per-intent cost carries every unset stage past half the hourly limit", project: cost,
			edit: func(d *ProjectSettings) { d.Limits.MaxCostMicroUSD = 12_000_001 },
			want: []ProjectSettingsRefusal{bounded(FieldMaxCost, RefusedRunBudgetOverHourly, 12_000_000)}},
		{name: "hourly: exactly half is allowed", project: cost,
			edit: func(d *ProjectSettings) { d.Limits.MaxCostMicroUSD = 12_000_000 }},
		{name: "hourly: stages with budgets of their own do not follow the per-intent cost",
			project: and(cost, func(p *v1alpha1.Project) {
				for _, s := range []*v1alpha1.StageLimits{&p.Spec.Limits.Plan, &p.Spec.Limits.Build,
					&p.Spec.Limits.Revise} {
					s.MaxCostMicroUSD = 5_000_000
				}
			}),
			edit: func(d *ProjectSettings) { d.Limits.MaxCostMicroUSD = 20_000_000 }},
		{name: "hourly: a stage's own budget past it",
			project: and(cost, func(p *v1alpha1.Project) { p.Spec.Limits.MaxCostMicroUSD = 20_000_000 }),
			edit:    func(d *ProjectSettings) { d.Stages.Build.MaxCostMicroUSD = 13_000_000 },
			// The stage was unset, so its runs already had the whole
			// per-intent cost: 13 is less than the 20 it had.
			want: nil},
		{name: "hourly: a stage's budget raised past it",
			project: and(cost, func(p *v1alpha1.Project) {
				p.Spec.Limits.MaxCostMicroUSD = 20_000_000
				p.Spec.Limits.Build.MaxCostMicroUSD = 6_000_000
			}),
			edit: func(d *ProjectSettings) { d.Stages.Build.MaxCostMicroUSD = 13_000_000 },
			want: []ProjectSettingsRefusal{bounded("limits.build.maxCostMicroUSD", RefusedRunBudgetOverHourly, 12_000_000)}},
		{name: "hourly: a stage's budget cleared lands on the per-intent cost",
			project: and(cost, func(p *v1alpha1.Project) {
				p.Spec.Limits.MaxCostMicroUSD = 20_000_000
				p.Spec.Limits.Build.MaxCostMicroUSD = 6_000_000
			}),
			edit: func(d *ProjectSettings) { d.Stages.Build.MaxCostMicroUSD = 0 },
			want: []ProjectSettingsRefusal{bounded("limits.build.maxCostMicroUSD", RefusedRunBudgetOverHourly, 12_000_000)}},
		{name: "hourly: nothing while the broker counts tokens",
			project: func(p *v1alpha1.Project) { p.Status.Admits.MaxRunCostMicroUSD = 12_000_000 },
			edit:    func(d *ProjectSettings) { d.Limits.MaxCostMicroUSD = 20_000_000 }},
		{name: "hourly: the evaluator's stages count once the Project is verified",
			project: and(cost, func(p *v1alpha1.Project) {
				p.Status.Admits.MaxRunCostMicroUSD = 5_000_000
				p.Spec.Limits.MaxCostMicroUSD = 5_000_000
				p.Spec.Verification.Enabled = false
			}),
			edit: func(d *ProjectSettings) { d.Verification.Enabled = true },
			want: []ProjectSettingsRefusal{bounded(FieldVerificationEnabled, RefusedRunBudgetOverHourly, 5_000_000)}},

		// The reserve is judged with the operator's switch on, as below: a
		// budget under the reserve is also under the evaluate stage's
		// floor, which TestCheckCeilingsAndFloors judges.
		{name: "reserve: an evaluate budget under what its model's requests reserve", project: cost, policy: evaluator,
			edit: func(d *ProjectSettings) { d.Stages.Evaluate.MaxCostMicroUSD = 1_500_000 },
			want: []ProjectSettingsRefusal{{Field: evaluateCost, Reason: RefusedRunBudgetUnderReserve,
				Limit: 1_600_000, Value: "anthropic/claude-b"}}},
		{name: "reserve: at the floor", project: cost, policy: evaluator,
			edit: func(d *ProjectSettings) { d.Stages.Evaluate.MaxCostMicroUSD = 1_600_000 }},
		{name: "reserve: a dearer model moves the floor past the budget",
			project: and(cost, func(p *v1alpha1.Project) { p.Spec.Limits.Evaluate.MaxCostMicroUSD = 2_000_000 }),
			edit:    func(d *ProjectSettings) { d.Models.Evaluate = "anthropic/claude-dear" },
			want: []ProjectSettingsRefusal{{Field: "models.evaluate", Reason: RefusedRunBudgetUnderReserve,
				Limit: 4_800_000, Value: "anthropic/claude-dear"}}},
		{name: "reserve: the evaluation cost is the budget of a stage with none of its own", policy: evaluator,
			project: cost, edit: func(d *ProjectSettings) { d.Limits.MaxEvaluateCostMicroUSD = 1_000_000 },
			want: []ProjectSettingsRefusal{{Field: FieldMaxEvaluateCost, Reason: RefusedRunBudgetUnderReserve,
				Limit: 1_600_000, Value: "anthropic/claude-b"}}},
		// The check reviewer is retired: no run of it is created, so the
		// budget its retired limits name, however short, is never judged,
		// as the controller no longer judges it.
		{name: "reserve: the retired check reviewer's budget is not judged",
			project: and(cost, func(p *v1alpha1.Project) {
				p.Spec.Limits.CheckReview.MaxCostMicroUSD = 1
				p.Spec.Verification.Enabled = false
			}),
			edit: func(d *ProjectSettings) { d.Verification.Enabled = true }},
		{name: "hourly: the retired check reviewer's budget is not judged",
			project: and(cost, func(p *v1alpha1.Project) {
				p.Status.Admits.MaxRunCostMicroUSD = 5_000_000
				p.Spec.Limits.Evaluate.MaxCostMicroUSD = 2_000_000
			}),
			edit: func(d *ProjectSettings) { d.Limits.MaxEvaluateCostMicroUSD = 7_000_000 }},
		{name: "reserve: a model with no entry has no floor",
			project: and(cost, func(p *v1alpha1.Project) { p.Spec.Limits.Evaluate.MaxCostMicroUSD = 1_000_000 }),
			edit:    func(d *ProjectSettings) { d.Models.Evaluate = "anthropic/claude-a" }},
		{name: "reserve: a budget already short may be raised toward the floor",
			project: and(cost, func(p *v1alpha1.Project) { p.Spec.Limits.Evaluate.MaxCostMicroUSD = 1_000_000 }),
			edit:    func(d *ProjectSettings) { d.Stages.Evaluate.MaxCostMicroUSD = 1_200_000 }},
		{name: "reserve: turning verification on with a budget that is short",
			project: and(cost, func(p *v1alpha1.Project) {
				p.Spec.Limits.Evaluate.MaxCostMicroUSD = 1_000_000
				p.Spec.Verification.Enabled = false
			}),
			edit: func(d *ProjectSettings) { d.Verification.Enabled = true },
			want: []ProjectSettingsRefusal{{Field: FieldVerificationEnabled, Reason: RefusedRunBudgetUnderReserve,
				Limit: 1_600_000, Value: "anthropic/claude-b"}}},
		{name: "reserve: not judged while the Project is not verified", policy: evaluator,
			project: and(cost, func(p *v1alpha1.Project) { p.Spec.Verification.Enabled = false }),
			edit:    func(d *ProjectSettings) { d.Stages.Evaluate.MaxCostMicroUSD = 1_000_000 }},
	})
}

// A run's budget is the stage's own, or the ceiling it is spent against
// when it has none or states more: the per-intent cost for plan, build and
// revise, the evaluation cost (held to the controller's own maximum) for
// the evaluate stage.
func TestRunBudget(t *testing.T) {
	spec := func(perIntent, evaluation, own int64) *v1alpha1.ProjectSpec {
		s := &v1alpha1.ProjectSpec{}
		s.Limits.MaxCostMicroUSD = perIntent
		if evaluation != 0 {
			s.Limits.MaxEvaluateCostMicroUSD = i64(evaluation)
		}
		s.Limits.Build.MaxCostMicroUSD, s.Limits.Evaluate.MaxCostMicroUSD = own, own
		return s
	}
	admits := func(max int64) *v1alpha1.ProjectAdmits { return &v1alpha1.ProjectAdmits{MaxEvaluateCostMicroUSD: max} }
	for _, tc := range []struct {
		name   string
		spec   *v1alpha1.ProjectSpec
		admits *v1alpha1.ProjectAdmits
		stage  string
		want   int64
	}{
		{"its own budget", spec(20_000_000, 0, 4_000_000), admits(0), "build", 4_000_000},
		{"none: the per-intent cost", spec(20_000_000, 0, 0), admits(0), "build", 20_000_000},
		{"none, nothing defaulted: the default per-intent cost", spec(0, 0, 0), nil, "build", 10_000_000},
		{"more than the per-intent cost: the per-intent cost", spec(3_000_000, 0, 4_000_000), nil, "build", 3_000_000},
		{"another stage's budget is not its own", spec(20_000_000, 0, 4_000_000), nil, "plan", 20_000_000},
		{"evaluate: its own budget", spec(20_000_000, 5_000_000, 4_000_000), admits(0), "evaluate", 4_000_000},
		{"evaluate: none, the evaluation cost", spec(20_000_000, 5_000_000, 0), admits(0), "evaluate", 5_000_000},
		{"evaluate: none and no evaluation cost, its default", spec(20_000_000, 0, 0), nil, "evaluate", 6_000_000},
		{"evaluate: the evaluation cost held to the controller's maximum", spec(20_000_000, 5_000_000, 0),
			admits(3_000_000), "evaluate", 3_000_000},
		{"evaluate: its own budget held to it too", spec(20_000_000, 5_000_000, 4_000_000), admits(3_000_000),
			"evaluate", 3_000_000},
		{"evaluate: a maximum above the evaluation cost changes nothing", spec(20_000_000, 5_000_000, 0),
			admits(9_000_000), "evaluate", 5_000_000},
		{"the retired check reviewer is no stage", spec(20_000_000, 5_000_000, 4_000_000), admits(0),
			"checkReview", 0},
		{"no such stage", spec(20_000_000, 0, 0), nil, "deploy", 0},
	} {
		if got := RunBudget(tc.spec, tc.admits, tc.stage); got != tc.want {
			t.Errorf("%s: RunBudget(%s) = %d, want %d", tc.name, tc.stage, got, tc.want)
		}
	}
}

// An existing value outside the policy never locks the form: only what a
// save changes is judged.
func TestCheckJudgesOnlyWhatChanged(t *testing.T) {
	p := baseProject(func(p *v1alpha1.Project) {
		p.Spec.Limits.MaxRevisions = i32(18)                           // past the page ceiling
		p.Spec.Limits.MaxCostMicroUSD = 90_000_000                     // past the page ceiling
		p.Spec.Limits.Evaluate.MaxTurns = 3                            // under the floor
		p.Spec.Limits.Build.MaxTurns = 900                             // past the controller's ceiling
		p.Spec.Models = &v1alpha1.ProjectModels{Plan: "openai/gpt-x"}  // not admitted
		p.Spec.Repositories[0].AgentResourceClass = "huge"             // not defined
		p.Spec.Verification.Handoff = v1alpha1.HandoffEarly            // the evaluator's switch
		p.Spec.Approvers.Logins = []string{"alice", "Alice", "x[bot]"} // no valid list
		p.Spec.Checks.Fix = nil
	})
	pol := DefaultPolicy()
	pol.Locked = []Group{GroupHandoff}
	out, changes := save(t, p, func(d *ProjectSettings) { d.Limits.MaxCheckFixes = i32(4) })
	if len(changes) != 1 {
		t.Fatalf("changes = %+v", changes)
	}
	if got := Check(p, out, pol, "alice"); len(got) != 0 {
		t.Errorf("refusals = %+v, want none: nothing out of policy was touched", got)
	}
}

// landings is, per bounded field, the number a spec's value takes effect
// as, written here from the API's own accessors and not from the table: a
// stage limit left unset is the controller's ceiling, the most there is.
func landings(s *v1alpha1.ProjectSpec) map[string]int64 {
	open := func(n int64) int64 {
		if n == 0 {
			return math.MaxInt64
		}
		return n
	}
	revisions, checkFixes := int64(v1alpha1.DefaultMaxRevisions), int64(v1alpha1.DefaultMaxCheckFixes)
	if s.Limits.MaxRevisions != nil {
		revisions = int64(*s.Limits.MaxRevisions)
	}
	if s.Limits.MaxCheckFixes != nil {
		checkFixes = int64(*s.Limits.MaxCheckFixes)
	}
	timeout := int64(0)
	if t := s.Limits.Evaluate.Timeout; t != nil {
		timeout = int64(t.Duration / time.Second)
	}
	return map[string]int64{
		FieldMaxCost:                  orDefault(s.Limits.MaxCostMicroUSD, v1alpha1.DefaultMaxCostMicroUSD),
		FieldMaxRevisions:             revisions,
		FieldMaxCheckFixes:            checkFixes,
		FieldMaxVerifyFixes:           int64(s.Limits.MaxVerifyFixesLimit()),
		FieldMaxVerifications:         int64(s.Limits.MaxVerificationsLimit()),
		FieldMaxActiveIntents:         orDefault(int64(s.Limits.MaxActiveIntents), int64(v1alpha1.DefaultMaxActiveIntents)),
		FieldMaxEvaluateCost:          s.Limits.MaxEvaluateCostLimit(),
		FieldMaxToolCalls:             int64(s.MaxToolCalls()),
		FieldHandoffMax:               int64(s.HandoffMax() / time.Second),
		"limits.evaluate.maxTurns":    open(int64(s.Limits.Evaluate.MaxTurns)),
		"limits.evaluate.tokenBudget": open(s.Limits.Evaluate.TokenBudget),
		"limits.evaluate.timeout":     open(timeout),
	}
}

// The containment property. Whatever the Project, the policy and the save,
// a save Check lets through:
//   - changes nothing in a locked group;
//   - leaves no limit above the operator's ceiling unless it was already at
//     least as high, and none under a floor unless it was already at least
//     as low or the operator allows the evaluator to be disabled;
//   - changes the approvers only with the switch, by one login, keeping
//     the editor, to a list the schema admits;
//   - turns verification off, weakens the hand-off or empties the checks
//     only with their switches;
//   - names no new model and no new class but one the controller admits.
func TestNoSavePassesTheOperatorsBounds(t *testing.T) {
	passed, refusedSaves := 0, 0
	prop := func(p *v1alpha1.Project, pol Policy, editSeed int64) bool {
		doc := Read(&p.Spec)
		genEdit(rand.New(rand.NewSource(editSeed)), &doc)
		out := p.DeepCopy()
		changes, err := Apply(out, doc)
		if err != nil {
			t.Logf("Apply: %v", err)
			return false
		}
		editor := p.Spec.Approvers.Logins[0]
		refusals := Check(p, out, pol, editor)
		if len(changes) == 0 && len(refusals) != 0 {
			t.Logf("refused a save that changes nothing: %+v", refusals)
			return false
		}
		if len(refusals) != 0 {
			refusedSaves++
			return true
		}
		if len(changes) == 0 {
			return true
		}
		passed++
		for _, c := range changes {
			if pol.Locks(Group(c.Group)) {
				t.Logf("let through a change to %s, in locked group %s", c.Field, c.Group)
				return false
			}
		}
		was, is := landings(&p.Spec), landings(&out.Spec)
		for _, b := range pol.ceilings() {
			if is[b.field] > b.n && is[b.field] > was[b.field] {
				t.Logf("%s: %d -> %d, past the ceiling %d", b.field, was[b.field], is[b.field], b.n)
				return false
			}
		}
		for _, b := range pol.floors() {
			if !pol.Allow.DisableEvaluator && is[b.field] < b.n && is[b.field] < was[b.field] {
				t.Logf("%s: %d -> %d, under the floor %d", b.field, was[b.field], is[b.field], b.n)
				return false
			}
		}
		return switchesHold(t, p, out, pol, editor) && admitsHold(t, p, out, was, is) && delegationHolds(t, p, out)
	}
	cfg := quickConfig(20000, func(args []reflect.Value, r *rand.Rand) {
		args[0] = reflect.ValueOf(genProject(r))
		args[1] = reflect.ValueOf(genPolicy(r))
		args[2] = reflect.ValueOf(r.Int63())
	})
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
	// The property says something only of saves that change something and
	// are let through, and the generators must reach the refusals too.
	if passed < 2000 || refusedSaves < 2000 {
		t.Errorf("of 20000 saves %d were let through and %d refused: the property judged too few", passed,
			refusedSaves)
	}
	t.Logf("%d let through, %d refused", passed, refusedSaves)
}

// delegationHolds: no switch the controller does not admit was turned on,
// and workflows are never left on without subagents.
func delegationHolds(t *testing.T, p, out *v1alpha1.Project) bool {
	t.Helper()
	a, was, is := p.Status.Admits, p.Spec.Delegation, out.Spec.Delegation
	leaf := func(d *v1alpha1.ProjectDelegation) [2]bool {
		if d == nil {
			return [2]bool{}
		}
		return [2]bool{d.Subagents, d.Workflows}
	}
	before, after := leaf(was), leaf(is)
	for i, admitted := range [2]bool{a != nil && a.Subagents, a != nil && a.Workflows} {
		if after[i] && !before[i] && !admitted {
			t.Logf("delegation switch %d turned on, admits %+v", i, a)
			return false
		}
	}
	if after[1] && !after[0] {
		t.Log("workflows left on without subagents")
		return false
	}
	return true
}

func switchesHold(t *testing.T, p, out *v1alpha1.Project, pol Policy, editor string) bool {
	t.Helper()
	was, is := &p.Spec, &out.Spec
	if !sameSet(was.Approvers.Logins, is.Approvers.Logins) {
		added, removed := missing(is.Approvers.Logins, was.Approvers.Logins),
			missing(was.Approvers.Logins, is.Approvers.Logins)
		if !pol.Allow.Approvers || len(added)+len(removed) != 1 || !is.Approvers.Has(editor) ||
			CheckApprovers(is.Approvers.Logins) != nil {
			t.Logf("approvers %v -> %v let through (allow %v, editor %s)", was.Approvers.Logins,
				is.Approvers.Logins, pol.Allow.Approvers, editor)
			return false
		}
	}
	if pol.Allow.DisableEvaluator {
		return pol.Allow.EmptyChecks || len(was.Checks.Fix) == 0 || len(is.Checks.Fix) != 0
	}
	if was.VerificationEnabled() && !is.VerificationEnabled() {
		t.Log("verification turned off without the switch")
		return false
	}
	// With the install's default unknown, a hand-off counts as gated only
	// when it says so, and as early only when it says so.
	mode := func(s *v1alpha1.ProjectSpec) v1alpha1.HandoffMode { return s.HandoffMode("") }
	if mode(was) != mode(is) && mode(is) != v1alpha1.HandoffGated && mode(was) != v1alpha1.HandoffEarly {
		t.Logf("hand-off %q -> %q without the switch", mode(was), mode(is))
		return false
	}
	if !pol.Allow.EmptyChecks && len(was.Checks.Fix) != 0 && len(is.Checks.Fix) == 0 {
		t.Log("checks emptied without the switch")
		return false
	}
	return true
}

func admitsHold(t *testing.T, p, out *v1alpha1.Project, was, is map[string]int64) bool {
	t.Helper()
	var admitted, classes []string
	if a := p.Status.Admits; a != nil {
		admitted = a.Models
		for _, c := range a.ResourceClasses {
			classes = append(classes, c.Name)
		}
	}
	pair := func(m *v1alpha1.ProjectModels) []string {
		if m == nil {
			return make([]string, 4)
		}
		return []string{m.Plan, m.Build, m.Evaluate, m.CheckReview}
	}
	before, after := pair(p.Spec.Models), pair(out.Spec.Models)
	for i, m := range after {
		if m != before[i] && m != "" && !slices.Contains(admitted, m) {
			t.Logf("model %q let through, admitted %v", m, admitted)
			return false
		}
	}
	for i, repo := range out.Spec.Repositories {
		class := repo.AgentResourceClass
		if class != p.Spec.Repositories[i].AgentResourceClass && class != "" && !slices.Contains(classes, class) {
			t.Logf("class %q let through, defined %v", class, classes)
			return false
		}
	}
	return controllerHolds(t, p, out, was, is)
}

// controllerHolds: nothing intent-controller bounds was raised past its
// ceiling, or raised at all while it publishes none.
func controllerHolds(t *testing.T, p, out *v1alpha1.Project, was, is map[string]int64) bool {
	t.Helper()
	for _, f := range Fields() {
		if !clamped(f.ID) {
			continue
		}
		// Only a stated value can pass the controller's ceiling (unset is
		// clamped to it), and only a raise is judged: of the two limits
		// with a default, a value above where the limit landed before; of
		// a stage limit, one stated where there was none, or a higher one.
		r, _ := rowByID(f.ID)
		old, now := r.read(&p.Spec).n, r.read(&out.Spec).n
		raised := now != 0 && now != old && (old == 0 || now > old)
		if f.HasDefault {
			raised = now != 0 && is[f.ID] > was[f.ID]
		}
		ceiling := controllerCeiling(p.Status.Admits, f.ID)
		if raised && (p.Status.Admits == nil || ceiling > 0 && now > ceiling) {
			t.Logf("%s raised to %d, controller ceiling %d (admits published: %v)", f.ID, now, ceiling,
				p.Status.Admits != nil)
			return false
		}
	}
	return true
}

// Every refusal names an editable field and a reason from the fixed set,
// so the page can put it beside the right input in words of its own.
func TestRefusalsNameAFieldAndAFixedReason(t *testing.T) {
	reasons := []string{RefusedLocked, RefusedOutOfRange, RefusedInvalid, RefusedApproversNotAllowed,
		RefusedApproversInvalid, RefusedApproversOneAtATime, RefusedApproversSelfRemoval,
		RefusedEvaluatorOffNotAllowed, RefusedHandoffNotAllowed, RefusedBelowFloor, RefusedAboveCeiling,
		RefusedAboveControllerCeiling, RefusedAdmitsUnpublished, RefusedModelNotAdmitted, RefusedClassNotAdmitted,
		RefusedChecksEmptyNotAllowed, RefusedRunBudgetOverHourly, RefusedRunBudgetUnderReserve,
		RefusedDelegationNotAdmitted, RefusedWorkflowsNeedSubagents}
	for _, r := range reasons {
		if r == "" || strings.ToLower(r) != r || strings.ContainsAny(r, " _") {
			t.Errorf("reason %q is not a lower-case key", r)
		}
	}
	ids := make([]string, 0, len(Fields()))
	for _, f := range Fields() {
		ids = append(ids, f.ID)
	}
	seen := map[string]bool{}
	prop := func(p *v1alpha1.Project, pol Policy, editSeed int64) bool {
		doc := Read(&p.Spec)
		genEdit(rand.New(rand.NewSource(editSeed)), &doc)
		out := p.DeepCopy()
		if _, err := Apply(out, doc); err != nil {
			return false
		}
		for _, r := range Check(p, out, pol, "nobody") {
			seen[r.Reason] = true
			if !slices.Contains(ids, r.Field) || !slices.Contains(reasons, r.Reason) {
				t.Logf("refusal %+v", r)
				return false
			}
			if (r.Item != "") != (r.Field == FieldRepositoryClass) {
				t.Logf("refusal %+v: an item goes with the per-repository field only", r)
				return false
			}
		}
		return true
	}
	cfg := quickConfig(6000, func(args []reflect.Value, r *rand.Rand) {
		args[0] = reflect.ValueOf(genProject(r))
		args[1] = reflect.ValueOf(genPolicy(r))
		args[2] = reflect.ValueOf(r.Int63())
	})
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
	// The generators reach most of the rules; one they never reach is a
	// rule the property above says nothing about.
	for _, r := range []string{RefusedLocked, RefusedOutOfRange, RefusedApproversNotAllowed,
		RefusedApproversOneAtATime, RefusedApproversSelfRemoval, RefusedEvaluatorOffNotAllowed,
		RefusedHandoffNotAllowed, RefusedBelowFloor, RefusedAboveCeiling, RefusedAboveControllerCeiling,
		RefusedAdmitsUnpublished, RefusedModelNotAdmitted, RefusedClassNotAdmitted, RefusedChecksEmptyNotAllowed,
		RefusedDelegationNotAdmitted, RefusedWorkflowsNeedSubagents} {
		if !seen[r] {
			t.Errorf("no generated save was refused as %s", r)
		}
	}
}
