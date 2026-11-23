// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package projectsettings

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The chart's defaults are the owner's decision (2026-11-23): every switch
// off, nothing locked, and these ceilings and floors.
func TestDefaultPolicyIsTheOwnersDecision(t *testing.T) {
	want := Policy{
		Locked: []Group{},
		Ceilings: Ceilings{MaxCostMicroUSD: 25_000_000, MaxRevisions: 10, MaxCheckFixes: 10, MaxVerifyFixes: 5,
			MaxVerifications: 50, MaxActiveIntents: 5},
		Floors: Floors{MaxEvaluateCostMicroUSD: 2_000_000, EvaluateMaxTurns: 40,
			EvaluateTimeout: Duration(10 * time.Minute), EvaluateTokenBudget: 100_000, MaxToolCalls: 100,
			MaxVerifications: 3, HandoffMax: Duration(30 * time.Minute)},
	}
	got := DefaultPolicy()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DefaultPolicy = %+v, want %+v", got, want)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("the default policy is not valid: %v", err)
	}
}

// The JSON is the chart's contract: its keys are the values' keys.
func TestPolicyWireShape(t *testing.T) {
	raw, err := json.Marshal(DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"allow":{"approvers":false,"disableEvaluator":false,"emptyChecks":false},"locked":[],` +
		`"ceilings":{"maxCostMicroUSD":25000000,"maxRevisions":10,"maxCheckFixes":10,"maxVerifyFixes":5,` +
		`"maxVerifications":50,"maxActiveIntents":5},` +
		`"floors":{"maxEvaluateCostMicroUSD":2000000,"evaluateMaxTurns":40,"evaluateTimeout":"10m0s",` +
		`"evaluateTokenBudget":100000,"maxToolCalls":100,"maxVerifications":3,"handoffMax":"30m0s"}}`
	if string(raw) != want {
		t.Errorf("policy JSON =\n%s\nwant\n%s", raw, want)
	}
	back, err := ParsePolicy(raw)
	if err != nil || !reflect.DeepEqual(back, DefaultPolicy()) {
		t.Errorf("ParsePolicy(default JSON) = %+v, %v", back, err)
	}
}

func TestParsePolicy(t *testing.T) {
	valid := func(edit string) string {
		// A whole policy with one part replaced.
		base := map[string]string{
			"allow":  `{"approvers":true,"disableEvaluator":true,"emptyChecks":true}`,
			"locked": `["approvers","resources"]`,
			"ceilings": `{"maxCostMicroUSD":25000000,"maxRevisions":0,"maxCheckFixes":10,"maxVerifyFixes":5,` +
				`"maxVerifications":50,"maxActiveIntents":5}`,
			"floors": `{"evaluateTimeout":"10m","handoffMax":"1800s"}`,
		}
		if k, v, ok := strings.Cut(edit, "="); ok {
			base[k] = v
		}
		return `{"allow":` + base["allow"] + `,"locked":` + base["locked"] + `,"ceilings":` + base["ceilings"] +
			`,"floors":` + base["floors"] + `}`
	}
	p, err := ParsePolicy([]byte(valid("")))
	if err != nil {
		t.Fatal(err)
	}
	if !p.Allow.Approvers || !p.Locks(GroupResources) || p.Locks(GroupModels) ||
		p.Floors.HandoffMax != Duration(30*time.Minute) || p.Ceilings.MaxRevisions != 0 {
		t.Errorf("parsed %+v", p)
	}
	for name, tc := range map[string]struct{ raw, want string }{
		"empty":                    {"", "policy"},
		"not JSON":                 {"allow: {}", "policy"},
		"an unknown key":           {valid(`allow={"approvers":true,"anything":true}`), "anything"},
		"an unknown top-level key": {`{"allow":{},"lockd":[]}`, "lockd"},
		"trailing data":            {valid("") + `{}`, "more than one"},
		"an unknown group":         {valid(`locked=["approvers","everything"]`), "everything"},
		"a duration as a number":   {valid(`floors={"handoffMax":1800}`), "duration"},
		"a duration that is none":  {valid(`floors={"handoffMax":"soon"}`), "soon"},
		"a ceiling the field cannot hold": {valid(
			`ceilings={"maxCostMicroUSD":25000000,"maxRevisions":21,"maxVerifications":50,"maxActiveIntents":5}`),
			"limits.maxRevisions"},
		"a ceiling left out where zero is no value": {valid(`ceilings={"maxRevisions":10}`),
			"limits.maxCostMicroUSD"},
		"a floor the field cannot hold": {valid(`floors={"maxToolCalls":301}`), "verification.maxToolCalls"},
		"a floor under the field's range": {valid(`floors={"handoffMax":"5m"}`),
			"verification.handoffMax"},
		"a floor above its ceiling": {valid(`floors={"maxVerifications":60}`), "above its ceiling"},
	} {
		_, err := ParsePolicy([]byte(tc.raw))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %v, want one naming %q", name, err, tc.want)
		}
	}
}

func TestPolicyBounds(t *testing.T) {
	p := DefaultPolicy()
	for field, want := range map[string]int64{FieldMaxCost: 25_000_000, FieldMaxRevisions: 10,
		FieldMaxCheckFixes: 10, FieldMaxVerifyFixes: 5, FieldMaxVerifications: 50, FieldMaxActiveIntents: 5} {
		if got, ok := p.Ceiling(field); !ok || got != want {
			t.Errorf("Ceiling(%s) = %d, %v, want %d", field, got, ok, want)
		}
	}
	for field, want := range map[string]int64{FieldMaxEvaluateCost: 2_000_000, "limits.evaluate.maxTurns": 40,
		"limits.evaluate.timeout": 600, "limits.evaluate.tokenBudget": 100_000, FieldMaxToolCalls: 100,
		FieldMaxVerifications: 3, FieldHandoffMax: 1800} {
		if got, ok := p.Floor(field); !ok || got != want {
			t.Errorf("Floor(%s) = %d, %v, want %d", field, got, ok, want)
		}
	}
	if _, ok := p.Ceiling(FieldMaxEvaluateCost); ok {
		t.Error("the evaluation cost has a page ceiling; its ceiling is the controller's")
	}
	if _, ok := p.Floor("limits.plan.maxTurns"); ok {
		t.Error("a plan limit has a floor")
	}
	p.Floors.MaxToolCalls = 0
	if _, ok := p.Floor(FieldMaxToolCalls); ok {
		t.Error("a zero floor is a floor")
	}
	// Every bound is on a field of the table, of a kind that has a number.
	for _, b := range append(p.ceilings(), p.floors()...) {
		r, ok := rowByID(b.field)
		if !ok || !r.Kind.numeric() {
			t.Errorf("bound on %s, which is no numeric field", b.field)
		}
	}
}
