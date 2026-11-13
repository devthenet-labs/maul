// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package e2e

import (
	"encoding/json"
	"strings"
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// TestIntentAgentsFindingHoldsItsHeads (G-B-LIB-1, defect gate 2's two
// false passes): round 1's evaluator confirms the one outcome but reports
// a look-around anomaly, so the round is Unconfirmed and maul/acceptance is
// an error. An approver's /maul verify fix at the same heads starts round
// 2, whose evaluator now reports nothing. The anomaly is a fact about those
// commits: round 2's handoff hands it to the evaluator to examine again,
// round 2 is full and stays Unconfirmed with round 1's anomaly carried, and
// maul/acceptance never says success, until the approver accepts the
// anomaly with /maul accept anomalies, which accepts it and no outcome.
func TestIntentAgentsFindingHoldsItsHeads(t *testing.T) {
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
	in := e.waitEval(t, name, "round 1 judged on its anomaly", func(v *v1alpha1.IntentVerification) bool {
		return v.Round == 1 && v.State == v1alpha1.VerificationUnconfirmed && len(v.Anomalies) == 1
	})
	if r := in.Status.Verification.Outcomes; len(r) != 1 || r[0].Result != v1alpha1.OutcomePassed {
		t.Fatalf("round 1 outcomes %+v, want the outcome confirmed: only the anomaly holds the round", r)
	}
	e.waitStatus(t, pr.HeadSHA, "error", "it noticed something odd while it looked around")

	// Round 2 at the same heads: its evaluator raises nothing.
	e.kubelet.setAgentEnv("MAUL_FAKE_CLAUDE_ANOMALY", "")
	e.requestRound(t, number)
	in = e.waitEval(t, name, "round 2 judged", func(v *v1alpha1.IntentVerification) bool {
		return v.Round == 2 && v.OutcomesRound == 2 && v.State != v1alpha1.VerificationEvaluating
	})
	v := in.Status.Verification
	if v.State != v1alpha1.VerificationUnconfirmed || len(v.Anomalies) != 1 || v.Anomalies[0].Round != 1 ||
		v.Anomalies[0].Summary != anomaly {
		t.Fatalf("round 2 = %s, anomalies %+v; want Unconfirmed on round 1's anomaly, carried", v.State, v.Anomalies)
	}
	if v.Rung != v1alpha1.RungFull || v.ModelRound == nil || v.ModelRound.Round != 2 || v.ModelRound.Clear {
		t.Errorf("round 2 rung %s, model round %+v; want full, not clear", v.Rung, v.ModelRound)
	}
	evals := e.evaluatorJobs()
	if len(evals) != 2 {
		t.Fatalf("evaluate Jobs = %d, want both rounds'", len(evals))
	}
	var handoff struct {
		Held []struct {
			Round   int    `json:"round"`
			Summary string `json:"summary"`
		} `json:"held_anomalies"`
	}
	if err := json.Unmarshal(evals[1].Issue, &handoff); err != nil {
		t.Fatalf("round 2's evaluate handoff: %v", err)
	}
	if len(handoff.Held) != 1 || handoff.Held[0].Round != 1 || handoff.Held[0].Summary != anomaly {
		t.Errorf("round 2's evaluator was handed %+v, want round 1's anomaly", handoff.Held)
	}
	e.waitStatus(t, pr.HeadSHA, "error", "it noticed something odd while it looked around")
	eventually(t, "the verify fix reply to say nothing is fixed", func() bool {
		return e.botSays(int64(number), "No outcome fails at these commits, so there is nothing to fix yet")
	})

	e.gh.CommentAs(number, "/maul accept anomalies", approver)
	e.waitStatus(t, pr.HeadSHA, "success", "1/1 outcomes confirmed")
	in = e.intent(t, name)
	if a := in.Status.Verification.Anomalies; len(a) != 1 || a[0].Accepted == nil ||
		!strings.EqualFold(a[0].Accepted.By, approver.Login) {
		t.Errorf("anomalies %+v, want round 1's accepted by the approver", a)
	}
}
