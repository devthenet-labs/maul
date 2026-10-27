// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"maps"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The observation's bounds. The PreviewVerification CRD repeats them as
// maxLength/maxItems.
const (
	// MaxResponseBytes is the most of a response body the tester reads;
	// past it the step is TransportTooLarge.
	MaxResponseBytes = 64 << 10
	// MaxCaptureBytes bounds a captured value.
	MaxCaptureBytes = 128
	// MaxBodyPrefixBytes bounds the display-only body prefix, before base64.
	MaxBodyPrefixBytes = 512
	// maxRunBytes bounds a try's recorded run token.
	maxRunBytes = 64
)

// TooLarge stands for an observed value over MaxValueBytes. It is not JSON,
// so it can never equal an expected value; only $present judges it.
const TooLarge = "!too-large"

// KeyBodyContains is the reserved Pointers key holding the bodyContains
// observation: the expected text as a JSON string when the body contains
// it, absent otherwise. It is not a JSON pointer, so it cannot collide.
const KeyBodyContains = "$bodyContains"

// Transport classifies how a request failed before a response was judged.
type Transport string

// Transport classes. Anything but TransportNone fails the step.
const (
	TransportNone           Transport = "none"
	TransportRefused        Transport = "refused"
	TransportTimeout        Transport = "timeout"
	TransportReset          Transport = "reset"
	TransportDNS            Transport = "dns"
	TransportTLS            Transport = "tls"
	TransportTooLarge       Transport = "too-large"
	TransportCaptureMissing Transport = "capture-missing"
)

var transports = map[Transport]bool{
	TransportNone: true, TransportRefused: true, TransportTimeout: true, TransportReset: true,
	TransportDNS: true, TransportTLS: true, TransportTooLarge: true, TransportCaptureMissing: true,
}

// CriterionObservation is what the tester saw of one criterion: one try,
// or two when the first failed.
type CriterionObservation struct {
	ID    string           `json:"id"`
	Tries []TryObservation `json:"tries,omitempty"`
}

// TryObservation is one try: the {{run}} token it ran under and its steps,
// up to the first that failed.
type TryObservation struct {
	Run   string            `json:"run"`
	Steps []StepObservation `json:"steps,omitempty"`
}

// StepObservation is one request and what came back, bounded. Judge reads
// only these raw facts, never a verdict of the tester's.
type StepObservation struct {
	// Method and Path are the request as sent (Path with its query).
	Method string `json:"method"`
	Path   string `json:"path"`
	Status int32  `json:"status"`
	// TransportError is TransportNone or how the request failed.
	TransportError Transport `json:"transportError"`
	JSONValid      bool      `json:"jsonValid"`
	BodyBytes      int32     `json:"bodyBytes"`
	LatencyMs      int32     `json:"latencyMs"`
	// Pointers holds, for each expected JSON pointer, the canonical JSON
	// there, reduced so that it judges as the full value would (an $order
	// list projected to its expected values, a long $contains string to the
	// expected text or a prefix without it, anything else over
	// MaxValueBytes to TooLarge); an absent key is an absent value. Plus
	// KeyBodyContains.
	Pointers map[string]string `json:"pointers,omitempty"`
	// Captures holds each declared capture that resolved to a string,
	// number or boolean of at most MaxCaptureBytes.
	Captures map[string]string `json:"captures,omitempty"`
	// Headers holds each expected response header, keyed lowercase, its
	// values joined by ", ", reduced like Pointers.
	Headers map[string]string `json:"headers,omitempty"`
	// BodyPrefix is the first MaxBodyPrefixBytes of the body, base64. For
	// display only: never judged.
	BodyPrefix string `json:"bodyPrefix,omitempty"`
}

// Part roles, as PartFact.Role.
const (
	RoleComponent = "component"
	RoleService   = "service"
	RoleTester    = "tester"
	RoleCommand   = "command"
	RoleSeed      = "seed"
)

// PartFact is the environment evidence of one part of a Preview's slot,
// from its pod's container status.
type PartFact struct {
	Name     string `json:"name"`
	Role     string `json:"role"`
	Ready    bool   `json:"ready"`
	Restarts int32  `json:"restarts"`
	// Waiting is the container's waiting reason (CrashLoopBackOff,
	// ImagePullBackOff, …), empty when it is not waiting.
	Waiting string `json:"waiting,omitempty"`
	// LastTermination is the container's last (or, for a finished Job's
	// container, current) termination.
	LastTermination *Termination `json:"lastTermination,omitempty"`
	Evicted         bool         `json:"evicted,omitempty"`
	Unschedulable   bool         `json:"unschedulable,omitempty"`
	// LogTail is opaque display text: never judged, never fingerprinted.
	LogTail string `json:"logTail,omitempty"`
}

// Termination is a container termination.
type Termination struct {
	Reason   string `json:"reason"`
	ExitCode int32  `json:"exitCode"`
}

// Call is one request Observe asks its Doer to send.
type Call struct {
	// Try is 1 or 2, Step the step's index.
	Try, Step int
	// Run is the try's {{run}} token, Captures those of earlier steps
	// (a copy). Both are already substituted into Request.
	Run      string
	Captures map[string]string
	Request  BuiltRequest
}

// Response is what a Doer got back. A Doer reads at most
// MaxResponseBytes+1 of the body and reports TransportTooLarge past
// MaxResponseBytes.
type Response struct {
	Status int32
	Header map[string][]string
	Body   []byte
	// Transport is TransportNone (or empty) when a response arrived.
	Transport Transport
	LatencyMs int32
}

// Doer sends one request: the tester's HTTP client, or a fake.
type Doer func(Call) Response

// Observe runs an http criterion the way the tester does: each step's
// request built by BuildRequest and sent through do, the response recorded
// as a bounded StepObservation, the try stopped at the first step
// JudgeStep fails, and a failed try retried once under a fresh {{run}}. A
// step whose request cannot be built (a capture missing, or one that would
// leave the path grammar) is recorded as TransportCaptureMissing and never
// sent. A criterion of another kind has nothing to observe.
func Observe(c Criterion, rt RunTokens, do Doer) CriterionObservation {
	c = normalize(c)
	obs := CriterionObservation{ID: c.ID}
	if c.Kind != KindHTTP {
		return obs
	}
	for try := 1; try <= 2; try++ {
		t, passed := observeTry(c, try, rt.Token(try), do)
		obs.Tries = append(obs.Tries, t)
		if passed {
			break
		}
	}
	return obs
}

func observeTry(c Criterion, try int, run string, do Doer) (TryObservation, bool) {
	t := TryObservation{Run: run}
	caps := map[string]string{}
	for i, st := range c.Steps {
		built, err := BuildRequest(st, run, caps)
		if err != nil {
			t.Steps = append(t.Steps, StepObservation{
				Method: st.Request.Method, Path: truncateUTF8(st.Request.Path, MaxPathBytes),
				TransportError: TransportCaptureMissing,
			})
			return t, false
		}
		resp := do(Call{Try: try, Step: i, Run: run, Captures: maps.Clone(caps), Request: built})
		so := observeResponse(st, built, resp, run, caps)
		t.Steps = append(t.Steps, so)
		if !JudgeStep(st, so, run, caps).Pass {
			return t, false
		}
		for name := range st.Capture {
			caps[name] = so.Captures[name]
		}
	}
	return t, true
}

func observeResponse(st Step, built BuiltRequest, resp Response, run string, caps map[string]string) StepObservation {
	so := StepObservation{
		Method: built.Method, Path: built.Target(), Status: resp.Status,
		TransportError: resp.Transport, LatencyMs: resp.LatencyMs, BodyBytes: int32(min(len(resp.Body), math32)),
	}
	if so.TransportError == "" {
		so.TransportError = TransportNone
	}
	if len(resp.Body) > MaxResponseBytes {
		so.TransportError = TransportTooLarge
	}
	if so.TransportError != TransportNone {
		return so
	}
	so.BodyPrefix = base64.StdEncoding.EncodeToString(resp.Body[:min(len(resp.Body), MaxBodyPrefixBytes)])
	doc, valid := decodeBody(resp.Body)
	so.JSONValid = valid
	so.Pointers = observePointers(st.Expect, doc, valid, resp.Body, run, caps)
	var missing bool
	so.Captures, missing = observeCaptures(st.Capture, doc, valid)
	if missing {
		so.TransportError = TransportCaptureMissing
	}
	so.Headers = observeHeaders(st.Expect.Headers, resp.Header, run, caps)
	return so
}

const math32 = 1<<31 - 1

func observePointers(e Expect, doc any, valid bool, body []byte, run string, caps map[string]string) map[string]string {
	out := map[string]string{}
	if valid {
		for ptr, expected := range e.JSON {
			m, err := parseMatcher(expected)
			if err == nil {
				m, err = m.substitute(run, caps)
			}
			v, ok := resolvePointer(doc, ptr)
			if !ok {
				continue
			}
			if err != nil {
				m = matcher{kind: matchLiteral}
			}
			out[ptr] = m.reduce(v)
		}
	}
	if e.BodyContains != "" {
		if needle, err := Substitute(e.BodyContains, run, caps); err == nil && bytes.Contains(body, []byte(needle)) {
			out[KeyBodyContains] = quote(needle)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
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
			return bounded(quote(truncateUTF8(s, MaxValueBytes/2)))
		}
	case matchOrder:
		if list, ok := v.([]any); ok {
			return bounded(m.project(list))
		}
	}
	raw, err := valueOf(v)
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
			if e, ok = resolvePointer(e, m.by); !ok {
				continue
			}
		}
		for i, want := range m.order {
			if !found[i] && jsonEqual(want.decoded(), e) {
				found[i] = true
				kept = append(kept, e)
				break
			}
		}
	}
	if kept == nil {
		kept = []any{}
	}
	raw, err := valueOf(kept)
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
	v, _ := valueOf(s)
	return v.String()
}

// observeCaptures reads each declared capture; missing reports one that is
// absent, not a string, number or boolean, or over MaxCaptureBytes.
func observeCaptures(capture map[string]string, doc any, valid bool) (map[string]string, bool) {
	if len(capture) == 0 {
		return nil, false
	}
	out := map[string]string{}
	missing := !valid
	for name, ptr := range capture {
		var v any
		var ok bool
		if valid {
			v, ok = resolvePointer(doc, ptr)
		}
		var s string
		switch t := v.(type) {
		case string:
			s = t
		case json.Number:
			s = string(t)
		case bool:
			s = strconv.FormatBool(t)
		default:
			ok = false
		}
		if !ok || len(s) > MaxCaptureBytes {
			missing = true
			continue
		}
		out[name] = s
	}
	if len(out) == 0 {
		out = nil
	}
	return out, missing
}

// observeHeaders records each expected header (case-insensitive), values
// joined by ", "; one over MaxHeaderBytes is reduced as a $contains or an
// exact value would judge it.
func observeHeaders(exp map[string]Value, h map[string][]string, run string, caps map[string]string) map[string]string {
	if len(exp) == 0 {
		return nil
	}
	out := map[string]string{}
	for name, expected := range exp {
		var vals []string
		for k, vs := range h {
			if strings.EqualFold(k, name) {
				vals = append(vals, vs...)
			}
		}
		if vals == nil {
			continue
		}
		got := strings.Join(vals, ", ")
		if len(got) > MaxHeaderBytes {
			got = TooLarge
			if m, err := parseMatcher(expected); err == nil && m.kind == matchContains {
				if m, err = m.substitute(run, caps); err == nil && strings.Contains(strings.Join(vals, ", "), m.contains) {
					got = m.contains
				}
			}
		}
		out[strings.ToLower(name)] = got
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// truncateUTF8 cuts s to at most n bytes on a rune boundary.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// Check holds an observation to the bounds Observe keeps, as the
// controller does to what it reads off a pod's log.
func (o CriterionObservation) Check() error {
	if !idPattern.MatchString(o.ID) {
		return errors.New("observation id is not a criterion id")
	}
	if len(o.Tries) > 2 {
		return errors.New("more than 2 tries")
	}
	for _, t := range o.Tries {
		if len(t.Run) > maxRunBytes || len(t.Steps) > MaxStepsPerHTTP {
			return errors.New("a try is over its bounds")
		}
		for _, s := range t.Steps {
			if err := s.check(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s StepObservation) check() error {
	switch {
	case len(s.Method) > 8 || len(s.Path) > MaxPathBytes:
		return errors.New("a step's request is over its bounds")
	case !transports[s.TransportError]:
		return fmt.Errorf("transport %q is not a transport class", s.TransportError)
	case s.Status < 0 || s.Status > 999 || s.BodyBytes < 0 || s.LatencyMs < 0:
		return errors.New("a step's numbers are out of range")
	case len(s.BodyPrefix) > base64.StdEncoding.EncodedLen(MaxBodyPrefixBytes):
		return errors.New("a step's body prefix is over its bound")
	case len(s.Pointers) > MaxExpectPointers+1 || len(s.Captures) > MaxCaptures || len(s.Headers) > MaxExpectHeaders:
		return errors.New("a step records too many values")
	}
	return s.checkValues()
}

// checkValues holds each recorded key and value to its bound.
func (s StepObservation) checkValues() error {
	for k, v := range s.Pointers {
		if (k != KeyBodyContains && CheckPointer(k) != nil) || len(v) > MaxValueBytes {
			return errors.New("a step's pointer value is over its bounds")
		}
	}
	for k, v := range s.Captures {
		if !captureName.MatchString(k) || len(v) > MaxCaptureBytes {
			return errors.New("a step's capture is over its bounds")
		}
	}
	for k, v := range s.Headers {
		if !headerPattern.MatchString(k) || len(v) > MaxHeaderBytes {
			return errors.New("a step's header is over its bounds")
		}
	}
	return nil
}

// Wire format v1: the tester's stdout.

// WirePrefix starts every wire line; any other log line means nothing.
const WirePrefix = "MAUL-VERIFY: "

// WireVersion is the wire format's version.
const WireVersion = 1

// Wire errors. preview-controller maps ErrNoSummary to
// deadline-no-summary, ErrSummaryMismatch to summary-mismatch and
// ErrWireInvalid to tester-output-invalid.
var (
	ErrWireInvalid     = errors.New("tester output is not valid MAUL-VERIFY v1")
	ErrNoSummary       = errors.New("tester output has no summary")
	ErrSummaryMismatch = errors.New("tester output does not match its summary")
)

// Summary closes the wire output: how many observation lines came before
// it and the sha256 of those lines, each with its "\n".
type Summary struct {
	Count  int    `json:"count"`
	SHA256 string `json:"sha256"`
}

type wireLine struct {
	V           int                   `json:"v"`
	Observation *CriterionObservation `json:"observation,omitempty"`
	Summary     *Summary              `json:"summary,omitempty"`
}

// WireWriter writes observation lines and then the summary.
type WireWriter struct {
	w   io.Writer
	h   hash.Hash
	n   int
	end bool
}

// NewWireWriter writes the wire format to w.
func NewWireWriter(w io.Writer) *WireWriter { return &WireWriter{w: w, h: sha256.New()} }

// Write writes one observation line.
func (w *WireWriter) Write(o CriterionObservation) error {
	if w.end {
		return errors.New("wire: write after close")
	}
	if err := o.Check(); err != nil {
		return err
	}
	line, err := encodeLine(wireLine{V: WireVersion, Observation: &o})
	if err != nil {
		return err
	}
	w.h.Write(line)
	w.n++
	_, err = w.w.Write(line)
	return err
}

// Close writes the summary line and returns the summary, which the tester
// also writes as its termination message (Summary.TerminationMessage).
func (w *WireWriter) Close() (Summary, error) {
	if w.end {
		return Summary{}, errors.New("wire: closed twice")
	}
	w.end = true
	s := Summary{Count: w.n, SHA256: hex.EncodeToString(w.h.Sum(nil))}
	line, err := encodeLine(wireLine{V: WireVersion, Summary: &s})
	if err != nil {
		return Summary{}, err
	}
	_, err = w.w.Write(line)
	return s, err
}

func encodeLine(l wireLine) ([]byte, error) {
	b, err := json.Marshal(l)
	if err != nil {
		return nil, err
	}
	return append(append([]byte(WirePrefix), b...), '\n'), nil
}

// TerminationMessage is the summary as the tester's termination message.
func (s Summary) TerminationMessage() string {
	b, _ := json.Marshal(s)
	return string(b)
}

func sha256Hex(s string) bool {
	_, err := hex.DecodeString(s)
	return len(s) == sha256.Size*2 && err == nil && strings.ToLower(s) == s
}

// ParseSummary reads a termination message.
func ParseSummary(msg string) (Summary, error) {
	var s Summary
	err := decodeStrictJSON([]byte(msg), &s)
	if err != nil || s.Count < 0 || s.Count > MaxCriteria || !sha256Hex(s.SHA256) {
		return Summary{}, ErrWireInvalid
	}
	return s, nil
}

// ParseWire reads a tester's log: only lines starting with WirePrefix, each
// one v1 line, at most MaxCriteria observations each within its bounds,
// then exactly one summary whose count and hash match the lines before it,
// and nothing after. It returns the observations and the summary, to be
// compared with the termination message.
func ParseWire(log []byte) ([]CriterionObservation, Summary, error) {
	var obs []CriterionObservation
	var sum *Summary
	h := sha256.New()
	for _, line := range bytes.SplitAfter(log, []byte("\n")) {
		body, ok := bytes.CutPrefix(bytes.TrimSuffix(line, []byte("\n")), []byte(WirePrefix))
		if !ok {
			continue
		}
		if sum != nil {
			return nil, Summary{}, fmt.Errorf("%w: a line after the summary", ErrWireInvalid)
		}
		var l wireLine
		if err := decodeStrictJSON(body, &l); err != nil || l.V != WireVersion ||
			(l.Observation == nil) == (l.Summary == nil) {
			return nil, Summary{}, ErrWireInvalid
		}
		if l.Summary != nil {
			sum = l.Summary
			continue
		}
		if len(obs) == MaxCriteria || l.Observation.Check() != nil {
			return nil, Summary{}, ErrWireInvalid
		}
		h.Write(append(bytes.TrimSuffix(line, []byte("\n")), '\n'))
		obs = append(obs, *l.Observation)
	}
	if sum == nil {
		return nil, Summary{}, ErrNoSummary
	}
	if sum.Count != len(obs) || sum.SHA256 != hex.EncodeToString(h.Sum(nil)) {
		return nil, Summary{}, ErrSummaryMismatch
	}
	return obs, *sum, nil
}

func decodeStrictJSON(b []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data")
	}
	return nil
}
