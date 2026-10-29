// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

//go:build e2edial

package previewtools

import (
	"bytes"
	"context"
	"encoding/json"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/checks"
	"github.com/devthenet-labs/maul/internal/checks/playwright"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// scriptedFixture is the scripted browser serving page on its own front.
func scriptedFixture(t *testing.T, page ScriptedPage) *BrowserOptions {
	t.Helper()
	br, _, front, err := ScriptedBrowser(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/battles" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	})}
	go func() { _ = srv.Serve(front) }()
	t.Cleanup(func() { _ = srv.Close() })
	return br
}

// voteButton is a 32 px square button at the given width.
func votePage(width float64, mut func(*ScriptedElement)) ScriptedPage {
	el := ScriptedElement{Role: "button", Name: "Vote", TestID: "vote", Text: "Vote", X: 100, Y: 200, Width: width,
		Height: 32}
	if mut != nil {
		mut(&el)
	}
	return ScriptedPage{Title: "Battle", Elements: []ScriptedElement{
		{Role: "heading", Name: "Battle", Text: "Battle", X: 10, Y: 10, Width: 300, Height: 40}, el}}
}

func ptr[T any](v T) *T { return &v }

// measuredCheck is the check the e2e evaluator saves: the vote button is
// visible, 32 px square and clickable.
func measuredCheck() checks.BrowserCheck {
	vote := &checks.Locator{TestID: "vote"}
	return checks.BrowserCheck{Steps: []checks.BrowserStep{
		{Goto: "/battles"},
		{Expect: &checks.Assertion{Assert: checks.AssertVisible, Locator: vote}},
		{Expect: &checks.Assertion{Assert: checks.AssertSize, Locator: vote, Width: ptr(32.0), Height: ptr(32.0)}},
		{Expect: &checks.Assertion{Assert: checks.AssertClickable, Locator: vote}},
		{Expect: &checks.Assertion{Assert: checks.AssertText, Locator: &checks.Locator{Role: "heading"},
			Text: &checks.TextMatch{Contains: "Battle"}}},
	}}
}

// TestScriptedRunnerMeasures: the scripted runner measures the compiled
// check's assertions against the page, so a box 2 px off, a hidden or a
// covered element fails at its step with the support module's words, and
// a statement it cannot read is a check error, never a pass.
func TestScriptedRunnerMeasures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		page   ScriptedPage
		check  checks.BrowserCheck
		result toolwire.AttemptResult
		step   int
		detail string
	}{
		{name: "correct", page: votePage(32, nil), check: measuredCheck(), result: toolwire.ResultPass, step: 4},
		{name: "2px narrow", page: votePage(30, nil), check: measuredCheck(), result: toolwire.ResultFail, step: 2,
			detail: "maul: size failed: width 30 not 32 +/- 0"},
		{name: "hidden", page: votePage(32, func(e *ScriptedElement) { e.Hidden = true }), check: measuredCheck(),
			result: toolwire.ResultFail, step: 1, detail: "maul: visible failed: the element is hidden"},
		{name: "covered", page: votePage(32, func(e *ScriptedElement) { e.Covered = true }), check: measuredCheck(),
			result: toolwire.ResultFail, step: 3,
			detail: "maul: clickable failed: another element receives a click at its centre"},
		{name: "missing", page: ScriptedPage{}, check: measuredCheck(), result: toolwire.ResultFail, step: 1,
			detail: "an action timed out"},
		{name: "text", page: votePage(32, nil), check: checks.BrowserCheck{Steps: []checks.BrowserStep{
			{Goto: "/battles"},
			{Expect: &checks.Assertion{Assert: checks.AssertText, Locator: &checks.Locator{TestID: "vote"},
				Text: &checks.TextMatch{Matches: "^Vo"}}},
			{Expect: &checks.Assertion{Assert: checks.AssertText, Locator: &checks.Locator{TestID: "vote"},
				Text: &checks.TextMatch{Equals: "Voted"}}},
		}}, result: toolwire.ResultFail, step: 2, detail: "the text expectation was not met"},
		{name: "css is unread", page: votePage(32, nil), check: checks.BrowserCheck{Steps: []checks.BrowserStep{
			{Goto: "/battles"},
			{Expect: &checks.Assertion{Assert: checks.AssertVisible, Locator: &checks.Locator{CSS: "button"}}},
		}}, result: toolwire.ResultError, step: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			br := scriptedFixture(t, tc.page)
			spec, err := playwright.Compile(tc.check, playwright.Options{RunToken: "v1a1t1-abc"})
			if err != nil {
				t.Fatal(err)
			}
			report, err := br.Runner.RunSpec(context.Background(), spec)
			if err != nil {
				t.Fatal(err)
			}
			steps := judgeReport(report, spec)
			last := steps[len(steps)-1]
			if last.Result != tc.result || last.Index != tc.step || (tc.detail != "" && last.Detail != tc.detail) {
				t.Errorf("steps %+v; want step %d %s %q", steps, tc.step, tc.result, tc.detail)
			}
		})
	}
}

// TestScriptedCLI: the CLI answers in the Playwright CLI's shape, through
// the front: a snapshot file with refs, a locator for a ref, a PNG
// screenshot, and "not open" before the session is.
func TestScriptedCLI(t *testing.T) {
	br := scriptedFixture(t, votePage(32, nil))
	ctx := context.Background()
	run := func(args ...string) CLIResult {
		t.Helper()
		out, err := br.CLI.Run(ctx, args)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if out := run("snapshot"); out.Exit == 0 || !strings.Contains(out.Output, lostSession) {
		t.Errorf("snapshot before open = %+v, want not open", out)
	}
	run("open", "--config="+br.ConfigFile)
	out := run("goto", br.Base+"/battles")
	m := snapshotLink.FindStringSubmatch(out.Output)
	if out.Exit != 0 || m == nil {
		t.Fatalf("goto = %+v, want a snapshot link", out)
	}
	snap, err := os.ReadFile(filepath.Join(br.Dir, cliArtifactDir, m[1]))
	if err != nil || !strings.Contains(string(snap), `button "Vote" [ref=e3]`) {
		t.Errorf("snapshot %q (%v), want the vote button at e3", snap, err)
	}
	if out := run("generate-locator", "e3"); !strings.Contains(out.Output, "### Result\ngetByTestId('vote')") {
		t.Errorf("generate-locator = %+v", out)
	}
	if out := run("click", "e9"); out.Exit == 0 {
		t.Errorf("click of an unknown ref = %+v, want an error", out)
	}
	shot := run("screenshot")
	m = artifactLink.FindStringSubmatch(shot.Output)
	if m == nil || !strings.HasSuffix(m[1], ".png") {
		t.Fatalf("screenshot = %+v, want a png link", shot)
	}
	data, err := os.ReadFile(filepath.Join(br.Dir, cliArtifactDir, m[1]))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := png.Decode(bytes.NewReader(data)); err != nil {
		t.Errorf("screenshot is no PNG: %v", err)
	}
}

// TestE2EBrowserSeam: the browser seam is the scripted browser only for
// an absolute directory.
func TestE2EBrowserSeam(t *testing.T) {
	if _, err := SeamsFrom(envOf(map[string]string{BrowserEnv: "relative"})); err == nil {
		t.Error("a relative browser directory was admitted")
	}
	dir := t.TempDir()
	s, err := SeamsFrom(envOf(map[string]string{BrowserEnv: dir}))
	if err != nil || s.BrowserDir != dir {
		t.Fatalf("seams = %+v, %v", s, err)
	}
	br, _, front, err := s.Browser()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = front.Close() }()
	if _, ok := br.CLI.(*scriptedCLI); !ok || br.Base != "http://"+front.Addr().String() {
		t.Errorf("browser %+v on %s, want the scripted one on its front", br, front.Addr())
	}
}
