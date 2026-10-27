// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"slices"
	"sort"
	"strconv"
	"strings"
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
	Index     int       `json:"index"`
	Status    int32     `json:"status"`
	Transport Transport `json:"transport,omitempty"`
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
func JudgeStep(st Step, o StepObservation, run string, caps map[string]string) StepVerdict {
	st = normalize(Criterion{Kind: KindHTTP, Steps: []Step{st}}).Steps[0]
	transportOK := o.TransportError == TransportNone || o.TransportError == ""
	v := StepVerdict{Status: o.Status, Transport: o.TransportError}
	if v.Transport == "" {
		v.Transport = TransportNone
	}
	built, err := BuildRequest(st, run, caps)
	v.Matched = append(v.Matched, err == nil && built.Method == o.Method && built.Target() == o.Path)
	v.Matched = append(v.Matched, transportOK && slices.Contains(st.Expect.Status, o.Status))
	for _, ptr := range sortedKeys(st.Expect.JSON) {
		raw, present := o.Pointers[ptr]
		v.Matched = append(v.Matched, transportOK && o.JSONValid &&
			matches(st.Expect.JSON[ptr], run, caps, raw, present))
	}
	for _, name := range sortedFold(st.Expect.Headers) {
		raw, present := o.Headers[strings.ToLower(name)]
		v.Matched = append(v.Matched, transportOK && matchesHeader(st.Expect.Headers[name], run, caps, raw, present))
	}
	if st.Expect.BodyContains != "" {
		v.Matched = append(v.Matched, transportOK && bodyContains(st.Expect.BodyContains, run, caps, o.Pointers))
	}
	for _, name := range sortedKeys(st.Capture) {
		val, ok := o.Captures[name]
		v.Matched = append(v.Matched, transportOK && ok && len(val) <= MaxCaptureBytes)
	}
	v.Pass = transportOK && !slices.Contains(v.Matched, false)
	return v
}

func matches(expected Value, run string, caps map[string]string, raw string, present bool) bool {
	m, err := parseMatcher(expected)
	if err != nil {
		return false
	}
	m, err = m.substitute(run, caps)
	return err == nil && m.match(raw, present)
}

// matchesHeader judges an expected header: an exact string or $contains,
// over the observed text (not JSON).
func matchesHeader(expected Value, run string, caps map[string]string, raw string, present bool) bool {
	if !present || raw == TooLarge {
		return false
	}
	m, err := parseMatcher(expected)
	if err != nil {
		return false
	}
	if m, err = m.substitute(run, caps); err != nil {
		return false
	}
	switch m.kind {
	case matchContains:
		return strings.Contains(raw, m.contains)
	case matchLiteral:
		s, ok := m.literal.decoded().(string)
		return ok && s == raw
	default:
		return false
	}
}

// bodyContains judges a bodyContains expectation: the recorded proof must
// be exactly the proof of the needle as Judge substitutes it, so a proof
// that is missing, malformed or of another needle fails.
func bodyContains(needle, run string, caps map[string]string, pointers map[string]string) bool {
	n, err := Substitute(needle, run, caps)
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

// Matchers.

type matchKind int

const (
	matchLiteral matchKind = iota
	matchPresent
	matchAbsent
	matchContains
	matchApprox
	matchOrder
)

const (
	// MaxValueBytes bounds an expected literal and an observed value.
	MaxValueBytes = 256
	// maxMatcherBytes bounds a whole matcher object.
	maxMatcherBytes = 2 << 10
)

// matcher is one parsed expected value.
type matcher struct {
	kind       matchKind
	literal    Value
	contains   string
	approx     []float64
	approxList bool
	tolerance  float64
	order      []Value
	by         string
}

// parseMatcher reads an expected value: a literal holding no "$" key at any
// depth, or exactly one matcher object.
func parseMatcher(v Value) (matcher, error) {
	d := v.decoded()
	obj, isObj := d.(map[string]any)
	if !isObj || !hasDollarKey(obj) {
		if dollarAnywhere(d) {
			return matcher{}, errors.New(`a literal may not hold a "$" key; use one matcher object`)
		}
		if len(v.String()) > MaxValueBytes {
			return matcher{}, fmt.Errorf("a literal is over %d bytes", MaxValueBytes)
		}
		return matcher{kind: matchLiteral, literal: v}, nil
	}
	if len(v.String()) > maxMatcherBytes {
		return matcher{}, fmt.Errorf("a matcher is over %d bytes", maxMatcherBytes)
	}
	switch {
	case oneKey(obj, "$present"):
		return flagMatcher(obj["$present"], matchPresent, "$present")
	case oneKey(obj, "$absent"):
		return flagMatcher(obj["$absent"], matchAbsent, "$absent")
	case oneKey(obj, "$contains"):
		s, ok := obj["$contains"].(string)
		if !ok || s == "" || checkLine("$contains", s, MaxContainsChars) != nil {
			return matcher{}, fmt.Errorf("$contains takes text of 1 to %d characters", MaxContainsChars)
		}
		return matcher{kind: matchContains, contains: s}, nil
	case len(obj) == 2 && has(obj, "$approx") && has(obj, "$tolerance"):
		return approxMatcher(obj)
	case oneKey(obj, "$order") || (len(obj) == 2 && has(obj, "$order") && has(obj, "$by")):
		return orderMatcher(obj)
	default:
		return matcher{}, errors.New("a matcher is one of {$present: true}, {$absent: true}, {$contains: s}, " +
			"{$approx: n, $tolerance: n} or {$order: [...], $by: pointer}")
	}
}

func flagMatcher(v any, kind matchKind, name string) (matcher, error) {
	if b, ok := v.(bool); !ok || !b {
		return matcher{}, fmt.Errorf("%s takes true", name)
	}
	return matcher{kind: kind}, nil
}

func approxMatcher(obj map[string]any) (matcher, error) {
	m := matcher{kind: matchApprox}
	tol, ok := number(obj["$tolerance"])
	if !ok || tol < 0 {
		return matcher{}, errors.New("$tolerance takes a number of at least 0")
	}
	m.tolerance = tol
	switch a := obj["$approx"].(type) {
	case []any:
		if len(a) == 0 || len(a) > MaxListItems {
			return matcher{}, fmt.Errorf("$approx takes 1 to %d numbers", MaxListItems)
		}
		m.approxList = true
		for _, e := range a {
			n, ok := number(e)
			if !ok {
				return matcher{}, errors.New("$approx takes numbers")
			}
			m.approx = append(m.approx, n)
		}
	default:
		n, ok := number(a)
		if !ok {
			return matcher{}, errors.New("$approx takes a number or a list of numbers")
		}
		m.approx = []float64{n}
	}
	return m, nil
}

func orderMatcher(obj map[string]any) (matcher, error) {
	list, ok := obj["$order"].([]any)
	if !ok || len(list) == 0 || len(list) > MaxListItems {
		return matcher{}, fmt.Errorf("$order takes a list of 1 to %d values", MaxListItems)
	}
	m := matcher{kind: matchOrder}
	if by, has := obj["$by"]; has {
		s, ok := by.(string)
		if !ok || CheckPointer(s) != nil {
			return matcher{}, errors.New("$by takes a JSON pointer")
		}
		m.by = s
	}
	for _, e := range list {
		if dollarAnywhere(e) {
			return matcher{}, errors.New(`an $order value may not hold a "$" key`)
		}
		v, err := valueOf(e)
		if err != nil {
			return matcher{}, err
		}
		if len(v.String()) > MaxValueBytes {
			return matcher{}, fmt.Errorf("an $order value is over %d bytes", MaxValueBytes)
		}
		m.order = append(m.order, v)
	}
	return m, nil
}

func number(v any) (float64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(string(n), 64)
	return f, err == nil && !math.IsInf(f, 0) && !math.IsNaN(f)
}

func has(obj map[string]any, k string) bool { _, ok := obj[k]; return ok }

func oneKey(obj map[string]any, k string) bool { return len(obj) == 1 && has(obj, k) }

func hasDollarKey(obj map[string]any) bool {
	for k := range obj {
		if strings.HasPrefix(k, "$") {
			return true
		}
	}
	return false
}

func dollarAnywhere(v any) bool {
	switch t := v.(type) {
	case map[string]any:
		if hasDollarKey(t) {
			return true
		}
		for _, e := range t {
			if dollarAnywhere(e) {
				return true
			}
		}
	case []any:
		for _, e := range t {
			if dollarAnywhere(e) {
				return true
			}
		}
	}
	return false
}

// eachString calls f on every text the matcher substitutes into.
func (m matcher) eachString(f func(string) error) error {
	switch m.kind {
	case matchLiteral:
		return valueStrings(m.literal.decoded(), f)
	case matchContains:
		return f(m.contains)
	case matchOrder:
		for _, v := range m.order {
			if err := valueStrings(v.decoded(), f); err != nil {
				return err
			}
		}
	}
	return nil
}

// substitute returns m with run and the captures substituted into its texts.
func (m matcher) substitute(run string, caps map[string]string) (matcher, error) {
	var err error
	switch m.kind {
	case matchLiteral:
		m.literal, err = substituteValue(m.literal, run, caps)
	case matchContains:
		m.contains, err = Substitute(m.contains, run, caps)
	case matchOrder:
		order := make([]Value, len(m.order))
		for i, v := range m.order {
			if order[i], err = substituteValue(v, run, caps); err != nil {
				return matcher{}, err
			}
		}
		m.order = order
	}
	return m, err
}

// match judges an observed raw value (canonical JSON, TooLarge, or absent).
func (m matcher) match(raw string, present bool) bool {
	switch m.kind {
	case matchPresent:
		return present
	case matchAbsent:
		return !present
	}
	if !present || raw == TooLarge {
		return false
	}
	observed, err := decodeJSON([]byte(raw))
	if err != nil {
		return false
	}
	switch m.kind {
	case matchContains:
		s, ok := observed.(string)
		return ok && strings.Contains(s, m.contains)
	case matchApprox:
		return m.matchApprox(observed)
	case matchOrder:
		return m.matchOrder(observed)
	default:
		return jsonEqual(m.literal.decoded(), observed)
	}
}

func (m matcher) matchApprox(observed any) bool {
	var got []any
	if m.approxList {
		list, ok := observed.([]any)
		if !ok {
			return false
		}
		got = list
	} else {
		got = []any{observed}
	}
	if len(got) != len(m.approx) {
		return false
	}
	for i, g := range got {
		n, ok := number(g)
		if !ok || math.Abs(n-m.approx[i]) > m.tolerance {
			return false
		}
	}
	return true
}

// matchOrder: every expected value occurs in the observed list, and their
// first occurrences come in the expected order.
func (m matcher) matchOrder(observed any) bool {
	list, ok := observed.([]any)
	if !ok {
		return false
	}
	last := -1
	for _, want := range m.order {
		w := want.decoded()
		at := slices.IndexFunc(list, func(e any) bool { return jsonEqual(w, e) })
		if at <= last {
			return false
		}
		last = at
	}
	return true
}

// jsonEqual compares decoded JSON values, numbers by value.
func jsonEqual(a, b any) bool {
	switch x := a.(type) {
	case json.Number:
		y, ok := b.(json.Number)
		return ok && numberEqual(string(x), string(y))
	case []any:
		y, ok := b.([]any)
		return ok && slices.EqualFunc(x, y, jsonEqual)
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, xv := range x {
			yv, ok := y[k]
			if !ok || !jsonEqual(xv, yv) {
				return false
			}
		}
		return true
	default:
		return a == b
	}
}

// numberEqual compares two JSON numbers by value: integers exactly (both
// texts are bounded, so big.Int is cheap), anything else as float64. An
// exponent too large for a float64 compares by text, never by building the
// number.
func numberEqual(a, b string) bool {
	if a == b {
		return true
	}
	fa, errA := strconv.ParseFloat(a, 64)
	fb, errB := strconv.ParseFloat(b, 64)
	if errA != nil || errB != nil || fa != fb {
		return false
	}
	if isInteger(a) && isInteger(b) {
		ia, okA := new(big.Int).SetString(a, 10)
		ib, okB := new(big.Int).SetString(b, 10)
		return okA && okB && ia.Cmp(ib) == 0
	}
	return true
}

func isInteger(s string) bool { return !strings.ContainsAny(s, ".eE") }
