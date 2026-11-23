// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"fmt"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/report"
	"github.com/devthenet-labs/maul/internal/templates"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// The switch ("evaluator observed", W4): intent-controller judges a round
// with acceptance.Judge, counting a verdict only on a citation the PV's
// verified index admits.

// citedIndex is a verified index whose every entry is a head http call of
// the evaluator's but for the entries of over, given by seq.
func citedIndex(n int32, over map[int32]v1alpha1.LedgerIndexEntry) *v1alpha1.LedgerSummary {
	l := observedLedger(n)
	for seq, e := range over {
		e.Seq = seq
		if e.ResultSHA256 == "" {
			e.ResultSHA256 = strings.Repeat("b", 64)
		}
		l.Entries[seq-1] = e
	}
	return l
}

// passedJudgement is the observed judgement of in's current round at its
// heads seeing every evaluable outcome hold, each verdict citing a head
// http call of the evaluator's.
func passedJudgement(in *v1alpha1.Intent) roundJudgement {
	v, acc := in.Status.Verification, in.Status.Acceptance
	pv := &v1alpha1.PreviewVerification{Spec: v1alpha1.PreviewVerificationSpec{Round: v.Round,
		HeadSetDigest: v.HeadSetDigest}, Status: v1alpha1.PreviewVerificationStatus{Ledger: observedLedger(5)}}
	ev := &report.Evaluation{}
	for i, o := range acc.Outcomes {
		if !o.Manual {
			ev.Verdicts = append(ev.Verdicts, report.OutcomeVerdict{ID: o.ID, Verdict: report.VerdictPass,
				Ledger: []int32{int32(i%5 + 1)}})
		}
	}
	return judgeRound(v, acc, pv, ev)
}

// oneOutcome is an approved plan of outcome o alone.
func oneOutcome() *v1alpha1.IntentAcceptance {
	return &v1alpha1.IntentAcceptance{Outcomes: []v1alpha1.AcceptanceOutcome{
		{ID: oVersion, Outcome: "GET /version answers with the commit it runs"}}}
}

// TestJudgeRoundCitationTable: a verdict counts only on a citation of the
// round's verified index that is the evaluator's own unrefused call
// exercising the application at the head: a pass or fail citing a relay's
// or runner's entry, a refused call, a call at the base, a files, logs or
// events read, a retired check-run, a seq the index does not hold, or a
// browser command that exited non-zero is unsure (no-valid-evidence); one
// citing a head http, browser (exit 0), exec or run call passes or fails.
func TestJudgeRoundCitationTable(t *testing.T) {
	agent := func(verb v1alpha1.LedgerVerb) v1alpha1.LedgerIndexEntry {
		return v1alpha1.LedgerIndexEntry{Kind: v1alpha1.LedgerKindAgent, Verb: verb}
	}
	refused, base, failed := agent(v1alpha1.LedgerVerbHTTP), agent(v1alpha1.LedgerVerbExec),
		agent(v1alpha1.LedgerVerbBrowser)
	refused.Refused, base.Side, failed.Exit = true, v1alpha1.TreeBase, 1
	for _, tt := range []struct {
		name   string
		entry  *v1alpha1.LedgerIndexEntry // at seq 3; nil leaves the head http call
		cite   int32
		result acceptance.Result
	}{
		{"relay", &v1alpha1.LedgerIndexEntry{Kind: v1alpha1.LedgerKindRelay, Verb: v1alpha1.LedgerVerbHTTP}, 3,
			acceptance.ResultUnsure},
		{"runner", &v1alpha1.LedgerIndexEntry{Kind: v1alpha1.LedgerKindRunner, Verb: v1alpha1.LedgerVerbFinish}, 3,
			acceptance.ResultUnsure},
		{"refused", &refused, 3, acceptance.ResultUnsure},
		{"base", &base, 3, acceptance.ResultUnsure},
		{"files only", new(agent(v1alpha1.LedgerVerbFiles)), 3, acceptance.ResultUnsure},
		{"logs only", new(agent(v1alpha1.LedgerVerbLogs)), 3, acceptance.ResultUnsure},
		{"events only", new(agent(v1alpha1.LedgerVerbEvents)), 3, acceptance.ResultUnsure},
		{"retired check-run", new(agent(v1alpha1.LedgerVerbCheckRun)), 3, acceptance.ResultUnsure},
		{"retired check-results", new(agent(v1alpha1.LedgerVerbCheckResults)), 3, acceptance.ResultUnsure},
		{"out of range", nil, 6, acceptance.ResultUnsure},
		{"seq 0", nil, 0, acceptance.ResultUnsure},
		{"browser exit 1", &failed, 3, acceptance.ResultUnsure},
		{"head http", nil, 3, acceptance.ResultPassed},
		{"head browser", new(agent(v1alpha1.LedgerVerbBrowser)), 3, acceptance.ResultPassed},
		{"head exec", new(agent(v1alpha1.LedgerVerbExec)), 3, acceptance.ResultPassed},
		{"head run, exit 1", &v1alpha1.LedgerIndexEntry{Kind: v1alpha1.LedgerKindAgent, Verb: v1alpha1.LedgerVerbRun,
			Exit: 1}, 3, acceptance.ResultPassed},
	} {
		t.Run(tt.name, func(t *testing.T) {
			over := map[int32]v1alpha1.LedgerIndexEntry{}
			if tt.entry != nil {
				over[3] = *tt.entry
			}
			pv := &v1alpha1.PreviewVerification{Spec: v1alpha1.PreviewVerificationSpec{Round: 1},
				Status: v1alpha1.PreviewVerificationStatus{Ledger: citedIndex(5, over)}}
			for _, claim := range []report.Verdict{report.VerdictPass, report.VerdictFail} {
				ev := &report.Evaluation{Verdicts: []report.OutcomeVerdict{{ID: oVersion, Verdict: claim,
					Ledger: []int32{tt.cite}}}}
				d := judgeRound(nil, oneOutcome(), pv, ev).judgement.PerOutcome[0]
				want := tt.result
				if want == acceptance.ResultPassed && claim == report.VerdictFail {
					want = acceptance.ResultFailed
				}
				if d.Result != want {
					t.Errorf("%s citing %d = %s (%s), want %s", claim, tt.cite, d.Result, d.Reason, want)
				}
				if want == acceptance.ResultUnsure && d.Reason != acceptance.ReasonNoValidEvidence {
					t.Errorf("%s citing %d: reason %s, want no-valid-evidence", claim, tt.cite, d.Reason)
				}
			}
		})
	}
}

// TestJudgeRoundUnverifiedLedger: a round with no verified index in status
// (the tools log did not hold to its summary, or the index was dropped)
// passes and fails nothing, whatever its evaluator cites; one whose
// application failed (EnvFailed) with no verified index says so of every
// outcome (env-failed, never maul's own infra), and fixes nothing.
func TestJudgeRoundUnverifiedLedger(t *testing.T) {
	ev := &report.Evaluation{Verdicts: []report.OutcomeVerdict{{ID: oVersion, Verdict: report.VerdictFail,
		Ledger: []int32{1}}}}
	pv := &v1alpha1.PreviewVerification{Spec: v1alpha1.PreviewVerificationSpec{Round: 1}}
	j := judgeRound(nil, oneOutcome(), pv, ev)
	if d := j.judgement.PerOutcome[0]; d.Result != acceptance.ResultUnsure || d.Reason != acceptance.ReasonInfra ||
		j.judgement.Fix != nil {
		t.Errorf("no index: %+v, fix %+v; want unsure on infra, no fix", d, j.judgement.Fix)
	}
	pv.Status.Environment = []v1alpha1.PartFact{{Name: "app", Role: v1alpha1.PreviewRoleComponent, Ready: true,
		CrashedDuring: true, Restarts: 1}}
	j = judgeRound(nil, oneOutcome(), pv, ev)
	if d := j.judgement.PerOutcome[0]; d.Result != acceptance.ResultUnsure || d.Reason != acceptance.ReasonEnvFailed ||
		j.judgement.Fix != nil {
		t.Errorf("app failure, no index: %+v, fix %+v; want unsure on env-failed, no fix", d, j.judgement.Fix)
	}
	in := &v1alpha1.Intent{Status: v1alpha1.IntentStatus{Acceptance: oneOutcome(),
		Verification: &v1alpha1.IntentVerification{Round: 1, HeadSetDigest: "sha256:" + strings.Repeat("a", 64)}}}
	applyJudgement(in, j)
	if got := environmentWhy(in.Status.Verification); got != templates.HandoffWhyEnvironment {
		t.Errorf("hand-off why = %q, want the environment's", got)
	}
}

// evaluatedPass is a pass over an intent of acc with pv's evaluate run
// complete, reporting text.
func evaluatedPass(acc *v1alpha1.IntentAcceptance, pv *v1alpha1.PreviewVerification, text string) *pass {
	run := &v1alpha1.IntentRun{Spec: v1alpha1.IntentRunSpec{Stage: v1alpha1.IntentStageEvaluate,
		Evaluation: &v1alpha1.IntentRunEvaluation{Verification: pv.Name, UID: pv.UID}},
		Status: v1alpha1.IntentRunStatus{Phase: v1alpha1.RunComplete, Report: text}}
	in := &v1alpha1.Intent{Status: v1alpha1.IntentStatus{Acceptance: acc,
		Verification: &v1alpha1.IntentVerification{Round: pv.Spec.Round, HeadSetDigest: pv.Spec.HeadSetDigest}}}
	return &pass{in: in, runs: []*v1alpha1.IntentRun{run}}
}

// verdictReport is an evaluate report of verdicts, each "id claim seq".
func verdictReport(verdicts ...string) string {
	var b strings.Builder
	b.WriteString("---\nverdicts:\n")
	for _, v := range verdicts {
		f := strings.Fields(v)
		fmt.Fprintf(&b, "  - id: %s\n    verdict: %s\n    ledger: [%s]\n    note: \"note on %s\"\n", f[0], f[1],
			f[2], f[0])
	}
	b.WriteString("---\nbody\n")
	return b.String()
}

// TestEvaluationFailureTargetsEveryHead: an outcome seen not to hold,
// citing a call maul counts, is a failure the verify-fix round is asked
// for: its targets are every head's component in the round's head order,
// its signature the outcome fingerprint (v2) of the failing outcomes, and
// one piece per failure (o2:), nothing the evaluator chose. A failure
// whose only cite does not count asks for nothing.
func TestEvaluationFailureTargetsEveryHead(t *testing.T) {
	acc := &v1alpha1.IntentAcceptance{Outcomes: []v1alpha1.AcceptanceOutcome{
		{ID: oVersion, Outcome: "GET /version answers with the commit it runs"},
		{ID: oHealthy, Outcome: "GET /healthz still answers ok"}}}
	pv := &v1alpha1.PreviewVerification{ObjectMeta: metav1.ObjectMeta{Name: "x-v-r1-a1", UID: types.UID("pv-1")},
		Spec: v1alpha1.PreviewVerificationSpec{Round: 1, HeadSetDigest: "sha256:" + strings.Repeat("a", 64),
			Heads: []v1alpha1.VerificationHead{{Component: "web"}, {Component: "api"}, {Component: "web"}}},
		Status: v1alpha1.PreviewVerificationStatus{Ledger: citedIndex(4,
			map[int32]v1alpha1.LedgerIndexEntry{4: {Kind: v1alpha1.LedgerKindRelay, Verb: v1alpha1.LedgerVerbHTTP}})}}
	p := evaluatedPass(acc, pv, verdictReport(oVersion+" fail 2", oHealthy+" fail 3"))
	f, fix, _, ok := p.evaluationFailure(t.Context(), pv)
	if !ok || fix == nil || len(fix.Failures) != 2 {
		t.Fatalf("failure %+v, fix %+v, ok %v; want both outcomes", f, fix, ok)
	}
	if !slices.Equal(f.targets, []string{"web", "api"}) {
		t.Errorf("targets = %q, want every head's component in order", f.targets)
	}
	refs := []acceptance.OutcomeRef{{ID: oVersion, Digest: outcomeDigest(acc.Outcomes[0])},
		{ID: oHealthy, Digest: outcomeDigest(acc.Outcomes[1])}}
	if f.signature != acceptance.OutcomeFingerprint(refs) || !strings.HasPrefix(f.signature,
		acceptance.OutcomeFingerprintVersion+":") {
		t.Errorf("signature = %q, want the v2 fingerprint of both outcomes", f.signature)
	}
	if want := []string{acceptance.OutcomeFingerprintPiece(refs[0]),
		acceptance.OutcomeFingerprintPiece(refs[1])}; !slices.Equal(f.pieces, want) {
		t.Errorf("pieces = %q, want %q", f.pieces, want)
	}
	// The same failures cited otherwise, with other notes: the same pieces.
	again := evaluatedPass(acc, pv, strings.ReplaceAll(verdictReport(oVersion+" fail 3", oHealthy+" fail 1,2"),
		"note on", "another note on"))
	if g, _, _, _ := again.evaluationFailure(t.Context(), pv); g.signature != f.signature ||
		!slices.Equal(g.pieces, f.pieces) {
		t.Errorf("cited otherwise: %+v, want %+v", g, f)
	}
	uncited := evaluatedPass(acc, pv, verdictReport(oVersion+" fail 4", oHealthy+" pass 2"))
	if g, fix, _, ok := uncited.evaluationFailure(t.Context(), pv); ok || fix != nil {
		t.Errorf("a fail citing a relay's call asks for %+v, %+v", g, fix)
	}
}

// TestRepeatedFailureOutcomePieces: with outcome-only pieces (v2) a later
// failure of the outcomes the last fix round was for is a repeat, however
// it is cited and whatever else fails beside them; one that no longer
// fails one of them is not; a round recorded with check-era pieces (v1,
// "o:") is never repeated by a v2 failure.
func TestRepeatedFailureOutcomePieces(t *testing.T) {
	a := acceptance.OutcomeRef{ID: oVersion, Digest: "sha256:" + strings.Repeat("1", 64)}
	b := acceptance.OutcomeRef{ID: oHealthy, Digest: "sha256:" + strings.Repeat("2", 64)}
	piece := acceptance.OutcomeFingerprintPiece
	failing := func(refs ...acceptance.OutcomeRef) verifyFailure {
		f := verifyFailure{signature: acceptance.OutcomeFingerprint(refs)}
		for _, r := range refs {
			f.pieces = append(f.pieces, piece(r))
		}
		return f
	}
	prior := priorFailure{signature: acceptance.OutcomeFingerprint([]acceptance.OutcomeRef{a}),
		pieces: []string{piece(a)}}
	for _, tt := range []struct {
		name  string
		prior priorFailure
		cur   verifyFailure
		want  bool
	}{
		{"the same outcome", prior, failing(a), true},
		{"the same outcome beside another", prior, failing(a, b), true},
		{"another outcome", prior, failing(b), false},
		{"the outcome's text changed", prior, failing(acceptance.OutcomeRef{ID: oVersion,
			Digest: "sha256:" + strings.Repeat("3", 64)}), false},
		// An earlier release's record: a v1 fingerprint and piece, as it
		// stored them.
		{"a check-era round", priorFailure{signature: "maul-evaluation-fingerprint/1:" + strings.Repeat("4", 64),
			pieces: []string{"o:" + oVersion + " maul-evaluation-fingerprint/1:" + strings.Repeat("5", 64)}},
			failing(a), false},
	} {
		if got := repeatedFailure(tt.prior, tt.cur); got != tt.want {
			t.Errorf("%s: repeatedFailure = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// TestJudgeRoundNeverStrongerThanIndex (seeded): over random rounds, a
// report whose verdicts cite only seqs the PV's index marks a relay's or
// runner's entry, refused, at the base, a read, a retired verb, a failed
// browser command, or not in the index at all never yields a pass, a fail
// or a fix, nor a Passed or Failing verification, whatever it claims; and
// adding a valid cite to an outcome changes no other outcome's result.
func TestJudgeRoundNeverStrongerThanIndex(t *testing.T) {
	rng := rand.New(rand.NewPCG(acceptanceJudgeSeed, 4))
	ids := []string{"a", "b", "c", "d"}
	acc := &v1alpha1.IntentAcceptance{}
	for _, id := range ids {
		acc.Outcomes = append(acc.Outcomes, v1alpha1.AcceptanceOutcome{ID: id, Outcome: "outcome " + id})
	}
	invalid := []func(e *v1alpha1.LedgerIndexEntry){
		func(e *v1alpha1.LedgerIndexEntry) { e.Kind = v1alpha1.LedgerKindRelay },
		func(e *v1alpha1.LedgerIndexEntry) {
			e.Kind, e.Verb = v1alpha1.LedgerKindRunner, v1alpha1.LedgerVerbFinish
		},
		func(e *v1alpha1.LedgerIndexEntry) { e.Refused = true },
		func(e *v1alpha1.LedgerIndexEntry) { e.Verb, e.Side = v1alpha1.LedgerVerbExec, v1alpha1.TreeBase },
		func(e *v1alpha1.LedgerIndexEntry) { e.Verb = v1alpha1.LedgerVerbFiles },
		func(e *v1alpha1.LedgerIndexEntry) { e.Verb = v1alpha1.LedgerVerbLogs },
		func(e *v1alpha1.LedgerIndexEntry) { e.Verb = v1alpha1.LedgerVerbEvents },
		func(e *v1alpha1.LedgerIndexEntry) { e.Verb = v1alpha1.LedgerVerbCheckRun },
		func(e *v1alpha1.LedgerIndexEntry) { e.Verb = v1alpha1.LedgerVerbConfirm },
		func(e *v1alpha1.LedgerIndexEntry) { e.Verb, e.Exit = v1alpha1.LedgerVerbBrowser, 2 },
	}
	claims := []report.Verdict{report.VerdictPass, report.VerdictFail, report.VerdictUnsure}
	for round := range 500 {
		n := int32(4 + rng.IntN(20))
		ledger := observedLedger(n)
		var bad []int32
		for i := range ledger.Entries {
			if rng.IntN(3) > 0 {
				invalid[rng.IntN(len(invalid))](&ledger.Entries[i])
				bad = append(bad, ledger.Entries[i].Seq)
			}
		}
		if len(bad) == 0 {
			continue
		}
		pv := &v1alpha1.PreviewVerification{Spec: v1alpha1.PreviewVerificationSpec{Round: 1,
			HeadSetDigest: "sha256:" + strings.Repeat("e", 64)}, Status: v1alpha1.PreviewVerificationStatus{Ledger: ledger}}
		ev := &report.Evaluation{}
		for _, id := range ids {
			var cites []int32
			for range 1 + rng.IntN(4) {
				if rng.IntN(5) == 0 {
					cites = append(cites, n+1+rng.Int32N(5)) // not in the index
				} else {
					cites = append(cites, bad[rng.IntN(len(bad))])
				}
			}
			ev.Verdicts = append(ev.Verdicts, report.OutcomeVerdict{ID: id, Verdict: claims[rng.IntN(len(claims))],
				Ledger: cites})
		}
		j := judgeRound(nil, acc, pv, ev)
		for _, d := range j.judgement.PerOutcome {
			if d.Result != acceptance.ResultUnsure || len(d.Cites) != 0 {
				t.Fatalf("round %d: %+v on invalid cites alone (index %+v)", round, d, ledger.Entries)
			}
		}
		in := &v1alpha1.Intent{Status: v1alpha1.IntentStatus{Acceptance: acc,
			Verification: &v1alpha1.IntentVerification{Round: 1, HeadSetDigest: pv.Spec.HeadSetDigest}}}
		applyJudgement(in, j)
		if s := in.Status.Verification.State; j.judgement.Fix != nil || s == v1alpha1.VerificationPassed ||
			s == v1alpha1.VerificationFailing {
			t.Fatalf("round %d: state %s, fix %+v on invalid cites alone", round, s, j.judgement.Fix)
		}
		// One outcome gains a valid cite: only it may change.
		valid := slices.IndexFunc(ledger.Entries, func(e v1alpha1.LedgerIndexEntry) bool {
			return !slices.Contains(bad, e.Seq)
		})
		if valid < 0 {
			continue
		}
		k := rng.IntN(len(ids))
		ev.Verdicts[k].Ledger = append(ev.Verdicts[k].Ledger, ledger.Entries[valid].Seq)
		j2 := judgeRound(nil, acc, pv, ev)
		for i, d := range j2.judgement.PerOutcome {
			if i != k && !reflect.DeepEqual(d, j.judgement.PerOutcome[i]) {
				t.Fatalf("round %d: outcome %s changed with outcome %s's cite: %+v, was %+v", round, d.ID, ids[k], d,
					j.judgement.PerOutcome[i])
			}
		}
	}
}

// acceptanceJudgeSeed is the seed of the controller's observed-judgement
// property (design: seed 20261123, as acceptance's own).
const acceptanceJudgeSeed = 20261123

// TestVerifyExploreIsAPlainVerify: /maul verify explore, retired with the
// cost ladder, is recorded and answered as a plain /maul verify, on the
// issue and on a pull request: the request never says explore, and starts
// the next round as any request does.
func TestVerifyExploreIsAPlainVerify(t *testing.T) {
	for _, surface := range []string{"issue", "pull request"} {
		t.Run(surface, func(t *testing.T) {
			e, name := newEvaluatorEnv(t, 0.4)
			e.evaluateReporting(anomalyReport)
			e.acceptManual(name)
			e.observedRound(name, 1)
			replies := e.request(name, surface, "/maul verify explore")
			if len(replies) != 1 || strings.Contains(replies[0], "explore") {
				t.Errorf("replies %q, want one, a plain request's", replies)
			}
			if r := e.verification(name).Requested; r != nil && r.Explore {
				t.Errorf("request %+v, want recorded without explore", r)
			}
			if pv := e.roundOf(name, 2); pv.Annotations[annotationVerificationTrigger] != verificationByRequest {
				t.Errorf("round 2 started by %q, want the request", pv.Annotations[annotationVerificationTrigger])
			}
		})
	}
}

// TestHeldFindingThroughARequestedRound (G-B-LIB-1 on the live path): an
// outcome round 1 saw not to hold at the heads stays held when an approver
// asks for another round at the same heads, automatic fixes stopped, and
// that round's evaluator sees it hold: unsure (held-finding), Unconfirmed,
// while the outcome it saw hold before passes.
func TestHeldFindingThroughARequestedRound(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.failRound(name, failingReport, []toolwire.LedgerEntry{
		httpEntry(2, "GET", "/version", `{"commit":"dev"}`),
		httpEntry(3, "GET", "/healthz", "ok"),
	})
	e.request(name, "issue", "/maul verify stop")
	e.evaluateReporting(evaluationReport)
	e.request(name, "issue", "/maul verify")
	e.observedRound(name, 2)
	v := e.verification(name)
	if v.Round != 2 || v.OutcomesRound != 2 || v.State != v1alpha1.VerificationUnconfirmed {
		t.Fatalf("verification = round %d judged %d, %s; want round 2 judged Unconfirmed", v.Round, v.OutcomesRound,
			v.State)
	}
	for _, r := range v.Outcomes {
		switch r.ID {
		case oVersion:
			if r.Result != v1alpha1.OutcomeUnsure || r.Reason != acceptance.ReasonHeldFinding {
				t.Errorf("%s = %+v, want held from round 1", r.ID, r)
			}
		case oHealthy:
			if r.Result != v1alpha1.OutcomePassed {
				t.Errorf("%s = %+v, want passed", r.ID, r)
			}
		}
	}
	if n := len(e.runsOf(name, v1alpha1.IntentStageRevise)); n != 0 {
		t.Errorf("%d fix rounds while stopped, want none", n)
	}
}
