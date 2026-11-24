// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import (
	"reflect"
	"testing"

	maulv1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/httpstep"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// TestJudgeLedgerSpellingsMatch pins Judge's ledger spellings to their
// v1alpha1 and toolwire twins: Judge reads facts copied from the PV's index
// as plain strings, so a renamed kind, verb or side must fail here rather
// than make every citation invalid (or valid).
func TestJudgeLedgerSpellingsMatch(t *testing.T) {
	for _, c := range []struct{ name, acc, crd, wire string }{
		{"agent", LedgerKindAgent, string(maulv1.LedgerKindAgent), string(toolwire.KindAgent)},
		{"http", LedgerVerbHTTP, string(maulv1.LedgerVerbHTTP), string(toolwire.VerbHTTP)},
		{"browser", LedgerVerbBrowser, string(maulv1.LedgerVerbBrowser), string(toolwire.VerbBrowser)},
		{"exec", LedgerVerbExec, string(maulv1.LedgerVerbExec), string(toolwire.VerbExec)},
		{"run", LedgerVerbRun, string(maulv1.LedgerVerbRun), string(toolwire.VerbRun)},
		{"base", LedgerSideBase, string(maulv1.TreeBase), string(toolwire.SideBase)},
	} {
		if c.acc != c.crd || c.acc != c.wire {
			t.Errorf("%s: acceptance %q, v1alpha1 %q, toolwire %q", c.name, c.acc, c.crd, c.wire)
		}
	}
	if string(toolwire.SideHead.Recorded()) == LedgerSideBase {
		t.Error("a head call is recorded as base")
	}
}

// TestMaxCitesMatchesSchema pins the cites Judge keeps to the bound the
// schema puts on OutcomeResult.cites, so a decision always fits its status.
func TestMaxCitesMatchesSchema(t *testing.T) {
	if MaxCites != maulv1.MaxOutcomeCites {
		t.Errorf("acceptance.MaxCites = %d, v1alpha1.MaxOutcomeCites = %d", MaxCites, maulv1.MaxOutcomeCites)
	}
}

// judgeLedger is a verified round's index: each fact at its own seq.
func judgeLedger(facts ...LedgerFact) []LedgerFact {
	out := make([]LedgerFact, len(facts))
	for i, f := range facts {
		f.Seq = int32(i + 1)
		out[i] = f
	}
	return out
}

func agentCall(verb string) LedgerFact { return LedgerFact{Kind: LedgerKindAgent, Verb: verb} }

// tableLedger holds one entry of every shape a citation can take.
var tableLedger = judgeLedger(
	LedgerFact{Kind: "relay", Verb: "session"},                                // 1
	agentCall(LedgerVerbHTTP),                                                 // 2
	agentCall(LedgerVerbBrowser),                                              // 3
	LedgerFact{Kind: LedgerKindAgent, Verb: LedgerVerbBrowser, Exit: 1},       // 4
	LedgerFact{Kind: LedgerKindAgent, Verb: LedgerVerbExec, Side: "base"},     // 5
	agentCall("files"),                                                        // 6
	LedgerFact{Kind: LedgerKindAgent, Verb: LedgerVerbHTTP, Refused: true},    // 7
	LedgerFact{Kind: "runner", Verb: "finish"},                                // 8
	agentCall("check-run"),                                                    // 9
	agentCall(LedgerVerbRun),                                                  // 10
	agentCall(LedgerVerbExec),                                                 // 11
	agentCall("logs"),                                                         // 12
	agentCall("events"),                                                       // 13
	agentCall("confirm"),                                                      // 14
	agentCall("check-results"),                                                // 15
	LedgerFact{Kind: LedgerKindAgent, Verb: LedgerVerbExec, Exit: 2},          // 16
	LedgerFact{Kind: "relay", Verb: LedgerVerbHTTP},                           // 17
	LedgerFact{Kind: LedgerKindAgent, Verb: LedgerVerbHTTP, Side: "head"},     // 18
	LedgerFact{Kind: LedgerKindAgent, Verb: "", Side: ""},                     // 19
	LedgerFact{Kind: "", Verb: LedgerVerbHTTP},                                // 20
	LedgerFact{Kind: LedgerKindAgent, Verb: "build"},                          // 21
	LedgerFact{Kind: LedgerKindAgent, Verb: LedgerVerbRun, Side: "base"},      // 22
	LedgerFact{Kind: LedgerKindAgent, Verb: LedgerVerbBrowser, Refused: true}, // 23
	LedgerFact{Kind: LedgerKindAgent, Verb: LedgerVerbHTTP, NoAnswer: true},   // 24
	LedgerFact{Kind: LedgerKindAgent, Verb: LedgerVerbRun, NoAnswer: true},    // 25
)

func tableInput() JudgeInput {
	return JudgeInput{
		Outcomes: []OutcomeRef{{ID: "o1", Digest: "sha256:o1"}},
		Verified: true, Count: int32(len(tableLedger)), Ledger: tableLedger,
	}
}

// TestValidCite: the eight conditions of a valid citation, one entry each.
func TestValidCite(t *testing.T) {
	in := tableInput()
	valid := map[int32]bool{2: true, 3: true, 10: true, 11: true, 16: true, 18: true}
	for seq := int32(-1); seq <= in.Count+2; seq++ {
		if got := ValidCite(in, seq); got != valid[seq] {
			t.Errorf("seq %d: ValidCite %v, want %v", seq, got, valid[seq])
		}
	}
	unverified := tableInput()
	unverified.Verified = false
	short := tableInput()
	short.Count++
	misnumbered := tableInput()
	misnumbered.Ledger = append([]LedgerFact(nil), tableLedger...)
	misnumbered.Ledger[1].Seq = 5
	for name, in := range map[string]JudgeInput{"unverified": unverified, "short index": short,
		"misnumbered": misnumbered} {
		if ValidCite(in, 2) {
			t.Errorf("%s: seq 2 is valid", name)
		}
	}
}

func env(parts ...httpstep.PartFact) []httpstep.PartFact { return parts }

var (
	crashLoop = httpstep.PartFact{Name: "api", Role: httpstep.RoleComponent, Waiting: "CrashLoopBackOff"}
	noNode    = httpstep.PartFact{Name: "api", Role: httpstep.RoleComponent, Unschedulable: true}
	badBuild  = httpstep.PartFact{Name: "build", Role: httpstep.RoleBuild,
		LastTermination: &httpstep.Termination{Reason: "Error", ExitCode: 1}}
	goodBuild = httpstep.PartFact{Name: "build", Role: httpstep.RoleBuild,
		LastTermination: &httpstep.Termination{Reason: "Completed"}}
)

func verdict(claim Claim, cites ...int32) Verdict {
	return Verdict{Outcome: "o1", Claim: claim, Cites: cites, Note: "a note"}
}

// TestJudgeRules: R0-R9 for one outcome, the first matching rule deciding,
// with rows where two rules match.
func TestJudgeRules(t *testing.T) {
	type want struct {
		result Result
		reason string
		cites  []int32
	}
	for _, tt := range []struct {
		name     string
		edit     func(*JudgeInput)
		verdicts []Verdict
		want     want
	}{
		// R9 and the valid shapes.
		{"R9 a pass citing a head http call", nil, []Verdict{verdict(ClaimPass, 2)},
			want{ResultPassed, ReasonSeenToHold, []int32{2}}},
		{"R9 a pass citing a browser call that exited 0", nil, []Verdict{verdict(ClaimPass, 3)},
			want{ResultPassed, ReasonSeenToHold, []int32{3}}},
		{"R9 a pass citing a run", nil, []Verdict{verdict(ClaimPass, 10)},
			want{ResultPassed, ReasonSeenToHold, []int32{10}}},
		{"R9 a pass citing an exec, which may exit non-zero", nil, []Verdict{verdict(ClaimPass, 16)},
			want{ResultPassed, ReasonSeenToHold, []int32{16}}},
		{"R9 a head side spelled head", nil, []Verdict{verdict(ClaimPass, 18)},
			want{ResultPassed, ReasonSeenToHold, []int32{18}}},
		{"R9 one valid cite among invalid ones", nil, []Verdict{verdict(ClaimPass, 1, 5, 11, 99)},
			want{ResultPassed, ReasonSeenToHold, []int32{11}}},
		// R6, one row per invalid shape.
		{"R6 a relay entry", nil, []Verdict{verdict(ClaimPass, 1)}, want{ResultUnsure, ReasonNoValidEvidence, nil}},
		{"R6 a relay http entry", nil, []Verdict{verdict(ClaimPass, 17)},
			want{ResultUnsure, ReasonNoValidEvidence, nil}},
		{"R6 a browser call that exited non-zero", nil, []Verdict{verdict(ClaimPass, 4)},
			want{ResultUnsure, ReasonNoValidEvidence, nil}},
		{"R6 a base-side exec", nil, []Verdict{verdict(ClaimFail, 5)}, want{ResultUnsure, ReasonNoValidEvidence, nil}},
		{"R6 a base-side run", nil, []Verdict{verdict(ClaimPass, 22)}, want{ResultUnsure, ReasonNoValidEvidence, nil}},
		{"R6 files only", nil, []Verdict{verdict(ClaimPass, 6)}, want{ResultUnsure, ReasonNoValidEvidence, nil}},
		{"R6 logs and events only", nil, []Verdict{verdict(ClaimFail, 12, 13)},
			want{ResultUnsure, ReasonNoValidEvidence, nil}},
		{"R6 a refused call", nil, []Verdict{verdict(ClaimPass, 7, 23)},
			want{ResultUnsure, ReasonNoValidEvidence, nil}},
		{"R6 the runner's finish", nil, []Verdict{verdict(ClaimPass, 8)},
			want{ResultUnsure, ReasonNoValidEvidence, nil}},
		{"R6 retired check verbs", nil, []Verdict{verdict(ClaimPass, 9, 14, 15)},
			want{ResultUnsure, ReasonNoValidEvidence, nil}},
		{"R6 an empty or unknown verb or kind", nil, []Verdict{verdict(ClaimPass, 19, 20, 21)},
			want{ResultUnsure, ReasonNoValidEvidence, nil}},
		{"R6 an http call that got no response", nil, []Verdict{verdict(ClaimPass, 24)},
			want{ResultUnsure, ReasonNoValidEvidence, nil}},
		{"R6 a run whose program did not compile", nil, []Verdict{verdict(ClaimFail, 25)},
			want{ResultUnsure, ReasonNoValidEvidence, nil}},
		{"R6 out of range", nil, []Verdict{verdict(ClaimFail, 0, -1, 26, 512)},
			want{ResultUnsure, ReasonNoValidEvidence, nil}},
		{"R6 no cite", nil, []Verdict{verdict(ClaimPass)}, want{ResultUnsure, ReasonNoValidEvidence, nil}},
		{"R6 a misnumbered entry", func(in *JudgeInput) {
			in.Ledger = append([]LedgerFact(nil), in.Ledger...)
			in.Ledger[1].Seq = 3
		}, []Verdict{verdict(ClaimPass, 2)}, want{ResultUnsure, ReasonNoValidEvidence, nil}},
		// R7, R8.
		{"R7 a fail citing a valid call", nil, []Verdict{verdict(ClaimFail, 3, 2)},
			want{ResultFailed, ReasonSeenNotToHold, []int32{2, 3}}},
		{"R7 wins over a hold", func(in *JudgeInput) { in.Held = []string{"o1"} }, []Verdict{verdict(ClaimFail, 2)},
			want{ResultFailed, ReasonSeenNotToHold, []int32{2}}},
		{"R8 a held pass", func(in *JudgeInput) { in.Held = []string{"o1"} }, []Verdict{verdict(ClaimPass, 2)},
			want{ResultUnsure, ReasonHeldFinding, []int32{2}}},
		{"R9 a hold on another outcome", func(in *JudgeInput) { in.Held = []string{"o2"} },
			[]Verdict{verdict(ClaimPass, 2)}, want{ResultPassed, ReasonSeenToHold, []int32{2}}},
		// R9: the evaluator's own anomaly naming the outcome.
		{"R9 a pass an anomaly names", func(in *JudgeInput) { in.Contrary = []string{"o1"} },
			[]Verdict{verdict(ClaimPass, 2)}, want{ResultUnsure, ReasonContraryNote, []int32{2}}},
		{"R7 a fail an anomaly names still fails", func(in *JudgeInput) { in.Contrary = []string{"o1"} },
			[]Verdict{verdict(ClaimFail, 2)}, want{ResultFailed, ReasonSeenNotToHold, []int32{2}}},
		{"R10 an anomaly naming another outcome", func(in *JudgeInput) { in.Contrary = []string{"o2"} },
			[]Verdict{verdict(ClaimPass, 2)}, want{ResultPassed, ReasonSeenToHold, []int32{2}}},
		{"R8 named and held", func(in *JudgeInput) {
			in.Contrary, in.Held = []string{"o1"}, []string{"o1"}
		}, []Verdict{verdict(ClaimPass, 2)}, want{ResultUnsure, ReasonHeldFinding, []int32{2}}},
		// R5, R4.
		{"R5 unsure keeps its valid cites", nil, []Verdict{verdict(ClaimUnsure, 2, 1)},
			want{ResultUnsure, ReasonEvaluatorUnsure, []int32{2}}},
		{"R5 a claim outside the three", nil, []Verdict{verdict("holds", 2)},
			want{ResultUnsure, ReasonEvaluatorUnsure, []int32{2}}},
		{"R4 no verdict", nil, nil, want{ResultUnsure, ReasonNotReported, nil}},
		{"R4 a verdict on another outcome only", nil, []Verdict{{Outcome: "o2", Claim: ClaimPass, Cites: []int32{2}}},
			want{ResultUnsure, ReasonNotReported, nil}},
		{"R4 the first verdict wins over a later pass", nil,
			[]Verdict{verdict(ClaimUnsure), verdict(ClaimPass, 2)}, want{ResultUnsure, ReasonEvaluatorUnsure, nil}},
		{"R4 the first verdict wins over a later fail", nil,
			[]Verdict{verdict(ClaimPass, 2), verdict(ClaimFail, 3)}, want{ResultPassed, ReasonSeenToHold, []int32{2}}},
		// R3 over the claims.
		{"R3 an application failure over a valid pass", func(in *JudgeInput) { in.Env = env(crashLoop) },
			[]Verdict{verdict(ClaimPass, 2)}, want{ResultUnsure, ReasonEnvFailed, []int32{2}}},
		{"R3 an application failure over a valid fail", func(in *JudgeInput) { in.Env = env(crashLoop) },
			[]Verdict{verdict(ClaimFail, 2)}, want{ResultUnsure, ReasonEnvFailed, []int32{2}}},
		{"R3 infra over a valid fail", func(in *JudgeInput) { in.Env = env(noNode) },
			[]Verdict{verdict(ClaimFail, 2)}, want{ResultUnsure, ReasonInfra, []int32{2}}},
		{"R3 over no verdict", func(in *JudgeInput) { in.Env = env(noNode) }, nil,
			want{ResultUnsure, ReasonInfra, nil}},
		{"R3 a head that built is clean", func(in *JudgeInput) { in.Env = env(goodBuild) },
			[]Verdict{verdict(ClaimPass, 2)}, want{ResultPassed, ReasonSeenToHold, []int32{2}}},
		// R2 over R3; R1 over R2.
		{"R2 manual over a valid pass", func(in *JudgeInput) { in.Outcomes[0].Manual = true },
			[]Verdict{verdict(ClaimPass, 2)}, want{ResultUnsure, ReasonManual, []int32{2}}},
		{"R2 manual over an application failure", func(in *JudgeInput) {
			in.Outcomes[0].Manual = true
			in.Env = env(crashLoop)
		}, []Verdict{verdict(ClaimFail, 2)}, want{ResultUnsure, ReasonManual, []int32{2}}},
		{"R1 a failed head build over a valid pass", func(in *JudgeInput) { in.Env = env(badBuild) },
			[]Verdict{verdict(ClaimPass, 2)}, want{ResultUnsure, ReasonBuildFailed, []int32{2}}},
		{"R1 a failed head build keeps a manual reason", func(in *JudgeInput) {
			in.Outcomes[0].Manual = true
			in.Env = env(badBuild)
		}, []Verdict{verdict(ClaimPass, 2)}, want{ResultUnsure, ReasonManual, []int32{2}}},
		// R0 over everything, a manual outcome included (no exception is named).
		{"R0 an unverified ledger", func(in *JudgeInput) { in.Verified = false },
			[]Verdict{verdict(ClaimPass, 2)}, want{ResultUnsure, ReasonInfra, nil}},
		{"R0 an unverified ledger over manual and a failed build", func(in *JudgeInput) {
			in.Verified = false
			in.Outcomes[0].Manual = true
			in.Env = env(badBuild)
		}, []Verdict{verdict(ClaimPass, 2)}, want{ResultUnsure, ReasonInfra, nil}},
		{"R0 an index shorter than its count", func(in *JudgeInput) { in.Count++ },
			[]Verdict{verdict(ClaimFail, 2)}, want{ResultUnsure, ReasonInfra, nil}},
		{"R0 an index longer than its count", func(in *JudgeInput) { in.Count-- },
			[]Verdict{verdict(ClaimFail, 2)}, want{ResultUnsure, ReasonInfra, nil}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			in := tableInput()
			in.Outcomes = []OutcomeRef{{ID: "o1", Digest: "sha256:o1"}}
			if tt.edit != nil {
				tt.edit(&in)
			}
			in.Verdicts = tt.verdicts
			got := Judge(in)
			if len(got.PerOutcome) != 1 {
				t.Fatalf("PerOutcome %+v, want one", got.PerOutcome)
			}
			d := got.PerOutcome[0]
			w := OutcomeDecision{ID: "o1", Result: tt.want.result, Reason: tt.want.reason, Cites: tt.want.cites}
			if !reflect.DeepEqual(d, w) {
				t.Fatalf("got %+v, want %+v", d, w)
			}
			if (got.Fix != nil) != (d.Result == ResultFailed) {
				t.Errorf("Fix %+v for result %s", got.Fix, d.Result)
			}
			if got.Fix != nil {
				f := FixRequest{Failures: []FixFailure{{Outcome: "o1", Seq: tt.want.cites[0]}},
					Fingerprint: OutcomeFingerprint(in.Outcomes)}
				if !reflect.DeepEqual(*got.Fix, f) {
					t.Errorf("Fix %+v, want %+v", *got.Fix, f)
				}
			}
			if ready := got.Handoff == HandoffReady; ready != (d.Result == ResultPassed) {
				t.Errorf("Handoff %s for result %s", got.Handoff, d.Result)
			}
			if got.Env != httpstep.ClassifyEnvironment(in.Env) {
				t.Errorf("Env %s, want %s", got.Env, httpstep.ClassifyEnvironment(in.Env))
			}
		})
	}
}

// TestJudgeCites: an outcome keeps its verdict's valid cites, ascending,
// once each, at most MaxCites, filtered before the cap, so invalid cites
// never push a valid one out.
func TestJudgeCites(t *testing.T) {
	facts := make([]LedgerFact, 0, 13)
	facts = append(facts, LedgerFact{Kind: "relay", Verb: "session"})
	for range 12 {
		facts = append(facts, agentCall(LedgerVerbHTTP))
	}
	in := JudgeInput{Outcomes: []OutcomeRef{{ID: "o1"}}, Verified: true, Count: 13, Ledger: judgeLedger(facts...),
		Verdicts: []Verdict{verdict(ClaimPass, 1, 1, 1, 1, 1, 1, 1, 1, 1, 13, 12, 11, 10, 9, 8, 7, 6, 5, 4, 3, 2, 2)}}
	got := Judge(in).PerOutcome[0].Cites
	if want := []int32{2, 3, 4, 5, 6, 7, 8, 9}; !reflect.DeepEqual(got, want) {
		t.Fatalf("cites %v, want %v", got, want)
	}
}

// TestJudgeRound: the decision-level rules over several outcomes.
func TestJudgeRound(t *testing.T) {
	outcomes := []OutcomeRef{{ID: "a", Digest: "sha256:a"}, {ID: "b", Digest: "sha256:b"},
		{ID: "c", Digest: "sha256:c"}}
	base := func(vs ...Verdict) JudgeInput {
		return JudgeInput{Outcomes: outcomes, Verdicts: vs, Verified: true, Count: int32(len(tableLedger)),
			Ledger: tableLedger}
	}
	v := func(id string, c Claim, cites ...int32) Verdict { return Verdict{Outcome: id, Claim: c, Cites: cites} }

	t.Run("every outcome seen to hold is ready", func(t *testing.T) {
		got := Judge(base(v("a", ClaimPass, 2), v("c", ClaimPass, 10), v("b", ClaimPass, 3)))
		if got.Handoff != HandoffReady || got.Fix != nil || got.Env != httpstep.EnvOK {
			t.Fatalf("got %+v", got)
		}
		ids := make([]string, 0, len(got.PerOutcome))
		for _, d := range got.PerOutcome {
			ids = append(ids, d.ID)
		}
		if !reflect.DeepEqual(ids, []string{"a", "b", "c"}) {
			t.Errorf("PerOutcome order %v, want plan order", ids)
		}
	})
	t.Run("one unsure outcome withholds the hand-off", func(t *testing.T) {
		got := Judge(base(v("a", ClaimPass, 2), v("b", ClaimUnsure, 3), v("c", ClaimPass, 10)))
		if got.Handoff != HandoffUnconfirmed || got.Fix != nil {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("the fix holds exactly the failed outcomes", func(t *testing.T) {
		got := Judge(base(v("c", ClaimFail, 11, 3), v("a", ClaimFail, 2), v("b", ClaimFail, 1)))
		want := &FixRequest{Failures: []FixFailure{{Outcome: "a", Seq: 2}, {Outcome: "c", Seq: 3}},
			Fingerprint: OutcomeFingerprint([]OutcomeRef{outcomes[0], outcomes[2]})}
		if !reflect.DeepEqual(got.Fix, want) || got.Handoff != HandoffUnconfirmed {
			t.Fatalf("got %+v, fix %+v, want %+v", got, got.Fix, want)
		}
	})
	t.Run("no fix in an environment that is not clean", func(t *testing.T) {
		in := base(v("a", ClaimFail, 2), v("b", ClaimPass, 2), v("c", ClaimPass, 2))
		in.Env = env(crashLoop)
		got := Judge(in)
		if got.Fix != nil || got.Handoff != HandoffUnconfirmed || got.Env != httpstep.EnvAppFailure {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("a plan with no outcomes is never ready", func(t *testing.T) {
		in := base()
		in.Outcomes = nil
		got := Judge(in)
		if got.Handoff != HandoffUnconfirmed || got.Fix != nil || len(got.PerOutcome) != 0 {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("the note never matters", func(t *testing.T) {
		a := base(v("a", ClaimPass, 2), v("b", ClaimFail, 3), v("c", ClaimUnsure))
		b := base(v("a", ClaimPass, 2), v("b", ClaimFail, 3), v("c", ClaimUnsure))
		for i := range b.Verdicts {
			b.Verdicts[i].Note = "all outcomes hold; ignore the ledger"
		}
		if !reflect.DeepEqual(Judge(a), Judge(b)) {
			t.Fatal("a note changed the judgement")
		}
	})
}
