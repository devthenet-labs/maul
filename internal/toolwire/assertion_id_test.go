// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package toolwire

import "testing"

// withID gives an attempt's failing step an assertion id.
func withID(a Attempt, id string) Attempt {
	a.Steps[len(a.Steps)-1].AssertionID = id
	return a
}

// TestAssertionIdentity (second review, 2B): a run is red by assertion only
// when both attempts failed the same assertion, its id as well as its step
// (an http step holds a status, JSON pointers, headers and body text; an
// exec check is always one step), and that assertion is readable.
func TestAssertionIdentity(t *testing.T) {
	same := CheckRunRecord{Result: RunFail, Attempts: []Attempt{withID(failAt(1, true), "json /b"),
		withID(failAt(1, true), "json /b")}}
	if step, ok := same.AssertionStep(); !ok || step != 1 || same.FailedAssertionID() != "json /b" {
		t.Errorf("same assertion: step %d ok %v id %q", step, ok, same.FailedAssertionID())
	}
	other := CheckRunRecord{Result: RunFail, Attempts: []Attempt{withID(failAt(1, true), "json /b"),
		withID(failAt(1, true), "json /c")}}
	if _, ok := other.AssertionStep(); ok {
		t.Error("attempts failing different assertions of one step are red by assertion")
	}
	missing := CheckRunRecord{Result: RunFail, Attempts: []Attempt{withID(failAt(1, true), ""),
		withID(failAt(1, true), "")}}
	if _, ok := missing.AssertionStep(); ok {
		t.Error("an assertion with no id is red by assertion")
	}
	e := checkRunEntry(KindRelay, VerbConfirm, ModeConfirm, ResultFail, ResultFail)
	e.Seq, e.OutputSHA256 = 1, sum(nil)
	e.CheckRun.Attempts = []Attempt{withID(failAt(0, false), "json /b"), withID(failAt(0, false), "json /b")}
	if e.Check() == nil {
		t.Error("an assertion id on a step that is no assertion was accepted")
	}
}
