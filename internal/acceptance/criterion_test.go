// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// specExample is the spec's plan frontmatter example (SPEC §3.1), decoded the
// way the report parser decodes a plan: strict, unknown keys refused.
const specExample = `
acceptance:
  - id: battle-leaderboard
    kind: http
    statement: a battle's leaderboard ranks its patterns by votes
    target: api
    steps:
      - name: battle
        request: {method: POST, path: /api/battles, json: {name: "maul-{{run}}"}}
        expect: {status: 201}
        capture: {battle: /id}
      - request: {method: GET, path: "/api/battles/{{capture.battle}}/leaderboard"}
        expect:
          status: 200
          json: {"": {$order: ["{{capture.battle}}", "x"], $by: /id}}
  - id: kick-onsets
    kind: command
    statement: a four-on-the-floor kick renders onsets at 0/500/1000/1500 ms
    in: {component: render}
    argv: [/beatbattle-render, onsets, --bpm, "120", --kick, "1,5,9,13"]
    expect: {exit: 0, stdoutJSON: {/kick: {$approx: [0, 500, 1000, 1500], $tolerance: 5}}}
  - id: sounds-good
    kind: manual
    statement: it sounds good
    reason: only a human can judge how it sounds
  - id: health
    kind: http
    statement: the api answers its health check
    target: api
    request: {path: /healthz}
    expect: {status: [200, 204], headers: {Content-Type: {$contains: json}}, bodyContains: ok}
`

func decodeYAML(t *testing.T, src string) []Criterion {
	t.Helper()
	var doc struct {
		Acceptance []Criterion `yaml:"acceptance"`
	}
	dec := yaml.NewDecoder(strings.NewReader(src))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return doc.Acceptance
}

func TestDecodeSpecExample(t *testing.T) {
	cs := decodeYAML(t, specExample)
	if len(cs) != 4 {
		t.Fatalf("got %d criteria, want 4", len(cs))
	}
	if err := Validate(cs); err != nil {
		t.Fatalf("Validate(spec example) = %v", err)
	}
	leader := cs[0]
	if leader.Kind != KindHTTP || len(leader.Steps) != 2 || leader.Steps[0].Capture["battle"] != "/id" {
		t.Errorf("leaderboard criterion decoded as %+v", leader)
	}
	if got := leader.Steps[0].Request.JSON.String(); got != `{"name":"maul-{{run}}"}` {
		t.Errorf("request json = %s", got)
	}
	if got := leader.Steps[1].Expect.JSON[""].String(); got != `{"$by":"/id","$order":["{{capture.battle}}","x"]}` {
		t.Errorf("order matcher = %s", got)
	}
	kick := cs[1]
	if kick.In == nil || kick.In.Component != "render" || kick.Expect == nil || kick.Expect.Exit == nil ||
		*kick.Expect.Exit != 0 || len(kick.Argv) != 6 {
		t.Errorf("command criterion decoded as %+v", kick)
	}
	health := cs[3]
	if health.Request == nil || health.Expect == nil || !reflect.DeepEqual(health.Expect.Status, StatusList{200, 204}) {
		t.Errorf("shorthand criterion decoded as %+v", health)
	}
}

func TestDecodeRefusesUnknownKeys(t *testing.T) {
	for name, src := range map[string]string{
		"criterion": "acceptance: [{id: a, kind: manual, statement: s, colour: red}]",
		"request":   "acceptance: [{id: a, kind: http, statement: s, target: t, request: {path: /, host: x}}]",
		"expect":    "acceptance: [{id: a, kind: http, statement: s, target: t, request: {path: /}, expect: {code: 1}}]",
	} {
		t.Run(name, func(t *testing.T) {
			var doc struct {
				Acceptance []Criterion `yaml:"acceptance"`
			}
			dec := yaml.NewDecoder(strings.NewReader(src))
			dec.KnownFields(true)
			if err := dec.Decode(&doc); err == nil {
				t.Errorf("decoded %q without error", src)
			}
		})
	}
}

func TestStatusListScalarOrList(t *testing.T) {
	for src, want := range map[string]StatusList{
		`201`:        {201},
		`[200, 204]`: {200, 204},
	} {
		var y StatusList
		if err := yaml.Unmarshal([]byte(src), &y); err != nil || !reflect.DeepEqual(y, want) {
			t.Errorf("yaml %s = %v, %v; want %v", src, y, err, want)
		}
		var j StatusList
		if err := json.Unmarshal([]byte(src), &j); err != nil || !reflect.DeepEqual(j, want) {
			t.Errorf("json %s = %v, %v; want %v", src, j, err, want)
		}
		// Always marshalled as a list, so a CRD mirror can be a plain []int32.
		out, _ := json.Marshal(j)
		if out[0] != '[' {
			t.Errorf("marshal %v = %s, want a list", j, out)
		}
	}
	for _, bad := range []string{`"201"`, `[200, "x"]`, `2.5`, `{}`, `[[200]]`} {
		var j StatusList
		if err := json.Unmarshal([]byte(bad), &j); err == nil {
			t.Errorf("json %s accepted as %v", bad, j)
		}
		var y StatusList
		if err := yaml.Unmarshal([]byte(bad), &y); err == nil {
			t.Errorf("yaml %s accepted as %v", bad, y)
		}
	}
}

func TestValueCanonical(t *testing.T) {
	var a, b Value
	if err := json.Unmarshal([]byte(`{ "b": 1, "a": [true, null, "x"] }`), &a); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal([]byte("{a: [true, null, x], b: 1}"), &b); err != nil {
		t.Fatal(err)
	}
	want := `{"a":[true,null,"x"],"b":1}`
	if a.String() != want || b.String() != want {
		t.Errorf("canonical forms %s / %s, want %s", a, b, want)
	}
	out, err := json.Marshal(map[string]Value{"/x": a})
	if err != nil || !bytes.Equal(out, []byte(`{"/x":`+want+`}`)) {
		t.Errorf("marshal = %s, %v", out, err)
	}
	var zero Value
	if out, _ := json.Marshal(zero); string(out) != "null" {
		t.Errorf("zero Value marshals as %s, want null", out)
	}
	// A quoted date is the text it shows.
	var ts Value
	if err := yaml.Unmarshal([]byte(`"2026-10-26"`), &ts); err != nil || ts.String() != `"2026-10-26"` {
		t.Errorf("quoted date = %s, %v", ts, err)
	}
	// An unquoted one would be checked as a timestamp the approver never
	// saw written, so it is refused.
	for _, bad := range []string{"{1: a}", ".nan", ".inf", "2026-10-26", "{a: [2026-10-26T10:00:00Z]}"} {
		var v Value
		if err := yaml.Unmarshal([]byte(bad), &v); err == nil {
			t.Errorf("yaml %q accepted as %s", bad, v)
		}
	}
}

// TestJSONRoundTrip holds the JSON form (the CRD mirror's wire form) to the
// decoded value: decode, encode, decode again gives the same criteria and
// the same bytes.
func TestJSONRoundTrip(t *testing.T) {
	cs := decodeYAML(t, specExample)
	first, err := json.Marshal(cs)
	if err != nil {
		t.Fatal(err)
	}
	var again []Criterion
	if err := json.Unmarshal(first, &again); err != nil {
		t.Fatal(err)
	}
	second, err := json.Marshal(again)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("round trip changed the bytes:\n%s\n%s", first, second)
	}
	if Digest(cs) != Digest(again) {
		t.Error("round trip changed the digest")
	}
}

func TestAutomatable(t *testing.T) {
	cs := decodeYAML(t, specExample)
	ids := func(cs []Criterion) []string {
		out := make([]string, 0, len(cs))
		for _, c := range cs {
			out = append(out, c.ID)
		}
		return out
	}
	if got := ids(Automatable(cs, 1)); !reflect.DeepEqual(got, []string{"battle-leaderboard", "health"}) {
		t.Errorf("slice 1 = %v", got)
	}
	want := []string{"battle-leaderboard", "kick-onsets", "health"}
	if got := ids(Automatable(cs, 2)); !reflect.DeepEqual(got, want) {
		t.Errorf("slice 2 = %v", got)
	}
	if got := Automatable(nil, 1); got != nil {
		t.Errorf("Automatable(nil) = %v", got)
	}
}

func TestNormalize(t *testing.T) {
	cs := decodeYAML(t, specExample)
	n := Normalize(cs)
	health := n[3]
	if health.Request != nil || health.Expect != nil || health.Capture != nil || len(health.Steps) != 1 {
		t.Fatalf("shorthand not folded into steps: %+v", health)
	}
	if health.Steps[0].Request.Method != "GET" {
		t.Errorf("default method = %q, want GET", health.Steps[0].Request.Method)
	}
	if !reflect.DeepEqual(n[0].Steps[1].Expect.Status, StatusList{200}) {
		t.Errorf("status kept as %v", n[0].Steps[1].Expect.Status)
	}
	// A step with no status expectation gets the explicit default.
	c := Criterion{ID: "a", Kind: KindHTTP, Statement: "s", Target: "t", Request: &Request{Path: "/"}}
	if got := Normalize([]Criterion{c})[0].Steps[0].Expect.Status; !reflect.DeepEqual(got, StatusList{200}) {
		t.Errorf("default status = %v, want [200]", got)
	}
	// The command criterion keeps its own expect.
	if n[1].Expect == nil || n[1].Steps != nil {
		t.Errorf("command criterion normalised to %+v", n[1])
	}
	// Normalize never mutates its input.
	if cs[3].Request == nil {
		t.Error("Normalize mutated its input")
	}
	if !reflect.DeepEqual(Normalize(n), n) {
		t.Error("Normalize is not idempotent")
	}
}
