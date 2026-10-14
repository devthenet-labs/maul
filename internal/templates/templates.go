// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"embed"
	"fmt"
	"strings"
	"text/template"
	"unicode"
	"unicode/utf8"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/report"
)

//go:embed *.md.tmpl
var files embed.FS

// tmpl parses every embedded template once; a parse failure is a programmer
// error caught by the package's golden tests, so panicking at init is right.
var tmpl = template.Must(template.New("").
	Funcs(template.FuncMap{"join": strings.Join, "code": code, "fence": fence, "chomp": chomp,
		"openPullRequests": openPullRequestsListing}).
	ParseFS(files, "*.md.tmpl"))

// chomp drops trailing line breaks, so a block ending in one (fence) can sit
// flush against the template text that follows it.
func chomp(s string) string { return strings.TrimRight(s, "\r\n") }

func render(name string, data any) (string, error) {
	var b strings.Builder
	if err := tmpl.ExecuteTemplate(&b, name, data); err != nil {
		return "", fmt.Errorf("render %s: %w", name, err)
	}
	return b.String(), nil
}

// NotReproducedMarker opens the warning a pull request body carries when the
// remediation did not show a reproduction of the bug.
const NotReproducedMarker = "patchy could not reproduce this bug before fixing it"

// PRBody renders the pull-request body for a remediation branch; issue is
// the tracking issue number ("Fixes #issue" auto-links and auto-closes).
// untested marks a run on patchy's default agent image, which carries none
// of the repository's toolchain: the body then warns, ahead of the report,
// that patchy did not build or test the change.
//
// repro is the agent's account of reproducing the bug with its regression
// test (agentresult.Reproduction). The body states it plainly, as the
// agent's claim: that the agent reports it reproduced the bug, with the
// command and the before and after excerpts; or, when the agent says it did
// not or the report records nothing (nil), a warning that patchy could not
// reproduce the bug before fixing it.
//
// The evidence is agent text, so it is sanitised for the markdown reading
// (the command inline, each excerpt as a code block no line of it can
// close) and then defanged for the plain one: a repository may have GitHub
// copy the body into the squash commit, where inline code protects nothing
// and a "closes #12" quoted from test output would close issue 12.
func PRBody(issue int, reportBody string, untested bool, repro *v1alpha1.Reproduction) (string, error) {
	type reproduction struct {
		Reproduced             bool
		Command, Before, After string
	}
	var r *reproduction
	if repro != nil {
		r = &reproduction{Reproduced: repro.Reproduced}
		if c := strings.TrimSpace(repro.Command); c != "" {
			r.Command = defang(SanitizeInline(code(c)))
		}
		r.Before = evidenceBlock(repro.Before)
		r.After = evidenceBlock(repro.After)
	}
	return render("pr_body.md.tmpl", struct {
		Issue               int
		Report              string
		Untested            bool
		GuideURL            string
		Reproduction        *reproduction
		NotReproducedMarker string
	}{issue, reportBody, untested, AgentImageGuideURL, r, NotReproducedMarker})
}

// evidenceBlock renders a quoted excerpt of test output as a text code block
// nothing in it can close, sanitised and defanged; "" when there is none.
func evidenceBlock(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return defang(chomp(Sanitize(fence(s))))
}

// InvestigatePrompt is the data for the analysis-stage prompt (stage 1:
// exploitability/likelihood/impact plus the verdict).
type InvestigatePrompt struct {
	IssuePath     string
	ReportPath    string
	AllowedModels []string
	// ReadOnlyEnforced says the stage's harness itself holds it read-only (no
	// shell, writes refused outside the report's directory), so the prompt
	// can state that tool surface as fact. Without it the prompt only asks
	// for the posture, which is all a harness that leaves it to the pod can
	// truthfully say.
	ReadOnlyEnforced bool
	// AutoMaxTurns/AutoTokenBudget is what a remediation gets unattended, and
	// the line past which an estimate needs human approval — NOT a cap.
	AutoMaxTurns    int
	AutoTokenBudget int
	// ManualMaxTurns/ManualTokenBudget is the most a human-approved run can
	// get. Rendered so the agent knows asking beyond it buys nothing.
	ManualMaxTurns    int
	ManualTokenBudget int
	// Calibration is how earlier estimates compared to reality; nil omits the
	// section (a cold start has nothing honest to say).
	Calibration *Calibration
	// PreviousAttempt is the failed investigation this one retries; nil
	// omits the section.
	PreviousAttempt *PreviousAttempt
}

// Calibration reports how previous remediations compared to what the
// investigation predicted, so the next estimate can correct for the observed
// skew. It is derived from a FindingRollup's remediation stage aggregate and
// carried to the pod as JSON — the runner has no Kubernetes access, so this
// travels as configuration like every other fact it needs.
type Calibration struct {
	// Scope describes where the figures came from, e.g. "owner/repo" or
	// "all repositories" when the per-repository history was too thin.
	Scope string `json:"scope,omitempty"`
	// Runs the averages are drawn from.
	Runs int64 `json:"runs"`
	// AvgPredictedTurns/AvgActualTurns and the token pair are whole-number
	// averages over those runs.
	AvgPredictedTurns        int64 `json:"avgPredictedTurns"`
	AvgActualTurns           int64 `json:"avgActualTurns"`
	AvgPredictedOutputTokens int64 `json:"avgPredictedOutputTokens"`
	AvgActualOutputTokens    int64 `json:"avgActualOutputTokens"`
}

// TurnSkew is how far actual turns ran over (positive) or under (negative)
// the prediction, as a percentage. Zero when there is no prediction to
// compare against.
func (c *Calibration) TurnSkew() int64 {
	return skewPercent(c.AvgPredictedTurns, c.AvgActualTurns)
}

// TokenSkew is the same measure for output tokens.
func (c *Calibration) TokenSkew() int64 {
	return skewPercent(c.AvgPredictedOutputTokens, c.AvgActualOutputTokens)
}

// skewPercent is actual ÷ predicted - 1, as a rounded percentage.
func skewPercent(predicted, actual int64) int64 {
	if predicted <= 0 {
		return 0
	}
	return (actual - predicted) * 100 / predicted
}

// PreviousAttempt is the failed run a retry follows, rendered into the stage
// prompt so the agent is told what went wrong rather than handed the inputs
// that already failed once. It is copied from the retry's own immutable spec
// and carried to the pod as JSON, like Calibration — the runner has no
// Kubernetes access.
//
// Detail is UNTRUSTED: a run's detail can quote output from the repository
// or the image it ran on (a git status listing, a stderr tail). The prompt
// renders it bounded (PreviousDetailMaxBytes), stripped of control
// characters, in a fence no line of it can close, under a statement that it
// is data, not instructions. Outcome is the controller's, named from a closed
// vocabulary (agentresult.PreviousAttempt), so it is stated as fact — and a
// section with no detail quotes nothing untrusted; it is still bounded
// (PreviousOutcomeMaxBytes) and flattened onto one line here.
type PreviousAttempt struct {
	// Attempt is the failed run's ordinal.
	Attempt int32 `json:"attempt"`
	// Outcome is its stage outcome (commit_failed, timeout, ...), or
	// pull_request_closed when its pull request was closed unmerged.
	Outcome string `json:"outcome"`
	// Detail explains the outcome.
	Detail string `json:"detail,omitempty"`
}

// Bounds on what a prompt quotes from a previous attempt, in bytes. A detail
// cut short is marked with previousDetailCut after the bound.
const (
	PreviousOutcomeMaxBytes = 64
	PreviousDetailMaxBytes  = 4096
	previousDetailCut       = "\n[truncated]"
)

// quotable returns p as a prompt may quote it: the outcome on one line and
// the detail with its line breaks normalized, both free of control and
// format characters, valid UTF-8, and cut to their byte bounds on a rune
// boundary. Nil stays nil.
func (p *PreviousAttempt) quotable() *PreviousAttempt {
	if p == nil {
		return nil
	}
	detail := plainText(p.Detail)
	if len(detail) > PreviousDetailMaxBytes {
		detail = cutRunes(detail, PreviousDetailMaxBytes) + previousDetailCut
	}
	return &PreviousAttempt{
		Attempt: p.Attempt,
		Outcome: cutRunes(strings.Join(strings.Fields(plainText(p.Outcome)), " "), PreviousOutcomeMaxBytes),
		Detail:  strings.TrimRight(detail, "\n"),
	}
}

// plainText makes untrusted text safe to quote in a prompt: invalid UTF-8
// replaced, CRLF and lone CR turned into LF, and every other control or
// format character dropped except tab — a NUL cannot even be passed as a
// command-line argument, and terminal escapes or bidi overrides have no
// business in a prompt.
func plainText(s string) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	s = strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(s)
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r):
			return -1
		}
		return r
	}, s)
}

// cutRunes caps valid UTF-8 s at limit bytes without splitting a rune.
func cutRunes(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// RenderInvestigatePrompt renders the investigation prompt.
func RenderInvestigatePrompt(p InvestigatePrompt) (string, error) {
	p.PreviousAttempt = p.PreviousAttempt.quotable()
	return render("prompt_investigate.md.tmpl", p)
}

// RemediatePrompt is the data for the stage-2 (remediation) prompt.
type RemediatePrompt struct {
	IssuePath         string
	InvestigationPath string
	ReportPath        string
	CommitScriptPath  string
	// PreviousAttempt is the failed remediation this one retries; nil omits
	// the section.
	PreviousAttempt *PreviousAttempt
}

// RenderRemediatePrompt renders the remediation prompt.
func RenderRemediatePrompt(p RemediatePrompt) (string, error) {
	p.PreviousAttempt = p.PreviousAttempt.quotable()
	return render("prompt_remediate.md.tmpl", struct {
		RemediatePrompt
		ExcerptMaxBytes, CommandMaxBytes int
	}{p, report.ReproductionExcerptMaxBytes, report.ReproductionCommandMaxBytes})
}
