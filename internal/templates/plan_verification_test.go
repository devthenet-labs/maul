// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
)

// TestPlanVerificationRoundTrip: what a plan Job is handed as
// MAUL_PLAN_VERIFICATION decodes to exactly the components the prompt would
// state, one line each, bounded; no verification, or none with a component
// to name, is handed nothing.
func TestPlanVerificationRoundTrip(t *testing.T) {
	in := &PlanVerification{Components: []PlanVerifyComponent{
		{Key: "web", Path: "/", Readiness: "/healthz"},
		{Key: "api\n", Path: "/api", Readiness: ""},
		{Key: "", Path: "/nobody"},
		{Key: "render", Path: "/render\u202e", Readiness: "/render/ready"},
	}}
	got, err := DecodePlanVerification(EncodePlanVerification(in))
	if err != nil {
		t.Fatal(err)
	}
	want := &PlanVerification{Components: []PlanVerifyComponent{
		{Key: "web", Path: "/", Readiness: "/healthz"},
		{Key: "api", Path: "/api"},
		{Key: "render", Path: "/render", Readiness: "/render/ready"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
	for _, none := range []*PlanVerification{nil, {}, {Components: []PlanVerifyComponent{{Path: "/"}}}} {
		if s := EncodePlanVerification(none); s != "" {
			t.Errorf("EncodePlanVerification(%+v) = %q, want none", none, s)
		}
	}
	if got, err := DecodePlanVerification(""); err != nil || got != nil {
		t.Errorf("none decodes to %+v, %v", got, err)
	}
}

// TestDecodePlanVerificationRefuses: the handoff is the controller's own
// encoding, so anything else is refused rather than half-read.
func TestDecodePlanVerificationRefuses(t *testing.T) {
	many := `{"components":[` + strings.Repeat(`{"key":"a","path":"/"},`, planVerificationMaxComponents) +
		`{"key":"a","path":"/"}]}`
	for name, raw := range map[string]string{
		"broken JSON":     "{",
		"unknown field":   `{"components":[{"key":"web","path":"/"}],"limits":1}`,
		"no component":    `{"components":[]}`,
		"keyless":         `{"components":[{"key":"","path":"/"}]}`,
		"pathless":        `{"components":[{"key":"web","path":""}]}`,
		"two lines":       `{"components":[{"key":"web\nweb","path":"/"}]}`,
		"too many":        many,
		"trailing data":   `{"components":[{"key":"web","path":"/"}]} {}`,
		"not an object":   `[]`,
		"null components": `{"components":null}`,
	} {
		if got, err := DecodePlanVerification(raw); err == nil {
			t.Errorf("%s: decoded %+v, want refused", name, got)
		}
	}
}

// TestPlanVerificationRoundTripProperty: for any verification, the encoding
// decodes without error to what the prompt states of it (each field one
// line, keyless or pathless components dropped, at most
// planVerificationMaxComponents), re-encodes byte for byte, and encodes to
// nothing exactly when nothing is left to state.
func TestPlanVerificationRoundTripProperty(t *testing.T) {
	alphabet := []string{"a", "web", "/", "/api", "-", " ", "\n", "\r", "\t", "\x00", "\u202e", "\u200b", "é",
		"`", "\"", "\\", "{", "}"}
	text := func(r *rand.Rand) string {
		var b strings.Builder
		for range r.Intn(6) {
			b.WriteString(alphabet[r.Intn(len(alphabet))])
		}
		return b.String()
	}
	cfg := &quick.Config{
		MaxCount: 2000,
		Rand:     rand.New(rand.NewSource(20261027)),
		Values: func(args []reflect.Value, r *rand.Rand) {
			var v *PlanVerification
			if r.Intn(10) != 0 {
				v = &PlanVerification{}
				for range r.Intn(planVerificationMaxComponents + 4) {
					v.Components = append(v.Components,
						PlanVerifyComponent{Key: text(r), Path: text(r), Readiness: text(r)})
				}
			}
			args[0] = reflect.ValueOf(v)
		},
	}
	prop := func(v *PlanVerification) bool {
		enc := EncodePlanVerification(v)
		got, err := DecodePlanVerification(enc)
		if err != nil {
			t.Logf("%+v encodes to %q, which does not decode: %v", v, enc, err)
			return false
		}
		var want *PlanVerification
		if v != nil {
			for _, c := range v.Components {
				c = PlanVerifyComponent{Key: oneLine(c.Key), Path: oneLine(c.Path), Readiness: oneLine(c.Readiness)}
				if c.Key == "" || c.Path == "" {
					continue
				}
				if want == nil {
					want = &PlanVerification{}
				}
				if len(want.Components) < planVerificationMaxComponents {
					want.Components = append(want.Components, c)
				}
			}
		}
		return reflect.DeepEqual(got, want) && EncodePlanVerification(got) == enc && (enc == "") == (want == nil)
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}
