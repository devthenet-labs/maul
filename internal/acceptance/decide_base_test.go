// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import (
	"math/rand"
	"reflect"
	"slices"
	"testing"
	"testing/quick"
)

// genAgentsRound is a random agents-placement round, biased towards
// passes: every check stored, valid and reviewed proves, mostly run green at
// head with a token, the evaluator mostly agreeing; the base varies freely
// (required or not, buildable or not, any run of each check), as do head
// fails (red by assertion or not, in every form) and the head build.
type genAgentsRound struct{ in Input }

func (genAgentsRound) Generate(r *rand.Rand, _ int) reflect.Value {
	in := Input{Base: &BaseRuns{Required: r.Intn(4) != 0, Unbuildable: r.Intn(6) == 0}}
	forms := []CheckForm{CheckFormExec, CheckFormRun, CheckFormHTTP, CheckFormBrowser}
	for i, id := range genOutcomes[:1+r.Intn(2)] {
		o := OutcomeRef{ID: id, Digest: od(id), Keep: r.Intn(3) == 0}
		in.Outcomes = append(in.Outcomes, o)
		for _, c := range genDigests[2*i : 2*i+1+r.Intn(2)] {
			in.Checks = append(in.Checks, StoredCheck{Digest: c, Outcome: id, Form: pickOf(r, forms), Valid: true})
			in.Reviews = append(in.Reviews, CheckReview{CheckDigest: c, OutcomeDigest: o.Digest, Answer: ReviewProves})
			head := CheckRun{CheckDigest: c, Result: RunPass, Token: "tok-h", Seq: int32(1 + r.Intn(20))}
			if r.Intn(4) == 0 {
				head.Result, head.AssertionStep = RunFail, int32(r.Intn(2))
			}
			if r.Intn(2) == 0 {
				in.Replay = append(in.Replay, head)
			} else {
				in.Confirmations = append(in.Confirmations, head)
			}
			for range r.Intn(3) {
				in.Base.Runs = append(in.Base.Runs, CheckRun{CheckDigest: c,
					Result: pickOf(r, []RunResult{RunFail, RunFail, RunPass, RunError, RunRefused, RunQuarantined}),
					Token:  pickOf(r, []string{"tok-b", "tok-b", ""}), Seq: int32(1 + r.Intn(20)),
					AssertionStep: int32(r.Intn(2)), AssertionID: "stdout"})
			}
		}
		v := EvaluatorVerdict{Outcome: id, Claim: pickOf(r, []Claim{ClaimPass, ClaimPass, ClaimFail, ClaimUnsure}),
			Ledger: []int32{1}}
		if r.Intn(10) == 0 {
			v.Contrary = []int32{2}
		}
		in.Verdicts = append(in.Verdicts, v)
	}
	if r.Intn(8) == 0 {
		in.Env = []PartFact{failedBuild()}
	}
	return reflect.ValueOf(genAgentsRound{in})
}

// TestDecideAgentsRoundProperty holds agents-placement rounds to the 4.3
// oracles (checkDecision): no pass without all six rules, a required base
// included (no pass with a missing base run, no discrimination from a fail
// with no assertion, from a compile error or timeout alone, nor for a
// change outcome whose check passed at base); no fail on a program check
// failing with no assertion (O4); neither a fail nor a pass when the head
// did not build.
func TestDecideAgentsRoundProperty(t *testing.T) {
	var stats decideStats
	prop := func(g genAgentsRound) bool {
		d := Decide(g.in)
		stats.add(g.in, d)
		if err := checkDecision(g.in, d); err != nil {
			t.Logf("%v\ninput %+v\nbase %+v", err, g.in, *g.in.Base)
			return false
		}
		return true
	}
	if err := quick.Check(prop, quickConfig(5000)); err != nil {
		t.Error(err)
	}
	if stats.requiredPasses < 100 || stats.assertedBasePasses < 100 || stats.notAsserted < 20 ||
		stats.buildFailed < 20 || stats.fixes < 20 {
		t.Errorf("the generator is too weak: %+v", stats)
	}
}

// baseRun is one run of check "a" at base.
func baseRun(result RunResult, assertionStep int32) CheckRun {
	run := CheckRun{CheckDigest: dg("a"), Result: result, Token: "tok-b", Seq: 40, AssertionStep: assertionStep}
	if assertionStep > 0 {
		run.AssertionID = "stdout"
	}
	return run
}

// atBase is confirmedPass with a base: the given runs, required or not.
func atBase(keep, required, unbuildable bool, runs ...CheckRun) Input {
	return with(confirmedPass(), func(in *Input) {
		in.Outcomes[0].Keep = keep
		in.Base = &BaseRuns{Runs: runs, Required: required, Unbuildable: unbuildable}
	})
}

// TestDecideBaseRuns is rule 5 with a base (slice S2, the agents
// placement): a change outcome's check discriminates only red by assertion
// at base, a keep outcome's only green there; a check that could not run at
// base (an error, a refusal, a compile failure or a timeout, both errors in
// the agents placement) or a base that could not be built is n/a; a
// required base with no run of the check is not met.
func TestDecideBaseRuns(t *testing.T) {
	const change, keep = false, true
	passed := func(in Input) (Input, Result, string) { return in, ResultPassed, ReasonConfirmed }
	unsure := func(in Input) (Input, Result, string) { return in, ResultUnsure, ReasonModelOnlyPass }
	tests := []struct {
		name   string
		build  func() (Input, Result, string)
		basis  bool
		reason string
	}{
		{name: "change: red by assertion at base discriminates",
			build: func() (Input, Result, string) { return passed(atBase(change, true, false, baseRun(RunFail, 1))) }},
		{name: "change: red by assertion at an optional base discriminates",
			build: func() (Input, Result, string) { return passed(atBase(change, false, false, baseRun(RunFail, 1))) }},
		{name: "change: an exit-only fail at base is no discrimination",
			build: func() (Input, Result, string) { return unsure(atBase(change, true, false, baseRun(RunFail, 0))) }},
		{name: "change: green at base is no discrimination",
			build: func() (Input, Result, string) { return unsure(atBase(change, true, false, baseRun(RunPass, 0))) }},
		{name: "change: green at base with no token is no discrimination",
			build: func() (Input, Result, string) {
				r := baseRun(RunPass, 0)
				r.Token = ""
				return unsure(atBase(change, true, false, r))
			}},
		{name: "change: quarantined at base",
			build: func() (Input, Result, string) {
				return unsure(atBase(change, true, false, baseRun(RunQuarantined, 0)))
			}},
		{name: "change: green and red at base",
			build: func() (Input, Result, string) {
				return unsure(atBase(change, true, false, baseRun(RunFail, 1), baseRun(RunPass, 0)))
			}},
		{name: "change: red by assertion and an exit-only fail at base",
			build: func() (Input, Result, string) {
				return unsure(atBase(change, true, false, baseRun(RunFail, 1), baseRun(RunFail, 0)))
			}},
		{name: "change: red by assertion beside an error at base",
			build: func() (Input, Result, string) {
				return passed(atBase(change, true, false, baseRun(RunFail, 1), baseRun(RunError, 0)))
			}},
		{name: "change: a compile error or timeout at base (an error) is n/a",
			build: func() (Input, Result, string) { return passed(atBase(change, true, false, baseRun(RunError, 0))) }},
		{name: "change: a refusal at base is n/a",
			build: func() (Input, Result, string) { return passed(atBase(change, true, false, baseRun(RunRefused, 0))) }},
		{name: "change: a required base with no run of the check is not met",
			build: func() (Input, Result, string) { return unsure(atBase(change, true, false)) }},
		{name: "change: a required base run of another check does not count",
			build: func() (Input, Result, string) {
				r := baseRun(RunFail, 1)
				r.CheckDigest = dg("other")
				return unsure(atBase(change, true, false, r))
			}},
		{name: "change: an optional base with no run is n/a",
			build: func() (Input, Result, string) { return passed(atBase(change, false, false)) }},
		{name: "change: an unbuildable base is n/a, required or not",
			build: func() (Input, Result, string) { return passed(atBase(change, true, true)) }},
		{name: "change: an unbuildable base never hides a green base run",
			build: func() (Input, Result, string) { return unsure(atBase(change, true, true, baseRun(RunPass, 0))) }},
		{name: "keep: green at base",
			build: func() (Input, Result, string) { return passed(atBase(keep, true, false, baseRun(RunPass, 0))) }},
		{name: "keep: red by assertion at base",
			build: func() (Input, Result, string) { return unsure(atBase(keep, true, false, baseRun(RunFail, 1))) }},
		{name: "keep: an exit-only fail at base is n/a",
			build: func() (Input, Result, string) { return passed(atBase(keep, true, false, baseRun(RunFail, 0))) }},
		{name: "keep: an error at base is n/a",
			build: func() (Input, Result, string) { return passed(atBase(keep, true, false, baseRun(RunError, 0))) }},
		{name: "keep: quarantined at base",
			build: func() (Input, Result, string) { return unsure(atBase(keep, true, false, baseRun(RunQuarantined, 0))) }},
		{name: "keep: a required base with no run is not met",
			build: func() (Input, Result, string) { return unsure(atBase(keep, true, false)) }},
		{name: "keep: an unbuildable base is n/a",
			build: func() (Input, Result, string) { return passed(atBase(keep, true, true)) }},
		// Bug 13 (#317) holds with a base: a check that discriminates still
		// never passes an outcome the evaluator saw fail as worded.
		{name: "bug 13: the evaluator's fail withholds a discriminating pass",
			build: func() (Input, Result, string) {
				in := atBase(change, true, false, baseRun(RunFail, 1))
				in.Verdicts[0].Claim = ClaimFail
				return in, ResultUnsure, ReasonEvaluatorSawFailure
			}},
		{name: "bug 13: contrary entries withhold a discriminating pass",
			build: func() (Input, Result, string) {
				in := atBase(change, true, false, baseRun(RunFail, 1))
				in.Verdicts[0].Contrary = []int32{9}
				return in, ResultUnsure, ReasonEvaluatorSawFailure
			}},
		{name: "bug 13: a narrowed check never passes, whatever the base",
			build: func() (Input, Result, string) {
				in := atBase(change, true, false, baseRun(RunFail, 1))
				in.Checks[0].Narrowed = true
				return in, ResultUnsure, ReasonEvaluatorSawFailure
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in, result, reason := tt.build()
			d := Decide(in)
			got := d.PerOutcome[0]
			if got.Result != result || got.Reason != reason {
				t.Fatalf("got %+v, want %s (%s)", got, result, reason)
			}
			if (got.Result == ResultPassed) != (d.Handoff == HandoffReady) {
				t.Errorf("handoff %s for %+v", d.Handoff, got)
			}
		})
	}
}

// TestDecideBaseRunsNeverRefuseAFail: a base only ever withholds a pass; a
// reproduced failure at head is judged as without one.
func TestDecideBaseRunsNeverRefuseAFail(t *testing.T) {
	for _, base := range []*BaseRuns{
		nil, {Required: true}, {Unbuildable: true}, {Runs: []CheckRun{baseRun(RunPass, 0)}, Required: true},
	} {
		in := deterministicFail(ClaimFail, 3)
		in.Base = base
		d := Decide(in)
		if d.PerOutcome[0].Result != ResultFailed || d.Fix == nil {
			t.Errorf("base %+v: got %+v, fix %v; want failed with a fix", base, d.PerOutcome[0], d.Fix)
		}
	}
}

// failedBuild is the head build's part: it did not build.
func failedBuild() PartFact {
	return PartFact{Name: "build-head", Role: RoleBuild, LastTermination: &Termination{Reason: "Error", ExitCode: 1}}
}

// TestDecideHeadBuildFailed: a tool-pod build that failed at the head is
// maul's Infra (tools-build-failed), and the round then has no fail and no
// pass, whatever its runs show: CI built the head green, so the tool pod's
// failure to is no evidence about the change.
func TestDecideHeadBuildFailed(t *testing.T) {
	built := PartFact{Name: "build-head", Role: RoleBuild, LastTermination: &Termination{Reason: "Completed"}}
	tests := []struct {
		name   string
		in     Input
		result Result
		reason string
		env    EnvClass
	}{
		{name: "a pass is withheld",
			in:     with(confirmedPass(), func(in *Input) { in.Env = []PartFact{failedBuild()} }),
			result: ResultUnsure, reason: ReasonBuildFailed, env: EnvInfra},
		{name: "a reproduced fail is withheld",
			in:     with(deterministicFail(ClaimFail, 3), func(in *Input) { in.Env = []PartFact{failedBuild()} }),
			result: ResultUnsure, reason: ReasonBuildFailed, env: EnvInfra},
		{name: "a build part with no termination did not build",
			in: with(confirmedPass(), func(in *Input) {
				in.Env = []PartFact{{Name: "build-head", Role: RoleBuild}}
			}),
			result: ResultUnsure, reason: ReasonBuildFailed, env: EnvInfra},
		{name: "a build the kubelet killed for memory did not build",
			in: with(confirmedPass(), func(in *Input) {
				in.Env = []PartFact{{Name: "build-head", Role: RoleBuild,
					LastTermination: &Termination{Reason: "OOMKilled", ExitCode: 137}}}
			}),
			result: ResultUnsure, reason: ReasonBuildFailed, env: EnvInfra},
		{name: "a manual outcome stays manual",
			in: with(confirmedPass(), func(in *Input) {
				in.Outcomes[0].Manual = true
				in.Env = []PartFact{failedBuild()}
			}),
			result: ResultUnsure, reason: ReasonManual, env: EnvInfra},
		{name: "a build that completed is no trouble",
			in:     with(confirmedPass(), func(in *Input) { in.Env = []PartFact{built} }),
			result: ResultPassed, reason: ReasonConfirmed, env: EnvOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := Decide(tt.in)
			got := d.PerOutcome[0]
			if got.Result != tt.result || got.Reason != tt.reason || d.Env != tt.env {
				t.Fatalf("got %+v env %s, want %s (%s) env %s", got, d.Env, tt.result, tt.reason, tt.env)
			}
			if tt.reason == ReasonBuildFailed || tt.reason == ReasonManual {
				if d.Fix != nil || d.Suspected != nil || d.ReplayRound || len(d.Retire) > 0 || len(d.Narrowed) > 0 ||
					d.Handoff != HandoffUnconfirmed {
					t.Errorf("a failed head build decided more than unsure: %+v", d)
				}
			}
		})
	}
}

// programFail is one outcome with a stored, reviewed program check (exec
// or run form) whose replay failed, red by assertion at step 1 or not, and
// the evaluator's cited fail.
func programFail(form CheckForm, assertionStep int32) Input {
	in := deterministicFail(ClaimFail, 3)
	in.Checks[0].Form = form
	in.Replay[0].AssertionStep = assertionStep
	if assertionStep > 0 {
		in.Replay[0].AssertionID = "marker winner"
	}
	return in
}

// TestDecideProgramFailNeedsAssertion is owner decision O4: a program check
// (exec or run form) that failed other than by assertion (a crash, a
// compile error, a timeout, a wrong exit with no marker) never fails its
// outcome nor starts a fix, in either placement; red by assertion, it does.
// http and browser checks are judged as before.
func TestDecideProgramFailNeedsAssertion(t *testing.T) {
	tests := []struct {
		name   string
		in     Input
		result Result
		reason string
		fix    []string
	}{
		{name: "exec red by assertion: failed", in: programFail(CheckFormExec, 1),
			result: ResultFailed, reason: ReasonReproduced, fix: []string{dg("a")}},
		{name: "run red by assertion: failed", in: programFail(CheckFormRun, 1),
			result: ResultFailed, reason: ReasonReproduced, fix: []string{dg("a")}},
		{name: "exec failed with no assertion: unsure", in: programFail(CheckFormExec, 0),
			result: ResultUnsure, reason: ReasonNotAsserted},
		{name: "run failed with no assertion: unsure", in: programFail(CheckFormRun, 0),
			result: ResultUnsure, reason: ReasonNotAsserted},
		{name: "exec failed by assertion once and once not: unsure",
			in: with(programFail(CheckFormExec, 1), func(in *Input) {
				in.Replay = append(in.Replay, CheckRun{CheckDigest: dg("a"), Result: RunFail, Token: "tok-s", Seq: 4})
			}),
			result: ResultUnsure, reason: ReasonNotAsserted},
		{name: "http with no assertion is unchanged", in: programFail(CheckFormHTTP, 0),
			result: ResultFailed, reason: ReasonReproduced, fix: []string{dg("a")}},
		{name: "browser with no assertion is unchanged", in: programFail(CheckFormBrowser, 0),
			result: ResultFailed, reason: ReasonReproduced, fix: []string{dg("a")}},
		{name: "the evaluator not agreeing keeps its reason",
			in:     with(programFail(CheckFormExec, 0), func(in *Input) { in.Verdicts[0].Claim = ClaimUnsure }),
			result: ResultUnsure, reason: ReasonEvaluatorDisagrees},
		{name: "an asserted sibling still fails the outcome, alone",
			in: with(programFail(CheckFormExec, 0), func(in *Input) {
				in.Checks = append(in.Checks, StoredCheck{Digest: dg("b"), Outcome: "elo", Form: CheckFormHTTP, Valid: true})
				in.Reviews = append(in.Reviews, CheckReview{CheckDigest: dg("b"), OutcomeDigest: od("elo"),
					Answer: ReviewProves})
				in.Replay = append(in.Replay, CheckRun{CheckDigest: dg("b"), Result: RunFail, Token: "tok-b", Seq: 5,
					AssertionStep: 1, AssertionID: "json /a"})
			}),
			result: ResultFailed, reason: ReasonReproduced, fix: []string{dg("b")}},
		{name: "a run check passes like any of maul's forms",
			in:     with(confirmedPass(), func(in *Input) { in.Checks[0].Form = CheckFormRun }),
			result: ResultPassed, reason: ReasonConfirmed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := Decide(tt.in)
			got := d.PerOutcome[0]
			if got.Result != tt.result || got.Reason != tt.reason {
				t.Fatalf("got %+v, want %s (%s)", got, tt.result, tt.reason)
			}
			var fix []string
			if d.Fix != nil {
				for _, f := range d.Fix.Failures {
					fix = append(fix, f.CheckDigest)
				}
			}
			if !slices.Equal(fix, tt.fix) || d.Suspected != nil {
				t.Errorf("fix %v suspected %v, want fix %v", fix, d.Suspected, tt.fix)
			}
		})
	}
}
