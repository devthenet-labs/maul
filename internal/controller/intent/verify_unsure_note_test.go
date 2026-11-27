// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
)

// unsureReport is an evaluator's report on version-shown with verdict and
// note as given (gate 5 G5-B-BR-1: an unsure verdict asking a person a
// question), a fail citing no call maul counts, still-healthy passed with
// a note of its own.
func unsureReport(verdict, note string) string {
	ledger := ""
	if verdict != "unsure" {
		ledger = "\n    ledger: [9]" // no such call: it counts for nothing
	}
	return fmt.Sprintf(`---
verdicts:
  - id: version-shown
    verdict: %s%s
    note: %q
  - id: still-healthy
    verdict: pass
    ledger: [3]
    note: "HEALTHY-NOTE: it answers its health check"
---
The version endpoint needs a person's reading.
`, verdict, ledger, note)
}

// TestUnsureOutcomeShowsTheEvaluatorsNote (gate 5 G5-B-BR-1): on an observed
// round the evaluator's note on an outcome is shown on the issue and on the
// pull request whatever its verdict, marked as its words, not checked by
// maul, and sanitised; a blank note shows none. An outcome the evaluator
// judged unsure is unsure (evaluator-unsure); one it judged to fail citing
// no call maul counts is unsure (no-valid-evidence), never failed. Display
// only: the verification, the hand-off and the status comment are the same
// with the note as without it.
func TestUnsureOutcomeShowsTheEvaluatorsNote(t *testing.T) {
	const note = "Does a version string of dev count as the commit? A person should judge @octocat #12"
	type seen struct {
		state    v1alpha1.IntentVerificationState
		outcomes string
		status   string
	}
	results := map[string]seen{}
	for _, tt := range []struct {
		name, verdict, note, reason string
		want                        string // "" for no note shown
	}{
		{"unsure with a note", "unsure", note, acceptance.ReasonEvaluatorUnsure, "The evaluator's note on " +
			"`version-shown` (its words, not checked by maul): Does a version string of dev count as the commit? " +
			"A person should judge `@octocat` `#12`."},
		{"unsure with a blank note", "unsure", "", acceptance.ReasonEvaluatorUnsure, ""},
		{"failed citing nothing, with a note", "fail", note, acceptance.ReasonNoValidEvidence,
			"The evaluator's note on `version-shown` (its words, not checked by maul): Does a version string"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e, name := newEvaluatorEnv(t, 0.4)
			e.evaluateReporting(unsureReport(tt.verdict, tt.note))
			e.review(name, 3)
			e.evalRound(name)
			e.review(name, 3)
			v := e.verification(name)
			i := slices.IndexFunc(v.Outcomes, func(r v1alpha1.OutcomeResult) bool { return r.ID == "version-shown" })
			if i < 0 || v.Outcomes[i].Result != v1alpha1.OutcomeUnsure {
				t.Fatalf("verification = %s %+v, want version-shown unsure", v.State, v.Outcomes)
			}
			if v.State != v1alpha1.VerificationUnconfirmed || v.Outcomes[i].Reason != tt.reason {
				t.Fatalf("verification = %s %+v, want Unconfirmed with version-shown %s", v.State, v.Outcomes,
					tt.reason)
			}
			if tt.verdict == "unsure" {
				outcomes := make([]string, 0, len(v.Outcomes))
				for _, r := range v.Outcomes {
					outcomes = append(outcomes, r.ID+" "+string(r.Result)+" "+r.Reason)
				}
				results[tt.name] = seen{v.State, strings.Join(outcomes, ", "), e.statusBody()}
			}
			cs := e.verifyComments(name, 1)
			if len(cs) != 1 {
				t.Fatalf("verification comments = %d, want one", len(cs))
			}
			pr := e.get(name).Status.PullRequests[0]
			for surface, body := range map[string]string{"issue": cs[0].Body,
				"pull request": e.onePreviewComment(name, pr.Number).Body} {
				shown := strings.Contains(body, "The evaluator's note on `version-shown`")
				switch {
				case tt.want == "" && shown:
					t.Errorf("%s shows a note:\n%s", surface, body)
				case tt.want != "" && !strings.Contains(body, tt.want):
					t.Errorf("%s lacks %q:\n%s", surface, tt.want, body)
				}
				if !strings.Contains(body, "HEALTHY-NOTE") {
					t.Errorf("%s does not show the passed outcome's note:\n%s", surface, body)
				}
			}
		})
	}
	with, without := results["unsure with a note"], results["unsure with a blank note"]
	if with.status == "" {
		t.Fatal("no status comment to compare")
	}
	if with != without {
		t.Errorf("showing the note changed the verdict or the status:\nwith    %+v\nwithout %+v", with, without)
	}
}
