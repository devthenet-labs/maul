// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import (
	"slices"
	"sort"

	"github.com/devthenet-labs/maul/internal/httpstep"
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

// CheckForm is a saved check's form. Only maul's forms can support a pass
// (rule 1); anything else, a model-judged screenshot included (7.5),
// cannot.
type CheckForm string

// maul's check forms.
const (
	CheckFormHTTP    CheckForm = "http"
	CheckFormBrowser CheckForm = "browser"
	CheckFormExec    CheckForm = "exec"
	// CheckFormRun is a program maul compiles against the tree and runs
	// (slice S2, the agents placement). Like exec, it is a program check:
	// it fails its outcome only red by assertion (O4).
	CheckFormRun CheckForm = "run"
)

// programForm reports a form whose check is a program: exec and run. A
// program that crashed, did not compile, timed out or exited wrong with no
// assertion marker shows what a broken check shows too.
func programForm(f CheckForm) bool { return f == CheckFormExec || f == CheckFormRun }

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
	// FailedAssertion is which assertion of FailedStep failed (second
	// review, 2B): an http step holds several, an exec check one step of
	// many. Until Cleared, only a run red by this same assertion counts.
	FailedAssertion string
	Cleared         bool
	// NoRunData marks a saved check, green or red, that writes no data of
	// its own run (!checks.WritesRunData; a body that does not parse
	// included): it reads whatever the page or API shows, so rows other
	// runs made can move it. Its failure never starts an automatic fix,
	// cleared or not: it is a suspected defect, held for a person (#309
	// review).
	NoRunData bool
	// Narrowed marks a check that passed cleanly in an earlier round whose
	// evaluator saw its outcome, as worded, fail (Decision.Narrowed): it
	// asserts less than the outcome, so it never supports a pass of it
	// again, whatever this round's evaluator says (bug 13 review). It only
	// ever withholds a pass.
	Narrowed bool
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
	// AssertionID is which assertion of AssertionStep failed.
	AssertionID string
	// ExpectFail: a confirmation of a check proposed to show a defect.
	ExpectFail bool
	// Cause is, for an error run, why it errored when the relay recorded
	// one (RunCause); read only at base.
	Cause httpstep.RunCause
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
	// holds, and is the assertion that failed the outcome itself), about
	// the failing assertion FailedStep and FailedAssertion: it counts only
	// for a check saved failing that same assertion (second review, 2A).
	ExpectFail      bool
	FailedStep      int32
	FailedAssertion string
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
	// Contrary are the ledger seqs where the evaluator saw the outcome, as
	// worded, not hold, whatever it believes caused that (S1 demo bug 13):
	// a fact it records, apart from its verdict, which maul judges.
	Contrary []int32
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
// (rule 5): the merge base of the head and the PR's base branch. nil when no
// base is available: always in S1, the slot placement.
type BaseRuns struct {
	Runs []CheckRun
	// Required marks a round whose every check was to run at base too (the
	// agents placement): a check with no base run then does not
	// discriminate, so it never passes its outcome.
	Required bool
	// Unbuildable marks a base the tool pod could not build: no check could
	// run there, so a missing base run is n/a ("no base comparison") and a
	// pass rests on rules 1-4 and 6. A base run recorded anyway is still
	// judged: it can only withhold a pass.
	Unbuildable bool
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
	// ReplayRequired are the digests of the stored checks this round was to
	// replay (its copy of the store; review VC-3, mirroring
	// BaseRuns.Required): a stored, unretired check of an outcome among
	// them with no replay run this round was never run at these heads, so
	// its outcome cannot pass (ReasonNotReplayed). nil asks for no replay.
	ReplayRequired []string
	Reviews        []CheckReview
	Verdicts       []EvaluatorVerdict
	Disputes       []CheckDispute
	// Retired is this round's retirements.
	Retired   []Retirement
	Env       []httpstep.PartFact
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
	// ReasonSuspectedDefect: the outcome would be failed only on checks
	// saved red (a defect present from round 1): unsure, never an
	// automatic fix; a person confirms it with /maul verify fix.
	ReasonSuspectedDefect = "suspected-defect"
	// ReasonHeldFinding: an earlier round at the same heads found the
	// outcome failing, or suspected a defect in it, on a check still saved
	// (G-B-LIB-1): a finding is a fact about the heads, so a later round
	// there leaves the outcome unsure, never passed, until the heads move,
	// an approver accepts it or the check is retired through a dispute with
	// a proven replacement. Never Decide's: the intent controller, which
	// keeps the rounds' records, applies it.
	ReasonHeldFinding = "held-finding"
	// ReasonFlaky: a check of the outcome both passed and failed at the
	// same heads this round.
	ReasonFlaky = "flaky"
	// ReasonChecksDisagree: a check of the outcome failed while another,
	// valid and reviewed proves, passed cleanly: two vetted checks
	// disagree, so neither a fail nor a pass.
	ReasonChecksDisagree = "checks-disagree"
	// ReasonEvaluatorSawFailure: a check would pass the outcome, but the
	// evaluator says it saw the outcome fail this round (its verdict is fail,
	// or it names contrary entries), or every such check passed in an
	// earlier round where the evaluator saw the outcome fail (Narrowed):
	// neither a pass nor a fail (S1 demo bug 13, a narrowed check passed
	// where the outcome as worded failed).
	ReasonEvaluatorSawFailure = "evaluator-saw-failure"
	// ReasonNotAsserted: the outcome would be failed, but a program check
	// (exec or run form) failed other than by assertion (a crash, a compile
	// error, a timeout, a wrong exit with no marker): what a broken check
	// shows too, so neither a fail nor a pass (owner decision O4).
	ReasonNotAsserted = "not-asserted"
	// ReasonNoBaseEvidence: a check passes the outcome by every other rule,
	// but its runs at the base do not tell the change from its absence
	// (rule 5, slice S2): a change outcome's check did not fail there by an
	// assertion, nor could it not compile or be built there; a keep
	// outcome's failed there by one; or a required base has no run of it.
	ReasonNoBaseEvidence = "no-base-evidence"
	// ReasonBuildFailed: the tool pod could not build the head (slice S2):
	// maul's Infra, so the round has no fail and no pass whatever its runs
	// show (the PreviewVerification reason of the same name).
	ReasonBuildFailed = "tools-build-failed"
	// ReasonNotReplayed: a stored check of the outcome the round was to
	// replay has no replay run this round (the round was cut short before
	// it, review VC-3): it may be the very check that fails, so the outcome
	// passes on no other.
	ReasonNotReplayed = "not-replayed"
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
	// Fix is non-nil only when a failure is reproduced on a check saved
	// green: a regression, which starts an automatic fix.
	Fix *FixRequest
	// Suspected is a failure reproduced only on checks saved red (fourth
	// review): a defect found by the evaluator, which maul believes but
	// never fixes on its own. Its outcomes are unsure (ReasonSuspectedDefect);
	// a person confirms it, and that starts the fix round on this request.
	Suspected *FixRequest
	// ReplayRound asks for one model-free replay round at the same heads:
	// a disputed check's replacement is confirmed and reviewed proves.
	ReplayRound bool
	// Retire names the disputed checks that replacement retires, sorted.
	Retire []string
	// Narrowed are the checks, sorted, that passed cleanly this round while
	// the evaluator saw their outcome fail as worded (sawFailure): the
	// controller records them, and from the next round on they are
	// StoredCheck.Narrowed.
	Narrowed []string
	// Env is the environment's class.
	Env     httpstep.EnvClass
	Handoff Handoff
}

// runs aggregates one check's runs this round.
type runs struct {
	pass, fail, quarantined, other bool
	// na: an error of a cause that leaves no base comparison
	// (RunCause.NoComparison); other holds every other error and refusal.
	na bool
	// untrusted: a pass with no tools run token.
	untrusted bool
	// asserted: a fail red by assertion; bare: a fail that is not (a
	// timeout, a crash, a wrong exit with no marker).
	asserted, bare bool
	failSeq        int32
}

func (r runs) clean() bool {
	return r.pass && !r.fail && !r.quarantined && !r.other && !r.na && !r.untrusted
}

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
			a.asserted = a.asserted || run.AssertionStep > 0
			a.bare = a.bare || run.AssertionStep <= 0
		case run.Result == RunQuarantined:
			a.quarantined = true
		case run.Result == RunError && run.Cause.NoComparison():
			a.na = true
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
	env      httpstep.EnvClass
	runs     map[string]runs
	base     map[string]runs
	retired  map[string]bool
	disputed map[string]CheckDispute
	verdicts map[string]EvaluatorVerdict
	// red: the digests of checks saved to show a defect; notReproduced
	// the digests of checks proposed this round to show a defect whose
	// confirmation was not red by assertion.
	red, notReproduced map[string]bool
	// redChecks are the checks saved red, by digest; dropped those of them
	// whose fail this round Decide does not count (drop).
	redChecks map[string]StoredCheck
	dropped   map[string]bool
	// guard withholds a pass the evaluator says it saw fail (sawFailure).
	guard bool
}

// reproduces reports whether run is red by the assertion check c, saved
// red, was saved failing: the same step and the same assertion of it.
func reproduces(c StoredCheck, run CheckRun) bool {
	return run.Result == RunFail && run.AssertionStep > 0 && run.AssertionStep == c.FailedStep &&
		run.AssertionID == c.FailedAssertion
}

// drop records a fail of a check saved red that Decide does not count: not
// reproduced, neither a fail nor a pass, and not evidence; a dispute may
// still retire the check (dropped).
func (d *decider) drop(run CheckRun) {
	d.notReproduced[run.CheckDigest] = true
	if run.Result == RunFail {
		d.dropped[run.CheckDigest] = true
	}
}

// confirmationRuns are the confirmations Decide counts: every run of a
// check proposed to pass, except a fail of a check saved red and not yet
// replayed green that is not red by its saved assertion (judged as its
// replay is: second review, 2A); and of one proposed to show a defect (its
// run ExpectFail) only a fail red by assertion (guard 1) of a check no
// replay this round saw green (finding 3), by the assertion it was saved
// failing (2A), and never of a check saved red that already replayed green
// (it would need a new review); its others are not reproduced. A check
// proposed red that ran green is flaky, never a pass; a check saved red
// earlier and proposed green again after a fix is confirmed as any other.
func (d *decider) confirmationRuns() []CheckRun {
	var out []CheckRun
	for _, run := range d.in.Confirmations {
		greenReplay := slices.ContainsFunc(d.in.Replay, func(r CheckRun) bool {
			return r.CheckDigest == run.CheckDigest && r.Result == RunPass
		})
		c, saved := d.redChecks[run.CheckDigest]
		switch {
		case !run.ExpectFail && saved && !c.Cleared && run.Result == RunFail && !reproduces(c, run):
			d.drop(run)
		case !run.ExpectFail:
			out = append(out, run)
		case run.Result != RunFail || run.AssertionStep == 0 || greenReplay:
			d.drop(run)
		case saved && (c.Cleared || !reproduces(c, run)):
			d.drop(run)
		default:
			out = append(out, run)
		}
	}
	return out
}

// replayRuns are the replays Decide counts: every replay, but of a check
// saved red that has not yet replayed green, a fail only red by the
// assertion it was saved failing (findings 2 and 2B: its later steps and
// other assertions never ran); its other fails are not reproduced.
func (d *decider) replayRuns() []CheckRun {
	var out []CheckRun
	for _, run := range d.in.Replay {
		if c, red := d.redChecks[run.CheckDigest]; red && !c.Cleared && run.Result == RunFail && !reproduces(c, run) {
			d.drop(run)
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
// downgrades the hand-off. A pass is also withheld, unsure, when the
// evaluator says it saw the outcome fail (sawFailure, S1 demo bug 13),
// when its only support is a check narrowed in an earlier round
// (StoredCheck.Narrowed), or when a stored check of it the round was to
// replay has no replay run (ReplayRequired, review VC-3).
//
// With a base (slice S2), rule 5 is met for a change outcome's check only
// red by assertion at base (or, n/a, where it could not compile or be built
// there: no other base error counts), for a keep outcome's only not red by
// assertion there; a required base with no run of the check is not met. A program
// check (exec, run) failing other than by assertion never fails its outcome
// (O4), and a head the tool pod could not build (a RoleBuild part) leaves
// every outcome unsure.
func Decide(in Input) Decision { return decide(in, true) }

// decide is Decide, with the saw-failure guard on or (for its property
// test) off.
func decide(in Input, guard bool) Decision {
	if httpstep.HeadBuildFailed(in.Env) {
		return buildFailed(in)
	}
	d := index(in, guard)
	d.runs = aggregate(append(d.replayRuns(), d.confirmationRuns()...))
	if in.Base != nil {
		d.base = aggregate(in.Base.Runs)
	}
	for _, r := range in.Retired {
		d.retired[r.CheckDigest] = true
	}
	for _, dp := range in.Disputes {
		// A fail Decide dropped is still a fail a dispute may retire the
		// check for (second review: no dead end).
		if d.runs[dp.CheckDigest].fail || d.dropped[dp.CheckDigest] {
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
	var failures, suspected []FixFailure
	out.PerOutcome, failures, suspected = d.outcomes()
	if len(failures) > 0 {
		out.Fix = &FixRequest{Failures: failures, Fingerprint: fixFingerprint(failures, in.Env)}
	}
	if len(suspected) > 0 {
		out.Suspected = &FixRequest{Failures: suspected, Fingerprint: fixFingerprint(suspected, in.Env)}
	}
	out.Retire, out.ReplayRound = d.replacements()
	out.Narrowed = d.narrowed()
	if out.Fix == nil && !out.ReplayRound && len(in.Anomalies) == 0 && d.env.Clean() && allPassed(out.PerOutcome) {
		out.Handoff = HandoffReady
	}
	return out
}

// index is a decider over in with its store indexed (the checks saved red),
// no run aggregated yet: what Decide and StartRung's covered share.
func index(in Input, guard bool) decider {
	d := decider{
		in: in, env: httpstep.ClassifyEnvironment(in.Env), guard: guard,
		retired: map[string]bool{}, disputed: map[string]CheckDispute{}, verdicts: map[string]EvaluatorVerdict{},
		red: map[string]bool{}, notReproduced: map[string]bool{}, redChecks: map[string]StoredCheck{},
		dropped: map[string]bool{},
	}
	for _, c := range in.Checks {
		if c.ExpectFail {
			d.red[c.Digest] = true
			if _, dup := d.redChecks[c.Digest]; !dup {
				d.redChecks[c.Digest] = c
			}
		}
	}
	return d
}

// buildFailed is the decision of a round whose head the tool pod could not
// build (tools-build-failed): CI built that head green, so it is maul's
// Infra. No outcome fails or passes, whatever the runs show; nothing is
// fixed, retired, replayed or narrowed on them.
func buildFailed(in Input) Decision {
	out := Decision{Env: httpstep.ClassifyEnvironment(in.Env), Handoff: HandoffUnconfirmed,
		PerOutcome: make([]OutcomeResult, 0, len(in.Outcomes))}
	for _, o := range in.Outcomes {
		r := OutcomeResult{ID: o.ID, Result: ResultUnsure, Reason: ReasonBuildFailed}
		if o.Manual {
			r.Reason = ReasonManual
		}
		out.PerOutcome = append(out.PerOutcome, r)
	}
	return out
}

// outcomes judges every outcome, in order, with the failures a fix round
// takes (those of checks saved green, or promoted, that write data of their
// own run) and the suspected ones: an outcome failed only on checks held
// for a person (held) is unsure, a suspected defect (fourth review, #309
// review), never an automatic fix.
func (d decider) outcomes() ([]OutcomeResult, []FixFailure, []FixFailure) {
	per := make([]OutcomeResult, 0, len(d.in.Outcomes))
	var failures, suspected []FixFailure
	for _, o := range d.in.Outcomes {
		r, fs := d.outcome(o)
		green := slices.DeleteFunc(slices.Clone(fs), func(f FixFailure) bool { return d.held(f.CheckDigest, o) })
		if r.Result == ResultFailed && len(green) == 0 {
			r.Result, r.Reason = ResultUnsure, ReasonSuspectedDefect
			suspected = append(suspected, fs...)
			green = nil
		}
		per = append(per, r)
		failures = append(failures, green...)
	}
	return per, failures, suspected
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
	if slices.ContainsFunc(bound, func(c StoredCheck) bool { r := d.runs[c.Digest]; return r.pass && r.fail }) {
		// A check that passed and failed at the same heads this round is
		// flaky (third review, F4): neither a fail nor a pass.
		res.Reason = ReasonFlaky
		return res, nil
	}
	if failing := d.failing(bound); len(failing) > 0 {
		return d.failure(o, res, failing)
	}
	if slices.ContainsFunc(bound, d.notReplayed) {
		// A check the round was to replay never ran at these heads (VC-3):
		// it may be the one that fails, so no other check passes the
		// outcome. A failure above stands on its own run.
		res.Reason = ReasonNotReplayed
		return res, nil
	}
	basis, withheld, noBase := d.passBasis(o, bound)
	switch {
	case basis != "":
		return OutcomeResult{ID: o.ID, Result: ResultPassed, Basis: basis, Reason: ReasonConfirmed}, nil
	case withheld:
		res.Reason = ReasonEvaluatorSawFailure
		return res, nil
	case noBase:
		res.Reason = ReasonNoBaseEvidence
		return res, nil
	}
	if slices.ContainsFunc(bound, func(c StoredCheck) bool { return d.notReproduced[c.Digest] }) {
		// A check proposed or saved red whose run this round maul does not
		// count: not evidence either way. Another proven check may pass the
		// outcome (above); otherwise it is unsure.
		res.Reason = ReasonNotReproduced
		return res, nil
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

// passBasis is the first of bound that passes o (rules 1-6), "" for none;
// withheld reports a check that would have but for the saw-failure guard:
// the evaluator saw o fail this round, or the check was narrowed earlier;
// noBase a check that would have but for rule 5 (it does not discriminate
// at the base).
func (d decider) passBasis(o OutcomeRef, bound []StoredCheck) (basis string, withheld, noBase bool) {
	for _, c := range bound {
		if d.notReproduced[c.Digest] || !d.passes(c, o) || !d.runs[c.Digest].clean() {
			continue
		}
		if !d.discriminates(c, o) {
			noBase = true
			continue
		}
		if d.sawFailure(o) || (d.guard && c.Narrowed) {
			withheld = true
			continue
		}
		return c.Digest, false, false
	}
	return "", withheld, noBase
}

// sawFailure reports the evaluator saying it saw o fail this round: a fail
// verdict, or any verdict naming contrary entries (S1 demo bug 13). It only
// ever withholds a pass, which is then unsure: it never fails an outcome or
// starts a fix, so a correct build is never failed by it. A check that passes
// shows only what it asserts; when the evaluator saw the outcome as worded
// fail, the check asserted less than the outcome, whatever the reviewer said.
func (d decider) sawFailure(o OutcomeRef) bool {
	return d.guard && slices.ContainsFunc(d.in.Verdicts, func(v EvaluatorVerdict) bool {
		return v.Outcome == o.ID && (v.Claim == ClaimFail || len(v.Contrary) > 0)
	})
}

// narrowed are the checks, sorted, each once, that passed cleanly this
// round, bound to an outcome the evaluator says it saw fail (sawFailure): a
// check that passes where the outcome as worded failed asserts less than
// the outcome (bug 13 review), and is never again a basis for its pass.
func (d decider) narrowed() []string {
	var out []string
	for _, o := range d.in.Outcomes {
		if o.Manual || !d.sawFailure(o) {
			continue
		}
		for _, c := range d.bound(o.ID) {
			if d.runs[c.Digest].clean() && !slices.Contains(out, c.Digest) {
				out = append(out, c.Digest)
			}
		}
	}
	sort.Strings(out)
	return out
}

// failure judges an outcome with failing checks (rule 6 already denies it
// a pass): failed only on a clean environment, the evaluator's cited fail
// and every reproduction a check that could have supported a pass.
func (d decider) failure(o OutcomeRef, res OutcomeResult, failing []StoredCheck) (OutcomeResult, []FixFailure) {
	switch d.env {
	case httpstep.EnvAppFailure:
		res.Reason = ReasonEnvFailed
		return res, nil
	case httpstep.EnvInfra:
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
	unasserted := false
	for _, c := range failing {
		if !d.supports(c, o) {
			continue
		}
		if programForm(c.Form) && d.runs[c.Digest].bare {
			// O4: a program that failed other than by assertion shows what
			// a broken check shows too; it is no evidence of a defect.
			unasserted = true
			continue
		}
		fs = append(fs, FixFailure{Outcome: o.ID, CheckDigest: c.Digest, Seq: d.runs[c.Digest].failSeq})
	}
	if !agrees || len(fs) == 0 {
		res.Reason = ReasonEvaluatorDisagrees
		if agrees && unasserted {
			res.Reason = ReasonNotAsserted
		}
		return res, nil
	}
	return OutcomeResult{ID: o.ID, Result: ResultFailed, Basis: fs[0].CheckDigest, Reason: ReasonReproduced}, fs
}

// disagree reports another check of o, not failing, that could pass it
// this round (valid, reviewed proves, run clean, discriminating): two vetted
// checks disagree on the outcome (finding 3), so it is neither failed nor
// passed. A check narrowed in an earlier round (StoredCheck.Narrowed) is no
// such check: it asserts less than the outcome, so its pass is no vote that
// the outcome holds (S1 demo bug 14), and the failure is judged on its own.
func (d decider) disagree(o OutcomeRef, failing []StoredCheck) bool {
	for _, c := range d.bound(o.ID) {
		if c.Narrowed || slices.ContainsFunc(failing, func(f StoredCheck) bool { return f.Digest == c.Digest }) {
			continue
		}
		if d.passes(c, o) && d.runs[c.Digest].clean() && d.discriminates(c, o) {
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

// notReplayed reports a check the round was to replay (ReplayRequired)
// with no replay run this round, whatever its result: a confirmation is no
// replay of it.
func (d decider) notReplayed(c StoredCheck) bool {
	return slices.Contains(d.in.ReplayRequired, c.Digest) &&
		!slices.ContainsFunc(d.in.Replay, func(r CheckRun) bool { return r.CheckDigest == c.Digest })
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
	case CheckFormHTTP, CheckFormBrowser, CheckFormExec, CheckFormRun:
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
		proves = true
		red = red || (r.ExpectFail && r.FailedStep == c.FailedStep && r.FailedAssertion == c.FailedAssertion)
	}
	return proves && (red || d.cleared(c.Digest, o) || !c.ExpectFail && !d.red[c.Digest])
}

// held reports a failure of the check that waits for a person, never an
// automatic fix: a check saved red and not promoted (cleared), or any check
// that writes no data of its own run (NoRunData, #309 review), green ones
// included.
func (d decider) held(digest string, o OutcomeRef) bool {
	noRun := slices.ContainsFunc(d.in.Checks, func(c StoredCheck) bool { return c.Digest == digest && c.NoRunData })
	return noRun || d.red[digest] && !d.cleared(digest, o)
}

// cleared reports a check saved red that has replayed green (Cleared) and
// is reviewed proves for o with the green question (fifth review, 4):
// proven against a working build, it is a saved green check from then on,
// and its regression fixes automatically. Never one admitted red only by
// the layout exception (NoRunData, #309 review): it measures whatever the
// page shows, so its failures always wait for a person.
func (d decider) cleared(digest string, o OutcomeRef) bool {
	c, red := d.redChecks[digest]
	return red && c.Cleared && !c.NoRunData && d.greenReviewed(digest, o)
}

// greenReviewed reports a review of the green question answering proves
// for the check and o.
func (d decider) greenReviewed(digest string, o OutcomeRef) bool {
	return slices.ContainsFunc(d.in.Reviews, func(r CheckReview) bool {
		return r.CheckDigest == digest && r.OutcomeDigest == o.Digest && !r.ExpectFail && r.Answer == ReviewProves
	})
}

// passes is supports for a pass: a check saved red passes its outcome only
// once a review of the green question also answered proves (third review,
// F5): its red review asked only whether its failure shows a defect, never
// whether a pass could be special-cased or merely shows a call worked.
func (d decider) passes(c StoredCheck, o OutcomeRef) bool {
	if !d.supports(c, o) {
		return false
	}
	if !c.ExpectFail && !d.red[c.Digest] {
		return true
	}
	return d.greenReviewed(c.Digest, o)
}

// discriminates is rule 5. With no base it is n/a: true. A required base
// with no run of the check is not met, unless the base could not be built
// (n/a). A change outcome's check is met only red by assertion at base, or
// where every run there errored for a cause that leaves no base comparison
// (RunCause.NoComparison: it did not compile against the base tree, a tree
// a program cannot be built against, a base that did not build): a pass, a
// quarantine, a fail with no assertion (exit only), and any other error or
// refusal (a timeout, leftover processes, cut output, a bind or connection
// error: no base evidence; SPEC-S2 S2-20, zero false passes) is not, and
// one of them beside a red run spoils it. A keep outcome's is met by a
// clean pass at base and not met red by assertion there; a fail with no
// assertion, an error or a refusal is n/a.
func (d decider) discriminates(c StoredCheck, o OutcomeRef) bool {
	if d.base == nil {
		return true
	}
	b, ok := d.base[c.Digest]
	switch {
	case !ok:
		return !d.in.Base.Required || d.in.Base.Unbuildable
	case b.quarantined || b.untrusted || (b.pass && b.fail):
		return false
	case o.Keep:
		return !b.asserted
	}
	// change: red by assertion, or only errors with no base comparison (n/a)
	return !b.pass && !b.bare && !b.other
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
			if c.Digest == dp.Replacement && d.passes(c, o) && d.runs[c.Digest].clean() {
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

func fixFingerprint(fs []FixFailure, env []httpstep.PartFact) string {
	outcomes, checks := make([]string, 0, len(fs)), make([]string, 0, len(fs))
	for _, f := range fs {
		outcomes = append(outcomes, f.Outcome)
		checks = append(checks, f.CheckDigest)
	}
	return EvaluationFingerprint(outcomes, checks, env)
}
