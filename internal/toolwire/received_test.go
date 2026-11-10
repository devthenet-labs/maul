// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package toolwire

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestStepReceivedChecked (D-B4): a step's received text is the application's
// text a failing assertion read, so it is held to its bounds and kept only on
// a step that failed by assertion; a step with none encodes as before.
func TestStepReceivedChecked(t *testing.T) {
	asserted := func(received string) Attempt {
		a := failAt(1, true)
		a.Steps[1].Received = received
		return a
	}
	cases := map[string]struct {
		attempt Attempt
		ok      bool
	}{
		"on an assertion":        {asserted("by ana · 120 bpm · swing 0%"), true},
		"at the bound":           {asserted(strings.Repeat("x", MaxReceivedBytes)), true},
		"over the bound":         {asserted(strings.Repeat("x", MaxReceivedBytes+1)), false},
		"not UTF-8":              {asserted("swing \xff"), false},
		"two lines":              {asserted("swing 0%\nswing 20%"), false},
		"a carriage return":      {asserted("swing 0%\rswing 20%"), false},
		"on a fail by no assert": {func() Attempt { a := failAt(1, false); a.Steps[1].Received = "x"; return a }(), false},
		"on a pass": {Attempt{Run: "run", Result: ResultPass,
			Steps: []StepRecord{{Index: 0, Result: ResultPass, Received: "x"}}}, false},
		"on an error": {Attempt{Run: "run", Result: ResultError,
			Steps: []StepRecord{{Index: 0, Result: ResultError, Received: "x"}}}, false},
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
	b, err := json.Marshal(StepRecord{Index: 0, Result: ResultFail, Detail: "x"})
	if err != nil || strings.Contains(string(b), "received") {
		t.Errorf("a step with no received text encodes %s (%v)", b, err)
	}
}
