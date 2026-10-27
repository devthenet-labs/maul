// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"time"
)

// Kind is what runs a criterion.
type Kind string

const (
	// KindHTTP criteria are requests the tester sends to a previewed
	// component by its Service name, inside the Preview's slot.
	KindHTTP Kind = "http"
	// KindCommand criteria run an argv in a component's image or the
	// repository's agent image. Parsed and shown from slice 1, run from
	// slice 2.
	KindCommand Kind = "command"
	// KindManual criteria are judged by a human and closed by
	// `/maul accept <id>`.
	KindManual Kind = "manual"
	// KindBrowser is reserved for slice 4 and refused until then.
	KindBrowser Kind = "browser"
	// KindAgent is reserved for slice 5 and refused until then.
	KindAgent Kind = "agent"
)

// Criterion is one acceptance criterion of an approved plan. The JSON tags
// are the CRD form (v1alpha1.AcceptanceCriterion mirrors this type field
// for field); the YAML tags are the plan frontmatter's, which the report
// parser decodes strictly.
type Criterion struct {
	// ID names the criterion: ^[a-z][a-z0-9-]{0,31}$, unique in the list.
	ID string `json:"id" yaml:"id"`
	// Kind is http, command or manual.
	Kind Kind `json:"kind" yaml:"kind"`
	// Statement is what the criterion checks, one line of at most
	// MaxStatementChars characters.
	Statement string `json:"statement" yaml:"statement"`
	// Reason says why a manual criterion needs a human, at most
	// MaxReasonChars characters.
	Reason string `json:"reason,omitempty" yaml:"reason,omitempty"`
	// Target is the http criterion's previewed repository key.
	Target string `json:"target,omitempty" yaml:"target,omitempty"`
	// Request, Expect and Capture are the one-step shorthand of an http
	// criterion; Normalize folds them into Steps. Expect is also a command
	// criterion's expectation (exit, stdoutContains, stdoutJSON).
	Request *Request          `json:"request,omitempty" yaml:"request,omitempty"`
	Expect  *Expect           `json:"expect,omitempty" yaml:"expect,omitempty"`
	Capture map[string]string `json:"capture,omitempty" yaml:"capture,omitempty"`
	// Steps are an http criterion's requests, run in order.
	Steps []Step `json:"steps,omitempty" yaml:"steps,omitempty"`
	// In is where a command criterion runs: exactly one of a component or
	// a repository.
	In *Where `json:"in,omitempty" yaml:"in,omitempty"`
	// Argv is a command criterion's argument vector, run without a shell.
	Argv []string `json:"argv,omitempty" yaml:"argv,omitempty"`
	// Timeout bounds a command criterion, a Go duration of at most
	// MaxCommandTimeout.
	Timeout string `json:"timeout,omitempty" yaml:"timeout,omitempty"`
}

// Step is one request of an http criterion and what its response must show.
type Step struct {
	// Name labels the step in reports: ^[a-z][a-z0-9-]{0,31}$, optional.
	Name    string  `json:"name,omitempty" yaml:"name,omitempty"`
	Request Request `json:"request" yaml:"request"`
	Expect  Expect  `json:"expect" yaml:"expect"`
	// Capture maps a capture name to the JSON pointer of the response value
	// later steps use as {{capture.<name>}}.
	Capture map[string]string `json:"capture,omitempty" yaml:"capture,omitempty"`
}

// Request is the request a step sends. Its host is never the criterion's
// to choose: the tester sends it to the target's Service.
type Request struct {
	// Method is GET (the default), HEAD, POST, PUT, PATCH or DELETE.
	Method string `json:"method,omitempty" yaml:"method,omitempty"`
	// Path is the request path with an optional query (CheckPath), sent
	// unchanged.
	Path string `json:"path" yaml:"path"`
	// Headers are extra request headers.
	Headers map[string]string `json:"headers,omitempty" yaml:"headers,omitempty"`
	// JSON is a JSON body, sent as application/json unless a Content-Type
	// header says otherwise; Body a raw one. At most one of the two.
	JSON *Value `json:"json,omitempty" yaml:"json,omitempty"`
	Body string `json:"body,omitempty" yaml:"body,omitempty"`
}

// Expect is what a response (http) or a run (command) must show.
type Expect struct {
	// Status lists the acceptable HTTP statuses; written as one integer or
	// a list, stored as a list. Normalize defaults it to [200].
	Status StatusList `json:"status,omitempty" yaml:"status,omitempty"`
	// JSON maps a JSON pointer into the response body to a literal (equal,
	// numbers by value) or one matcher object: {$present: true},
	// {$absent: true}, {$contains: s}, {$approx: n|[n…], $tolerance: n} or
	// {$order: [v…], $by: <pointer>}.
	JSON map[string]Value `json:"json,omitempty" yaml:"json,omitempty"`
	// Headers maps a response header name to an exact string or
	// {$contains: s}.
	Headers map[string]Value `json:"headers,omitempty" yaml:"headers,omitempty"`
	// BodyContains is text the response body must contain.
	BodyContains string `json:"bodyContains,omitempty" yaml:"bodyContains,omitempty"`
	// Exit, StdoutContains and StdoutJSON are a command criterion's.
	Exit           *int32           `json:"exit,omitempty" yaml:"exit,omitempty"`
	StdoutContains string           `json:"stdoutContains,omitempty" yaml:"stdoutContains,omitempty"`
	StdoutJSON     map[string]Value `json:"stdoutJSON,omitempty" yaml:"stdoutJSON,omitempty"`
}

// Where is where a command criterion runs.
type Where struct {
	Component  string `json:"component,omitempty" yaml:"component,omitempty"`
	Repository string `json:"repository,omitempty" yaml:"repository,omitempty"`
}

// StatusList is a list of HTTP statuses that also decodes from a single
// integer. It always encodes as a list, so a CRD mirror is a plain []int32.
type StatusList []int32

// UnmarshalJSON accepts an integer or a list of integers.
func (s *StatusList) UnmarshalJSON(b []byte) error {
	var one int32
	if err := json.Unmarshal(b, &one); err == nil {
		*s = StatusList{one}
		return nil
	}
	var list []int32
	if err := json.Unmarshal(b, &list); err != nil {
		return errors.New("status must be an integer or a list of integers")
	}
	*s = list
	return nil
}

// UnmarshalYAML accepts an integer or a list of integers, nothing a
// decoder would coerce into one. It uses the function-taking form yaml
// decoders still honour, so this package needs no YAML library.
func (s *StatusList) UnmarshalYAML(unmarshal func(any) error) error {
	var raw any
	if err := unmarshal(&raw); err != nil {
		return err
	}
	items, isList := raw.([]any)
	if !isList {
		items = []any{raw}
	}
	out := make(StatusList, 0, len(items))
	for _, it := range items {
		n, ok := it.(int)
		if !ok || n < math.MinInt32 || n > math.MaxInt32 {
			return errors.New("status must be an integer or a list of integers")
		}
		out = append(out, int32(n))
	}
	*s = out
	return nil
}

// Value is a JSON value held in canonical form: compact, object keys
// sorted, numbers as written. The zero Value is null.
type Value struct{ raw string }

// ParseValue parses one JSON value into its canonical form.
func ParseValue(b []byte) (Value, error) {
	v, err := decodeJSON(b)
	if err != nil {
		return Value{}, err
	}
	return valueOf(v)
}

// valueOf encodes a decoded value canonically.
func valueOf(v any) (Value, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return Value{}, err
	}
	return Value{raw: string(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))}, nil
}

// decodeJSON decodes exactly one JSON value, numbers kept as json.Number.
func decodeJSON(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("trailing data after the JSON value")
	}
	return v, nil
}

// String is the canonical JSON text.
func (v Value) String() string {
	if v.raw == "" {
		return "null"
	}
	return v.raw
}

// decoded is the value, numbers as json.Number.
func (v Value) decoded() any {
	out, err := decodeJSON([]byte(v.String()))
	if err != nil {
		return nil // unreachable: raw is always canonical JSON
	}
	return out
}

// MarshalJSON writes the canonical text.
func (v Value) MarshalJSON() ([]byte, error) { return []byte(v.String()), nil }

// UnmarshalJSON reads any JSON value.
func (v *Value) UnmarshalJSON(b []byte) error {
	out, err := ParseValue(b)
	if err != nil {
		return err
	}
	*v = out
	return nil
}

// UnmarshalYAML reads any YAML value JSON can hold: string-keyed maps,
// lists and scalars, finite numbers. An unquoted timestamp is refused: it
// would be checked as a time the approved plan never showed.
func (v *Value) UnmarshalYAML(unmarshal func(any) error) error {
	var raw any
	if err := unmarshal(&raw); err != nil {
		return err
	}
	if hasTime(raw) {
		return errors.New("value holds an unquoted timestamp; quote it")
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return fmt.Errorf("value is not JSON: %w", err)
	}
	out, err := ParseValue(b)
	if err != nil {
		return err
	}
	*v = out
	return nil
}

func hasTime(v any) bool {
	switch t := v.(type) {
	case time.Time:
		return true
	case []any:
		return slices.ContainsFunc(t, hasTime)
	case map[string]any:
		for _, e := range t {
			if hasTime(e) {
				return true
			}
		}
	}
	return false
}

// Automatable returns the criteria slice runs, in plan order: http from
// slice 1, command from slice 2. Manual criteria are never automatable.
func Automatable(cs []Criterion, slice int) []Criterion {
	var out []Criterion
	for _, c := range cs {
		if (c.Kind == KindHTTP && slice >= 1) || (c.Kind == KindCommand && slice >= 2) {
			out = append(out, c)
		}
	}
	return out
}

// Normalize returns a copy of cs with every http criterion's shorthand
// folded into one step and every default explicit (method GET, status
// [200]). It never fails and never mutates cs; it is idempotent.
func Normalize(cs []Criterion) []Criterion {
	if cs == nil {
		return nil
	}
	out := make([]Criterion, len(cs))
	for i, c := range cs {
		out[i] = normalize(c)
	}
	return out
}

func normalize(c Criterion) Criterion {
	c = cloneCriterion(c)
	if c.Kind != KindHTTP {
		return c
	}
	if len(c.Steps) == 0 && (c.Request != nil || c.Expect != nil || c.Capture != nil) {
		var st Step
		if c.Request != nil {
			st.Request = *c.Request
		}
		if c.Expect != nil {
			st.Expect = *c.Expect
		}
		st.Capture = c.Capture
		c.Steps = []Step{st}
		c.Request, c.Expect, c.Capture = nil, nil, nil
	}
	for i := range c.Steps {
		st := &c.Steps[i]
		if st.Request.Method == "" {
			st.Request.Method = "GET"
		}
		if len(st.Expect.Status) == 0 {
			st.Expect.Status = StatusList{200}
		}
	}
	return c
}

func cloneCriterion(c Criterion) Criterion {
	if c.Request != nil {
		r := cloneRequest(*c.Request)
		c.Request = &r
	}
	if c.Expect != nil {
		e := cloneExpect(*c.Expect)
		c.Expect = &e
	}
	c.Capture = cloneMap(c.Capture)
	if c.Steps != nil {
		steps := make([]Step, len(c.Steps))
		for i, st := range c.Steps {
			steps[i] = Step{Name: st.Name, Request: cloneRequest(st.Request), Expect: cloneExpect(st.Expect),
				Capture: cloneMap(st.Capture)}
		}
		c.Steps = steps
	}
	if c.In != nil {
		in := *c.In
		c.In = &in
	}
	c.Argv = slices.Clone(c.Argv)
	return c
}

func cloneRequest(r Request) Request {
	r.Headers = cloneMap(r.Headers)
	if r.JSON != nil {
		v := *r.JSON
		r.JSON = &v
	}
	return r
}

func cloneExpect(e Expect) Expect {
	e.Status = slices.Clone(e.Status)
	e.JSON = cloneMap(e.JSON)
	e.Headers = cloneMap(e.Headers)
	e.StdoutJSON = cloneMap(e.StdoutJSON)
	if e.Exit != nil {
		x := *e.Exit
		e.Exit = &x
	}
	return e
}

// cloneMap copies m, an empty map becoming nil so equal criteria compare
// and encode alike.
func cloneMap[V any](m map[string]V) map[string]V {
	if len(m) == 0 {
		return nil
	}
	return maps.Clone(m)
}
