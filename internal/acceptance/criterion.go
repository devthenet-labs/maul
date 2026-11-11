// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import (
	"maps"
	"slices"

	"github.com/devthenet-labs/maul/internal/httpstep"
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
	Request *httpstep.Request `json:"request,omitempty" yaml:"request,omitempty"`
	Expect  *httpstep.Expect  `json:"expect,omitempty" yaml:"expect,omitempty"`
	Capture map[string]string `json:"capture,omitempty" yaml:"capture,omitempty"`
	// Steps are an http criterion's requests, run in order.
	Steps []httpstep.Step `json:"steps,omitempty" yaml:"steps,omitempty"`
	// In is where a command criterion runs: exactly one of a component or
	// a repository.
	In *Where `json:"in,omitempty" yaml:"in,omitempty"`
	// Argv is a command criterion's argument vector, run without a shell.
	Argv []string `json:"argv,omitempty" yaml:"argv,omitempty"`
	// Timeout bounds a command criterion, a Go duration of at most
	// MaxCommandTimeout.
	Timeout string `json:"timeout,omitempty" yaml:"timeout,omitempty"`
}

// Where is where a command criterion runs.
type Where struct {
	Component  string `json:"component,omitempty" yaml:"component,omitempty"`
	Repository string `json:"repository,omitempty" yaml:"repository,omitempty"`
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
		var st httpstep.Step
		if c.Request != nil {
			st.Request = *c.Request
		}
		if c.Expect != nil {
			st.Expect = *c.Expect
		}
		st.Capture = c.Capture
		c.Steps = []httpstep.Step{st}
		c.Request, c.Expect, c.Capture = nil, nil, nil
	}
	c.Steps = httpstep.NormalizeSteps(c.Steps)
	return c
}

func cloneCriterion(c Criterion) Criterion {
	if c.Request != nil {
		r := c.Request.Clone()
		c.Request = &r
	}
	if c.Expect != nil {
		e := c.Expect.Clone()
		c.Expect = &e
	}
	// An empty map becomes nil, as Step.Clone makes it, so equal criteria
	// compare and encode alike.
	c.Capture = maps.Clone(c.Capture)
	if len(c.Capture) == 0 {
		c.Capture = nil
	}
	if c.Steps != nil {
		steps := make([]httpstep.Step, len(c.Steps))
		for i, st := range c.Steps {
			steps[i] = st.Clone()
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
