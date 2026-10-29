// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package playwright

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/devthenet-labs/maul/internal/checks"
)

// liveEnv names a directory holding node_modules with @playwright/test
// 1.64.1 (its Chromium installed) and typescript: the live tests run only
// when it is set.
const liveEnv = "MAUL_LIVE_PLAYWRIGHT"

// standInFixture replaces maul's keyed CDP fixture (S1-12) with a local
// launch: what is under test here is the compiled spec and the page
// functions, not the attach.
const standInFixture = `export { test, expect } from "@playwright/test";
`

// liveCase is one compiled check over the fixture page and the verdict it
// must come to: pass, fail or indeterminate.
type liveCase struct {
	name, want string
	expect     checks.Assertion
}

func css(sel string) *checks.Locator { return &checks.Locator{CSS: sel} }

// liveCases are the SP-5 fixtures' verdicts (spike-SP-5.md, fixtures2.out)
// plus clickable and box cases.
func liveCases() []liveCase {
	verdicts := []struct{ id, visible, legible string }{
		{"t-hero", "pass", "pass"},
		{"t-img", "indeterminate", "indeterminate"},
		{"t-clip", "indeterminate", "indeterminate"},
		{"t-grey", "pass", "fail"},
		{"t-faint", "fail", "fail"},
		{"t-transparent", "fail", "fail"},
		{"t-glow", "indeterminate", "indeterminate"},
		{"t-large", "pass", "pass"},
		{"t-translucent", "pass", "pass"},
		{"t-gradok", "pass", "pass"},
		{"t-gradmix", "pass", "indeterminate"},
		{"t-thin", "pass", "fail"},
		{"t-underlay", "pass", "pass"},
		{"t-emoji", "pass", "indeterminate"},
		{"t-blend", "indeterminate", "indeterminate"},
		// The known miss: brightness(0) renders black on black.
		{"t-filter", "pass", "pass"},
		{"t-offscreen", "fail", "pass"},
	}
	var out []liveCase
	for _, v := range verdicts {
		out = append(out,
			liveCase{v.id + "-visible", v.visible, checks.Assertion{Assert: checks.AssertVisible, Locator: css("#" + v.id)}},
			liveCase{v.id + "-legible", v.legible, checks.Assertion{Assert: checks.AssertLegible, Locator: css("#" + v.id)}})
	}
	width := 200.0
	return append(out,
		liveCase{"c-free", "pass", checks.Assertion{Assert: checks.AssertClickable, Locator: css("#c-free")}},
		liveCase{"c-covered", "fail", checks.Assertion{Assert: checks.AssertClickable, Locator: css("#c-covered")}},
		liveCase{"c-veiled", "pass", checks.Assertion{Assert: checks.AssertClickable, Locator: css("#c-veiled")}},
		liveCase{"b-left-of", "pass", checks.Assertion{Assert: checks.AssertLeftOf, Locator: css("#b-left"),
			Other: css("#b-right")}},
		liveCase{"b-left-of-overlap", "fail", checks.Assertion{Assert: checks.AssertLeftOf, Locator: css("#b-first"),
			Other: css("#b-over")}},
		liveCase{"b-no-overlap", "pass", checks.Assertion{Assert: checks.AssertNoOverlap, Locator: css("#b-left"),
			Other: css("#b-right")}},
		liveCase{"b-overlap", "fail", checks.Assertion{Assert: checks.AssertNoOverlap, Locator: css("#b-first"),
			Other: css("#b-over")}},
		liveCase{"b-equal-size", "pass", checks.Assertion{Assert: checks.AssertEqualSize, Locator: css("#b-left"),
			Other: css("#b-right")}},
		liveCase{"b-aligned", "pass", checks.Assertion{Assert: checks.AssertAligned, Locator: css("#b-left"),
			Other: css("#b-right"), Axis: "y"}},
		liveCase{"b-above", "fail", checks.Assertion{Assert: checks.AssertAbove, Locator: css("#b-left"),
			Other: css("#b-right")}},
		liveCase{"b-size", "pass", checks.Assertion{Assert: checks.AssertSize, Locator: css("#b-left"),
			Width: &width}},
	)
}

// TestLivePageFunctions compiles one check per fixture and assertion, runs
// them with the real Playwright test runner against the fixture page and
// holds each to its verdict; it also typechecks every golden spec and the
// support module with tsc --strict.
func TestLivePageFunctions(t *testing.T) {
	root := os.Getenv(liveEnv)
	if root == "" {
		t.Skipf("set %s to a directory with node_modules holding @playwright/test and typescript", liveEnv)
	}
	dir, err := os.MkdirTemp(root, "maul-live-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	page, err := os.ReadFile(filepath.Join("testdata", "pagefns-fixtures.html"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(page)
	}))
	t.Cleanup(srv.Close)

	write := func(name string, b []byte) {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(SupportFile, Support())
	write("maul-fixture.ts", []byte(standInFixture))
	write("playwright.config.ts", []byte(`import { defineConfig } from "@playwright/test";
export default defineConfig({
  testDir: ".", testMatch: "*.spec.ts", workers: 4, retries: 0,
  reporter: [["json", { outputFile: "results.json" }]],
  use: { baseURL: process.env.MAUL_BASE },
});
`))
	cases := liveCases()
	for _, c := range cases {
		a := c.expect
		b := checks.BrowserCheck{Steps: []checks.BrowserStep{{Goto: "/fixtures.html"}, {Expect: &a}}}
		spec, err := Compile(b, Options{})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		write(c.name+".spec.ts", spec)
	}
	t.Run("typecheck", func(t *testing.T) { typecheck(t, root, dir) })

	cmd := exec.Command(filepath.Join(root, "node_modules", ".bin", "playwright"), "test")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "MAUL_BASE="+srv.URL)
	start := time.Now()
	out, _ := cmd.CombinedOutput()
	t.Logf("playwright ran %d checks in %s", len(cases), time.Since(start).Round(time.Millisecond))
	got := readVerdicts(t, filepath.Join(dir, "results.json"), out)
	for _, c := range cases {
		if v := got[c.name+".spec.ts"]; v.verdict != c.want {
			t.Errorf("%s: verdict %s, want %s (%s)", c.name, v.verdict, c.want, v.why)
		}
	}
}

// typecheck runs tsc --noEmit --strict over the live specs, the goldens and
// the support module.
func typecheck(t *testing.T, root, dir string) {
	goldens, err := filepath.Glob(filepath.Join("testdata", "*.spec.ts"))
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range goldens {
		b, err := os.ReadFile(g)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "golden-"+filepath.Base(g)), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.spec.ts"))
	files = append(files, filepath.Join(dir, SupportFile), filepath.Join(dir, "maul-fixture.ts"))
	sort.Strings(files)
	args := append([]string{"--noEmit", "--strict", "--target", "es2022", "--module", "nodenext",
		"--moduleResolution", "nodenext", "--lib", "es2022,dom", "--skipLibCheck"}, files...)
	cmd := exec.Command(filepath.Join(root, "node_modules", ".bin", "tsc"), args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("tsc: %v\n%s", err, out)
	}
	for _, f := range files {
		if strings.HasPrefix(filepath.Base(f), "golden-") {
			_ = os.Remove(f)
		}
	}
}

type liveVerdict struct{ verdict, why string }

// readVerdicts maps each spec file to its verdict: passed is pass, failed
// with the indeterminate annotation is indeterminate, failed otherwise is
// fail.
func readVerdicts(t *testing.T, path string, output []byte) map[string]liveVerdict {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no JSON report (%v); playwright said:\n%s", err, output)
	}
	var report struct {
		Suites []struct {
			File  string `json:"file"`
			Specs []struct {
				Tests []struct {
					Annotations []struct{ Type string } `json:"annotations"`
					Results     []struct {
						Status      string                     `json:"status"`
						Annotations []struct{ Type string }    `json:"annotations"`
						Errors      []struct{ Message string } `json:"errors"`
					} `json:"results"`
				} `json:"tests"`
			} `json:"specs"`
		} `json:"suites"`
	}
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	got := map[string]liveVerdict{}
	for _, s := range report.Suites {
		for _, sp := range s.Specs {
			for _, ts := range sp.Tests {
				for _, r := range ts.Results {
					v := liveVerdict{verdict: "fail"}
					if len(r.Errors) > 0 {
						v.why = firstLine(r.Errors[0].Message)
					}
					indeterminate := false
					for _, a := range append(ts.Annotations, r.Annotations...) {
						indeterminate = indeterminate || a.Type == AnnotationIndeterminate
					}
					switch {
					case r.Status == "passed":
						v.verdict = "pass"
					case indeterminate:
						v.verdict = "indeterminate"
					}
					got[filepath.Base(s.File)] = v
				}
			}
		}
	}
	if len(got) == 0 {
		t.Fatalf("the report holds no results; playwright said:\n%s", output)
	}
	return got
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	return fmt.Sprintf("%.200s", s)
}
