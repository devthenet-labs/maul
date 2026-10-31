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
	"github.com/devthenet-labs/maul/internal/provider"
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
type CheckReviewItem struct {
	CheckDigest   string `json:"check_digest"`
	OutcomeDigest string `json:"outcome_digest"`
	Outcome       string `json:"outcome"`
	Check         string `json:"check"`
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

// checkReview runs the checkreview stage: one fresh session per review, in
// order, each under the evaluate posture in the scratch directory, each
// writing its own report. Every answer is keyed by the digests the handoff
// carried, never by anything the session wrote. The stage's turns bound
// each session; its output tokens and wall clock are the whole run's, each
// session given what the earlier ones left. The first session that does not
// end OK ends the stage on its outcome, with no answers.
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
				MaxTurns: maxTurns, TokenBudget: budget, Timeout: a.cfg.InvestigateTimeout,
			},
			Outcome: item.Outcome,
			Check:   item.check,
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
			MaxTurns:  maxTurns,
			Sandbox:   harness.SandboxEvaluate,
			SessionID: a.newSessionID(),
			Env:       env,
			Gateway:   gateway,
			// A review writes one short report: a small output bound keeps
			// the broker's per-request reservation inside the stage's
			// small cost budget.
			MaxOutputTokens: provider.CheckReviewMaxOutputTokens,
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

		st := envelope.Stage{Harness: ev.Harness, Model: ev.Model}
		res, idle, runErr := a.start(ctx, sr)
		a.fillStage(&st, h, res)
		if outcome, detail := a.runOutcome(h, res, idle, runErr); outcome != envelope.OutcomeOK {
			addSession(&ev.Stage, st)
			return fail(outcome, session(detail))
		}
		c, _, outcome, detail := settleReport(ctx, a, sr, &st, path, readReport, report.ParseCheckReview)
		addSession(&ev.Stage, st)
		if outcome != envelope.OutcomeOK {
			return fail(outcome, session(detail))
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
