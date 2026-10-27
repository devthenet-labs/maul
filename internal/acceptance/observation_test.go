// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
)

func observationsOf(t *testing.T, g genScenario) []CriterionObservation {
	t.Helper()
	return []CriterionObservation{
		Observe(g.c, g.rt, (&server{c: g.c}).do),
		Observe(g.c, g.rt, (&server{c: g.c, failTries: map[int]bool{1: true}}).do),
	}
}

// TestWireRoundTrip: what the tester writes, the controller reads back
// exactly, summary checked.
func TestWireRoundTrip(t *testing.T) {
	prop := func(g genScenario) bool {
		obs := observationsOf(t, g)
		var out bytes.Buffer
		w := mustWireWriter(t, &out)
		for _, o := range obs {
			if err := w.Write(o); err != nil {
				return false
			}
		}
		sum, err := w.Close()
		if err != nil || sum.Count != len(obs) {
			return false
		}
		// Unrelated lines (a panic trace on stderr, merged into the pod's
		// log) are ignored.
		log := "starting\n" + out.String() + "goroutine 1 [running]:\n"
		got, gotSum, err := ParseWire([]byte(log))
		if err != nil {
			t.Logf("ParseWire: %v\n%s", err, log)
			return false
		}
		term, err := ParseSummary(sum.TerminationMessage())
		return reflect.DeepEqual(got, obs) && gotSum.Equal(sum) && err == nil && term.Equal(sum)
	}
	if err := quick.Check(prop, quickConfig(100)); err != nil {
		t.Error(err)
	}
}

// TestWireLineDoesNotEscapeHTML: a value the application chose costs its
// JSON escaping on the wire and nothing more; '<', '>' and '&' are not
// inflated sixfold to unicode escapes, and the line reads back exactly.
func TestWireLineDoesNotEscapeHTML(t *testing.T) {
	value := `"` + strings.Repeat("<>&", 80) + `"`
	o := CriterionObservation{ID: "home", Tries: []TryObservation{{Run: "v1a1t1-a1b2c3", Steps: []StepObservation{{
		Method: "GET", Path: "/", Status: 500, TransportError: TransportNone, JSONValid: true,
		Pointers: map[string]string{"/a": value},
	}}}}}
	var out bytes.Buffer
	w := mustWireWriter(t, &out)
	if err := w.Write(o); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
	line, _, _ := strings.Cut(out.String(), "\n")
	if !strings.Contains(line, strings.Repeat("<>&", 80)) || len(line) > len(WirePrefix)+len(value)+300 {
		t.Errorf("wire line is %d bytes for a %d-byte value: %s", len(line), len(value), line)
	}
	got, _, err := ParseWire(out.Bytes())
	if err != nil || len(got) != 1 || !reflect.DeepEqual(got[0], o) {
		t.Errorf("ParseWire = %+v, %v; want the observation back", got, err)
	}
}

func TestWireRefusals(t *testing.T) {
	o := CriterionObservation{ID: "a", Tries: []TryObservation{{Run: "v1a1t1-abcdef", Steps: []StepObservation{{
		Method: "GET", Path: "/", Status: 200, TransportError: TransportNone,
	}}}}}
	write := func(obs ...CriterionObservation) string {
		var b bytes.Buffer
		w := mustWireWriter(t, &b)
		for _, o := range obs {
			if err := w.Write(o); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := w.Close(); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	good := write(o, CriterionObservation{ID: "b", Tries: o.Tries})
	lines := strings.SplitAfter(good, "\n")
	cases := map[string]struct {
		log  string
		want error
	}{
		"no summary":    {strings.Join(lines[:2], ""), ErrNoSummary},
		"empty":         {"", ErrNoSummary},
		"dropped line":  {lines[0] + lines[2], ErrSummaryMismatch},
		"reordered":     {lines[1] + lines[0] + lines[2], ErrSummaryMismatch},
		"edited line":   {strings.Replace(good, `"a"`, `"c"`, 1), ErrSummaryMismatch},
		"after summary": {good + lines[0], ErrWireInvalid},
		"two summaries": {good + lines[2], ErrWireInvalid},
		"other version": {strings.Replace(good, `{"v":1,`, `{"v":2,`, 1), ErrWireInvalid},
		"unknown field": {WirePrefix + `{"v":1,"observation":{"id":"a","colour":1}}` + "\n", ErrWireInvalid},
		"not json":      {WirePrefix + "{\n", ErrWireInvalid},
		"both": {
			WirePrefix + `{"v":1,"observation":{"id":"a"},"summary":{"count":0,"sha256":""}}` + "\n", ErrWireInvalid,
		},
		"neither": {WirePrefix + `{"v":1}` + "\n", ErrWireInvalid},
		"marked too large": {
			strings.Replace(good, `{"id":"a",`, `{"id":"a","tooLarge":true,`, 1), ErrWireInvalid,
		},
		"over cap": {strings.Repeat(lines[0], MaxCriteria+1) + lines[2], ErrWireInvalid},
		"bad observation": {
			strings.Replace(good, `"method":"GET"`, `"method":"`+strings.Repeat("G", 600)+`"`, 1), ErrWireInvalid,
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := ParseWire([]byte(c.log)); !errors.Is(err, c.want) {
				t.Errorf("ParseWire = %v, want %v", err, c.want)
			}
		})
	}
	if err := mustWireWriter(t, &bytes.Buffer{}).Write(CriterionObservation{ID: "a", TooLarge: true}); err == nil {
		t.Error("the wire writer wrote an observation marked too large")
	}
	for _, bad := range []string{"", "count=1", `{"count":1}`, `{"count":-1,"sha256":"` + strings.Repeat("0", 64) + `"}`} {
		if _, err := ParseSummary(bad); err == nil {
			t.Errorf("ParseSummary(%q) accepted", bad)
		}
	}
}

// TestObserveBounds: the observation the tester records is bounded however
// large or hostile the response.
func TestObserveBounds(t *testing.T) {
	v := func(src string) Value { return mustValue(t, src) }
	c := Criterion{ID: "big", Kind: KindHTTP, Statement: "s", Target: "api", Steps: []Step{{
		Request: Request{Path: "/"},
		Expect: Expect{Status: StatusList{200}, JSON: map[string]Value{
			"/exact":    v(`"x"`),
			"/present":  v(`{"$present":true}`),
			"/contains": v(`{"$contains":"needle"}`),
			"/miss":     v(`{"$contains":"needle"}`),
			"/list":     v(`{"$order":["b","a"],"$by":"/id"}`),
		}, Headers: map[string]Value{"X-Big": v(`"x"`)}, BodyContains: "tail-marker"},
		Capture: map[string]string{"big": "/exact", "obj": "/present"},
	}}}
	huge := strings.Repeat("y", 4000)
	list := make([]any, 0, 600)
	for i := range 200 {
		list = append(list, map[string]any{"id": fmt.Sprintf("n%d", i)}, map[string]any{"id": "b"}, map[string]any{"id": "a"})
	}
	body, _ := json.Marshal(map[string]any{
		"exact": huge, "present": map[string]any{"deep": huge}, "contains": huge + "needle" + huge,
		"miss": huge, "list": list, "pad": huge + "tail-marker",
	})
	do := func(Call) Response {
		return Response{Status: 200, Body: body, Header: map[string][]string{"x-big": {huge, huge}}}
	}
	o := Observe(c, RunTokens{Round: 1, Attempt: 1, Run: "abcdef"}, do)
	st := o.Tries[0].Steps[0]
	for k, p := range st.Pointers {
		if len(p) > MaxValueBytes {
			t.Errorf("pointer %q holds %d bytes", k, len(p))
		}
	}
	if st.Pointers["/exact"] != TooLarge || st.Pointers["/present"] != TooLarge {
		t.Errorf("large values recorded as %q / %q", st.Pointers["/exact"], st.Pointers["/present"])
	}
	if st.Pointers["/contains"] != `"needle"` {
		t.Errorf("$contains reduced to %q", st.Pointers["/contains"])
	}
	if p := st.Pointers["/miss"]; p == TooLarge || strings.Contains(p, "needle") || !json.Valid([]byte(p)) {
		t.Errorf("$contains miss reduced to %q", p)
	}
	if st.Pointers["/list"] != `["b","a"]` {
		t.Errorf("$order reduced to %q", st.Pointers["/list"])
	}
	if st.Pointers[KeyBodyContains] != bodyContainsProof("tail-marker") {
		t.Errorf("bodyContains recorded as %q", st.Pointers[KeyBodyContains])
	}
	checkBoundedStep(t, st, len(body))
	if err := o.Check(); err != nil {
		t.Errorf("Observe produced an observation its own bounds refuse: %v", err)
	}
}

// TestObserveHeaderContainsBound: a header over MaxHeaderBytes that holds a
// $contains needle longer than MaxHeaderBytes is recorded within the
// observation's bound, so one such step cannot make the whole run's output
// fail its own check (and the tester stop before its summary).
func TestObserveHeaderContainsBound(t *testing.T) {
	needle := strings.Repeat("é", 150) // 300 bytes, 150 characters
	c := Criterion{ID: "hdr", Kind: KindHTTP, Statement: "s", Target: "api", Steps: []Step{{
		Request: Request{Path: "/"},
		Expect: Expect{Status: StatusList{200},
			Headers: map[string]Value{"X-T": mustValue(t, `{"$contains":"`+needle+`"}`)}},
	}}}
	do := func(Call) Response {
		return Response{Status: 200, Body: []byte(`{}`), Header: map[string][]string{"X-T": {"a" + needle}}}
	}
	o := Observe(c, RunTokens{Round: 1, Attempt: 1, Run: "abcdef"}, do)
	if err := o.Check(); err != nil {
		t.Fatalf("Observe produced an observation its own bounds refuse: %v", err)
	}
	if got := o.Tries[0].Steps[0].Headers["x-t"]; got != TooLarge {
		t.Errorf("header recorded as %q, want %q", got, TooLarge)
	}
}

func checkBoundedStep(t *testing.T, st StepObservation, bodyLen int) {
	t.Helper()
	if len(st.Headers["x-big"]) > MaxHeaderBytes {
		t.Errorf("header holds %d bytes", len(st.Headers["x-big"]))
	}
	if _, ok := st.Captures["big"]; ok {
		t.Error("an oversized capture was recorded")
	}
	if _, ok := st.Captures["obj"]; ok {
		t.Error("an object was captured")
	}
	if st.TransportError != TransportCaptureMissing {
		t.Errorf("transport = %q, want capture-missing", st.TransportError)
	}
	prefix, err := base64.StdEncoding.DecodeString(st.BodyPrefix)
	if err != nil || len(prefix) > MaxBodyPrefixBytes {
		t.Errorf("body prefix %d bytes, %v", len(prefix), err)
	}
	if st.BodyBytes != int32(bodyLen) || !st.JSONValid {
		t.Errorf("body bytes %d valid %v", st.BodyBytes, st.JSONValid)
	}
}

func TestObserveCaptures(t *testing.T) {
	c := Criterion{ID: "cap", Kind: KindHTTP, Statement: "s", Target: "api", Steps: []Step{{
		Request: Request{Method: "POST", Path: "/"}, Expect: Expect{Status: StatusList{201}},
		Capture: map[string]string{"s": "/s", "n": "/n", "b": "/b"},
	}, {
		Request: Request{Path: "/{{capture.s}}/{{capture.n}}/{{capture.b}}"},
	}}}
	var paths []string
	do := func(call Call) Response {
		paths = append(paths, call.Request.Target())
		if call.Step == 0 {
			return Response{Status: 201, Body: []byte(`{"s":"id-1","n":42,"b":true}`)}
		}
		return Response{Status: 200, Body: []byte(`{}`)}
	}
	o := Observe(c, RunTokens{Round: 1, Attempt: 1, Run: "abcdef"}, do)
	if !reflect.DeepEqual(paths, []string{"/", "/id-1/42/true"}) {
		t.Errorf("paths = %v", paths)
	}
	if v := Judge(c, o, RunTokens{Round: 1, Attempt: 1, Run: "abcdef"}); v.Outcome != OutcomePass {
		t.Errorf("verdict %+v", v)
	}
	// A body that is not JSON captures nothing, and the step fails.
	bad := func(call Call) Response { return Response{Status: 201, Body: []byte(`<html>`)} }
	o = Observe(c, RunTokens{Round: 1, Attempt: 1, Run: "abcdef"}, bad)
	first := o.Tries[0].Steps[0]
	if len(o.Tries) != 2 || first.TransportError != TransportCaptureMissing || first.JSONValid {
		t.Errorf("non-JSON capture observed as %s", mustJSON(o))
	}
	if len(o.Tries[0].Steps) != 1 {
		t.Error("the try went on past a failed step")
	}
}
