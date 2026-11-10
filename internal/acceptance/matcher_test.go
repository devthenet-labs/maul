// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import (
	"encoding/json"
	"errors"
	"strconv"

	"github.com/devthenet-labs/maul/internal/httpstep"
)

// The matcher engine lives in httpstep and keeps its parse private. The
// generators here (satisfy, mutateValue) read the matcher objects they built
// through this test-only reading of the same shapes; it never judges: Judge
// and Observe use httpstep's own parse, so a misreading here could only make
// a satisfied observation fail its property, never pass one.

type matchKind int

const (
	matchLiteral matchKind = iota
	matchPresent
	matchAbsent
	matchContains
	matchApprox
	matchOrder
)

type matcher struct {
	kind       matchKind
	literal    httpstep.Value
	contains   string
	approx     []float64
	approxList bool
	tolerance  float64
	order      []httpstep.Value
	by         string
}

// parseMatcher reads one expected value as httpstep's matcher grammar does,
// for the well-formed values the generators produce.
func parseMatcher(v httpstep.Value) (matcher, error) {
	obj, isObj := v.Decoded().(map[string]any)
	if !isObj {
		return matcher{kind: matchLiteral, literal: v}, nil
	}
	_, approx := obj["$approx"]
	_, order := obj["$order"]
	switch {
	case obj["$present"] == true && len(obj) == 1:
		return matcher{kind: matchPresent}, nil
	case obj["$absent"] == true && len(obj) == 1:
		return matcher{kind: matchAbsent}, nil
	case len(obj) == 1 && obj["$contains"] != nil:
		s, ok := obj["$contains"].(string)
		if !ok {
			return matcher{}, errors.New("$contains takes text")
		}
		return matcher{kind: matchContains, contains: s}, nil
	case approx:
		return testApprox(obj)
	case order:
		return testOrder(obj)
	}
	for k := range obj {
		if len(k) > 0 && k[0] == '$' {
			return matcher{}, errors.New("not a matcher the generators build")
		}
	}
	return matcher{kind: matchLiteral, literal: v}, nil
}

func testApprox(obj map[string]any) (matcher, error) {
	m := matcher{kind: matchApprox}
	tol, ok := testNumber(obj["$tolerance"])
	if !ok {
		return matcher{}, errors.New("$tolerance takes a number")
	}
	m.tolerance = tol
	list, isList := obj["$approx"].([]any)
	if !isList {
		list = []any{obj["$approx"]}
	}
	m.approxList = isList
	for _, e := range list {
		n, ok := testNumber(e)
		if !ok {
			return matcher{}, errors.New("$approx takes numbers")
		}
		m.approx = append(m.approx, n)
	}
	return m, nil
}

func testOrder(obj map[string]any) (matcher, error) {
	list, ok := obj["$order"].([]any)
	if !ok {
		return matcher{}, errors.New("$order takes a list")
	}
	m := matcher{kind: matchOrder}
	if by, ok := obj["$by"].(string); ok {
		m.by = by
	}
	for _, e := range list {
		v, err := httpstep.ValueOf(e)
		if err != nil {
			return matcher{}, err
		}
		m.order = append(m.order, v)
	}
	return m, nil
}

func testNumber(v any) (float64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(string(n), 64)
	return f, err == nil
}
