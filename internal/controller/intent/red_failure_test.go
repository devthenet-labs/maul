// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// TestRedCheckReviewGetsWhatItsFailureRecorded (G3-B-BR-1, G3-B-LIB-1):
// the red question is handed what maul recorded of the failing assertion in
// each of its confirmation runs (the step's detail: a browser assertion's
// measurement, a program's marker line), read from the verification's own
// ledger, each run's token written back as {{run}}. With any run's record
// missing or not one line, or no ledger, nothing is handed and the question
// is asked without it.
func TestRedCheckReviewGetsWhatItsFailureRecorded(t *testing.T) {
	const measured = "maul: visible failed: contrast 1.27..1.27 (#2b1d57 on #140c2b) < 1.5"
	for _, tc := range []struct {
		name    string
		ledger  bool
		details []string
		want    []string
	}{
		{"both runs recorded", true, []string{measured, measured}, []string{measured, measured}},
		{"a run's token written back", true,
			[]string{"the program printed its assertion marker: v: TOKEN gave 0 <nil>",
				"the program printed its assertion marker: v: TOKEN gave 0 <nil>"},
			[]string{"the program printed its assertion marker: v: {{run}} gave 0 <nil>",
				"the program printed its assertion marker: v: {{run}} gave 0 <nil>"}},
		{"a run with no record", true, []string{measured, ""}, nil},
		{"a record over two lines", true, []string{"a\nb", measured}, nil},
		{"no ledger", false, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, name := newEvaluatorEnv(t, 0.4)
			red := savedCheck(t, redBrowserCheck)
			red.Expect, red.FailedStep, red.FailedAssertion = v1alpha1.CheckExpectFail, 3, "expect"
			run := v1alpha1.CheckRun{CheckDigest: red.Digest, Result: v1alpha1.CheckRunFail, Token: "cf-a1", Seq: 10,
				DryRunSeq: 4, Expect: v1alpha1.CheckExpectFail, AssertionStep: 3, AssertionID: "expect"}
			r := &fakeReviewer{}
			e.reviewChecksWith(r)
			e.redRound(name, red, run, true)
			if tc.ledger {
				entry := redConfirmEntry(red.Digest, "TOKEN-v · v1.0", "TOKEN-v · v1.0")
				for i, d := range tc.details {
					a := &entry.CheckRun.Attempts[i]
					a.Steps[len(a.Steps)-1].Detail = strings.ReplaceAll(d, "TOKEN", a.Run)
				}
				e.ledgerOfEntries(e.onlyVerification(name).Name, entry)
			}
			e.settleReviews(name)
			if len(r.handoffs) != 1 || len(r.handoffs[0].Reviews) != 1 {
				t.Fatalf("handoffs = %+v, want the red check reviewed", r.handoffs)
			}
			if it := r.handoffs[0].Reviews[0]; it.Expect != "fail" || !slices.Equal(it.Failure, tc.want) {
				t.Errorf("review = %+v, want the red question with failure %q", it, tc.want)
			}
		})
	}
}

// TestAgentsRedReviewGetsItsMarkerLine (G3-B-CLI-2, G3-B-LIB-1): on a
// workspace round the red question is handed the line the program printed
// after its marker in each of maul's head confirmation runs (G5: "got [45
// 150] err <nil>"), never the base's runs of it, which the ledger records
// at a seq of their own.
func TestAgentsRedReviewGetsItsMarkerLine(t *testing.T) {
	const said = "the program printed its assertion marker: version: got [45 150] err <nil> for "
	e, name := newAgentsEnv(t, repoImage)
	check := savedCheck(t, agentsVersionRun)
	check.Expect, check.FailedStep, check.FailedAssertion = v1alpha1.CheckExpectFail, 1, "marker version"
	r := &fakeReviewer{}
	e.reviewChecksWith(r)
	pv := e.agentsEvaluated(name)
	e.agentsConfirm(pv.Name, false, func(c v1alpha1.SavedCheck) v1alpha1.CheckRun {
		return v1alpha1.CheckRun{CheckDigest: c.Digest, Result: v1alpha1.CheckRunFail, Token: "h-a1", Seq: 10,
			DryRunSeq: 1, Expect: v1alpha1.CheckExpectFail, AssertionStep: 1, AssertionID: "marker version"}
	}, func(c v1alpha1.SavedCheck) *v1alpha1.CheckRun {
		return &v1alpha1.CheckRun{CheckDigest: c.Digest, Result: v1alpha1.CheckRunError, Token: "b-a1", Seq: 11,
			Tree: v1alpha1.TreeBase, Cause: v1alpha1.CheckRunCauseCompile}
	}, check)
	entry := func(seq int64, side toolwire.Side, result toolwire.AttemptResult, tokens ...string) toolwire.LedgerEntry {
		rec := &toolwire.CheckRunRecord{Digest: strings.TrimPrefix(check.Digest, "sha256:"), CheckID: check.ID,
			Mode: toolwire.ModeConfirm, Result: toolwire.RunResult(result)}
		for _, tok := range tokens {
			s := toolwire.StepRecord{Result: result, Side: side, Detail: "the program did not compile (exit 1)"}
			if result == toolwire.ResultFail {
				s.Detail, s.Assertion, s.AssertionID = said+tok, true, "marker version"
			}
			rec.Attempts = append(rec.Attempts, toolwire.Attempt{Run: tok, Result: result,
				Steps: []toolwire.StepRecord{s}})
		}
		return toolwire.LedgerEntry{Seq: seq, TS: time.Unix(seq, 0).UTC(), Kind: toolwire.KindRelay,
			Verb: toolwire.VerbConfirm, Args: json.RawMessage(`{}`), CheckRun: rec}
	}
	e.ledgerOfEntries(pv.Name, entry(10, "", toolwire.ResultFail, "h-a1", "h-b2"),
		entry(11, toolwire.SideBase, toolwire.ResultError, "b-a1", "b-b2"))
	e.settleReviews(name)
	if len(r.handoffs) != 1 || len(r.handoffs[0].Reviews) != 1 {
		t.Fatalf("handoffs = %+v, want the red check reviewed", r.handoffs)
	}
	want := []string{said + "{{run}}", said + "{{run}}"}
	if it := r.handoffs[0].Reviews[0]; it.Expect != "fail" || !slices.Equal(it.Failure, want) {
		t.Errorf("review = %+v, want the head's marker lines %q", it, want)
	}
}

// TestReviewFromIsTheRequestLine (G3-B-LIB-1 point 5): an outcome's from is
// held to 300 characters by the plan grammar, so a long issue line reaches
// the plan cut mid-word; the reviewer is handed the whole line of the
// request it quotes, when exactly one line holds it, and the quote as it
// is otherwise.
func TestReviewFromIsTheRequestLine(t *testing.T) {
	const line = "- For the README's example pattern it returns `[0 500 1000 1500]` for `\"kick\"`. For a pattern at " +
		"100 bpm and swing 30 with kick on steps 1 and 2 it returns `[0 195]` for `\"kick\"`."
	request := "# render library: hit times\n\n" + line + "\n- A track that plays no step returns an empty slice.\n"
	cut := line[2:120]
	for _, tc := range []struct{ name, request, from, want string }{
		{"a quote cut mid-word", request, cut, line},
		{"a quote with the planner's ellipsis", request, cut + "...", line},
		{"a quote with a typographic ellipsis", request, cut + "…", line},
		{"a whole line", request, "A track that plays no step returns an empty slice.",
			"- A track that plays no step returns an empty slice."},
		{"no line holds it", request, "ratings should update", "ratings should update"},
		{"two lines hold it", request + line + "\n", cut, cut},
		{"no request", "", cut, cut},
		{"no quote", request, "", ""},
		{"a blank quote", request, "   ", "   "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := reviewFrom(tc.request, tc.from); got != tc.want {
				t.Errorf("reviewFrom = %q, want %q", got, tc.want)
			}
		})
	}
}
