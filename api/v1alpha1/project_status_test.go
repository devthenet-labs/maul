// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// publishedProjectStatus is a Project status with everything the project
// settings page reads from it set: every approver state and every admit.
func publishedProjectStatus() ProjectStatus {
	at := metav1.NewTime(time.Date(2026, 11, 22, 9, 30, 0, 0, time.UTC))
	stage := func(turns int32, tokens int64, timeout time.Duration) ProjectStageCeiling {
		return ProjectStageCeiling{MaxTurns: turns, TokenBudget: tokens, Timeout: &metav1.Duration{Duration: timeout}}
	}
	return ProjectStatus{
		Approvers: &ProjectApproversStatus{
			Identities: []ProjectApproverIdentity{
				{Login: "octocat", ID: 583231, Write: true},
				{Login: "Mona", ID: 21},
				{Login: "ghost"},
			},
			ResolvedAt: &at,
		},
		Admits: &ProjectAdmits{
			Models: []string{"anthropic/claude-opus-5", "anthropic/claude-sonnet-5"},
			DefaultModels: ProjectStageModels{
				Plan: "anthropic/claude-opus-5", Build: "anthropic/claude-sonnet-5",
				Evaluate: "anthropic/claude-sonnet-5", CheckReview: "anthropic/claude-haiku-5",
			},
			Stages: ProjectStageCeilings{
				Plan:        stage(40, 200000, 20*time.Minute),
				Build:       stage(150, 800000, time.Hour),
				Revise:      stage(80, 400000, 45*time.Minute),
				Evaluate:    stage(100, 400000, 20*time.Minute),
				CheckReview: stage(6, 100000, 15*time.Minute),
			},
			MaxEvaluateCostMicroUSD: 6000000,
			MaxToolCalls:            300,
			CostBudgets:             true,
			MaxRunCostMicroUSD:      25000000,
			RunBudgetFloors: []ProjectRunBudgetFloor{
				{Model: "anthropic/claude-opus-5", EvaluateMicroUSD: 1600000, CheckReviewMicroUSD: 409600},
				{Model: "anthropic/claude-sonnet-5", EvaluateMicroUSD: 960000},
			},
			Evaluator: true,
			Subagents: true,
			Workflows: true,
			ResourceClasses: []ProjectResourceClass{{
				Name:     "large",
				Requests: ProjectResourceQuantities{CPU: "4", Memory: "8Gi"},
				Limits:   ProjectResourceQuantities{Memory: "10Gi"},
			}},
		},
	}
}

// TestProjectStatusWirePaths pins the JSON of what intent-controller
// publishes for the project settings page. The names are a contract with
// two readers that cannot be compiled against these types: the admission
// policy's CEL (oldObject.status.admits.models,
// oldObject.status.admits.resourceClasses[].name) and the page's own wire
// types. A rename here would leave the policy reading a field that is never
// there, which it must treat as nothing admitted.
func TestProjectStatusWirePaths(t *testing.T) {
	got, err := json.Marshal(publishedProjectStatus())
	if err != nil {
		t.Fatalf("Marshal(status) = %v", err)
	}
	const want = `{` +
		`"approvers":{"identities":[` +
		`{"login":"octocat","id":583231,"write":true},{"login":"Mona","id":21},{"login":"ghost"}],` +
		`"resolvedAt":"2026-11-22T09:30:00Z"},` +
		`"admits":{"models":["anthropic/claude-opus-5","anthropic/claude-sonnet-5"],` +
		`"defaultModels":{"plan":"anthropic/claude-opus-5","build":"anthropic/claude-sonnet-5",` +
		`"evaluate":"anthropic/claude-sonnet-5","checkReview":"anthropic/claude-haiku-5"},` +
		`"stages":{"plan":{"maxTurns":40,"tokenBudget":200000,"timeout":"20m0s"},` +
		`"build":{"maxTurns":150,"tokenBudget":800000,"timeout":"1h0m0s"},` +
		`"revise":{"maxTurns":80,"tokenBudget":400000,"timeout":"45m0s"},` +
		`"evaluate":{"maxTurns":100,"tokenBudget":400000,"timeout":"20m0s"},` +
		`"checkReview":{"maxTurns":6,"tokenBudget":100000,"timeout":"15m0s"}},` +
		`"maxEvaluateCostMicroUSD":6000000,"maxToolCalls":300,"costBudgets":true,"maxRunCostMicroUSD":25000000,` +
		`"runBudgetFloors":[` +
		`{"model":"anthropic/claude-opus-5","evaluateMicroUSD":1600000,"checkReviewMicroUSD":409600},` +
		`{"model":"anthropic/claude-sonnet-5","evaluateMicroUSD":960000}],` +
		`"evaluator":true,"subagents":true,"workflows":true,` +
		`"resourceClasses":[{"name":"large","requests":{"cpu":"4","memory":"8Gi"},"limits":{"memory":"10Gi"}}]}}`
	if string(got) != want {
		t.Errorf("status marshals as\n  %s\nwant\n  %s", got, want)
	}
}

// TestProjectDelegation pins spec.delegation's JSON, which the admission
// policy's CEL reads (object.spec.?delegation.?subagents) without the types,
// and its accessors: unset is neither, and workflows count only beside
// subagents, so no caller re-derives that rule.
func TestProjectDelegation(t *testing.T) {
	got, err := json.Marshal(ProjectSpec{Delegation: &ProjectDelegation{Subagents: true, Workflows: true}})
	if err != nil {
		t.Fatalf("Marshal(spec) = %v", err)
	}
	if want := `"delegation":{"subagents":true,"workflows":true}`; !strings.Contains(string(got), want) {
		t.Errorf("spec marshals as %s, want it to hold %s", got, want)
	}
	for _, tt := range []struct {
		name                 string
		d                    *ProjectDelegation
		subagents, workflows bool
	}{
		{"unset", nil, false, false},
		{"empty", &ProjectDelegation{}, false, false},
		{"subagents", &ProjectDelegation{Subagents: true}, true, false},
		{"both", &ProjectDelegation{Subagents: true, Workflows: true}, true, true},
		{"workflows alone, which the schema refuses", &ProjectDelegation{Workflows: true}, false, false},
	} {
		s := &ProjectSpec{Delegation: tt.d}
		if s.SubagentsEnabled() != tt.subagents || s.WorkflowsEnabled() != tt.workflows {
			t.Errorf("%s: SubagentsEnabled, WorkflowsEnabled = %v, %v; want %v, %v", tt.name, s.SubagentsEnabled(),
				s.WorkflowsEnabled(), tt.subagents, tt.workflows)
		}
	}
}

// TestProjectStatusAbsentIsNotEmpty: a Project the controller has published
// nothing for says so. The settings page fails closed on an absent
// status.admits (no model may be chosen, no limit raised) and on an absent
// status.approvers (nobody may edit), which it could not tell from "the
// controller admits no model" or "no approver resolved" if either were a
// value that is always there.
func TestProjectStatusAbsentIsNotEmpty(t *testing.T) {
	unpublished, err := json.Marshal(ProjectStatus{})
	if err != nil {
		t.Fatalf("Marshal(empty status) = %v", err)
	}
	if string(unpublished) != `{}` {
		t.Errorf("an unpublished status marshals as %s, want {}", unpublished)
	}
	var read ProjectStatus
	if err := json.Unmarshal([]byte(`{"approvers":{},"admits":{}}`), &read); err != nil {
		t.Fatalf("Unmarshal = %v", err)
	}
	if read.Approvers == nil || read.Admits == nil {
		t.Errorf("published-but-empty read back as approvers %v, admits %v; want both set", read.Approvers, read.Admits)
	}
	var none ProjectStatus
	if err := json.Unmarshal([]byte(`{"activeIntents":1}`), &none); err != nil {
		t.Fatalf("Unmarshal = %v", err)
	}
	if none.Approvers != nil || none.Admits != nil {
		t.Errorf("an unpublished status read back as approvers %v, admits %v; want neither", none.Approvers, none.Admits)
	}
}

// TestProjectSettingsConditionAndBounds pins the Project condition the
// approver resolution adds, and the status bounds to what they bound: a
// status the schema refused would take the Project's whole status write,
// Ready included, with it.
func TestProjectSettingsConditionAndBounds(t *testing.T) {
	if ConditionApproversResolved != "ApproversResolved" {
		t.Errorf("ConditionApproversResolved = %q", ConditionApproversResolved)
	}
	// One identity per login the spec may hold, and one admitted class per
	// name a repository may pick; the schema markers repeat both literals.
	if MaxProjectApprovers != 20 {
		t.Errorf("MaxProjectApprovers = %d, want the spec's 20 logins", MaxProjectApprovers)
	}
	if MaxAdmittedResourceClasses != 16 || MaxResourceClassNameLength != 32 {
		t.Errorf("class bounds = %d classes of %d characters, want 16 of 32", MaxAdmittedResourceClasses,
			MaxResourceClassNameLength)
	}
	if MaxAdmittedModels != 64 || MaxAdmittedModelLength != 256 {
		t.Errorf("model bounds = %d models of %d characters, want 64 of 256", MaxAdmittedModels, MaxAdmittedModelLength)
	}
}
