// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"bytes"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/checks"
	"github.com/devthenet-labs/maul/internal/envelope"
	"github.com/devthenet-labs/maul/internal/report"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// noPassEvaluation is goodEvaluation with its pass withdrawn: a report that
// passes nothing, so maul has no check to ask for.
var noPassEvaluation = strings.Replace(goodEvaluation, "verdict: pass", "verdict: unsure", 1)

// contraryEvaluation is goodEvaluation whose leaderboard verdict names a
// seq where the outcome did not hold.
var contraryEvaluation = strings.Replace(goodEvaluation, "    verdict: unsure\n",
	"    verdict: unsure\n    contrary: [16]\n", 1)

// TestEvaluateAsksForTheChecksOfUncoveredPasses (gate 4, G4-B-LIB-1): a
// report that passes an outcome with no check bound to it is not left to
// hold a correct build Unconfirmed. The evaluator's session is resumed once
// with the cover ask, naming that outcome; the report it rewrites, proposing
// a check of it, is the stage's, and that check is confirmed.
func TestEvaluateAsksForTheChecksOfUncoveredPasses(t *testing.T) {
	var out bytes.Buffer
	cfg, ws := roleConfig(t, PhaseEvaluate, evaluateHandoff, &out)
	brokeredClaude(t, &cfg)
	cfg.InvestigateMaxTurns = 100
	tools := useTools(t, &cfg)
	digest := checks.Digest(rateBattleCheck)
	rewritten := proposingEvaluation("rate-battle.api", "rate-battle", digest)
	fx := &fakeExec{steps: []step{
		{ws: ws, stdout: streamSuccess, writes: map[string]string{"scratch/report.md": goodEvaluation}},
		{ws: ws, stdout: streamResumed, writes: map[string]string{
			"scratch/report.md":                   rewritten,
			"scratch/checks/rate-battle-api.json": string(checks.Canonical(rateBattleCheck)),
		}},
	}}
	e := onlyEvent(t, cfg, fx, &out).Evaluation
	if e == nil || e.Outcome != envelope.OutcomeOK || e.ReportMarkdown != rewritten {
		t.Fatalf("evaluation = %+v, want the report rewritten with the check", e)
	}
	if len(e.Proposed) != 1 || e.Proposed[0].Digest != digest || e.Proposed[0].Outcome != "rate-battle" {
		t.Errorf("proposed = %+v, want the new check of rate-battle", e.Proposed)
	}
	if len(fx.specs) != 2 || !slices.Contains(fx.specs[1].Argv, "--resume") {
		t.Fatalf("commands = %d, want the stage and one resumed ask", len(fx.specs))
	}
	ask := strings.Join(strings.Fields(argvFlag(t, fx.specs[1].Argv, "-p")), " ")
	for _, want := range []string{"Outcomes you passed with no check", "- `rate-battle`", "This is ask 1 of at most 2."} {
		if !strings.Contains(ask, want) {
			t.Errorf("the ask lacks %q:\n%s", want, ask)
		}
	}
	if strings.Contains(ask, "`leaderboard`") {
		t.Errorf("the ask names leaderboard, which the report leaves unsure:\n%s", ask)
	}
	// What is left of the stage's 100 turns: the first run took 7.
	if got := argvFlag(t, fx.specs[1].Argv, "--max-turns"); got != "93" {
		t.Errorf("ask --max-turns = %s, want the 93 the stage has left", got)
	}
	if e.NumTurns != 7+2 {
		t.Errorf("num turns = %d, want both runs' 9", e.NumTurns)
	}
	var confirm toolwire.ConfirmRequest
	tools.request(t, toolwire.VerbConfirm, &confirm)
	if !slices.Equal(confirm.Digests, []string{strings.TrimPrefix(digest, "sha256:")}) {
		t.Errorf("confirmed %v, want the new check", confirm.Digests)
	}
}

// TestEvaluateCoverAskKeepsTheReport: an ask whose rewrite is not
// acceptable keeps the report maul accepted before, as it was: a rewrite
// that does not parse, one that changes a verdict maul did not ask about,
// and one that drops a contrary seq. A rewrite that covers nothing ends the
// asks after the first.
func TestEvaluateCoverAskKeepsTheReport(t *testing.T) {
	for name, tt := range map[string]struct{ before, rewrite string }{
		"unparsable":         {goodEvaluation, "---\nverdicts: [\n---\n"},
		"another verdict":    {goodEvaluation, strings.Replace(goodEvaluation, "verdict: unsure", "verdict: fail\n    ledger: [3]", 1)},
		"a contrary lost":    {contraryEvaluation, goodEvaluation},
		"nothing covered":    {goodEvaluation, goodEvaluation},
		"the pass withdrawn": {goodEvaluation, noPassEvaluation},
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			cfg, ws := roleConfig(t, PhaseEvaluate, evaluateHandoff, &out)
			brokeredClaude(t, &cfg)
			fx := &fakeExec{steps: []step{
				{ws: ws, stdout: streamSuccess, writes: map[string]string{"scratch/report.md": tt.before}},
				{ws: ws, stdout: streamResumed, writes: map[string]string{"scratch/report.md": tt.rewrite}},
			}}
			e := onlyEvent(t, cfg, fx, &out).Evaluation
			if e == nil || e.Outcome != envelope.OutcomeOK {
				t.Fatalf("evaluation = %+v, want ok", e)
			}
			want := tt.before
			if name == "the pass withdrawn" || name == "nothing covered" {
				want = tt.rewrite // an acceptable rewrite is the stage's
			}
			if e.ReportMarkdown != want {
				t.Errorf("report = %q, want %q", e.ReportMarkdown, want)
			}
			if len(fx.specs) != 2 {
				t.Errorf("commands = %d, want the stage and one ask", len(fx.specs))
			}
		})
	}
}

// TestEvaluateNoCoverAsk: no ask is sent for a report whose passes all have
// a check maul can count, one that passes nothing, a pass maul passed in
// the previous round (a saved check covers it), a pass of an outcome a
// person judges, or on a harness that cannot continue a session (codex).
func TestEvaluateNoCoverAsk(t *testing.T) {
	digest := checks.Digest(rateBattleCheck)
	checkFile := string(checks.Canonical(rateBattleCheck))
	prior := evalHandoff(func(h *EvaluateHandoff) {
		h.Round, h.PriorRound = 2, 1
		h.PriorVerdicts = []PriorVerdict{{Outcome: "rate-battle", Result: string(acceptance.ResultPassed),
			Reason: "confirmed"}}
	})
	manual := evalHandoff(func(h *EvaluateHandoff) {
		h.Outcomes[0].Manual, h.Outcomes[0].Reason = true, "a person judges the feel"
	})
	for name, tt := range map[string]struct {
		handoff, report, harness string
		files                    map[string]string
	}{
		"covered": {evaluateHandoff, proposingEvaluation("rate-battle.api", "rate-battle", digest), "claude",
			map[string]string{"scratch/checks/rate-battle-api.json": checkFile}},
		"no pass":       {evaluateHandoff, noPassEvaluation, "claude", nil},
		"passed before": {prior, goodEvaluation, "claude", nil},
		"a person's":    {manual, goodEvaluation, "claude", nil},
		"codex":         {evaluateHandoff, goodEvaluation, "codex", nil},
	} {
		t.Run(name, func(t *testing.T) {
			var out, log bytes.Buffer
			cfg, ws := roleConfig(t, PhaseEvaluate, tt.handoff, &out)
			brokeredClaude(t, &cfg)
			if tt.harness == "codex" {
				cfg.InvestigateHarness = "codex"
				cfg.InvestigateModel = "openai/gpt-5.3-codex"
				cfg.BrokerTokenFile, cfg.CodexBaseURL = codexTokenFile(t), "http://broker.maul.svc:8080/openai/v1"
				cfg.Log = slog.New(slog.NewTextHandler(&log, nil))
			}
			writes := map[string]string{"scratch/report.md": tt.report}
			for k, v := range tt.files {
				writes[k] = v
			}
			stdout := streamSuccess
			if tt.harness == "codex" {
				stdout = codexStream
			}
			fx := &fakeExec{steps: []step{{ws: ws, stdout: stdout, writes: writes}}}
			e := onlyEvent(t, cfg, fx, &out).Evaluation
			if e == nil || e.Outcome != envelope.OutcomeOK || e.ReportMarkdown != tt.report {
				t.Fatalf("evaluation = %+v, want the report as written", e)
			}
			if len(fx.specs) != 1 {
				t.Errorf("commands = %d, want the stage alone", len(fx.specs))
			}
		})
	}
}

// g6Handoff is the outcomes and previous round of gate 4's G6 round 2
// (render-library-100-ev-r2-a1-input): ten outcomes, repeat-rule passed in
// round 1 on its saved check repeat-rule-all, repeat-bad-pattern failed
// there on its saved red check, not yet proof.
func g6Handoff() EvaluateHandoff {
	ids := []string{"repeat-rule", "repeat-others-kept", "repeat-no-mutation", "repeat-cycles", "repeat-16",
		"repeat-1", "repeat-bad-length", "repeat-bad-track", "repeat-bad-pattern", "validate-kept"}
	h := EvaluateHandoff{Round: 2, PriorRound: 1}
	for _, id := range ids {
		kind := acceptance.OutcomeKindChange
		if id == "validate-kept" {
			kind = acceptance.OutcomeKindKeep
		}
		h.Outcomes = append(h.Outcomes, acceptance.PlanOutcome{ID: id, Outcome: id, Kind: kind})
		result, reason := "unsure", "model-only-pass"
		switch id {
		case "repeat-rule":
			result, reason = "passed", "confirmed"
		case "repeat-bad-pattern":
			reason = "suspected-defect"
		}
		h.PriorVerdicts = append(h.PriorVerdicts, PriorVerdict{Outcome: id, Result: result, Reason: reason})
	}
	h.PriorReplay = []PriorReplay{{Check: "repeat-bad-pattern-all", Outcome: "repeat-bad-pattern", Result: "fail"}}
	h.NotYetProof = []NotYetProof{{Outcome: "repeat-bad-pattern", Check: "repeat-bad-pattern-all"}}
	return h
}

// g6Report is G6 round 2's report as its account states it: all ten
// passed, one new check proposed, for repeat-bad-pattern.
func g6Report(h EvaluateHandoff) *evaluationReport {
	e := &report.Evaluation{Proposed: []report.ProposedCheck{{ID: "repeat-bad-pattern-valid",
		Outcome: "repeat-bad-pattern", Digest: "sha256:4759e2b0ee0efb1110a0d5b693784c36f1c3a20c5e313e56a2dff9d9d1a14971"}}}
	for _, o := range h.Outcomes {
		e.Verdicts = append(e.Verdicts, report.OutcomeVerdict{ID: o.ID, Outcome: o.ID, Verdict: report.VerdictPass,
			Ledger: []int32{14}})
	}
	return &evaluationReport{Evaluation: e}
}

// TestUncoveredPassesG6: of G6 round 2's ten passes, maul asks for the
// eight that rest on the exploring run alone: not repeat-rule, which a
// saved check passed in round 1, nor repeat-bad-pattern, which the report
// proposes a check of.
func TestUncoveredPassesG6(t *testing.T) {
	h := g6Handoff()
	got := uncoveredPasses(h, g6Report(h))
	want := []string{"repeat-others-kept", "repeat-no-mutation", "repeat-cycles", "repeat-16", "repeat-1",
		"repeat-bad-length", "repeat-bad-track", "validate-kept"}
	if !slices.Equal(got, want) {
		t.Errorf("uncoveredPasses = %v, want %v", got, want)
	}
}

// TestUncoveredPasses: what counts as a check maul can count, outcome by
// outcome.
func TestUncoveredPasses(t *testing.T) {
	base := func() (EvaluateHandoff, *evaluationReport) {
		h := EvaluateHandoff{Outcomes: []acceptance.PlanOutcome{{ID: "a", Kind: acceptance.OutcomeKindChange}}}
		e := &evaluationReport{Evaluation: &report.Evaluation{Verdicts: []report.OutcomeVerdict{
			{ID: "a", Verdict: report.VerdictPass, Ledger: []int32{3}}}}}
		return h, e
	}
	for _, tt := range []struct {
		name string
		edit func(*EvaluateHandoff, *evaluationReport)
		want []string
	}{
		{"a pass with nothing", func(*EvaluateHandoff, *evaluationReport) {}, []string{"a"}},
		{"a green proposal", func(_ *EvaluateHandoff, e *evaluationReport) {
			e.Proposed = []report.ProposedCheck{{ID: "a", Outcome: "a", Digest: "sha256:x"}}
		}, nil},
		{"only a red proposal", func(_ *EvaluateHandoff, e *evaluationReport) {
			e.Proposed = []report.ProposedCheck{{ID: "a", Outcome: "a", Digest: "sha256:x", Expect: report.ExpectFail}}
		}, []string{"a"}},
		{"a proposal of another outcome", func(_ *EvaluateHandoff, e *evaluationReport) {
			e.Proposed = []report.ProposedCheck{{ID: "b", Outcome: "b", Digest: "sha256:x"}}
		}, []string{"a"}},
		{"a dropped proposal", func(_ *EvaluateHandoff, e *evaluationReport) {
			e.dropped = []droppedProposal{{id: "a", outcome: "a", reason: "not a check"}}
		}, []string{"a"}},
		{"unsure", func(_ *EvaluateHandoff, e *evaluationReport) { e.Verdicts[0].Verdict = report.VerdictUnsure }, nil},
		{"fail", func(_ *EvaluateHandoff, e *evaluationReport) { e.Verdicts[0].Verdict = report.VerdictFail }, nil},
		{"manual", func(h *EvaluateHandoff, _ *evaluationReport) { h.Outcomes[0].Manual = true }, nil},
		{"not an outcome of the round", func(_ *EvaluateHandoff, e *evaluationReport) { e.Verdicts[0].ID = "z" }, nil},
		{"passed in the previous round", func(h *EvaluateHandoff, _ *evaluationReport) {
			h.PriorVerdicts = []PriorVerdict{{Outcome: "a", Result: "passed"}}
		}, nil},
		{"accepted by a person in the previous round", func(h *EvaluateHandoff, _ *evaluationReport) {
			h.PriorVerdicts = []PriorVerdict{{Outcome: "a", Result: "accepted"}}
		}, []string{"a"}},
		{"a saved check replayed", func(h *EvaluateHandoff, _ *evaluationReport) {
			h.PriorReplay = []PriorReplay{{Check: "a", Outcome: "a", Result: "pass"}}
		}, nil},
		{"a saved check not yet proof", func(h *EvaluateHandoff, _ *evaluationReport) {
			h.PriorReplay = []PriorReplay{{Check: "a", Outcome: "a", Result: "pass"}}
			h.NotYetProof = []NotYetProof{{Outcome: "a", Check: "a"}}
		}, []string{"a"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h, e := base()
			tt.edit(&h, e)
			if got := uncoveredPasses(h, e); !slices.Equal(got, tt.want) {
				t.Errorf("uncoveredPasses = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestCoverRewriteRefused: a rewrite may add proposals of the asked
// outcomes and change their verdicts, keeping their contrary seqs; any
// other change keeps the report before.
func TestCoverRewriteRefused(t *testing.T) {
	before := func() *report.Evaluation {
		return &report.Evaluation{
			Verdicts: []report.OutcomeVerdict{
				{ID: "a", Verdict: report.VerdictPass, Ledger: []int32{3}, Contrary: []int32{9}},
				{ID: "b", Verdict: report.VerdictFail, Ledger: []int32{4}},
			},
			Proposed:  []report.ProposedCheck{{ID: "b", Outcome: "b", Digest: "sha256:bb", Expect: report.ExpectFail}},
			Disputes:  []report.Dispute{{CheckDigest: "sha256:dd", Reason: "r", Ledger: []int32{5}}},
			Retire:    []report.Retire{{CheckDigest: "sha256:ee", Reason: "r"}},
			Anomalies: []report.Anomaly{{Summary: "s", Route: "/", Ledger: []int32{6}}},
		}
	}
	asked := []string{"a"}
	for _, tt := range []struct {
		name    string
		edit    func(*report.Evaluation)
		refused bool
	}{
		{"unchanged", func(*report.Evaluation) {}, false},
		{"a proposal of the asked outcome", func(e *report.Evaluation) {
			e.Proposed = append(e.Proposed, report.ProposedCheck{ID: "a", Outcome: "a", Digest: "sha256:aa"})
		}, false},
		{"the asked verdict withdrawn, its note and ledger changed", func(e *report.Evaluation) {
			e.Verdicts[0].Verdict, e.Verdicts[0].Note, e.Verdicts[0].Ledger = report.VerdictUnsure, "n", []int32{3, 12}
		}, false},
		{"a contrary seq added to the asked verdict", func(e *report.Evaluation) {
			e.Verdicts[0].Contrary = []int32{9, 13}
		}, false},
		{"empty lists for nil", func(e *report.Evaluation) {
			e.Verdicts[1].Contrary = []int32{}
		}, false},
		{"a contrary seq of the asked verdict dropped", func(e *report.Evaluation) {
			e.Verdicts[0].Contrary = nil
		}, true},
		{"another verdict changed", func(e *report.Evaluation) { e.Verdicts[1].Verdict = report.VerdictPass }, true},
		{"another verdict's note changed", func(e *report.Evaluation) { e.Verdicts[1].Note = "n" }, true},
		{"a verdict left out", func(e *report.Evaluation) { e.Verdicts = e.Verdicts[:1] }, true},
		{"a verdict renamed", func(e *report.Evaluation) { e.Verdicts[1].ID = "c" }, true},
		{"a proposal dropped", func(e *report.Evaluation) { e.Proposed = nil }, true},
		{"a proposal's expectation changed", func(e *report.Evaluation) { e.Proposed[0].Expect = "" }, true},
		{"a proposal of another outcome added", func(e *report.Evaluation) {
			e.Proposed = append(e.Proposed, report.ProposedCheck{ID: "b2", Outcome: "b", Digest: "sha256:b2"})
		}, true},
		{"a dispute dropped", func(e *report.Evaluation) { e.Disputes = nil }, true},
		{"a retirement added", func(e *report.Evaluation) {
			e.Retire = append(e.Retire, report.Retire{CheckDigest: "sha256:ff"})
		}, true},
		{"an anomaly dropped", func(e *report.Evaluation) { e.Anomalies = nil }, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			after := before()
			tt.edit(after)
			why := coverRewriteRefused(before(), after, asked)
			if (why != "") != tt.refused {
				t.Errorf("coverRewriteRefused = %q, want refused %v", why, tt.refused)
			}
		})
	}
}
