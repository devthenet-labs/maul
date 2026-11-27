// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/devthenet-labs/maul/internal/cdpshim"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// The browser relay against the real thing (`mise run live-browser`,
// MAUL_LIVE_BROWSER=1): the pinned Playwright CLI from node/ (npm ci), a
// real Chromium (MAUL_LIVE_CHROMIUM, default
// /usr/bin/chromium) behind the keyed CDP shim, and the battle fixture
// (hack/testdata/battle) behind the preview front. CI's browser-tools job
// runs it on every change to node/ (a renovate bump included).

var updateGolden = flag.Bool("update-cli-format", false, "rewrite testdata/playwright-cli-format.golden")

// liveKey is the CDP key the live tests' shim demands.
var liveKey = []byte(strings.Repeat("live-key-", 5))

// liveEnv is one live setup: the shim, the fixture behind the front, the
// node project.
type liveEnv struct {
	node, nodeDir string
	shimURL       string
	keyFile       string
	frontURL      string
	cfg           Config
}

func newLiveEnv(t *testing.T, variant string) *liveEnv {
	t.Helper()
	if os.Getenv("MAUL_LIVE_BROWSER") != "1" {
		t.Skip("set MAUL_LIVE_BROWSER=1 (mise run live-browser) to run against a real Chromium")
	}
	nodeDir, err := filepath.Abs("node")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(nodeDir, "node_modules", "@playwright", "cli")); err != nil {
		t.Fatalf("run npm ci --ignore-scripts in %s first (hack/live-browser.sh does): %v", nodeDir, err)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal(err)
	}
	chromium := os.Getenv("MAUL_LIVE_CHROMIUM")
	if chromium == "" {
		chromium = "/usr/bin/chromium"
	}
	shim, err := cdpshim.New(cdpshim.Options{Key: liveKey, Launch: cdpshim.Chromium{Path: chromium,
		TempDir: t.TempDir()}.Launch, Log: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))})
	if err != nil {
		t.Fatal(err)
	}
	shimSrv := httptest.NewServer(shim.Handler())
	t.Cleanup(func() { shim.Close(); shimSrv.Close() })
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, liveKey, 0o600); err != nil {
		t.Fatal(err)
	}

	web, api := battleServers(t, variant)
	cfg := Config{
		Components: []Component{
			{Key: "api", Path: "/api", Addr: api.Listener.Addr().String()},
			{Key: "web", Path: "/", Addr: web.Listener.Addr().String()},
		},
		Host: "intent-1.preview.example", Viewer: "maul-evaluator-live", MaxToolCalls: 300, TimeoutSeconds: 600,
		Browser: true,
	}
	front := httptest.NewServer(NewFront(cfg, nil))
	t.Cleanup(front.Close)
	return &liveEnv{node: node, nodeDir: nodeDir, shimURL: shimSrv.URL, keyFile: keyFile, frontURL: front.URL,
		cfg: cfg}
}

// battleServers serves the battle fixture: web (the page under /battles/,
// its assets, variant.css) and api (/api/battles/<id>, votes kept in
// memory).
func battleServers(t *testing.T, variant string) (*httptest.Server, *httptest.Server) {
	t.Helper()
	dir := filepath.Join("..", "..", "hack", "testdata", "battle")
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	variantCSS := []byte{}
	if variant != "correct" {
		variantCSS = read(variant + ".css")
	}
	assets := map[string]struct {
		body []byte
		ct   string
	}{
		"/style.css":   {read("style.css"), "text/css"},
		"/variant.css": {variantCSS, "text/css"},
		"/battle.js":   {read("battle.js"), "text/javascript"},
	}
	page := read("battle.html")
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a, ok := assets[r.URL.Path]; ok {
			w.Header().Set("Content-Type", a.ct)
			_, _ = w.Write(a.body)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/battles/") {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(page)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(web.Close)
	var mu sync.Mutex
	votes := map[string]int{}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/votes") {
			var in struct{ Side string }
			_ = json.NewDecoder(r.Body).Decode(&in)
			mu.Lock()
			votes[in.Side]++
			mu.Unlock()
		}
		mu.Lock()
		out := map[string]any{"nameA": "Four on the floor", "nameB": "Broken beat", "votesA": votes["a"],
			"votesB": votes["b"]}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(api.Close)
	return web, api
}

func (e *liveEnv) cli(t *testing.T, session string) PlaywrightCLI {
	return PlaywrightCLI{Node: e.node, NodeDir: e.nodeDir, Dir: t.TempDir(), Session: session}
}

func (e *liveEnv) cliConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cli.json")
	if err := WriteCLIConfig(path, e.shimURL, liveKey); err != nil {
		t.Fatal(err)
	}
	return path
}

var (
	snapshotLine = regexp.MustCompile(`^(  )*- (/url: \S+|text: .+|[a-z]+( "(?:[^"\\]|\\.)*")?` +
		`( \[(level=\d+|ref=f?\d*e\d+|cursor=pointer|active|checked|disabled|expanded|pressed|selected)\])*:?` +
		`( .+)?)$`)
	refInSnapshot = regexp.MustCompile(`\[ref=(f?\d*e\d+)\]`)
	normalizers   = []struct {
		re   *regexp.Regexp
		with string
	}{
		{regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}-\d{2}-\d{2}-\d{3}Z`), "<ts>"},
		{regexp.MustCompile(`pid \d+`), "pid <pid>"},
		{regexp.MustCompile(`127\.0\.0\.1:\d+`), "<front>"},
		{regexp.MustCompile(`\[\s*\d+ms\]`), "[<ms>]"},
		{regexp.MustCompile(`--config=\S+`), "--config=<config>"},
		// The CLI reports a page's console entries on whichever command
		// first follows them, so where the block lands is a race with the
		// page's own scripts. The relay never parses it.
		{regexp.MustCompile(`(?m)^### Events\n(?:- .*\n)*`), ""},
	}
)

func normalize(s string) string {
	for _, n := range normalizers {
		s = n.re.ReplaceAllString(s, n.with)
	}
	return s
}

// TestPlaywrightCLIFormat holds the pinned CLI to the output the relay
// parses (SPEC S1-12, SP-1): the snapshot link and inline grammar, refs,
// generate-locator's result, the lost-session message, and the attached
// session's viewport after the relay's resize. Any change fails against
// the golden; rerun with -update-cli-format after reading the diff.
func TestPlaywrightCLIFormat(t *testing.T) {
	e := newLiveEnv(t, "correct")
	ctx := context.Background()
	cli := e.cli(t, "maul-format")
	cfg := e.cliConfig(t)
	var transcript strings.Builder
	run := func(args ...string) CLIResult {
		t.Helper()
		out, err := cli.Run(ctx, args)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		fmt.Fprintf(&transcript, "$ %s\n[exit %d]\n%s\n", strings.Join(args, " "), out.Exit, stripANSI(out.Output))
		return out
	}
	t.Cleanup(func() { _, _ = cli.Run(context.Background(), []string{"close"}) })

	unopened := e.cli(t, "maul-format-unopened")
	if out, err := unopened.Run(ctx, []string{"snapshot"}); err != nil || !strings.Contains(out.Output, lostSession) {
		t.Errorf("an unopened session's output %q (%v) does not say %q", out.Output, err, lostSession)
	}
	if out := run("open", "--config="+cfg); out.Exit != 0 {
		t.Fatalf("open: %s", out.Output)
	}
	run("resize", "1280", "800")
	got := run("goto", e.frontURL+"/battles/1")
	if !snapshotLink.MatchString(stripANSI(got.Output)) {
		t.Errorf("goto's output has no snapshot link the relay inlines:\n%s", got.Output)
	}
	if !artifactLink.MatchString(stripANSI(got.Output)) {
		t.Errorf("goto's output names no artifact")
	}
	vote := voteRef(t, stripANSI(run("snapshot").Output))
	loc := run("generate-locator", vote)
	if _, after, ok := strings.Cut(stripANSI(loc.Output), "### Result\n"); !ok || !strings.HasPrefix(after, "getBy") {
		t.Errorf("generate-locator %s gave %q", vote, loc.Output)
	}
	vp := run("eval", "() => [window.innerWidth, window.innerHeight].join('x')")
	if !strings.Contains(vp.Output, `"1280x800"`) {
		t.Errorf("the attached session's viewport after resize: %q, want 1280x800", vp.Output)
	}
	compareGolden(t, normalize(transcript.String()))
}

// voteRef holds every line of a snapshot's yaml block to the grammar and
// returns the first Vote button's ref.
func voteRef(t *testing.T, snap string) string {
	t.Helper()
	body, ok := yamlBlock(snap)
	if !ok {
		t.Fatalf("snapshot has no yaml block:\n%s", snap)
	}
	var vote string
	for _, l := range strings.Split(body, "\n") {
		if !snapshotLine.MatchString(l) {
			t.Errorf("snapshot line outside the grammar: %q", l)
		}
		if m := refInSnapshot.FindStringSubmatch(l); m != nil && vote == "" && strings.Contains(l, `button "Vote"`) {
			vote = m[1]
		}
	}
	if vote == "" || !snapshotRef.MatchString(vote) {
		t.Fatalf("no Vote button ref in:\n%s", body)
	}
	return vote
}

// compareGolden holds text to the CLI format golden.
func compareGolden(t *testing.T, text string) {
	t.Helper()
	golden := filepath.Join("testdata", "playwright-cli-format.golden")
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run with -update-cli-format)", err)
	}
	if string(want) != text {
		t.Errorf("the CLI's output changed (rerun with -update-cli-format once the relay handles it):\n--- got\n%s"+
			"\n--- want\n%s", text, want)
	}
}

// yamlBlock is the body of a snapshot's ```yaml block.
func yamlBlock(s string) (string, bool) {
	_, after, ok := strings.Cut(s, "```yaml\n")
	if !ok {
		return "", false
	}
	body, _, ok := strings.Cut(after, "```")
	return strings.TrimRight(body, "\n"), ok
}

// TestLiveBrowser drives the relay's own session against the real CLI and
// Chromium, as an evaluator does: open, a goto, a snapshot, a click by ref
// with its locator recorded, a screenshot kept by digest, and a goto to
// another origin refused. (The trial's matrix of compiled checks, each
// replayed three times, went with the check runner.)
func TestLiveBrowser(t *testing.T) {
	e := newLiveEnv(t, "correct")
	h, k := e.relay(t)
	e.driveSession(t, h, k)
}

// relay is a relay over the live env: the real CLI.
func (e *liveEnv) relay(t *testing.T) (*harness, *DirArtifacts) {
	t.Helper()
	k := &DirArtifacts{Dir: t.TempDir()}
	session := t.TempDir()
	cfgFile := e.cliConfig(t)
	h := newHarness(t, e.cfg, routes{}, func(o *Options) {
		o.Client = NewClient(ClientOptions{})
		o.Token = nil // crypto/rand, as released
		o.Artifacts = k
		o.Browser = &BrowserOptions{
			CLI: PlaywrightCLI{Node: e.node, NodeDir: e.nodeDir, Dir: session, Session: "maul-live-" +
				filepath.Base(session)},
			Dir:        session,
			Base:       e.frontURL,
			ConfigFile: cfgFile,
		}
	})
	t.Cleanup(func() { _, _ = h.relay.br.CLI.Run(context.Background(), []string{"close"}) })
	return h, k
}

// driveSession: the relay opens the session, the agent's goto, snapshot,
// click by ref and screenshot go through it.
func (e *liveEnv) driveSession(t *testing.T, h *harness, k *DirArtifacts) {
	t.Helper()
	h.relay.openBrowser(context.Background())
	resp := browserCall(t, h, "goto", "/battles/1")
	if resp.Refused != "" || !strings.Contains(resp.Untrusted, "```yaml") {
		t.Fatalf("goto: %+v", resp)
	}
	snap := browserCall(t, h, "snapshot")
	var vote string
	for _, l := range strings.Split(snap.Untrusted, "\n") {
		if m := refInSnapshot.FindStringSubmatch(l); m != nil && strings.Contains(l, `button "Vote"`) {
			vote = m[1]
			break
		}
	}
	if vote == "" {
		t.Fatalf("no Vote ref in %s", snap.Untrusted)
	}
	if resp := browserCall(t, h, "click", vote); resp.Refused != "" || resp.Observed == nil || *resp.Observed.Exit != 0 {
		t.Fatalf("click %s: %+v", vote, resp)
	}
	shot := browserCall(t, h, "screenshot")
	if len(shot.Artifacts) != 1 || !toolwire.IsScreenshot(shot.Artifacts[0]) {
		t.Fatalf("screenshot artifacts %+v", shot.Artifacts)
	}
	if _, err := k.Read(shot.Artifacts[0].SHA256); err != nil {
		t.Errorf("the screenshot is not kept: %v", err)
	}
	if out := browserCall(t, h, "goto", "https://example.com"); out.Refused != toolwire.RefusedNavigation {
		t.Errorf("goto another origin: %+v", out)
	}
	l := h.finish()
	var locs []string
	for _, en := range l.Entries {
		locs = append(locs, en.Locators...)
	}
	if len(locs) != 1 || !strings.HasPrefix(locs[0], "getBy") {
		t.Errorf("locators %v, want the Vote button's", locs)
	}
	if l.Entries[len(l.Entries)-1].Verb != toolwire.VerbFinish || l.Entries[0].Verb != toolwire.VerbSession {
		t.Errorf("entries do not start with the session and end with finish")
	}
}
