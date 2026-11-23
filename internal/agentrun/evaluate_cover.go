// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/envelope"
	"github.com/devthenet-labs/maul/internal/harness"
	"github.com/devthenet-labs/maul/internal/report"
	"github.com/devthenet-labs/maul/internal/templates"
)

// coverRounds bounds how many times an evaluator is asked, in its own
// session, for the checks of the outcomes its accepted report passes with
// none (coverChecks).
const coverRounds = 2

// coverChecks asks the evaluator, in its own session, for a check of each
// outcome its accepted report passes with no check maul can count
// (uncoveredPasses; gate 4, G4-B-LIB-1: a correct build was held
// Unconfirmed because its evaluator rested eight passes on one exploring
// run and proposed a check for none of them). Each ask is a resumed run
// under what is left of the stage's turns, wall clock and output tokens
// (repairWindow), at most coverRounds of them, stopping once an ask covers
// no further outcome.
//
// The rewritten report replaces the accepted one only when it parses and
// changes nothing but what was asked (coverRewriteRefused): new proposals
// of the asked outcomes, and those outcomes' verdicts, each keeping its
// contrary seqs. Anything else keeps the earlier report, so an ask can never
// lose a contrary, a dispute or a verdict maul did not ask about; maul puts
// that report back where the evaluator reads it, and the next ask, if any,
// says why the rewrite was refused. The stage's tool pod outlives its wall
// clock by the confirmation margin (intent.EvaluationToolsMargin), so an ask
// that spends the clock still leaves its checks time to confirm. A stage
// that ran out of its turns or its clock is not asked, nor is a harness
// that cannot continue a session (codex): either keeps its report as it is.
func (a *Agent) coverChecks(ctx context.Context, sr *stageRun, st *envelope.Stage, h EvaluateHandoff,
	e *evaluationReport, raw []byte) (*evaluationReport, []byte) {
	if sr.outOfTurns || sr.outOfTime {
		return e, raw
	}
	resumer, ok := sr.h.(harness.Resumer)
	if !ok {
		return e, raw
	}
	if sr.used == 0 {
		// No repair ran, which would have counted the first run's turns.
		sr.used = countedTurns(st.NumTurns, sr.req.MaxTurns)
	}
	refused := ""
	for round := 1; round <= coverRounds; round++ {
		asked := uncoveredPasses(h, e)
		if len(asked) == 0 {
			return e, raw
		}
		if why := a.repairWindow(ctx, sr, st); why != "" {
			a.cfg.Log.Info("outcomes passed with no check; not asking for them", "phase", a.cfg.Phase,
				"outcomes", strings.Join(asked, ","), "why", why)
			return e, raw
		}
		if !a.coverRound(ctx, sr, st, resumer, h, e, coverAsk{outcomes: asked, refused: refused, round: round}) {
			return e, raw
		}
		next, nextRaw, outcome, detail := readReportAs(a.cfg.evaluationPath(), readReport, a.parseEvaluation)
		refused = detail
		if outcome == envelope.OutcomeOK {
			refused = coverRewriteRefused(e.Evaluation, next.Evaluation, asked)
		}
		if refused == "" {
			covered := len(asked) - len(uncoveredPasses(h, next))
			e, raw = next, nextRaw
			if covered == 0 {
				return e, raw
			}
			continue
		}
		a.cfg.Log.Warn("refused the report rewritten for the outcomes passed with no check; keeping the one before",
			"phase", a.cfg.Phase, "round", round, "reason", oneLine(refused, repairFailureBytes))
		if sr.rec != nil {
			sr.rec.Notice("maul refused the report rewritten in ask %d and kept the one before: %s", round,
				oneLine(refused, repairFailureBytes))
		}
		// The next ask starts from the report maul kept, put back where
		// the evaluator reads it.
		if err := os.WriteFile(a.cfg.evaluationPath(), raw, 0o644); err != nil {
			a.cfg.Log.Warn("could not put the kept report back; asking no more", "error", err)
			return e, raw
		}
	}
	return e, raw
}

// coverAsk is one ask's terms: the outcomes it names, why maul refused the
// report the ask before it rewrote ("" for none), and its ordinal.
type coverAsk struct {
	outcomes []string
	refused  string
	round    int
}

// coverRound runs one ask: the evaluator's session resumed with the cover
// prompt, under what is left of the stage's turns, wall clock and output
// tokens, its spend added to st. It reports whether the run started and the
// stage was not cancelled during it.
func (a *Agent) coverRound(ctx context.Context, sr *stageRun, st *envelope.Stage, resumer harness.Resumer,
	h EvaluateHandoff, e *evaluationReport, ask coverAsk) bool {
	asked, round := ask.outcomes, ask.round
	prompt, err := templates.RenderCoverPrompt(templates.CoverPrompt{
		ReportPath: a.cfg.evaluationPath(), ChecksDir: a.cfg.checksDir(), Workspace: h.Tools.Workspace != nil,
		Outcomes: asked, Dropped: coverDropped(e.dropped, asked), Refused: ask.refused, Round: round,
		Rounds: coverRounds,
	})
	if err != nil {
		a.cfg.Log.Warn("could not render the ask for outcomes passed with no check", "error", err)
		return false
	}
	env, gateway, err := a.brokerEnv(sr.h)
	if err != nil {
		a.cfg.Log.Warn("could not ask for outcomes passed with no check", "error", err)
		return false
	}
	req := sr.req
	req.Prompt, req.Env, req.Gateway = prompt, env, gateway
	if left := sr.turns - sr.used; sr.turns > 0 {
		// Every turn left, but a later ask's minimum while that leaves this
		// one its own.
		later := (coverRounds - round) * repairMinTurns
		req.MaxTurns = max(left-later, min(left, repairMinTurns))
	}
	budget := 0
	if sr.budget > 0 {
		budget = sr.budget - st.Usage.OutputTokens
	}
	session := st.SessionID
	if session == "" {
		session = sr.req.SessionID
	}
	if sr.rec != nil {
		sr.rec.AddSecrets(a.scrub...)
		sr.rec.Notice("maul found outcomes passed with no check (%s); asking the agent for their checks, ask %d of %d",
			strings.Join(asked, ", "), round, coverRounds)
	}
	a.cfg.Log.Info("asking the agent for the checks of the outcomes it passed with none", "phase", a.cfg.Phase,
		"round", round, "outcomes", strings.Join(asked, ","), "session", session)
	res, _, _ := a.run(ctx, sr, pinCLI(resumer.ResumeSpec(a.workDir(), session, req), sr.cli),
		sr.timeout-time.Since(sr.started), budget)
	var spent envelope.Stage
	a.fillStage(&spent, sr.h, res)
	addRepair(st, spent)
	sr.used += countedTurns(spent.NumTurns, req.MaxTurns)
	if sr.delegated != "" {
		// The report it rewrote is not read; the one before stands.
		a.cfg.Log.Warn("an ask for checks delegated what the stage was not granted; keeping the report before",
			"phase", a.cfg.Phase, "round", round, "reason", sr.delegated)
		return false
	}
	return ctx.Err() == nil
}

// uncoveredPasses are the outcomes e passes that maul has no check to
// count for, in the report's order: each approved, not judged by a person,
// with no proposal of e bound to it that ran green (a dropped one is no
// longer in e), and not covered by a saved check: neither passed by maul in
// the previous round nor bound to a saved check of that round's replay
// (one saved to show a defect that is not yet proof aside, and one the
// substance rules have come to refuse, with the pass a round before may
// have given on it: the prompt already asks a new check of each).
func uncoveredPasses(h EvaluateHandoff, e *evaluationReport) []string {
	saved := map[string]bool{}
	for _, v := range h.PriorVerdicts {
		// A pass of the previous round may have stood on a saved check
		// the substance rules have since come to refuse, which counts for
		// nothing now: only a saved check of this round's replay covers
		// such an outcome.
		refused := slices.ContainsFunc(h.NotYetProof, func(n NotYetProof) bool {
			return n.Refused && n.Outcome == v.Outcome
		})
		if v.Result == string(acceptance.ResultPassed) && !refused {
			saved[v.Outcome] = true
		}
	}
	for _, r := range h.PriorReplay {
		if !slices.ContainsFunc(h.NotYetProof, func(n NotYetProof) bool { return n.Check == r.Check }) {
			saved[r.Outcome] = true
		}
	}
	var out []string
	for _, v := range e.Verdicts {
		i := slices.IndexFunc(h.Outcomes, func(o acceptance.PlanOutcome) bool { return o.ID == v.ID })
		if v.Verdict != report.VerdictPass || i < 0 || h.Outcomes[i].Manual || saved[v.ID] ||
			slices.Contains(out, v.ID) ||
			slices.ContainsFunc(e.Proposed, func(p report.ProposedCheck) bool {
				return p.Outcome == v.ID && !p.ExpectsFail()
			}) {
			continue
		}
		out = append(out, v.ID)
	}
	return out
}

// coverDropped are the proposals maul dropped that were bound to one of the
// asked outcomes, with why.
func coverDropped(dropped []droppedProposal, asked []string) []templates.CoverDropped {
	var out []templates.CoverDropped
	for _, d := range dropped {
		if slices.Contains(asked, d.outcome) {
			out = append(out, templates.CoverDropped{ID: d.id, Outcome: d.outcome, Reason: d.reason})
		}
	}
	return out
}

// coverRewriteRefused says why after, the report rewritten for the asked
// outcomes, may not replace before, or "" when it may: it must keep every
// verdict but those of the asked outcomes exactly, keep each asked
// outcome's contrary seqs, keep every proposal, dispute, retirement and
// anomaly, and add proposals only of the asked outcomes. So the ask can add
// evidence, or withdraw a pass, and nothing else: no verdict maul did not
// ask about, no contrary and no dispute is lost to a rewrite.
func coverRewriteRefused(before, after *report.Evaluation, asked []string) string {
	if len(after.Verdicts) != len(before.Verdicts) {
		return fmt.Sprintf("it has %d verdicts, not %d", len(after.Verdicts), len(before.Verdicts))
	}
	for _, b := range before.Verdicts {
		i := slices.IndexFunc(after.Verdicts, func(v report.OutcomeVerdict) bool { return v.ID == b.ID })
		switch {
		case i < 0:
			return fmt.Sprintf("it has no verdict of %s", b.ID)
		case !slices.Contains(asked, b.ID) && !sameVerdict(after.Verdicts[i], b):
			return fmt.Sprintf("it changed the verdict of %s, which maul did not ask about", b.ID)
		case !containsAll(after.Verdicts[i].Contrary, b.Contrary):
			return fmt.Sprintf("it dropped a contrary seq of %s", b.ID)
		}
	}
	if !slices.EqualFunc(after.Disputes, before.Disputes, sameDispute) {
		return "it changed the disputes"
	}
	if !slices.EqualFunc(after.Retire, before.Retire, sameRetire) {
		return "it changed the retirements"
	}
	if !slices.EqualFunc(after.Anomalies, before.Anomalies, sameAnomaly) {
		return "it changed the anomalies"
	}
	for _, b := range before.Proposed {
		if !slices.ContainsFunc(after.Proposed, func(p report.ProposedCheck) bool { return sameProposal(p, b) }) {
			return fmt.Sprintf("it dropped the proposal %s", b.ID)
		}
	}
	for _, p := range after.Proposed {
		isNew := !slices.ContainsFunc(before.Proposed, func(b report.ProposedCheck) bool { return sameProposal(p, b) })
		if isNew && !slices.Contains(asked, p.Outcome) {
			return fmt.Sprintf("it added the proposal %s of %s, which maul did not ask about", p.ID, p.Outcome)
		}
	}
	return ""
}

// sameVerdict, sameDispute, sameRetire and sameAnomaly compare what maul
// judges a round by (acceptance.Decide, agentresult): a reworded note,
// reason or summary is the same entry, any other change is not.
func sameVerdict(a, b report.OutcomeVerdict) bool {
	return a.ID == b.ID && a.Verdict == b.Verdict && slices.Equal(a.Ledger, b.Ledger) &&
		slices.Equal(a.Contrary, b.Contrary)
}

func sameDispute(a, b report.Dispute) bool {
	return a.CheckDigest == b.CheckDigest && a.Replacement == b.Replacement && slices.Equal(a.Ledger, b.Ledger)
}

func sameRetire(a, b report.Retire) bool { return a.CheckDigest == b.CheckDigest }

func sameAnomaly(a, b report.Anomaly) bool {
	return a.Route == b.Route && slices.Equal(a.Ledger, b.Ledger)
}

// sameProposal compares two resolved proposals by what maul confirms: the
// check's id, outcome, digest and expectation.
func sameProposal(a, b report.ProposedCheck) bool {
	return a.ID == b.ID && a.Outcome == b.Outcome && a.ExpectsFail() == b.ExpectsFail() &&
		strings.TrimPrefix(a.Digest, "sha256:") == strings.TrimPrefix(b.Digest, "sha256:")
}

// containsAll reports whether got holds every seq of want.
func containsAll(got, want []int32) bool {
	for _, s := range want {
		if !slices.Contains(got, s) {
			return false
		}
	}
	return true
}
