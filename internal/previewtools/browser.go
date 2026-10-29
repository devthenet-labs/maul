// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	maulv1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/checks/playwright"
	"github.com/devthenet-labs/maul/internal/previewauth"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// The browser relay (DESIGN 2.7; SPEC S1-12, SP-1): maul's Go relay in
// front of the pinned Playwright CLI, attached over the keyed CDP shim to
// the browser container's Chromium. The relay owns the session (opened
// before the agent starts, reopened once if lost), passes only the
// allowlisted verbs with no flags, confines navigation to the preview
// front, inlines the post-action snapshot, strips ANSI, filters favicon
// noise, hashes and keeps every artifact, and records generate-locator for
// each ref acted on.

// The tool pod's browser wiring: where the relay finds the CDP shim, its
// key, the Node project and the preview front. preview-controller mounts
// the key Secret at CDPKeyFile (SPEC S1-13).
const (
	// CDPEndpoint is the shim in the browser container (cmd/cdp-shim's
	// default address, in the pod's network namespace).
	CDPEndpoint = "http://127.0.0.1:9222"
	// CDPKeyFile holds the per-evaluation CDP key.
	CDPKeyFile = "/etc/maul-cdp/key"
	// CDPKeyHeader carries the key (internal/cdpshim.KeyHeader).
	CDPKeyHeader = "X-Maul-Cdp-Key"
	// minCDPKeyBytes is the shortest key the shim accepts.
	minCDPKeyBytes = 32
	// NodeDir is the image's Node project (internal/previewtools/node,
	// installed with npm ci).
	NodeDir = "/opt/maul-tools/node"
	// BrowserDir is the CLI's working directory, its config and the
	// check runs' temporary directories: an emptyDir.
	BrowserDir = "/tmp/maul-browser"
	// FrontAddr is the preview front's listen address: loopback only, so
	// only the pod's own containers reach it.
	FrontAddr = "127.0.0.1:8091"
	// SessionName is the relay's one CLI session.
	SessionName = "maul"
)

// cliArtifactDir is where the CLI writes snapshots, screenshots and
// console logs, under its working directory.
const cliArtifactDir = ".playwright-cli"

// lostSession is how the CLI says the session is gone.
const lostSession = "is not open"

// CLIResult is one CLI invocation's combined output and exit code.
type CLIResult struct {
	Output string
	Exit   int
}

// BrowserCLI runs the Playwright CLI once, in the session, with args (the
// session flag is the adapter's).
type BrowserCLI interface {
	Run(ctx context.Context, args []string) (CLIResult, error)
}

// ArtifactKeeper is an ArtifactStore that also keeps new artifacts.
type ArtifactKeeper interface {
	ArtifactStore
	// Put keeps data and returns its sha256 (lowercase hex).
	Put(data []byte) (string, error)
}

// BrowserOptions configure the browser relay. A relay without them answers
// every browser verb and browser check "unavailable".
type BrowserOptions struct {
	// CLI runs the Playwright CLI; Dir is its working directory, under
	// which it writes its artifacts.
	CLI BrowserCLI
	Dir string
	// Runner runs compiled browser checks; nil makes them unavailable.
	Runner TestRunner
	// Base is the preview front's URL ("http://127.0.0.1:8091"): every
	// path the agent navigates to is resolved against it.
	Base string
	// ConfigFile is the CLI session config (WriteCLIConfig), passed to
	// open by path; the key never reaches argv.
	ConfigFile string
}

// browserVerb is one allowlisted verb: its argument counts and the index
// of the argument naming an element acted on (-1 for none).
type browserVerb struct {
	min, max, target int
}

// browserVerbs is the allowlist (DESIGN 2.7). Every other verb is refused.
var browserVerbs = map[string]browserVerb{
	"goto":             {1, 1, -1},
	"snapshot":         {0, 1, -1},
	"click":            {1, 2, 0},
	"fill":             {2, 2, 0},
	"type":             {1, 1, -1},
	"press":            {1, 1, -1},
	"hover":            {1, 1, 0},
	"select":           {2, 2, 0},
	"screenshot":       {0, 1, -1},
	"console":          {0, 1, -1},
	"requests":         {0, 0, -1},
	"reload":           {0, 0, -1},
	"generate-locator": {1, 1, -1},
	"eval":             {1, 2, 1},
}

// relayVerbs are the verbs the relay keeps for itself, refused with that
// reason; anything else off the allowlist is simply not available.
var relayVerbs = map[string]bool{"open": true, "close": true, "resize": true, "tab-new": true,
	"tab-close": true, "tab-select": true, "close-all": true, "kill-all": true, "delete-data": true,
	"install": true, "install-browser": true, "list": true}

// snapshotRef is a snapshot ref: e12, or f1e19 inside a frame.
var snapshotRef = regexp.MustCompile(`^f?\d*e\d+$`)

// browser answers `maul-tool browser`.
func (r *Relay) browser(ctx context.Context, req *toolwire.BrowserRequest) result {
	b := r.br
	if b == nil || b.CLI == nil {
		return refusal(toolwire.RefusedUnavailable, reasonNoBrowser)
	}
	args, res, ok := b.admit(req)
	if !ok {
		return res
	}
	var locators []string
	if v := browserVerbs[req.Verb]; v.target >= 0 && v.target < len(req.Args) &&
		snapshotRef.MatchString(req.Args[v.target]) {
		if loc := r.generateLocator(ctx, req.Args[v.target]); loc != "" {
			locators = append(locators, loc)
		}
	}
	out, err := r.runCLI(ctx, args)
	if err != nil {
		return refusal(toolwire.RefusedUnavailable, "the browser did not answer")
	}
	full, inline, arts := b.render(out.Output, r.keeper(), req.Verb == "console" || req.Verb == "requests")
	exit := out.Exit
	return result{output: full, inline: inline, artifacts: arts, locators: locators, exit: exit,
		observed: &toolwire.Observed{Exit: &exit}}
}

// admit holds a call to the allowlist and resolves its navigation,
// returning the CLI's argv.
func (b *BrowserOptions) admit(req *toolwire.BrowserRequest) ([]string, result, bool) {
	v, ok := browserVerbs[req.Verb]
	switch {
	case relayVerbs[req.Verb]:
		return nil, refusal(toolwire.RefusedVerb, "browser "+req.Verb+" belongs to the relay: the session is "+
			"maul's and stays open"), false
	case !ok:
		return nil, refusal(toolwire.RefusedVerb, "browser "+req.Verb+" is not available; the verbs are "+
			strings.Join(allowedVerbs(), ", ")), false
	}
	for _, a := range req.Args {
		if strings.HasPrefix(a, "-") {
			return nil, refusal(toolwire.RefusedVerb, "browser flags (an argument starting with -) are the "+
				"relay's"), false
		}
	}
	if len(req.Args) < v.min || len(req.Args) > v.max {
		return nil, refusal(toolwire.RefusedInvalid, fmt.Sprintf("browser %s takes %d to %d arguments", req.Verb,
			v.min, v.max)), false
	}
	args := append([]string{req.Verb}, req.Args...)
	if req.Verb == "goto" {
		u, ok := b.resolve(req.Args[0])
		if !ok {
			return nil, refusal(toolwire.RefusedNavigation, "goto takes a path on the preview (\"/battles\") or "+
				b.Base+"; other origins, file:, chrome: and data: are refused"), false
		}
		args[1] = u
	}
	return args, result{}, true
}

func allowedVerbs() []string {
	out := make([]string, 0, len(browserVerbs))
	for v := range browserVerbs {
		out = append(out, v)
	}
	slices.Sort(out)
	return out
}

// resolve is a goto target as the URL the browser opens: a path on the
// preview resolved against the front, or the front's own URL as given.
func (b *BrowserOptions) resolve(target string) (string, bool) {
	if strings.HasPrefix(target, "/") && !strings.HasPrefix(target, "//") && !strings.ContainsAny(target, "\\") {
		u, err := url.Parse(b.Base + target)
		return b.Base + target, err == nil && sameOrigin(u, b.Base)
	}
	u, err := url.Parse(target)
	if err != nil || !sameOrigin(u, b.Base) {
		return "", false
	}
	return target, true
}

// sameOrigin reports u at the front's scheme and host, with no user info.
func sameOrigin(u *url.URL, base string) bool {
	bu, err := url.Parse(base)
	return err == nil && u.User == nil && u.Scheme == bu.Scheme && u.Host == bu.Host && u.Opaque == ""
}

// generateLocator runs generate-locator on ref and returns its locator, ""
// when it gives none.
func (r *Relay) generateLocator(ctx context.Context, ref string) string {
	out, err := r.br.CLI.Run(ctx, []string{"generate-locator", ref})
	if err != nil || out.Exit != 0 {
		return ""
	}
	text := stripANSI(out.Output)
	_, after, ok := strings.Cut(text, "### Result\n")
	if !ok {
		return ""
	}
	loc, _, _ := strings.Cut(after, "\n")
	loc = strings.TrimSpace(loc)
	if loc == "" || len(loc) > toolwire.MaxLocatorBytes {
		return ""
	}
	return loc
}

// runCLI runs args in the session, reopening a lost session once in the
// relay's life (a relay session entry) and retrying.
func (r *Relay) runCLI(ctx context.Context, args []string) (CLIResult, error) {
	out, err := r.br.CLI.Run(ctx, args)
	if err != nil || r.reopened || !strings.Contains(out.Output, lostSession) {
		return out, err
	}
	r.reopened = true
	r.openSession(ctx, "reopen")
	return r.br.CLI.Run(ctx, args)
}

// openBrowser opens the session before the agent starts (DESIGN 2.7).
func (r *Relay) openBrowser(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.br == nil || r.br.CLI == nil || r.finished {
		return
	}
	r.openSession(ctx, "open")
}

// openSession opens the CLI session against the shim and sets the fixed
// viewport (over CDP the CLI ignores its config's viewport, SP-1), one
// relay session entry. The caller holds r.mu.
func (r *Relay) openSession(ctx context.Context, why string) {
	v := playwright.DefaultViewport
	var outs []string
	exit := 0
	for _, args := range [][]string{
		{"open", "--config=" + r.br.ConfigFile},
		{"resize", strconv.Itoa(v.Width), strconv.Itoa(v.Height)},
	} {
		out, err := r.br.CLI.Run(ctx, args)
		if err != nil {
			outs, exit = append(outs, "the browser did not answer"), -1
			break
		}
		outs = append(outs, stripANSI(out.Output))
		if out.Exit != 0 {
			exit = out.Exit
			break
		}
	}
	_, _ = r.record(toolwire.LedgerEntry{Kind: toolwire.KindRelay, Verb: toolwire.VerbSession,
		Args: mustArgs(map[string]any{"action": why, "viewport": fmt.Sprintf("%dx%d", v.Width, v.Height)}),
		Exit: exit, Output: strings.Join(outs, "\n")})
}

// keeper is the relay's artifact keeper, nil when its store keeps nothing.
func (r *Relay) keeper() ArtifactKeeper {
	k, _ := r.store.(ArtifactKeeper)
	return k
}

var (
	ansiEscape   = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)`)
	artifactLink = regexp.MustCompile(`(?:\./)?\.playwright-cli/([A-Za-z0-9][A-Za-z0-9._-]{0,127})`)
	snapshotLink = regexp.MustCompile(`(?m)^- \[Snapshot\]\(\.playwright-cli/([A-Za-z0-9][A-Za-z0-9._-]{0,127}\.yml)\)$`)
)

func stripANSI(s string) string { return ansiEscape.ReplaceAllString(s, "") }

// render turns the CLI's output into the ledger's full output and the
// agent's inline text: ANSI stripped, each snapshot file inlined, artifact
// paths rewritten to their names, and (for console and requests) favicon
// 404 noise left out of the inline text. Every artifact the output names
// is hashed and, with a keeper, kept.
func (b *BrowserOptions) render(raw string, k ArtifactKeeper, filterNoise bool) (string, string,
	[]toolwire.Artifact) {
	text := stripANSI(raw)
	files := map[string][]byte{}
	var arts []toolwire.Artifact
	for _, m := range artifactLink.FindAllStringSubmatch(text, -1) {
		name := m[1]
		if _, seen := files[name]; seen || len(arts) >= toolwire.MaxArtifacts {
			continue
		}
		data, err := b.readArtifact(name)
		if err != nil {
			files[name] = nil
			continue
		}
		files[name] = data
		sum := sha256Hex(data)
		if k != nil {
			if _, err := k.Put(data); err != nil {
				continue
			}
		}
		arts = append(arts, toolwire.Artifact{Name: name, SHA256: sum, Bytes: int64(len(data))})
	}
	text = snapshotLink.ReplaceAllStringFunc(text, func(line string) string {
		name := snapshotLink.FindStringSubmatch(line)[1]
		if data := files[name]; data != nil {
			body := strings.TrimRight(string(data), "\n")
			return "```yaml\n" + body + "\n```"
		}
		return line
	})
	text = artifactLink.ReplaceAllString(text, "$1")
	inline := text
	if filterNoise {
		inline = withoutFaviconNoise(text)
	}
	return text, inline, arts
}

// readArtifact reads one artifact the CLI wrote, bounded.
func (b *BrowserOptions) readArtifact(name string) ([]byte, error) {
	root, err := os.OpenRoot(filepath.Join(b.Dir, cliArtifactDir))
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, toolwire.MaxArtifactBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > toolwire.MaxArtifactBytes {
		return nil, errors.New("artifact too large")
	}
	return data, nil
}

// withoutFaviconNoise drops the lines reporting the page's favicon.ico
// 404: the trial's only "console error" on every run.
func withoutFaviconNoise(s string) string {
	lines := strings.Split(s, "\n")
	out := lines[:0]
	for _, l := range lines {
		if strings.Contains(l, "/favicon.ico") && strings.Contains(l, "404") {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// serveArtifact answers GET /v1/artifacts/<sha256>: the bytes of an
// artifact an unrefused entry recorded, held to its digest (maul-tool
// copies screenshots to the evaluator's scratch by this path).
func (r *Relay) serveArtifact(w http.ResponseWriter, req *http.Request) {
	digest := req.PathValue("digest")
	r.mu.Lock()
	recorded := false
	for _, set := range r.artifacts {
		recorded = recorded || set[digest]
	}
	store := r.store
	r.mu.Unlock()
	if toolwire.ArtifactPath(digest) == "" || !recorded || store == nil {
		http.NotFound(w, req)
		return
	}
	data, err := store.Read(digest)
	if err != nil || sha256Hex(data) != digest || len(data) > toolwire.MaxArtifactBytes {
		http.NotFound(w, req)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data)
}

// WriteCLIConfig writes the CLI session config to path, mode 0600: the
// shim's endpoint and the key header (SP-1: browser.cdpEndpoint and
// browser.cdpHeaders). The key is never in argv or the environment.
func WriteCLIConfig(path, endpoint string, key []byte) error {
	if len(key) < minCDPKeyBytes {
		return fmt.Errorf("previewtools: a CDP key of %d bytes, need at least %d", len(key), minCDPKeyBytes)
	}
	b, err := json.Marshal(map[string]any{"browser": map[string]any{
		"browserName": "chromium", "cdpEndpoint": endpoint,
		"cdpHeaders": map[string]string{CDPKeyHeader: string(key)},
	}})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// ReadCDPKey reads the mounted key, whitespace trimmed, as the shim does.
func ReadCDPKey(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("previewtools: CDP key: %w", err)
	}
	key := []byte(strings.TrimSpace(string(raw)))
	if len(key) < minCDPKeyBytes {
		return nil, fmt.Errorf("previewtools: a CDP key of %d bytes, need at least %d", len(key), minCDPKeyBytes)
	}
	return key, nil
}

// NodeBinary is the image's node.
const NodeBinary = "/usr/bin/node"

// ImageBrowser is the released tool image's browser relay: the key read
// from CDPKeyFile into a 0600 session config under BrowserDir, the pinned
// CLI and test runner from NodeDir, artifacts kept under BrowserDir, the
// preview front at FrontAddr.
func ImageBrowser() (*BrowserOptions, ArtifactKeeper, error) {
	key, err := ReadCDPKey(CDPKeyFile)
	if err != nil {
		return nil, nil, err
	}
	cfgPath := filepath.Join(BrowserDir, "cli.json")
	if err := WriteCLIConfig(cfgPath, CDPEndpoint, key); err != nil {
		return nil, nil, err
	}
	session := filepath.Join(BrowserDir, "session")
	base := "http://" + FrontAddr
	return &BrowserOptions{
		CLI: PlaywrightCLI{Node: NodeBinary, NodeDir: NodeDir, Dir: session, Session: SessionName},
		Dir: session,
		Runner: NodeRunner{Node: NodeBinary, NodeDir: NodeDir, TmpDir: filepath.Join(BrowserDir, "checks"),
			CDPURL: CDPEndpoint, KeyFile: CDPKeyFile, BaseURL: base},
		Base:       base,
		ConfigFile: cfgPath,
	}, DirArtifacts{Dir: filepath.Join(BrowserDir, "artifacts")}, nil
}

// NewFront is the preview front: one origin for the browser, serving the
// preview's components path-routed as the gateway routes them
// (v1alpha1.PreviewRoute), each request with the gateway's header set and
// the preview host. A redirect to the preview host is rewritten to its
// path, so it stays on the front.
func NewFront(cfg Config, dial DialFunc) http.Handler {
	if dial == nil {
		dial = (&net.Dialer{Timeout: RequestTimeout}).DialContext
	}
	paths := cfg.paths()
	fwd := previewauth.ForwardedFor{Viewer: cfg.Viewer, Host: cfg.Host}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			i := maulv1.PreviewRoute(paths, pr.In.URL.Path)
			addr := cfg.Components[i].Addr
			pr.Out.URL.Scheme, pr.Out.URL.Host = "http", addr
			pr.Out.Host = addr
			if cfg.Host != "" {
				pr.Out.Host = cfg.Host
			}
			previewauth.CleanRequest(pr.Out.Header, fwd)
		},
		Transport: &http.Transport{Proxy: nil, DialContext: dial, ResponseHeaderTimeout: RequestTimeout,
			MaxResponseHeaderBytes: maxResponseHeaderBytes},
		ModifyResponse: func(resp *http.Response) error {
			if loc := resp.Header.Get("Location"); loc != "" && cfg.Host != "" {
				if u, err := url.Parse(loc); err == nil && u.IsAbs() && strings.EqualFold(u.Host, cfg.Host) {
					resp.Header.Set("Location", u.RequestURI()+fragment(u))
				}
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "maul: the component did not answer", http.StatusBadGateway)
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if maulv1.PreviewRoute(paths, req.URL.Path) < 0 {
			http.Error(w, "maul: no previewed component serves this path", http.StatusNotFound)
			return
		}
		proxy.ServeHTTP(w, req)
	})
}

func fragment(u *url.URL) string {
	if u.Fragment == "" {
		return ""
	}
	return "#" + u.EscapedFragment()
}
