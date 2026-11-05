// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"slices"
	"testing"
)

// s1CorpusPath is the S1 corpus: 900 rounds, each an input and 0.13.13's
// decision on it, recorded by running 0.13.13's Decide (ba0b1262, before
// slice S2 changed it) over the property generator's inputs (genDecide,
// seed 20261105) made S1-shaped: no base, no build part, no run form; the
// last 300 with every check an exec check, so O4 is exercised. The file
// keeps the inputs, so a later generator change never moves the corpus; it
// is a record of 0.13.13, never rewritten from the Decide under test.
const s1CorpusPath = "testdata/decide-s1-corpus.json.gz"

// s1Case is one recorded S1 round.
type s1Case struct {
	In       Input
	Decision Decision
}

// TestDecideS1Corpus holds S1 inputs (Base == nil) to 0.13.13's decisions.
// The one change slice S2 makes there is owner decision O4: a program
// check (exec or run) that failed other than by assertion (a crash, a
// compile error, a timeout, a wrong exit alone) never fails its outcome, so
// an outcome 0.13.13 failed on such a check alone is unsure now. Every
// other difference fails the test.
func TestDecideS1Corpus(t *testing.T) {
	cases := readS1Corpus(t)
	changed := 0
	for i, c := range cases {
		if c.In.Base != nil {
			t.Fatalf("case %d: an S1 input has a base", i)
		}
		got := Decide(c.In)
		if reflect.DeepEqual(normalise(got), normalise(c.Decision)) {
			continue
		}
		changed++
		if err := onlyO4(c.In, c.Decision, got); err != nil {
			t.Errorf("case %d: %v\nwas %+v\ngot %+v", i, err, c.Decision, got)
		}
	}
	if len(cases) != 900 {
		t.Errorf("the corpus holds %d cases, want 900", len(cases))
	}
	if changed == 0 {
		t.Error("no S1 decision changed: the corpus does not exercise O4")
	}
	t.Logf("%d of %d S1 decisions changed by O4", changed, len(cases))
}

// normalise makes nil and empty slices equal, as JSON loses the
// difference.
func normalise(d Decision) Decision {
	if len(d.PerOutcome) == 0 {
		d.PerOutcome = nil
	}
	if len(d.Retire) == 0 {
		d.Retire = nil
	}
	if len(d.Narrowed) == 0 {
		d.Narrowed = nil
	}
	return d
}

// onlyO4 reports a difference between 0.13.13's decision and now that O4
// does not explain: only outcomes failed (or held as a suspected defect) on
// program checks failing other than by assertion may turn unsure
// (ReasonNotAsserted), and only their failures may leave Fix and
// Suspected.
func onlyO4(in Input, was, got Decision) error {
	if len(was.PerOutcome) != len(got.PerOutcome) {
		return fmt.Errorf("outcome count changed")
	}
	var moved []string
	for i, w := range was.PerOutcome {
		g := got.PerOutcome[i]
		if w == g {
			continue
		}
		fromFail := w.Result == ResultFailed || w.Reason == ReasonSuspectedDefect
		if !fromFail || g.Result != ResultUnsure || g.Reason != ReasonNotAsserted || !unasserted(in, w.ID) {
			return fmt.Errorf("outcome %s: %+v -> %+v is not O4", w.ID, w, g)
		}
		moved = append(moved, w.ID)
	}
	if !sameFailuresBut(was.Fix, got.Fix, moved) || !sameFailuresBut(was.Suspected, got.Suspected, moved) {
		return fmt.Errorf("fix requests changed beyond the outcomes O4 made unsure %v", moved)
	}
	w, g := normalise(was), normalise(got)
	w.PerOutcome, g.PerOutcome, w.Fix, g.Fix, w.Suspected, g.Suspected = nil, nil, nil, nil, nil, nil
	if !reflect.DeepEqual(w, g) {
		return fmt.Errorf("the rest of the decision changed")
	}
	return nil
}

// unasserted reports a stored program check of the outcome with a fail
// this round that is not red by assertion.
func unasserted(in Input, outcome string) bool {
	return slices.ContainsFunc(in.Checks, func(c StoredCheck) bool {
		if c.Outcome != outcome || (c.Form != CheckFormExec && c.Form != CheckFormRun) {
			return false
		}
		return slices.ContainsFunc(append(slices.Clone(in.Replay), in.Confirmations...), func(r CheckRun) bool {
			return r.CheckDigest == c.Digest && r.Result == RunFail && r.AssertionStep == 0
		})
	})
}

// sameFailuresBut reports got as was less the failures of the moved
// outcomes (nil when none is left); the fingerprint follows the failures.
func sameFailuresBut(was, got *FixRequest, moved []string) bool {
	var keep []FixFailure
	if was != nil {
		keep = slices.DeleteFunc(slices.Clone(was.Failures), func(f FixFailure) bool {
			return slices.Contains(moved, f.Outcome)
		})
	}
	if len(keep) == 0 {
		return got == nil
	}
	if got == nil || !reflect.DeepEqual(keep, got.Failures) {
		return false
	}
	return len(keep) != len(was.Failures) || got.Fingerprint == was.Fingerprint
}

// readS1Corpus reads the recorded corpus.
func readS1Corpus(t *testing.T) []s1Case {
	t.Helper()
	f, err := os.Open(s1CorpusPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	var cases []s1Case
	if err := json.NewDecoder(zr).Decode(&cases); err != nil {
		t.Fatal(err)
	}
	return cases
}
