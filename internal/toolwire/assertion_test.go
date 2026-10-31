// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package toolwire

import (
	"encoding/json"
	"strings"
	"testing"
)

// failAt is an attempt whose steps pass up to index at, which fails (an
// assertion when assertion is set).
func failAt(at int, assertion bool) Attempt {
	a := Attempt{Run: "run", Result: ResultFail}
	for i := range at {
		a.Steps = append(a.Steps, StepRecord{Index: i, Result: ResultPass})
	}
	a.Steps = append(a.Steps, StepRecord{Index: at, Result: ResultFail, Detail: "json \"/a\" did not match",
		Assertion: assertion})
	return a
}

// TestAssertionStep: a run is red by assertion only when it failed (never
// quarantined, never an error) with every attempt failing at the same step,
// and that step an assertion the check otherwise ran up to.
func TestAssertionStep(t *testing.T) {
	cases := []struct {
		name     string
		attempts []Attempt
		step     int
		ok       bool
	}{
		{"both attempts at one assertion", []Attempt{failAt(2, true), failAt(2, true)}, 2, true},
		{"an attempt not by assertion", []Attempt{failAt(2, true), failAt(2, false)}, 0, false},
		{"neither by assertion", []Attempt{failAt(1, false), failAt(1, false)}, 0, false},
		{"at different steps", []Attempt{failAt(1, true), failAt(2, true)}, 0, false},
		{"one attempt only", []Attempt{failAt(1, true)}, 0, false},
		{"fail then pass (quarantined)", []Attempt{failAt(1, true), {Run: "r", Result: ResultPass}}, 0, false},
		{"an error", []Attempt{failAt(1, true), {Run: "r", Result: ResultError,
			Steps: []StepRecord{{Index: 0, Result: ResultError}}}}, 0, false},
		{"a pass", []Attempt{{Run: "r", Result: ResultPass, Steps: []StepRecord{{Index: 0, Result: ResultPass}}}}, 0,
			false},
		{"an attempt with no step", []Attempt{failAt(0, true), {Run: "r", Result: ResultFail}}, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := CheckRunRecord{Attempts: c.attempts, Result: DeriveResult(c.attempts)}
			step, ok := r.AssertionStep()
			if ok != c.ok || step != c.step {
				t.Errorf("AssertionStep() = %d, %v; want %d, %v", step, ok, c.step, c.ok)
			}
		})
	}
}

// TestEntryCheckHoldsAssertion: only a failing step is an assertion.
func TestEntryCheckHoldsAssertion(t *testing.T) {
	e := checkRunEntry(KindRelay, VerbConfirm, ModeConfirm, ResultFail, ResultFail)
	e.Seq, e.OutputSHA256 = 1, sum(nil)
	e.CheckRun.Attempts = []Attempt{failAt(0, true), failAt(0, true)}
	if err := e.Check(); err != nil {
		t.Fatalf("an assertion failure was refused: %v", err)
	}
	e.CheckRun.Attempts[1].Steps[0].Result = ResultError
	e.CheckRun.Attempts[1].Result = ResultError
	e.CheckRun.Result = DeriveResult(e.CheckRun.Attempts)
	if e.Check() == nil {
		t.Error("an error step marked as an assertion was accepted")
	}
}

// TestConfirmExpectFail: a confirm may name digests it expects to fail,
// each one it names to run, once.
func TestConfirmExpectFail(t *testing.T) {
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	ok := &ConfirmRequest{Digests: []string{a, b}, ExpectFail: []string{b}}
	if err := ok.Check(); err != nil {
		t.Fatalf("Check() = %v", err)
	}
	raw, err := json.Marshal(ok)
	if err != nil {
		t.Fatal(err)
	}
	var back ConfirmRequest
	if err := DecodeRequest(strings.NewReader(string(raw)), &back); err != nil || len(back.ExpectFail) != 1 {
		t.Fatalf("round trip: %+v, %v", back, err)
	}
	for name, req := range map[string]*ConfirmRequest{
		"not named":  {Digests: []string{a}, ExpectFail: []string{b}},
		"twice":      {Digests: []string{a}, ExpectFail: []string{a, a}},
		"not sha256": {Digests: []string{a}, ExpectFail: []string{"abc"}},
	} {
		if req.Check() == nil {
			t.Errorf("%s: Check accepted %+v", name, req)
		}
	}
}
