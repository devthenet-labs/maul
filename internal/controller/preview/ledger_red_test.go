// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/checks"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// redLedger builds ledgers of checks proposed to show a defect.
type redLedger struct {
	t       *testing.T
	entries []toolwire.LedgerEntry
}

func (l *redLedger) add(e toolwire.LedgerEntry) int64 {
	e.Seq, e.TS = int64(len(l.entries)+1), time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	l.entries = append(l.entries, e)
	return e.Seq
}

// failing is a failed run's two attempts, each failing at step at (an
// assertion when assertion is set; the second attempt at step second).
func failing(at, second int, assertion bool) []toolwire.Attempt {
	attempt := func(run string, step int) toolwire.Attempt {
		a := toolwire.Attempt{Run: run, Result: toolwire.ResultFail}
		for i := range step {
			a.Steps = append(a.Steps, toolwire.StepRecord{Index: i, Result: toolwire.ResultPass})
		}
		a.Steps = append(a.Steps, toolwire.StepRecord{Index: step, Result: toolwire.ResultFail, Detail: "json did not match",
			Assertion: assertion})
		return a
	}
	return []toolwire.Attempt{attempt("0123456789abcdea", at), attempt("0123456789abcdeb", second)}
}

var green = []toolwire.Attempt{{Run: "0123456789abcdef", Result: toolwire.ResultPass}}

// dryRun records the agent's dry run of body, its attempts given.
func (l *redLedger) dryRun(body string, attempts []toolwire.Attempt) (string, int64) {
	c, err := checks.Parse([]byte(body))
	if err != nil {
		l.t.Fatal(err)
	}
	d := strings.TrimPrefix(checks.Digest(c), "sha256:")
	args, _ := json.Marshal(toolwire.CheckRunRequest{Spec: checks.Canonical(c)})
	return d, l.add(toolwire.LedgerEntry{Kind: toolwire.KindAgent, Verb: toolwire.VerbCheckRun, Args: args,
		CheckRun: &toolwire.CheckRunRecord{Digest: d, Mode: toolwire.ModeDryRun,
			Result: toolwire.DeriveResult(attempts), Attempts: attempts}})
}

// confirm records the relay's confirmation of d, expected to fail when red.
func (l *redLedger) confirm(d string, red bool, attempts []toolwire.Attempt) int64 {
	a := toolwire.ConfirmArgs{Digest: d}
	if red {
		a.Expect = toolwire.ExpectFail
	}
	args, _ := json.Marshal(a)
	return l.add(toolwire.LedgerEntry{Kind: toolwire.KindRelay, Verb: toolwire.VerbConfirm, Args: args,
		CheckRun: &toolwire.CheckRunRecord{Digest: d, Mode: toolwire.ModeConfirm, Result: toolwire.DeriveResult(attempts),
			Attempts: attempts}})
}

// TestConfirmedRedChecks: a check proposed to show a defect is confirmed
// only when its dry run and its confirmation are both red by assertion at
// the same step, with no green dry run of it; its confirmation run on
// status records the expectation and the assertion, and names the red dry
// run it required.
func TestConfirmedRedChecks(t *testing.T) {
	body := func(name string) string { return strings.Replace(testCheckBody, "items.body", "items."+name, 1) }
	l := &redLedger{t: t}

	good, goodDry := l.dryRun(body("red"), failing(1, 1, true))
	goodConf := l.confirm(good, true, failing(1, 1, true))
	other, _ := l.dryRun(body("other-step"), failing(1, 1, true))
	l.confirm(other, true, failing(0, 0, true))
	noAssert, _ := l.dryRun(body("no-assertion"), failing(1, 1, false))
	l.confirm(noAssert, true, failing(1, 1, false))
	flaky, _ := l.dryRun(body("flaky"), failing(1, 1, true))
	l.confirm(flaky, true, green)
	alsoGreen, _ := l.dryRun(body("also-green"), green)
	l.dryRun(body("also-green"), failing(1, 1, true))
	l.confirm(alsoGreen, true, failing(1, 1, true))
	unexpected, _ := l.dryRun(body("unexpected"), failing(1, 1, true))
	l.confirm(unexpected, false, failing(1, 1, true))
	// Review finding 3: a saved check this round's replay saw green, then
	// re-proposed red after the agent's own dry runs went red.
	replayedGreen, _ := l.dryRun(body("replayed-green"), green)
	l.entries[len(l.entries)-1].Kind, l.entries[len(l.entries)-1].CheckRun.Mode = toolwire.KindRelay,
		toolwire.ModeReplay
	l.dryRun(body("replayed-green"), failing(1, 1, true))
	l.confirm(replayedGreen, true, failing(1, 1, true))

	got := confirmedChecks(l.entries, testOutcomes())
	if len(got) != 1 || got[0].Digest != "sha256:"+good || got[0].Expect != v1alpha1.CheckExpectFail ||
		got[0].ID != "items.red" || got[0].FailedStep != 2 {
		t.Fatalf("confirmed %+v, want only items.red, expected to fail at step 2", got)
	}
	runs := confirmationRuns(l.entries)
	if len(runs) != 7 {
		t.Fatalf("confirmation runs %+v", runs)
	}
	if r := runs[0]; r.Seq != int32(goodConf) || r.DryRunSeq != int32(goodDry) || r.AssertionStep != 2 ||
		r.Expect != v1alpha1.CheckExpectFail || r.Result != v1alpha1.CheckRunFail {
		t.Errorf("the red confirmation %+v, want fail by assertion, expected, on dry run %d", r, goodDry)
	}
	if r := runs[2]; r.AssertionStep != 0 || r.DryRunSeq != 0 {
		t.Errorf("a confirmation not by assertion %+v: no assertion, no red dry run", r)
	}
	if r := runs[3]; r.Result != v1alpha1.CheckRunPass || r.Expect != v1alpha1.CheckExpectFail {
		t.Errorf("the flaky confirmation %+v, want a pass of a check expected to fail", r)
	}
}

// TestReplayRunsRecordAssertion: a replay records whether its red was an
// assertion.
func TestReplayRunsRecordAssertion(t *testing.T) {
	l := &redLedger{t: t}
	d, _ := l.dryRun(testCheckBody, green)
	for _, assertion := range []bool{true, false} {
		attempts := failing(1, 1, assertion)
		l.add(toolwire.LedgerEntry{Kind: toolwire.KindRelay, Verb: toolwire.VerbCheckRun, Args: json.RawMessage(`{}`),
			CheckRun: &toolwire.CheckRunRecord{Digest: d, Mode: toolwire.ModeReplay,
				Result: toolwire.DeriveResult(attempts), Attempts: attempts}})
	}
	runs := replayRuns(l.entries)
	if len(runs) != 2 || runs[0].AssertionStep != 2 || runs[1].AssertionStep != 0 {
		t.Errorf("replays %+v, want the first red by assertion only", runs)
	}
}
