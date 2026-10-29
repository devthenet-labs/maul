// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/checks"
	"github.com/devthenet-labs/maul/internal/untrusted"
)

// The evaluate prompt's bounds on what it quotes (DESIGN 2.8, SPEC Q15).
const (
	// LookAroundToolCalls is the most tool calls the look-around may
	// spend, of the run's whole MaxToolCalls.
	LookAroundToolCalls = 40
	// FixDiffMaxBytes bounds the fix diff a round after a fix quotes.
	FixDiffMaxBytes = 32 << 10
	// ChangedPathsMax bounds the changed paths the prompt lists; the rest
	// are counted.
	ChangedPathsMax = 200
	// changedPathMaxBytes bounds one changed path as quoted.
	changedPathMaxBytes = 300
	// checkShownMaxBytes bounds a check as a reviewer is shown it: its
	// canonical JSON (checks.MaxCanonicalBytes) indented, with room to spare,
	// so a valid check is never cut.
	checkShownMaxBytes = 64 << 10
	// evaluateComponentsMax bounds the previewed components named: a
	// Preview's.
	evaluateComponentsMax = 4
)

// runToken is the exploration token's grammar: what the prompt states
// as maul's own, outside any fence, so nothing but this can pass.
var runToken = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// EvaluatePrompt is the data for an intent's evaluate-stage prompt: judge
// the approved outcomes against the running preview with maul-tool, save
// checks for them, look around what the change touched, and report.
//
// Request, Outcomes, ChangedPaths and FixDiff are UNTRUSTED (DESIGN 4.4):
// the issue anyone may have written, the plan's outcome text, and paths and
// diff text from the pull request. RenderEvaluatePrompt shows each only
// inside a fenced block it labels as data, made legible by
// untrusted.Visible. Everything else is maul's own and is stated as fact.
type EvaluatePrompt struct {
	// ReportPath is where the agent writes its report, under its
	// working directory (/work/scratch).
	ReportPath string
	// ChecksDir is where the agent writes check files with its Write tool
	// (/work/scratch/checks); ShotsDir is where maul-tool writes
	// screenshots (/work/scratch/shots).
	ChecksDir string
	ShotsDir  string
	// Limits are the run's own.
	Limits StageLimits
	// Round is the verification round, 1-based.
	Round int
	// RunToken is the text the agent names the data it creates while
	// exploring with; a saved check writes {{run}} instead, which maul
	// replaces with a fresh token on every run. It must match runToken.
	RunToken string
	// Components are the previewed components: the key an http check's
	// target names, and the path it is served under.
	Components []EvaluateComponent
	// Tools are what the round's tool pod offers.
	Tools EvaluateTools
	// Request is the intent's issue, quoted as context.
	Request string
	// Outcomes are the approved outcomes, in plan order: at least one.
	Outcomes []acceptance.PlanOutcome
	// ChangedPaths are the files the pull requests change, with their
	// line counts (SPEC Q15).
	ChangedPaths []ChangedPath
	// FixDiff is the fix's diff since the last evaluated heads, on a round
	// after a fix; "" omits the section.
	FixDiff string
}

// EvaluateComponent is one previewed component as the evaluate prompt
// names it.
type EvaluateComponent struct {
	Key  string
	Path string
}

// EvaluateTools are what a round's tool pod offers beside http, logs and
// events (v1alpha1.ToolsSpec).
type EvaluateTools struct {
	// Browser offers maul-tool browser and browser checks.
	Browser bool
	// Sandbox offers maul-tool exec and exec checks.
	Sandbox bool
	// MaxToolCalls is the run's tool-call bound.
	MaxToolCalls int
	// LookAroundRoutes bounds the routes the look-around visits; zero
	// skips it.
	LookAroundRoutes int
}

// ChangedPath is one file a pull request changes, with its added and
// deleted line counts.
type ChangedPath struct {
	Path    string
	Added   int
	Deleted int
}

// evaluateView is EvaluatePrompt as the template reads it: every untrusted
// value already rendered into the text of the block that quotes it.
type evaluateView struct {
	EvaluatePrompt
	RequestBlock  string
	OutcomesBlock string
	ChangedBlock  string
	FixDiffBlock  string
	Examples      evaluateExamples
	// LookAroundCalls is LookAroundToolCalls, for the template.
	LookAroundCalls int
}

// evaluateExamples are the check and report examples the prompt shows,
// which the package's tests hold to the grammar and the report parser.
type evaluateExamples struct {
	HTTP, Browser, Exec, Report string
}

// RenderEvaluatePrompt renders the evaluate-stage prompt.
func RenderEvaluatePrompt(p EvaluatePrompt) (string, error) {
	if len(p.Outcomes) == 0 {
		return "", errors.New("render evaluate prompt: no outcome to evaluate")
	}
	if !runToken.MatchString(p.RunToken) {
		return "", fmt.Errorf("render evaluate prompt: run token %q is not %s", p.RunToken, runToken)
	}
	p.Components = boundEvaluateComponents(p.Components)
	p.Tools.LookAroundRoutes = min(max(p.Tools.LookAroundRoutes, 0), 8)
	v := evaluateView{
		EvaluatePrompt: p,
		RequestBlock:   strings.Trim(untrusted.Visible(p.Request, RequestQuoteMaxBytes), "\n"),
		OutcomesBlock:  outcomesBlock(p.Outcomes),
		ChangedBlock:   changedBlock(p.ChangedPaths),
		Examples: evaluateExamples{HTTP: ExampleHTTPCheck, Browser: ExampleBrowserCheck, Exec: ExampleExecCheck,
			Report: ExampleEvaluationReport},
		LookAroundCalls: LookAroundToolCalls,
	}
	if strings.TrimSpace(p.FixDiff) != "" {
		v.FixDiffBlock = strings.Trim(untrusted.Visible(p.FixDiff, FixDiffMaxBytes), "\n")
	}
	return render("prompt_evaluate.md.tmpl", v)
}

// boundEvaluateComponents keeps the components with a valid key and a
// path, each on one line, at most evaluateComponentsMax.
func boundEvaluateComponents(in []EvaluateComponent) []EvaluateComponent {
	var out []EvaluateComponent
	for _, c := range in {
		c = EvaluateComponent{Key: oneLine(c.Key), Path: oneLine(c.Path)}
		if len(c.Key) > acceptance.MaxKeyLength || !acceptance.KeyPattern.MatchString(c.Key) ||
			!strings.HasPrefix(c.Path, "/") || len(out) == evaluateComponentsMax {
			continue
		}
		out = append(out, c)
	}
	return out
}

// quotedLine is one untrusted value made one legible line: line breaks
// folded to spaces, cut to maxBytes, then escaped.
func quotedLine(s string, maxBytes int) string {
	s = strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ").Replace(s)
	return untrusted.Escape(untrusted.Cut(strings.TrimSpace(s), maxBytes))
}

// outcomesBlock lists the outcomes as the prompt quotes them.
func outcomesBlock(os []acceptance.PlanOutcome) string {
	var b strings.Builder
	for i, o := range os {
		if i == acceptance.MaxOutcomes {
			break
		}
		kind := o.Kind
		if kind == "" {
			kind = acceptance.OutcomeKindChange
		}
		fmt.Fprintf(&b, "- id: %s\n  kind: %s\n", quotedLine(o.ID, 64), quotedLine(string(kind), 16))
		if o.Manual {
			b.WriteString("  manual: true\n")
		}
		fmt.Fprintf(&b, "  outcome: %s\n", quotedLine(o.Outcome, acceptance.MaxOutcomeChars*4))
		if o.From != "" {
			fmt.Fprintf(&b, "  from the request: %s\n", quotedLine(o.From, acceptance.MaxFromChars*4))
		}
		if o.Manual && o.Reason != "" {
			fmt.Fprintf(&b, "  why a person judges it: %s\n", quotedLine(o.Reason, acceptance.MaxReasonChars*4))
		}
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// changedBlock lists the changed paths, "+added -deleted path", at most
// ChangedPathsMax of them; "" for none.
func changedBlock(ps []ChangedPath) string {
	var lines []string
	for _, p := range ps {
		path := quotedLine(p.Path, changedPathMaxBytes)
		if path == "" {
			continue
		}
		lines = append(lines, fmt.Sprintf("+%d -%d %s", max(p.Added, 0), max(p.Deleted, 0), path))
	}
	if len(lines) > ChangedPathsMax {
		more := len(lines) - ChangedPathsMax
		lines = append(lines[:ChangedPathsMax], "[and "+strconv.Itoa(more)+" more paths]")
	}
	return strings.Join(lines, "\n")
}

// CheckReviewPrompt is the data for one check reviewer session's prompt:
// whether one check, if it passes, shows one outcome holds.
//
// Outcome and Check are UNTRUSTED: plan text and a check the evaluator
// wrote. RenderCheckReviewPrompt quotes both as data, the check rendered
// by maul with no id, no outcome id and no step names (DESIGN 2.2 step 9,
// 4.4): a reviewer sees the steps and assertions, never a title or note the
// evaluator could argue with.
type CheckReviewPrompt struct {
	ReportPath string
	Limits     StageLimits
	// Outcome is the outcome's text.
	Outcome string
	// Check is the check under review.
	Check checks.Check
}

// checkReviewView is CheckReviewPrompt as the template reads it.
type checkReviewView struct {
	CheckReviewPrompt
	OutcomeBlock string
	Form         string
	CheckBlock   string
	Files        []reviewFile
}

// reviewFile is one exec program file as the reviewer reads it.
type reviewFile struct {
	Path, Content string
	Executable    bool
}

// RenderCheckReviewPrompt renders the check reviewer's prompt.
func RenderCheckReviewPrompt(p CheckReviewPrompt) (string, error) {
	shown, files := reviewedCheck(p.Check)
	if shown.Form != checks.FormHTTP && shown.Form != checks.FormBrowser && shown.Form != checks.FormExec {
		return "", fmt.Errorf("render check review prompt: check form %q is not http, browser or exec", shown.Form)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(shown); err != nil {
		return "", fmt.Errorf("render check review prompt: %w", err)
	}
	v := checkReviewView{
		CheckReviewPrompt: p,
		OutcomeBlock:      quotedLine(p.Outcome, acceptance.MaxOutcomeChars*4),
		Form:              string(shown.Form),
		CheckBlock:        strings.Trim(untrusted.Visible(buf.String(), checkShownMaxBytes), "\n"),
		Files:             files,
	}
	return render("prompt_checkreview.md.tmpl", v)
}

// shownCheck is a check as a reviewer is shown it: its form and body,
// never its id or its outcome's.
type shownCheck struct {
	Form    checks.Form          `json:"form"`
	HTTP    *checks.HTTPCheck    `json:"http,omitempty"`
	Browser *checks.BrowserCheck `json:"browser,omitempty"`
	Exec    *checks.ExecCheck    `json:"exec,omitempty"`
}

// reviewedCheck is c as a reviewer is shown it: a deep copy without its id,
// its outcome id or any http step's name, and with an exec check's program
// files taken out to be shown whole, each in its own block.
func reviewedCheck(c checks.Check) (shownCheck, []reviewFile) {
	shown := shownCheck{Form: c.Form}
	var files []reviewFile
	switch {
	case c.HTTP != nil:
		h := checks.HTTPCheck{Target: c.HTTP.Target, Steps: slices.Clone(c.HTTP.Steps)}
		for i := range h.Steps {
			h.Steps[i].Name = ""
		}
		shown.HTTP = &h
	case c.Browser != nil:
		b := *c.Browser
		shown.Browser = &b
	case c.Exec != nil:
		e := *c.Exec
		for _, f := range e.Files {
			files = append(files, reviewFile{Path: quotedLine(f.Path, 300),
				Content:    strings.Trim(untrusted.Visible(f.Content, checks.MaxProgramBytes), "\n"),
				Executable: f.Executable})
		}
		e.Files = nil
		shown.Exec = &e
	}
	return shown, files
}

// The examples the evaluate prompt shows: one check of each form and a
// report. Tests hold each check to checks.Validate and the report to
// report.ParseEvaluation, so the prompt never teaches a refused shape.
const (
	ExampleHTTPCheck = `{
  "id": "winner-rises-api",
  "outcome": "winner-rises",
  "form": "http",
  "http": {
    "target": "api",
    "steps": [
      {"request": {"method": "POST", "path": "/api/players", "json": {"name": "{{run}}-alice"}},
       "expect": {"status": 201}, "capture": {"alice": "/id"}},
      {"request": {"method": "POST", "path": "/api/players", "json": {"name": "{{run}}-bob"}},
       "expect": {"status": 201}, "capture": {"bob": "/id"}},
      {"request": {"method": "POST", "path": "/api/battles",
                   "json": {"winner": "{{capture.alice}}", "loser": "{{capture.bob}}"}},
       "expect": {"status": 201}},
      {"request": {"method": "GET", "path": "/api/leaderboard"},
       "expect": {"status": 200,
                  "json": {"/players": {"$order": ["{{run}}-alice", "{{run}}-bob"], "$by": "/name"}}}}
    ]
  }
}`
	ExampleBrowserCheck = `{
  "id": "winner-rises-page",
  "outcome": "winner-rises",
  "form": "browser",
  "browser": {
    "steps": [
      {"goto": "/players"},
      {"fill": {"locator": {"label": "Player name"}, "text": "{{run}}-alice"}},
      {"click": {"role": "button", "name": "Add player"}},
      {"expect": {"assert": "visible", "locator": {"role": "row", "hasText": "{{run}}-alice"}}},
      {"expect": {"assert": "legible", "locator": {"role": "row", "hasText": "{{run}}-alice"}}},
      {"expect": {"assert": "clickable", "locator": {"role": "button", "name": "Rate"}}}
    ]
  }
}`
	ExampleExecCheck = `{
  "id": "live-score-socket",
  "outcome": "live-score",
  "form": "exec",
  "exec": {
    "argv": ["python3", "client.py", "{{run}}"],
    "files": [{"path": "client.py", "content": "import sys\n# connect, send, print the reply as JSON\n"}],
    "timeout": "30s",
    "expect": {"exit": 0, "stdoutJSON": {"/player": "{{run}}-alice"}}
  }
}`
	ExampleEvaluationReport = `---
verdicts:
  - id: "winner-rises"
    verdict: pass
    ledger: [14, 15, 22]
    note: "The winner is listed above the loser after one rating, by the API and on the page."
  - id: "live-score"
    verdict: fail
    ledger: [31, 33]
    note: "The score socket closes before the first update; see the screenshot in 33."
  - id: "sounds-good"
    verdict: unsure
    ledger: []
    note: "Only a person can judge this."
disputes:
  - check_digest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
    reason: "It asserts the whole leaderboard's first row, which data from earlier rounds can take."
    replacement: "winner-rises-api"
    ledger: [3]
proposed:
  - id: "winner-rises-api"
    outcome: "winner-rises"
    digest: "sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
retire: []
anomalies:
  - summary: "The Back to battles link is dark blue on a dark background and barely readable."
    route: "/battles"
    ledger: [40, 41]
---

What I checked, in a few lines.
`
)

// ExamplePlanOutcomes is the outcome list the plan prompt shows, under the
// evaluator; a test holds it to report.ParsePlan.
const ExamplePlanOutcomes = `acceptance:
  - id: winner-rises
    outcome: "Rating a battle moves the winner above the loser on the leaderboard."
    from: "ratings should update after every battle"
    kind: change
  - id: leaderboard-still-loads
    outcome: "The leaderboard page still lists every player."
    kind: keep
  - id: sounds-good
    outcome: "The new drum kit sounds good."
    from: "make the drums punchier"
    manual: true
    reason: "only a person can judge how it sounds"`

// OutcomesExample is ExamplePlanOutcomes, for the plan prompt's template.
func (PlanPrompt) OutcomesExample() string { return ExamplePlanOutcomes }
