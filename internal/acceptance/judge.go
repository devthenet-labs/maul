// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import (
	"slices"

	"github.com/devthenet-labs/maul/internal/httpstep"
)

// The observed evaluator's judging rule ("evaluator observed"). Judge
// takes the evaluator's per-outcome verdicts at their word when, and only
// when, each cites an entry of the round's verified tools ledger that
// exercised the application: maul checks the citation (ValidCite), never
// whether the cited call shows what the claim says, which is the
// evaluator's reading and the human reviewer's to audit.

// Ledger facts as Judge reads them, the spellings of v1alpha1's LedgerKind
// and LedgerVerb and of toolwire's recorded Side (this package imports
// neither; a test pins them).
const (
	// LedgerKindAgent is the evaluator's own call through maul-tool. The
	// relay's and the runner's entries (readiness, build, session, finish)
	// are never evidence.
	LedgerKindAgent = "agent"
	// LedgerVerbHTTP, LedgerVerbBrowser, LedgerVerbExec and LedgerVerbRun
	// are the calls that exercise the application: the only verbs a claim
	// can rest on. files, logs and events are context; check-run,
	// check-results and confirm are retired spellings, never evidence.
	LedgerVerbHTTP    = "http"
	LedgerVerbBrowser = "browser"
	LedgerVerbExec    = "exec"
	LedgerVerbRun     = "run"
	// LedgerSideBase is an agents round's base side: exploration only,
	// never evidence about the head.
	LedgerSideBase = "base"
)

// MaxCites bounds the valid citations an outcome's decision keeps (the
// schema's status.verification.outcomes[].cites).
const MaxCites = 8

// Reasons Judge gives, beside ReasonManual, ReasonEnvFailed, ReasonInfra,
// ReasonHeldFinding and ReasonBuildFailed, which it shares (result.go).
const (
	// ReasonNotReported: the evaluator gave no verdict on the outcome.
	ReasonNotReported = "not-reported"
	// ReasonEvaluatorUnsure: the evaluator said it could not tell.
	ReasonEvaluatorUnsure = "evaluator-unsure"
	// ReasonNoValidEvidence: a pass or fail citing no entry ValidCite
	// admits.
	ReasonNoValidEvidence = "no-valid-evidence"
	// ReasonSeenNotToHold: a fail citing a valid entry (failed).
	ReasonSeenNotToHold = "seen-not-to-hold"
	// ReasonSeenToHold: a pass citing a valid entry (passed).
	ReasonSeenToHold = "seen-to-hold"
	// ReasonContraryNote: a pass citing a valid entry, on an outcome an
	// anomaly of the same report names: the evaluator's own note says it
	// may not hold.
	ReasonContraryNote = "contrary-note"
)

// Verdict is the evaluator's verdict on one outcome: its claim and the
// ledger seqs it cites. Note is its one-line reading, carried for the
// controller; Judge never reads it.
type Verdict struct {
	Outcome string
	Claim   Claim
	Cites   []int32
	Note    string
}

// LedgerFact is one entry of the round's verified ledger index (the PV's
// status.ledger.entries), without its output.
type LedgerFact struct {
	Seq     int32
	Kind    string
	Verb    string
	Side    string
	Exit    int32
	Refused bool
	// NoAnswer is the index's noAnswer: the call ran but got nothing back
	// from what it exercised (an http request with no response, a run
	// whose program did not compile), so it showed nothing.
	NoAnswer bool
}

// JudgeInput is everything Judge judges one round on.
type JudgeInput struct {
	// Outcomes are the approved outcomes, in plan order.
	Outcomes []OutcomeRef
	// Verdicts are the evaluator's, in report order; the first verdict on
	// an outcome wins.
	Verdicts []Verdict
	// Verified reports that the round's ledger was held to its summary
	// (toolledger.Parse); Count is the summary's entry count and Ledger the
	// index, entry seq-1 at position seq-1.
	Verified bool
	Count    int32
	Ledger   []LedgerFact
	Env      []httpstep.PartFact
	// Held are the outcome ids an earlier evidenced fail at these heads
	// still holds (the controller's heldOutcomes).
	Held []string
	// Contrary are the outcome ids an anomaly of the same report names
	// (report.Anomaly.Outcome): the evaluator's own note that the outcome
	// does not hold, or may not, whatever its verdict says.
	Contrary []string
}

// OutcomeDecision is one outcome's result, its reason and the valid
// citations of its verdict.
type OutcomeDecision struct {
	ID     string
	Result Result
	Reason string
	// Cites are the verdict's citations ValidCite admits, ascending,
	// without duplicates, at most MaxCites; set whatever the result.
	Cites []int32
}

// Judgement is Judge's answer.
type Judgement struct {
	// PerOutcome is in JudgeInput.Outcomes order.
	PerOutcome []OutcomeDecision
	// Fix is non-nil iff some outcome failed: its Failures are the failed
	// outcomes in plan order, each with its lowest valid citation as Seq,
	// and its Fingerprint is OutcomeFingerprint of them.
	Fix     *FixRequest
	Env     httpstep.EnvClass
	Handoff Handoff
}

// ValidCite reports that seq is evidence a claim can rest on: the round's
// ledger is verified (its summary held and its index as long as its
// count), seq is an entry of its index numbered seq, and that entry is the
// evaluator's own unrefused call exercising the application at the head
// that got its answer (no http request without a response, no run whose
// program did not compile; a browser call only when its command exited 0).
// Every kind and verb not named is refused, so a retired or unknown one
// never counts.
func ValidCite(in JudgeInput, seq int32) bool {
	if !verified(in) || seq < 1 || int64(seq) > int64(len(in.Ledger)) {
		return false
	}
	f := in.Ledger[seq-1]
	if f.Seq != seq || f.Kind != LedgerKindAgent || f.Refused || f.Side == LedgerSideBase || f.NoAnswer {
		return false
	}
	switch f.Verb {
	case LedgerVerbHTTP, LedgerVerbExec, LedgerVerbRun:
		return true
	case LedgerVerbBrowser:
		return f.Exit == 0
	}
	return false
}

// verified reports a ledger held to its summary whose index is whole.
func verified(in JudgeInput) bool { return in.Verified && int64(len(in.Ledger)) == int64(in.Count) }

// Judge judges a round's outcomes from the evaluator's verdicts and the
// verified ledger. Per outcome, the first rule that matches decides:
//
//	R0 the ledger is not verified: unsure, infra.
//	R1 the head did not build: unsure, tools-build-failed (a manual
//	   outcome keeps manual).
//	R2 a manual outcome: unsure, manual (/maul accept is the
//	   controller's).
//	R3 the environment is not clean: unsure, env-failed or infra.
//	R4 no verdict: unsure, not-reported (the first verdict wins).
//	R5 the claim is not pass or fail: unsure, evaluator-unsure.
//	R6 a pass or fail with no valid cite: unsure, no-valid-evidence.
//	R7 a fail with a valid cite: failed, seen-not-to-hold, whatever holds
//	   it.
//	R8 a pass with a valid cite on a held outcome: unsure, held-finding.
//	R9 a pass with a valid cite on an outcome an anomaly of the same report
//	   names (Contrary): unsure, contrary-note.
//	R10 a pass with a valid cite: passed, seen-to-hold.
//
// The fix asks for every failed outcome; the hand-off is ready only when
// the environment is clean and every outcome passed. Judge never reads a
// verdict's note, nor one outcome's verdict for another; of the anomalies
// it reads only which outcomes they name, and only to keep those from
// passing.
func Judge(in JudgeInput) Judgement {
	out := Judgement{Env: httpstep.ClassifyEnvironment(in.Env), Handoff: HandoffUnconfirmed,
		PerOutcome: make([]OutcomeDecision, 0, len(in.Outcomes))}
	first := map[string]Verdict{}
	for _, v := range in.Verdicts {
		if _, dup := first[v.Outcome]; !dup {
			first[v.Outcome] = v
		}
	}
	var failures []FixFailure
	var failed []OutcomeRef
	for _, o := range in.Outcomes {
		v, reported := first[o.ID]
		d := judgeOutcome(in, out.Env, o, v, reported)
		if d.Result == ResultFailed {
			failures = append(failures, FixFailure{Outcome: o.ID, Seq: d.Cites[0]})
			failed = append(failed, o)
		}
		out.PerOutcome = append(out.PerOutcome, d)
	}
	if len(failures) > 0 {
		out.Fix = &FixRequest{Failures: failures, Fingerprint: OutcomeFingerprint(failed)}
	}
	if out.Fix == nil && out.Env.Clean() && allDecided(out.PerOutcome, ResultPassed) {
		out.Handoff = HandoffReady
	}
	return out
}

// judgeOutcome is R0-R10 for one outcome and its first verdict, if any.
func judgeOutcome(in JudgeInput, env httpstep.EnvClass, o OutcomeRef, v Verdict, reported bool) OutcomeDecision {
	d := OutcomeDecision{ID: o.ID, Result: ResultUnsure}
	if reported {
		d.Cites = validCites(in, v.Cites)
	}
	switch {
	case !verified(in):
		d.Reason = ReasonInfra
	case httpstep.HeadBuildFailed(in.Env) && !o.Manual:
		d.Reason = ReasonBuildFailed
	case o.Manual:
		d.Reason = ReasonManual
	case env == httpstep.EnvAppFailure:
		d.Reason = ReasonEnvFailed
	case !env.Clean():
		d.Reason = ReasonInfra
	case !reported:
		d.Reason = ReasonNotReported
	case v.Claim != ClaimPass && v.Claim != ClaimFail:
		d.Reason = ReasonEvaluatorUnsure
	case len(d.Cites) == 0:
		d.Reason = ReasonNoValidEvidence
	case v.Claim == ClaimFail:
		d.Result, d.Reason = ResultFailed, ReasonSeenNotToHold
	case slices.Contains(in.Held, o.ID):
		d.Reason = ReasonHeldFinding
	case slices.Contains(in.Contrary, o.ID):
		d.Reason = ReasonContraryNote
	default:
		d.Result, d.Reason = ResultPassed, ReasonSeenToHold
	}
	return d
}

// validCites are the cites ValidCite admits, ascending, once each, at most
// MaxCites: filtered before the cap, so an invalid cite never displaces a
// valid one.
func validCites(in JudgeInput, cites []int32) []int32 {
	var out []int32
	for _, c := range cites {
		if ValidCite(in, c) {
			out = append(out, c)
		}
	}
	slices.Sort(out)
	out = slices.Compact(out)
	if len(out) > MaxCites {
		out = out[:MaxCites]
	}
	return out
}

// allDecided reports a non-empty list whose every outcome has result r.
func allDecided(ds []OutcomeDecision, r Result) bool {
	for _, d := range ds {
		if d.Result != r {
			return false
		}
	}
	return len(ds) > 0
}
