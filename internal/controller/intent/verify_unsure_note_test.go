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
// question), still-healthy passed with a note of its own (HEALTHY-NOTE,
// never shown: maul confirmed it).
func unsureReport(verdict, note string) string {
	ledger := ""
	if verdict != "unsure" {
		ledger = "\n    ledger: [9]" // no such call: the claim shows none
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

// TestUnsureOutcomeShowsTheEvaluatorsNote (gate 5 G5-B-BR-1): an outcome
// the evaluator itself judged unsure, with a note, shows that note on the
// issue and on the pull request, marked as its unconfirmed words and
// sanitised; a blank note, an outcome it passed and maul confirmed, and
// one it judged to fail (shown as its claim instead) show none. Display
// only: the verification, the hand-off and the status comment are the
// same with the note as without it.
func TestUnsureOutcomeShowsTheEvaluatorsNote(t *testing.T) {
	const note = "Does a version string of dev count as the commit? A person should judge @octocat #12"
	type seen struct {
		state    v1alpha1.IntentVerificationState
		outcomes string
		status   string
	}
	results := map[string]seen{}
	for _, tt := range []struct {
		name, verdict, note string
		want                string // "" for no note shown
	}{
		{"unsure with a note", "unsure", note, "The evaluator was unsure of `version-shown` too; its note (its " +
			"words, not checked by maul): Does a version string of dev count as the commit? A person should " +
			"judge `@octocat` `#12`."},
		{"unsure with a blank note", "unsure", "", ""},
		{"failed with a note", "fail", note, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e, name := newEvaluatorEnv(t, 0.4)
			e.evaluateReporting(unsureReport(tt.verdict, tt.note))
			health := savedCheck(t, healthCheck)
			e.storeChecks(name, health)
			e.review(name, 3)
			pv := e.evalRound(name, v1alpha1.CheckRun{CheckDigest: health.Digest, Result: v1alpha1.CheckRunPass,
				Token: "t2", Seq: 3})
			e.ledgerOfEntries(pv.Name, checkRunEntry(3, health.Digest, "still-healthy", 0, "never shown"))
			e.review(name, 3)
			v := e.verification(name)
			i := slices.IndexFunc(v.Outcomes, func(r v1alpha1.OutcomeResult) bool { return r.ID == "version-shown" })
			if i < 0 || v.Outcomes[i].Result != v1alpha1.OutcomeUnsure {
				t.Fatalf("verification = %s %+v, want version-shown unsure", v.State, v.Outcomes)
			}
			reason := acceptance.ReasonNoCheck
			if tt.verdict == "fail" {
				reason = acceptance.ReasonModelOnlyFail
			}
			if v.State != v1alpha1.VerificationUnconfirmed || v.Outcomes[i].Reason != reason {
				t.Fatalf("verification = %s %+v, want Unconfirmed with version-shown %s", v.State, v.Outcomes, reason)
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
				shown := strings.Contains(body, "The evaluator was unsure of")
				switch {
				case tt.want == "" && shown:
					t.Errorf("%s shows a note:\n%s", surface, body)
				case tt.want != "" && !strings.Contains(body, tt.want):
					t.Errorf("%s lacks %q:\n%s", surface, tt.want, body)
				}
				if claim := strings.Contains(body, "The evaluator's claim on `version-shown`"); claim != (tt.verdict == "fail") {
					t.Errorf("%s shows the claim %v, want %v:\n%s", surface, claim, tt.verdict == "fail", body)
				}
				if strings.Contains(body, "HEALTHY-NOTE") || strings.Contains(body, "never shown") {
					t.Errorf("%s shows a confirmed outcome's note:\n%s", surface, body)
				}
			}
		})
	}
	with, without := results["unsure with a note"], results["unsure with a blank note"]
	if with != without {
		t.Errorf("showing the note changed the verdict or the status:\nwith    %+v\nwithout %+v", with, without)
	}
}
