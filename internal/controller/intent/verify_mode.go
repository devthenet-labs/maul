// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"

// emptyCriteriaDigest is the digest of the empty criteria list,
// acceptance.Digest(nil): what IntentAcceptance.Digest (a required field)
// holds for an outcomes plan and for a criteria plan that declared none.
// It is a literal, pinned to acceptance.Digest(nil) by a test, so it
// outlives the criteria grammar: once status.acceptance.criteria leaves
// the CRD the API server prunes it, and a criteria-era record is then told
// apart only by a digest other than this one with no outcomes digest
// (recordPrunedCriteria).
const emptyCriteriaDigest = "sha256:4f53cda18c2baa0c0354bb5f9a3ecbe5ed12ab4d8e11ba873c2f11161202b945"

// recordKind is what the approved plan revision's acceptance on
// record holds (pass.record).
type recordKind int

const (
	// recordNone: nothing of the approved revision: no record, or another
	// revision's.
	recordNone recordKind = iota
	// recordEmpty: a plan that declared nothing, of either era: the
	// empty-list digest, and neither criteria nor outcomes nor an outcomes
	// digest.
	recordEmpty
	// recordCriteria: slice-1 criteria.
	recordCriteria
	// recordPrunedCriteria: a criteria-era record the API server pruned of
	// its criteria: a digest of criteria (not emptyCriteriaDigest), and
	// neither criteria nor outcomes nor an outcomes digest. Nothing of it
	// can be run, judged or accepted.
	recordPrunedCriteria
	// recordOutcomes: the agent evaluator's outcomes (an outcomes digest,
	// or outcomes).
	recordOutcomes
	// recordMixed: criteria and outcomes both; never written, never
	// verified.
	recordMixed
)

// recordOf is what acc holds. A Digest of "" (which the CRD's pattern never
// admits, only a test's fixture) reads as the empty list.
func recordOf(acc *v1alpha1.IntentAcceptance) recordKind {
	if acc == nil {
		return recordNone
	}
	outcomes, criteria := acc.OutcomesDigest != "" || len(acc.Outcomes) > 0, len(acc.Criteria) > 0
	switch {
	case outcomes && criteria:
		return recordMixed
	case outcomes:
		return recordOutcomes
	case criteria:
		return recordCriteria
	case acc.Digest != "" && acc.Digest != emptyCriteriaDigest:
		return recordPrunedCriteria
	}
	return recordEmpty
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
// holds (pass.record). Where the evaluator switch and the record disagree
// nothing is verified, judged or reported as passing.
type verifyMode int

const (
	// modeNone: nothing on record for the approved revision as the switch
	// records it: none, another revision's, or a criteria-era record (its
	// criteria, or pruned of them) while the evaluator verifies the
	// Project. syncAcceptance reads the approved plan again: an evaluator
	// refuses a criteria plan, which stops for a replan (stopForReplan,
	// Unavailable).
	modeNone verifyMode = iota
	// modeOff: the approved revision's record matches the switch, but the
	// Intent is not verified (its Project, the controller or its tool host
	// does not verify it): settled Off.
	modeOff
	// modeCriteria: slice 1 verifies the approved plan's criteria.
	modeCriteria
	// modeOutcomes: the agent evaluator verifies the approved plan's
	// outcomes.
	modeOutcomes
	// modeMismatch: a record the evaluator, switched off (or its agents
	// host off), cannot verify: outcomes, or a criteria-era record pruned
	// of its criteria. Nothing runs it: settled Off, its maul/acceptance
	// the switched-off error, until the evaluator is back or a plan is
	// approved again.
	modeMismatch
)

// verifyModeOf is the mode for an Intent verified or not (verifies), under
// the evaluator or not (evaluates), whose approved revision's record holds
// rec.
func verifyModeOf(verifies, evaluates bool, rec recordKind) verifyMode {
	switch {
	case rec == recordNone:
		return modeNone
	case evaluates && (rec == recordCriteria || rec == recordPrunedCriteria || rec == recordMixed):
		// Read again: slice-1 criteria the evaluator does not run.
		return modeNone
	case !evaluates && (rec == recordOutcomes || rec == recordPrunedCriteria || rec == recordMixed):
		return modeMismatch
	case !verifies:
		return modeOff
	case evaluates:
		return modeOutcomes
	}
	return modeCriteria
}

// mode is this pass's verifyMode. It is computed on every call, from the
// Intent as the pass holds it now: a status write replaces it mid-pass, and
// a mode kept from before would be the stale decision CX-1 came from.
func (p *pass) mode() verifyMode {
	return verifyModeOf(p.verifies(), p.set.evaluates(p.proj), p.record())
}
