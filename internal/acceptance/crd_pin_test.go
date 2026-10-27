// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/quick"

	"sigs.k8s.io/yaml"

	maulv1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// api/ never imports internal/, so v1alpha1.AcceptanceCriterion mirrors
// Criterion by hand. These tests hold the two shapes together: same JSON
// fields and tag options, a lossless seeded round trip through the CRD form
// (including the API server's re-encoding), and the CRD schema's bounds
// agreeing with the grammar's.

// shorthand are the acceptance-only fields: the one-step http shorthand,
// which Normalize folds into Steps before a criterion is ever stored.
var shorthand = map[string]bool{"Criterion.request": true, "Criterion.capture": true}

// optionsDiffer are the deliberate tag-option differences. The CRD requires
// a request's method because only normalised criteria are stored (Normalize
// makes GET explicit), and omitempty on a struct field (Step.expect) encodes
// nothing either way.
var optionsDiffer = map[string]bool{"Request.method": true, "Step.expect": true}

// mirrors pairs each acceptance type with its CRD twin.
var mirrors = []struct {
	name string
	acc  reflect.Type
	crd  reflect.Type
}{
	{"Criterion", reflect.TypeFor[Criterion](), reflect.TypeFor[maulv1.AcceptanceCriterion]()},
	{"Step", reflect.TypeFor[Step](), reflect.TypeFor[maulv1.AcceptanceStep]()},
	{"Request", reflect.TypeFor[Request](), reflect.TypeFor[maulv1.AcceptanceRequest]()},
	{"Expect", reflect.TypeFor[Expect](), reflect.TypeFor[maulv1.AcceptanceExpect]()},
	{"Where", reflect.TypeFor[Where](), reflect.TypeFor[maulv1.AcceptanceWhere]()},
}

// jsonFields maps a struct's JSON field names to their tag options.
func jsonFields(t reflect.Type) map[string]string {
	out := map[string]string{}
	for f := range t.Fields() {
		name, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
		out[name] = opts
	}
	return out
}

func TestCRDMirrorFieldsMatch(t *testing.T) {
	for _, m := range mirrors {
		acc, crd := jsonFields(m.acc), jsonFields(m.crd)
		for name, opts := range acc {
			if shorthand[m.name+"."+name] {
				if _, ok := crd[name]; ok {
					t.Errorf("%s.%s is the shorthand and must not be stored in the CRD", m.name, name)
				}
				continue
			}
			copts, ok := crd[name]
			if !ok {
				t.Errorf("%s.%s has no CRD field", m.name, name)
				continue
			}
			if opts != copts && !optionsDiffer[m.name+"."+name] {
				t.Errorf("%s.%s json options differ: acceptance %q, CRD %q", m.name, name, opts, copts)
			}
		}
		for name := range crd {
			if _, ok := acc[name]; !ok {
				t.Errorf("CRD %s.%s has no acceptance field", m.name, name)
			}
		}
	}
}

func TestCRDKindsMatchValidate(t *testing.T) {
	crdKinds := []maulv1.AcceptanceKind{maulv1.AcceptanceHTTP, maulv1.AcceptanceCommand, maulv1.AcceptanceManual}
	for _, k := range crdKinds {
		if !slices.Contains([]Kind{KindHTTP, KindCommand, KindManual}, Kind(k)) {
			t.Errorf("CRD kind %q is not an acceptance kind", k)
		}
	}
	for _, k := range []Kind{KindBrowser, KindAgent, "other"} {
		if err := Validate([]Criterion{{ID: "x", Kind: k, Statement: "s"}}); err == nil {
			t.Errorf("Validate accepted kind %q, which the CRD enum refuses", k)
		}
	}
}

// viaCRD re-encodes a criteria list the way the API server stores it: into
// the CRD type (unknown fields refused) and through generic JSON, object
// keys sorted and insignificant space dropped.
func viaCRD(cs []Criterion) ([]Criterion, error) {
	b, err := json.Marshal(cs)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var stored []maulv1.AcceptanceCriterion
	if err := dec.Decode(&stored); err != nil {
		return nil, fmt.Errorf("does not fit the CRD type: %w", err)
	}
	if b, err = json.Marshal(stored); err != nil {
		return nil, err
	}
	var generic any
	if err := json.Unmarshal(b, &generic); err != nil {
		return nil, err
	}
	if b, err = json.Marshal(generic); err != nil {
		return nil, err
	}
	var back []Criterion
	if err := json.Unmarshal(b, &back); err != nil {
		return nil, err
	}
	return back, nil
}

func TestCRDRoundTripKeepsDigest(t *testing.T) {
	prop := func(g genCriteria) bool {
		norm := Normalize(g.cs)
		if err := Validate(norm); err != nil {
			t.Logf("generated list is invalid: %v", err)
			return false
		}
		back, err := viaCRD(norm)
		if err != nil {
			t.Log(err)
			return false
		}
		if Digest(back) != Digest(g.cs) {
			t.Logf("digest changed through the CRD form:\nwant %s\n got %s", Digest(g.cs), Digest(back))
			return false
		}
		return Validate(back) == nil
	}
	if err := quick.Check(prop, quickConfig(300)); err != nil {
		t.Error(err)
	}
}

// The shorthand is not stored: a list that still carries it does not fit
// the CRD type, so intent-controller must Normalize before it stores.
func TestCRDRefusesTheShorthand(t *testing.T) {
	cs := []Criterion{{ID: "home", Kind: KindHTTP, Statement: "s", Target: "web",
		Request: &Request{Path: "/"}}}
	if _, err := viaCRD(cs); err == nil {
		t.Fatal("the CRD type took the unnormalised shorthand")
	}
	n := Normalize(cs)
	if n[0].Request != nil || n[0].Expect != nil || n[0].Capture != nil || len(n[0].Steps) != 1 {
		t.Fatalf("Normalize left the shorthand: %+v", n[0])
	}
	if _, err := viaCRD(n); err != nil {
		t.Fatal(err)
	}
}

// The CRD cannot say "json or body, not both" in CEL, so Validate must.
func TestRequestJSONAndBodyAreExclusive(t *testing.T) {
	v, err := ParseValue([]byte(`{"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	c := Criterion{ID: "both", Kind: KindHTTP, Statement: "s", Target: "web", Steps: []Step{{
		Request: Request{Method: "POST", Path: "/", JSON: &v, Body: "x"}}}}
	if err := Validate([]Criterion{c}); err == nil {
		t.Fatal("Validate accepted a request with both json and body, which the CRD cannot refuse")
	}
}

// crdProps walks the generated PreviewVerification CRD to the criterion
// item schema's properties.
func crdProps(t *testing.T) map[string]any {
	t.Helper()
	b, err := os.ReadFile("../../deploy/kustomize/base/crds/maul.run_previewverifications.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd map[string]any
	if err := yaml.Unmarshal(b, &crd); err != nil {
		t.Fatal(err)
	}
	v := crd["spec"].(map[string]any)["versions"].([]any)[0].(map[string]any)
	root := sub(v, "schema", "openAPIV3Schema", "properties", "spec", "properties", "criteria", "items")
	return sub(root, "properties")
}

func sub(m map[string]any, keys ...string) map[string]any {
	for _, k := range keys {
		m = m[k].(map[string]any)
	}
	return m
}

func TestCRDSchemaBoundsMatchGrammar(t *testing.T) {
	p := crdProps(t)
	step := sub(p, "steps", "items", "properties")
	req := sub(step, "request", "properties")
	exp := sub(step, "expect", "properties")
	in := sub(p, "in", "properties")
	cases := []struct {
		what string
		got  any
		want int
	}{
		{"statement maxLength", p["statement"].(map[string]any)["maxLength"], MaxStatementChars},
		{"reason maxLength", p["reason"].(map[string]any)["maxLength"], MaxReasonChars},
		{"target maxLength", p["target"].(map[string]any)["maxLength"], maulv1.MaxRepositoryKeyLength},
		{"in.component maxLength", in["component"].(map[string]any)["maxLength"], maulv1.MaxRepositoryKeyLength},
		{"in.repository maxLength", in["repository"].(map[string]any)["maxLength"], maulv1.MaxRepositoryKeyLength},
		{"steps maxItems", p["steps"].(map[string]any)["maxItems"], MaxStepsPerHTTP},
		{"argv maxItems", p["argv"].(map[string]any)["maxItems"], MaxArgs},
		{"argv item maxLength", sub(p, "argv", "items")["maxLength"], MaxArgChars},
		{"headers maxProperties", req["headers"].(map[string]any)["maxProperties"], MaxHeaders},
		{"path maxLength", req["path"].(map[string]any)["maxLength"], MaxPathBytes},
		{"expect.json maxProperties", exp["json"].(map[string]any)["maxProperties"], MaxExpectPointers},
		{"expect.headers maxProperties", exp["headers"].(map[string]any)["maxProperties"], MaxExpectHeaders},
		{"expect.stdoutJSON maxProperties", exp["stdoutJSON"].(map[string]any)["maxProperties"], MaxExpectPointers},
		{"bodyContains maxLength", exp["bodyContains"].(map[string]any)["maxLength"], MaxContainsChars},
		{"stdoutContains maxLength", exp["stdoutContains"].(map[string]any)["maxLength"], MaxContainsChars},
		{"capture maxProperties", step["capture"].(map[string]any)["maxProperties"], MaxCaptures},
	}
	for _, c := range cases {
		if fmt.Sprint(c.got) != fmt.Sprint(c.want) {
			t.Errorf("CRD %s is %v, the grammar's bound is %d", c.what, c.got, c.want)
		}
	}
	enum := p["kind"].(map[string]any)["enum"].([]any)
	kinds := make([]string, 0, len(enum))
	for _, k := range enum {
		kinds = append(kinds, k.(string))
	}
	slices.Sort(kinds)
	if want := []string{"command", "http", "manual"}; !slices.Equal(kinds, want) {
		t.Errorf("CRD kinds are %v, want %v", kinds, want)
	}
	if got := p["id"].(map[string]any)["pattern"]; got != idPattern.String() {
		t.Errorf("CRD id pattern %v differs from the grammar's %s", got, idPattern)
	}
}
