// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"

// emptyCriteriaDigest is the digest of the empty criteria list,
// acceptance.Digest(nil): what IntentAcceptance.Digest (a required field)
// holds for an outcomes plan and for a criteria plan that declared none.
// It is v1alpha1.EmptyListDigest, a literal pinned to acceptance.Digest(nil)
// by a test: status.acceptance.criteria has left the CRD, so the API server
// prunes it, and a criteria-era record is told apart only by a digest other
// than this one with no outcomes digest (recordCriteriaEra).
const emptyCriteriaDigest = v1alpha1.EmptyListDigest

// recordKind is what the approved plan revision's acceptance on
// record holds (pass.record).
type recordKind int

const (
	// recordNone: nothing of the approved revision: no record, or another
	// revision's.
	recordNone recordKind = iota
	// recordCriteriaEra: a plan stating slice-1 criteria, which nothing runs
	// any more: a digest of criteria (not emptyCriteriaDigest), and neither
	// outcomes nor an outcomes digest. slice 1 recorded the criteria beside
	// it, which the API server now prunes; a plan stating them where the
	// evaluator does not verify is recorded by the digest alone. Nothing of
	// it can be run, judged or accepted.
	recordCriteriaEra
	// recordOutcomes: the agent evaluator's outcomes (an outcomes digest,
	// or outcomes), or a plan that declared nothing (the empty-list digest
	// and nothing else), which is the evaluator's with nothing to evaluate.
	recordOutcomes
)

// recordOf is what acc holds. A Digest of "" (which the CRD's pattern never
// admits, only a test's fixture) reads as the empty list.
func recordOf(acc *v1alpha1.IntentAcceptance) recordKind {
	if acc == nil {
		return recordNone
	}
	outcomes := acc.OutcomesDigest != "" || len(acc.Outcomes) > 0
	if !outcomes && acc.Digest != "" && acc.Digest != emptyCriteriaDigest {
		return recordCriteriaEra
	}
	return recordOutcomes
}

// record is what the approved plan revision's acceptance on record holds:
// recordNone without an approval, or with another revision's on record.
func (p *pass) record() recordKind {
	acc, ap := p.in.Status.Acceptance, p.in.Status.Approval
	if acc == nil || ap == nil || acc.PlanRevision != ap.PlanRevision {
		return recordNone
	}
	return recordOf(acc)
}

// verifyMode is how this pass verifies the Intent: the one decision every
// verify site branches on (S4, review CX-1), from whether the Intent is
// verified (pass.verifies), whether the agent evaluator verifies its
// Project (Settings.evaluates) and what the approved revision's record
// holds (pass.record). Only the evaluator verifies (S4): where it does not
// verify the Project, nothing is verified, judged or reported as passing.
type verifyMode int

const (
	// modeNone: nothing on record for the approved revision as the
	// evaluator records it: none, another revision's, or a criteria-era
	// record (its criteria, or pruned of them) while the evaluator
	// verifies the Project. syncAcceptance reads the approved plan again:
	// the evaluator refuses a criteria plan, which stops for a replan
	// (stopForReplan, Unavailable).
	modeNone verifyMode = iota
	// modeOff: the evaluator verifies the Project, but the Intent's tool
	// host cannot verify it (pass.verifies): settled Off.
	modeOff
	// modeOutcomes: the agent evaluator verifies the approved plan's
	// outcomes.
	modeOutcomes
	// modeMismatch: a record of the approved revision while the evaluator
	// does not verify the Project (switched off, or the Project not
	// verified): its outcomes, a plan that declared nothing, or a
	// criteria-era record. Nothing runs it: settled Off, its
	// maul/acceptance the switched-off error, and nothing is accepted,
	// until the evaluator verifies it or a plan is approved again.
	modeMismatch
)

// verifyModeOf is the mode for an Intent verified or not (verifies), under
// the evaluator or not (evaluates), whose approved revision's record holds
// rec.
func verifyModeOf(verifies, evaluates bool, rec recordKind) verifyMode {
	switch {
	case rec == recordNone:
		return modeNone
	case !evaluates:
		return modeMismatch
	case rec != recordOutcomes:
		// Read again: slice-1 criteria the evaluator does not run.
		return modeNone
	case !verifies:
		return modeOff
	}
	return modeOutcomes
}

// mode is this pass's verifyMode. It is computed on every call, from the
// Intent as the pass holds it now: a status write replaces it mid-pass, and
// a mode kept from before would be the stale decision CX-1 came from.
func (p *pass) mode() verifyMode {
	return verifyModeOf(p.verifies(), p.set.evaluates(p.proj), p.record())
}
