// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"testing/quick"

	"github.com/devthenet-labs/maul/internal/httpstep"
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
	if health.Request == nil || health.Expect == nil ||
		!reflect.DeepEqual(health.Expect.Status, httpstep.StatusList{200, 204}) {
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
	if !reflect.DeepEqual(n[0].Steps[1].Expect.Status, httpstep.StatusList{200}) {
		t.Errorf("status kept as %v", n[0].Steps[1].Expect.Status)
	}
	// A step with no status expectation gets the explicit default.
	c := Criterion{ID: "a", Kind: KindHTTP, Statement: "s", Target: "t", Request: &httpstep.Request{Path: "/"}}
	if got := Normalize([]Criterion{c})[0].Steps[0].Expect.Status; !reflect.DeepEqual(got, httpstep.StatusList{200}) {
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

// TestNormalizeStepsAgrees holds httpstep.NormalizeSteps, which the saved
// checks and the tools relay call on bare steps, to what Normalize makes of
// the same steps as an http criterion's, for every generated criterion:
// written as steps, and as the one-step shorthand.
func TestNormalizeStepsAgrees(t *testing.T) {
	prop := func(g genHTTPCriterion) bool {
		steps := Normalize([]Criterion{g.c})[0].Steps
		raw := g.c.Steps
		if raw == nil {
			raw = []httpstep.Step{{Capture: g.c.Capture}}
			if g.c.Request != nil {
				raw[0].Request = *g.c.Request
			}
			if g.c.Expect != nil {
				raw[0].Expect = *g.c.Expect
			}
		}
		viaCriterion := Normalize([]Criterion{{Kind: KindHTTP, Steps: raw}})[0].Steps
		return reflect.DeepEqual(httpstep.NormalizeSteps(raw), viaCriterion) &&
			reflect.DeepEqual(httpstep.NormalizeSteps(raw), steps)
	}
	if err := quick.Check(prop, quickConfig(500)); err != nil {
		t.Error(err)
	}
	for _, steps := range [][]httpstep.Step{nil, {}} {
		viaCriterion := Normalize([]Criterion{{Kind: KindHTTP, Steps: steps}})[0].Steps
		if got := httpstep.NormalizeSteps(steps); !reflect.DeepEqual(got, viaCriterion) {
			t.Errorf("NormalizeSteps(%#v) = %#v, Normalize gives %#v", steps, got, viaCriterion)
		}
	}
}
