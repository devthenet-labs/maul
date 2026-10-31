// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import (
	"slices"
	"sort"
)

// The agent evaluator's pass rules (DESIGN 4.3). Decide is the only place a
// round's outcomes are judged: what the tools ledger recorded, what the
// check reviewer answered and what the evaluator claimed go in, a result
// per outcome, at most one fix request and the hand-off come out. It never
// reads a check's body: the caller judges the saved-check grammar
// (internal/checks, which imports this package) when it stores a check and
// records the answer in StoredCheck.Valid.

// OutcomeRef is one approved outcome as Decide sees it, in plan order.
type OutcomeRef struct {
	ID string
	// Digest is the outcome's own digest, which check reviews are keyed
	// by. A review only counts for the exact text it was asked about.
	Digest string
	// Keep marks a `keep` outcome (its check must pass at base too, rule
	// 5); otherwise it is a `change` outcome (its check must fail at base).
	Keep bool
	// Manual marks an outcome closed only by a human (`/maul accept`).
	Manual bool
}

// CheckForm is a saved check's form. Only maul's three forms can support a
// pass (rule 1); anything else, a model-judged screenshot included (7.5),
// cannot.
type CheckForm string

// maul's check forms.
const (
	CheckFormHTTP    CheckForm = "http"
	CheckFormBrowser CheckForm = "browser"
	CheckFormExec    CheckForm = "exec"
)

// StoredCheck is a saved check in the Intent's immutable check store.
type StoredCheck struct {
	Digest string
	// Outcome is the id of the outcome it is bound to.
	Outcome string
	Form    CheckForm
	// Valid records that its body passed the saved-check grammar when it
	// was stored: substantive and scoped to its own run token (rule 2).
	Valid bool
	// Retired marks a check retired in an earlier round (or this one).
	Retired bool
	// ExpectFail marks a check saved to show a defect (S1 bug 7): the
	// evaluator proposed it for a fail verdict, red. Its confirmation counts
	// only red by assertion (CheckRun.AssertionStep), never green, and only a
	// review of the red question (CheckReview.ExpectFail) lets it support a
	// result.
	ExpectFail bool
	// FailedStep is, for a check saved red, the step (1-based) its
	// confirmation failed at by assertion; Cleared that it has since
	// replayed green. Until it has, its later steps never ran, so a replay
	// fail reproduces the defect only red by assertion at or before
	// FailedStep (review finding 2).
	FailedStep int32
	Cleared    bool
}

// RunResult is a check run's result as the tools ledger recorded it.
type RunResult string

// Run results (toolwire's, plus refused: the relay ran nothing).
const (
	RunPass        RunResult = "pass"
	RunFail        RunResult = "fail"
	RunError       RunResult = "error"
	RunQuarantined RunResult = "quarantined"
	RunRefused     RunResult = "refused"
)

// CheckRun is one run of a check, read from the tools ledger of this
// round's PreviewVerification, at its heads.
type CheckRun struct {
	CheckDigest string
	Result      RunResult
	// Token is the run token tools generated; a pass without one confirms
	// nothing (rule 3).
	Token string
	// Seq is the run's ledger entry.
	Seq int32
	// AssertionStep is, for a fail red by assertion (both attempts at the
	// same step of a check that otherwise ran: toolwire's AssertionStep),
	// that step, 1-based; 0 otherwise.
	AssertionStep int32
	// ExpectFail: a confirmation of a check proposed to show a defect.
	ExpectFail bool
}

// ReviewAnswer is the check reviewer's answer.
type ReviewAnswer string

// Review answers.
const (
	ReviewProves       ReviewAnswer = "proves"
	ReviewDoesNotProve ReviewAnswer = "does_not_prove"
	ReviewUnclear      ReviewAnswer = "unclear"
)

// CheckReview is one cached reviewer answer, keyed by (check, outcome).
type CheckReview struct {
	CheckDigest   string
	OutcomeDigest string
	Answer        ReviewAnswer
	// ExpectFail: the reviewer was asked the red question (does this check
	// encode the outcome as approved, would it pass where the outcome
	// holds, and is the assertion that failed the outcome itself).
	ExpectFail bool
}

// Claim is what the evaluator said about an outcome.
type Claim string

// Claims.
const (
	ClaimPass   Claim = "pass"
	ClaimFail   Claim = "fail"
	ClaimUnsure Claim = "unsure"
)

// EvaluatorVerdict is the evaluator's verdict on one outcome, with the
// ledger seqs it cites.
type EvaluatorVerdict struct {
	Outcome string
	Claim   Claim
	Ledger  []int32
}

// CheckDispute is the evaluator's claim that a failing saved check is not
// a defect, with the digest of its proposed replacement, if any.
type CheckDispute struct {
	CheckDigest string
	Reason      string
	Replacement string
	Ledger      []int32
}

// Retirement is a check retired this round.
type Retirement struct {
	CheckDigest string
	Reason      string
}

// AnomalyRef is a look-around anomaly.
type AnomalyRef struct {
	Summary string
	Route   string
	Ledger  []int32
}

// BaseRuns are this round's runs of saved checks at the base revision
// (rule 5). nil when no base is available: always in S1.
type BaseRuns struct {
	Runs []CheckRun
}

// Input is everything Decide judges one round on.
type Input struct {
	Outcomes []OutcomeRef
	Checks   []StoredCheck
	// Replay holds this round's replay runs of stored checks and
	// Confirmations the confirmation runs of proposed ones: both tools'
	// own runs at the current heads.
	Replay        []CheckRun
	Confirmations []CheckRun
	Reviews       []CheckReview
	Verdicts      []EvaluatorVerdict
	Disputes      []CheckDispute
	// Retired is this round's retirements.
	Retired   []Retirement
	Env       []PartFact
	Anomalies []AnomalyRef
	Base      *BaseRuns
}

// Result is an outcome's result this round. A human's acceptance is the
// controller's, layered on top.
type Result string

// Results.
const (
	ResultPassed      Result = "passed"
	ResultFailed      Result = "failed"
	ResultUnsure      Result = "unsure"
	ResultQuarantined Result = "quarantined"
)

// Reasons, one per result path.
const (
	// ReasonConfirmed: a check clears rules 1-6 (passed).
	ReasonConfirmed = "confirmed"
	// ReasonReproduced: a fix-worthy check failed and the evaluator agrees
	// (failed).
	ReasonReproduced = "reproduced"
	// ReasonManual: a human closes this outcome.
	ReasonManual = "manual"
	// ReasonRetired: a check of the outcome was retired this round.
	ReasonRetired = "retired"
	// ReasonDisputed: the evaluator disputes a failing check of it.
	ReasonDisputed = "disputed"
	// ReasonEnvFailed, ReasonInfra: a check failed while the environment
	// was failing (EnvAppFailure, EnvInfra).
	ReasonEnvFailed = "env-failed"
	ReasonInfra     = "infra"
	// ReasonEvaluatorDisagrees: a check failed but there is no agreeing,
	// cited fail on a reviewed, valid check.
	ReasonEvaluatorDisagrees = "evaluator-disagrees"
	// ReasonQuarantined: the only support is a quarantined run.
	ReasonQuarantined = "quarantined"
	// ReasonModelOnlyPass, ReasonModelOnlyFail: the evaluator's word with
	// no confirmed check behind it.
	ReasonModelOnlyPass = "model-only-pass"
	ReasonModelOnlyFail = "model-only-fail"
	// ReasonNoCheck: no check and no verdict.
	ReasonNoCheck = "no-confirmed-check"
	// ReasonNotReproduced: a check saved to show a defect was not red by
	// assertion in maul's own confirmation run (it passed, flaked, errored
	// or failed some other way, or the same check replayed green this
	// round), or, not yet replayed green since, its replay failed other than
	// by assertion at its saved step: neither a fail nor a pass.
	ReasonNotReproduced = "not-reproduced"
	// ReasonChecksDisagree: a check of the outcome failed while another,
	// valid and reviewed proves, passed cleanly: two vetted checks
	// disagree, so neither a fail nor a pass.
	ReasonChecksDisagree = "checks-disagree"
)

// OutcomeResult is one outcome's result, with the check digest it rests on
// (a pass's or a fail's) and the reason.
type OutcomeResult struct {
	ID     string
	Result Result
	Basis  string
	Reason string
}

// FixFailure is one failing check a verify-fix round reproduces.
type FixFailure struct {
	Outcome     string
	CheckDigest string
	// Seq is the failing run's ledger entry.
	Seq int32
}

// FixRequest asks for a verify-fix round.
type FixRequest struct {
	Failures []FixFailure
	// Fingerprint is EvaluationFingerprint of the failures.
	Fingerprint string
}

// Handoff is the hand-off this round earns.
type Handoff string

// Hand-offs.
const (
	// HandoffReady: every outcome passed, no anomaly, a clean environment.
	HandoffReady Handoff = "ready"
	// HandoffUnconfirmed: anything else ("maul couldn't confirm this").
	HandoffUnconfirmed Handoff = "unconfirmed"
)

// Decision is Decide's answer.
type Decision struct {
	// PerOutcome is in Input.Outcomes order.
	PerOutcome []OutcomeResult
	// Fix is non-nil only when a failure is reproduced.
	Fix *FixRequest
	// ReplayRound asks for one model-free replay round at the same heads:
	// a disputed check's replacement is confirmed and reviewed proves.
	ReplayRound bool
	// Retire names the disputed checks that replacement retires, sorted.
	Retire []string
	// Env is the environment's class.
	Env     EnvClass
	Handoff Handoff
}

// runs aggregates one check's runs this round.
type runs struct {
	pass, fail, quarantined, other bool
	// untrusted: a pass with no tools run token.
	untrusted bool
	failSeq   int32
}

func (r runs) clean() bool { return r.pass && !r.fail && !r.quarantined && !r.other && !r.untrusted }

func aggregate(rs []CheckRun) map[string]runs {
	out := map[string]runs{}
	for _, run := range rs {
		a := out[run.CheckDigest]
		switch {
		case run.Result == RunPass && run.Token != "":
			a.pass = true
		case run.Result == RunPass:
			a.untrusted = true
		case run.Result == RunFail:
			if !a.fail || run.Seq < a.failSeq {
				a.failSeq = run.Seq
			}
			a.fail = true
		case run.Result == RunQuarantined:
			a.quarantined = true
		default:
			a.other = true
		}
		out[run.CheckDigest] = a
	}
	return out
}

// decider holds one Input's indexes.
type decider struct {
	in       Input
	env      EnvClass
	runs     map[string]runs
	base     map[string]runs
	retired  map[string]bool
	disputed map[string]CheckDispute
	verdicts map[string]EvaluatorVerdict
	// red: the digests of checks saved to show a defect; notReproduced
	// the digests of checks proposed this round to show a defect whose
	// confirmation was not red by assertion.
	red, notReproduced map[string]bool
	// uncleared: the checks saved red that have not replayed green since.
	uncleared map[string]StoredCheck
}

// confirmationRuns are the confirmations Decide counts: every run of a
// check proposed to pass, and of one proposed to show a defect (its run
// ExpectFail) only a fail red by assertion (guard 1) of a check no replay
// this round saw green (finding 3), whose others are not reproduced. A
// check proposed red that ran green is flaky, never a pass; a check saved
// red earlier and proposed green again after a fix is confirmed as any
// other.
func (d *decider) confirmationRuns() []CheckRun {
	var out []CheckRun
	for _, run := range d.in.Confirmations {
		greenReplay := slices.ContainsFunc(d.in.Replay, func(r CheckRun) bool {
			return r.CheckDigest == run.CheckDigest && r.Result == RunPass
		})
		switch {
		case !run.ExpectFail:
			out = append(out, run)
		case run.Result == RunFail && run.AssertionStep > 0 && !greenReplay:
			out = append(out, run)
		default:
			d.notReproduced[run.CheckDigest] = true
		}
	}
	return out
}

// replayRuns are the replays Decide counts: every replay, but of a check
// saved red that has not yet replayed green, a fail only red by assertion
// at or before the step it was saved failing at (finding 2: its later
// steps never ran); its other fails are not reproduced.
func (d *decider) replayRuns() []CheckRun {
	var out []CheckRun
	for _, run := range d.in.Replay {
		c, red := d.uncleared[run.CheckDigest]
		if red && run.Result == RunFail && (run.AssertionStep == 0 || run.AssertionStep > c.FailedStep) {
			d.notReproduced[run.CheckDigest] = true
			continue
		}
		out = append(out, run)
	}
	return out
}

// Decide judges one evaluator round (DESIGN 4.3). An outcome passes only on
// a check bound to it that is stored and unretired in one of maul's forms
// (1), valid (2), passed every tools run this round with a tools token
// (3), is reviewed proves for this outcome's text (4), discriminates at
// base where a base exists (5), with no other unretired, unquarantined
// check of the outcome failing (6). A fail needs both a deterministic
// failure of a stored, valid, reviewed check and the evaluator's cited
// fail; for a check proposed to show a defect (S1 bug 7) its confirmation
// counts only red by assertion and its review only on the red question,
// and a confirmation of it that is not red by assertion leaves the outcome
// unsure, never passed. A dispute, a retirement this round or a failing
// environment makes the outcome unsure; any anomaly or environment failure
// downgrades the hand-off.
func Decide(in Input) Decision {
	d := decider{
		in: in, env: ClassifyEnvironment(in.Env),
		retired: map[string]bool{}, disputed: map[string]CheckDispute{}, verdicts: map[string]EvaluatorVerdict{},
		red: map[string]bool{}, notReproduced: map[string]bool{}, uncleared: map[string]StoredCheck{},
	}
	for _, c := range in.Checks {
		if c.ExpectFail {
			d.red[c.Digest] = true
			if !c.Cleared {
				d.uncleared[c.Digest] = c
			}
		}
	}
	d.runs = aggregate(append(d.replayRuns(), d.confirmationRuns()...))
	if in.Base != nil {
		d.base = aggregate(in.Base.Runs)
	}
	for _, r := range in.Retired {
		d.retired[r.CheckDigest] = true
	}
	for _, dp := range in.Disputes {
		if d.runs[dp.CheckDigest].fail {
			if _, dup := d.disputed[dp.CheckDigest]; !dup {
				d.disputed[dp.CheckDigest] = dp
			}
		}
	}
	for _, v := range in.Verdicts {
		if _, dup := d.verdicts[v.Outcome]; !dup {
			d.verdicts[v.Outcome] = v
		}
	}
	out := Decision{Env: d.env, Handoff: HandoffUnconfirmed}
	var failures []FixFailure
	for _, o := range in.Outcomes {
		r, fs := d.outcome(o)
		out.PerOutcome = append(out.PerOutcome, r)
		failures = append(failures, fs...)
	}
	if len(failures) > 0 {
		out.Fix = &FixRequest{Failures: failures, Fingerprint: fixFingerprint(failures, in.Env)}
	}
	out.Retire, out.ReplayRound = d.replacements()
	if out.Fix == nil && !out.ReplayRound && len(in.Anomalies) == 0 && d.env == EnvOK && allPassed(out.PerOutcome) {
		out.Handoff = HandoffReady
	}
	return out
}

// outcome judges one outcome, returning its failures for a fix round.
func (d decider) outcome(o OutcomeRef) (OutcomeResult, []FixFailure) {
	res := OutcomeResult{ID: o.ID, Result: ResultUnsure}
	bound := d.bound(o.ID)
	switch {
	case o.Manual:
		res.Reason = ReasonManual
		return res, nil
	case slices.ContainsFunc(d.in.Checks, func(c StoredCheck) bool { return c.Outcome == o.ID && d.retired[c.Digest] }):
		res.Reason = ReasonRetired
		return res, nil
	case slices.ContainsFunc(bound, func(c StoredCheck) bool { _, ok := d.disputed[c.Digest]; return ok }):
		res.Reason = ReasonDisputed
		return res, nil
	}
	if failing := d.failing(bound); len(failing) > 0 {
		return d.failure(o, res, failing)
	}
	if slices.ContainsFunc(bound, func(c StoredCheck) bool { return d.notReproduced[c.Digest] }) {
		// A check proposed red that maul's own run did not see red by
		// assertion: neither a fail nor a pass this round.
		res.Reason = ReasonNotReproduced
		return res, nil
	}
	for _, c := range bound {
		if d.supports(c, o) && d.runs[c.Digest].clean() && d.discriminates(c, o) {
			return OutcomeResult{ID: o.ID, Result: ResultPassed, Basis: c.Digest, Reason: ReasonConfirmed}, nil
		}
	}
	switch {
	case slices.ContainsFunc(bound, func(c StoredCheck) bool { return d.runs[c.Digest].quarantined }):
		res.Result, res.Reason = ResultQuarantined, ReasonQuarantined
	case d.verdicts[o.ID].Claim == ClaimFail:
		res.Reason = ReasonModelOnlyFail
	case d.verdicts[o.ID].Claim == ClaimPass:
		res.Reason = ReasonModelOnlyPass
	default:
		res.Reason = ReasonNoCheck
	}
	return res, nil
}

// failure judges an outcome with failing checks (rule 6 already denies it
// a pass): failed only on a clean environment, the evaluator's cited fail
// and every reproduction a check that could have supported a pass.
func (d decider) failure(o OutcomeRef, res OutcomeResult, failing []StoredCheck) (OutcomeResult, []FixFailure) {
	switch d.env {
	case EnvAppFailure:
		res.Reason = ReasonEnvFailed
		return res, nil
	case EnvInfra:
		res.Reason = ReasonInfra
		return res, nil
	}
	if d.disagree(o, failing) {
		res.Reason = ReasonChecksDisagree
		return res, nil
	}
	v, ok := d.verdicts[o.ID]
	agrees := ok && v.Claim == ClaimFail && len(v.Ledger) > 0
	var fs []FixFailure
	for _, c := range failing {
		if d.supports(c, o) {
			fs = append(fs, FixFailure{Outcome: o.ID, CheckDigest: c.Digest, Seq: d.runs[c.Digest].failSeq})
		}
	}
	if !agrees || len(fs) == 0 {
		res.Reason = ReasonEvaluatorDisagrees
		return res, nil
	}
	return OutcomeResult{ID: o.ID, Result: ResultFailed, Basis: fs[0].CheckDigest, Reason: ReasonReproduced}, fs
}

// disagree reports another check of o, not failing, that could pass it
// this round (valid, reviewed proves, run clean, discriminating): two vetted
// checks disagree on the outcome (finding 3), so it is neither failed nor
// passed.
func (d decider) disagree(o OutcomeRef, failing []StoredCheck) bool {
	for _, c := range d.bound(o.ID) {
		if slices.ContainsFunc(failing, func(f StoredCheck) bool { return f.Digest == c.Digest }) {
			continue
		}
		if d.supports(c, o) && d.runs[c.Digest].clean() && d.discriminates(c, o) {
			return true
		}
	}
	return false
}

// bound is the outcome's stored, unretired checks, by digest.
func (d decider) bound(id string) []StoredCheck {
	var out []StoredCheck
	for _, c := range d.in.Checks {
		dup := slices.ContainsFunc(out, func(o StoredCheck) bool { return o.Digest == c.Digest })
		if c.Outcome == id && !c.Retired && !dup {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Digest < out[j].Digest })
	return out
}

// failing is the bound checks that failed deterministically this round.
func (d decider) failing(bound []StoredCheck) []StoredCheck {
	var out []StoredCheck
	for _, c := range bound {
		if d.runs[c.Digest].fail {
			out = append(out, c)
		}
	}
	return out
}

// supports reports rules 1, 2 and 4 for c and o: one of maul's forms,
// valid, reviewed proves for exactly this outcome's text with no other
// answer cached; a check saved to show a defect only once a review of the
// red question (guard 3) answered proves.
func (d decider) supports(c StoredCheck, o OutcomeRef) bool {
	switch c.Form {
	case CheckFormHTTP, CheckFormBrowser, CheckFormExec:
	default:
		return false
	}
	if !c.Valid || c.Retired || d.retired[c.Digest] || o.Digest == "" {
		return false
	}
	proves, red := false, false
	for _, r := range d.in.Reviews {
		if r.CheckDigest != c.Digest || r.OutcomeDigest != o.Digest {
			continue
		}
		if r.Answer != ReviewProves {
			return false
		}
		proves, red = true, red || r.ExpectFail
	}
	return proves && (red || !c.ExpectFail && !d.red[c.Digest])
}

// discriminates is rule 5. With no base, or no clean verdict at base (the
// check could not run there), it is n/a: true.
func (d decider) discriminates(c StoredCheck, o OutcomeRef) bool {
	if d.base == nil {
		return true
	}
	b, ok := d.base[c.Digest]
	switch {
	case !ok:
		return true
	case b.quarantined || b.untrusted || (b.pass && b.fail):
		return false
	case b.fail:
		return !o.Keep
	case b.pass:
		return o.Keep
	}
	return true // only errors or refusals: n/a
}

// replacements are the disputed checks whose replacement is stored for the
// same outcome, valid, cleanly confirmed and reviewed proves: they are
// retired, and one replay round may then pass the outcome.
func (d decider) replacements() ([]string, bool) {
	var retire []string
	for digest, dp := range d.disputed {
		o, ok := d.outcomeOf(digest)
		if !ok || dp.Replacement == "" || dp.Replacement == digest {
			continue
		}
		for _, c := range d.bound(o.ID) {
			if c.Digest == dp.Replacement && d.supports(c, o) && d.runs[c.Digest].clean() {
				retire = append(retire, digest)
				break
			}
		}
	}
	sort.Strings(retire)
	return retire, len(retire) > 0
}

// outcomeOf is the outcome a stored, unretired check is bound to.
func (d decider) outcomeOf(digest string) (OutcomeRef, bool) {
	for _, o := range d.in.Outcomes {
		if slices.ContainsFunc(d.bound(o.ID), func(c StoredCheck) bool { return c.Digest == digest }) {
			return o, true
		}
	}
	return OutcomeRef{}, false
}

func allPassed(rs []OutcomeResult) bool {
	for _, r := range rs {
		if r.Result != ResultPassed {
			return false
		}
	}
	return len(rs) > 0
}

func fixFingerprint(fs []FixFailure, env []PartFact) string {
	outcomes, checks := make([]string, 0, len(fs)), make([]string, 0, len(fs))
	for _, f := range fs {
		outcomes = append(outcomes, f.Outcome)
		checks = append(checks, f.CheckDigest)
	}
	return EvaluationFingerprint(outcomes, checks, env)
}
