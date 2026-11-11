// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package e2e

import (
	"strings"
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
)

// The cost ladder's replay rung on the agents placement (minimal S3): the
// tally fixture with a keep outcome its echo check proves at the head and
// the base, round 1 a full round that passes. Each test makes one request:
// requests are spaced verifyInterval apart.

// startAgentsLadder runs the agents stack on the tally fixture whose build
// keeps --echo working, its evaluator writing an echo check of its run's
// own token, and waits for round 1, a full round, to pass. It returns the
// env, the issue number, the Intent's name and the pull request.
func startAgentsLadder(t *testing.T) (*evalEnv, int, string, v1alpha1.IntentPullRequest) {
	t.Helper()
	e := startAgents(t, agentsOptions{
		files:    map[string]string{"go.mod": tallyGoMod, "main.go": tallyBase},
		surfaces: tallyCLI(), build: tallyGreets, outcomes: tallyKeepOutcome,
		env: map[string]string{"MAUL_FAKE_CLAUDE_EVALUATE": "cli-pass",
			"MAUL_FAKE_CLAUDE_CLI_ARGS": "--echo {{run}}", "MAUL_FAKE_CLAUDE_CHECK_TEXT": "{{run}}"},
		intentArgs: []string{"--intent-max-tool-pods", "1"},
	})
	number, name, pr := e.toReview(t)
	in := e.waitEval(t, name, "round 1 to pass", func(v *v1alpha1.IntentVerification) bool {
		return v.State == v1alpha1.VerificationPassed
	})
	if v := in.Status.Verification; v.Round != 1 || v.Rung != v1alpha1.RungFull || v.ModelRound == nil ||
		!v.ModelRound.Clear {
		t.Fatalf("round 1 = %+v, want a clear full round", v)
	}
	return e, number, name, pr
}

// rungOf is the rung of the intent's round's first attempt.
func (e *evalEnv) rungOf(t *testing.T, name string, round int32) v1alpha1.VerificationRung {
	t.Helper()
	return e.round(t, name, round).Spec.Rung
}

// TestIntentAgentsReplayPasses: an approver's /maul verify at unchanged
// heads after a clear model round is a replay round on the agents tool
// pod: the saved check replays at the head and the base, no evaluate and
// no checkreview run is created, the outcome passes on the replay, and the
// verification comment says no model ran.
func TestIntentAgentsReplayPasses(t *testing.T) {
	e, number, name, pr := startAgentsLadder(t)
	evaluates, reviews := len(e.stageRuns(t, name, v1alpha1.IntentStageEvaluate)),
		len(e.stageRuns(t, name, v1alpha1.IntentStageCheckReview))
	e.gh.CommentAs(number, "/maul verify", approver)
	in := e.waitEval(t, name, "round 2 to pass on the replay", func(v *v1alpha1.IntentVerification) bool {
		return v.Round == 2 && v.State == v1alpha1.VerificationPassed
	})
	if v := in.Status.Verification; v.Rung != v1alpha1.RungReplay || e.rungOf(t, name, 2) != v1alpha1.RungReplay {
		t.Errorf("round 2 rung %q (status) / %q (spec), want replay", v.Rung, e.rungOf(t, name, 2))
	}
	if n, m := len(e.stageRuns(t, name, v1alpha1.IntentStageEvaluate)),
		len(e.stageRuns(t, name, v1alpha1.IntentStageCheckReview)); n != evaluates || m != reviews {
		t.Errorf("evaluate runs %d->%d, checkreview runs %d->%d: a replay round runs no model", evaluates, n,
			reviews, m)
	}
	pv := e.round(t, name, 2)
	if len(pv.Status.Replay) == 0 || pv.Status.Replay[0].Result != v1alpha1.CheckRunPass ||
		pv.Status.Base == nil || len(pv.Status.Base.Runs) == 0 {
		t.Errorf("round 2 replay %+v base %+v, want the saved check replayed green at the head and run at the base",
			pv.Status.Replay, pv.Status.Base)
	}
	eventually(t, "round 2's comment to say no model ran", func() bool {
		return strings.Contains(strings.Join(e.gh.Comments(number), ""),
			"### maul: verification 2, attempt 1 (replayed saved checks, no model)")
	})
	e.waitStatus(t, pr.HeadSHA, "success", "1/1 outcomes confirmed")
}

// TestIntentAgentsExploreIsFull: /maul verify explore at the same heads
// runs the full rung, the evaluator launched, where a replay could have
// settled it.
func TestIntentAgentsExploreIsFull(t *testing.T) {
	e, number, name, _ := startAgentsLadder(t)
	e.gh.CommentAs(number, "/maul verify explore", approver)
	in := e.waitEval(t, name, "round 2 to pass", func(v *v1alpha1.IntentVerification) bool {
		return v.Round == 2 && v.State == v1alpha1.VerificationPassed
	})
	if v := in.Status.Verification; v.Rung != v1alpha1.RungFull || v.ModelRound == nil || v.ModelRound.Round != 2 {
		t.Errorf("round 2 rung %q, model round %+v; want full, the model round", v.Rung, v.ModelRound)
	}
	if n := len(e.stageRuns(t, name, v1alpha1.IntentStageEvaluate)); n != 2 {
		t.Errorf("evaluate runs = %d, want the explored round's evaluator too", n)
	}
	eventually(t, "the explore request answered", func() bool {
		return e.botSays(int64(number), "The evaluator looks again")
	})
}

// TestIntentAgentsReplayEscalatesToAFix: the head's program now prints
// something else at the same commits (the sandbox garbles its output). An
// approver's /maul verify fix runs a replay round: the saved check fails by
// its assertion, which no evaluator agrees with, so the outcome is unsure
// and never failed, and the round asks for one full round at the same
// heads. That round's evaluator sees the program fail and agrees: the
// outcome fails on the saved check, and the verify-fix round starts as it
// does today.
func TestIntentAgentsReplayEscalatesToAFix(t *testing.T) {
	e, number, name, _ := startAgentsLadder(t)
	e.kubelet.garbleHeadRuns(-1)
	e.kubelet.setAgentEnv("MAUL_FAKE_CLAUDE_EVALUATE", "cli-fail")
	e.requestRound(t, number)
	fix := e.waitFixRun(t, name)
	if fix.Spec.Inputs.VerificationRef == nil || fix.Spec.Inputs.VerificationRef.Name != e.round(t, name, 3).Name {
		t.Errorf("fix run inputs %+v, want the escalated round 3", fix.Spec.Inputs)
	}
	replay := e.round(t, name, 2)
	if replay.Spec.Rung != v1alpha1.RungReplay || len(replay.Status.Replay) == 0 ||
		replay.Status.Replay[0].Result != v1alpha1.CheckRunFail {
		t.Errorf("round 2 = %s, replay %+v; want a replay round whose saved check failed", replay.Spec.Rung,
			replay.Status.Replay)
	}
	escalated := e.round(t, name, 3)
	if escalated.Spec.Rung != v1alpha1.RungFull || escalated.Spec.HeadSetDigest != replay.Spec.HeadSetDigest {
		t.Errorf("round 3 = %s at %s, want the full escalation at round 2's heads", escalated.Spec.Rung,
			escalated.Spec.HeadSetDigest)
	}
	if n := len(e.stageRuns(t, name, v1alpha1.IntentStageEvaluate)); n != 2 {
		t.Errorf("evaluate runs = %d, want round 1's and the escalation's", n)
	}
	v := e.intent(t, name).Status.Verification
	if v.Escalation == nil || v.Escalation.Round != 2 {
		t.Errorf("escalation %+v, want round 2's", v.Escalation)
	}
	if v.Round != 3 || (v.State != v1alpha1.VerificationFailing && v.State != v1alpha1.VerificationFixing) {
		t.Errorf("verification round %d %s, want round 3 failing", v.Round, v.State)
	}
	if r := v.Outcomes; len(r) != 1 || r[0].Result != v1alpha1.OutcomeFailed ||
		r[0].Reason != acceptance.ReasonReproduced {
		t.Errorf("outcomes %+v, want failed, reproduced", r)
	}
}
