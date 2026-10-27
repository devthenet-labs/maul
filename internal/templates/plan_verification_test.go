// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"math/rand"
	"reflect"
	"slices"
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

// TestEncodePlanVerificationCases pins the encoding by hand: the exact bytes
// of a plain verification (readiness left out when empty), and the bound,
// past which components are dropped in order.
func TestEncodePlanVerificationCases(t *testing.T) {
	plain := &PlanVerification{Components: []PlanVerifyComponent{
		{Key: "web", Path: "/", Readiness: "/healthz"}, {Key: "api", Path: "/api"}}}
	if got, want := EncodePlanVerification(plain),
		`{"components":[{"key":"web","path":"/","readiness":"/healthz"},{"key":"api","path":"/api"}]}`; got != want {
		t.Errorf("EncodePlanVerification = %s, want %s", got, want)
	}
	var many PlanVerification
	for i := range 10 {
		many.Components = append(many.Components, PlanVerifyComponent{Key: string(rune('a' + i)), Path: "/"})
	}
	got, err := DecodePlanVerification(EncodePlanVerification(&many))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Components) != 8 || got.Components[0].Key != "a" || got.Components[7].Key != "h" {
		t.Errorf("ten components encode to %+v, want the first eight", got.Components)
	}
}

// TestPlanPromptVerificationNormalised: the prompt renders a verification
// through the same normaliser as the handoff, so a verification with no
// component to name renders no section at all, rather than one listing
// nothing, and a component without a key or a path is never listed.
func TestPlanPromptVerificationNormalised(t *testing.T) {
	render := func(v *PlanVerification) string {
		t.Helper()
		got, err := RenderPlanPrompt(PlanPrompt{
			IssuePath: "/workspace/input/issue.md", ReportPath: "/workspace/reports/plan.md",
			Intent: testPlanRequest, BuildMaxTurns: 150, BuildTokenBudget: 800000, Limits: testPlanLimits,
			Verification: v,
		})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	base := render(nil)
	for _, v := range []*PlanVerification{{}, {Components: []PlanVerifyComponent{{Key: "web"}, {Path: "/"}}}} {
		if got := render(v); got != base {
			t.Errorf("a verification with nothing to name (%+v) changed the prompt", v)
		}
	}
	got := render(&PlanVerification{Components: []PlanVerifyComponent{{Key: "web", Path: "/"}, {Key: "ghost"}}})
	if !strings.Contains(got, "- `web`: served under `/`") || strings.Contains(got, "ghost") {
		t.Errorf("prompt lists %q:\n%s", "ghost", got)
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

// TestPlanVerificationRoundTripProperty: for any verification, by invariants
// independent of how the encoder normalises: the encoding always decodes; a
// decoded verification holds 1 to planVerificationMaxComponents components,
// each with a key and a path and every field its own one-line form, each
// one an input component's; encoding is idempotent (decode then encode gives
// the same bytes) and so is the round trip; and the encoding is empty
// exactly when no input component has a key and a path.
func TestPlanVerificationRoundTripProperty(t *testing.T) {
	cfg := &quick.Config{
		MaxCount: 2000,
		Rand:     rand.New(rand.NewSource(20261027)),
		Values: func(args []reflect.Value, r *rand.Rand) {
			args[0] = reflect.ValueOf(randomPlanVerification(r))
		},
	}
	prop := func(v *PlanVerification) bool {
		enc := EncodePlanVerification(v)
		got, err := DecodePlanVerification(enc)
		if err != nil {
			t.Logf("%+v encodes to %q, which does not decode: %v", v, enc, err)
			return false
		}
		if (enc == "") != !hasStateable(v) || (enc == "") != (got == nil) {
			t.Logf("%+v encodes to %q", v, enc)
			return false
		}
		if got == nil {
			return true
		}
		if !wellFormedFrom(got, v) {
			t.Logf("%+v decodes to %+v", v, got)
			return false
		}
		again, err := DecodePlanVerification(EncodePlanVerification(got))
		return EncodePlanVerification(got) == enc && err == nil && reflect.DeepEqual(again, got)
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}

// randomPlanVerification is nil one time in ten, else up to
// planVerificationMaxComponents+3 components of short text dense in what
// breaks a line or hides: line breaks, control and format characters, and
// JSON's own syntax.
func randomPlanVerification(r *rand.Rand) *PlanVerification {
	if r.Intn(10) == 0 {
		return nil
	}
	alphabet := []string{"a", "web", "/", "/api", "-", " ", "\n", "\r", "\t", "\x00", "\u202e", "\u200b", "é",
		"`", "\"", "\\", "{", "}"}
	text := func() string {
		var b strings.Builder
		for range r.Intn(6) {
			b.WriteString(alphabet[r.Intn(len(alphabet))])
		}
		return b.String()
	}
	v := &PlanVerification{}
	for range r.Intn(planVerificationMaxComponents + 4) {
		v.Components = append(v.Components, PlanVerifyComponent{Key: text(), Path: text(), Readiness: text()})
	}
	return v
}

// hasStateable reports whether any of v's components has a key and a path
// that are not empty on one line.
func hasStateable(v *PlanVerification) bool {
	return v != nil && slices.ContainsFunc(v.Components, func(c PlanVerifyComponent) bool {
		return oneLine(c.Key) != "" && oneLine(c.Path) != ""
	})
}

// wellFormedFrom reports whether got holds 1 to
// planVerificationMaxComponents components, each with a key and a path,
// every field already in its one-line form, and each the one-line form of
// one of in's components.
func wellFormedFrom(got, in *PlanVerification) bool {
	if n := len(got.Components); n == 0 || n > planVerificationMaxComponents {
		return false
	}
	for _, c := range got.Components {
		if c.Key == "" || c.Path == "" || c.Key != oneLine(c.Key) || c.Path != oneLine(c.Path) ||
			c.Readiness != oneLine(c.Readiness) {
			return false
		}
		if !slices.ContainsFunc(in.Components, func(i PlanVerifyComponent) bool {
			return oneLine(i.Key) == c.Key && oneLine(i.Path) == c.Path && oneLine(i.Readiness) == c.Readiness
		}) {
			return false
		}
	}
	return true
}
