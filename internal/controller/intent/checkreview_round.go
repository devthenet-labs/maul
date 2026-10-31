// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/agentresult"
	"github.com/devthenet-labs/maul/internal/agentrun"
)

// The reviewer round (DESIGN 2.2 step 9, SPEC S1-19): once a full round
// is collected (its evaluator's run complete, its verification observed),
// and before it is judged, the round's new confirmed checks are reviewed
// and the proven ones stored (checkstore.go). One step a pass:
//
//  1. The candidates (storeCandidates: confirmed, valid, not stored) whose
//     review is not cached for their outcome's text go to one checkreview
//     IntentRun of the round and attempt (createRoleRun: the evaluation
//     ceiling, attribution, the credential hold), handed each check's
//     canonical JSON and its outcome's text, keyed by both digests. Past
//     the evaluation ceiling no review runs: those checks stay unstored.
//  2. Once that run settles, its answers for those keys are cached
//     (status.acceptance.reviews, the run named). A failed reviewer caches
//     nothing, and its checks stay unstored this round; it is not retried.
//  3. The candidates cached proves are stored in the next check store.
//  4. The round is judged (judgeEvaluation).
//
// A cache hit launches nothing. The reviewer never sees app content, a
// step's name or the evaluator's notes: it is handed what maul stores.

// maxCachedReviews bounds status.acceptance.reviews (its schema's).
const maxCachedReviews = 80

// reviewChecks is the reviewer round of pv, a collected full round: it
// reports whether it wrote, and whether the round waits for its reviewer.
// Neither: the round is judged.
func (p *pass) reviewChecks(ctx context.Context, pv *v1alpha1.PreviewVerification) (changed, wait bool,
	err error) {
	acc := p.in.Status.Acceptance
	stored, _, err := p.storedChecks(ctx)
	if err != nil {
		// The store cannot be read as recorded: nothing is added to it;
		// the round is judged on what it observed.
		p.r.log().LogAttrs(ctx, slog.LevelWarn, "read the intent's check store; no check is stored this round",
			slog.String("intent", p.in.Name), slog.Any("error", err))
		return false, false, nil
	}
	cands := storeCandidates(pv, acc.Outcomes, stored, p.checkPreview(pv),
		func(reason string, c v1alpha1.SavedCheck, detail string) {
			p.r.log().LogAttrs(ctx, slog.LevelInfo, "a confirmed check is not stored: "+reason,
				slog.String("intent", p.in.Name), slog.String("verification", pv.Name),
				slog.String("check", c.Digest), slog.String("detail", detail))
		})
	if len(cands) == 0 {
		return false, false, nil
	}
	if pending := unreviewed(cands, acc.Reviews); len(pending) > 0 {
		changed, wait, err := p.checkReviewRound(ctx, pv, pending, stored)
		if changed || wait || err != nil {
			return changed, wait, err
		}
	}
	accepted := proven(cands, acc.Reviews)
	if room := v1alpha1.MaxSavedChecks - len(stored); len(accepted) > room {
		p.r.log().LogAttrs(ctx, slog.LevelInfo, "the check store is full; proven checks past it are not stored",
			slog.String("intent", p.in.Name), slog.Int("dropped", len(accepted)-max(room, 0)))
		accepted = accepted[:max(room, 0)]
	}
	if len(accepted) == 0 {
		return false, false, nil
	}
	return true, false, p.writeCheckStore(ctx, append(slices.Clone(stored), accepted...))
}

// unreviewed are the candidates with no cached review for their outcome's
// text.
func unreviewed(cands []storeCandidate, reviews []v1alpha1.CheckReviewRecord) []storeCandidate {
	answers := reviewsByKey(reviews)
	var out []storeCandidate
	for _, c := range cands {
		if _, ok := answers[c.reviewKey()]; !ok {
			out = append(out, c)
		}
	}
	return out
}

// checkReviewRound drives pv's checkreview run for pending: created when there
// is none (nothing past the evaluation ceiling), waited on while it runs,
// and its answers cached once it settles.
func (p *pass) checkReviewRound(ctx context.Context, pv *v1alpha1.PreviewVerification, pending []storeCandidate,
	stored []v1alpha1.SavedCheck) (changed, wait bool, err error) {
	run := p.checkReviewRun(pv)
	switch {
	case run == nil:
		created, err := p.launchCheckReview(ctx, pv, pending)
		if errors.Is(err, errEvaluationCeiling) {
			p.r.log().LogAttrs(ctx, slog.LevelInfo,
				"the intent's evaluations are at their ceiling; its new checks are not reviewed or stored",
				slog.String("intent", p.in.Name), slog.String("verification", pv.Name))
			return false, false, nil
		}
		return created, false, err
	case !runSettled(run):
		return false, true, nil
	}
	return p.recordReviews(ctx, run, pending, stored)
}

// checkReviewRun is pv's checkreview run (its round and attempt), nil when
// none was created.
func (p *pass) checkReviewRun(pv *v1alpha1.PreviewVerification) *v1alpha1.IntentRun {
	name := v1alpha1.IntentRunName(p.in.Name, v1alpha1.IntentStageCheckReview, pv.Spec.Round, "", pv.Spec.Attempt)
	for _, run := range p.runs {
		if run.Spec.Stage == v1alpha1.IntentStageCheckReview && run.Name == name {
			return run
		}
	}
	return nil
}

// launchCheckReview creates pv's checkreview run reviewing pending,
// through the normal path: the credential hold, then createRoleRun (the
// evaluation ceiling, errEvaluationCeiling past it). It reports whether it
// wrote.
func (p *pass) launchCheckReview(ctx context.Context, pv *v1alpha1.PreviewVerification,
	pending []storeCandidate) (bool, error) {
	if p.proj.Spec.Suspend {
		return false, nil
	}
	attributed := p.attributeRun(v1alpha1.IntentStageCheckReview)
	if held, err := p.holdForCredential(ctx, p.set.Attribution.Stamp(attributed), false); held || err != nil {
		return held, err
	}
	items := make([]agentrun.CheckReviewItem, 0, len(pending))
	for _, c := range pending {
		it := agentrun.CheckReviewItem{CheckDigest: c.check.Digest, OutcomeDigest: outcomeDigest(c.outcome),
			Outcome: reviewOutcomeText(c.outcome), Check: c.check.Body}
		if c.red() {
			// The red question, naming the assertion maul's own run failed.
			it.Expect, it.FailedStep = string(v1alpha1.CheckExpectFail), int(c.check.FailedStep)
			it.FailedAssertion = c.check.FailedAssertion
		}
		items = append(items, it)
	}
	handoff, err := encodeCheckReviewHandoff(items)
	if err != nil {
		return false, err
	}
	run, err := p.createRoleRun(ctx, v1alpha1.IntentStageCheckReview, pv.Spec.Round, pv.Spec.Attempt, nil,
		map[string]string{keyIssue: handoff})
	if err != nil {
		return false, err
	}
	p.r.log().LogAttrs(ctx, slog.LevelInfo, "reviewer round: the check reviewer is launched",
		slog.String("intent", p.in.Name), slog.String("verification", pv.Name), slog.String("run", run.Name),
		slog.Int("checks", len(items)))
	return true, nil
}

// recordReviews caches run's answers for pending's keys, each naming run
// and the question it answered (red or not). A run that failed, or answered
// none of them, caches nothing, and its checks stay unstored this round; so
// does an answer agent-runner wrote itself for a review with no accepted
// report (unreviewed: unclear, its check alone unstored, and reviewed again
// should a later round confirm it). It reports whether it wrote.
func (p *pass) recordReviews(ctx context.Context, run *v1alpha1.IntentRun, pending []storeCandidate,
	stored []v1alpha1.SavedCheck) (changed, wait bool, err error) {
	answers := reviewAnswers(run)
	want := make(map[reviewKey]storeCandidate, len(pending))
	for _, c := range pending {
		want[reviewKey{check: c.check.Digest, outcome: outcomeDigest(c.outcome)}] = c
	}
	var add []v1alpha1.CheckReviewRecord
	for _, a := range answers {
		k := reviewKey{check: a.CheckDigest, outcome: a.OutcomeDigest}
		c, ok := want[k]
		if !ok {
			continue
		}
		delete(want, k)
		if a.Unreviewed {
			p.r.log().LogAttrs(ctx, slog.LevelInfo, "a check's review ended with no report; it is unclear and not stored",
				slog.String("intent", p.in.Name), slog.String("run", run.Name), slog.String("check", a.CheckDigest))
			continue
		}
		rec := v1alpha1.CheckReviewRecord{CheckDigest: a.CheckDigest, OutcomeDigest: a.OutcomeDigest,
			Answer: v1alpha1.ReviewAnswer(a.Answer), Run: run.Name}
		if c.red() {
			rec.Expect, rec.FailedStep, rec.FailedAssertion = v1alpha1.CheckExpectFail, c.check.FailedStep,
				c.check.FailedAssertion
		}
		add = append(add, rec)
	}
	if len(add) == 0 {
		p.r.log().LogAttrs(ctx, slog.LevelInfo, "the check reviewer answered none of the round's checks",
			slog.String("intent", p.in.Name), slog.String("run", run.Name),
			slog.String("phase", string(run.Status.Phase)), slog.String("outcome", run.Status.Outcome))
		return false, false, nil
	}
	return true, false, p.update(ctx, func(cur *v1alpha1.Intent) error {
		acc := cur.Status.Acceptance
		acc.Reviews = trimReviews(append(acc.Reviews, add...), stored)
		return nil
	})
}

// reviewAnswers are a complete checkreview run's answers as
// collectCheckReview kept them, those of a known answer; none for a run
// that did not complete or whose report does not decode.
func reviewAnswers(run *v1alpha1.IntentRun) []agentresult.ReviewAnswer {
	if run.Status.Phase != v1alpha1.RunComplete || run.Status.Report == "" {
		return nil
	}
	var answers []agentresult.ReviewAnswer
	if err := json.Unmarshal([]byte(run.Status.Report), &answers); err != nil {
		return nil
	}
	return slices.DeleteFunc(answers, func(a agentresult.ReviewAnswer) bool {
		switch v1alpha1.ReviewAnswer(a.Answer) {
		case v1alpha1.ReviewProves, v1alpha1.ReviewDoesNotProve, v1alpha1.ReviewUnclear:
			return false
		}
		return true
	})
}

// trimReviews holds rs to maxCachedReviews: the oldest reviews of checks
// not in stored go first, then the oldest of any, so a stored check keeps
// its review as long as it can.
func trimReviews(rs []v1alpha1.CheckReviewRecord, stored []v1alpha1.SavedCheck) []v1alpha1.CheckReviewRecord {
	for len(rs) > maxCachedReviews {
		i := slices.IndexFunc(rs, func(r v1alpha1.CheckReviewRecord) bool {
			return !slices.ContainsFunc(stored, func(c v1alpha1.SavedCheck) bool { return c.Digest == r.CheckDigest })
		})
		if i < 0 {
			i = 0
		}
		rs = slices.Delete(rs, i, i+1)
	}
	return rs
}

// checkReviewCurrent re-reads, uncached, what a checkreview run is launched
// for: its round and attempt are still the Intent's verification in flight,
// evaluating. A refusal says why and launches nothing: a stale round's
// checks are never reviewed.
func (r *RunReconciler) checkReviewCurrent(ctx context.Context, run *v1alpha1.IntentRun) (*result, error) {
	var in v1alpha1.Intent
	if err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: run.Namespace, Name: run.Spec.IntentRef.Name},
		&in); err != nil {
		return nil, err
	}
	v := in.Status.Verification
	want := v1alpha1.PreviewVerificationName(in.Name, run.Spec.Round, run.Spec.Attempt)
	if in.UID != run.Spec.IntentRef.UID || v == nil || v.Ref == nil || v.Ref.Name != want ||
		v.State != v1alpha1.VerificationEvaluating {
		return &result{outcome: OutcomeAborted, detail: fmt.Sprintf("the evaluation round is no longer current: "+
			"the intent no longer evaluates %s; nothing was launched", want)}, nil
	}
	return nil, nil
}
