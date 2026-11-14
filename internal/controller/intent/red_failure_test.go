// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"slices"
	"strings"
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
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
