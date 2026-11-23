// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

//go:build e2edial

package previewtools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/devthenet-labs/maul/internal/toolwire"
)

// The e2e suite's scripted browser (SPEC S1-24). envtest runs no Chromium,
// so the e2e build's relay puts a browser in miniature behind the same
// BrowserCLI seam the image's Playwright CLI fills, and everything around
// it (the allowlist, navigation confinement, the preview front, snapshot
// inlining, artifact keeping, generate-locator, the ledger) runs as
// released.
//
// Every page it opens it fetches through the relay's own preview front, so
// the front's path routing reaches the component, and reads the answer as
// a ScriptedPage: elements with a role, a name, a test id, a text and a
// box, each optionally hidden, covered or illegible. The CLI answers the
// allowlisted verbs in the Playwright CLI's output format, writing its
// snapshots and screenshots (a PNG drawn from the boxes) where the CLI
// writes them. Never in a released binary.

// ScriptedPage is a page as the scripted browser renders it: the JSON a
// component answers in a page's place.
type ScriptedPage struct {
	Title    string            `json:"title,omitempty"`
	Elements []ScriptedElement `json:"elements,omitempty"`
}

// ScriptedElement is one element of a ScriptedPage, its box in CSS px at
// the fixed viewport.
type ScriptedElement struct {
	Role   string  `json:"role"`
	Name   string  `json:"name,omitempty"`
	TestID string  `json:"testid,omitempty"`
	Label  string  `json:"label,omitempty"`
	Text   string  `json:"text,omitempty"`
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
	// Hidden: no box, not visible (display: none).
	Hidden bool `json:"hidden,omitempty"`
	// Covered: another element receives a click at its centre.
	Covered bool `json:"covered,omitempty"`
	// Illegible: its text's contrast is under WCAG AA.
	Illegible bool `json:"illegible,omitempty"`
}

// ScriptedBrowser is the scripted browser working under dir, and its
// preview front's loopback listener (a port of its own, so suites run in
// parallel).
func ScriptedBrowser(dir string) (*BrowserOptions, ArtifactKeeper, net.Listener, error) {
	session := filepath.Join(dir, "session")
	if err := os.MkdirAll(session, 0o700); err != nil {
		return nil, nil, nil, err
	}
	front, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, nil, err
	}
	base := "http://" + front.Addr().String()
	f := scriptedFetcher{base: base, client: &http.Client{Timeout: 10 * time.Second,
		Transport: &http.Transport{Proxy: nil}}}
	return &BrowserOptions{
		CLI:        &scriptedCLI{dir: session, fetch: f},
		Dir:        session,
		Base:       base,
		ConfigFile: filepath.Join(dir, "cli.json"),
	}, DirArtifacts{Dir: filepath.Join(dir, "artifacts")}, front, nil
}

// scriptedFetcher loads pages through the preview front.
type scriptedFetcher struct {
	base   string
	client *http.Client
}

// load fetches target (a URL on the front, or a path) and reads its page;
// an answer that is not a ScriptedPage is an empty page.
func (f scriptedFetcher) load(ctx context.Context, target string) (string, int, ScriptedPage, error) {
	u := target
	if strings.HasPrefix(target, "/") {
		u = f.base + target
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return u, 0, ScriptedPage{}, err
	}
	res, err := f.client.Do(req)
	if err != nil {
		return u, 0, ScriptedPage{}, err
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(res.Body, MaxResponseBytes))
	if err != nil {
		return u, res.StatusCode, ScriptedPage{}, err
	}
	var p ScriptedPage
	if json.Unmarshal(body, &p) != nil {
		p = ScriptedPage{}
	}
	return res.Request.URL.String(), res.StatusCode, p, nil
}

// ---- the CLI -----------------------------------------------------------

// scriptedCLI is the Playwright CLI's session in miniature.
type scriptedCLI struct {
	mu       sync.Mutex
	dir      string
	fetch    scriptedFetcher
	open     bool
	url      string
	page     ScriptedPage
	n        int
	requests []string
}

// Run implements BrowserCLI.
func (c *scriptedCLI) Run(ctx context.Context, args []string) (CLIResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(args) == 0 {
		return CLIResult{Output: "### Error\nError: no command\n", Exit: 1}, nil
	}
	verb, rest := args[0], args[1:]
	if verb == "open" {
		c.open, c.url, c.page = true, "about:blank", ScriptedPage{}
		return c.pageResult("### Browser `"+SessionName+"` opened with pid 1.\n", "await page.goto('about:blank');")
	}
	if !c.open {
		return CLIResult{Output: "The browser '" + SessionName + "' " + lostSession + ", please run open first\n",
			Exit: 1}, nil
	}
	switch verb {
	case "resize":
		return CLIResult{Output: code("await page.setViewportSize({ width: " + arg(rest, 0) + ", height: " +
			arg(rest, 1) + " });")}, nil
	case "goto":
		return c.navigate(ctx, arg(rest, 0), "await page.goto("+jsString(arg(rest, 0))+");")
	case "reload":
		return c.navigate(ctx, c.url, "await page.reload();")
	case "snapshot":
		return c.pageResult("", "")
	case "click", "hover", "fill", "select":
		i, res, ok := c.ref(arg(rest, 0))
		if !ok {
			return res, nil
		}
		return c.pageResult("", "await page."+locatorOf(c.page.Elements[i])+"."+verb+"();")
	case "type", "press":
		return c.pageResult("", "await page.keyboard."+verb+"("+jsString(arg(rest, 0))+");")
	case "screenshot":
		return c.screenshot()
	case "generate-locator":
		i, res, ok := c.ref(arg(rest, 0))
		if !ok {
			return res, nil
		}
		return CLIResult{Output: "### Result\n" + locatorOf(c.page.Elements[i]) + "\n"}, nil
	case "console":
		return CLIResult{Output: "### Result\nTotal messages: 0 (Errors: 0, Warnings: 0)\n"}, nil
	case "requests":
		return CLIResult{Output: "### Result\n" + strings.Join(c.requests, "\n") + "\n"}, nil
	case "eval":
		return c.eval(arg(rest, 0)), nil
	}
	return CLIResult{Output: "### Error\nError: unknown command " + strconv.Quote(verb) + "\n", Exit: 1}, nil
}

// boxScript is the one page script the scripted browser evaluates: an
// element's bounding box, found by its test id.
var boxScript = regexp.MustCompile(`querySelector\(\s*['"]\[data-testid=["']?([\w-]+)["']?\]['"]\s*\)` +
	`\s*\.getBoundingClientRect\(\)`)

// eval answers a page script: the box of the element a boxScript names, as
// {"width","height"}; any other script, or an element the page lacks, is
// the CLI's error.
func (c *scriptedCLI) eval(script string) CLIResult {
	m := boxScript.FindStringSubmatch(script)
	if m == nil {
		return CLIResult{Output: "### Error\nError: the scripted browser evaluates no such script\n", Exit: 1}
	}
	for _, el := range c.page.Elements {
		if el.TestID != m[1] || el.Hidden {
			continue
		}
		return CLIResult{Output: fmt.Sprintf("### Result\n{\"width\":%s,\"height\":%s}\n", px(el.Width),
			px(el.Height)) + code("await page.evaluate("+jsString(script)+");")}
	}
	return CLIResult{Output: "### Error\nError: TypeError: Cannot read properties of null " +
		"(reading 'getBoundingClientRect')\n", Exit: 1}
}

func arg(args []string, i int) string {
	if i < len(args) {
		return args[i]
	}
	return ""
}

// navigate loads target as the page.
func (c *scriptedCLI) navigate(ctx context.Context, target, js string) (CLIResult, error) {
	u, status, p, err := c.fetch.load(ctx, target)
	if err != nil {
		return CLIResult{Output: "### Error\nError: page.goto: net::ERR_CONNECTION_REFUSED at " + target + "\n",
			Exit: 1}, nil
	}
	c.url, c.page = u, p
	c.requests = append(c.requests, fmt.Sprintf("[GET] %s => [%d]", u, status))
	return c.pageResult("", js)
}

// ref is the element a snapshot ref names, or the CLI's error.
func (c *scriptedCLI) ref(ref string) (int, CLIResult, bool) {
	n, err := strconv.Atoi(strings.TrimPrefix(ref, "e"))
	if err != nil || !strings.HasPrefix(ref, "e") || n < 2 || n-2 >= len(c.page.Elements) {
		return 0, CLIResult{Output: "### Error\nError: Ref " + ref + " not found in the current page snapshot. " +
			"Try capturing new snapshot.\n", Exit: 1}, false
	}
	return n - 2, CLIResult{}, true
}

// pageResult is an action's answer: the code it ran, the page and a link
// to its snapshot file, as the CLI writes them.
func (c *scriptedCLI) pageResult(head, js string) (CLIResult, error) {
	c.n++
	name := fmt.Sprintf("page-%d.yml", c.n)
	if err := c.write(name, []byte(snapshotOf(c.page))); err != nil {
		return CLIResult{}, err
	}
	var b strings.Builder
	b.WriteString(head)
	if js != "" {
		b.WriteString(code(js))
	}
	fmt.Fprintf(&b, "### Page\n- Page URL: %s\n- Page Title: %s\n### Snapshot\n- [Snapshot](%s/%s)\n", c.url,
		c.page.Title, cliArtifactDir, name)
	return CLIResult{Output: b.String()}, nil
}

// screenshot draws the page's boxes as a PNG at the viewport's size.
func (c *scriptedCLI) screenshot() (CLIResult, error) {
	v := toolwire.DefaultViewport
	img := image.NewNRGBA(image.Rect(0, 0, v.Width, v.Height))
	draw.Draw(img, img.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	for i, el := range c.page.Elements {
		if el.Hidden {
			continue
		}
		shade := uint8(40 + (i*50)%160)
		r := image.Rect(int(el.X), int(el.Y), int(el.X+el.Width), int(el.Y+el.Height))
		draw.Draw(img, r.Intersect(img.Bounds()), image.NewUniform(color.NRGBA{R: shade, G: shade, B: 200, A: 255}),
			image.Point{}, draw.Src)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return CLIResult{}, err
	}
	c.n++
	name := fmt.Sprintf("page-%d.png", c.n)
	if err := c.write(name, buf.Bytes()); err != nil {
		return CLIResult{}, err
	}
	link := cliArtifactDir + "/" + name
	return CLIResult{Output: "### Result\n- [Screenshot of viewport](" + link + ")\n" +
		code("await page.screenshot({ path: '"+link+"', scale: 'css', type: 'png' });")}, nil
}

func (c *scriptedCLI) write(name string, data []byte) error {
	dir := filepath.Join(c.dir, cliArtifactDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, name), data, 0o600)
}

func code(js string) string { return "### Ran Playwright code\n```js\n" + js + "\n```\n" }

func jsString(s string) string { return "'" + strings.ReplaceAll(s, "'", "\\'") + "'" }

// snapshotOf is the page's accessibility snapshot: e1 the root, each
// element from e2 in order.
func snapshotOf(p ScriptedPage) string {
	var b strings.Builder
	b.WriteString("- generic [ref=e1]:\n")
	for i, el := range p.Elements {
		fmt.Fprintf(&b, "  - %s", el.Role)
		if el.Name != "" {
			fmt.Fprintf(&b, " %s", strconv.Quote(el.Name))
		}
		fmt.Fprintf(&b, " [ref=e%d]", i+2)
		if el.Text != "" {
			fmt.Fprintf(&b, ": %s", strconv.Quote(el.Text))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// locatorOf is the locator generate-locator gives el.
func locatorOf(el ScriptedElement) string {
	if el.TestID != "" {
		return "getByTestId(" + jsString(el.TestID) + ")"
	}
	if el.Name != "" {
		return "getByRole(" + jsString(el.Role) + ", { name: " + jsString(el.Name) + " })"
	}
	return "getByRole(" + jsString(el.Role) + ")"
}

// px is n in CSS pixels as the page reports it: one decimal at most.
func px(n float64) string { return strconv.FormatFloat(math.Round(n*10)/10, 'f', -1, 64) }
