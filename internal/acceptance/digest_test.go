// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import (
	"encoding/json"
	"regexp"
	"testing"
	"testing/quick"
)

var digestForm = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func TestDigestForm(t *testing.T) {
	cs := decodeYAML(t, specExample)
	d := Digest(cs)
	if !digestForm.MatchString(d) {
		t.Fatalf("Digest = %q", d)
	}
	if Digest(cs) != d {
		t.Error("Digest is not deterministic")
	}
	// The empty list has a digest of its own, distinct from any criterion's.
	if Digest(nil) != Digest([]Criterion{}) || Digest(nil) == d {
		t.Error("empty list digest")
	}
}

// TestDigestCoversEverything: the digest is over the FULL list (manual
// criteria included, fix 4), in plan order, and over every field.
func TestDigestCoversEverything(t *testing.T) {
	cs := decodeYAML(t, specExample)
	base := Digest(cs)
	mutations := map[string]func(cs []Criterion){
		"manual reason":  func(cs []Criterion) { cs[2].Reason += "!" },
		"manual removed": func(cs []Criterion) { copy(cs[2:], cs[3:]) },
		"order":          func(cs []Criterion) { cs[0], cs[3] = cs[3], cs[0] },
		"statement":      func(cs []Criterion) { cs[0].Statement += "." },
		"path":           func(cs []Criterion) { cs[0].Steps[1].Request.Path += "x" },
		"status":         func(cs []Criterion) { cs[0].Steps[0].Expect.Status = StatusList{200} },
		"capture":        func(cs []Criterion) { cs[0].Steps[0].Capture = map[string]string{"battle": "/uuid"} },
		"argv":           func(cs []Criterion) { cs[1].Argv[1] = "offsets" },
		"target":         func(cs []Criterion) { cs[3].Target = "beatbattle-web" },
		"expect value": func(cs []Criterion) {
			v, _ := ParseValue([]byte(`{"$order":["x","{{capture.battle}}"],"$by":"/id"}`))
			cs[0].Steps[1].Expect.JSON[""] = v
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			c := decodeYAML(t, specExample)
			mutate(c)
			if name == "manual removed" {
				c = c[:3]
			}
			if Digest(c) == base {
				t.Errorf("digest unchanged by %s", name)
			}
		})
	}
}

// TestDigestShorthandInvariant: a bare request/expect pair and its one-step
// form, written in any key order or spacing, are the same criteria.
func TestDigestShorthandInvariant(t *testing.T) {
	short := decodeYAML(t, `
acceptance:
  - id: health
    kind: http
    statement: up
    target: api
    request: {path: /healthz}
    expect: {json: {/ok: true, /a: {b: 1, a: 2}}}
`)
	long := decodeYAML(t, `
acceptance:
  - kind: http
    id: health
    statement: up
    target: api
    steps:
      - expect:
          json:
            /a: {a: 2, b: 1}
            /ok: true
          status: [200]
        request: {method: GET, path: /healthz}
`)
	if Digest(short) != Digest(long) {
		t.Errorf("shorthand %s != long form %s", Digest(short), Digest(long))
	}
}

// TestDigestSurvivesJSON is the property the CRD copy relies on: criteria
// stored as JSON (Intent.status.acceptance, PreviewVerification spec) and
// read back digest the same.
func TestDigestSurvivesJSON(t *testing.T) {
	prop := func(g genCriteria) bool {
		raw, err := json.Marshal(g.cs)
		if err != nil {
			return false
		}
		var back []Criterion
		if err := json.Unmarshal(raw, &back); err != nil {
			return false
		}
		return Digest(back) == Digest(g.cs) && Digest(Normalize(g.cs)) == Digest(g.cs)
	}
	if err := quick.Check(prop, quickConfig(300)); err != nil {
		t.Error(err)
	}
}
