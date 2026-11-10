// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package httpstep

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// MatchValue judges an observed value (canonical JSON, TooLarge, or absent)
// against one expected value ValidateMatcher admits, under run and the
// captures.
func MatchValue(expected Value, run string, caps map[string]string, raw string, present bool) bool {
	m, err := parseMatcher(expected)
	if err != nil {
		return false
	}
	m, err = m.substitute(run, caps)
	return err == nil && m.match(raw, present)
}

// MatchHeaderValue judges an observed header against one expected header
// ValidateHeaderMatcher admits: an exact string or $contains, over the
// observed text (not JSON).
func MatchHeaderValue(expected Value, run string, caps map[string]string, raw string, present bool) bool {
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
		s, ok := m.literal.Decoded().(string)
		return ok && s == raw
	default:
		return false
	}
}

// JSONEqual reports whether two values are equal as the matchers compare
// them: numbers by value, objects regardless of key order.
func JSONEqual(a, b Value) bool { return jsonEqual(a.Decoded(), b.Decoded()) }

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
	d := v.Decoded()
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
		if !ok || s == "" || CheckLine("$contains", s, MaxContainsChars) != nil {
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
		v, err := ValueOf(e)
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
		return valueStrings(m.literal.Decoded(), f)
	case matchContains:
		return f(m.contains)
	case matchOrder:
		for _, v := range m.order {
			if err := valueStrings(v.Decoded(), f); err != nil {
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
		return jsonEqual(m.literal.Decoded(), observed)
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
		w := want.Decoded()
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

// Reduce is v, the observed value at one of a step's expected JSON
// pointers, as recorded for expected under run and the captures: bounded,
// and judged by MatchValue exactly as v itself would be. An expectation that
// does not parse or substitute records v as a literal's would.
func Reduce(expected Value, run string, caps map[string]string, v any) string {
	m, err := parseMatcher(expected)
	if err == nil {
		m, err = m.substitute(run, caps)
	}
	if err != nil {
		m = matcher{kind: matchLiteral}
	}
	return m.reduce(v)
}

// ContainsNeedle is expected's $contains text under run and the captures,
// when expected is a {$contains: s} that substitutes.
func ContainsNeedle(expected Value, run string, caps map[string]string) (string, bool) {
	m, err := parseMatcher(expected)
	if err != nil || m.kind != matchContains {
		return "", false
	}
	if m, err = m.substitute(run, caps); err != nil {
		return "", false
	}
	return m.contains, true
}

// reduce is the observed value v as recorded for m: bounded, and judged by
// m exactly as v itself would be.
func (m matcher) reduce(v any) string {
	switch m.kind {
	case matchContains:
		if s, ok := v.(string); ok && len(quote(s)) > MaxValueBytes {
			if strings.Contains(s, m.contains) {
				return bounded(quote(m.contains))
			}
			// A prefix of a text that lacks the needle lacks it too.
			return bounded(quote(TruncateUTF8(s, MaxValueBytes/2)))
		}
	case matchOrder:
		if list, ok := v.([]any); ok {
			return bounded(m.project(list))
		}
	}
	raw, err := ValueOf(v)
	if err != nil {
		return TooLarge
	}
	return bounded(raw.String())
}

// project keeps, in order, the first occurrence of each expected value
// among list's elements (each read at $by): the first occurrences are all
// matchOrder judges, so the projection judges as the list does.
func (m matcher) project(list []any) string {
	var kept []any
	found := make([]bool, len(m.order))
	for _, e := range list {
		if m.by != "" {
			var ok bool
			if e, ok = ResolvePointer(e, m.by); !ok {
				continue
			}
		}
		for i, want := range m.order {
			if !found[i] && jsonEqual(want.Decoded(), e) {
				found[i] = true
				kept = append(kept, e)
				break
			}
		}
	}
	if kept == nil {
		kept = []any{}
	}
	raw, err := ValueOf(kept)
	if err != nil {
		return TooLarge
	}
	return raw.String()
}

func bounded(raw string) string {
	if len(raw) > MaxValueBytes {
		return TooLarge
	}
	return raw
}

func quote(s string) string {
	v, _ := ValueOf(s)
	return v.String()
}

// TruncateUTF8 cuts s to at most n bytes on a rune boundary.
func TruncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
