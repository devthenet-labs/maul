// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"

	"k8s.io/apimachinery/pkg/api/meta"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// The judge marker ("evaluator observed"). An earlier release judged
// evaluations by saved checks, replays and a check reviewer; 0.14.18 judges
// the evaluator's cited, verified calls (acceptance.Judge), marks every
// verification it writes (status.verification.judge, v1alpha1.JudgeObserved),
// and withdrew once, per open intent, a verification an earlier release had
// judged (the supersede: its judgement cleared, the VerificationSuperseded
// condition set, one fresh round due). This release removes the check-era
// fields and the supersede with them: intent-controller does not start while
// an open intent still holds an unmarked judgement or a withdrawal whose
// fresh round has not started (the check-era census, census_checks.go). So
// an unmarked verification this release meets has nothing judged, and its
// marker alone is written (markVerification); every verification it starts
// is marked from the start (newVerification, createVerification). An
// intent 0.14.18 withdrew, whose fresh round it started, keeps the condition
// until a round judged under these rules records its own result
// (applyJudgement) or a new plan revision starts the record afresh
// (syncAcceptance): the comments say the earlier result was withdrawn
// (templates.WithdrawnSentence) meanwhile. A terminal intent's unmarked
// judgement is never judged again; its comments keep that release's words
// (handoffView, Earlier).

// reasonRulesChanged is the VerificationSuperseded condition's reason, as
// 0.14.18 wrote it.
const reasonRulesChanged = "VerificationRulesChanged"

// newVerification is a verification record as this release starts one:
// judged under its rules from the start.
func newVerification() *v1alpha1.IntentVerification {
	return &v1alpha1.IntentVerification{Judge: v1alpha1.JudgeObserved}
}

// markVerification writes the judge marker on a verification an earlier
// release left unmarked, which the check-era census holds to nothing
// judged and no withdrawal pending (heldByEarlierRules): nothing of it is
// withdrawn, and its next round is whatever is due, counted as always. It
// reports whether it wrote the Intent's status.
func (p *pass) markVerification(ctx context.Context) (bool, error) {
	if p.in.Status.Verification.Judge == v1alpha1.JudgeObserved {
		return false, nil
	}
	return true, p.update(ctx, func(cur *v1alpha1.Intent) error {
		cur.Status.Verification.Judge = v1alpha1.JudgeObserved
		return nil
	})
}

// clearSuperseded drops cur's withdrawal: a round judged under these rules
// recorded its own result, or a new plan revision starts afresh.
func clearSuperseded(cur *v1alpha1.Intent) {
	meta.RemoveStatusCondition(&cur.Status.Conditions, v1alpha1.ConditionVerificationSuperseded)
}
