// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package checks

import (
	"bytes"
	"encoding/json"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/devthenet-labs/maul/internal/acceptance"
)

// HTTPCheck is requests to one previewed component, by its key, run in
// order: slice 1's steps (acceptance.Step), whose expectations may also use
// the matchers below.
type HTTPCheck struct {
	// Target is the previewed component's key.
	Target string            `json:"target"`
	Steps  []acceptance.Step `json:"steps"`
}

var (
	stepNamePattern   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	headerNamePattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
)

func validateHTTP(h HTTPCheck, p Preview) *Refusal {
	if len(h.Target) > acceptance.MaxKeyLength || !acceptance.KeyPattern.MatchString(h.Target) {
		return refuse(RuleGrammar, "target %q is not a component key", h.Target)
	}
	if len(p.Components) > 0 && !slices.Contains(p.Components, h.Target) {
		return refuse(RuleOffPreview, "target %q is not one of the preview's components %v", h.Target, p.Components)
	}
	if len(h.Steps) == 0 {
		return refuse(RuleGrammar, "an http check has at least one step")
	}
	captured := map[string]bool{}
	names := map[string]bool{}
	content := false
	for i, st := range h.Steps {
		if st.Name != "" {
			if !stepNamePattern.MatchString(st.Name) || names[st.Name] {
				return refuse(RuleGrammar, "step %d: name %q is not a unique ^[a-z][a-z0-9-]{0,31}$", i+1, st.Name)
			}
			names[st.Name] = true
		}
		if r := validateHTTPExpect(st.Expect, captured); r != nil {
			r.Reason = "step " + strconv.Itoa(i+1) + ": " + r.Reason
			return r
		}
		if err := acceptance.ValidateStepShape(st, captured); err != nil {
			return refuse(RuleGrammar, "step %d: %v", i+1, err)
		}
		content = content || contentJSON(st.Expect.JSON) || len(st.Expect.Headers) > 0 || st.Expect.BodyContains != ""
	}
	if !content {
		return refuse(RuleNotSubstantive, "no step asserts on content (json, headers or bodyContains); a status "+
			"alone only says the call worked")
	}
	for i, st := range h.Steps {
		if r := runScoped("step "+strconv.Itoa(i+1)+" json", st.Expect.JSON); r != nil {
			return r
		}
	}
	return nil
}

func validateHTTPExpect(e acceptance.Expect, captured map[string]bool) *Refusal {
	if e.Exit != nil || e.StdoutContains != "" || e.StdoutJSON != nil {
		return refuse(RuleGrammar, "an http expect has status, json, headers and bodyContains only")
	}
	if len(e.Status) > acceptance.MaxStatuses {
		return refuse(RuleGrammar, "more than %d statuses", acceptance.MaxStatuses)
	}
	for _, s := range e.Status {
		if s < 100 || s > 599 {
			return refuse(RuleGrammar, "status %d is not an HTTP status", s)
		}
	}
	if r := validateExpectedJSON("json", e.JSON, captured); r != nil {
		return r
	}
	if r := validateExpectedHeaders(e.Headers, captured); r != nil {
		return r
	}
	if r := checkLine("bodyContains", e.BodyContains); r != nil {
		return r
	}
	if err := acceptance.ValidateMatcher(stringValue(e.BodyContains), captured); err != nil {
		return refuse(RuleGrammar, "bodyContains: %v", err)
	}
	return nil
}

func validateExpectedHeaders(hs map[string]acceptance.Value, captured map[string]bool) *Refusal {
	if len(hs) > acceptance.MaxExpectHeaders {
		return refuse(RuleGrammar, "more than %d expected headers", acceptance.MaxExpectHeaders)
	}
	seen := map[string]bool{}
	for name, v := range hs {
		if !headerNamePattern.MatchString(name) || seen[strings.ToLower(name)] {
			return refuse(RuleGrammar, "expected header name %q is not allowed or is named twice", name)
		}
		seen[strings.ToLower(name)] = true
		if presence(v) {
			continue
		}
		if err := acceptance.ValidateHeaderMatcher(v, captured); err != nil {
			return refuse(RuleGrammar, "expected header %s: %v; or {$present: true} or {$absent: true}", name, err)
		}
	}
	return nil
}

// validateExpectedJSON checks an expected-JSON map (an http step's json, an
// exec check's stdoutJSON): at most MaxExpectPointers pointers, each to a
// literal, a slice-1 matcher, {$range: [lo, hi]} or {$includes: v}.
func validateExpectedJSON(field string, m map[string]acceptance.Value, captured map[string]bool) *Refusal {
	if len(m) > acceptance.MaxExpectPointers {
		return refuse(RuleGrammar, "%s: more than %d pointers", field, acceptance.MaxExpectPointers)
	}
	for ptr, v := range m {
		if err := acceptance.CheckPointer(ptr); err != nil {
			return refuse(RuleGrammar, "%s: %v", field, err)
		}
		if r := validateJSONMatcher(v, captured); r != nil {
			r.Reason = field + " " + strconv.Quote(ptr) + ": " + r.Reason
			return r
		}
	}
	return nil
}

func validateJSONMatcher(v acceptance.Value, captured map[string]bool) *Refusal {
	obj, _ := v.Decoded().(map[string]any)
	switch {
	case obj != nil && obj["$range"] != nil:
		if len(obj) != 1 {
			return refuse(RuleGrammar, "$range stands alone")
		}
		if _, _, ok := rangeOf(obj["$range"]); !ok {
			return refuse(RuleGrammar, "$range takes [lo, hi], two finite numbers with lo <= hi")
		}
		return nil
	case obj != nil && obj["$includes"] != nil:
		if len(obj) != 1 {
			return refuse(RuleGrammar, "$includes stands alone")
		}
		inner, err := acceptance.ValueOf(obj["$includes"])
		if err != nil {
			return refuse(RuleGrammar, "$includes: %v", err)
		}
		// A literal: no "$" key at any depth, bounded, placeholders known.
		if err := acceptance.ValidateMatcher(inner, captured); err != nil || hasDollarKeyDeep(obj["$includes"]) {
			return refuse(RuleGrammar, "$includes takes one literal value (an object matches as a subset), "+
				"holding no matcher")
		}
		return nil
	}
	if err := acceptance.ValidateMatcher(v, captured); err != nil {
		return refuse(RuleGrammar, "%v; or {$range: [lo, hi]} or {$includes: v}", err)
	}
	return nil
}

// hasDollarKeyDeep reports a "$" object key at any depth (a "$" string
// value is data, not a key).
func hasDollarKeyDeep(v any) bool {
	switch t := v.(type) {
	case map[string]any:
		for k, e := range t {
			if strings.HasPrefix(k, "$") || hasDollarKeyDeep(e) {
				return true
			}
		}
	case []any:
		return slices.ContainsFunc(t, hasDollarKeyDeep)
	}
	return false
}

func presence(v acceptance.Value) bool {
	obj, ok := v.Decoded().(map[string]any)
	if !ok || len(obj) != 1 {
		return false
	}
	for _, k := range []string{"$present", "$absent"} {
		if b, ok := obj[k].(bool); ok && b {
			return true
		}
	}
	return false
}

func rangeOf(v any) (lo, hi float64, ok bool) {
	list, isList := v.([]any)
	if !isList || len(list) != 2 {
		return 0, 0, false
	}
	lo, okLo := number(list[0])
	hi, okHi := number(list[1])
	return lo, hi, okLo && okHi && lo <= hi
}

func number(v any) (float64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(string(n), 64)
	return f, err == nil && !math.IsInf(f, 0) && !math.IsNaN(f)
}

func stringValue(s string) acceptance.Value {
	v, _ := acceptance.ValueOf(s)
	return v
}

// Matching.

// MatchJSON judges an observed JSON value (canonical JSON text,
// acceptance.TooLarge, or absent) against one expected value Validate
// admits, under the run token and the captures: {$range: [lo, hi]} a
// number within it, inclusive; {$includes: v} a list holding an element
// equal to v, an object v matching any element that has its keys with
// matching values (recursively); anything else as slice 1 judges it.
func MatchJSON(expected acceptance.Value, run string, caps map[string]string, raw string, present bool) bool {
	obj, _ := expected.Decoded().(map[string]any)
	switch {
	case obj != nil && obj["$range"] != nil:
		lo, hi, ok := rangeOf(obj["$range"])
		n, isNum := number(observed(raw, present))
		return ok && isNum && n >= lo && n <= hi
	case obj != nil && obj["$includes"] != nil:
		want, err := substituteAny(obj["$includes"], run, caps)
		list, isList := observed(raw, present).([]any)
		return err == nil && isList && slices.ContainsFunc(list, func(e any) bool { return subset(want, e) })
	}
	return acceptance.MatchValue(expected, run, caps, raw, present)
}

// MatchHeader judges an observed response header against one expected
// header Validate admits: {$present: true}, {$absent: true}, or as slice 1
// judges it (an exact string or {$contains: s}).
func MatchHeader(expected acceptance.Value, run string, caps map[string]string, raw string, present bool) bool {
	if presence(expected) {
		obj, _ := expected.Decoded().(map[string]any)
		return (obj["$present"] != nil) == present
	}
	return acceptance.MatchHeaderValue(expected, run, caps, raw, present)
}

// observed decodes an observed value, nil when absent, too large or not
// JSON.
func observed(raw string, present bool) any {
	if !present || raw == acceptance.TooLarge {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil || dec.More() {
		return nil
	}
	return v
}

// subset: an object matches when every key of want is in got with a
// matching value; anything else is equal as slice 1 compares values.
func subset(want, got any) bool {
	if w, ok := want.(map[string]any); ok {
		g, ok := got.(map[string]any)
		if !ok {
			return false
		}
		for k, wv := range w {
			gv, has := g[k]
			if !has || !subset(wv, gv) {
				return false
			}
		}
		return true
	}
	a, errA := acceptance.ValueOf(want)
	b, errB := acceptance.ValueOf(got)
	return errA == nil && errB == nil && acceptance.JSONEqual(a, b)
}

// substituteAny substitutes the run token and captures into every string
// of a decoded value, object keys left alone.
func substituteAny(v any, run string, caps map[string]string) (any, error) {
	switch t := v.(type) {
	case string:
		return acceptance.Substitute(t, run, caps)
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			var err error
			if out[i], err = substituteAny(e, run, caps); err != nil {
				return nil, err
			}
		}
		return out, nil
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			var err error
			if out[k], err = substituteAny(e, run, caps); err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	return v, nil
}
