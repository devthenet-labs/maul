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
	"time"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/checks"
	"github.com/devthenet-labs/maul/internal/checks/playwright"
	"github.com/devthenet-labs/maul/internal/httpstep"
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
	// priorReplaysMax bounds the previous round's replays shown: an
	// intent's saved checks (v1alpha1.MaxSavedChecks); priorReasonMaxBytes
	// one prior verdict's reason (the Intent's schema bound).
	priorReplaysMax     = 40
	priorReasonMaxBytes = 600
)

// PriorRefusedMax bounds the refused checks the evaluate prompt shows (C3):
// half the reviews an intent caches. The controller hands over at most the
// newest this many.
const PriorRefusedMax = 40

// HeldAnomaliesMax bounds the anomalies held at the heads the evaluate
// prompt shows (G-B-LIB-1): every one the Intent records. heldSummaryMaxBytes
// and heldRouteMaxBytes bound each one's words, its schema's.
const (
	HeldAnomaliesMax    = 10
	heldSummaryMaxBytes = 600
	heldRouteMaxBytes   = 300
)

// NotYetProofMax bounds the saved checks not yet proof of a pass the
// evaluate prompt names (gate D-B9): every check a store holds.
const NotYetProofMax = v1alpha1.MaxSavedChecks

// GreenRubric is the check reviewer's rule for a check confirmed green, word
// for word: rendered into every green review and into the evaluator's
// prompt, so the evaluator writes its checks to the rule they are judged by
// (coverage design B1). A test pins both.
const GreenRubric = "Answer `proves` only if a pass of this check really shows the outcome holds: it does what " +
	"the outcome describes, on data it created itself, and it would fail if the outcome did not hold. Answer " +
	"`does_not_prove` if it could pass while the outcome does not hold: it checks something else or only part of " +
	"the outcome, it asserts only that a call succeeded or a page loaded, it is keyed to fixed values that an " +
	"implementation could special-case, or it depends on data it did not create. A check that leaves out, " +
	"replaces or alters anything the outcome's words name (a field of a body it says to send, a step, a value, a " +
	"page or a surface) tests less than the outcome: answer `does_not_prove`, even if what it does test is a " +
	"reasonable reading of the outcome. Answer `does_not_prove` too for a check that a plausible wrong build " +
	"would still pass: one " +
	"that shows something is left out, refused or absent only by asserting what is there (it must create the " +
	"item to be left out and assert that item is not there), one that compares only some of what the outcome " +
	"says must match (a few items of a list it says is the same), one whose inputs make the expected result the " +
	"one a fixed or default answer gives (a new, empty item where the outcome's value depends on what is in " +
	"it), one that tries a rule the outcome states only on the example the outcome or the request names (a " +
	"build right only on that example would pass it), and one whose program can print what its `expect` looks " +
	"for and exit as it expects whatever it " +
	"observed. Answer `unclear` if you cannot tell from what is shown."

// LayoutReadRule is the green rule's one exception, for a layout read
// (checks.LayoutRead): a reviewer is shown it for such a check, the
// evaluator always (coverage design B4), so it is worded in the third
// person to read in both. It mirrors the red question's layout exemption;
// such a check's failure is still held for a person.
const LayoutReadRule = "A browser check that writes nothing and asserts only the layout of the page's fixed " +
	"markup, at least once by a measure (size, left-of, above, no-overlap, equal-size, aligned or legible), on " +
	"elements it finds by role, label, test id or CSS rather than by their text or position, reads the same markup " +
	"on every run: it needs no data it created, and is not refused for that, unless what it measures is a row or " +
	"record that other runs make, or is sized or placed by them."

// GreenRubric and LayoutReadRule, wrapped as the templates are, for them.
func (evaluateView) GreenRubric() string       { return wrap(GreenRubric, "", "") }
func (evaluateView) LayoutReadRule() string    { return wrap(LayoutReadRule, "", "") }
func (checkReviewView) GreenRubric() string    { return wrap(GreenRubric, "", "") }
func (checkReviewView) LayoutReadRule() string { return wrap(LayoutReadRule, "", "") }

// runToken is the exploration token's grammar: what the prompt states
// as maul's own, outside any fence, so nothing but this can pass.
var runToken = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// EvaluatePrompt is the data for an intent's evaluate-stage prompt: judge
// the approved outcomes against the running preview with maul-tool, save
// checks for them, look around what the change touched, and report.
//
// Request, Outcomes, ChangedPaths, FixDiff, PriorVerdicts, PriorReplay,
// HeldAnomalies, PriorRefused and NotYetProof are UNTRUSTED (DESIGN 4.4):
// the issue anyone may have written, the plan's
// outcome text, paths and diff text from the pull request, and a previous
// round's results, whose reasons may quote its evaluator. RenderEvaluatePrompt shows each only
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
	// after a fix; "" omits the section.
	FixDiff string
	// PriorRound is the last judged round before this one, and
	// PriorVerdicts and PriorReplay its results (SPEC Q15): maul's result
	// per outcome, and how each saved check replayed at its start. Zero, or
	// neither, omits the section.
	PriorRound    int
	PriorVerdicts []PriorVerdict
	PriorReplay   []PriorReplay
	// HeldAnomalies are the look-around anomalies earlier rounds found at
	// exactly these heads that no approver accepted (G-B-LIB-1): each holds
	// the change from being confirmed until the heads move or an approver
	// accepts it, whatever this round finds, and the prompt asks for each
	// to be examined again. None omits the section.
	HeldAnomalies []HeldAnomaly
	// PriorRefused are the checks the reviewer refused in earlier rounds,
	// with its reasons (coverage design C3); none omits the section.
	PriorRefused []PriorRefusal
	// NotYetProof are the saved checks saved to show a defect that no
	// review has yet accepted as proof their outcome holds (gate D-B9): the
	// prompt asks for a new check of each one's outcome even when it
	// replays green. None omits the section.
	NotYetProof []NotYetProof
}

// NotYetProof is one saved check saved red that is not yet proof of a
// pass, by its id and outcome.
type NotYetProof struct {
	Check, Outcome string
}

// HeldAnomaly is one look-around anomaly an earlier round's evaluator
// reported at this round's heads: its round, route and words.
type HeldAnomaly struct {
	Round          int
	Summary, Route string
}

// PriorRefusal is one check the reviewer refused in an earlier round, by
// its id and outcome, with the reviewer's reason.
type PriorRefusal struct {
	Check, Outcome, Reason string
}

// PriorVerdict is maul's result of one outcome in a previous round, with
// why it did not pass.
type PriorVerdict struct {
	Outcome, Result, Reason string
}

// PriorReplay is how one saved check, bound to Outcome, replayed in a
// previous round.
type PriorReplay struct {
	Check, Outcome, Result string
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
	// BrowserEntry is the path the browser opens first; "" leaves it to the
	// evaluator. Else it must be v1alpha1.ValidBrowserEntry.
	BrowserEntry string
	// Sandbox offers maul-tool exec and exec checks.
	Sandbox bool
	// MaxToolCalls is the run's tool-call bound.
	MaxToolCalls int
	// LookAroundRoutes bounds the routes the look-around visits; zero
	// skips it.
	LookAroundRoutes int
}

// Workspace surfaces, as EvaluateWorkspace and ReviewWorkspace name them.
const (
	SurfaceCLI     = "cli"
	SurfaceLibrary = "library"
)

// EvaluateWorkspace is what the agents-namespace tool pod built (SPEC-S2):
// the Project's surface, maul's own and stated as fact. A cli names its
// Build argv and its Binary (v1alpha1.ValidCLIBuild, ValidCLIBinary); a
// library its Language, go (checks.LanguageGo), and no build.
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
		if w.Language != checks.LanguageGo || w.Binary != "" || len(w.Build) > 0 {
			return fmt.Errorf("a library workspace names language %s and no build or binary", checks.LanguageGo)
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

// The workspace placeholders (SPEC-S2 Q23), for the templates.
func (evaluateView) BinPlaceholder() string     { return checks.BinPlaceholder }
func (evaluateView) TreePlaceholder() string    { return checks.TreePlaceholder }
func (evaluateView) RunPlaceholder() string     { return checks.RunPlaceholder }
func (checkReviewView) BinPlaceholder() string  { return checks.BinPlaceholder }
func (checkReviewView) TreePlaceholder() string { return checks.TreePlaceholder }

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
	PriorBlock    string
	HeldBlock     string
	RefusedBlock  string
	NotYetBlock   string
	Examples      evaluateExamples
	// WS is the workspace, on a round with no preview.
	WS workspaceView
	// LookAroundCalls is LookAroundToolCalls, for the template.
	LookAroundCalls int
	// ReportBy is the turn by which the prompt asks for the report
	// (reportBy).
	ReportBy int
	// OutcomeTurns is the turns the prompt budgets for each outcome a person
	// does not judge and no saved check covers (outcomeTurns).
	OutcomeTurns int
}

// outcomeTurns shares the turns before the report, less the two that start
// a round, among the outcomes a person does not judge and no saved check
// covers (replayedGreen), an outcome with a saved check that is not yet
// proof of a pass counted uncovered (gate D-B9): at least 1 each. It is
// advice only, so an outcome whose check fails this round after all is
// still judged.
func outcomeTurns(reportBy int, os []acceptance.PlanOutcome, replays []PriorReplay, notYet []NotYetProof) int {
	green := replayedGreen(replays)
	for _, c := range notYet {
		delete(green, c.Outcome)
	}
	n := 0
	for _, o := range os {
		if !o.Manual && !green[o.ID] {
			n++
		}
	}
	return max(1, (reportBy-2)/max(n, 1))
}

// replayedGreen is the outcomes whose saved checks all replayed, at least
// one, and every one passed.
func replayedGreen(replays []PriorReplay) map[string]bool {
	green := map[string]bool{}
	for _, r := range replays {
		pass := r.Result == string(v1alpha1.CheckRunPass)
		if seen, ok := green[r.Outcome]; ok {
			pass = pass && seen
		}
		green[r.Outcome] = pass
	}
	return green
}

// ExecMarker is checks.ExecMarkerPlaceholder, for the templates: a line of an
// exec check's output that starts with what it stands for fails the check
// (checks.ExecMarked).
func (evaluateView) ExecMarker() string    { return checks.ExecMarkerPlaceholder }
func (checkReviewView) ExecMarker() string { return checks.ExecMarkerPlaceholder }

// ExecAssertionIDMax is checks.MaxExecAssertionIDLen, for the templates:
// a marker naming a longer id is no assertion (gate D-B2).
func (evaluateView) ExecAssertionIDMax() int { return checks.MaxExecAssertionIDLen }

// ExecTimeoutMax is checks.MaxExecTimeout, for the templates, as a check
// would write it ("1m"): the most time a check may claim, the exec shim's
// own bound on one program.
func (evaluateView) ExecTimeoutMax() string {
	return strings.TrimSuffix(strings.TrimSuffix(checks.MaxExecTimeout.String(), "0s"), "0m")
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

// evaluateExamples are the check and report examples the prompt shows,
// which the package's tests hold to the grammar and the report parser.
type evaluateExamples struct {
	HTTP, Browser, Exec, Report string
	// Run is a workspace's run check example.
	Run string
}

// RenderEvaluatePrompt renders the evaluate-stage prompt.
func RenderEvaluatePrompt(p EvaluatePrompt) (string, error) {
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
	v.Examples = evaluateExamples{HTTP: ExampleHTTPCheck, Browser: ExampleBrowserCheck, Exec: ExampleExecCheck,
		Report: ExampleEvaluationReport}
	return render("prompt_evaluate.md.tmpl", v)
}

// renderEvaluateWorkspace renders the evaluate prompt of a round on a
// Project with no preview (SPEC-S2): the two builds, files, exec and run,
// the placeholders and the head-and-base rule; no component, browser or
// look-around, which such a round never has.
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
		v.Examples = evaluateExamples{Exec: ExampleCLIExecCheck, Run: ExampleCLIRunCheck,
			Report: ExampleCLIEvaluationReport}
	} else {
		v.Examples = evaluateExamples{Run: ExampleLibraryRunCheck, Report: ExampleLibraryEvaluationReport}
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
		ChangedBlock:    changedBlock(p.ChangedPaths),
		LookAroundCalls: LookAroundToolCalls,
		ReportBy:        reportBy(p.Limits.MaxTurns),
	}
	v.OutcomeTurns = outcomeTurns(v.ReportBy, p.Outcomes, p.PriorReplay, p.NotYetProof)
	if strings.TrimSpace(p.FixDiff) != "" {
		v.FixDiffBlock = strings.Trim(untrusted.Visible(p.FixDiff, FixDiffMaxBytes), "\n")
	}
	if p.PriorRound > 0 {
		v.PriorBlock = priorBlock(p.PriorVerdicts, p.PriorReplay)
	}
	v.HeldBlock = heldBlock(p.HeldAnomalies)
	v.RefusedBlock = refusedBlock(p.PriorRefused)
	v.NotYetBlock = notYetBlock(p.NotYetProof)
	return v
}

// heldBlock lists the anomalies earlier rounds found at these heads, each
// with its round, route and words, at most HeldAnomaliesMax; "" for none.
func heldBlock(as []HeldAnomaly) string {
	var b strings.Builder
	for i, a := range as {
		if i == HeldAnomaliesMax {
			break
		}
		fmt.Fprintf(&b, "- round %d", a.Round)
		if a.Route != "" {
			fmt.Fprintf(&b, ", at %s", quotedLine(a.Route, heldRouteMaxBytes))
		}
		fmt.Fprintf(&b, ": %s\n", quotedLine(a.Summary, heldSummaryMaxBytes))
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// notYetBlock lists the saved checks that are not yet proof of a pass, each
// by its id and outcome, at most NotYetProofMax; "" for none.
func notYetBlock(cs []NotYetProof) string {
	var b strings.Builder
	for i, c := range cs {
		if i == NotYetProofMax {
			break
		}
		fmt.Fprintf(&b, "- check %s, of outcome %s\n", quotedLine(c.Check, 72), quotedLine(c.Outcome, 64))
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// refusedBlock lists the checks the reviewer refused, as priorBlock lists a
// round's results: each by its id and outcome with the reviewer's reason,
// at most PriorRefusedMax; "" for none.
func refusedBlock(rs []PriorRefusal) string {
	var b strings.Builder
	for i, r := range rs {
		if i == PriorRefusedMax {
			break
		}
		fmt.Fprintf(&b, "- check %s, of outcome %s, was refused.", quotedLine(r.Check, 72), quotedLine(r.Outcome, 64))
		if r.Reason != "" {
			fmt.Fprintf(&b, " Why, quoted: %s", quotedLine(r.Reason, priorReasonMaxBytes))
		}
		b.WriteString("\n")
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// priorBlock lists a previous round's results as the prompt quotes them, in
// prose as the outcomes are (outcomesBlock): maul's result per outcome with
// its reason, then each saved check's replay, each list bounded; "" for
// neither.
func priorBlock(verdicts []PriorVerdict, replays []PriorReplay) string {
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
	for i, r := range replays {
		if i == priorReplaysMax {
			break
		}
		if i == 0 {
			b.WriteString("Replayed at the start of that round:\n")
		}
		fmt.Fprintf(&b, "- check %s, of outcome %s, replayed %s\n", quotedLine(r.Check, 72),
			quotedLine(r.Outcome, 64), quotedLine(r.Result, 16))
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
	// From is the line of the request the outcome answers, and Kind the
	// outcome's kind (empty is change). Request is the intent's request, as
	// the evaluator is given it, and Components the previewed components
	// (coverage design C1): context, so a reviewer judges the application's
	// own words by the request. From and Request are UNTRUSTED, quoted as
	// data; empty ones, and no components, leave their sections out.
	From       string
	Kind       acceptance.OutcomeKind
	Request    string
	Components []EvaluateComponent
	// Check is the check under review.
	Check checks.Check
	// ExpectFail asks the red question: the check failed on the build under
	// review, at step FailedStep (1-based), by its assertion FailedAssertion
	// (toolwire.StepRecord.AssertionID), and the evaluator proposes it to
	// show that the outcome does not hold.
	ExpectFail      bool
	FailedStep      int
	FailedAssertion string
	// Observed is, for the red question about a browser text assertion,
	// the text its element showed in each of maul's runs, the run's token
	// written as checks.RunPlaceholder (toolwire.StepRecord.Received, D-B4):
	// UNTRUSTED, the application's own text, quoted as data. Empty when maul
	// kept none (no element, a text it could not read whole), and then the
	// red question is asked exactly as before.
	Observed []string
	// Workspace is a check of a Project with no preview (SPEC-S2): an exec
	// or run check, reviewed beside maul's own record of the build and of
	// its runs at the head and the base. nil is a preview's check, the
	// prompt exactly as before.
	Workspace *ReviewWorkspace
}

// ReviewWorkspace is what maul recorded of a check under review on a
// Project with no preview, all maul's own and stated as fact: the surface
// and its build, whether the base built, and the check's confirmation runs
// at each side.
type ReviewWorkspace struct {
	EvaluateWorkspace
	// BaseUnbuildable: the base did not build, so the check never ran
	// there.
	BaseUnbuildable bool
	// Head and Base are the check's runs at each side.
	Head, Base ReviewRun
}

// ReviewRun is one side's run of the check under review: Result a
// v1alpha1.CheckRunResult ("" for none), for a fail the assertion
// (toolwire.StepRecord.AssertionID) it was red by, "" for none, and for an
// error the v1alpha1.CheckRunCause maul recorded, "" for none.
type ReviewRun struct {
	Result, Assertion, Cause string
}

// reviewCauses are the causes of an error run in maul's words; one not
// listed is not shown.
var reviewCauses = map[v1alpha1.CheckRunCause]string{
	v1alpha1.CheckRunCauseCompile:            "the program did not compile against the tree",
	v1alpha1.CheckRunCauseVendoredTree:       "the tree vendors its modules, which a run program cannot build against",
	v1alpha1.CheckRunCauseReplaceOutsideTree: "the tree replaces a module with a directory outside it",
	v1alpha1.CheckRunCauseNoModule:           "the tree has no go.mod a run program can build against",
	v1alpha1.CheckRunCauseBaseUnbuildable:    "the base did not build",
}

// sentence is r as maul states it, after "it ran at the head" or "at the
// base"; an unknown result is unknown, never the text it carried.
func (r ReviewRun) sentence() string {
	switch v1alpha1.CheckRunResult(r.Result) {
	case v1alpha1.CheckRunPass:
		return "passed"
	case v1alpha1.CheckRunFail:
		if a := quotedLine(r.Assertion, 512); a != "" {
			return "failed by its assertion " + code(a)
		}
		return "failed without an assertion: by its exit code, a crash or missing output"
	case v1alpha1.CheckRunError:
		if why, ok := reviewCauses[v1alpha1.CheckRunCause(r.Cause)]; ok {
			return "could not build or run: an error, not a result (" + why + ")"
		}
		return "could not build or run: an error, not a result"
	case v1alpha1.CheckRunQuarantined:
		return "passed only on retry"
	case v1alpha1.CheckRunRefused:
		return "was refused by maul"
	case "":
		return "did not run"
	}
	return "ended in a result maul does not know"
}

// reviewWorkspaceView is ReviewWorkspace as the template reads it.
type reviewWorkspaceView struct {
	workspaceView
	Head, Base string
	// NoBase: the base did not build.
	NoBase bool
}

// ExampleCheckReviewReport is the report the check reviewer's prompt
// shows, which its repair restates; a test decodes it with
// report.ParseCheckReview, so the prompt never teaches a refused shape (S1
// bug 8: a live reviewer left its frontmatter open).
const ExampleCheckReviewReport = `---
answer: proves
reason: "It creates its own battle, votes on it and asserts the rating moved by the stake shown."
---
`

// ReportExample is ExampleCheckReviewReport, for the template.
func (checkReviewView) ReportExample() string { return ExampleCheckReviewReport }

// checkReviewView is CheckReviewPrompt as the template reads it.
type checkReviewView struct {
	CheckReviewPrompt
	OutcomeBlock string
	FromBlock    string
	RequestBlock string
	// KindSentence states the outcome's kind: keep, else change.
	KindSentence string
	Form         string
	CheckBlock   string
	Files        []reviewFile
	// Assertion is FailedAssertion made one legible line; Marker, for an
	// exec check, the marker line its program printed, up to the colon
	// after the id.
	Assertion, Marker string
	// Layout: a browser check that writes no data of its own run, failing
	// a measured layout assertion (checks.FailsLayout, S1 demo bug 9): the
	// red question asks about the page's layout, not data it created.
	Layout bool
	// LayoutRead: a green question about a layout read (checks.LayoutRead):
	// the green rule's layout exception is shown beside it.
	LayoutRead bool
	// ObservedBlock is Observed as the reviewer reads it, one line per run;
	// RunScoped, with it, a text assertion on an element the check finds by
	// its own run data (checks.FailsRunScopedText) that showed that run's
	// token in every run: other runs' rows on the page cannot be that
	// element (D-B4).
	ObservedBlock string
	RunScoped     bool
	// Viewport is the fixed window a browser check runs in.
	Viewport playwright.Viewport
	// WS is the workspace, for a check of a Project with no preview.
	WS *reviewWorkspaceView
}

// reviewFile is one exec program file as the reviewer reads it.
type reviewFile struct {
	Path, Content string
	Executable    bool
}

// RenderCheckReviewPrompt renders the check reviewer's prompt.
func RenderCheckReviewPrompt(p CheckReviewPrompt) (string, error) {
	shown, files := reviewedCheck(p.Check)
	ws, err := reviewWorkspace(p.Workspace, shown.Form)
	if err != nil {
		return "", fmt.Errorf("render check review prompt: %w", err)
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
		Assertion:         quotedLine(p.FailedAssertion, 512),
		Viewport:          playwright.DefaultViewport,
		FromBlock:         quotedLine(p.From, acceptance.MaxFromChars*4),
		RequestBlock:      strings.Trim(untrusted.Visible(p.Request, RequestQuoteMaxBytes), "\n"),
		KindSentence:      "It is a change: something the change makes true.",
	}
	if p.ExpectFail {
		// The red question keeps to the outcome as approved: a request
		// that says more than its narrowed outcome must not push a
		// reviewer toward a fix outside that contract (coverage review).
		v.FromBlock, v.RequestBlock = "", ""
	}
	if p.Kind == acceptance.OutcomeKindKeep {
		v.KindSentence = "It is a keep: something the change must leave true."
	}
	v.Components = boundEvaluateComponents(p.Components)
	if v.WS = ws; ws != nil {
		v.Components = nil
	}
	if id, ok := strings.CutPrefix(p.FailedAssertion, "marker "); ok &&
		(shown.Form == checks.FormExec || shown.Form == checks.FormRun) {
		v.Marker = checks.ExecMarkerPlaceholder + " " + quotedLine(id, checks.MaxExecAssertionIDLen) + ":"
	}
	v.Layout = p.ExpectFail && !checks.WritesRunData(p.Check) && checks.FailsLayout(p.Check, p.FailedStep-1)
	v.LayoutRead = !p.ExpectFail && checks.LayoutRead(p.Check)
	if p.ExpectFail && ws == nil && !v.Layout && shown.Form == checks.FormBrowser {
		v.ObservedBlock = observedBlock(p.Observed)
		v.RunScoped = v.ObservedBlock != "" && checks.FailsRunScopedText(p.Check, p.FailedStep-1) &&
			!slices.ContainsFunc(p.Observed, func(o string) bool { return !strings.Contains(o, checks.RunPlaceholder) })
	}
	return render("prompt_checkreview.md.tmpl", v)
}

// observedMax bounds the runs and the bytes of each run's text a reviewer
// is shown: a red check's two attempts, each toolwire.MaxReceivedBytes
// with room for its token written back.
const (
	observedRunsMax  = 2
	observedBytesMax = 2 << 10
)

// observedBlock is a red check's observed texts as the reviewer reads them,
// one escaped line per run; "" unless every run has one.
func observedBlock(observed []string) string {
	if len(observed) == 0 || len(observed) > observedRunsMax {
		return ""
	}
	lines := make([]string, 0, len(observed))
	for i, o := range observed {
		q := quotedLine(o, observedBytesMax)
		if q == "" {
			return ""
		}
		lines = append(lines, "run "+strconv.Itoa(i+1)+": "+q)
	}
	return strings.Join(lines, "\n")
}

// reviewWorkspace is w as the check review template reads it, for a check
// of form f: a preview's check is http, browser or exec, with no
// workspace; a workspace's is exec (a cli's) or run, with a valid one.
func reviewWorkspace(w *ReviewWorkspace, f checks.Form) (*reviewWorkspaceView, error) {
	if w == nil {
		if f != checks.FormHTTP && f != checks.FormBrowser && f != checks.FormExec {
			return nil, fmt.Errorf("check form %q is not http, browser or exec", f)
		}
		return nil, nil
	}
	if err := w.validate(); err != nil {
		return nil, err
	}
	cli := w.Surface == SurfaceCLI
	if f != checks.FormRun && (f != checks.FormExec || !cli) {
		return nil, fmt.Errorf("check form %q is not one a %s workspace runs", f, w.Surface)
	}
	v := &reviewWorkspaceView{workspaceView: workspaceView{CLI: cli, Binary: w.Binary},
		Head: w.Head.sentence(), Base: w.Base.sentence(), NoBase: w.BaseUnbuildable}
	if cli {
		v.Build = buildSpan(w.Build)
	}
	return v, nil
}

// shownCheck is a check as a reviewer is shown it: its form and body,
// never its id or its outcome's.
type shownCheck struct {
	Form    checks.Form          `json:"form"`
	HTTP    *checks.HTTPCheck    `json:"http,omitempty"`
	Browser *checks.BrowserCheck `json:"browser,omitempty"`
	Exec    *checks.ExecCheck    `json:"exec,omitempty"`
	Run     *shownRun            `json:"run,omitempty"`
}

// shownRun is a run check as a reviewer is shown it: its program files are
// shown apart, each whole.
type shownRun struct {
	Language string            `json:"language"`
	Args     []string          `json:"args,omitempty"`
	Timeout  string            `json:"timeout,omitempty"`
	Expect   checks.ExecExpect `json:"expect"`
}

// reviewedCheck is c as a reviewer is shown it: a deep copy without its id,
// its outcome id or any http step's name, and with an exec or run check's
// program files taken out to be shown whole, each in its own block.
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
		files = reviewFiles(e.Files)
		e.Files = nil
		shown.Exec = &e
	case c.Run != nil:
		files = reviewFiles(c.Run.Files)
		shown.Run = &shownRun{Language: c.Run.Language, Args: slices.Clone(c.Run.Args), Timeout: c.Run.Timeout,
			Expect: c.Run.Expect}
	}
	return shown, files
}

// reviewFiles are a program's files as a reviewer reads them, each whole.
func reviewFiles(fs []checks.File) []reviewFile {
	files := make([]reviewFile, 0, len(fs))
	for _, f := range fs {
		files = append(files, reviewFile{Path: quotedLine(f.Path, 300),
			Content:    strings.Trim(untrusted.Visible(f.Content, checks.MaxProgramBytes), "\n"),
			Executable: f.Executable})
	}
	return files
}

// The examples the evaluate prompt shows: one check of each form and a
// report. Tests hold each check to checks.Validate and the report to
// report.ParseEvaluation, so the prompt never teaches a refused shape.
const (
	ExampleHTTPCheck = `{
  "id": "rating-moves-api",
  "outcome": "rating-moves",
  "form": "http",
  "http": {
    "target": "api",
    "steps": [
      {"request": {"method": "POST", "path": "/api/players", "json": {"name": "{{run}}-alice"}},
       "expect": {"status": 201}, "capture": {"alice": "/id"}},
      {"request": {"method": "POST", "path": "/api/players", "json": {"name": "{{run}}-bob"}},
       "expect": {"status": 201}, "capture": {"bob": "/id"}},
      {"request": {"method": "GET", "path": "/api/players/{{capture.alice}}"},
       "expect": {"status": 200, "json": {"/name": "{{run}}-alice", "/rating": 1000}}},
      {"request": {"method": "GET", "path": "/api/players/{{capture.bob}}"},
       "expect": {"status": 200, "json": {"/name": "{{run}}-bob", "/rating": 1000}}},
      {"request": {"method": "POST", "path": "/api/battles",
                   "json": {"winner": "{{capture.bob}}", "loser": "{{capture.alice}}"}},
       "expect": {"status": 201}},
      {"request": {"method": "GET", "path": "/api/players/{{capture.bob}}"},
       "expect": {"status": 200, "json": {"/rating": {"$range": [1001, 3000]}}}},
      {"request": {"method": "GET", "path": "/api/players/{{capture.alice}}"},
       "expect": {"status": 200, "json": {"/rating": {"$range": [0, 999]}}}}
    ]
  }
}`
	ExampleBrowserCheck = `{
  "id": "winner-listed-first-page",
  "outcome": "winner-listed-first",
  "form": "browser",
  "browser": {
    "steps": [
      {"goto": "/players"},
      {"fill": {"locator": {"label": "Player name"}, "text": "{{run}}-alice"}},
      {"click": {"role": "button", "name": "Add player"}},
      {"fill": {"locator": {"label": "Player name"}, "text": "{{run}}-bob"}},
      {"click": {"role": "button", "name": "Add player"}},
      {"goto": "/battle"},
      {"select": {"label": "Winner", "option": "{{run}}-bob"}},
      {"select": {"label": "Loser", "option": "{{run}}-alice"}},
      {"click": {"role": "button", "name": "Rate"}},
      {"goto": "/leaderboard"},
      {"expect": {"assert": "above", "locator": {"role": "row", "hasText": "{{run}}-bob"},
                  "other": {"role": "row", "hasText": "{{run}}-alice"}}},
      {"goto": "/battle"},
      {"select": {"label": "Winner", "option": "{{run}}-alice"}},
      {"select": {"label": "Loser", "option": "{{run}}-bob"}},
      {"click": {"role": "button", "name": "Rate"}},
      {"goto": "/leaderboard"},
      {"expect": {"assert": "above", "locator": {"role": "row", "hasText": "{{run}}-alice"},
                  "other": {"role": "row", "hasText": "{{run}}-bob"}}},
      {"expect": {"assert": "legible", "locator": {"role": "row", "hasText": "{{run}}-alice"}}}
    ]
  }
}`
	ExampleExecCheck = `{
  "id": "live-score-socket",
  "outcome": "live-score",
  "form": "exec",
  "exec": {
    "argv": ["python3", "client.py", "{{run}}"],
    "files": [{"path": "client.py",
               "content": "import json, sys\n# judge once; on a fail print {{marker}} score-shown: <what>\n"}],
    "timeout": "30s",
    "expect": {"exit": 0, "stdoutJSON": {"/player": "{{run}}-alice"}}
  }
}`
	ExampleEvaluationReport = `---
verdicts:
  - outcome: "rating-moves"
    verdict: pass
    ledger: [14, 15, 22]
    note: "Both players start at 1000; after the battle the winner is at 1016 and the loser at 984."
  - outcome: "live-score"
    verdict: fail
    ledger: [31, 33]
    note: "The score socket closes before the first update; see the screenshot in 33."
  - outcome: "sounds-good"
    verdict: unsure
    ledger: []
    note: "Only a person can judge this."
  - outcome: "loser-emailed"
    verdict: unsure
    ledger: []
    note: "The clause 'emails the loser' could not be exercised: the preview sends no mail and no tool reads one."
  - outcome: "profile-round-trip"
    verdict: unsure
    ledger: []
    contrary: [41]
    note: "Posting back the exact profile GET returned is a 400 (seq 41), on a field this change did not touch."
disputes:
  - check_digest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
    reason: "It asserts the whole leaderboard's first row, which data from earlier rounds can take."
    replacement: "rating-moves-api"
    ledger: [3]
proposed:
  - id: "rating-moves-api"
    outcome: "rating-moves"
    file: "/work/scratch/checks/rating-moves-api.json"
  - id: "live-score-socket"
    outcome: "live-score"
    file: "/work/scratch/checks/live-score-socket.json"
    expect: fail
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

// Viewport is the fixed window a browser check runs in, for the plan
// prompt's template.
func (PlanPrompt) Viewport() playwright.Viewport { return playwright.DefaultViewport }
