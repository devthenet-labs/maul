// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/quick"
)

// dg is a fixed check digest for a short name, so table rows read.
func dg(name string) string {
	sum := sha256.Sum256([]byte(name))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// od is an outcome's digest, which reviews are keyed by.
func od(id string) string { return dg("outcome-" + id) }

// confirmedPass is one outcome bound to one check that clears every rule:
// stored in maul's form, valid, confirmed this round with a tools token,
// reviewed proves, the evaluator agreeing.
func confirmedPass() Input {
	return Input{
		Outcomes:      []OutcomeRef{{ID: "elo", Digest: od("elo")}},
		Checks:        []StoredCheck{{Digest: dg("a"), Outcome: "elo", Form: CheckFormBrowser, Valid: true}},
		Confirmations: []CheckRun{{CheckDigest: dg("a"), Result: RunPass, Token: "tok-a", Seq: 7}},
		Reviews:       []CheckReview{{CheckDigest: dg("a"), OutcomeDigest: od("elo"), Answer: ReviewProves}},
		Verdicts:      []EvaluatorVerdict{{Outcome: "elo", Claim: ClaimPass, Ledger: []int32{7}}},
	}
}

// deterministicFail is one outcome whose stored, reviewed check failed
// deterministically in this round's replay.
func deterministicFail(claim Claim, ledger ...int32) Input {
	in := confirmedPass()
	in.Confirmations = nil
	in.Replay = []CheckRun{{CheckDigest: dg("a"), Result: RunFail, Token: "tok-r", Seq: 3}}
	in.Verdicts = []EvaluatorVerdict{{Outcome: "elo", Claim: claim, Ledger: ledger}}
	return in
}

func with(in Input, f func(*Input)) Input { f(&in); return in }

func TestDecide(t *testing.T) {
	tests := []struct {
		name        string
		in          Input
		result      Result
		reason      string
		basis       string
		fix         bool
		replay      bool
		retire      []string
		handoff     Handoff
		env         EnvClass
		fixFailures []FixFailure
	}{
		{
			name:   "rules 1-6 hold: passed",
			in:     confirmedPass(),
			result: ResultPassed, reason: ReasonConfirmed, basis: dg("a"), handoff: HandoffReady,
		},
		{
			name: "a replay pass is this round's tools run too",
			in: with(confirmedPass(), func(in *Input) {
				in.Replay, in.Confirmations = in.Confirmations, nil
			}),
			result: ResultPassed, reason: ReasonConfirmed, basis: dg("a"), handoff: HandoffReady,
		},
		{
			name: "rule 1: a run of a check that is not stored supports nothing",
			in: with(confirmedPass(), func(in *Input) {
				in.Checks = nil
			}),
			result: ResultUnsure, reason: ReasonModelOnlyPass, handoff: HandoffUnconfirmed,
		},
		{
			name: "rule 1: a check retired in an earlier round supports nothing",
			in: with(confirmedPass(), func(in *Input) {
				in.Checks[0].Retired = true
			}),
			result: ResultUnsure, reason: ReasonModelOnlyPass, handoff: HandoffUnconfirmed,
		},
		{
			name: "rule 1 and 7.5: a model-judged screenshot is not one of maul's forms",
			in: with(confirmedPass(), func(in *Input) {
				in.Checks[0].Form = "judged"
			}),
			result: ResultUnsure, reason: ReasonModelOnlyPass, handoff: HandoffUnconfirmed,
		},
		{
			name: "rule 2: a check the grammar did not admit supports nothing",
			in: with(confirmedPass(), func(in *Input) {
				in.Checks[0].Valid = false
			}),
			result: ResultUnsure, reason: ReasonModelOnlyPass, handoff: HandoffUnconfirmed,
		},
		{
			name: "rule 3: a pass with no tools run token is not a confirmation",
			in: with(confirmedPass(), func(in *Input) {
				in.Confirmations[0].Token = ""
			}),
			result: ResultUnsure, reason: ReasonModelOnlyPass, handoff: HandoffUnconfirmed,
		},
		{
			name: "rule 3: no run this round",
			in: with(confirmedPass(), func(in *Input) {
				in.Confirmations = nil
			}),
			result: ResultUnsure, reason: ReasonModelOnlyPass, handoff: HandoffUnconfirmed,
		},
		{
			name: "rule 3: an errored run is unsure, never a fail",
			in: with(confirmedPass(), func(in *Input) {
				in.Confirmations[0].Result = RunError
			}),
			result: ResultUnsure, reason: ReasonModelOnlyPass, handoff: HandoffUnconfirmed,
		},
		{
			name: "rule 3: a refused run is not a confirmation",
			in: with(confirmedPass(), func(in *Input) {
				in.Confirmations[0].Result = RunRefused
			}),
			result: ResultUnsure, reason: ReasonModelOnlyPass, handoff: HandoffUnconfirmed,
		},
		{
			name: "rule 4: reviewed does_not_prove",
			in: with(confirmedPass(), func(in *Input) {
				in.Reviews[0].Answer = ReviewDoesNotProve
			}),
			result: ResultUnsure, reason: ReasonModelOnlyPass, handoff: HandoffUnconfirmed,
		},
		{
			name: "rule 4: reviewed unclear",
			in: with(confirmedPass(), func(in *Input) {
				in.Reviews[0].Answer = ReviewUnclear
			}),
			result: ResultUnsure, reason: ReasonModelOnlyPass, handoff: HandoffUnconfirmed,
		},
		{
			name: "rule 4: not reviewed",
			in: with(confirmedPass(), func(in *Input) {
				in.Reviews = nil
			}),
			result: ResultUnsure, reason: ReasonModelOnlyPass, handoff: HandoffUnconfirmed,
		},
		{
			name: "rule 4: a review of another outcome text does not count",
			in: with(confirmedPass(), func(in *Input) {
				in.Reviews[0].OutcomeDigest = od("other")
			}),
			result: ResultUnsure, reason: ReasonModelOnlyPass, handoff: HandoffUnconfirmed,
		},
		{
			name: "rule 4: conflicting cached answers do not count",
			in: with(confirmedPass(), func(in *Input) {
				in.Reviews = append(in.Reviews, CheckReview{CheckDigest: dg("a"), OutcomeDigest: od("elo"), Answer: ReviewUnclear})
			}),
			result: ResultUnsure, reason: ReasonModelOnlyPass, handoff: HandoffUnconfirmed,
		},
		{
			name: "rule 5: a change outcome's check failing at base discriminates",
			in: with(confirmedPass(), func(in *Input) {
				in.Base = &BaseRuns{Runs: []CheckRun{{CheckDigest: dg("a"), Result: RunFail, Token: "b", Seq: 1}}}
			}),
			result: ResultPassed, reason: ReasonConfirmed, basis: dg("a"), handoff: HandoffReady,
		},
		{
			name: "rule 5: a change outcome's check passing at base does not",
			in: with(confirmedPass(), func(in *Input) {
				in.Base = &BaseRuns{Runs: []CheckRun{{CheckDigest: dg("a"), Result: RunPass, Token: "b", Seq: 1}}}
			}),
			result: ResultUnsure, reason: ReasonModelOnlyPass, handoff: HandoffUnconfirmed,
		},
		{
			name: "rule 5: a keep outcome's check must pass at base",
			in: with(confirmedPass(), func(in *Input) {
				in.Outcomes[0].Keep = true
				in.Base = &BaseRuns{Runs: []CheckRun{{CheckDigest: dg("a"), Result: RunFail, Token: "b", Seq: 1}}}
			}),
			result: ResultUnsure, reason: ReasonModelOnlyPass, handoff: HandoffUnconfirmed,
		},
		{
			name: "rule 5: a keep outcome's check passing at base",
			in: with(confirmedPass(), func(in *Input) {
				in.Outcomes[0].Keep = true
				in.Base = &BaseRuns{Runs: []CheckRun{{CheckDigest: dg("a"), Result: RunPass, Token: "b", Seq: 1}}}
			}),
			result: ResultPassed, reason: ReasonConfirmed, basis: dg("a"), handoff: HandoffReady,
		},
		{
			name: "rule 5: quarantined at base does not discriminate",
			in: with(confirmedPass(), func(in *Input) {
				in.Base = &BaseRuns{Runs: []CheckRun{{CheckDigest: dg("a"), Result: RunQuarantined, Token: "b", Seq: 1}}}
			}),
			result: ResultUnsure, reason: ReasonModelOnlyPass, handoff: HandoffUnconfirmed,
		},
		{
			name: "rule 5: a check that cannot run at base is n/a",
			in: with(confirmedPass(), func(in *Input) {
				in.Base = &BaseRuns{Runs: []CheckRun{{CheckDigest: dg("a"), Result: RunError, Token: "b", Seq: 1}}}
			}),
			result: ResultPassed, reason: ReasonConfirmed, basis: dg("a"), handoff: HandoffReady,
		},
		{
			name: "rule 5: no base run is n/a",
			in: with(confirmedPass(), func(in *Input) {
				in.Base = &BaseRuns{}
			}),
			result: ResultPassed, reason: ReasonConfirmed, basis: dg("a"), handoff: HandoffReady,
		},
		{
			name: "rule 6: a failing sibling blocks the pass",
			in: with(confirmedPass(), func(in *Input) {
				in.Checks = append(in.Checks, StoredCheck{Digest: dg("b"), Outcome: "elo", Form: CheckFormHTTP, Valid: true})
				in.Replay = []CheckRun{{CheckDigest: dg("b"), Result: RunFail, Token: "t", Seq: 2}}
			}),
			result: ResultUnsure, reason: ReasonChecksDisagree, handoff: HandoffUnconfirmed,
		},
		{
			name: "rule 6: a failing sibling that is not valid still blocks the pass",
			in: with(confirmedPass(), func(in *Input) {
				in.Checks = append(in.Checks, StoredCheck{Digest: dg("b"), Outcome: "elo", Form: CheckFormHTTP})
				in.Replay = []CheckRun{{CheckDigest: dg("b"), Result: RunFail, Token: "t", Seq: 2}}
			}),
			result: ResultUnsure, reason: ReasonChecksDisagree, handoff: HandoffUnconfirmed,
		},
		{
			name: "rule 6: a quarantined sibling does not block",
			in: with(confirmedPass(), func(in *Input) {
				in.Checks = append(in.Checks, StoredCheck{Digest: dg("b"), Outcome: "elo", Form: CheckFormHTTP, Valid: true})
				in.Replay = []CheckRun{{CheckDigest: dg("b"), Result: RunQuarantined, Token: "t", Seq: 2}}
			}),
			result: ResultPassed, reason: ReasonConfirmed, basis: dg("a"), handoff: HandoffReady,
		},
		{
			name: "rule 6: a sibling retired in an earlier round does not block",
			in: with(confirmedPass(), func(in *Input) {
				in.Checks = append(in.Checks,
					StoredCheck{Digest: dg("b"), Outcome: "elo", Form: CheckFormHTTP, Valid: true, Retired: true})
				in.Replay = []CheckRun{{CheckDigest: dg("b"), Result: RunFail, Token: "t", Seq: 2}}
			}),
			result: ResultPassed, reason: ReasonConfirmed, basis: dg("a"), handoff: HandoffReady,
		},
		{
			name: "a check failing in one run and passing in another is no pass",
			in: with(confirmedPass(), func(in *Input) {
				in.Replay = []CheckRun{{CheckDigest: dg("a"), Result: RunFail, Token: "t", Seq: 2}}
				in.Verdicts[0].Claim = ClaimUnsure
			}),
			result: ResultUnsure, reason: ReasonEvaluatorDisagrees, handoff: HandoffUnconfirmed,
		},
		{
			name:   "deterministic failure and the evaluator's fail citing the ledger: failed with a fix",
			in:     deterministicFail(ClaimFail, 3, 4),
			result: ResultFailed, reason: ReasonReproduced, basis: dg("a"), fix: true, handoff: HandoffUnconfirmed,
			fixFailures: []FixFailure{{Outcome: "elo", CheckDigest: dg("a"), Seq: 3}},
		},
		{
			name:   "the evaluator's fail citing nothing is no agreement",
			in:     deterministicFail(ClaimFail),
			result: ResultUnsure, reason: ReasonEvaluatorDisagrees, handoff: HandoffUnconfirmed,
		},
		{
			name:   "the evaluator says pass over a deterministic failure (no_defect disagreement)",
			in:     deterministicFail(ClaimPass, 3),
			result: ResultUnsure, reason: ReasonEvaluatorDisagrees, handoff: HandoffUnconfirmed,
		},
		{
			name:   "the evaluator says unsure over a deterministic failure",
			in:     deterministicFail(ClaimUnsure),
			result: ResultUnsure, reason: ReasonEvaluatorDisagrees, handoff: HandoffUnconfirmed,
		},
		{
			name: "no verdict for the outcome over a deterministic failure",
			in: with(deterministicFail(ClaimFail, 3), func(in *Input) {
				in.Verdicts = nil
			}),
			result: ResultUnsure, reason: ReasonEvaluatorDisagrees, handoff: HandoffUnconfirmed,
		},
		{
			name: "a failing check not reviewed proves is no reproduction",
			in: with(deterministicFail(ClaimFail, 3), func(in *Input) {
				in.Reviews = nil
			}),
			result: ResultUnsure, reason: ReasonEvaluatorDisagrees, handoff: HandoffUnconfirmed,
		},
		{
			name: "a failing check the grammar did not admit is no reproduction",
			in: with(deterministicFail(ClaimFail, 3), func(in *Input) {
				in.Checks[0].Valid = false
			}),
			result: ResultUnsure, reason: ReasonEvaluatorDisagrees, handoff: HandoffUnconfirmed,
		},
		{
			name: "a disputed failure is unsure, never a fix, even if the verdict says fail",
			in: with(deterministicFail(ClaimFail, 3), func(in *Input) {
				in.Disputes = []CheckDispute{{CheckDigest: dg("a"), Ledger: []int32{3}}}
			}),
			result: ResultUnsure, reason: ReasonDisputed, handoff: HandoffUnconfirmed,
		},
		{
			name: "a dispute with a confirmed, reviewed replacement schedules one replay round",
			in: with(deterministicFail(ClaimPass, 3, 9), func(in *Input) {
				in.Disputes = []CheckDispute{{CheckDigest: dg("a"), Replacement: dg("r"), Ledger: []int32{3}}}
				in.Checks = append(in.Checks, StoredCheck{Digest: dg("r"), Outcome: "elo", Form: CheckFormBrowser, Valid: true})
				in.Confirmations = []CheckRun{{CheckDigest: dg("r"), Result: RunPass, Token: "tok-c", Seq: 9}}
				in.Reviews = append(in.Reviews, CheckReview{CheckDigest: dg("r"), OutcomeDigest: od("elo"), Answer: ReviewProves})
			}),
			result: ResultUnsure, reason: ReasonDisputed, replay: true, retire: []string{dg("a")}, handoff: HandoffUnconfirmed,
		},
		{
			name: "a dispute whose replacement is not reviewed schedules nothing",
			in: with(deterministicFail(ClaimPass, 3, 9), func(in *Input) {
				in.Disputes = []CheckDispute{{CheckDigest: dg("a"), Replacement: dg("r"), Ledger: []int32{3}}}
				in.Checks = append(in.Checks, StoredCheck{Digest: dg("r"), Outcome: "elo", Form: CheckFormBrowser, Valid: true})
				in.Confirmations = []CheckRun{{CheckDigest: dg("r"), Result: RunPass, Token: "tok-c", Seq: 9}}
			}),
			result: ResultUnsure, reason: ReasonDisputed, handoff: HandoffUnconfirmed,
		},
		{
			name: "a dispute whose replacement failed its confirmation schedules nothing",
			in: with(deterministicFail(ClaimPass, 3, 9), func(in *Input) {
				in.Disputes = []CheckDispute{{CheckDigest: dg("a"), Replacement: dg("r"), Ledger: []int32{3}}}
				in.Checks = append(in.Checks, StoredCheck{Digest: dg("r"), Outcome: "elo", Form: CheckFormBrowser, Valid: true})
				in.Confirmations = []CheckRun{{CheckDigest: dg("r"), Result: RunQuarantined, Token: "tok-c", Seq: 9}}
				in.Reviews = append(in.Reviews, CheckReview{CheckDigest: dg("r"), OutcomeDigest: od("elo"), Answer: ReviewProves})
			}),
			result: ResultUnsure, reason: ReasonDisputed, handoff: HandoffUnconfirmed,
		},
		{
			name: "a replacement bound to another outcome schedules nothing",
			in: with(deterministicFail(ClaimPass, 3, 9), func(in *Input) {
				in.Outcomes = append(in.Outcomes, OutcomeRef{ID: "rank", Digest: od("rank")})
				in.Disputes = []CheckDispute{{CheckDigest: dg("a"), Replacement: dg("r"), Ledger: []int32{3}}}
				in.Checks = append(in.Checks, StoredCheck{Digest: dg("r"), Outcome: "rank", Form: CheckFormBrowser, Valid: true})
				in.Confirmations = []CheckRun{{CheckDigest: dg("r"), Result: RunPass, Token: "tok-c", Seq: 9}}
				in.Reviews = append(in.Reviews, CheckReview{CheckDigest: dg("r"), OutcomeDigest: od("rank"), Answer: ReviewProves})
			}),
			result: ResultUnsure, reason: ReasonDisputed, handoff: HandoffUnconfirmed,
		},
		{
			name: "the replay round after a dispute: the disputed check retired, the replacement passes",
			in: Input{
				Outcomes: []OutcomeRef{{ID: "elo", Digest: od("elo")}},
				Checks: []StoredCheck{
					{Digest: dg("a"), Outcome: "elo", Form: CheckFormHTTP, Valid: true, Retired: true},
					{Digest: dg("r"), Outcome: "elo", Form: CheckFormBrowser, Valid: true},
				},
				Replay:  []CheckRun{{CheckDigest: dg("r"), Result: RunPass, Token: "tok-x", Seq: 1}},
				Reviews: []CheckReview{{CheckDigest: dg("r"), OutcomeDigest: od("elo"), Answer: ReviewProves}},
			},
			result: ResultPassed, reason: ReasonConfirmed, basis: dg("r"), handoff: HandoffReady,
		},
		{
			name: "a dispute of a check that did not fail changes nothing",
			in: with(confirmedPass(), func(in *Input) {
				in.Disputes = []CheckDispute{{CheckDigest: dg("a"), Ledger: []int32{7}}}
			}),
			result: ResultPassed, reason: ReasonConfirmed, basis: dg("a"), handoff: HandoffReady,
		},
		{
			name: "model says fail, no failing check",
			in: with(confirmedPass(), func(in *Input) {
				in.Checks, in.Confirmations, in.Reviews = nil, nil, nil
				in.Verdicts[0].Claim = ClaimFail
			}),
			result: ResultUnsure, reason: ReasonModelOnlyFail, handoff: HandoffUnconfirmed,
		},
		{
			name:   "nothing at all",
			in:     Input{Outcomes: []OutcomeRef{{ID: "elo", Digest: od("elo")}}},
			result: ResultUnsure, reason: ReasonNoCheck, handoff: HandoffUnconfirmed,
		},
		{
			name: "a check retired this round makes its outcome unsure though another passes",
			in: with(confirmedPass(), func(in *Input) {
				in.Checks = append(in.Checks,
					StoredCheck{Digest: dg("old"), Outcome: "elo", Form: CheckFormHTTP, Valid: true, Retired: true})
				in.Retired = []Retirement{{CheckDigest: dg("old"), Reason: "wrong path"}}
			}),
			result: ResultUnsure, reason: ReasonRetired, handoff: HandoffUnconfirmed,
		},
		{
			name: "only support is a quarantined check",
			in: with(confirmedPass(), func(in *Input) {
				in.Confirmations[0].Result = RunQuarantined
			}),
			result: ResultQuarantined, reason: ReasonQuarantined, handoff: HandoffUnconfirmed,
		},
		{
			name: "an application failure in the window: no fix, unsure",
			in: with(deterministicFail(ClaimFail, 3), func(in *Input) {
				in.Env = []PartFact{{Name: "api", Role: RoleComponent, Ready: true, CrashedDuring: true}}
			}),
			result: ResultUnsure, reason: ReasonEnvFailed, env: EnvAppFailure, handoff: HandoffUnconfirmed,
		},
		{
			name: "infrastructure trouble in the window: no fix, unsure",
			in: with(deterministicFail(ClaimFail, 3), func(in *Input) {
				in.Env = []PartFact{{Name: "api", Role: RoleComponent, Unschedulable: true}}
			}),
			result: ResultUnsure, reason: ReasonInfra, env: EnvInfra, handoff: HandoffUnconfirmed,
		},
		{
			name: "an environment failure leaves a confirmed pass but downgrades the hand-off",
			in: with(confirmedPass(), func(in *Input) {
				in.Env = []PartFact{{Name: "api", Role: RoleComponent, Ready: true, CrashedDuring: true}}
			}),
			result: ResultPassed, reason: ReasonConfirmed, basis: dg("a"), env: EnvAppFailure, handoff: HandoffUnconfirmed,
		},
		{
			name: "an anomaly downgrades the hand-off of an all-passing round",
			in: with(confirmedPass(), func(in *Input) {
				in.Anomalies = []AnomalyRef{{Summary: "500 on /admin", Route: "/admin", Ledger: []int32{12}}}
			}),
			result: ResultPassed, reason: ReasonConfirmed, basis: dg("a"), handoff: HandoffUnconfirmed,
		},
		{
			name: "an anomaly never starts a fix",
			in: with(confirmedPass(), func(in *Input) {
				in.Confirmations = nil
				in.Verdicts[0].Claim = ClaimFail
				in.Anomalies = []AnomalyRef{{Summary: "panic", Route: "api", Ledger: []int32{12}}}
			}),
			result: ResultUnsure, reason: ReasonModelOnlyFail, handoff: HandoffUnconfirmed,
		},
		{
			name: "a manual outcome is for a human",
			in: with(confirmedPass(), func(in *Input) {
				in.Outcomes[0].Manual = true
			}),
			result: ResultUnsure, reason: ReasonManual, handoff: HandoffUnconfirmed,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := Decide(tt.in)
			if len(d.PerOutcome) != len(tt.in.Outcomes) {
				t.Fatalf("PerOutcome has %d entries, want %d", len(d.PerOutcome), len(tt.in.Outcomes))
			}
			got := d.PerOutcome[0]
			if got.ID != tt.in.Outcomes[0].ID || got.Result != tt.result || got.Reason != tt.reason || got.Basis != tt.basis {
				t.Errorf("outcome = %+v, want result %s reason %s basis %q", got, tt.result, tt.reason, tt.basis)
			}
			if (d.Fix != nil) != tt.fix {
				t.Errorf("Fix = %+v, want present %v", d.Fix, tt.fix)
			}
			if tt.fixFailures != nil && (d.Fix == nil || !reflect.DeepEqual(d.Fix.Failures, tt.fixFailures)) {
				t.Errorf("Fix = %+v, want failures %+v", d.Fix, tt.fixFailures)
			}
			if d.ReplayRound != tt.replay || !slices.Equal(d.Retire, tt.retire) {
				t.Errorf("ReplayRound %v Retire %v, want %v %v", d.ReplayRound, d.Retire, tt.replay, tt.retire)
			}
			wantEnv := tt.env
			if wantEnv == "" {
				wantEnv = EnvOK
			}
			if d.Env != wantEnv || d.Handoff != tt.handoff {
				t.Errorf("Env %s Handoff %s, want %s %s", d.Env, d.Handoff, wantEnv, tt.handoff)
			}
		})
	}
}

func TestDecideHandoffNeedsEveryOutcome(t *testing.T) {
	in := confirmedPass()
	in.Outcomes = append(in.Outcomes, OutcomeRef{ID: "rank", Digest: od("rank")})
	d := Decide(in)
	if d.Handoff != HandoffUnconfirmed {
		t.Errorf("Handoff = %s with one outcome unconfirmed, want unconfirmed", d.Handoff)
	}
	if d.PerOutcome[0].Result != ResultPassed || d.PerOutcome[1].Result != ResultUnsure {
		t.Errorf("PerOutcome = %+v", d.PerOutcome)
	}
	if got := Decide(Input{}).Handoff; got != HandoffUnconfirmed {
		t.Errorf("no outcomes: Handoff = %s, want unconfirmed", got)
	}
}

func TestDecideFixFingerprint(t *testing.T) {
	a := Decide(deterministicFail(ClaimFail, 3))
	b := Decide(with(deterministicFail(ClaimFail, 4), func(in *Input) {
		in.Replay[0].Seq, in.Replay[0].Token = 11, "another-token"
	}))
	if a.Fix == nil || b.Fix == nil {
		t.Fatal("want a fix in both")
	}
	if a.Fix.Fingerprint != b.Fix.Fingerprint {
		t.Errorf("fingerprint depends on seq or token: %s != %s", a.Fix.Fingerprint, b.Fix.Fingerprint)
	}
	if !strings.HasPrefix(a.Fix.Fingerprint, EvaluationFingerprintVersion+":") {
		t.Errorf("fingerprint %q lacks its version", a.Fix.Fingerprint)
	}
	want := EvaluationFingerprint([]string{"elo"}, []string{dg("a")}, nil)
	if a.Fix.Fingerprint != want {
		t.Errorf("fingerprint = %s, want EvaluationFingerprint's %s", a.Fix.Fingerprint, want)
	}
}

// TestDecideIssue44 replays #44: three deterministic failures of saved
// checks on a correct change, the evaluator disputing each (stale shared
// data, a start-up race, a wrong check). It must start no fix.
func TestDecideIssue44(t *testing.T) {
	in := Input{}
	for i, id := range []string{"rate", "rank", "history"} {
		c := dg("44-" + id)
		in.Outcomes = append(in.Outcomes, OutcomeRef{ID: id, Digest: od(id)})
		in.Checks = append(in.Checks, StoredCheck{Digest: c, Outcome: id, Form: CheckFormHTTP, Valid: true})
		in.Replay = append(in.Replay, CheckRun{CheckDigest: c, Result: RunFail, Token: "t" + id, Seq: int32(i)})
		in.Reviews = append(in.Reviews, CheckReview{CheckDigest: c, OutcomeDigest: od(id), Answer: ReviewProves})
		in.Verdicts = append(in.Verdicts, EvaluatorVerdict{Outcome: id, Claim: ClaimPass, Ledger: []int32{int32(10 + i)}})
		in.Disputes = append(in.Disputes, CheckDispute{CheckDigest: c, Reason: "stale data", Ledger: []int32{int32(i)}})
	}
	for _, claim := range []Claim{ClaimPass, ClaimUnsure, ClaimFail} {
		for i := range in.Verdicts {
			in.Verdicts[i].Claim = claim
		}
		d := Decide(in)
		if d.Fix != nil {
			t.Fatalf("verdicts %s: #44 started a fix: %+v", claim, d.Fix)
		}
		for _, r := range d.PerOutcome {
			if r.Result != ResultUnsure || r.Reason != ReasonDisputed {
				t.Errorf("verdicts %s: %s = %s (%s), want unsure (disputed)", claim, r.ID, r.Result, r.Reason)
			}
		}
	}
}

// genDecide is a random Decide input over small pools of outcomes,
// digests, results and answers, so every rule is hit both ways.
type genDecide struct{ in Input }

var (
	genOutcomes = []string{"o1", "o2", "o3"}
	genDigests  = []string{dg("c1"), dg("c2"), dg("c3"), dg("c4"), dg("c5")}
	genResults  = []RunResult{RunPass, RunPass, RunFail, RunError, RunQuarantined, RunRefused}
	genAnswers  = []ReviewAnswer{ReviewProves, ReviewProves, ReviewDoesNotProve, ReviewUnclear}
	// genAssertions are the assertion ids runs fail by and red checks are
	// saved failing.
	genAssertions = []string{"json /a", "json /a", "header x"}
)

func pickOf[T any](r *rand.Rand, xs []T) T { return xs[r.Intn(len(xs))] }

func (genDecide) Generate(r *rand.Rand, _ int) reflect.Value {
	var in Input
	for _, id := range genOutcomes[:1+r.Intn(len(genOutcomes))] {
		o := OutcomeRef{ID: id, Digest: od(id), Keep: r.Intn(3) == 0, Manual: r.Intn(8) == 0}
		if r.Intn(10) == 0 {
			o.Digest = ""
		}
		in.Outcomes = append(in.Outcomes, o)
	}
	forms := []CheckForm{CheckFormHTTP, CheckFormBrowser, CheckFormExec, "judged"}
	for _, c := range genDigests {
		if r.Intn(4) != 0 {
			in.Checks = append(in.Checks, StoredCheck{Digest: c, Outcome: pickOf(r, genOutcomes), Form: pickOf(r, forms),
				Valid: r.Intn(5) != 0, Retired: r.Intn(6) == 0, ExpectFail: r.Intn(4) == 0,
				FailedStep: int32(1 + r.Intn(2)), FailedAssertion: pickOf(r, genAssertions), Cleared: r.Intn(3) == 0})
		}
	}
	in.Replay, in.Confirmations = genRuns(r), genRuns(r)
	if r.Intn(2) == 0 {
		in.Base = &BaseRuns{Runs: genRuns(r)}
	}
	genBiased(r, &in)
	for range r.Intn(4) {
		in.Reviews = append(in.Reviews, CheckReview{CheckDigest: pickOf(r, genDigests),
			OutcomeDigest: od(pickOf(r, genOutcomes)), Answer: pickOf(r, genAnswers), ExpectFail: r.Intn(3) == 0})
	}
	genClaims(r, &in)
	for range r.Intn(2) {
		in.Retired = append(in.Retired, Retirement{CheckDigest: pickOf(r, genDigests), Reason: "r"})
	}
	if r.Intn(6) == 0 {
		in.Anomalies = []AnomalyRef{{Summary: "s", Route: "/", Ledger: []int32{1}}}
	}
	switch r.Intn(6) {
	case 0:
		in.Env = []PartFact{{Name: "api", Role: RoleComponent, Ready: true, CrashedDuring: true}}
	case 1:
		in.Env = []PartFact{{Name: "api", Role: RoleComponent, Unschedulable: true}}
	}
	return reflect.ValueOf(genDecide{in})
}

// genRuns is up to four runs of any digest, any result, with or without a
// token.
func genRuns(r *rand.Rand) []CheckRun {
	n := r.Intn(5)
	out := make([]CheckRun, 0, n)
	for range n {
		out = append(out, CheckRun{CheckDigest: pickOf(r, genDigests), Result: pickOf(r, genResults),
			Token: pickOf(r, []string{"", "tok"}), Seq: int32(r.Intn(20)), AssertionStep: int32(r.Intn(3)),
			AssertionID: pickOf(r, genAssertions)})
	}
	return out
}

// genBiased biases towards the paths that matter: most stored checks get a
// review of their own outcome and a run, and a dispute often names a stored
// replacement, so passes, fixes and replay rounds happen often enough to be
// judged.
func genBiased(r *rand.Rand, in *Input) {
	for _, c := range in.Checks {
		if r.Intn(4) != 0 {
			in.Reviews = append(in.Reviews, CheckReview{CheckDigest: c.Digest, OutcomeDigest: od(c.Outcome),
				Answer: pickOf(r, genAnswers), ExpectFail: c.ExpectFail && r.Intn(4) != 0, FailedStep: c.FailedStep,
				FailedAssertion: c.FailedAssertion})
		}
		switch {
		case c.ExpectFail && r.Intn(2) == 0:
			// Its confirmation, this round: mostly red by assertion.
			in.Confirmations = append(in.Confirmations, CheckRun{CheckDigest: c.Digest,
				Result: pickOf(r, []RunResult{RunFail, RunFail, RunPass, RunError, RunQuarantined}), Token: "tok",
				Seq: int32(r.Intn(20)), AssertionStep: int32(r.Intn(3)), AssertionID: pickOf(r, genAssertions),
				ExpectFail: r.Intn(4) != 0})
		case r.Intn(3) != 0:
			in.Replay = append(in.Replay, CheckRun{CheckDigest: c.Digest, Result: pickOf(r, genResults),
				Token: "tok", Seq: int32(r.Intn(20)), AssertionStep: int32(r.Intn(3)),
				AssertionID: pickOf(r, genAssertions)})
		}
	}
	if len(in.Checks) > 1 && r.Intn(2) == 0 {
		// A replacement is proposed green, for the disputed check's outcome.
		in.Checks[1].ExpectFail = false
		if r.Intn(2) == 0 {
			in.Checks[1].Outcome = in.Checks[0].Outcome
		}
		in.Disputes = append(in.Disputes, CheckDispute{CheckDigest: in.Checks[0].Digest,
			Replacement: in.Checks[1].Digest, Ledger: []int32{1}})
	}
	for range r.Intn(3) {
		d := CheckDispute{CheckDigest: pickOf(r, genDigests), Ledger: []int32{1}}
		if r.Intn(2) == 0 {
			d.Replacement = pickOf(r, genDigests)
		}
		in.Disputes = append(in.Disputes, d)
	}
}

// genClaims gives most outcomes a verdict, most of them cited.
func genClaims(r *rand.Rand, in *Input) {
	for _, o := range in.Outcomes {
		if r.Intn(5) == 0 {
			continue
		}
		v := EvaluatorVerdict{Outcome: o.ID, Claim: pickOf(r, []Claim{ClaimPass, ClaimFail, ClaimUnsure})}
		if r.Intn(4) != 0 {
			v.Ledger = []int32{int32(r.Intn(20))}
		}
		in.Verdicts = append(in.Verdicts, v)
	}
}

// oracleRuns is every run of digest this round, replay and confirmation.
func oracleRuns(in Input, digest string) []CheckRun {
	var out []CheckRun
	for _, run := range append(slices.Clone(in.Replay), in.Confirmations...) {
		if run.CheckDigest == digest {
			out = append(out, run)
		}
	}
	return out
}

func oracleFailed(in Input, digest string) bool {
	return slices.ContainsFunc(oracleRuns(in, digest), func(run CheckRun) bool { return run.Result == RunFail })
}

func oracleStored(in Input, digest, outcome string) (StoredCheck, bool) {
	for _, c := range in.Checks {
		if c.Digest == digest && c.Outcome == outcome {
			return c, true
		}
	}
	return StoredCheck{}, false
}

func oracleProves(in Input, digest string, o OutcomeRef) bool {
	n := 0
	for _, rv := range in.Reviews {
		if rv.CheckDigest == digest && rv.OutcomeDigest == o.Digest {
			if rv.Answer != ReviewProves {
				return false
			}
			n++
		}
	}
	return o.Digest != "" && n > 0
}

// oracleSixRules restates DESIGN 4.3 independently of decide.go: does the
// basis check, bound to o, clear rules 1-6 this round, with the outcome
// neither manual, retired from nor disputed this round?
func oracleSixRules(in Input, o OutcomeRef, basis string) error {
	for _, rule := range []func(Input, OutcomeRef, string) error{oracleRules1To4, oracleRule5, oracleRule6, oracleRound} {
		if err := rule(in, o, basis); err != nil {
			return err
		}
	}
	return nil
}

func oracleRules1To4(in Input, o OutcomeRef, basis string) error {
	c, ok := oracleStored(in, basis, o.ID)
	if !ok || c.Retired || slices.ContainsFunc(in.Retired, func(r Retirement) bool { return r.CheckDigest == basis }) {
		return fmt.Errorf("rule 1: basis %s is not a stored, unretired check of %s", basis, o.ID)
	}
	if c.Form != CheckFormHTTP && c.Form != CheckFormBrowser && c.Form != CheckFormExec {
		return fmt.Errorf("rule 1: form %q", c.Form)
	}
	if !c.Valid {
		return fmt.Errorf("rule 2: basis not valid")
	}
	runs := oracleRuns(in, basis)
	if len(runs) == 0 {
		return fmt.Errorf("rule 3: no run")
	}
	for _, run := range runs {
		if run.Result != RunPass || run.Token == "" {
			return fmt.Errorf("rule 3: run %+v", run)
		}
	}
	if !oracleProves(in, basis, o) {
		return fmt.Errorf("rule 4: not reviewed proves")
	}
	if oracleRed(in, basis) && !oracleRedReview(in, basis, o) {
		return fmt.Errorf("rule 4: a check saved red not reviewed proves with the red question")
	}
	// A check proposed red never passes on its own confirmation, whatever
	// that showed (a flaky green included).
	if slices.ContainsFunc(in.Confirmations, func(run CheckRun) bool {
		return run.CheckDigest == basis && run.ExpectFail
	}) {
		return fmt.Errorf("the basis %s was proposed red this round", basis)
	}
	return nil
}

// oracleRed reports a check saved to show a defect.
func oracleRed(in Input, digest string) bool {
	return slices.ContainsFunc(in.Checks, func(c StoredCheck) bool { return c.Digest == digest && c.ExpectFail })
}

// oracleRedReview reports a review of the red question answering proves.
func oracleRedReview(in Input, digest string, o OutcomeRef) bool {
	return slices.ContainsFunc(in.Reviews, func(rv CheckReview) bool {
		return rv.CheckDigest == digest && rv.OutcomeDigest == o.Digest && rv.ExpectFail && rv.Answer == ReviewProves
	})
}

func oracleRule5(in Input, o OutcomeRef, basis string) error {
	if in.Base == nil {
		return nil
	}
	for _, run := range in.Base.Runs {
		if run.CheckDigest != basis {
			continue
		}
		switch {
		case run.Result == RunQuarantined:
			return fmt.Errorf("rule 5: quarantined at base")
		case run.Result == RunPass && !o.Keep:
			return fmt.Errorf("rule 5: change check passed at base")
		case run.Result == RunFail && o.Keep:
			return fmt.Errorf("rule 5: keep check failed at base")
		}
	}
	return nil
}

func oracleRule6(in Input, o OutcomeRef, _ string) error {
	for _, sib := range in.Checks {
		if sib.Outcome == o.ID && !sib.Retired && oracleCountedFail(in, sib.Digest) {
			return fmt.Errorf("rule 6: sibling %s failed", sib.Digest)
		}
	}
	return nil
}

// oracleRound is the outcome-level conditions: never manual, never a pass
// or a fix in a round that retires or disputes one of its checks.
func oracleRound(in Input, o OutcomeRef, _ string) error {
	if o.Manual {
		return fmt.Errorf("manual outcome")
	}
	for _, r := range in.Retired {
		if _, ok := oracleStored(in, r.CheckDigest, o.ID); ok {
			return fmt.Errorf("a check of %s was retired this round", o.ID)
		}
	}
	for _, d := range in.Disputes {
		if c, ok := oracleStored(in, d.CheckDigest, o.ID); ok && !c.Retired && oracleFailed(in, d.CheckDigest) {
			return fmt.Errorf("a check of %s is disputed this round", o.ID)
		}
	}
	return nil
}

// decideStats counts what the property exercised, so a weak generator
// fails instead of passing vacuously.
type decideStats struct{ passed, fixes, redFixes, replays, ready int }

func (s *decideStats) add(in Input, d Decision) {
	for _, r := range d.PerOutcome {
		if r.Result == ResultPassed {
			s.passed++
		}
	}
	if d.Fix != nil {
		s.fixes++
		if slices.ContainsFunc(d.Fix.Failures, func(f FixFailure) bool { return oracleRed(in, f.CheckDigest) }) {
			s.redFixes++
		}
	}
	if d.ReplayRound {
		s.replays++
	}
	if d.Handoff == HandoffReady {
		s.ready++
	}
}

// TestDecideNoPassWithoutAllSixRules is the 4.3 property: no passed outcome
// without every rule; no fix without a failing, reviewed, stored check plus
// the evaluator's agreeing, cited fail, undisputed, in a clean environment;
// a ready hand-off only when every outcome passed with no anomaly.
func TestDecideNoPassWithoutAllSixRules(t *testing.T) {
	var stats decideStats
	prop := func(g genDecide) bool {
		d := Decide(g.in)
		stats.add(g.in, d)
		if err := checkDecision(g.in, d); err != nil {
			t.Logf("%v\ninput %+v", err, g.in)
			return false
		}
		return true
	}
	if err := quick.Check(prop, quickConfig(5000)); err != nil {
		t.Error(err)
	}
	if stats.passed == 0 || stats.fixes == 0 || stats.redFixes == 0 || stats.replays == 0 || stats.ready == 0 {
		t.Errorf("the generator is too weak: %+v", stats)
	}
}

// checkDecision holds d to the oracles.
func checkDecision(in Input, d Decision) error {
	if len(d.PerOutcome) != len(in.Outcomes) {
		return fmt.Errorf("PerOutcome length %d for %d outcomes", len(d.PerOutcome), len(in.Outcomes))
	}
	allPassed := len(in.Outcomes) > 0
	for i, r := range d.PerOutcome {
		o := in.Outcomes[i]
		if r.ID != o.ID {
			return fmt.Errorf("PerOutcome[%d] is %s, want %s", i, r.ID, o.ID)
		}
		if r.Result == ResultPassed {
			if err := oracleSixRules(in, o, r.Basis); err != nil {
				return fmt.Errorf("%s passed but %w", o.ID, err)
			}
		}
		allPassed = allPassed && r.Result == ResultPassed
	}
	if d.Fix != nil {
		if len(d.Fix.Failures) == 0 || ClassifyEnvironment(in.Env) != EnvOK {
			return fmt.Errorf("fix %+v in env %s", d.Fix, ClassifyEnvironment(in.Env))
		}
		for _, f := range d.Fix.Failures {
			if err := oracleFixable(in, f); err != nil {
				return fmt.Errorf("fix failure %+v: %w", f, err)
			}
		}
	}
	ready := allPassed && len(in.Anomalies) == 0 && ClassifyEnvironment(in.Env) == EnvOK && d.Fix == nil &&
		!d.ReplayRound
	if (d.Handoff == HandoffReady) != ready {
		return fmt.Errorf("handoff %s, want ready=%v", d.Handoff, ready)
	}
	return nil
}

func oracleFixable(in Input, f FixFailure) error {
	i := slices.IndexFunc(in.Outcomes, func(o OutcomeRef) bool { return o.ID == f.Outcome })
	if i < 0 {
		return fmt.Errorf("unknown outcome")
	}
	o := in.Outcomes[i]
	c, ok := oracleStored(in, f.CheckDigest, o.ID)
	if !ok || c.Retired || !c.Valid {
		return fmt.Errorf("not a stored, valid, unretired check of the outcome")
	}
	if err := oracleFixRun(in, o, f); err != nil {
		return err
	}
	if !oracleProves(in, f.CheckDigest, o) {
		return fmt.Errorf("failing check not reviewed proves")
	}
	if oracleRed(in, f.CheckDigest) && !oracleRedReview(in, f.CheckDigest, o) {
		return fmt.Errorf("a check saved red not reviewed proves with the red question")
	}
	if !slices.ContainsFunc(in.Verdicts, func(v EvaluatorVerdict) bool {
		return v.Outcome == o.ID && v.Claim == ClaimFail && len(v.Ledger) > 0
	}) {
		return fmt.Errorf("no agreeing, cited evaluator fail")
	}
	return oracleRound(in, o, "")
}

// oracleFixRun holds a fix failure to a run that may fail its outcome: a
// check saved red and not yet replayed green fails on a replay only red by
// assertion at or before its saved step (finding 2); a red confirmation
// never beside a green replay of the same check, and never with a vetted
// sibling passing cleanly (finding 3).
func oracleFixRun(in Input, o OutcomeRef, f FixFailure) error {
	failed := func(run CheckRun) bool {
		return run.CheckDigest == f.CheckDigest && run.Result == RunFail && run.Seq == f.Seq
	}
	if !oracleCountedFail(in, f.CheckDigest) || !slices.ContainsFunc(oracleRuns(in, f.CheckDigest), failed) {
		return fmt.Errorf("no counted failing run at seq %d", f.Seq)
	}
	return oracleNoVettedSibling(in, o, f)
}

// oracleNoVettedSibling: two vetted checks disagreeing is never a fail
// (finding 3).
func oracleNoVettedSibling(in Input, o OutcomeRef, f FixFailure) error {
	for _, c := range in.Checks {
		if c.Outcome != o.ID || c.Digest == f.CheckDigest || c.Retired || !c.Valid ||
			slices.ContainsFunc(in.Retired, func(r Retirement) bool { return r.CheckDigest == c.Digest }) {
			continue
		}
		if oracleRules1To4(in, o, c.Digest) == nil && oracleRule5(in, o, c.Digest) == nil &&
			!oracleFailed(in, c.Digest) {
			return fmt.Errorf("a vetted sibling %s passed cleanly", c.Digest)
		}
	}
	return nil
}

// oracleSavedRed is the first stored check of digest saved red.
func oracleSavedRed(in Input, digest string) (StoredCheck, bool) {
	for _, c := range in.Checks {
		if c.Digest == digest && c.ExpectFail {
			return c, true
		}
	}
	return StoredCheck{}, false
}

// oracleCountedFail restates which fails of digest count this round (the
// reviews' rules, independently of decide.go): a fail of a check saved red
// and not yet replayed green only by the assertion it was saved failing; a
// confirmation proposed red only red by assertion, with no green replay of
// the check, and, for a check already saved red, uncleared and by its saved
// assertion.
func oracleCountedFail(in Input, digest string) bool {
	c, red := oracleSavedRed(in, digest)
	same := func(run CheckRun) bool {
		return run.AssertionStep > 0 && run.AssertionStep == c.FailedStep && run.AssertionID == c.FailedAssertion
	}
	greenReplay := slices.ContainsFunc(in.Replay, func(run CheckRun) bool {
		return run.CheckDigest == digest && run.Result == RunPass
	})
	counts := func(run CheckRun, confirmation bool) bool {
		switch {
		case run.CheckDigest != digest || run.Result != RunFail:
			return false
		case confirmation && run.ExpectFail:
			return run.AssertionStep > 0 && !greenReplay && (!red || !c.Cleared && same(run))
		}
		return !red || c.Cleared || same(run)
	}
	return slices.ContainsFunc(in.Replay, func(r CheckRun) bool { return counts(r, false) }) ||
		slices.ContainsFunc(in.Confirmations, func(r CheckRun) bool { return counts(r, true) })
}
