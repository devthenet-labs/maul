// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/checks"
	"github.com/devthenet-labs/maul/internal/untrusted"
)

// The fix-round reproduction (DESIGN 2.2, 2.9 use 2, 7.7; SPEC S1-21): what
// a verify-fix round started by an evaluation is given. Per failure, maul
// renders a plain bug report from its own records: the approved outcome, the
// failing check's steps in maul's words with what each expects, what the
// check runner observed on the failing run (the tools ledger), and at most
// verifyReproLogLines redacted log lines from that run's window. Never the
// evaluator's prompt, its notes, its transcript, another check or the check
// store: the values carry none of them.
//
// Every value but maul's own sentences is the evaluator's or the
// application's, so each is escaped (untrusted.Escape) and shown only in a
// code span or a fence no line of it can close, cut to a fixed size.

// Bounds on what a reproduction shows.
const (
	// verifyReproLogLines bounds the log lines one failure shows, over all
	// its components (DESIGN 4.1: 40 lines).
	verifyReproLogLines = 40
	// verifyReproLogLineBytes bounds one log line.
	verifyReproLogLineBytes = 400
	// verifyReproDetailBytes bounds one step's observed detail.
	verifyReproDetailBytes = 512
	// verifyReproValueBytes bounds one value a step shows (a path, a
	// locator, an expected value).
	verifyReproValueBytes = 256
	// verifyReproFileBytes bounds one exec file's content.
	verifyReproFileBytes = 2 << 10
	// verifyReproStatementBytes bounds the approved outcome's text.
	verifyReproStatementBytes = 1 << 10
	// verifyReproObserved bounds the observations one failure shows.
	verifyReproObserved = 16
)

// VerifyReproduction is an evaluation's verify-fix round's input: the
// verification that failed, at its heads, and its failures.
type VerifyReproduction struct {
	// Verification names the PreviewVerification, Round and Attempt its
	// numbering.
	Verification   string
	Round, Attempt int32
	Heads          []ReproductionHead
	Failures       []ReproductionFailure
	// Workspace is an agents round's: the checks ran against maul's builds
	// of the commits, never a preview (SPEC-S2 S2-20b).
	Workspace bool
}

// ReproductionHead is one previewed component at its commit.
type ReproductionHead struct{ Component, Revision string }

// ReproductionFailure is one failing outcome's reproduction.
type ReproductionFailure struct {
	// Outcome is the approved outcome's id, Statement its approved text.
	Outcome, Statement string
	// Check is the failing check; nil when its body does not parse (the
	// steps are then not shown).
	Check *checks.Check
	// Observed is what the check runner reported for the failing run's
	// steps that did not pass, in order; none when maul's record of the
	// run could not be read.
	Observed []ReproductionObservation
	// Logs are the excerpts from the failing run's window, per component.
	Logs []ReproductionLog
}

// ReproductionObservation is one step of one attempt that did not pass.
type ReproductionObservation struct {
	// Attempt and Step count from 1; Step 0 is the attempt as a whole (it
	// recorded no step).
	Attempt, Step int
	// Result is the step's result (fail, error), Detail the runner's
	// message: it can quote the application.
	Result, Detail string
}

// ReproductionLog is one component's log lines from the window.
type ReproductionLog struct {
	Component string
	Lines     []string
}

// reproView is the template's view.
type reproView struct {
	Verification, Round, Attempt string
	Heads                        []string
	Failures                     []reproFailureView
	Tokens, Workspace            bool
	Placeholders                 []string
}

type reproFailureView struct {
	Outcome, Statement, Form string
	Steps                    []string
	NoSteps                  bool
	Observed                 []string
	NoObserved               bool
	Logs                     []reproLogView
}

type reproLogView struct{ Component, Block string }

// RenderVerifyReproduction renders r as a fix round's feedback.
func RenderVerifyReproduction(r VerifyReproduction) (string, error) {
	v := reproView{Verification: reproCode(r.Verification), Round: strconv.Itoa(int(r.Round)),
		Attempt: strconv.Itoa(int(r.Attempt)), Workspace: r.Workspace}
	for _, h := range r.Heads {
		v.Heads = append(v.Heads, reproCode(h.Component)+" at "+reproCode(h.Revision))
	}
	var cs []checks.Check
	for _, f := range r.Failures {
		fv := reproFailure(f, r.Workspace)
		if f.Check != nil {
			cs = append(cs, *f.Check)
		}
		v.Tokens = v.Tokens || f.Check != nil && usesRunToken(*f.Check)
		v.Failures = append(v.Failures, fv)
	}
	if r.Workspace {
		v.Placeholders = workspacePlaceholders(cs)
	}
	out, err := render("intent_reproduction.md.tmpl", v)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(out, "\n"), nil
}

// reproFailure is one failure's view, of a workspace's check when
// workspace.
func reproFailure(f ReproductionFailure, workspace bool) reproFailureView {
	fv := reproFailureView{Outcome: reproCode(f.Outcome),
		Statement: chomp(fence(untrusted.Visible(f.Statement, verifyReproStatementBytes)))}
	if f.Check == nil {
		fv.NoSteps = true
	} else {
		var steps []string
		fv.Form, steps = checkSteps(*f.Check, workspace)
		for i, st := range steps {
			fv.Steps = append(fv.Steps, fmt.Sprintf("%d. %s", i+1, st))
		}
	}
	for i, o := range f.Observed {
		if i == verifyReproObserved {
			fv.Observed = append(fv.Observed, fmt.Sprintf("%d more not shown", len(f.Observed)-i))
			break
		}
		line := fmt.Sprintf("attempt %d", o.Attempt)
		if o.Step > 0 {
			line += fmt.Sprintf(", step %d", o.Step)
		}
		line += ": " + reproResult(o.Result)
		if d := strings.TrimSpace(o.Detail); d != "" {
			line += ": " + code(untrusted.Visible(d, verifyReproDetailBytes))
		}
		fv.Observed = append(fv.Observed, line)
	}
	fv.NoObserved = len(fv.Observed) == 0
	budget := verifyReproLogLines
	for _, l := range f.Logs {
		if budget == 0 {
			break
		}
		lines := l.Lines
		if len(lines) > budget {
			lines = lines[:budget]
		}
		budget -= len(lines)
		shown := make([]string, 0, len(lines))
		for _, line := range lines {
			shown = append(shown, untrusted.Visible(line, verifyReproLogLineBytes))
		}
		if len(shown) > 0 {
			fv.Logs = append(fv.Logs, reproLogView{Component: reproCode(l.Component),
				Block: chomp(fence(strings.Join(shown, "\n")))})
		}
	}
	return fv
}

// reproResult is a step result in maul's words; one not listed is unknown.
func reproResult(r string) string {
	switch r {
	case "fail":
		return "failed"
	case "error":
		return "could not be judged"
	}
	return "unknown result"
}

// reproCode is untrusted s, escaped and bounded, in a code span.
func reproCode(s string) string {
	if s == "" {
		return "(none)"
	}
	return code(untrusted.Visible(s, verifyReproValueBytes))
}

// reproJSON is v as compact JSON in a code span, its characters as
// written (no HTML escaping).
func reproJSON(v any) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "(unreadable)"
	}
	return reproCode(strings.TrimSuffix(b.String(), "\n"))
}

// usesRunToken reports a check that names the run token.
func usesRunToken(c checks.Check) bool { return names(c, "{{run}}") }

// names reports a check whose body names placeholder.
func names(c checks.Check, placeholder string) bool {
	b, err := json.Marshal(c)
	return err == nil && strings.Contains(string(b), placeholder)
}

// workspacePlaceholders are the sentences saying what each workspace
// placeholder the checks name stands for, in a fixed order.
func workspacePlaceholders(cs []checks.Check) []string {
	var out []string
	for _, p := range []struct{ name, means string }{
		{checks.BinPlaceholder, "stands for the program the Project's build wrote."},
		{checks.TreePlaceholder, "stands for the sandbox's writable copy of the repository's tree."},
		{checks.ExecMarkerPlaceholder, "stands for a marker maul picks for every run: the program prints it when " +
			"one of its own assertions finds the outcome does not hold."},
	} {
		if slices.ContainsFunc(cs, func(c checks.Check) bool { return names(c, p.name) }) {
			out = append(out, "`"+p.name+"` "+p.means)
		}
	}
	return out
}

// checkSteps are c's form and its steps in maul's words, each with what it
// expects; workspace says c ran against maul's build of a tree.
func checkSteps(c checks.Check, workspace bool) (string, []string) {
	switch {
	case c.HTTP != nil:
		steps := make([]string, 0, len(c.HTTP.Steps))
		for _, s := range c.HTTP.Steps {
			steps = append(steps, httpStep(s))
		}
		return "an http check against component " + reproCode(c.HTTP.Target), steps
	case c.Browser != nil:
		steps := make([]string, 0, len(c.Browser.Steps))
		for _, s := range c.Browser.Steps {
			steps = append(steps, browserStep(s))
		}
		return "a browser check", steps
	case c.Exec != nil && workspace:
		return "a program run in the sandbox of the pull request's build", execSteps(c.Exec)
	case c.Exec != nil:
		return "a program run in the sandbox", execSteps(c.Exec)
	case c.Run != nil:
		return "a Go program built against the pull request's tree", runSteps(c.Run)
	}
	return "a check of a form maul cannot show", nil
}

// httpStep: "Send `POST /notes` with JSON body `{…}`. Expected: status `201`."
func httpStep(s acceptance.Step) string {
	method := s.Request.Method
	if method == "" {
		method = "GET"
	}
	line := "Send " + reproCode(method+" "+s.Request.Path)
	var with []string
	for _, k := range slices.Sorted(maps.Keys(s.Request.Headers)) {
		with = append(with, "header "+reproCode(k+": "+s.Request.Headers[k]))
	}
	if s.Request.JSON != nil {
		with = append(with, "JSON body "+reproCode(s.Request.JSON.String()))
	}
	if s.Request.Body != "" {
		with = append(with, "body "+reproCode(s.Request.Body))
	}
	if len(with) > 0 {
		line += " with " + strings.Join(with, ", ")
	}
	line += "."
	if expects := httpExpects(s.Expect); len(expects) > 0 {
		line += " Expected: " + strings.Join(expects, "; ") + "."
	}
	if len(s.Capture) > 0 {
		caps := make([]string, 0, len(s.Capture))
		for _, k := range slices.Sorted(maps.Keys(s.Capture)) {
			caps = append(caps, reproCode(s.Capture[k])+" as "+reproCode("{{capture."+k+"}}"))
		}
		line += " Keeps " + strings.Join(caps, ", ") + " for later steps."
	}
	return line
}

// httpExpects are an http step's expectations in maul's words.
func httpExpects(e acceptance.Expect) []string {
	var out []string
	switch len(e.Status) {
	case 0:
	case 1:
		out = append(out, "status "+reproCode(strconv.Itoa(int(e.Status[0]))))
	default:
		codes := make([]string, 0, len(e.Status))
		for _, s := range e.Status {
			codes = append(codes, strconv.Itoa(int(s)))
		}
		out = append(out, "status one of "+reproCode(strings.Join(codes, ", ")))
	}
	for _, k := range slices.Sorted(maps.Keys(e.JSON)) {
		out = append(out, "JSON body at "+reproCode(k)+": "+reproCode(e.JSON[k].String()))
	}
	for _, k := range slices.Sorted(maps.Keys(e.Headers)) {
		out = append(out, "header "+reproCode(k)+": "+reproCode(e.Headers[k].String()))
	}
	if e.BodyContains != "" {
		out = append(out, "body contains "+reproCode(e.BodyContains))
	}
	return out
}

// browserStep is one browser step in maul's words.
func browserStep(s checks.BrowserStep) string {
	switch {
	case s.Goto != "":
		return "Open " + reproCode(s.Goto) + "."
	case s.WaitForURL != "":
		return "Wait for the page's path to match " + reproCode(s.WaitForURL) + "."
	case s.Click != nil:
		return "Click " + reproJSON(s.Click) + "."
	case s.Fill != nil:
		return "Fill " + reproJSON(s.Fill.Locator) + " with " + reproCode(s.Fill.Text) + "."
	case s.Select != nil:
		return "Select " + reproCode(s.Select.Option) + " in the field labelled " + reproCode(s.Select.Label) + "."
	case s.Press != nil:
		if s.Press.Locator != nil {
			return "Press " + reproCode(s.Press.Key) + " in " + reproJSON(s.Press.Locator) + "."
		}
		return "Press " + reproCode(s.Press.Key) + "."
	case s.Scroll != nil:
		return fmt.Sprintf("Scroll %s by %d, %d px.", reproJSON(s.Scroll.Locator), s.Scroll.DX, s.Scroll.DY)
	case s.Expect != nil:
		return "Expected: " + reproCode(string(s.Expect.Assert)) + " " + reproJSON(s.Expect) + "."
	}
	return "A step maul cannot show."
}

// execSteps are an exec check's files, its command and what it expects.
func execSteps(e *checks.ExecCheck) []string {
	out := writeFiles(e.Files)
	return append(out, withExpects("Run "+reproJSON(e.Argv)+timeoutOf(e.Timeout)+".", e.Expect))
}

// runSteps are a run check's files, its build and run, and what it
// expects.
func runSteps(r *checks.RunCheck) []string {
	out := writeFiles(r.Files)
	out = append(out, "maul writes the program's `go.mod` and builds it with the tree in a Go workspace.")
	run := "Run it"
	if len(r.Args) > 0 {
		run += " with the arguments " + reproJSON(r.Args)
	}
	return append(out, withExpects(run+timeoutOf(r.Timeout)+".", r.Expect))
}

// writeFiles are the steps writing a check's program files.
func writeFiles(files []checks.File) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		mode := ""
		if f.Executable {
			mode = " (executable)"
		}
		out = append(out, "Write the file "+reproCode(f.Path)+mode+":\n\n"+
			reproIndent(chomp(fence(untrusted.Visible(f.Content, verifyReproFileBytes)))))
	}
	return out
}

// timeoutOf is a check's timeout as a step's suffix, "" for none.
func timeoutOf(timeout string) string {
	if timeout == "" {
		return ""
	}
	return " (timeout " + reproCode(timeout) + ")"
}

// withExpects is step followed by what e expects of the program.
func withExpects(step string, e checks.ExecExpect) string {
	var expects []string
	if e.Exit != nil {
		expects = append(expects, "exit status "+reproCode(strconv.Itoa(int(*e.Exit))))
	}
	if e.StdoutContains != "" {
		expects = append(expects, "standard output contains "+reproCode(e.StdoutContains))
	}
	for _, k := range slices.Sorted(maps.Keys(e.StdoutJSON)) {
		expects = append(expects, "standard output's JSON at "+reproCode(k)+": "+
			reproCode(e.StdoutJSON[k].String()))
	}
	if len(expects) > 0 {
		step += " Expected: " + strings.Join(expects, "; ") + "."
	}
	return step
}

// reproIndent indents every line of s by three spaces, under a list item.
func reproIndent(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = "   " + l
		}
	}
	return strings.Join(lines, "\n")
}
