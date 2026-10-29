// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import "fmt"

// The functions below expose, unchanged, the pieces of this grammar the
// saved-check grammar (internal/checks) builds on by import: a step's
// request and capture declarations, one expected value's matcher, and the
// matching itself. They add no behaviour; slice-1 callers are untouched.

// ValidateStepShape checks st's request (method, path, headers, body,
// placeholders) and its capture declarations exactly as Validate does,
// leaving its expectation to the caller. The method defaults to GET.
// captured holds the captures earlier steps declared and gains st's.
func ValidateStepShape(st Step, captured map[string]bool) error {
	if st.Request.Method == "" {
		st.Request.Method = "GET"
	}
	if err := validateRequest(st.Request, captured); err != nil {
		return err
	}
	if len(st.Capture) > MaxCaptures {
		return fmt.Errorf("more than %d captures", MaxCaptures)
	}
	for name, ptr := range st.Capture {
		if !captureName.MatchString(name) {
			return fmt.Errorf("capture name %q must match ^[a-z][a-z0-9]{0,15}$", name)
		}
		if captured[name] {
			return fmt.Errorf("capture %q is declared twice", name)
		}
		if err := CheckPointer(ptr); err != nil {
			return fmt.Errorf("capture %s: %w", name, err)
		}
	}
	for name := range st.Capture {
		captured[name] = true
	}
	return nil
}

// ValidateMatcher checks one expected JSON value as Validate does: a
// literal or one of the matchers {$present}, {$absent}, {$contains},
// {$approx, $tolerance} or {$order[, $by]}, every placeholder in it known
// (nil captured: no capture may be referenced).
func ValidateMatcher(v Value, captured map[string]bool) error {
	m, err := parseMatcher(v)
	if err != nil {
		return err
	}
	return m.eachString(func(s string) error { return checkRefs("value", s, captured) })
}

// ValidateHeaderMatcher checks one expected response header as Validate
// does: an exact string or {$contains: s}.
func ValidateHeaderMatcher(v Value, captured map[string]bool) error {
	return validateHeaderExpect(v, captured)
}

// MatchValue judges an observed value (canonical JSON, TooLarge, or absent)
// against one expected value ValidateMatcher admits, under run and the
// captures, as Judge does.
func MatchValue(expected Value, run string, caps map[string]string, raw string, present bool) bool {
	return matches(expected, run, caps, raw, present)
}

// MatchHeaderValue judges an observed header against one expected header
// ValidateHeaderMatcher admits, as Judge does.
func MatchHeaderValue(expected Value, run string, caps map[string]string, raw string, present bool) bool {
	return matchesHeader(expected, run, caps, raw, present)
}

// JSONEqual reports whether two values are equal as Judge compares them:
// numbers by value, objects regardless of key order.
func JSONEqual(a, b Value) bool { return jsonEqual(a.decoded(), b.decoded()) }

// Decoded is v decoded, numbers as json.Number.
func (v Value) Decoded() any { return v.decoded() }

// ValueOf encodes a decoded value (numbers as json.Number) canonically.
func ValueOf(d any) (Value, error) { return valueOf(d) }
