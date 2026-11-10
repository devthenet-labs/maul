// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package httpstep

import (
	"maps"
	"slices"
)

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
	// a list, stored as a list. NormalizeSteps defaults it to [200].
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

// NormalizeSteps returns a copy of steps with every default explicit
// (method GET, status [200]), as acceptance.Normalize makes an http
// criterion's. It never fails and never mutates steps; it is idempotent. A
// nil list stays nil.
func NormalizeSteps(steps []Step) []Step {
	if steps == nil {
		return nil
	}
	out := make([]Step, len(steps))
	for i, st := range steps {
		st = st.Clone()
		if st.Request.Method == "" {
			st.Request.Method = "GET"
		}
		if len(st.Expect.Status) == 0 {
			st.Expect.Status = StatusList{200}
		}
		out[i] = st
	}
	return out
}

// Clone is a deep copy of st, an empty map becoming nil.
func (st Step) Clone() Step {
	return Step{Name: st.Name, Request: st.Request.Clone(), Expect: st.Expect.Clone(), Capture: cloneMap(st.Capture)}
}

// Clone is a deep copy of r, an empty map becoming nil.
func (r Request) Clone() Request {
	r.Headers = cloneMap(r.Headers)
	if r.JSON != nil {
		v := *r.JSON
		r.JSON = &v
	}
	return r
}

// Clone is a deep copy of e, an empty map becoming nil.
func (e Expect) Clone() Expect {
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
