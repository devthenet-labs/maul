// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/agentresult"
	"github.com/devthenet-labs/maul/internal/checks"
	"github.com/devthenet-labs/maul/internal/envelope"
	"github.com/devthenet-labs/maul/internal/harness"
	"github.com/devthenet-labs/maul/internal/report"
	"github.com/devthenet-labs/maul/internal/templates"
)

// Check review handoff bounds. A round reviews at most
// agentresult.CheckReviewMaxAnswers checks, each at most
// v1alpha1.MaxSavedCheckBytes, beside an outcome of at most
// checkReviewOutcomeMaxBytes.
const (
	checkReviewOutcomeMaxBytes = 4 << 10
	checkReviewHandoffMaxBytes = 512 << 10
)

// CheckReviewHandoff is the checkreview stage's handoff (input/issue.md):
// the new checks of a round, each with the outcome it is to prove, which
// the intent controller encodes as strict JSON (encoding/json of this
// type).
type CheckReviewHandoff struct {
	Reviews []CheckReviewItem `json:"reviews"`
}

// CheckReviewItem is one review: a check and its outcome's text, each keyed
// by its digest. Check is the check's JSON (checks.Canonical, at most
// v1alpha1.MaxSavedCheckBytes), whose digest (checks.Digest) must be
// CheckDigest; the reviewer reads it as maul renders it
// (templates.RenderCheckReviewPrompt: no id, no outcome id, no step names).
// The digests key the answer and never reach the session.
//
// Expect is fail for a check the evaluator proposed to show a defect, red
// by assertion in maul's own runs: the reviewer is asked the red question
// (templates.CheckReviewPrompt.ExpectFail), told which step failed
// (FailedStep, 1-based, of the check as maul renders it). Both are unset for
// a check confirmed green.
type CheckReviewItem struct {
	CheckDigest   string `json:"check_digest"`
	OutcomeDigest string `json:"outcome_digest"`
	Outcome       string `json:"outcome"`
	Check         string `json:"check"`
	Expect        string `json:"expect,omitempty"`
	FailedStep    int    `json:"failed_step,omitempty"`
}

// checkSteps is how many steps c has as a reviewer reads it: an http or
// browser check's steps, an exec check's one program run.
func checkSteps(c checks.Check) int {
	switch {
	case c.HTTP != nil:
		return len(c.HTTP.Steps)
	case c.Browser != nil:
		return len(c.Browser.Steps)
	case c.Exec != nil:
		return 1
	}
	return 0
}

// validExpectation holds an item's expectation: none, or fail with the
// step that failed, one of the check's.
func validExpectation(it CheckReviewItem, c checks.Check) error {
	switch it.Expect {
	case "":
		if it.FailedStep != 0 {
			return errors.New("failed_step is set on a check confirmed green")
		}
		return nil
	case "fail":
	default:
		return fmt.Errorf("expect %q is not fail", it.Expect)
	}
	if it.FailedStep < 1 || it.FailedStep > checkSteps(c) {
		return fmt.Errorf("failed_step %d is not a step of the check", it.FailedStep)
	}
	return nil
}

// checkReviewItem is a review as the stage runs it: the handoff's item and
// its check, parsed.
type checkReviewItem struct {
	CheckReviewItem
	check checks.Check
}

// checkReviewInput reads and checks the checkreview stage's handoff: strict
// JSON, one to agentresult.CheckReviewMaxAnswers reviews, each with
// well-formed digests, a non-empty bounded outcome, a check that parses and
// has its digest, and no (check, outcome) pair twice.
func (a *Agent) checkReviewInput() ([]checkReviewItem, error) {
	raw, err := a.readHandoff(checkReviewHandoffMaxBytes)
	if err != nil {
		return nil, err
	}
	var h CheckReviewHandoff
	if err := decodeHandoff(raw, "check review", &h); err != nil {
		return nil, err
	}
	items, err := validReviews(h.Reviews)
	if err != nil {
		return nil, fmt.Errorf("input handoff: %w", err)
	}
	return items, nil
}

// validReviews holds a handoff's reviews to their bounds and parses each
// check.
func validReviews(in []CheckReviewItem) ([]checkReviewItem, error) {
	switch {
	case len(in) == 0:
		return nil, errors.New("the check review handoff has no reviews")
	case len(in) > agentresult.CheckReviewMaxAnswers:
		return nil, fmt.Errorf("the check review handoff has %d reviews, over %d", len(in),
			agentresult.CheckReviewMaxAnswers)
	}
	type pair struct{ check, outcome string }
	seen := make(map[pair]bool, len(in))
	items := make([]checkReviewItem, 0, len(in))
	var errs []error
	for i, it := range in {
		refuse := func(format string, args ...any) {
			errs = append(errs, fmt.Errorf("reviews[%d]: "+format, append([]any{i}, args...)...))
		}
		if !report.ValidDigest(it.CheckDigest) {
			refuse("check_digest %q is not sha256:<64 hex>", it.CheckDigest)
		}
		if !report.ValidDigest(it.OutcomeDigest) {
			refuse("outcome_digest %q is not sha256:<64 hex>", it.OutcomeDigest)
		}
		switch {
		case strings.TrimSpace(it.Outcome) == "":
			refuse("outcome is empty")
		case len(it.Outcome) > checkReviewOutcomeMaxBytes:
			refuse("outcome is %d bytes, over %d", len(it.Outcome), checkReviewOutcomeMaxBytes)
		}
		item := checkReviewItem{CheckReviewItem: it}
		switch {
		case strings.TrimSpace(it.Check) == "":
			refuse("check is empty")
		case len(it.Check) > v1alpha1.MaxSavedCheckBytes:
			refuse("check is %d bytes, over %d", len(it.Check), v1alpha1.MaxSavedCheckBytes)
		default:
			c, err := checks.Parse([]byte(it.Check))
			switch {
			case err != nil:
				refuse("check: %v", err)
			case checks.Digest(c) != it.CheckDigest:
				refuse("check's digest is %s, not its check_digest", checks.Digest(c))
			default:
				if err := validExpectation(it, c); err != nil {
					refuse("%v", err)
				}
			}
			item.check = c
		}
		p := pair{it.CheckDigest, it.OutcomeDigest}
		if seen[p] {
			refuse("repeats the check and outcome of an earlier review")
		}
		seen[p] = true
		items = append(items, item)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return items, nil
}

// unreviewedReason is the reason of the unclear agent-runner answers for a
// review that ended without a report it accepts.
const unreviewedReason = "maul: the review's report was refused and could not be repaired, so this check is " +
	"not saved this round"

// checkReview runs the checkreview stage: one fresh session per review, in
// order, each under the evaluate posture in the scratch directory, each
// writing its own report. Every answer is keyed by the digests the handoff
// carried, never by anything the session wrote. The stage's turns bound
// each session, whose first run keeps a reserve back for a repair of its
// report (reportReserve); its output tokens and wall clock are the whole
// run's, each session given what the earlier ones left. A session whose
// report is refused, or missing at its turn cap, and not repaired makes that
// check unclear (an unreviewed answer, maul's own) and the stage goes on to
// the next (S1 bug 8: one bad review dropped every check of the round). A
// session that ends any other way short of OK ends the stage on its
// outcome, with no answers.
func (a *Agent) checkReview(ctx context.Context, items []checkReviewItem) *envelope.CheckReview {
	ev := &envelope.CheckReview{Stage: envelope.Stage{
		Harness: a.cfg.InvestigateHarness,
		Model:   a.cfg.InvestigateModel,
	}}
	fail := func(outcome envelope.Outcome, detail string) *envelope.CheckReview {
		ev.Outcome, ev.Detail, ev.Answers = outcome, detail, nil
		return ev
	}
	h, ok := harness.ByID(a.cfg.InvestigateHarness)
	if !ok {
		return fail(envelope.OutcomeRuntimeError, fmt.Sprintf("unknown harness %q", a.cfg.InvestigateHarness))
	}
	maxTurns, budget, err := a.stageLimits()
	if err != nil {
		return fail(envelope.OutcomeRuntimeError, err.Error())
	}
	started := time.Now()
	firstTurns := maxTurns - reportReserve(maxTurns)
	var sr *stageRun
	for i, item := range items {
		n := i + 1
		session := func(detail string) string { return fmt.Sprintf("review %d of %d: %s", n, len(items), detail) }
		left := 0
		if budget > 0 {
			if left = budget - ev.Usage.OutputTokens; left <= 0 {
				return fail(envelope.OutcomeBudgetExceeded, session("the stage's output tokens are spent"))
			}
		}
		timeout := a.cfg.InvestigateTimeout - time.Since(started)
		if timeout <= 0 {
			return fail(envelope.OutcomeTimeout, session("the stage's wall clock is spent"))
		}
		path := a.cfg.reviewPath(n)
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fail(envelope.OutcomeRuntimeError, session(err.Error()))
		}
		prompt, err := templates.RenderCheckReviewPrompt(templates.CheckReviewPrompt{
			ReportPath: path,
			Limits: templates.StageLimits{
				MaxTurns: firstTurns, TokenBudget: budget, Timeout: a.cfg.InvestigateTimeout,
			},
			Outcome:    item.Outcome,
			Check:      item.check,
			ExpectFail: item.Expect == "fail",
			FailedStep: item.FailedStep,
		})
		if err != nil {
			return fail(envelope.OutcomeRuntimeError, session(err.Error()))
		}
		env, gateway, err := a.brokerEnv(h)
		if err != nil {
			return fail(envelope.OutcomeRuntimeError, session(err.Error()))
		}
		req := harness.PromptRequest{
			Prompt:    prompt,
			Model:     a.cliModel(a.cfg.InvestigateModel, a.cfg.InvestigateHarness),
			MaxTurns:  firstTurns,
			Sandbox:   harness.SandboxEvaluate,
			SessionID: a.newSessionID(),
			Env:       env,
			Gateway:   gateway,
		}
		if sr == nil {
			// One recorder for every session, so the transcript's sequence
			// numbers stay unique across the stage.
			sr = a.newStage(h, "", req, timeout, a.cfg.InvestigateIdleTimeout, left)
		} else {
			sr.req, sr.timeout, sr.budget = req, timeout, left
			if sr.rec != nil {
				sr.rec.AddSecrets(a.scrub...)
			}
		}
		sr.turns, sr.used, sr.outOfTurns, sr.example = maxTurns, 0, false, templates.ExampleCheckReviewReport

		st := envelope.Stage{Harness: ev.Harness, Model: ev.Model}
		res, idle, runErr := a.start(ctx, sr)
		a.fillStage(&st, h, res)
		if outcome, detail := a.runOutcome(h, res, idle, runErr); outcome != envelope.OutcomeOK {
			if !turnCapped(h, res, idle, runErr) {
				addSession(&ev.Stage, st)
				return fail(outcome, session(detail))
			}
			// Out of turns: ask the session for its report, with the turns
			// kept back.
			sr.outOfTurns = true
		}
		c, _, outcome, detail := settleReport(ctx, a, sr, &st, path, readReport, report.ParseCheckReview)
		addSession(&ev.Stage, st)
		if outcome != envelope.OutcomeOK {
			a.cfg.Log.Warn("a check review ended without a report maul accepts; that check is unclear",
				"phase", a.cfg.Phase, "review", n, "of", len(items), "outcome", outcome,
				"detail", oneLine(detail, repairFailureBytes))
			if sr.rec != nil {
				sr.rec.Notice("maul refused review %d of %d (%s); that check is unclear and not saved", n, len(items),
					oneLine(detail, repairFailureBytes))
			}
			ev.Answers = append(ev.Answers, envelope.ReviewAnswer{
				CheckDigest: item.CheckDigest, OutcomeDigest: item.OutcomeDigest,
				Answer: string(report.AnswerUnclear), Reason: unreviewedReason, Unreviewed: true,
			})
			continue
		}
		ev.Answers = append(ev.Answers, envelope.ReviewAnswer{
			CheckDigest: item.CheckDigest, OutcomeDigest: item.OutcomeDigest,
			Answer: string(c.Answer), Reason: c.Reason,
		})
	}
	ev.Outcome = envelope.OutcomeOK
	return ev
}

// addSession adds one review session's accounting, its repairs included,
// to the stage's. Each session is its own, so its cost adds as its tokens
// do; the stage keeps the first session's id.
func addSession(st *envelope.Stage, s envelope.Stage) {
	if st.SessionID == "" {
		st.SessionID = s.SessionID
	}
	st.ElapsedSeconds += s.ElapsedSeconds
	st.NumTurns += s.NumTurns
	st.Usage.InputTokens += s.Usage.InputTokens
	st.Usage.OutputTokens += s.Usage.OutputTokens
	st.Usage.CacheReadTokens += s.Usage.CacheReadTokens
	st.Usage.CacheCreationTokens += s.Usage.CacheCreationTokens
	st.Usage.CostUSD += s.Usage.CostUSD
}
