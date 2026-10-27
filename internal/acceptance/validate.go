// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// The grammar's bounds (SPEC §3.1).
const (
	MaxCriteria       = 16
	MaxSteps          = 48
	MaxStepsPerHTTP   = 8
	MaxStatementChars = 200
	MaxReasonChars    = 120
	MaxHeaders        = 8
	MaxHeaderBytes    = 256
	MaxExpectPointers = 8
	MaxExpectHeaders  = 4
	MaxContainsChars  = 200
	MaxCaptures       = 4
	MaxArgs           = 16
	MaxArgChars       = 200
	MaxCommandTimeout = 10 * time.Minute
	// MaxListItems bounds an $approx or $order list.
	MaxListItems = 16
	// MaxStatuses bounds an expect.status list.
	MaxStatuses = 8
)

// CriterionError is a criterion the grammar refuses. It is the only error
// Validate returns for a criterion, and the only static error there is: a
// failure observed at run time is a fail verdict, never an error.
type CriterionError struct {
	// ID is the refused criterion's id, as written.
	ID string
	// Reason says what is wrong, in maul's words.
	Reason string
}

func (e *CriterionError) Error() string {
	return fmt.Sprintf("acceptance criterion %q: %s", e.ID, e.Reason)
}

var (
	idPattern     = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	keyPattern    = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	headerPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
)

// maxKeyLength is a repository key's bound (v1alpha1.MaxRepositoryKeyLength).
const maxKeyLength = 16

// forbiddenHeaders are the request headers a criterion may not set: they
// would retarget, smuggle or authenticate the request.
var forbiddenHeaders = map[string]bool{
	"host": true, "content-length": true, "transfer-encoding": true, "connection": true, "cookie": true,
}

var methods = map[string]bool{"GET": true, "HEAD": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true}

// Validate checks cs against the static grammar: at most MaxCriteria
// criteria with unique ids, at most MaxSteps http steps in all and a
// worst-case tester output (WireBound) within MaxWireBytes, each criterion
// valid for its kind. A refused criterion is a *CriterionError; a
// list-level refusal names the criterion that crossed the bound.
func Validate(cs []Criterion) error {
	if len(cs) > MaxCriteria {
		return &CriterionError{ID: cs[MaxCriteria].ID, Reason: fmt.Sprintf("more than %d criteria", MaxCriteria)}
	}
	seen := map[string]bool{}
	steps := 0
	wire := summaryWireBytes()
	for _, c := range cs {
		if seen[c.ID] {
			return &CriterionError{ID: c.ID, Reason: "id is used twice"}
		}
		seen[c.ID] = true
		if err := validateCriterion(c); err != nil {
			return &CriterionError{ID: c.ID, Reason: err.Error()}
		}
		steps += len(normalize(c).Steps)
		if steps > MaxSteps {
			return &CriterionError{ID: c.ID, Reason: fmt.Sprintf("more than %d steps in all", MaxSteps)}
		}
		if wire += criterionWireBound(c); wire > MaxWireBytes {
			return &CriterionError{ID: c.ID, Reason: fmt.Sprintf("the plan's observations could reach %d bytes, "+
				"over the %d-byte budget the tester's output must fit (MaxWireBytes); use fewer steps, "+
				"expected pointers, headers or captures", wire, MaxWireBytes)}
		}
	}
	return nil
}

func validateCriterion(c Criterion) error {
	if !idPattern.MatchString(c.ID) {
		return errors.New(`id must match ^[a-z][a-z0-9-]{0,31}$`)
	}
	if err := checkLine("statement", c.Statement, MaxStatementChars); err != nil {
		return err
	}
	if strings.TrimSpace(c.Statement) == "" {
		return errors.New("statement is required")
	}
	if err := checkLine("reason", c.Reason, MaxReasonChars); err != nil {
		return err
	}
	switch c.Kind {
	case KindHTTP:
		return validateHTTP(c)
	case KindCommand:
		return validateCommand(c)
	case KindManual:
		return validateManual(c)
	case KindBrowser, KindAgent:
		return fmt.Errorf("kind %s is not supported yet", c.Kind)
	default:
		return fmt.Errorf("kind %q is not one of http, command, manual", c.Kind)
	}
}

// checkLine holds free text to one line of at most limit characters with no
// control or invisible-format character.
func checkLine(field, s string, limit int) error {
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

func validateManual(c Criterion) error {
	if c.Target != "" || c.Request != nil || c.Expect != nil || c.Capture != nil || c.Steps != nil ||
		c.In != nil || c.Argv != nil || c.Timeout != "" {
		return errors.New("a manual criterion has only an id, a statement and a reason")
	}
	return nil
}

func validateCommand(c Criterion) error {
	if c.Target != "" || c.Request != nil || c.Capture != nil || c.Steps != nil {
		return errors.New("a command criterion has in, argv, expect and timeout, not target, request or steps")
	}
	if err := validateWhere(c.In); err != nil {
		return err
	}
	if err := validateArgv(c.Argv); err != nil {
		return err
	}
	if c.Timeout != "" {
		d, err := time.ParseDuration(c.Timeout)
		if err != nil || d <= 0 || d > MaxCommandTimeout {
			return fmt.Errorf("timeout %q must be a duration in (0, %s]", c.Timeout, MaxCommandTimeout)
		}
	}
	if c.Expect == nil {
		return errors.New("a command criterion needs an expect")
	}
	return validateCommandExpect(*c.Expect)
}

func validateWhere(in *Where) error {
	if in == nil || (in.Component == "") == (in.Repository == "") {
		return errors.New("in names exactly one of a component or a repository")
	}
	return checkKey("in", in.Component+in.Repository)
}

func checkKey(field, key string) error {
	if len(key) > maxKeyLength || !keyPattern.MatchString(key) {
		return fmt.Errorf("%s %q is not a repository key (a DNS label of at most %d characters)", field, key, maxKeyLength)
	}
	return nil
}

func validateArgv(argv []string) error {
	if len(argv) == 0 || len(argv) > MaxArgs {
		return fmt.Errorf("argv holds 1 to %d arguments", MaxArgs)
	}
	for _, a := range argv {
		if err := checkLine("an argument", a, MaxArgChars); err != nil {
			return err
		}
	}
	if argv[0] == "" {
		return errors.New("argv[0] is empty")
	}
	return nil
}

func validateCommandExpect(e Expect) error {
	if e.Status != nil || e.JSON != nil || e.Headers != nil || e.BodyContains != "" {
		return errors.New("a command expect has only exit, stdoutContains and stdoutJSON")
	}
	if err := checkLine("stdoutContains", e.StdoutContains, MaxContainsChars); err != nil {
		return err
	}
	return validatePointers("stdoutJSON", e.StdoutJSON, nil)
}

func validateHTTP(c Criterion) error {
	if c.In != nil || c.Argv != nil || c.Timeout != "" {
		return errors.New("an http criterion has target and steps, not in, argv or timeout")
	}
	if err := checkKey("target", c.Target); err != nil {
		return err
	}
	shorthand := c.Request != nil || c.Expect != nil || c.Capture != nil
	if shorthand && c.Steps != nil {
		return errors.New("write either steps or one request/expect pair, not both")
	}
	if shorthand && c.Request == nil {
		return errors.New("a request is required")
	}
	steps := normalize(c).Steps
	if len(steps) == 0 || len(steps) > MaxStepsPerHTTP {
		return fmt.Errorf("an http criterion has 1 to %d steps", MaxStepsPerHTTP)
	}
	captured := map[string]bool{}
	names := map[string]bool{}
	for i, st := range steps {
		if st.Name != "" {
			if !idPattern.MatchString(st.Name) || names[st.Name] {
				return fmt.Errorf("step %d: name %q is not a unique ^[a-z][a-z0-9-]{0,31}$", i+1, st.Name)
			}
			names[st.Name] = true
		}
		if err := validateStep(st, captured); err != nil {
			return fmt.Errorf("step %d: %w", i+1, err)
		}
	}
	return nil
}

// validateStep checks one step; captured holds the captures earlier steps
// declared, and gains this step's.
func validateStep(st Step, captured map[string]bool) error {
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
		if !headerPattern.MatchString(name) || forbiddenHeaders[strings.ToLower(name)] {
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
		return valueStrings(r.JSON.decoded(), func(s string) error { return checkRefs("json body", s, captured) })
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
	if err := validatePointers("json", e.JSON, captured); err != nil {
		return err
	}
	if len(e.Headers) > MaxExpectHeaders {
		return fmt.Errorf("more than %d expected headers", MaxExpectHeaders)
	}
	seen := map[string]bool{}
	for name, v := range e.Headers {
		if !headerPattern.MatchString(name) {
			return fmt.Errorf("expected header name %q is not allowed", name)
		}
		if err := once(seen, name); err != nil {
			return err
		}
		if err := validateHeaderExpect(v, captured); err != nil {
			return fmt.Errorf("expected header %s: %w", name, err)
		}
	}
	if err := checkLine("bodyContains", e.BodyContains, MaxContainsChars); err != nil {
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

func validateHeaderExpect(v Value, captured map[string]bool) error {
	m, err := parseMatcher(v)
	if err != nil {
		return err
	}
	var field, text string
	switch m.kind {
	case matchContains:
		field, text = "$contains", m.contains
	case matchLiteral:
		s, ok := m.literal.decoded().(string)
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
// literal text, maxRunBytes per {{run}} and MaxCaptureBytes per capture.
// s has already passed checkRefs.
func substitutedBound(s string) int {
	n := 0
	_ = scan(s, func(t string) { n += len(t) }, func(c string) error {
		if c == "" {
			n += maxRunBytes
		} else {
			n += MaxCaptureBytes
		}
		return nil
	})
	return n
}

// validatePointers checks an expected-JSON map: at most MaxExpectPointers
// valid pointers, each to a literal or one matcher, captures referenced
// only once captured (nil captured: no capture may be referenced).
func validatePointers(field string, ps map[string]Value, captured map[string]bool) error {
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
