// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package e2e

import (
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// TestIntentAgentsAnomalyIsANote (owner decision of 2026-11-14): the
// evaluator answers whether the work is correct by the outcomes alone.
// Round 1's evaluator confirms the one outcome and reports a look-around
// anomaly: the round is Passed, maul/acceptance is success, and the note is
// shown on the issue for the reviewer. An old /maul accept anomalies gets
// the reply any id the plan does not name gets.
func TestIntentAgentsAnomalyIsANote(t *testing.T) {
	const anomaly = "Likely defect in main.go: --greet ignores a second argument"
	e := startAgents(t, agentsOptions{
		files:    map[string]string{"go.mod": tallyGoMod, "main.go": tallyBase},
		surfaces: tallyCLI(), build: tallyGreets, outcomes: tallyGreetOutcome,
		env: map[string]string{"MAUL_FAKE_CLAUDE_EVALUATE": "cli-pass", "MAUL_FAKE_CLAUDE_CLI_ARGS": "--greet",
			"MAUL_FAKE_CLAUDE_CHECK_TEXT": "hello from tally", "MAUL_FAKE_CLAUDE_ANOMALY": anomaly,
			// The anomaly's route: a round with no preview has no component path.
			"MAUL_FAKE_CLAUDE_CHECK_PATH": "main.go"},
	})
	number, name, pr := e.toReview(t)
	in := e.waitEval(t, name, "round 1 judged", func(v *v1alpha1.IntentVerification) bool {
		return v.Round == 1 && v.OutcomesRound == 1 && v.State != v1alpha1.VerificationEvaluating
	})
	v := in.Status.Verification
	if v.State != v1alpha1.VerificationPassed || len(v.Anomalies) != 1 || v.Anomalies[0].Summary != anomaly {
		t.Fatalf("round 1 = %s, anomalies %+v; want Passed with the note recorded", v.State, v.Anomalies)
	}
	e.waitStatus(t, pr.HeadSHA, "success", "1/1 outcomes seen to hold")
	eventually(t, "the verification comment to show the note", func() bool {
		return e.botSays(int64(number), anomaly)
	})

	e.gh.CommentAs(number, "/maul accept anomalies", approver)
	eventually(t, "the reply to /maul accept anomalies", func() bool {
		return e.botSays(int64(number), "`/maul accept anomalies` names no outcome of the approved plan.")
	})
}
