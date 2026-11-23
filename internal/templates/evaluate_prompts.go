// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/httpstep"
	"github.com/devthenet-labs/maul/internal/toolwire"
	"github.com/devthenet-labs/maul/internal/untrusted"
)

// The evaluate prompt's bounds on what it quotes (DESIGN 2.8, SPEC Q15).
const (
	// LookAroundToolCalls is the most tool calls the look-around may
	// spend, of the run's whole MaxToolCalls.
	LookAroundToolCalls = 40
	// FixDiffMaxBytes bounds the fix diff a round after a fix quotes.
	FixDiffMaxBytes = 32 << 10
	// FixDiffFilesMaxBytes bounds the files a cut fix diff names as not
	// shown in full, each at most FixDiffPathMaxBytes; the rest are counted.
	// A controller cutting the diff keeps the diff and its files within
	// FixDiffMaxBytes together.
	FixDiffFilesMaxBytes = 4 << 10
	FixDiffPathMaxBytes  = 256
	// fixDiffReposMax bounds the repositories a fix diff note names: a
	// Project's.
	fixDiffReposMax = 8
	// ChangedPathsMax bounds the changed paths the prompt lists; the rest
	// are counted.
	ChangedPathsMax = 200
	// changedPathMaxBytes bounds one changed path as quoted.
	changedPathMaxBytes = 300
	// evaluateComponentsMax bounds the previewed components named: a
	// Preview's.
	evaluateComponentsMax = 4
	// priorReasonMaxBytes bounds one prior verdict's reason (the Intent's
	// schema bound).
	priorReasonMaxBytes = 600
)

// runToken is the exploration token's grammar: what the prompt states
// as maul's own, outside any fence, so nothing but this can pass.
var runToken = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// EvaluatePrompt is the data for an intent's evaluate-stage prompt: use the
// running preview (or maul's builds of the pull request) as a reviewer would,
// judge each approved outcome pass, fail or unsure citing the evaluator's own
// calls in the round's ledger, look around what the change touched, and
// report (the observed evaluator: no check is written, saved or replayed).
//
// Request, Outcomes, ChangedPaths, FixDiff, FixDiffCut.Files and
// PriorVerdicts are UNTRUSTED (DESIGN 4.4): the issue anyone may have
// written, the plan's outcome text, paths and diff text from the pull
// request, and a previous round's results, whose reasons may quote its
// evaluator. RenderEvaluatePrompt shows each only inside a fenced block it
// labels as data, made legible by untrusted.Visible. Everything else is
// maul's own and is stated as fact.
type EvaluatePrompt struct {
	// ReportPath is where the agent writes its report, under its
	// working directory (/work/scratch).
	ReportPath string
	// ShotsDir is where maul-tool writes screenshots (/work/scratch/shots).
	ShotsDir string
	// Limits are the run's own.
	Limits StageLimits
	// Round is the verification round, 1-based.
	Round int
	// RunToken is the text the agent names the data it creates with. It
	// must match runToken.
	RunToken string
	// Components are the previewed components: each key, and the path it
	// is served under.
	Components []EvaluateComponent
	// Tools are what the round's tool pod offers.
	Tools EvaluateTools
	// Workspace is a round on a Project with no preview (SPEC-S2): the tool
	// pod built the pull request's head and its base, and the prompt is the
	// workspace variant, with no preview, browser or look-around, and no
	// Components. nil is a preview round, the prompt exactly as before.
	Workspace *EvaluateWorkspace
	// Request is the intent's issue, quoted as context.
	Request string
	// Outcomes are the approved outcomes, in plan order: at least one.
	Outcomes []acceptance.PlanOutcome
	// ChangedPaths are the files the pull requests change, with their
	// line counts (SPEC Q15).
	ChangedPaths []ChangedPath
	// FixDiff is the fix's diff since the last evaluated heads, on a round
	// after a fix; "" omits the section unless FixDiffCut says a diff
	// could not be read.
	FixDiff string
	// FixDiffCut is what FixDiff leaves out of the fix (code review
	// 2026-11-18, agent-prompts 8), which the prompt states as maul's own
	// fact beside the quoted diff; the zero value: FixDiff is the whole fix.
	FixDiffCut FixDiffCut
	// PriorRound is the last judged round before this one, and
	// PriorVerdicts its results (SPEC Q15): maul's result per outcome. Zero,
	// or none, omits the section.
	PriorRound    int
	PriorVerdicts []PriorVerdict

	// shed is what RenderEvaluatePrompt left out to keep the prompt within
	// its bound, which the prompt says.
	shed evaluateShed
}

// EvaluatePromptMaxBytes bounds the rendered evaluate prompt. Every harness
// that runs the evaluate stage reads it on its standard input
// (harness.Claude.PromptSpec, harness.Codex.PromptSpec), never as an
// argument, so nothing but maul limits it; the bound is kept for itself: an
// evaluator's prompt stays one it can take in whole, and what it leaves out
// it says it left out. Every part the prompt always keeps, each at its own
// bound, fits within it with room to spare (TestEvaluatePromptHeadroom).
const EvaluatePromptMaxBytes = 256 << 10

// evaluateShed counts what a prompt over its bound left out: changed
// paths.
type evaluateShed struct {
	paths int
}

// shedOne leaves out the next part of p in the order a prompt over its
// bound gives them up, and reports false when nothing is left to leave out:
// half the changed paths. The request, the fix diff (the guard against a
// fix that special-cases the calls a failure cited), the outcomes and a
// previous round's results always stay: an evaluator is never asked to
// judge without them. What is left out is counted and the prompt says so,
// never cut.
func (p *EvaluatePrompt) shedOne() bool {
	if len(p.ChangedPaths) == 0 {
		return false
	}
	keep := len(p.ChangedPaths) / 2
	p.shed.paths += len(p.ChangedPaths) - keep
	p.ChangedPaths = p.ChangedPaths[:keep]
	return true
}

// PriorVerdict is maul's result of one outcome in a previous round, with
// why it did not pass.
type PriorVerdict struct {
	Outcome, Result, Reason string
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
	// Browser offers maul-tool browser.
	Browser bool
	// BrowserEntry is the path the browser opens first; "" leaves it to the
	// evaluator. Else it must be v1alpha1.ValidBrowserEntry.
	BrowserEntry string
	// Sandbox offers maul-tool exec.
	Sandbox bool
	// MaxToolCalls is the run's tool-call bound.
	MaxToolCalls int
	// LookAroundRoutes bounds the routes the look-around visits; zero
	// skips it.
	LookAroundRoutes int
}

// Workspace surfaces, as EvaluateWorkspace names them.
const (
	SurfaceCLI     = "cli"
	SurfaceLibrary = "library"
)

// EvaluateWorkspace is what the agents-namespace tool pod built (SPEC-S2):
// the Project's surface, maul's own and stated as fact. A cli names its
// Build argv and its Binary (v1alpha1.ValidCLIBuild, ValidCLIBinary); a
// library its Language, go (toolwire.LanguageGo), and no build.
type EvaluateWorkspace struct {
	// Surface is SurfaceCLI or SurfaceLibrary.
	Surface  string
	Binary   string
	Language string
	Build    []string
}

// validate refuses a workspace that is not exactly a valid cli or a go
// library.
func (w EvaluateWorkspace) validate() error {
	switch w.Surface {
	case SurfaceCLI:
		if !v1alpha1.ValidCLIBinary(w.Binary) || !v1alpha1.ValidCLIBuild(w.Build) || w.Language != "" {
			return fmt.Errorf("a cli workspace names a build writing under %s and a binary there, and no language",
				v1alpha1.CLIBinDir)
		}
	case SurfaceLibrary:
		if w.Language != toolwire.LanguageGo || w.Binary != "" || len(w.Build) > 0 {
			return fmt.Errorf("a library workspace names language %s and no build or binary", toolwire.LanguageGo)
		}
	default:
		return fmt.Errorf("workspace surface %q is not %s or %s", w.Surface, SurfaceCLI, SurfaceLibrary)
	}
	return nil
}

// workspaceView is EvaluateWorkspace as the templates read it: Build one
// code span.
type workspaceView struct {
	CLI    bool
	Binary string
	Build  string
}

// buildSpan is a cli's build argv as one code span, every character shown.
func buildSpan(argv []string) string { return code(shown(strings.Join(argv, " "))) }

// The workspace placeholders an exploratory call is given (SPEC-S2 Q23),
// for the templates.
func (evaluateView) BinPlaceholder() string  { return toolwire.BinPlaceholder }
func (evaluateView) TreePlaceholder() string { return toolwire.TreePlaceholder }

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
	// FixDiffNote is maul's statement of what the fix diff leaves out, ""
	// when it is whole; FixDiffFilesBlock the files it names as not shown
	// in full (fixDiffView).
	FixDiffNote       string
	FixDiffFilesBlock string
	PriorBlock        string
	Examples          evaluateExamples
	// ExercisingCalls names, as one phrase, the maul-tool calls that use the
	// application on a slot round (acceptance.ValidCite's verbs the round
	// offers): only they count as a pass's or a fail's evidence.
	ExercisingCalls string
	// OwnScripts names, as one phrase, the slot round's calls that run a
	// script or program the evaluator wrote (a page script, an exec
	// program); "" when the round offers none.
	OwnScripts string
	// MaxCites is acceptance.MaxCites, the most calls of a verdict maul
	// keeps.
	MaxCites int
	// WS is the workspace, on a round with no preview.
	WS workspaceView
	// LookAroundCalls is LookAroundToolCalls, for the template.
	LookAroundCalls int
	// ReportBy is the turn by which the prompt asks for the report
	// (reportBy).
	ReportBy int
	// OutcomeTurns is the turns the prompt budgets for each outcome a person
	// does not judge (outcomeTurns).
	OutcomeTurns int
}

// outcomeTurns shares the turns before the report, less the two that start
// a round, among the outcomes a person does not judge: at least 1 each. It
// is advice only.
func outcomeTurns(reportBy int, os []acceptance.PlanOutcome) int {
	n := 0
	for _, o := range os {
		if !o.Manual {
			n++
		}
	}
	return max(1, (reportBy-2)/max(n, 1))
}

// reportBy is the turn by which an evaluate run of maxTurns is asked to
// have written its report: a tenth of its turns, at least 2, kept back for
// it, and never before turn 1.
func reportBy(maxTurns int) int {
	return max(1, maxTurns-max(2, maxTurns/10))
}

// ReportByTime is the time left, as maul-tool's time-left line counts it,
// by which an evaluate run is asked to have written its report (gate D-B8):
// an eighth of its wall clock in whole minutes, at least 2; "" for a run
// with no clock.
func (v evaluateView) ReportByTime() string {
	if v.Limits.Timeout <= 0 {
		return ""
	}
	return StageLimits{Timeout: max(2*time.Minute, (v.Limits.Timeout / 8).Truncate(time.Minute))}.WallClock()
}

// evaluateExamples are the examples the prompt shows: its report's, which
// the package's tests hold to the report parser.
type evaluateExamples struct {
	Report string
}

// RenderEvaluatePrompt renders the evaluate-stage prompt.
//
// A prompt over EvaluatePromptMaxBytes gives up parts as shedOne orders
// them, each said to be not shown, until it fits; one that still does not
// is refused, never cut.
func RenderEvaluatePrompt(p EvaluatePrompt) (string, error) {
	return renderEvaluateWithin(p, EvaluatePromptMaxBytes)
}

// renderEvaluateWithin is RenderEvaluatePrompt held to maxBytes.
func renderEvaluateWithin(p EvaluatePrompt, maxBytes int) (string, error) {
	p.shed = evaluateShed{}
	for {
		s, err := renderEvaluate(p)
		if err != nil || len(s) <= maxBytes {
			return s, err
		}
		if !p.shedOne() {
			return "", fmt.Errorf("render evaluate prompt: %d bytes with every part that may be left out left out, "+
				"over %d", len(s), maxBytes)
		}
	}
}

// renderEvaluate is RenderEvaluatePrompt's one render of p.
func renderEvaluate(p EvaluatePrompt) (string, error) {
	if len(p.Outcomes) == 0 {
		return "", errors.New("render evaluate prompt: no outcome to evaluate")
	}
	if !runToken.MatchString(p.RunToken) {
		return "", fmt.Errorf("render evaluate prompt: run token %q is not %s", p.RunToken, runToken)
	}
	if p.Workspace != nil {
		return renderEvaluateWorkspace(p)
	}
	if p.Tools.BrowserEntry != "" && (!p.Tools.Browser || !v1alpha1.ValidBrowserEntry(p.Tools.BrowserEntry)) {
		return "", fmt.Errorf("render evaluate prompt: browser entry %q is not a path on a preview with a browser",
			p.Tools.BrowserEntry)
	}
	p.Components = boundEvaluateComponents(p.Components)
	p.Tools.LookAroundRoutes = min(max(p.Tools.LookAroundRoutes, 0), 8)
	v := newEvaluateView(p)
	v.Examples = evaluateExamples{Report: ExampleEvaluationReport}
	v.ExercisingCalls, v.OwnScripts = slotCalls(p.Tools)
	return render("prompt_evaluate.md.tmpl", v)
}

// slotCalls are the phrases naming a slot round's calls that use the
// application (every one acceptance.ValidCite admits that the round offers)
// and those that run a script or program of the evaluator's own (a page
// script, an exec program); own is "" when the round offers neither.
func slotCalls(t EvaluateTools) (exercising, own string) {
	verbs := []string{"`maul-tool http`"}
	var scripts []string
	if t.Browser {
		verbs = append(verbs, "`maul-tool browser`")
		scripts = append(scripts, "A page script you run with `eval`")
	}
	if t.Sandbox {
		verbs = append(verbs, "`maul-tool exec`")
		scripts = append(scripts, "a program you run with `maul-tool exec`")
	}
	if len(scripts) > 0 {
		own = strings.ToUpper(scripts[0][:1]) + scripts[0][1:]
		if len(scripts) == 2 {
			own += ", or " + scripts[1] + ","
		}
	}
	return orList(verbs), own
}

// orList joins items as "a", "a or b", "a, b or c".
func orList(items []string) string {
	if len(items) < 2 {
		return strings.Join(items, "")
	}
	return strings.Join(items[:len(items)-1], ", ") + " or " + items[len(items)-1]
}

// renderEvaluateWorkspace renders the evaluate prompt of a round on a
// Project with no preview (SPEC-S2): the two builds, files, exec and run,
// the placeholders, and that only what the evaluator saw at the head
// counts; no component, browser or look-around, which such a round never
// has.
func renderEvaluateWorkspace(p EvaluatePrompt) (string, error) {
	w := *p.Workspace
	if err := w.validate(); err != nil {
		return "", fmt.Errorf("render evaluate prompt: %w", err)
	}
	if p.Tools.Browser || p.Tools.BrowserEntry != "" {
		return "", errors.New("render evaluate prompt: a workspace has no browser")
	}
	p.Components, p.Tools.LookAroundRoutes = nil, 0
	v := newEvaluateView(p)
	v.WS = workspaceView{CLI: w.Surface == SurfaceCLI, Binary: w.Binary}
	if v.WS.CLI {
		v.WS.Build = buildSpan(w.Build)
		v.Examples = evaluateExamples{Report: ExampleCLIEvaluationReport}
	} else {
		v.Examples = evaluateExamples{Report: ExampleLibraryEvaluationReport}
	}
	return render("prompt_evaluate_workspace.md.tmpl", v)
}

// newEvaluateView is p as both evaluate prompts read it, every untrusted
// value rendered into the block that quotes it; the caller adds the
// examples.
func newEvaluateView(p EvaluatePrompt) evaluateView {
	v := evaluateView{
		EvaluatePrompt:  p,
		RequestBlock:    strings.Trim(untrusted.Visible(p.Request, RequestQuoteMaxBytes), "\n"),
		OutcomesBlock:   outcomesBlock(p.Outcomes),
		ChangedBlock:    changedBlock(p.ChangedPaths, p.shed.paths),
		LookAroundCalls: LookAroundToolCalls,
		ReportBy:        reportBy(p.Limits.MaxTurns),
		MaxCites:        acceptance.MaxCites,
	}
	v.OutcomeTurns = outcomeTurns(v.ReportBy, p.Outcomes)
	v.FixDiffBlock, v.FixDiffNote, v.FixDiffFilesBlock = fixDiffView(p.FixDiff, p.FixDiffCut)
	if p.PriorRound > 0 {
		v.PriorBlock = priorBlock(p.PriorVerdicts)
	}
	return v
}

// priorBlock lists a previous round's results as the prompt quotes them, in
// prose as the outcomes are (outcomesBlock): maul's result per outcome with
// its reason, bounded; "" for none.
func priorBlock(verdicts []PriorVerdict) string {
	var b strings.Builder
	for i, r := range verdicts {
		if i == acceptance.MaxOutcomes {
			break
		}
		if i == 0 {
			b.WriteString("Results:\n")
		}
		fmt.Fprintf(&b, "- %s was judged %s.", quotedLine(r.Outcome, 64), quotedLine(r.Result, 16))
		if r.Reason != "" {
			fmt.Fprintf(&b, " Why, quoted: %s", quotedLine(r.Reason, priorReasonMaxBytes))
		}
		b.WriteString("\n")
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// boundEvaluateComponents keeps the components with a valid key and a
// path, each on one line, at most evaluateComponentsMax.
func boundEvaluateComponents(in []EvaluateComponent) []EvaluateComponent {
	var out []EvaluateComponent
	for _, c := range in {
		c = EvaluateComponent{Key: oneLine(c.Key), Path: oneLine(c.Path)}
		if len(c.Key) > httpstep.MaxKeyLength || !httpstep.KeyPattern.MatchString(c.Key) ||
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

// outcomesBlock lists the outcomes as the prompt quotes them: numbered, in
// prose, never as YAML a model would carry into its report (S1 bug 5: the
// live evaluators copied this list's `outcome:` key into every verdict).
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
		judged := ""
		if o.Manual {
			judged = ", judged by a person"
		}
		fmt.Fprintf(&b, "%d. %s (a %s%s)\n   %s\n", i+1, quotedLine(o.ID, 64), quotedLine(string(kind), 16), judged,
			quotedLine(o.Outcome, acceptance.MaxOutcomeChars*4))
		if o.From != "" {
			fmt.Fprintf(&b, "   From the request, quoted: %s\n", quotedLine(o.From, acceptance.MaxFromChars*4))
		}
		if o.Manual && o.Reason != "" {
			fmt.Fprintf(&b, "   Why a person judges it, quoted: %s\n",
				quotedLine(o.Reason, acceptance.MaxReasonChars*4))
		}
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// changedBlock lists the changed paths, "+added -deleted path", at most
// ChangedPathsMax of them, then counts the rest and the shed ones left out
// to keep the prompt within its bound; "" for none.
func changedBlock(ps []ChangedPath, shed int) string {
	var lines []string
	for _, p := range ps {
		path := quotedLine(p.Path, changedPathMaxBytes)
		if path == "" {
			continue
		}
		lines = append(lines, fmt.Sprintf("+%d -%d %s", max(p.Added, 0), max(p.Deleted, 0), path))
	}
	more := max(len(lines)-ChangedPathsMax, 0) + shed
	if more > 0 {
		lines = append(lines[:min(len(lines), ChangedPathsMax)], "[and "+strconv.Itoa(more)+" more paths]")
	}
	return strings.Join(lines, "\n")
}

// The example report the evaluate prompt shows. A test holds it to
// report.ParseEvaluation, so the prompt never teaches a refused shape.
const (
	ExampleEvaluationReport = `---
verdicts:
  - outcome: "rating-moves"
    verdict: pass
    ledger: [14, 15, 22]
    note: "Two players I created both start at 1000; after their battle the winner shows 1016 and the loser 984."
  - outcome: "live-score"
    verdict: fail
    ledger: [31, 33]
    note: "The score stays 0-0 after I scored twice: seq 31 is the page after 10 s, and seq 33 the same on a reload."
  - outcome: "sounds-good"
    verdict: unsure
    ledger: []
    note: "Only a person can judge this."
  - outcome: "loser-emailed"
    verdict: unsure
    ledger: []
    note: "The clause 'emails the loser' could not be exercised: the preview sends no mail and no tool reads one."
anomalies:
  - summary: "The Back to battles link is dark blue on a dark background and barely readable."
    route: "/battles"
    ledger: [40, 41]
---

What I checked, in a few lines.
`
)

// ExamplePlanOutcomes is the outcome list the plan prompt shows, under the
// evaluator; a test holds it to report.ParsePlan. Each outcome models the
// prompt's rules: about items the person checking creates, one claim a
// check can show, concrete values, a layout outcome naming one fixed
// element and the numbers it must meet, a keep naming one action,
// and where a person sees it, and a rule the request states, with its
// example as one instance (G-B-LIB-2).
const ExamplePlanOutcomes = `acceptance:
  - id: rating-moves
    outcome: "After a battle of two players you create, /api/players rates the winner above 1000, the loser below."
    from: "ratings should update after every battle"
    kind: change
  - id: winner-listed-first
    outcome: "On /leaderboard, the winner of a battle between two players you create is listed above the loser."
    from: "ratings should update after every battle"
    kind: change
  - id: kick-pad-size
    outcome: "On /, the kick row's first step pad is at least 48 by 48 px."
    from: "make the step pads at least 48px"
    kind: change
  - id: tempo-shown
    outcome: "A beat you save at any tempo from 60 to 200 BPM shows that tempo on /beats, for example 90 bpm."
    from: "a beat shows its tempo, which can be anything from 60 to 200 BPM"
    kind: change
  - id: new-player-listed
    outcome: "A player you add on the players page (/players) is listed there by the name you gave."
    kind: keep
  - id: sounds-good
    outcome: "The new drum kit sounds good."
    from: "make the drums punchier"
    manual: true
    reason: "only a person can judge how it sounds"`

// OutcomesExample is ExamplePlanOutcomes, for the plan prompt's template.
func (PlanPrompt) OutcomesExample() string { return ExamplePlanOutcomes }

// Viewport is the fixed window every browser session opens at, for the
// plan prompt's template.
func (PlanPrompt) Viewport() toolwire.Viewport { return toolwire.DefaultViewport }
