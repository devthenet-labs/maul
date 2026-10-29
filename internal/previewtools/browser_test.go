// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/devthenet-labs/maul/internal/toolwire"
)

// fakeCLI is the Playwright CLI: it records every argv and answers each
// verb from script, writing the artifacts an answer names under its dir.
type fakeCLI struct {
	mu     sync.Mutex
	dir    string
	calls  [][]string
	script func(args []string, n int) (string, int)
	files  map[string][]byte // written under .playwright-cli/ before an answer that names them
}

func (f *fakeCLI) Run(_ context.Context, args []string) (CLIResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := len(f.calls)
	f.calls = append(f.calls, slices.Clone(args))
	out, exit := f.script(args, n)
	for name, b := range f.files {
		if strings.Contains(out, cliArtifactDir+"/"+name) {
			p := filepath.Join(f.dir, cliArtifactDir, name)
			_ = os.MkdirAll(filepath.Dir(p), 0o700)
			_ = os.WriteFile(p, b, 0o600)
		}
	}
	return CLIResult{Output: out, Exit: exit}, nil
}

func (f *fakeCLI) argv() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// keeper is an in-memory artifact store that keeps what it is given.
type keeper struct {
	mu sync.Mutex
	m  map[string][]byte
}

func (k *keeper) Put(b []byte) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.m == nil {
		k.m = map[string][]byte{}
	}
	s := sha256.Sum256(b)
	d := hex.EncodeToString(s[:])
	k.m[d] = slices.Clone(b)
	return d, nil
}

func (k *keeper) Read(d string) ([]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	b, ok := k.m[d]
	if !ok {
		return nil, os.ErrNotExist
	}
	return b, nil
}

const snapYAML = "- generic [ref=e1]:\n  - heading \"Battle\" [level=1] [ref=e2]\n  - button \"Vote\" [ref=e3]\n"

// actionOutput is what the CLI prints after an action: the code it ran,
// the page, and a link to the snapshot file.
func actionOutput(code string) string {
	return "\x1b[1m### Ran Playwright code\x1b[0m\n```js\n" + code + "\n```\n### Page\n- Page URL: " + frontBase +
		"/battles\n### Snapshot\n- [Snapshot](.playwright-cli/page-1.yml)\n"
}

const frontBase = "http://127.0.0.1:8091"

func browserHarness(t *testing.T, script func(args []string, n int) (string, int)) (*harness, *fakeCLI, *keeper) {
	t.Helper()
	cliDir := t.TempDir()
	cli := &fakeCLI{dir: cliDir, script: script, files: map[string][]byte{
		"page-1.yml": []byte(snapYAML),
		"shot-1.png": []byte("\x89PNG fake image"),
		"console-1.log": []byte("[ERROR] Failed to load resource: the server responded with a status of 404 " +
			"(Not Found) @ " + frontBase + "/favicon.ico:0\n"),
	}}
	k := &keeper{}
	cfg := baseConfig()
	cfg.Browser = true
	h := newHarness(t, cfg, routes{}, func(o *Options) {
		o.Artifacts = k
		o.Browser = &BrowserOptions{CLI: cli, Dir: cliDir, Base: frontBase, ConfigFile: "/tmp/maul-browser/cli.json"}
	})
	return h, cli, k
}

func defaultScript(args []string, _ int) (string, int) {
	switch args[0] {
	case "generate-locator":
		return "### Result\ngetByRole('button', { name: 'Vote' })\n", 0
	case "snapshot":
		return "### Page\n- Page URL: " + frontBase + "/battles\n### Snapshot\n```yaml\n" + snapYAML + "```\n", 0
	case "screenshot":
		return "### Result\n- [Screenshot of viewport](.playwright-cli/shot-1.png)\n", 0
	case "console":
		return "### Result\nTotal messages: 2 (Errors: 1, Warnings: 0)\n\n[ERROR] Failed to load resource: the " +
			"server responded with a status of 404 (Not Found) @ " + frontBase + "/favicon.ico:0\n[LOG] hello @ " +
			frontBase + "/app.js:3\n", 0
	case "eval":
		return "### Result\n1280\n", 0
	}
	return actionOutput("await page." + args[0] + "();"), 0
}

func browserCall(t *testing.T, h *harness, verb string, args ...string) toolwire.Response {
	t.Helper()
	body, err := json.Marshal(toolwire.BrowserRequest{Verb: verb, Args: args})
	if err != nil {
		t.Fatal(err)
	}
	code, resp := h.call(toolwire.VerbBrowser, string(body))
	if code != http.StatusOK {
		t.Fatalf("%s: status %d", verb, code)
	}
	return resp
}

// TestBrowserRefusals: every verb off the allowlist, every flag, and every
// navigation off the preview is refused before the CLI runs, each refusal
// a ledger entry.
func TestBrowserRefusals(t *testing.T) {
	tests := []struct {
		name    string
		verb    string
		args    []string
		refused string
	}{
		{"run-code", "run-code", []string{"await page.close()"}, toolwire.RefusedVerb},
		{"route", "route", []string{"**/*"}, toolwire.RefusedVerb},
		{"unroute", "unroute", nil, toolwire.RefusedVerb},
		{"attach", "attach", []string{"x"}, toolwire.RefusedVerb},
		{"detach", "detach", nil, toolwire.RefusedVerb},
		{"state-load", "state-load", []string{"/etc/passwd"}, toolwire.RefusedVerb},
		{"state-save", "state-save", []string{"s.json"}, toolwire.RefusedVerb},
		{"upload", "upload", []string{"/etc/passwd"}, toolwire.RefusedVerb},
		{"drop", "drop", []string{"e1"}, toolwire.RefusedVerb},
		{"install", "install", nil, toolwire.RefusedVerb},
		{"install-browser", "install-browser", nil, toolwire.RefusedVerb},
		{"show", "show", nil, toolwire.RefusedVerb},
		{"video-start", "video-start", nil, toolwire.RefusedVerb},
		{"pause-at", "pause-at", []string{"x"}, toolwire.RefusedVerb},
		{"resume", "resume", nil, toolwire.RefusedVerb},
		{"step-over", "step-over", nil, toolwire.RefusedVerb},
		{"close", "close", nil, toolwire.RefusedVerb},
		{"close-all", "close-all", nil, toolwire.RefusedVerb},
		{"kill-all", "kill-all", nil, toolwire.RefusedVerb},
		{"delete-data", "delete-data", nil, toolwire.RefusedVerb},
		{"open", "open", []string{"/"}, toolwire.RefusedVerb},
		{"open with --config", "open", []string{"--config=/tmp/x.json"}, toolwire.RefusedVerb},
		{"resize", "resize", []string{"300", "300"}, toolwire.RefusedVerb},
		{"tab-new", "tab-new", nil, toolwire.RefusedVerb},
		{"unknown", "frobnicate", nil, toolwire.RefusedVerb},
		{"session flag", "snapshot", []string{"-s=other"}, toolwire.RefusedVerb},
		{"--session flag", "click", []string{"e3", "--session=x"}, toolwire.RefusedVerb},
		{"--config flag", "goto", []string{"/", "--config=/tmp/x"}, toolwire.RefusedVerb},
		{"--filename flag", "screenshot", []string{"--filename=/etc/x.png"}, toolwire.RefusedVerb},
		{"--raw flag", "eval", []string{"() => 1", "--raw"}, toolwire.RefusedVerb},
		{"too many args", "click", []string{"e3", "left", "x"}, toolwire.RefusedInvalid},
		{"too few args", "fill", []string{"e3"}, toolwire.RefusedInvalid},
		{"another origin", "goto", []string{"https://example.com"}, toolwire.RefusedNavigation},
		{"file", "goto", []string{"file:///etc/passwd"}, toolwire.RefusedNavigation},
		{"chrome", "goto", []string{"chrome://settings"}, toolwire.RefusedNavigation},
		{"data", "goto", []string{"data:text/html,<p>x</p>"}, toolwire.RefusedNavigation},
		{"javascript", "goto", []string{"javascript:alert(1)"}, toolwire.RefusedNavigation},
		{"scheme-relative", "goto", []string{"//example.com/x"}, toolwire.RefusedNavigation},
		{"the shim", "goto", []string{"http://127.0.0.1:9222/json/version"}, toolwire.RefusedNavigation},
		{"the relay", "goto", []string{"http://10.0.0.9:8090/v1/finish"}, toolwire.RefusedNavigation},
		{"a component address", "goto", []string{"http://10.0.0.1:80/api"}, toolwire.RefusedNavigation},
		{"front with userinfo", "goto", []string{"http://x@127.0.0.1:8091/"}, toolwire.RefusedNavigation},
		{"a relative path", "goto", []string{"battles"}, toolwire.RefusedNavigation},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, cli, _ := browserHarness(t, defaultScript)
			resp := browserCall(t, h, tc.verb, tc.args...)
			if resp.Refused != tc.refused || resp.Reason == "" {
				t.Fatalf("got %q (%s), want %q with a reason", resp.Refused, resp.Reason, tc.refused)
			}
			if n := len(cli.argv()); n != 0 {
				t.Fatalf("a refused call ran the CLI: %v", cli.argv())
			}
			l := h.finish()
			e := l.Entries[0]
			if e.Seq != resp.Seq || e.Refused != tc.refused || e.Verb != toolwire.VerbBrowser || e.Kind != toolwire.KindAgent {
				t.Fatalf("entry %+v does not record the refusal", e)
			}
		})
	}
}

// TestBrowserGotoResolvesAgainstThePreview: a path is the preview front's,
// and the front's own URL passes as given.
func TestBrowserGotoResolvesAgainstThePreview(t *testing.T) {
	for _, tc := range []struct{ arg, want string }{
		{"/battles", frontBase + "/battles"},
		{"/battles/1?x=y#z", frontBase + "/battles/1?x=y#z"},
		{frontBase + "/battles", frontBase + "/battles"},
		{frontBase, frontBase},
	} {
		h, cli, _ := browserHarness(t, defaultScript)
		if resp := browserCall(t, h, "goto", tc.arg); resp.Refused != "" {
			t.Fatalf("goto %s refused: %s", tc.arg, resp.Reason)
		}
		got := cli.argv()
		if len(got) != 1 || !slices.Equal(got[0], []string{"goto", tc.want}) {
			t.Fatalf("goto %s ran %v, want goto %s", tc.arg, got, tc.want)
		}
	}
}

// TestBrowserActionOutput: ANSI stripped, the snapshot file inlined, its
// path rewritten, every artifact hashed and kept, the full output in the
// ledger, the exit observed.
func TestBrowserActionOutput(t *testing.T) {
	h, _, k := browserHarness(t, defaultScript)
	resp := browserCall(t, h, "reload")
	if resp.Refused != "" {
		t.Fatalf("refused: %s", resp.Reason)
	}
	if strings.Contains(resp.Untrusted, "\x1b") {
		t.Errorf("ANSI escapes reach the agent: %q", resp.Untrusted)
	}
	if !strings.Contains(resp.Untrusted, snapYAML) {
		t.Errorf("the post-action snapshot is not inline:\n%s", resp.Untrusted)
	}
	if strings.Contains(resp.Untrusted, ".playwright-cli/") {
		t.Errorf("artifact paths are not rewritten:\n%s", resp.Untrusted)
	}
	if resp.Observed == nil || resp.Observed.Exit == nil || *resp.Observed.Exit != 0 {
		t.Errorf("observed %+v, want exit 0", resp.Observed)
	}
	sum := sha256.Sum256([]byte(snapYAML))
	want := toolwire.Artifact{Name: "page-1.yml", SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(snapYAML))}
	if len(resp.Artifacts) != 1 || resp.Artifacts[0] != want {
		t.Fatalf("artifacts %+v, want %+v", resp.Artifacts, want)
	}
	if _, err := k.Read(want.SHA256); err != nil {
		t.Errorf("the artifact is not kept: %v", err)
	}
	l := h.finish()
	e := l.Entries[0]
	if e.Inline != resp.Untrusted || !strings.Contains(e.Output, snapYAML) || e.Artifacts[0] != want {
		t.Fatalf("entry %+v does not keep the output", e)
	}
}

// TestBrowserLongSnapshotIsCapped: the inline text is capped with the
// marker; the ledger keeps the full output.
func TestBrowserLongSnapshotIsCapped(t *testing.T) {
	big := strings.Repeat("- text: \"row\" [ref=e9]\n", 2000)
	h, cli, _ := browserHarness(t, defaultScript)
	cli.files["page-1.yml"] = []byte(big)
	resp := browserCall(t, h, "reload")
	if len(resp.Untrusted) > toolwire.MaxInlineBytes || !strings.HasSuffix(resp.Untrusted, toolwire.TruncationMarker) {
		t.Fatalf("inline %d bytes, want at most %d ending in the marker", len(resp.Untrusted), toolwire.MaxInlineBytes)
	}
	l := h.finish()
	if !strings.Contains(l.Entries[0].Output, big) {
		t.Errorf("the ledger does not keep the full snapshot")
	}
}

// TestBrowserConsoleFiltersFaviconNoise: the favicon 404 leaves the agent's
// console and requests output, never the ledger's.
func TestBrowserConsoleFiltersFaviconNoise(t *testing.T) {
	h, _, _ := browserHarness(t, defaultScript)
	resp := browserCall(t, h, "console")
	if strings.Contains(resp.Untrusted, "favicon.ico") || !strings.Contains(resp.Untrusted, "[LOG] hello") {
		t.Fatalf("console inline:\n%s", resp.Untrusted)
	}
	l := h.finish()
	if !strings.Contains(l.Entries[0].Output, "favicon.ico") {
		t.Errorf("the ledger lost the full output")
	}
}

// TestBrowserRecordsLocatorsForRefsActedOn: a ref acted on gets its
// generate-locator, run before the action, in the same entry.
func TestBrowserRecordsLocatorsForRefsActedOn(t *testing.T) {
	h, cli, _ := browserHarness(t, defaultScript)
	browserCall(t, h, "click", "e3")
	browserCall(t, h, "fill", "f1e19", "hello")
	browserCall(t, h, "press", "Enter")
	browserCall(t, h, "eval", "el => el.textContent", "e2")
	got := cli.argv()
	want := [][]string{
		{"generate-locator", "e3"}, {"click", "e3"},
		{"generate-locator", "f1e19"}, {"fill", "f1e19", "hello"},
		{"press", "Enter"},
		{"generate-locator", "e2"}, {"eval", "el => el.textContent", "e2"},
	}
	if !slices.EqualFunc(got, want, slices.Equal) {
		t.Fatalf("argv %v, want %v", got, want)
	}
	l := h.finish()
	loc := "getByRole('button', { name: 'Vote' })"
	for i, wantLocs := range [][]string{{loc}, {loc}, nil, {loc}} {
		if !slices.Equal(l.Entries[i].Locators, wantLocs) {
			t.Errorf("entry %d locators %v, want %v", i+1, l.Entries[i].Locators, wantLocs)
		}
	}
}

// TestBrowserScreenshotIsServed: a screenshot is an artifact maul-tool
// fetches by digest; an unrecorded digest is not served.
func TestBrowserScreenshotIsServed(t *testing.T) {
	h, _, _ := browserHarness(t, defaultScript)
	resp := browserCall(t, h, "screenshot")
	if len(resp.Artifacts) != 1 || !toolwire.IsScreenshot(resp.Artifacts[0]) {
		t.Fatalf("artifacts %+v, want one screenshot", resp.Artifacts)
	}
	a := resp.Artifacts[0]
	req := httptest.NewRequest(http.MethodGet, toolwire.ArtifactPath(a.SHA256), nil)
	req.RemoteAddr = "10.9.9.9:41000"
	rec := httptest.NewRecorder()
	h.relay.Handler().ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Body)
	if rec.Code != http.StatusOK || string(body) != "\x89PNG fake image" {
		t.Fatalf("GET artifact: %d %q", rec.Code, body)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("artifact served without nosniff")
	}
	for _, path := range []string{toolwire.ArtifactPath(strings.Repeat("b", 64)), "/v1/artifacts/../readyz",
		"/v1/artifacts/ABC"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "10.9.9.9:41000"
		rec := httptest.NewRecorder()
		h.relay.Handler().ServeHTTP(rec, req)
		if rec.Code == http.StatusOK {
			t.Errorf("GET %s served an artifact no entry recorded", path)
		}
	}
	// The cited screenshot leaves as evidence at finish.
	body2 := mustJSON(t, toolwire.FinishRequest{Evidence: []toolwire.EvidenceRef{{Seq: resp.Seq, SHA256: a.SHA256}}})
	if code, fr := h.call(toolwire.VerbFinish, body2); code != http.StatusOK || fr.Refused != "" {
		t.Fatalf("finish: %d %+v", code, fr)
	}
	l := h.verify()
	if len(l.Evidence) != 1 {
		t.Fatalf("evidence %+v, want the screenshot", l.Evidence)
	}
}

// TestBrowserReopensALostSessionOnce: a lost session is reopened once, as
// a relay entry, and the call retried; a second loss is the agent's error.
func TestBrowserReopensALostSessionOnce(t *testing.T) {
	lost := "The browser 'maul' is not open, please run open first\n"
	h, cli, _ := browserHarness(t, func(args []string, n int) (string, int) {
		switch {
		case args[0] == "open", args[0] == "resize":
			return actionOutput("await page.goto('about:blank');"), 0
		case n == 0 || n == 4: // the first snapshot, and the one after the reopen's retry
			return lost, 1
		}
		return defaultScript(args, n)
	})
	if resp := browserCall(t, h, "snapshot"); resp.Refused != "" || !strings.Contains(resp.Untrusted, "button") {
		t.Fatalf("after a reopen: %+v", resp)
	}
	resp := browserCall(t, h, "snapshot")
	if !strings.Contains(resp.Untrusted, "is not open") {
		t.Fatalf("a second loss is reported as is: %+v", resp)
	}
	got := cli.argv()
	want := [][]string{{"snapshot"}, {"open", "--config=/tmp/maul-browser/cli.json"}, {"resize", "1280", "800"},
		{"snapshot"}, {"snapshot"}}
	if !slices.EqualFunc(got, want, slices.Equal) {
		t.Fatalf("argv %v, want %v", got, want)
	}
	l := h.finish()
	kinds := make([]string, 0, len(l.Entries))
	for _, e := range l.Entries {
		kinds = append(kinds, fmt.Sprintf("%s/%s", e.Kind, e.Verb))
	}
	wantKinds := []string{"relay/session", "agent/browser", "agent/browser", "runner/finish"}
	if !slices.Equal(kinds, wantKinds) {
		t.Fatalf("entries %v, want %v", kinds, wantKinds)
	}
	if !strings.Contains(string(l.Entries[0].Args), "reopen") {
		t.Errorf("the reopen entry args %s do not say reopen", l.Entries[0].Args)
	}
}

// TestOpenBrowserSession: the relay opens the session with its config file
// and sets the viewport, one relay entry.
func TestOpenBrowserSession(t *testing.T) {
	h, cli, _ := browserHarness(t, defaultScript)
	h.relay.openBrowser(context.Background())
	want := [][]string{{"open", "--config=/tmp/maul-browser/cli.json"}, {"resize", "1280", "800"}}
	if got := cli.argv(); !slices.EqualFunc(got, want, slices.Equal) {
		t.Fatalf("argv %v, want %v", got, want)
	}
	l := h.finish()
	if e := l.Entries[0]; e.Kind != toolwire.KindRelay || e.Verb != toolwire.VerbSession ||
		!strings.Contains(string(e.Args), "open") {
		t.Fatalf("entry %+v, want the relay's session open", e)
	}
}

// TestWriteCLIConfig: the key sits in a 0600 file, never in argv.
func TestWriteCLIConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cli.json")
	key := []byte(strings.Repeat("k", 43))
	if err := WriteCLIConfig(path, "http://127.0.0.1:9222", key); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v (%v), want 0600", fi.Mode(), err)
	}
	b, _ := os.ReadFile(path)
	var got struct {
		Browser struct {
			BrowserName string            `json:"browserName"`
			CDPEndpoint string            `json:"cdpEndpoint"`
			CDPHeaders  map[string]string `json:"cdpHeaders"`
		} `json:"browser"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Browser.BrowserName != "chromium" || got.Browser.CDPEndpoint != "http://127.0.0.1:9222" ||
		got.Browser.CDPHeaders[CDPKeyHeader] != string(key) {
		t.Fatalf("config %s", b)
	}
	if err := WriteCLIConfig(path, "http://127.0.0.1:9222", []byte("short")); err == nil {
		t.Errorf("a short key was written")
	}
}

// TestFrontRoutesByPath: the preview front sends each path to the
// component serving it, with the gateway's header set and the preview
// host, and refuses a path no component serves.
func TestFrontRoutesByPath(t *testing.T) {
	api := newFakeComponent(t, func(w http.ResponseWriter, _ *http.Request, _ int) { _, _ = io.WriteString(w, "api") })
	web := newFakeComponent(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		if r.URL.Path == "/old" {
			w.Header().Set("Location", "https://intent-1.preview.example/new?x=1")
			w.WriteHeader(http.StatusFound)
			return
		}
		_, _ = io.WriteString(w, "web")
	})
	rt := routes{"10.0.0.1:80": api.srv.Listener.Addr().String(), "10.0.0.2:80": web.srv.Listener.Addr().String()}
	front := NewFront(baseConfig(), rt.dial)
	for _, tc := range []struct{ path, want string }{{"/api/battles", "api"}, {"/battles", "web"}, {"/", "web"}} {
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8091"+tc.path, nil)
		req.Header.Set("Cookie", "maul_gw=1; app=2")
		req.Header.Set("X-Maul-Viewer", "forged")
		rec := httptest.NewRecorder()
		front.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || rec.Body.String() != tc.want {
			t.Fatalf("%s: %d %q, want %q", tc.path, rec.Code, rec.Body.String(), tc.want)
		}
	}
	seen := api.requests()[0]
	if seen.Host != "intent-1.preview.example" || seen.Header.Get("X-Maul-Viewer") != "maul-evaluator-pv1" ||
		seen.Header.Get("X-Forwarded-Host") != "intent-1.preview.example" {
		t.Errorf("the component saw %+v, want the gateway's header set", seen)
	}
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8091/old", nil)
	rec := httptest.NewRecorder()
	front.ServeHTTP(rec, req)
	if loc := rec.Header().Get("Location"); loc != "/new?x=1" {
		t.Errorf("a redirect to the preview host left the front: Location %q", loc)
	}
	cfg := baseConfig()
	cfg.Components = cfg.Components[:1]
	rec = httptest.NewRecorder()
	NewFront(cfg, rt.dial).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8091/battles", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("a path no component serves: %d, want 404", rec.Code)
	}
}

// TestRunOpensTheSessionAndServesTheFront: with a browser, Run serves the
// preview front from the start and opens the CLI session (a relay session
// entry) after the replay and before the agent's first call.
func TestRunOpensTheSessionAndServesTheFront(t *testing.T) {
	api := fakeAPI(t, func(int) int { return 5 })
	rt := routes{"10.0.0.1:80": api.srv.Listener.Addr().String(), "10.0.0.2:80": api.srv.Listener.Addr().String()}
	cfg := baseConfig()
	cfg.Browser = true
	l := newLifecycle(t, cfg, rt)
	cli := &fakeCLI{dir: t.TempDir(), script: defaultScript}
	l.o.Browser = &BrowserOptions{CLI: cli, Dir: cli.dir, Base: frontBase, ConfigFile: "/tmp/cli.json"}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	front, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l.o.Listener, l.o.Front = ln, front
	done := make(chan error, 1)
	go func() { _, err := Run(context.Background(), l.o); done <- err }()
	base := "http://" + ln.Addr().String()
	waitReady(t, base)

	resp, err := http.Get("http://" + front.Addr().String() + "/api/players/p1")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the front answered %d", resp.StatusCode)
	}
	fin, err := http.Post(base+toolwire.Path(toolwire.VerbFinish), "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = fin.Body.Close()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	led := l.verify(t)
	verbs := make([]toolwire.Verb, 0, len(led.Entries))
	for _, e := range led.Entries {
		verbs = append(verbs, e.Verb)
	}
	want := []toolwire.Verb{toolwire.VerbHTTP, toolwire.VerbSession, toolwire.VerbFinish}
	if !slices.Equal(verbs, want) {
		t.Fatalf("entries %v, want readiness, the session open, finish", verbs)
	}
	if got := cli.argv(); len(got) != 2 || got[0][0] != "open" || got[1][0] != "resize" {
		t.Errorf("argv %v, want open then resize", got)
	}
}
