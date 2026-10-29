// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package playwright

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/checks"
)

var update = flag.Bool("update", false, "rewrite golden files")

// testToken is the run token every golden is compiled under.
const testToken = "v1a1t1-abc123"

// goldenCases are one browser check per assertion kind, together using
// every step and every locator form. Each passes checks.Validate.
var goldenCases = []struct {
	name, check string
}{
	{"visible", `{"steps":[
		{"goto":"/battles"},
		{"expect":{"assert":"visible","locator":{"role":"heading","name":"Battles"}}}]}`},
	{"legible", `{"steps":[
		{"goto":"/"},
		{"expect":{"assert":"legible","locator":{"role":"link","name":"Back to the sequencer"}}}]}`},
	{"clickable", `{"steps":[
		{"goto":"/battles/new"},
		{"click":{"role":"button","name":"Start battle"}},
		{"waitForURL":"/battles/*"},
		{"expect":{"assert":"clickable","locator":{"testid":"vote-a"}}}]}`},
	{"text", `{"steps":[
		{"goto":"/battles"},
		{"fill":{"locator":{"label":"Name"},"text":"Battle {{run}}"}},
		{"press":{"locator":{"label":"Name"},"key":"Enter"}},
		{"waitForURL":"/battles/*"},
		{"expect":{"assert":"text","locator":{"role":"heading","nth":0},"text":{"equals":"Battle {{run}}"}}},
		{"expect":{"assert":"text","locator":{"text":"Score","within":{"css":"article.side"}},
			"text":{"contains":"points"}}},
		{"expect":{"assert":"text","locator":{"testid":"score-a"},"text":{"matches":"^[0-9]+$"}}}]}`},
	{"count", `{"steps":[
		{"goto":"/battles?page=1"},
		{"click":{"role":"button","name":"Add {{run}}"}},
		{"press":{"key":"Control+a"}},
		{"expect":{"assert":"count","locator":{"role":"row","hasText":"{{run}}"},"count":2}}]}`},
	{"url", `{"steps":[
		{"goto":"/battles"},
		{"click":{"role":"link","name":"New battle","nth":-1}},
		{"expect":{"assert":"url","url":"/battles/new"}},
		{"expect":{"assert":"visible","locator":{"label":"Pattern A"}}}]}`},
	{"boxes", `{"steps":[
		{"goto":"/battles/{{run}}"},
		{"expect":{"assert":"left-of","locator":{"css":".side","nth":0},"other":{"css":".side","nth":1},
			"tolerance":2}},
		{"expect":{"assert":"above","locator":{"role":"heading","name":"Battle"},"other":{"testid":"versus"}}},
		{"expect":{"assert":"no-overlap","locator":{"testid":"side-a"},"other":{"testid":"side-b"}}},
		{"expect":{"assert":"equal-size","locator":{"testid":"side-a"},"other":{"testid":"side-b"},
			"tolerance":1.5}},
		{"expect":{"assert":"size","locator":{"testid":"side-a"},"width":400,"tolerance":40}},
		{"expect":{"assert":"size","locator":{"testid":"side-b"},"height":220.5}}]}`},
	{"scroll", `{"steps":[
		{"goto":"/sequencer"},
		{"select":{"label":"Tempo","option":"120 bpm"}},
		{"scroll":{"locator":{"role":"grid","name":"Steps"},"dx":0,"dy":40}},
		{"expect":{"assert":"aligned","locator":{"role":"row","name":"Kick {{run}}"},
			"other":{"role":"row","name":"Snare {{run}}"},"axis":"x","tolerance":1}}]}`},
}

func parseBrowser(t *testing.T, s string) checks.BrowserCheck {
	t.Helper()
	var b checks.BrowserCheck
	dec := json.NewDecoder(strings.NewReader(s))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		t.Fatalf("test check does not parse: %v", err)
	}
	return b
}

// TestGoldenCompile pins the compiled spec of one check per assertion.
func TestGoldenCompile(t *testing.T) {
	for _, tc := range goldenCases {
		t.Run(tc.name, func(t *testing.T) {
			b := parseBrowser(t, tc.check)
			c := checks.Check{ID: "golden", Outcome: "golden", Form: checks.FormBrowser, Browser: &b}
			if err := checks.Validate(c, checks.Preview{}); err != nil {
				t.Fatalf("golden input is not a valid check: %v", err)
			}
			got, err := Compile(b, Options{RunToken: testToken})
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			again, _ := Compile(b, Options{RunToken: testToken})
			if !bytes.Equal(got, again) {
				t.Fatal("Compile is not deterministic")
			}
			golden(t, tc.name+".spec.ts", got)
		})
	}
}

// TestGoldenSupport pins the support module every compiled spec imports.
func TestGoldenSupport(t *testing.T) {
	golden(t, SupportFile, Support())
}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -update)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from the golden (run go test -update and review the diff):\n%s", name, got)
	}
}

// TestSupportCarriesPageFunctions: the support module holds the embedded
// page functions, verbatim, as one JSON string literal.
func TestSupportCarriesPageFunctions(t *testing.T) {
	lit, err := json.Marshal(pageFunctions)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(Support(), lit) {
		t.Error("the support module does not carry pagefns.js as a string literal")
	}
	for _, want := range []string{"VISIBLE_MIN_CONTRAST = 1.5", "VISIBLE_MIN_OPACITY = 0.1", "LEGIBLE_MIN_CONTRAST = 4.5",
		"LEGIBLE_MIN_CONTRAST_LARGE = 3", "elementsFromPoint", "elementFromPoint", "Emoji_Presentation"} {
		if !strings.Contains(pageFunctions, want) {
			t.Errorf("pagefns.js lacks %q", want)
		}
	}
}

// TestCompileHeader: every spec imports test and expect from maul's
// fixture (never @playwright/test), and records its viewport and action
// timeout.
func TestCompileHeader(t *testing.T) {
	b := parseBrowser(t, goldenCases[0].check)
	for _, tc := range []struct {
		opts Options
		want []string
	}{
		{Options{}, []string{`from "./maul-fixture"`, `from "./maul-checks"`, "width: 1280, height: 800",
			"actionTimeout: 5000", "test.setTimeout(30000)", "page.setViewportSize({ width: 1280, height: 800 })"}},
		{Options{Viewport: Viewport{Width: 390, Height: 844}}, []string{"width: 390, height: 844"}},
	} {
		got, err := Compile(b, tc.opts)
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range tc.want {
			if !bytes.Contains(got, []byte(w)) {
				t.Errorf("compiled spec lacks %q:\n%s", w, got)
			}
		}
		if bytes.Contains(got, []byte("@playwright/test")) {
			t.Error("a compiled spec imports @playwright/test; it must import maul's fixture")
		}
	}
}

// TestCompileRefuses: Compile compiles only a valid check it can emit
// exactly.
func TestCompileRefuses(t *testing.T) {
	visible := `{"expect":{"assert":"visible","locator":{"role":"heading"}}}`
	for _, tc := range []struct {
		name, check string
		opts        Options
		want        string
	}{
		{"invalid check", `{"steps":[{"goto":"/"}]}`, Options{}, "not-substantive"},
		{"no steps", `{"steps":[]}`, Options{}, "at least one step"},
		{"snapshot ref", `{"steps":[{"goto":"/"},{"click":{"text":"e21"}},` + visible + `]}`, Options{},
			"snapshot-ref"},
		{"unknown role", `{"steps":[{"goto":"/"},{"expect":{"assert":"visible","locator":{"role":"widget"}}}]}`,
			Options{}, `role "widget"`},
		{"unknown role within", `{"steps":[{"goto":"/"},{"expect":{"assert":"visible","locator":{"text":"x",` +
			`"within":{"role":"thing"}}}}]}`, Options{}, `role "thing"`},
		{"selector chain", `{"steps":[{"goto":"/"},{"expect":{"assert":"visible","locator":{"css":"main >> text=x"}}}]}`,
			Options{}, "selector engine"},
		{"run without token", `{"steps":[{"goto":"/b/{{run}}"},` + visible + `]}`, Options{}, "run token"},
		{"bad token", `{"steps":[{"goto":"/b/{{run}}"},` + visible + `]}`, Options{RunToken: "a/b"}, "run token"},
		{"bad viewport", `{"steps":[{"goto":"/"},` + visible + `]}`, Options{Viewport: Viewport{Width: -1, Height: 9}},
			"viewport"},
		{"huge viewport", `{"steps":[{"goto":"/"},` + visible + `]}`,
			Options{Viewport: Viewport{Width: 1280, Height: 100000}}, "viewport"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := Compile(parseBrowser(t, tc.check), tc.opts)
			if err == nil {
				t.Fatalf("Compile accepted it:\n%s", out)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// TestCompileRefusalIsTyped: a check the grammar refuses comes back as
// the grammar's *checks.Refusal, so callers can name the rule.
func TestCompileRefusalIsTyped(t *testing.T) {
	_, err := Compile(parseBrowser(t, `{"steps":[{"goto":"https://example.com/"}]}`), Options{})
	var r *checks.Refusal
	if !errors.As(err, &r) || r.Rule != checks.RuleOffPreview {
		t.Fatalf("err = %v, want a *checks.Refusal of rule %s", err, checks.RuleOffPreview)
	}
}

// TestRunTokenSubstituted: {{run}} becomes the token everywhere, and no
// placeholder survives.
func TestRunTokenSubstituted(t *testing.T) {
	for _, tc := range goldenCases {
		out, err := Compile(parseBrowser(t, tc.check), Options{RunToken: testToken})
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(out, []byte("{{")) {
			t.Errorf("%s: a placeholder survived:\n%s", tc.name, out)
		}
		if strings.Contains(tc.check, "{{run}}") && !bytes.Contains(out, []byte(testToken)) {
			t.Errorf("%s: the run token is missing", tc.name)
		}
	}
}
