// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/devthenet-labs/maul/internal/checks"
	"github.com/devthenet-labs/maul/internal/checks/playwright"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// Running a browser check (DESIGN 2.7, SPEC Q3): internal/checks/playwright
// compiles it under the attempt's run token, the runner writes the spec
// into a fresh directory beside maul's support module, fixture and config,
// and the Playwright test runner runs that one check in its own process,
// attached over CDP to a fresh Chromium of its own. The JSON reporter's
// report is judged here: a pass, a fail at a step, or a check error (the
// indeterminate annotation, a runner that reported nothing).

// The files a check run's directory holds.
const (
	specFile    = "check.spec.ts"
	fixtureFile = "maul-fixture.ts"
	configFile  = "playwright.config.ts"
	reportFile  = "results.json"
)

// fixtureSource is maul's fixture (node/maul-fixture.ts).
//
//go:embed node/maul-fixture.ts
var fixtureSource []byte

// configSource is the runner's config (node/playwright.config.ts).
//
//go:embed node/playwright.config.ts
var configSource []byte

// FixtureSource is maul's Playwright fixture, written beside every
// compiled check.
func FixtureSource() []byte { return append([]byte(nil), fixtureSource...) }

// ConfigSource is the test runner's config, written beside every compiled
// check.
func ConfigSource() []byte { return append([]byte(nil), configSource...) }

// TestRunner runs one compiled spec alone and returns the JSON reporter's
// report.
type TestRunner interface {
	RunSpec(ctx context.Context, spec []byte) ([]byte, error)
}

// maxReportBytes bounds a report: one test.
const maxReportBytes = 1 << 20

// checkRunTimeout bounds one runner process: the test's own timeout, the
// CDP attach and the runner's start.
const checkRunTimeout = time.Duration(playwright.TestTimeoutMS)*time.Millisecond + 60*time.Second

// browserAttempt runs a browser check once under run.
func (r *Relay) browserAttempt(ctx context.Context, b checks.BrowserCheck, run string) []toolwire.StepRecord {
	errStep := func(detail string) []toolwire.StepRecord {
		return []toolwire.StepRecord{{Result: toolwire.ResultError, Detail: detail}}
	}
	if r.br == nil || r.br.Runner == nil {
		return errStep(reasonNoBrowser)
	}
	spec, err := playwright.Compile(b, playwright.Options{RunToken: run})
	if err != nil {
		return errStep("the check did not compile: " + bounded(err.Error()))
	}
	report, err := r.br.Runner.RunSpec(ctx, spec)
	if err != nil {
		return errStep("the test runner did not run")
	}
	return judgeReport(report, spec)
}

// stepComment marks a compiled spec's step N (1-based).
var stepComment = regexp.MustCompile(`^\s*// step (\d+): `)

// judgeReport is a report's verdict as step records: every step passing,
// or the steps up to the one that did not, that one failed (or a check
// error when the test carried the indeterminate annotation). Details are
// maul's words: a message maul's support module threw, cut before any
// page text, or a fixed phrase; never the page's bytes.
func judgeReport(report, spec []byte) []toolwire.StepRecord {
	starts := stepLines(spec)
	n := max(len(starts), 1)
	errStep := func(detail string) []toolwire.StepRecord {
		return []toolwire.StepRecord{{Index: 0, Result: toolwire.ResultError, Detail: detail}}
	}
	var rep jsonReport
	if err := json.Unmarshal(report, &rep); err != nil {
		return errStep("the test runner's report is not JSON")
	}
	res, annotations, ok := rep.only()
	if !ok {
		return errStep("the test runner reported no result")
	}
	if res.Status == "passed" {
		out := make([]toolwire.StepRecord, n)
		for i := range out {
			out[i] = toolwire.StepRecord{Index: i, Result: toolwire.ResultPass}
		}
		return out
	}
	indeterminate := false
	for _, a := range append(annotations, res.Annotations...) {
		indeterminate = indeterminate || a.Type == playwright.AnnotationIndeterminate
	}
	result := toolwire.ResultFail
	switch {
	case indeterminate:
		result = toolwire.ResultError
	case res.Status != "failed" && res.Status != "timedOut":
		result = toolwire.ResultError
	}
	at, detail := 0, failureDetail(res)
	if len(res.Errors) > 0 && res.Errors[0].Location != nil {
		at = stepAt(starts, res.Errors[0].Location.Line)
	}
	out := make([]toolwire.StepRecord, 0, at+1)
	for i := range at {
		out = append(out, toolwire.StepRecord{Index: i, Result: toolwire.ResultPass})
	}
	return append(out, toolwire.StepRecord{Index: at, Result: result, Detail: detail})
}

// stepLines are the 1-based line numbers of each step's comment, in order.
func stepLines(spec []byte) []int {
	var out []int
	for i, l := range strings.Split(string(spec), "\n") {
		if stepComment.MatchString(l) {
			out = append(out, i+1)
		}
	}
	return out
}

// stepAt is the 0-based step whose lines hold line: the last step comment
// at or before it, the first step when none is.
func stepAt(starts []int, line int) int {
	at := 0
	for i, s := range starts {
		if s <= line {
			at = i
		}
	}
	return at
}

// failureDetail is maul's words for a result that did not pass.
func failureDetail(res jsonResult) string {
	if res.Status == "timedOut" {
		return "the check timed out"
	}
	if res.Status != "failed" {
		return "the test runner ended the check " + strconv.Quote(bounded(res.Status))
	}
	if len(res.Errors) == 0 {
		return "the step failed"
	}
	msg := stripANSI(res.Errors[0].Message)
	first, _, _ := strings.Cut(msg, "\n")
	first = strings.TrimPrefix(strings.TrimSpace(first), "Error: ")
	if strings.HasPrefix(first, "maul: ") {
		// The support module's own words, measurements and element
		// descriptions; only a trial click carries Playwright's error,
		// which quotes the page's markup.
		if i := strings.Index(first, "trial click"); i >= 0 {
			return first[:i] + "trial click failed"
		}
		return bounded(first)
	}
	switch {
	case strings.Contains(first, "strict mode violation"):
		return "a locator matched more than one element"
	case strings.Contains(first, "toHaveCount"):
		return "the count expectation was not met"
	case strings.Contains(first, "toHaveText"), strings.Contains(first, "toContainText"),
		strings.Contains(first, "toMatch"):
		return "the text expectation was not met"
	case strings.Contains(first, "Timeout") || strings.Contains(first, "timeout"):
		return "an action timed out"
	case strings.Contains(first, "connectOverCDP"):
		return "the browser did not answer"
	}
	return "the step failed"
}

// bounded cuts s to 300 bytes on a rune boundary.
func bounded(s string) string {
	const limit = 300
	s = strings.ToValidUTF8(s, "�")
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}

// jsonReport is the part of the JSON reporter's report the judge reads.
type jsonReport struct {
	Suites []jsonSuite `json:"suites"`
}

type jsonSuite struct {
	Specs  []jsonSpec  `json:"specs"`
	Suites []jsonSuite `json:"suites"`
}

type jsonSpec struct {
	Tests []jsonTest `json:"tests"`
}

type jsonTest struct {
	Annotations []jsonAnnotation `json:"annotations"`
	Results     []jsonResult     `json:"results"`
}

type jsonAnnotation struct {
	Type string `json:"type"`
}

type jsonResult struct {
	Status      string           `json:"status"`
	Annotations []jsonAnnotation `json:"annotations"`
	Errors      []jsonError      `json:"errors"`
}

type jsonError struct {
	Message  string `json:"message"`
	Location *struct {
		Line int `json:"line"`
	} `json:"location"`
}

// only is the report's one test's last result.
func (r jsonReport) only() (jsonResult, []jsonAnnotation, bool) {
	var tests []jsonTest
	var walk func([]jsonSuite)
	walk = func(ss []jsonSuite) {
		for _, s := range ss {
			for _, sp := range s.Specs {
				tests = append(tests, sp.Tests...)
			}
			walk(s.Suites)
		}
	}
	walk(r.Suites)
	if len(tests) != 1 || len(tests[0].Results) == 0 {
		return jsonResult{}, nil, false
	}
	t := tests[0]
	return t.Results[len(t.Results)-1], t.Annotations, true
}

// NodeRunner runs compiled checks with the image's Playwright test runner,
// attached to the shim with the key file's key.
type NodeRunner struct {
	// Node is the node binary; NodeDir the Node project holding
	// node_modules.
	Node, NodeDir string
	// TmpDir holds each run's directory, removed after it.
	TmpDir string
	// CDPURL, KeyFile and BaseURL reach the fixture and config by the
	// environment: the shim, the key's file (never its value) and the
	// preview front.
	CDPURL, KeyFile, BaseURL string
}

// RunSpec implements TestRunner: the spec alone in a fresh directory, one
// runner process.
func (n NodeRunner) RunSpec(ctx context.Context, spec []byte) ([]byte, error) {
	if err := os.MkdirAll(n.TmpDir, 0o700); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(n.TmpDir, "check-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	for name, b := range map[string][]byte{specFile: spec, playwright.SupportFile: playwright.Support(),
		fixtureFile: fixtureSource, configFile: configSource} {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			return nil, err
		}
	}
	modules := filepath.Join(n.NodeDir, "node_modules")
	if err := os.Symlink(modules, filepath.Join(dir, "node_modules")); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, checkRunTimeout)
	defer cancel()
	// @playwright/test's own runner: node_modules/.bin/playwright may be the
	// CLI's alpha build.
	cmd := exec.CommandContext(ctx, n.Node, filepath.Join(modules, "@playwright", "test", "cli.js"), "test",
		"--config", configFile)
	cmd.Dir = dir
	cmd.Env = []string{
		"PATH=" + filepath.Dir(n.Node) + ":/usr/bin:/bin",
		"HOME=" + dir, "TMPDIR=" + dir, "XDG_CACHE_HOME=" + filepath.Join(dir, ".cache"),
		"NO_UPDATE_NOTIFIER=1", "PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1", "CI=1",
		"MAUL_CDP_URL=" + n.CDPURL, "MAUL_CDP_KEY_FILE=" + n.KeyFile, "MAUL_BASE_URL=" + n.BaseURL,
	}
	cmd.WaitDelay = 5 * time.Second
	runErr := cmd.Run()
	f, err := os.Open(filepath.Join(dir, reportFile))
	if err != nil {
		return nil, errors.Join(fmt.Errorf("previewtools: no report: %w", err), runErr)
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxReportBytes+1))
	if err != nil || len(b) > maxReportBytes {
		return nil, errors.New("previewtools: the report is unreadable or too large")
	}
	return b, nil
}

// PlaywrightCLI runs the image's Playwright CLI in the relay's session.
type PlaywrightCLI struct {
	// Node is the node binary; NodeDir the Node project; Dir the CLI's
	// working directory (its artifacts are written under it).
	Node, NodeDir, Dir string
	// Session names the session (SessionName).
	Session string
	// Timeout bounds one call; zero is a minute.
	Timeout time.Duration
}

// Run implements BrowserCLI.
func (p PlaywrightCLI) Run(ctx context.Context, args []string) (CLIResult, error) {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := os.MkdirAll(p.Dir, 0o700); err != nil {
		return CLIResult{}, err
	}
	bin := filepath.Join(p.NodeDir, "node_modules", "@playwright", "cli", "playwright-cli.js")
	argv := append([]string{bin, "-s=" + p.Session}, args...)
	cmd := exec.CommandContext(ctx, p.Node, argv...)
	cmd.Dir = p.Dir
	cmd.Env = []string{
		"PATH=" + filepath.Dir(p.Node) + ":/usr/bin:/bin",
		"HOME=" + p.Dir, "TMPDIR=" + p.Dir, "XDG_CACHE_HOME=" + filepath.Join(p.Dir, ".cache"),
		"NO_UPDATE_NOTIFIER=1", "PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1",
	}
	// A file, not a pipe: open starts the session's daemon, which keeps
	// the CLI's descriptors, so a pipe would not close when the CLI exits.
	out, err := os.CreateTemp(p.Dir, ".out-")
	if err != nil {
		return CLIResult{}, err
	}
	defer func() { _ = out.Close(); _ = os.Remove(out.Name()) }()
	cmd.Stdout, cmd.Stderr = out, out
	cmd.WaitDelay = 5 * time.Second
	err = cmd.Run()
	exit := 0
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee):
		exit = ee.ExitCode()
	case err != nil:
		return CLIResult{}, err
	}
	if _, err := out.Seek(0, io.SeekStart); err != nil {
		return CLIResult{}, err
	}
	b, err := io.ReadAll(io.LimitReader(out, toolwire.MaxOutputBytes))
	if err != nil {
		return CLIResult{}, err
	}
	return CLIResult{Output: string(b), Exit: exit}, nil
}

// DirArtifacts keeps artifacts as files named by their sha256 in a
// directory (an emptyDir in the pod).
type DirArtifacts struct {
	Dir string
}

// Put implements ArtifactKeeper.
func (d DirArtifacts) Put(data []byte) (string, error) {
	sum := sha256Hex(data)
	if err := os.MkdirAll(d.Dir, 0o700); err != nil {
		return "", err
	}
	return sum, os.WriteFile(filepath.Join(d.Dir, sum), data, 0o600)
}

// Read implements ArtifactStore.
func (d DirArtifacts) Read(digest string) ([]byte, error) {
	if toolwire.ArtifactPath(digest) == "" {
		return nil, os.ErrNotExist
	}
	f, err := os.Open(filepath.Join(d.Dir, digest))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, toolwire.MaxArtifactBytes+1))
}
