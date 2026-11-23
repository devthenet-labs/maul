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

// TestScriptedEvalMeasuresABox: the scripted browser evaluates one shape
// of page script, an element's bounding box by its test id, answering its
// width and height as the CLI prints a result; any other script is the
// CLI's error, never a made-up value.
func TestScriptedEvalMeasuresABox(t *testing.T) {
	br := scriptedFixture(t, votePage(30, nil))
	ctx := context.Background()
	run := func(args ...string) CLIResult {
		t.Helper()
		out, err := br.CLI.Run(ctx, args)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	run("open", "--config="+br.ConfigFile)
	run("goto", br.Base+"/battles")
	out := run("eval", `() => { const r = document.querySelector('[data-testid="vote"]').getBoundingClientRect(); `+
		`return { width: r.width, height: r.height }; }`)
	if out.Exit != 0 || !strings.HasPrefix(out.Output, "### Result\n{\"width\":30,\"height\":32}\n") {
		t.Errorf("eval of the vote box = %+v, want its width and height", out)
	}
	for _, script := range []string{"() => document.title",
		`() => document.querySelector('[data-testid="nope"]').getBoundingClientRect()`} {
		if out := run("eval", script); out.Exit == 0 {
			t.Errorf("eval %q = %+v, want the CLI's error", script, out)
		}
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
