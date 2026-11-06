// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package toolwire

import (
	"encoding/json"
	"strings"
	"testing"
)

// erred is an attempt whose one step errored with cause.
func erred(cause StepCause) Attempt {
	return Attempt{Run: "run", Result: ResultError,
		Steps: []StepRecord{{Index: 0, Result: ResultError, Detail: "maul's words", Cause: cause}}}
}

// TestRunCause: a run's cause is its attempts' one cause, and only of a
// run that errored at every attempt, each for that same cause: a run with
// an attempt that failed, passed, errored with no cause or for another
// cause has none (SPEC-S2 S2-20: only a recorded cause leaves no base
// comparison).
func TestRunCause(t *testing.T) {
	pass := Attempt{Run: "run", Result: ResultPass, Steps: []StepRecord{{Index: 0, Result: ResultPass}}}
	cases := []struct {
		name     string
		attempts []Attempt
		want     StepCause
	}{
		{"one attempt", []Attempt{erred(CauseCompile)}, CauseCompile},
		{"two attempts, one cause", []Attempt{erred(CauseVendoredTree), erred(CauseVendoredTree)},
			CauseVendoredTree},
		{"two causes", []Attempt{erred(CauseCompile), erred(CauseNoModule)}, ""},
		{"an attempt with no cause", []Attempt{erred(CauseCompile), erred("")}, ""},
		{"an attempt that failed", []Attempt{failAt(0, false), erred(CauseCompile)}, ""},
		{"an error then a pass", []Attempt{erred(CauseCompile), pass}, ""},
		{"no cause", []Attempt{erred(""), erred("")}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := CheckRunRecord{Attempts: c.attempts, Result: DeriveResult(c.attempts)}
			if got := r.Cause(); got != c.want {
				t.Errorf("Cause() = %q, want %q", got, c.want)
			}
		})
	}
}

// TestStepCauseChecked: a cause is one of the closed set, and only on a
// step that errored.
func TestStepCauseChecked(t *testing.T) {
	for _, c := range []StepCause{CauseCompile, CauseVendoredTree, CauseReplaceOutsideTree, CauseNoModule,
		CauseBaseUnbuildable} {
		if !c.Known() {
			t.Errorf("%q is not known", c)
		}
	}
	cases := map[string]struct {
		attempt Attempt
		ok      bool
	}{
		"an error with a cause": {erred(CauseCompile), true},
		"an error with none":    {erred(""), true},
		"an unknown cause":      {erred("timeout"), false},
		"a cause on a fail": {Attempt{Run: "run", Result: ResultFail,
			Steps: []StepRecord{{Index: 0, Result: ResultFail, Cause: CauseCompile}}}, false},
		"a cause on a pass": {Attempt{Run: "run", Result: ResultPass,
			Steps: []StepRecord{{Index: 0, Result: ResultPass, Cause: CauseCompile}}}, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e := checkRunEntry(KindRelay, VerbCheckRun, ModeReplay)
			e.CheckRun.Attempts = []Attempt{c.attempt}
			e.CheckRun.Result = DeriveResult(e.CheckRun.Attempts)
			e.Seq, e.OutputSHA256 = 1, sum(nil)
			if err := e.Check(); (err == nil) != c.ok {
				t.Errorf("Check() = %v, want ok %v", err, c.ok)
			}
		})
	}
	// An S1 step line is byte-identical: no cause, no field.
	b, err := json.Marshal(StepRecord{Index: 0, Result: ResultError, Detail: "x"})
	if err != nil || strings.Contains(string(b), "cause") {
		t.Errorf("a step with no cause encodes %s (%v)", b, err)
	}
}
