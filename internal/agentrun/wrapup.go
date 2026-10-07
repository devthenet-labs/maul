// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/bitwise-media-group/patchy/internal/envelope"
	"github.com/bitwise-media-group/patchy/internal/harness"
	"github.com/bitwise-media-group/patchy/internal/report"
	"github.com/bitwise-media-group/patchy/internal/runner"
	"github.com/bitwise-media-group/patchy/internal/templates"
)

// Wrap-up bounds: what an intent stage holds back from its grant for the
// rounds after its first run. They are constants, not configuration, for
// report repair's reason: a new PATCHY_* key would change the environment a
// repository-image Job blanks.
//
// The turns are held back INSIDE the grant, so the grant still bounds the
// stage: the first run gets --max-turns of the grant less the reserve, and
// a run stopped there is resumed with what is left. A grant below
// wrapUpMinGrant holds nothing back: a wrap-up needs a tool call and a
// closing message at least, and taking even wrapUpMinTurns from a small
// grant costs its first run too much.
//
// No output tokens are held back. The runner's kill switch sums the output
// counts claude's stream reports, which on CLI 2.1 are each message's count
// at its START, so it barely moves (TestKillSwitchNearInertOnClaude21), and
// a token reserve it was meant to enforce would bound nothing. What bounds
// a round's spend is its turn cap and the check, before every round, of the
// output tokens the result events reported against the stage's budget.
//
// Nor is wall clock held back from the first run yet: a soft deadline ends
// a run with SIGKILL, and resuming a session killed mid-stream is unproven
// on the pinned CLI. wrapUpWall bounds each round instead.
const (
	wrapUpMinGrant  = 15
	wrapUpMinTurns  = 3
	wrapUpMaxTurns  = repairMaxTurns
	wrapUpTurnShare = 5 // the reserve is grant/wrapUpTurnShare, within the bounds above
	wrapUpWallShare = 15
	wrapUpMinWall   = 3 * time.Minute
	wrapUpMaxWall   = repairTimeout
)

// grantSplit is how an intent stage's turn grant and wall clock are split
// between its first run and the rounds after it (a wrap-up, repairs).
type grantSplit struct {
	// grant is the stage's turn grant (0 for the CLI's default, no cap);
	// first is the first run's --max-turns and reserve the turns held back,
	// so first + reserve == grant.
	grant, first, reserve int
	// wall is the most wall clock one round after the first run may take:
	// wrapUpWallShare percent of the stage's, within [wrapUpMinWall,
	// wrapUpMaxWall], and never more than the stage's own.
	wall time.Duration
}

// splitGrant splits an intent stage's turn grant and wall clock. It is
// pure; TestSplitGrantProperty states its invariants.
func splitGrant(turns int, timeout time.Duration) grantSplit {
	s := grantSplit{grant: turns, first: turns}
	if turns >= wrapUpMinGrant {
		s.reserve = min(wrapUpMaxTurns, max(wrapUpMinTurns, turns/wrapUpTurnShare))
		s.first = turns - s.reserve
	}
	if timeout > 0 {
		s.wall = min(timeout, max(wrapUpMinWall, min(wrapUpMaxWall, timeout*wrapUpWallShare/100)))
	}
	return s
}

// newIntentStage sets up an intent stage's session on its split grant: the
// first run is held to the first share of the turns, and every round after
// it to the intent window.
func (a *Agent) newIntentStage(h harness.Harness, cli string, req harness.PromptRequest, timeout,
	idle time.Duration, budget int, split grantSplit) *stageRun {
	req.MaxTurns = split.first
	sr := a.newStage(h, cli, req, timeout, idle, budget)
	sr.split = &split
	return sr
}

// intentWindow is the one window every round after an intent stage's first
// run is held to, a wrap-up and a repair alike: it says what the round may
// spend elapsed into the stage, or why none may start. The turns are what
// the whole grant has left (the first run spent at most its share), at most
// repairMaxTurns; the output tokens are what the stage's budget has left
// after the tokens its runs' result events reported; the wall clock is what
// the stage has left, at most the split's per-round share.
func intentWindow(ctx context.Context, sr *stageRun, st *envelope.Stage, elapsed time.Duration) (roundLimits,
	string) {
	if ctx.Err() != nil {
		return roundLimits{}, "the stage was cancelled"
	}
	left := sr.timeout - elapsed
	if left < repairMinWindow {
		return roundLimits{}, fmt.Sprintf("less than %s of the stage's %s wall clock left",
			shortDuration(repairMinWindow), shortDuration(sr.timeout))
	}
	grant := sr.split.grant
	if grant > 0 && grant-st.NumTurns < 1 {
		return roundLimits{}, fmt.Sprintf("no turns left of the stage's %d", grant)
	}
	if sr.budget > 0 && sr.budget-st.Usage.OutputTokens < 1 {
		return roundLimits{}, fmt.Sprintf("no output tokens left of the stage's %d", sr.budget)
	}
	lim := roundLimits{maxTurns: repairMaxTurns, timeout: min(sr.split.wall, left)}
	if grant > 0 {
		lim.maxTurns = min(repairMaxTurns, grant-st.NumTurns)
	}
	if sr.budget > 0 {
		lim.budget = sr.budget - st.Usage.OutputTokens
	}
	return lim, ""
}

// fillRun copies a run's accounting into st (fillStage). On an intent
// stage, a run that ended without the CLI's result event still records the
// session it ran in, which patchy assigned before it started, and the turns
// its stream shows, so a run cut off mid-way says how far it got. A Finding
// stage records exactly what it always has.
func (a *Agent) fillRun(st *envelope.Stage, sr *stageRun, res runner.Result) {
	a.fillStage(st, sr.h, res)
	if sr.split == nil {
		return
	}
	if st.SessionID == "" {
		st.SessionID = sr.req.SessionID
	}
	if c, ok := sr.h.(harness.StreamTurnCounter); ok && st.NumTurns == 0 {
		st.NumTurns = c.StreamTurns(res.Stdout)
	}
}

// cutOff is how an intent stage's first run ended, for what can be salvaged
// of it.
type cutOff int

const (
	// cutNone: the run ended OK.
	cutNone cutOff = iota
	// cutMaxTurns: the CLI stopped it at its turn limit (error_max_turns),
	// on a clean turn boundary with a result event, so its session can be
	// resumed.
	cutMaxTurns
	// cutKillSwitch: the runner's output-token kill switch killed it.
	cutKillSwitch
	// cutDeadline: the stage's wall clock killed it.
	cutDeadline
	// cutIdle: the idle watchdog killed it.
	cutIdle
	// cutBrokerLimit: the egress broker refused it at a per-pod limit, so
	// a resumed run in the same pod would be refused again.
	cutBrokerLimit
	// cutOther: a crash, a runtime error or a cancelled stage, of which
	// nothing is salvaged.
	cutOther
)

// classifyCutOff says how a first run ended, in the order the stage's own
// outcome is decided (firstOutcome, stageOutcome): a cancelled stage first,
// since nothing may start after it.
func classifyCutOff(ctx context.Context, h harness.Harness, res runner.Result, idle string, runErr error) cutOff {
	switch {
	case ctx.Err() != nil:
		return cutOther
	case idle != "":
		return cutIdle
	case res.Aborted:
		return cutKillSwitch
	case runErr != nil:
		return cutOther
	case res.TimedOut:
		return cutDeadline
	}
	msg := h.RuntimeError(res.Stdout, res.ExitCode, res.TimedOut)
	if msg == "" {
		return cutNone
	}
	if b, ok := h.(harness.BudgetReporter); ok && b.Exhausted(res.Stdout) {
		return cutMaxTurns
	}
	var terminal string
	if r, ok := h.(harness.TerminalErrorReporter); ok {
		terminal = r.TerminalError(res.Stdout)
	}
	if brokerLimitMessage(msg, terminal, res.StderrTail) != "" {
		return cutBrokerLimit
	}
	return cutOther
}

// reason is the wrap-up reason a cut-off records, "" for none.
func (c cutOff) reason() envelope.WrapUpReason {
	switch c {
	case cutMaxTurns:
		return envelope.WrapUpMaxTurns
	case cutKillSwitch, cutBrokerLimit:
		return envelope.WrapUpBudget
	case cutDeadline:
		return envelope.WrapUpDeadline
	case cutIdle:
		return envelope.WrapUpIdle
	}
	return ""
}

// firstOutcome is the outcome and detail a stage's first run ended on, as
// every stage has always decided it: the idle watchdog, the kill switch,
// then the harness's verdict.
func (a *Agent) firstOutcome(h harness.Harness, res runner.Result, idle string,
	runErr error) (envelope.Outcome, string) {
	switch {
	case idle != "":
		return envelope.OutcomeTimeout, idle
	case res.Aborted:
		return envelope.OutcomeBudgetExceeded, res.AbortReason
	}
	return stageOutcome(h, res, runErr, credentialValues(h, a.scrub...))
}

// wrapUpRound resumes a stage stopped at its turn limit once, with the
// wrap-up prompt, in the intent window: a plan writes its report from what
// it has read; a build makes its report match the working tree, which the
// round must leave exactly as it found it (resumeRound's guard). written
// says the report was on disk when the run stopped. st.WrapUp records that
// the session was resumed.
func (a *Agent) wrapUpRound(ctx context.Context, sr *stageRun, st *envelope.Stage, path string,
	written bool) repairResult {
	resumer, ok := sr.h.(harness.Resumer)
	if !ok {
		return repairResult{why: "the " + sr.h.ID() + " harness cannot resume a session"}
	}
	lim, why := a.roundWindow(ctx, sr, st, time.Since(sr.started))
	if why != "" {
		return repairResult{why: why}
	}
	commitScript := ""
	if sr.writable() {
		commitScript = a.cfg.commitScript()
	}
	r := a.resumeRound(ctx, sr, st, resumer, lim, resumedRun{
		what: "the wrap-up", noun: "a wrap-up",
		prompt: func() (string, error) {
			return templates.RenderWrapUpPrompt(templates.WrapUpPrompt{
				ReportPath: path, CommitScriptPath: commitScript, FirstTurns: sr.req.MaxTurns, Turns: lim.maxTurns,
				ReportWritten: written,
			})
		},
		notice: fmt.Sprintf("the run stopped at its limit of %d turns; asking the agent to wrap up in at most %d more",
			sr.req.MaxTurns, lim.maxTurns),
		log:     "asking the agent to wrap up",
		logArgs: []any{"phase", a.cfg.Phase, "turns", lim.maxTurns},
	})
	st.WrapUp.Resumed = r.ran
	return r
}

// wrapUpNote explains, after a cut-off stage's own detail, why nothing it
// left was kept.
func wrapUpNote(r repairResult) string {
	switch {
	case r.refused != "":
		return "wrap-up refused: " + r.refused
	case r.why != "" && !r.ran:
		return "no wrap-up: " + r.why
	case r.why != "":
		return r.why
	case r.failure != "":
		return "the wrap-up run ended " + r.failure
	}
	return ""
}

// onDisk reports whether a report file exists at path.
func onDisk(path string) bool {
	_, err := os.Stat(path)
	return err == nil || !errors.Is(err, fs.ErrNotExist)
}

// salvagePlan settles what a plan whose first run did not end OK left
// behind (outcome and detail are that run's). A plan goes to a human
// approver, who reads it marked as written by a run that was cut off, so a
// report on disk is settled whatever cut the run off:
//
//   - stopped at its turn limit, the session is resumed once to write the
//     report when there is none, and the report is then settled with the
//     repair rounds the window allows;
//   - killed by the wall clock, the idle watchdog, the kill switch or the
//     broker, the report on disk is read and kept if it parses, with no
//     resume: resuming a session killed mid-stream is unproven;
//   - crashed or cancelled, nothing is salvaged.
//
// A plan salvaged ends ok with st.WrapUp.Kept; one not keeps the first
// run's outcome, with why after its detail.
func (a *Agent) salvagePlan(ctx context.Context, sr *stageRun, st *envelope.Stage, cut cutOff,
	outcome envelope.Outcome, detail string, parse func([]byte) (*report.Plan, error)) (*report.Plan, []byte,
	envelope.Outcome, string) {
	if cut == cutOther {
		return nil, nil, outcome, detail
	}
	st.WrapUp = &envelope.WrapUp{Reason: cut.reason()}
	path := a.cfg.planPath()
	settle := func() (*report.Plan, []byte, envelope.Outcome, string) {
		return readReportAs(path, readReport, parse)
	}
	if cut == cutMaxTurns {
		settle = func() (*report.Plan, []byte, envelope.Outcome, string) {
			return settleReport(ctx, a, sr, st, path, readReport, parse)
		}
		if !onDisk(path) {
			if r := a.wrapUpRound(ctx, sr, st, path, false); r.why != "" || r.failure != "" {
				// The wrap-up did not end cleanly: keep what it wrote if it
				// parses, but spend nothing more on it.
				p, raw, got, _ := readReportAs(path, readReport, parse)
				if got != envelope.OutcomeOK {
					return nil, nil, outcome, detail + " (" + wrapUpNote(r) + ")"
				}
				st.WrapUp.Kept = true
				return p, raw, got, ""
			}
		}
	}
	p, raw, got, why := settle()
	switch got {
	case envelope.OutcomeOK:
		st.WrapUp.Kept = true
		return p, raw, got, ""
	case envelope.OutcomeReportMissing:
		if cut != cutMaxTurns {
			return nil, nil, outcome, detail
		}
	}
	return nil, nil, outcome, detail + " (the report it left was not kept: " + why + ")"
}

// unconfirmedBuild is the note a build's detail carries when the report its
// cut-off run left is recorded, though never pushed.
const unconfirmedBuild = "its report was recorded, but a build cut off before confirming it is never pushed"

// salvageBuild settles what a build whose first run did not end OK left
// behind (outcome and detail are that run's). Its report may have been
// written early and claim success for work the run went on changing, so it
// is never taken as it stands:
//
//   - stopped at its turn limit, the session is resumed once (wrapUpRound)
//     to make the report match the working tree, which the round may not
//     change, and the report is then settled with the repair rounds the
//     window allows. A build salvaged this way ends ok, and returns the
//     clone as the wrap-up found it, which whatever commit.sh commits must
//     be a subset of (committedSubset);
//   - otherwise — killed, refused by the broker, crashed, cancelled, a
//     wrap-up that could not run or did not end cleanly — the report on
//     disk, if it parses, is returned for the controller to record beside
//     the first run's outcome, but nothing is packaged or pushed.
//
// A wrap-up that changed the tree is refused whole, and nothing it left is
// recorded.
func (a *Agent) salvageBuild(ctx context.Context, sr *stageRun, st *envelope.Stage, cut cutOff,
	outcome envelope.Outcome, detail string) (*report.Build, []byte, *fingerprint, envelope.Outcome, string) {
	path := a.cfg.buildPath()
	if reason := cut.reason(); reason != "" {
		st.WrapUp = &envelope.WrapUp{Reason: reason}
	}
	notes := []string{}
	if cut == cutMaxTurns {
		r := a.wrapUpRound(ctx, sr, st, path, onDisk(path))
		switch {
		case r.refused != "":
			return nil, nil, nil, outcome, detail + " (" + wrapUpNote(r) + ")"
		case r.why == "" && r.failure == "":
			b, raw, got, why := settleReport(ctx, a, sr, st, path, readReport, report.ParseBuild)
			if got == envelope.OutcomeOK {
				st.WrapUp.Kept = true
				return b, raw, &r.before, got, ""
			}
			return nil, nil, nil, outcome, detail + " (the report it left was not kept: " + why + ")"
		}
		notes = append(notes, wrapUpNote(r))
	}
	if _, raw, got, _ := readReportAs(path, readReport, report.ParseBuild); got == envelope.OutcomeOK {
		notes = append(notes, unconfirmedBuild)
		return nil, raw, nil, outcome, detail + " (" + strings.Join(notes, "; ") + ")"
	}
	if len(notes) == 0 {
		return nil, nil, nil, outcome, detail
	}
	return nil, nil, nil, outcome, detail + " (" + strings.Join(notes, "; ") + ")"
}
