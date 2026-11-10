// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/devthenet-labs/maul/internal/httpstep"
)

// Outcome is a criterion's verdict.
type Outcome string

const (
	// OutcomePass: the first try met every expectation.
	OutcomePass Outcome = "pass"
	// OutcomeQuarantined: the first try failed and the retry passed. Shown
	// to the human, never counted as passed (D7).
	OutcomeQuarantined Outcome = "quarantined"
	// OutcomeFail: anything else, including every transport failure and
	// every observation that does not fit its criterion.
	OutcomeFail Outcome = "fail"
)

// Verdict reasons, a fixed vocabulary: never text from an observation.
const (
	ReasonInvalidCriterion = "invalid-criterion"
	ReasonNotObservable    = "not-observable"
	ReasonIDMismatch       = "id-mismatch"
	ReasonTries            = "tries"
	ReasonRunToken         = "run-token"
	ReasonSteps            = "steps"
	ReasonExpectation      = "expectation"
	// ReasonObservationTooLarge: the observation was too large to keep and
	// was replaced by its marker (CriterionObservation.TooLarge).
	ReasonObservationTooLarge = "observation-too-large"
)

// RunTokens identify one verification attempt: its round, its attempt and
// the random hex maul drew for it. Each try of a criterion runs under its
// own {{run}} token (Token).
type RunTokens struct {
	Round, Attempt int32
	// Run is six random lowercase hex characters.
	Run string
}

// Token is the {{run}} value of try (1 or 2): v<round>a<attempt>t<try>-<run>.
func (r RunTokens) Token(try int) string {
	return fmt.Sprintf("v%da%dt%d-%s", r.Round, r.Attempt, try, r.Run)
}

// Verdict is the judgement of one criterion.
type Verdict struct {
	ID      string  `json:"id"`
	Outcome Outcome `json:"outcome"`
	// Reason is empty on a pass, else one of the Reason constants.
	Reason string `json:"reason,omitempty"`
	// Steps are the step verdicts of the deciding (last) try, in order, up
	// to the first that failed.
	Steps []StepVerdict `json:"steps,omitempty"`
}

// StepVerdict is the judgement of one observed step.
type StepVerdict struct {
	Index     int                `json:"index"`
	Status    int32              `json:"status"`
	Transport httpstep.Transport `json:"transport,omitempty"`
	// Matched holds one bit per declared expectation, in a fixed order:
	// the request was sent as declared; the status; each json pointer in
	// sorted order; each expected header in sorted order (case folded);
	// bodyContains, when declared; each capture in sorted order.
	Matched []bool `json:"matched"`
	Pass    bool   `json:"pass"`
}

// Judge judges an observation of c under rt: pass when the first try met
// every expectation, quarantined when only the retry did, fail otherwise.
// It re-derives everything the tester could have got wrong (the run token
// of each try, each request as sent, each capture's use) and never errors:
// an invalid criterion, an unobservable kind, an observation that does not
// fit or one marked TooLarge is a fail with a Reason.
func Judge(c Criterion, obs CriterionObservation, rt RunTokens) Verdict {
	fail := func(reason string, steps []StepVerdict) Verdict {
		return Verdict{ID: c.ID, Outcome: OutcomeFail, Reason: reason, Steps: steps}
	}
	if Validate([]Criterion{c}) != nil {
		return fail(ReasonInvalidCriterion, nil)
	}
	c = normalize(c)
	switch {
	case c.Kind != KindHTTP:
		return fail(ReasonNotObservable, nil)
	case obs.ID != c.ID:
		return fail(ReasonIDMismatch, nil)
	case obs.TooLarge:
		return fail(ReasonObservationTooLarge, nil)
	case len(obs.Tries) == 0 || len(obs.Tries) > 2:
		return fail(ReasonTries, nil)
	}
	first, firstReason := judgeTry(c, obs.Tries[0], rt.Token(1))
	if firstReason == "" {
		if len(obs.Tries) != 1 {
			return fail(ReasonTries, first) // a passing try is never retried
		}
		return Verdict{ID: c.ID, Outcome: OutcomePass, Steps: first}
	}
	if len(obs.Tries) == 1 {
		return fail(firstReason, first)
	}
	second, reason := judgeTry(c, obs.Tries[1], rt.Token(2))
	if reason == "" {
		return Verdict{ID: c.ID, Outcome: OutcomeQuarantined, Steps: second}
	}
	return fail(reason, second)
}

// judgeTry judges one try of a normalised http criterion; reason is empty
// when it passed.
func judgeTry(c Criterion, try TryObservation, run string) ([]StepVerdict, string) {
	if try.Run != run {
		return nil, ReasonRunToken
	}
	caps := map[string]string{}
	var out []StepVerdict
	for i, so := range try.Steps {
		if i >= len(c.Steps) || (i > 0 && !out[i-1].Pass) {
			return out, ReasonSteps
		}
		v := JudgeStep(c.Steps[i], so, run, caps)
		v.Index = i
		out = append(out, v)
		for name := range c.Steps[i].Capture {
			if val, ok := so.Captures[name]; ok {
				caps[name] = val
			}
		}
	}
	switch {
	case len(out) > 0 && !out[len(out)-1].Pass:
		return out, ReasonExpectation
	case len(out) != len(c.Steps):
		return out, ReasonSteps
	}
	return out, ""
}

// JudgeStep judges one step's observation under run and the captures of
// the steps before it. The tester uses it only to decide whether to stop a
// try and retry; the controller re-judges the whole criterion with Judge.
func JudgeStep(st httpstep.Step, o StepObservation, run string, caps map[string]string) StepVerdict {
	st = normalize(Criterion{Kind: KindHTTP, Steps: []httpstep.Step{st}}).Steps[0]
	transportOK := o.TransportError == httpstep.TransportNone || o.TransportError == ""
	v := StepVerdict{Status: o.Status, Transport: o.TransportError}
	if v.Transport == "" {
		v.Transport = httpstep.TransportNone
	}
	built, err := httpstep.BuildRequest(st, run, caps)
	v.Matched = append(v.Matched, err == nil && built.Method == o.Method && built.Target() == o.Path)
	v.Matched = append(v.Matched, transportOK && slices.Contains(st.Expect.Status, o.Status))
	for _, ptr := range sortedKeys(st.Expect.JSON) {
		raw, present := o.Pointers[ptr]
		v.Matched = append(v.Matched, transportOK && o.JSONValid &&
			httpstep.MatchValue(st.Expect.JSON[ptr], run, caps, raw, present))
	}
	for _, name := range sortedFold(st.Expect.Headers) {
		raw, present := o.Headers[strings.ToLower(name)]
		v.Matched = append(v.Matched,
			transportOK && httpstep.MatchHeaderValue(st.Expect.Headers[name], run, caps, raw, present))
	}
	if st.Expect.BodyContains != "" {
		v.Matched = append(v.Matched, transportOK && bodyContains(st.Expect.BodyContains, run, caps, o.Pointers))
	}
	for _, name := range sortedKeys(st.Capture) {
		val, ok := o.Captures[name]
		v.Matched = append(v.Matched, transportOK && ok && len(val) <= httpstep.MaxCaptureBytes)
	}
	v.Pass = transportOK && !slices.Contains(v.Matched, false)
	return v
}

// bodyContains judges a bodyContains expectation: the recorded proof must
// be exactly the proof of the needle as Judge substitutes it, so a proof
// that is missing, malformed or of another needle fails.
func bodyContains(needle, run string, caps map[string]string, pointers map[string]string) bool {
	n, err := httpstep.Substitute(needle, run, caps)
	if err != nil {
		return false
	}
	proof, ok := pointers[KeyBodyContains]
	return ok && proof == bodyContainsProof(n)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sortedFold sorts header names case-insensitively, ties broken bytewise,
// so the matched bits keep one order however the plan spelled them.
func sortedFold[V any](m map[string]V) []string {
	out := sortedKeys(m)
	sort.SliceStable(out, func(i, j int) bool { return strings.ToLower(out[i]) < strings.ToLower(out[j]) })
	return out
}
