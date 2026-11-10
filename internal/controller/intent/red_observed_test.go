// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// redBrowserCheck saves a note of its own run and asserts the text of the
// row that carries it (D4's shape, beatbattle-77).
const redBrowserCheck = `{"id":"version-shown","outcome":"version-shown","form":"browser","browser":{"steps":[` +
	`{"goto":"/"},{"fill":{"locator":{"label":"Note"},"text":"{{run}}-v"}},` +
	`{"expect":{"assert":"text","locator":{"css":"li","hasText":"{{run}}-v"},"text":{"contains":"v1.2"}}}]}}`

// redConfirmEntry is the red browser check's confirmation at seq 10, as the
// tools container records it: both attempts failing its text assertion,
// each under its own token, with what the element showed (received, "" for
// none).
func redConfirmEntry(digest string, received ...string) toolwire.LedgerEntry {
	rec := &toolwire.CheckRunRecord{Digest: strings.TrimPrefix(digest, "sha256:"), CheckID: "version-shown",
		Mode: toolwire.ModeConfirm, Result: toolwire.RunFail}
	for i, r := range received {
		token := []string{"cf-a1", "cf-b2"}[i]
		rec.Attempts = append(rec.Attempts, toolwire.Attempt{Run: token, Result: toolwire.ResultFail,
			Steps: []toolwire.StepRecord{{Index: 0, Result: toolwire.ResultPass}, {Index: 1, Result: toolwire.ResultPass},
				{Index: 2, Result: toolwire.ResultFail, Detail: "the text expectation was not met", Assertion: true,
					AssertionID: "expect", Received: strings.ReplaceAll(r, "TOKEN", token)}}})
	}
	return toolwire.LedgerEntry{Seq: 10, TS: time.Unix(10, 0).UTC(), Kind: toolwire.KindRelay,
		Verb: toolwire.VerbConfirm, Args: json.RawMessage(`{}`), CheckRun: rec}
}

// TestRedCheckReviewGetsWhatItsElementShowed (D-B4): the red question about
// a browser text check is handed what the failing assertion's element showed
// in each of maul's confirmation runs, read from the verification's own
// ledger, each run's token written back as {{run}} so the reviewer can tell
// the run's own data from another run's. With any run's text missing, or no
// ledger to read, nothing is handed and the question is asked as before.
func TestRedCheckReviewGetsWhatItsElementShowed(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ledger   bool
		received []string
		want     []string
	}{
		{"both runs showed text", true, []string{"TOKEN-v · v1.0", "TOKEN-v · v1.0"},
			[]string{"{{run}}-v · v1.0", "{{run}}-v · v1.0"}},
		{"another run's token stays as it was", true, []string{"TOKEN-v · v1.0", "cf-a1-v · v1.0"},
			[]string{"{{run}}-v · v1.0", "cf-a1-v · v1.0"}},
		{"a run with no text", true, []string{"TOKEN-v · v1.0", ""}, nil},
		{"a text the reviewer would refuse", true, []string{"TOKEN-v\nv1.0", "TOKEN-v · v1.0"}, nil},
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
				entry := redConfirmEntry(red.Digest, tc.received...)
				if tc.want != nil {
					// An entry the tools container could have written.
					sum := sha256.Sum256(nil)
					wire := entry
					wire.OutputSHA256 = hex.EncodeToString(sum[:])
					if err := wire.Check(); err != nil {
						t.Fatalf("redConfirmEntry is no ledger entry: %v", err)
					}
				}
				e.ledgerOfEntries(e.onlyVerification(name).Name, entry)
			}
			e.settleReviews(name)
			if len(r.handoffs) != 1 || len(r.handoffs[0].Reviews) != 1 {
				t.Fatalf("handoffs = %+v, want the red check reviewed", r.handoffs)
			}
			it := r.handoffs[0].Reviews[0]
			if it.Expect != "fail" || it.FailedStep != 3 || !slices.Equal(it.Observed, tc.want) {
				t.Errorf("review = %+v, want the red question at step 3 observing %q", it, tc.want)
			}
		})
	}
}
