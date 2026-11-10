// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package httpstep

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// A step's bounds (SPEC §3.1).
const (
	MaxHeaders        = 8
	MaxHeaderBytes    = 256
	MaxExpectPointers = 8
	MaxExpectHeaders  = 4
	MaxContainsChars  = 200
	MaxCaptures       = 4
	// MaxListItems bounds an $approx or $order list.
	MaxListItems = 16
	// MaxStatuses bounds an expect.status list.
	MaxStatuses = 8
)

var (
	// KeyPattern is a repository key's grammar: a DNS label.
	KeyPattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	// HeaderPattern is a header name's grammar, as a step sends or expects
	// it.
	HeaderPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
)

// MaxKeyLength is a repository key's bound (v1alpha1.MaxRepositoryKeyLength).
const MaxKeyLength = 16

// forbiddenHeaders are the request headers a criterion may not set: they
// would retarget, smuggle or authenticate the request.
var forbiddenHeaders = map[string]bool{
	"host": true, "content-length": true, "transfer-encoding": true, "connection": true, "cookie": true,
}

var methods = map[string]bool{"GET": true, "HEAD": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true}

// CheckLine holds free text to one line of at most limit characters with no
// control or invisible-format character.
func CheckLine(field, s string, limit int) error {
	if !utf8.ValidString(s) {
		return fmt.Errorf("%s is not UTF-8", field)
	}
	if n := utf8.RuneCountInString(s); n > limit {
		return fmt.Errorf("%s is %d characters, over %d", field, n, limit)
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return fmt.Errorf("%s holds the control character %U", field, r)
		}
	}
	return nil
}

// ValidateStep checks one step; captured holds the captures earlier steps
// declared, and gains this step's.
func ValidateStep(st Step, captured map[string]bool) error {
	if err := validateRequest(st.Request, captured); err != nil {
		return err
	}
	if err := validateHTTPExpect(st.Expect, captured); err != nil {
		return err
	}
	if len(st.Capture) > MaxCaptures {
		return fmt.Errorf("more than %d captures", MaxCaptures)
	}
	for name, ptr := range st.Capture {
		if !CaptureName.MatchString(name) {
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

// ValidateStepShape checks st's request (method, path, headers, body,
// placeholders) and its capture declarations exactly as ValidateStep does,
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
		if !CaptureName.MatchString(name) {
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

// ValidateMatcher checks one expected JSON value as ValidateStep does: a
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

// checkRefs checks s's placeholders, each capture declared by an earlier
// step.
func checkRefs(field, s string, captured map[string]bool) error {
	refs, err := placeholders(s)
	if err != nil {
		return fmt.Errorf("%s: %w", field, err)
	}
	for _, r := range refs {
		if !captured[r] {
			return fmt.Errorf("%s: {{capture.%s}} is not captured by an earlier step", field, r)
		}
	}
	return nil
}

func validateRequest(r Request, captured map[string]bool) error {
	if !methods[r.Method] {
		return fmt.Errorf("method %q is not one of GET, HEAD, POST, PUT, PATCH, DELETE", r.Method)
	}
	if err := checkRefs("path", r.Path, captured); err != nil {
		return err
	}
	// The path is checked with every placeholder standing in as a plain
	// token; BuildRequest re-checks it once the values are known.
	sample, _ := substituteWith(r.Path, "run", sampleCaptures(captured), func(string) string { return "x" })
	if err := CheckPath(sample); err != nil {
		return err
	}
	if len(r.Headers) > MaxHeaders {
		return fmt.Errorf("more than %d headers", MaxHeaders)
	}
	seen := map[string]bool{}
	for name, v := range r.Headers {
		if !HeaderPattern.MatchString(name) || forbiddenHeaders[strings.ToLower(name)] {
			return fmt.Errorf("header name %q is not allowed", name)
		}
		if err := once(seen, name); err != nil {
			return err
		}
		if err := checkHeaderValue(v); err != nil {
			return fmt.Errorf("header %s: %w", name, err)
		}
		if err := checkRefs("header "+name, v, captured); err != nil {
			return err
		}
	}
	return validateBody(r, captured)
}

func sampleCaptures(captured map[string]bool) map[string]string {
	out := map[string]string{}
	for k := range captured {
		out[k] = "x"
	}
	return out
}

// checkHeaderValue holds a header value to MaxHeaderBytes of text with no
// control character and no URL.
func checkHeaderValue(v string) error {
	if len(v) > MaxHeaderBytes {
		return fmt.Errorf("value is over %d bytes", MaxHeaderBytes)
	}
	for i := 0; i < len(v); i++ {
		if v[i] < 0x20 || v[i] == 0x7f {
			return errors.New("value holds a control character")
		}
	}
	if strings.Contains(v, "://") {
		return errors.New(`value may not contain "://"`)
	}
	return nil
}

func validateBody(r Request, captured map[string]bool) error {
	switch {
	case r.JSON != nil && r.Body != "":
		return errors.New("a request has json or body, not both")
	case r.JSON != nil:
		if len(r.JSON.String()) > MaxBodyBytes {
			return fmt.Errorf("json body is over %d bytes", MaxBodyBytes)
		}
		return valueStrings(r.JSON.Decoded(), func(s string) error { return checkRefs("json body", s, captured) })
	default:
		if len(r.Body) > MaxBodyBytes {
			return fmt.Errorf("body is over %d bytes", MaxBodyBytes)
		}
		return checkRefs("body", r.Body, captured)
	}
}

func validateHTTPExpect(e Expect, captured map[string]bool) error {
	if e.Exit != nil || e.StdoutContains != "" || e.StdoutJSON != nil {
		return errors.New("an http expect has status, json, headers and bodyContains only")
	}
	if len(e.Status) > MaxStatuses {
		return fmt.Errorf("more than %d statuses", MaxStatuses)
	}
	for _, s := range e.Status {
		if s < 100 || s > 599 {
			return fmt.Errorf("status %d is not an HTTP status", s)
		}
	}
	if err := ValidatePointers("json", e.JSON, captured); err != nil {
		return err
	}
	if len(e.Headers) > MaxExpectHeaders {
		return fmt.Errorf("more than %d expected headers", MaxExpectHeaders)
	}
	seen := map[string]bool{}
	for name, v := range e.Headers {
		if !HeaderPattern.MatchString(name) {
			return fmt.Errorf("expected header name %q is not allowed", name)
		}
		if err := once(seen, name); err != nil {
			return err
		}
		if err := ValidateHeaderMatcher(v, captured); err != nil {
			return fmt.Errorf("expected header %s: %w", name, err)
		}
	}
	if err := CheckLine("bodyContains", e.BodyContains, MaxContainsChars); err != nil {
		return err
	}
	return checkRefs("bodyContains", e.BodyContains, captured)
}

// once refuses a header name already seen in another spelling: header
// names are case-insensitive, and two spellings of one would leave which
// value is sent (or which expectation is recorded) to map order.
func once(seen map[string]bool, name string) error {
	key := strings.ToLower(name)
	if seen[key] {
		return fmt.Errorf("header %q is named twice, in different cases", name)
	}
	seen[key] = true
	return nil
}

// ValidateHeaderMatcher checks one expected response header as ValidateStep
// does: an exact string or {$contains: s}.
func ValidateHeaderMatcher(v Value, captured map[string]bool) error {
	m, err := parseMatcher(v)
	if err != nil {
		return err
	}
	var field, text string
	switch m.kind {
	case matchContains:
		field, text = "$contains", m.contains
	case matchLiteral:
		s, ok := m.literal.Decoded().(string)
		if !ok {
			return errors.New("an expected header is a string or {$contains: s}")
		}
		if err := checkHeaderValue(s); err != nil {
			return err
		}
		field, text = "value", s
	default:
		return errors.New("an expected header is a string or {$contains: s}")
	}
	if err := checkRefs(field, text, captured); err != nil {
		return err
	}
	// A header is recorded only up to MaxHeaderBytes, so an expectation
	// that could be longer once substituted could never be recorded.
	if n := substitutedBound(text); n > MaxHeaderBytes {
		return fmt.Errorf("%s may be %d bytes once substituted, over %d", field, n, MaxHeaderBytes)
	}
	return nil
}

// substitutedBound is the most bytes s can take once substituted: its
// literal text, MaxRunBytes per {{run}} and MaxCaptureBytes per capture.
// s has already passed checkRefs.
func substitutedBound(s string) int {
	n := 0
	_ = scan(s, func(t string) { n += len(t) }, func(c string) error {
		if c == "" {
			n += MaxRunBytes
		} else {
			n += MaxCaptureBytes
		}
		return nil
	})
	return n
}

// ValidatePointers checks an expected-JSON map: at most MaxExpectPointers
// valid pointers, each to a literal or one matcher, captures referenced
// only once captured (nil captured: no capture may be referenced).
func ValidatePointers(field string, ps map[string]Value, captured map[string]bool) error {
	if len(ps) > MaxExpectPointers {
		return fmt.Errorf("%s: more than %d pointers", field, MaxExpectPointers)
	}
	for ptr, v := range ps {
		if err := CheckPointer(ptr); err != nil {
			return fmt.Errorf("%s: %w", field, err)
		}
		m, err := parseMatcher(v)
		if err != nil {
			return fmt.Errorf("%s %q: %w", field, ptr, err)
		}
		if err := m.eachString(func(s string) error { return checkRefs(field+" "+ptr, s, captured) }); err != nil {
			return err
		}
	}
	return nil
}
