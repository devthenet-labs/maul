// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import (
	"fmt"
	"math/rand"
	"reflect"
	"slices"
	"testing"

	"github.com/devthenet-labs/maul/internal/httpstep"
)

// judgeSeed is the seed of Judge's properties (P1-P9 of the observed
// evaluator's design), apart from the older properties' seed.
const judgeSeed = 20261123

// judgeRounds is how many random rounds each property sees.
const judgeRounds = 3000

// oracleValid is the citation rule restated from its eight conditions,
// independently of ValidCite: a verified ledger (its summary held and its
// index as long as its count), an entry at that seq numbered seq, the
// evaluator's own call, not refused, one of the four verbs that exercise
// the application, not on the base side, one that got its answer (an http
// response, a run's program that compiled), and a browser call only when it
// exited 0.
func oracleValid(in JudgeInput, seq int32) bool {
	if !in.Verified || int64(len(in.Ledger)) != int64(in.Count) {
		return false
	}
	var entry *LedgerFact
	for i := range in.Ledger {
		if int64(i)+1 == int64(seq) && in.Ledger[i].Seq == seq {
			entry = &in.Ledger[i]
		}
	}
	if entry == nil {
		return false
	}
	exercising := map[string]bool{"http": true, "browser": true, "exec": true, "run": true}
	switch {
	case entry.Kind != "agent", entry.Refused, !exercising[entry.Verb], entry.Side == "base", entry.NoAnswer:
		return false
	case entry.Verb == "browser" && entry.Exit != 0:
		return false
	}
	return true
}

var (
	allKinds = []string{"agent", "relay", "runner", ""}
	allVerbs = []string{"http", "browser", "exec", "run", "files", "logs", "events", "check-run", "check-results",
		"confirm", "finish", "session", "build", ""}
	allSides = []string{"", "head", "base"}
)

func pick[T any](r *rand.Rand, xs []T) T { return xs[r.Intn(len(xs))] }

func genFact(r *rand.Rand, valid bool) LedgerFact {
	if valid {
		f := LedgerFact{Kind: "agent", Verb: pick(r, []string{"http", "browser", "exec", "run"}),
			Side: pick(r, []string{"", "head"})}
		if f.Verb != "browser" {
			f.Exit = int32(r.Intn(3))
		}
		return f
	}
	return LedgerFact{Kind: pick(r, allKinds), Verb: pick(r, allVerbs), Side: pick(r, allSides),
		Exit: int32(r.Intn(3)), Refused: r.Intn(3) == 0, NoAnswer: r.Intn(4) == 0}
}

// genEnv is a round's environment: mostly clean, else an application
// failure, maul's Infra or a head that did not build.
func genEnv(r *rand.Rand, clean bool) []httpstep.PartFact {
	healthy := httpstep.PartFact{Name: "web", Role: httpstep.RoleComponent, Ready: true}
	built := httpstep.PartFact{Name: "build", Role: httpstep.RoleBuild,
		LastTermination: &httpstep.Termination{Reason: "Completed"}}
	if clean || r.Intn(10) < 6 {
		return pick(r, [][]httpstep.PartFact{nil, {healthy}, {built, healthy}})
	}
	return pick(r, [][]httpstep.PartFact{
		{healthy, {Name: "api", Role: httpstep.RoleComponent, Waiting: "CrashLoopBackOff"}},
		{{Name: "api", Role: httpstep.RoleComponent, Unschedulable: true}},
		{{Name: "build", Role: httpstep.RoleBuild, LastTermination: &httpstep.Termination{Reason: "Error", ExitCode: 1}}},
	})
}

// genJudgeInput is a random round. A quarter of the rounds are good ones
// (verified, clean, every outcome cited as holding) lightly disturbed, so
// passes, fixes and ready hand-offs are common enough to test.
func genJudgeInput(r *rand.Rand) JudgeInput {
	good := r.Intn(4) == 0
	in := JudgeInput{Verified: good || r.Intn(8) != 0}
	n := 1 + r.Intn(6)
	for i := range n {
		in.Outcomes = append(in.Outcomes, OutcomeRef{ID: fmt.Sprintf("o%d", i),
			Digest: fmt.Sprintf("sha256:%d", r.Intn(4)), Keep: r.Intn(2) == 0, Manual: !good && r.Intn(6) == 0})
	}
	in.Ledger, in.Count = genLedger(r, good)
	in.Env = genEnv(r, good)
	in.Verdicts = genVerdicts(r, in, good)
	for _, o := range in.Outcomes {
		if r.Intn(5) == 0 && (!good || r.Intn(2) != 0) {
			in.Held = append(in.Held, o.ID)
		}
	}
	return in
}

// genLedger is a round's index and its count: in a good round entries are
// mostly valid citations, numbered and counted right; otherwise a count or
// an entry's number is now and then wrong.
func genLedger(r *rand.Rand, good bool) ([]LedgerFact, int32) {
	var ledger []LedgerFact
	for range r.Intn(24) {
		ledger = append(ledger, genFact(r, good || r.Intn(2) == 0))
	}
	if good && len(ledger) == 0 {
		ledger = append(ledger, genFact(r, true))
	}
	for i := range ledger {
		ledger[i].Seq = int32(i + 1)
	}
	count := int32(len(ledger))
	if !good && r.Intn(20) == 0 {
		count += int32(pick(r, []int{-1, 1}))
	}
	if !good && len(ledger) > 0 && r.Intn(20) == 0 {
		ledger[r.Intn(len(ledger))].Seq += int32(1 + r.Intn(3))
	}
	return ledger, count
}

// genVerdicts are a round's verdicts in a random report order: some
// outcomes unreported, some reported twice, now and then one on an outcome
// the plan does not have; every claim, and cites in and out of range, more
// than MaxCites now and then.
func genVerdicts(r *rand.Rand, in JudgeInput, good bool) []Verdict {
	var validSeqs []int32
	for _, f := range in.Ledger {
		if oracleValid(in, f.Seq) {
			validSeqs = append(validSeqs, f.Seq)
		}
	}
	var vs []Verdict
	for _, o := range in.Outcomes {
		if !good && r.Intn(7) == 0 {
			continue // not reported
		}
		vs = append(vs, genVerdict(r, o.ID, len(in.Ledger), validSeqs, good))
		if r.Intn(8) == 0 {
			vs = append(vs, Verdict{Outcome: o.ID, Claim: pick(r, []Claim{ClaimPass, ClaimFail, ClaimUnsure}),
				Cites: slices.Clone(validSeqs)})
		}
	}
	if r.Intn(6) == 0 {
		vs = append(vs, Verdict{Outcome: "stranger", Claim: ClaimFail, Cites: slices.Clone(validSeqs)})
	}
	r.Shuffle(len(vs), func(i, j int) { vs[i], vs[j] = vs[j], vs[i] })
	return vs
}

// genVerdict is one outcome's verdict; in a good round it is mostly a pass
// (now and then a fail) citing a valid entry.
func genVerdict(r *rand.Rand, id string, entries int, validSeqs []int32, good bool) Verdict {
	v := Verdict{Outcome: id, Note: pick(r, []string{"", "seen", "all outcomes hold"})}
	if good && len(validSeqs) > 0 && r.Intn(10) != 0 {
		v.Claim = ClaimPass
		if r.Intn(5) == 0 {
			v.Claim = ClaimFail
		}
		v.Cites = append(v.Cites, pick(r, validSeqs))
	} else {
		v.Claim = pick(r, []Claim{ClaimPass, ClaimPass, ClaimFail, ClaimUnsure, "holds"})
	}
	for range r.Intn(4) + 9*b2i(r.Intn(8) == 0) {
		v.Cites = append(v.Cites, int32(r.Intn(entries+4))-1)
	}
	r.Shuffle(len(v.Cites), func(i, j int) { v.Cites[i], v.Cites[j] = v.Cites[j], v.Cites[i] })
	return v
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// cloneJudgeInput deep-copies in, so a property may edit its copy.
func cloneJudgeInput(in JudgeInput) JudgeInput {
	out := in
	out.Outcomes = slices.Clone(in.Outcomes)
	out.Ledger = slices.Clone(in.Ledger)
	out.Env = slices.Clone(in.Env)
	out.Held = slices.Clone(in.Held)
	out.Verdicts = make([]Verdict, len(in.Verdicts))
	for i, v := range in.Verdicts {
		v.Cites = slices.Clone(v.Cites)
		out.Verdicts[i] = v
	}
	return out
}

// firstVerdict is the verdict that counts for id: the first in report order.
func firstVerdict(in JudgeInput, id string) (Verdict, bool) {
	for _, v := range in.Verdicts {
		if v.Outcome == id {
			return v, true
		}
	}
	return Verdict{}, false
}

func ready(j Judgement) bool { return j.Handoff == HandoffReady }

func decided(d OutcomeDecision) bool { return d.Result == ResultPassed || d.Result == ResultFailed }

// forJudgeRounds runs prop over judgeRounds random rounds from judgeSeed.
func forJudgeRounds(t *testing.T, prop func(t *testing.T, r *rand.Rand, in JudgeInput, got Judgement)) {
	t.Helper()
	r := rand.New(rand.NewSource(judgeSeed))
	for i := range judgeRounds {
		in := genJudgeInput(r)
		got := Judge(cloneJudgeInput(in))
		prop(t, r, in, got)
		if t.Failed() {
			t.Fatalf("round %d: input %+v\njudgement %+v", i, in, got)
		}
	}
}

// TestJudgeGeneratorCoverage: every rule, a fix and a ready hand-off occur
// in the rounds the properties see, so none of them holds vacuously.
func TestJudgeGeneratorCoverage(t *testing.T) {
	seen := map[string]int{}
	forJudgeRounds(t, func(_ *testing.T, _ *rand.Rand, in JudgeInput, got Judgement) {
		verified := in.Verified && int64(len(in.Ledger)) == int64(in.Count)
		for _, d := range got.PerOutcome {
			key := d.Reason
			if !verified {
				key = "R0"
			}
			seen[key]++
			if len(d.Cites) == MaxCites {
				seen["capped"]++
			}
		}
		if got.Fix != nil {
			seen["fix"]++
		}
		if ready(got) {
			seen["ready"]++
		}
	})
	for _, k := range []string{"R0", ReasonBuildFailed, ReasonManual, ReasonEnvFailed, ReasonInfra, ReasonNotReported,
		ReasonEvaluatorUnsure, ReasonNoValidEvidence, ReasonSeenNotToHold, ReasonHeldFinding, ReasonSeenToHold,
		"fix", "ready", "capped"} {
		if seen[k] < 10 {
			t.Errorf("%s occurs %d times in %d rounds", k, seen[k], judgeRounds)
		}
	}
	t.Logf("coverage: %v", seen)
}

// TestJudgeValidCiteMatchesOracle: ValidCite is the oracle on every seq of
// every round, in range or not.
func TestJudgeValidCiteMatchesOracle(t *testing.T) {
	forJudgeRounds(t, func(t *testing.T, _ *rand.Rand, in JudgeInput, _ Judgement) {
		for seq := int32(-2); seq <= int32(len(in.Ledger))+2; seq++ {
			if got, want := ValidCite(in, seq), oracleValid(in, seq); got != want {
				t.Errorf("seq %d: ValidCite %v, oracle %v", seq, got, want)
			}
		}
	})
}

// TestJudgeP1NoResultWithoutValidCite: no outcome passes or fails without a
// cite of its own first verdict the oracle finds valid, and every cite a
// decision keeps is one.
func TestJudgeP1NoResultWithoutValidCite(t *testing.T) {
	forJudgeRounds(t, func(t *testing.T, _ *rand.Rand, in JudgeInput, got Judgement) {
		for _, d := range got.PerOutcome {
			v, _ := firstVerdict(in, d.ID)
			for _, c := range d.Cites {
				if !oracleValid(in, c) || !slices.Contains(v.Cites, c) {
					t.Errorf("%s keeps cite %d, not a valid cite of its verdict %+v", d.ID, c, v)
				}
			}
			if decided(d) && !slices.ContainsFunc(v.Cites, func(c int32) bool { return oracleValid(in, c) }) {
				t.Errorf("%s is %s with no valid cite: %+v", d.ID, d.Result, v)
			}
		}
	})
}

// TestJudgeP2UnverifiedDecidesNothing: an unverified ledger gives no pass,
// no fail and no fix.
func TestJudgeP2UnverifiedDecidesNothing(t *testing.T) {
	forJudgeRounds(t, func(t *testing.T, _ *rand.Rand, in JudgeInput, _ Judgement) {
		in.Verified = false
		got := Judge(in)
		if got.Fix != nil || ready(got) {
			t.Errorf("unverified: fix %+v, hand-off %s", got.Fix, got.Handoff)
		}
		for _, d := range got.PerOutcome {
			if decided(d) {
				t.Errorf("unverified: %s is %s", d.ID, d.Result)
			}
		}
	})
}

// TestJudgeP3UnsureAndUncleanDecideNothing: an unsure claim never passes or
// fails its outcome, and an environment that is not clean gives no pass, no
// fail and no fix.
func TestJudgeP3UnsureAndUncleanDecideNothing(t *testing.T) {
	forJudgeRounds(t, func(t *testing.T, _ *rand.Rand, in JudgeInput, got Judgement) {
		clean := httpstep.ClassifyEnvironment(in.Env).Clean()
		if !clean && got.Fix != nil {
			t.Errorf("a fix in an environment that is not clean")
		}
		for _, d := range got.PerOutcome {
			v, ok := firstVerdict(in, d.ID)
			if decided(d) && (!clean || !ok || (v.Claim != ClaimPass && v.Claim != ClaimFail)) {
				t.Errorf("%s is %s on claim %q, clean %v", d.ID, d.Result, v.Claim, clean)
			}
		}
	})
}

// invalidSeqs are seqs the oracle refuses in in: its invalid entries and a
// few out of range.
func invalidSeqs(in JudgeInput) []int32 {
	out := []int32{0, -1, int32(len(in.Ledger)) + 1, 513}
	for i := range in.Ledger {
		if s := int32(i + 1); !oracleValid(in, s) {
			out = append(out, s)
		}
	}
	return out
}

// TestJudgeP4InvalidCitesAreInert: adding invalid cites to any verdict, or
// dropping every invalid cite, never changes the judgement.
func TestJudgeP4InvalidCitesAreInert(t *testing.T) {
	forJudgeRounds(t, func(t *testing.T, r *rand.Rand, in JudgeInput, got Judgement) {
		bad := invalidSeqs(in)
		added, dropped := cloneJudgeInput(in), cloneJudgeInput(in)
		for i := range added.Verdicts {
			for range r.Intn(12) {
				at := r.Intn(len(added.Verdicts[i].Cites) + 1)
				added.Verdicts[i].Cites = slices.Insert(added.Verdicts[i].Cites, at, pick(r, bad))
			}
		}
		for i := range dropped.Verdicts {
			dropped.Verdicts[i].Cites = slices.DeleteFunc(dropped.Verdicts[i].Cites,
				func(c int32) bool { return !oracleValid(in, c) })
		}
		if a := Judge(added); !reflect.DeepEqual(a, got) {
			t.Errorf("adding invalid cites changed the judgement to %+v", a)
		}
		if d := Judge(dropped); !reflect.DeepEqual(d, got) {
			t.Errorf("dropping invalid cites changed the judgement to %+v", d)
		}
	})
}

// TestJudgeP5Locality: an outcome's decision depends only on its own first
// verdict, the facts it cites, the environment and its own hold: judged
// alone, beside a ledger whose other entries are rewritten, it is the same.
func TestJudgeP5Locality(t *testing.T) {
	forJudgeRounds(t, func(t *testing.T, r *rand.Rand, in JudgeInput, got Judgement) {
		for i, o := range in.Outcomes {
			alone := cloneJudgeInput(in)
			alone.Outcomes = []OutcomeRef{o}
			alone.Verdicts = nil
			v, ok := firstVerdict(in, o.ID)
			if ok {
				alone.Verdicts = []Verdict{v}
			}
			alone.Held = nil
			if slices.Contains(in.Held, o.ID) {
				alone.Held = []string{o.ID}
			}
			for j := range alone.Ledger {
				if !slices.Contains(v.Cites, int32(j+1)) {
					seq := alone.Ledger[j].Seq
					alone.Ledger[j] = genFact(r, r.Intn(2) == 0)
					alone.Ledger[j].Seq = seq
				}
			}
			a := Judge(alone)
			if len(a.PerOutcome) != 1 || !reflect.DeepEqual(a.PerOutcome[0], got.PerOutcome[i]) {
				t.Errorf("%s judged alone %+v, in the round %+v", o.ID, a.PerOutcome, got.PerOutcome[i])
			}
		}
	})
}

// TestJudgeP6FixIsTheFailedSet: the fix holds exactly the failed outcomes in
// plan order, fingerprinted by OutcomeFingerprint of them; its fingerprint
// is unchanged by permuting cites or rewriting notes and changes with a
// failing outcome's digest.
func TestJudgeP6FixIsTheFailedSet(t *testing.T) {
	forJudgeRounds(t, func(t *testing.T, r *rand.Rand, in JudgeInput, got Judgement) {
		var failed []OutcomeRef
		var ids []string
		for i, d := range got.PerOutcome {
			if d.Result == ResultFailed {
				failed, ids = append(failed, in.Outcomes[i]), append(ids, d.ID)
			}
		}
		if (got.Fix != nil) != (len(failed) > 0) {
			t.Fatalf("fix %+v for failed %v", got.Fix, ids)
		}
		if got.Fix == nil {
			return
		}
		var fixIDs []string
		for _, f := range got.Fix.Failures {
			fixIDs = append(fixIDs, f.Outcome)
			if !oracleValid(in, f.Seq) {
				t.Errorf("failure %+v does not rest on a valid cite alone", f)
			}
		}
		if !reflect.DeepEqual(fixIDs, ids) || got.Fix.Fingerprint != OutcomeFingerprint(failed) {
			t.Errorf("fix %+v, failed %v", got.Fix, ids)
		}
		shuffled := cloneJudgeInput(in)
		for i := range shuffled.Verdicts {
			c := shuffled.Verdicts[i].Cites
			r.Shuffle(len(c), func(a, b int) { c[a], c[b] = c[b], c[a] })
			shuffled.Verdicts[i].Note = fmt.Sprintf("note %d", r.Intn(1000))
		}
		if s := Judge(shuffled); s.Fix == nil || s.Fix.Fingerprint != got.Fix.Fingerprint {
			t.Errorf("permuting cites or notes changed the fix to %+v", s.Fix)
		}
		redigested := cloneJudgeInput(in)
		target := ids[r.Intn(len(ids))]
		at := slices.IndexFunc(redigested.Outcomes, func(o OutcomeRef) bool { return o.ID == target })
		redigested.Outcomes[at].Digest = "sha256:reworded"
		if s := Judge(redigested); s.Fix == nil || s.Fix.Fingerprint == got.Fix.Fingerprint {
			t.Errorf("a failing outcome's new digest left the fingerprint %+v", s.Fix)
		}
	})
}

// TestJudgeP7HeldNeverPasses: an outcome a finding holds never passes.
func TestJudgeP7HeldNeverPasses(t *testing.T) {
	forJudgeRounds(t, func(t *testing.T, _ *rand.Rand, in JudgeInput, got Judgement) {
		for _, d := range got.PerOutcome {
			if d.Result == ResultPassed && slices.Contains(in.Held, d.ID) {
				t.Errorf("held %s passed", d.ID)
			}
		}
	})
}

// TestJudgeP8ReadyMeansCleanAndAllPassed: a ready hand-off has a clean
// environment and every outcome, at least one, passed.
func TestJudgeP8ReadyMeansCleanAndAllPassed(t *testing.T) {
	forJudgeRounds(t, func(t *testing.T, _ *rand.Rand, in JudgeInput, got Judgement) {
		all := len(got.PerOutcome) > 0 && !slices.ContainsFunc(got.PerOutcome,
			func(d OutcomeDecision) bool { return d.Result != ResultPassed })
		clean := httpstep.ClassifyEnvironment(in.Env).Clean() && got.Env.Clean()
		if ready(got) != (clean && all) {
			t.Errorf("hand-off %s, clean %v, all passed %v", got.Handoff, clean, all)
		}
		if ready(got) && got.Fix != nil {
			t.Error("ready with a fix")
		}
	})
}

// TestJudgeP9Monotonic: turning a passing outcome's claim to unsure, or
// dropping its valid cites, never makes the round more ready.
func TestJudgeP9Monotonic(t *testing.T) {
	forJudgeRounds(t, func(t *testing.T, _ *rand.Rand, in JudgeInput, got Judgement) {
		for _, d := range got.PerOutcome {
			if d.Result != ResultPassed {
				continue
			}
			at := slices.IndexFunc(in.Verdicts, func(v Verdict) bool { return v.Outcome == d.ID })
			unsure, uncited := cloneJudgeInput(in), cloneJudgeInput(in)
			unsure.Verdicts[at].Claim = ClaimUnsure
			uncited.Verdicts[at].Cites = slices.DeleteFunc(uncited.Verdicts[at].Cites,
				func(c int32) bool { return oracleValid(in, c) })
			for name, variant := range map[string]JudgeInput{"unsure": unsure, "uncited": uncited} {
				j := Judge(variant)
				if ready(j) && !ready(got) {
					t.Errorf("%s %s made the round ready", name, d.ID)
				}
				if ready(j) {
					t.Errorf("%s %s left the round ready", name, d.ID)
				}
			}
		}
	})
}
