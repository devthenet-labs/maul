// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/checks"
)

// Acceptance verification, as maul tells people about it: the plan comment's
// acceptance line, the plan prompt's teaching section, the verification
// comment on the intent's issue (one per verification round, edited per
// attempt), the verification section of each pull request's preview comment
// and the progress clause of the status comment's preview line. All are
// rendered from plain values the intent controller derived from the
// PreviewVerification and its own records.
//
// Every sentence is maul's. The only agent or application text shown is a
// criterion's approved statement (SanitizeInline), a failure's expected and
// observed values and body prefix, and a component's log tail; the last
// three sit in code spans or fences no line of them can close, cut to fixed
// sizes, with every character that renders as nothing shown by its code
// point.

// VerifyKeyPrefix heads the notice key of a round's verification comment.
const VerifyKeyPrefix = "verify-r"

// VerifyKey is the notice key of verification round's comment on the issue.
func VerifyKey(round int32) string { return fmt.Sprintf("%s%d", VerifyKeyPrefix, round) }

// Verification states, as the progress clause words them.
const (
	// VerifyWaiting: verification waits for the preview to be ready.
	VerifyWaiting = "waiting"
	// VerifyRunning: the checks are running or about to.
	VerifyRunning = "verifying"
	// VerifyPassed: every automated check passed at the current heads.
	VerifyPassed = "passed"
	// VerifyFailing: a check fails and the intent stays in review.
	VerifyFailing = "failing"
	// VerifyExhausted: a check fails and no automatic fix is left.
	VerifyExhausted = "exhausted"
	// VerifyUnavailable: the checks could not be run (infrastructure).
	VerifyUnavailable = "unavailable"
	// VerifyPreviewNotLive: the checks could not be run because the
	// preview was not live at the pull requests' heads in time.
	VerifyPreviewNotLive = "preview-not-live"
	// VerifyWaitingForCI: verification waits for the pull requests' named
	// checks to pass.
	VerifyWaitingForCI = "waiting-for-ci"
	// VerifyNeedsReplan: an approved criterion cannot run as approved, and
	// the approved plan cannot change in review (startOver).
	VerifyNeedsReplan = "needs-replan"
	// VerifyNotDeclared: the approved plan declares no outcomes.
	VerifyNotDeclared = "not-declared"
	// VerifyNotChecked: the approved plan declares criteria, none of which
	// maul checks automatically.
	VerifyNotChecked = "not-checked"
	// VerifyUnreadable: the approved plan's acceptance criteria cannot be
	// read, so nothing is verified until a plan is approved again.
	VerifyUnreadable = "unreadable"
	// VerifyCriteriaReplan: the approved plan states slice-1 criteria,
	// which the agent evaluator does not run, so nothing is verified until
	// a plan stating outcomes is approved.
	VerifyCriteriaReplan = "criteria-replan"
	// VerifyEvaluating: the agent evaluator checks the approved outcomes.
	VerifyEvaluating = "evaluating"
	// VerifyUnconfirmed: the evaluation settled without confirming every
	// outcome.
	VerifyUnconfirmed = "unconfirmed"
	// VerifyHeadsMoved: the pull requests moved past the commits the last
	// result was about, so no result is shown until maul checks the new
	// ones (review SM-1).
	VerifyHeadsMoved = "heads-moved"
)

// Automatic fix states.
const (
	FixQueued  = "queued"
	FixRunning = "running"
	FixHalted  = "halted"
)

// Bounds on what a comment quotes.
const (
	// VerifyLogMaxBytes bounds one component's log tail; verifyLogsMax the
	// tails a comment carries.
	VerifyLogMaxBytes = 3072
	verifyLogsMax     = 8
	// verifyReasonMaxBytes bounds an auto-fix reason.
	verifyReasonMaxBytes = 120
)

// VerifyProgress is where verification stands, as one clause.
type VerifyProgress struct {
	// State is one of the Verify state constants; "" shows nothing.
	State string
	// Verification is the verification's own number, as its comment's
	// header names it (0 omits it). FixesUsed of MaxFixes count the
	// automatic fix rounds spent against the Project's maxVerifyFixes;
	// they are never called rounds of verification, so the two numbers
	// cannot be read as one.
	Verification, FixesUsed, MaxFixes int32
	// Passing of Total automated checks pass; Total 0 omits the count.
	Passing, Total int32
	// ManualOpen counts the outcomes held for a human.
	ManualOpen int32
	// AutoFix is "", FixQueued, FixRunning or FixHalted; AutoFixReason is
	// the controller's short fixed reason for a halt.
	AutoFix       string
	AutoFixReason string
	// Outcomes: the agent evaluator checks approved outcomes, so the
	// clause names evaluations and counts outcomes confirmed. false is
	// slice 1's wording, unchanged.
	Outcomes bool
	// DefectConfirmed: a person confirmed the defect maul suspected (fifth
	// review, 6), so the fix is theirs, never called automatic, and no
	// failing count is shown for it.
	DefectConfirmed bool
	// Why is why an unconfirmed evaluation will not go on (a HandoffWhy
	// constant, "" for none): with every outcome confirmed, the line names
	// it in place of the counts' "couldn't confirm every outcome"
	// (UnconfirmedCause, review H2). Workspace is a Project with no preview.
	Why       string
	Workspace bool
}

// verifyRestingLine is the sentence of a state nothing is checked in:
// whole for one that never carries counts, else its head; "" for any other
// state.
func verifyRestingLine(state string) (line string, whole bool) {
	switch state {
	case VerifyWaitingForCI:
		return "Verification waits for the pull requests' checks to pass", false
	case VerifyNeedsReplan:
		return "Verification cannot run: an approved check's target is not previewed. " + startOver(), true
	case VerifyNotDeclared:
		return "No acceptance outcomes were declared.", true
	case VerifyUnreadable:
		return "Verification stopped: maul cannot read the approved plan's acceptance criteria, so nothing is " +
			"verified. " + startOver(), true
	case VerifyCriteriaReplan:
		return "Nothing is verified: the approved plan states acceptance criteria, which maul's evaluator does " +
			"not run. " + startOver(), true
	case VerifyNotChecked:
		return "Nothing is checked automatically", false
	case VerifyHeadsMoved:
		return "The pull requests moved to new commits: maul is checking the new commits, and the last result, " +
			"about earlier commits, is not shown.", true
	case VerifyPreviewNotLive:
		return "Verification unavailable: the preview did not become live at the pull requests' heads in time, so " +
			"the checks could not run. Verification resumes once the preview is live; `kubectl get preview` " +
			"says why it is not.", true
	}
	return "", false
}

// startOver is what an intent in review is told it can do for a new plan:
// maul does not replan an intent whose pull requests are open (replan from
// review is deferred; /maul replan there is not available), so an approver
// cancels it on its issue (cancel is no pull request command) and opens a
// new issue.
func startOver() string {
	return "The approved plan cannot change while its pull requests are open: to start over with a new plan, an " +
		"approver comments " + slashCommand("cancel") + " on the intent's issue (the pull requests are left to you) " +
		"and opens a new issue."
}

// verifyNames are the progress clause's words: which verification (or
// evaluation) it is, its counts (checks passing, or outcomes confirmed;
// "" with none), and its passed head.
func verifyNames(p VerifyProgress) (which, counts, passed string) {
	which = "Verification"
	counted := func(n, total int32) string { return fmt.Sprintf("%d/%d checks passing", n, total) }
	done := func(n, total int32) string { return fmt.Sprintf("%d/%d checks passed", n, total) }
	if p.Outcomes {
		which, counted, done = "Evaluation", OutcomeCount, OutcomeCount
	}
	if p.Verification > 0 {
		which = fmt.Sprintf("%s %d", which, p.Verification)
	}
	passed = which + " passed"
	if p.Total > 0 {
		counts = counted(p.Passing, p.Total)
		passed += ": " + done(p.Passing, p.Total)
	}
	return which, counts, passed
}

// OutcomeCount is n of total approved outcomes confirmed (or accepted), as
// every surface counts them: the comments' progress line and the commit
// status build it here alone (review L2).
func OutcomeCount(n, total int32) string { return fmt.Sprintf("%d/%d outcomes confirmed", n, total) }

// verifyLine is the progress as one sentence; "" for none.
func verifyLine(p VerifyProgress) string {
	head, whole := verifyRestingLine(p.State)
	if whole {
		return head
	}
	fixes := fmt.Sprintf("%d of %d automatic %s used", p.FixesUsed, p.MaxFixes, plural(p.MaxFixes, "fix", "fixes"))
	which, counts, passed := verifyNames(p)
	join := func(parts ...string) string {
		var kept []string
		for _, s := range parts {
			if s != "" {
				kept = append(kept, s)
			}
		}
		return strings.Join(kept, ", ")
	}
	switch p.State {
	case VerifyWaitingForCI, VerifyNotChecked:
		// verifyRestingLine's head.
	case VerifyWaiting:
		head = "Verification waits for the preview to be ready"
	case VerifyRunning, VerifyEvaluating:
		head = which + " running: " + join(counts, fixes)
	case VerifyUnconfirmed:
		head = which + ": maul couldn't confirm every outcome: " + join(counts, fixes)
		if cause := UnconfirmedCause(p.Why, p.Workspace); cause != "" && p.Total > 0 && p.Passing == p.Total {
			// Every outcome confirmed: the cause, not the counts, is why
			// maul could not confirm the round (review H2).
			head = which + ": " + counts + ", but maul couldn't confirm this: " + cause + "; " + fixes
		}
	case VerifyPassed:
		head = passed
	case VerifyFailing:
		if p.DefectConfirmed {
			head = which + ": a person confirmed the defect maul found: " + join(counts)
			break
		}
		head = which + " failing: " + join(counts, fixes)
	case VerifyExhausted:
		head = which + " failing: " + join(counts, "no automatic fix left")
	case VerifyUnavailable:
		head = "Verification unavailable: maul could not run the checks"
	default:
		return ""
	}
	if p.ManualOpen > 0 {
		head += fmt.Sprintf(", %d for a human", p.ManualOpen)
	}
	return head + fixClause(p) + "."
}

// fixClause is the progress clause's fix: queued, running or halted; a fix
// a person confirmed the defect for is never called automatic (fifth
// review, 6).
func fixClause(p VerifyProgress) string {
	switch p.AutoFix {
	case FixQueued, FixRunning:
		if p.DefectConfirmed {
			return "; a fix is " + p.AutoFix
		}
		return "; automatic fix " + p.AutoFix
	case FixHalted:
		if r := shown(cutRunes(visibleText(p.AutoFixReason), verifyReasonMaxBytes)); r != "" {
			return "; automatic fix halted (" + code(r) + ")"
		}
		return "; automatic fix halted"
	}
	return ""
}

// plural is one when n is 1, else many.
func plural(n int32, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// VerifyLog is one component's log tail (already redacted by the caller).
type VerifyLog struct {
	Part, Tail string
}

// VerifySection is the verification as a comment shows it. A zero Progress
// and no outcomes show nothing.
type VerifySection struct {
	Progress VerifyProgress
	// Outcomes are the agent evaluator's approved outcomes with their
	// decided results, in plan order; Anomalies are its look-around
	// anomalies.
	Outcomes  []VerifyOutcomeResult
	Anomalies []VerifyAnomaly
	// Logs are shown only when the caller passes them: the issue always,
	// a pull request only under the Project's postLogs.
	Logs []VerifyLog
	// Impostor is the login that posted a maul/acceptance status other than
	// maul's, which maul re-posted over; "" for none.
	Impostor string
	// StatusNotPosted: the maul App may not post commit statuses, so no
	// maul/acceptance status is posted.
	StatusNotPosted bool
	// NoDefect is the acceptance-fix round that concluded the code has no
	// defect, which halted automatic fixes; nil for none.
	NoDefect *VerifyNoDefect
	// Suspected is the defect maul suspects and waits for a person to
	// confirm (a defect found in round 1); nil for none.
	Suspected *VerifySuspected
}

// VerifySuspected is a suspected defect as a comment shows it: the
// outcomes, and maul's reproduction (rendered by maul from its own ledger;
// it quotes the application, so it is shown fenced and defanged).
type VerifySuspected struct {
	Outcomes     []string
	Reproduction string
	// ConfirmedBy is the approver whose /maul verify fix confirmed it
	// (fifth review, 6); "" while it waits for a person.
	ConfirmedBy string
	// Held: the hand-off is held while the defect waits for a person, so
	// the note says so; false says nothing of a hold, which is not in
	// force once released (gate D-B3).
	Held bool
	// Workspace: the Project has no preview (SPEC-S2), so the note names
	// the held hand-off, not a link, and only the limits of the checks a
	// workspace runs.
	Workspace bool
}

// VerifySuspectedMaxBytes bounds the reproduction a comment shows.
const VerifySuspectedMaxBytes = 6 << 10

// suspectedNote is the note on a suspected defect: what maul believes and
// why, that a person should confirm it, the command that does, the known
// limits, and the reproduction a fix round would be handed.
func suspectedNote(s *VerifySuspected) string { return suspectedText(s) + suspectedReproduction(s) }

// suspectedReproduction is the suspected defect's reproduction as its note
// ends with it, fenced and defanged: evidence, which a comment over its
// budget leaves out last (fitVerify); "" for none.
func suspectedReproduction(s *VerifySuspected) string {
	r := strings.TrimSpace(cutRunes(visibleText(s.Reproduction), VerifySuspectedMaxBytes))
	if r == "" {
		return ""
	}
	return "\n\nmaul's reproduction:\n\n" + fence(defang(r))
}

// suspectedText is the note on a suspected defect without its
// reproduction: maul's own words, never cut.
func suspectedText(s *VerifySuspected) string {
	ids := make([]string, 0, len(s.Outcomes))
	for _, id := range s.Outcomes {
		if c := shown(id); c != "" {
			ids = append(ids, code(c))
		}
	}
	var b strings.Builder
	if who := shown(s.ConfirmedBy); who != "" {
		fmt.Fprintf(&b, "A person, %s, confirmed the defect maul found in %s: a fix round, handed the reproduction "+
			"below, fixes it, and maul checks the outcomes again once the fix is pushed.", code(who),
			strings.Join(ids, ", "))
	} else {
		fmt.Fprintf(&b, "maul believes %s %s a defect: the evaluator reported the outcome fails, and maul's own runs "+
			"of the check it saved to show it, each alone under a fresh run token, failed the same assertion. maul "+
			"does not fix a defect found this way on its own: a person should confirm it. An approver's "+
			"`/maul verify fix` starts a fix round, handed the reproduction below%s. Known limits: %s",
			strings.Join(ids, ", "), plural(int32(len(ids)), "has", "have"), suspectedHold(s), suspectedLimits(s))
	}
	return b.String()
}

// suspectedHold is the clause naming the hold a suspected defect keeps,
// after "handed the reproduction below": the preview link on a slot
// Project, the hand-off on one with no preview; "" once it is released.
func suspectedHold(s *VerifySuspected) string {
	switch {
	case !s.Held:
		return ""
	case s.Workspace:
		return "; until then maul holds its hand-off to the reviewers"
	}
	return "; until then the preview link stays held"
}

// suspectedLimits are the known limits of the checks that show a suspected
// defect, ending in a full stop: a workspace runs exec and run checks only.
func suspectedLimits(s *VerifySuspected) string {
	if s.Workspace {
		return "an exec or run check's failed assertion is whatever its program reports, so a program that " +
			"reports every failure that way can look like this."
	}
	return "an http or exec check reads right after it writes, so an application that updates asynchronously or " +
		"serves a cached read for a few seconds can look like this; a `clickable` check judges one moment, so a " +
		"passing toast or spinner over the element can too; and an exec check's failed assertion is whatever its " +
		"program reports, so a program that reports every failure that way can look like this too."
}

// VerifyNoDefect is an acceptance-fix round's conclusion that the code has
// no defect: the run, maul's name for it, and the agent's one-line reason,
// agent text shown sanitised and bounded.
type VerifyNoDefect struct {
	Run, Reason string
}

// verifyNoDefectMaxBytes bounds the agent's reason a comment quotes.
const verifyNoDefectMaxBytes = 600

// noDefectNote is the note on a no-defect conclusion: the agent's reason as
// its own words, that it may be wrong, and what a human can do next.
func noDefectNote(n *VerifyNoDefect) string {
	var b strings.Builder
	b.WriteString("The automatic fix")
	if run := shown(n.Run); run != "" {
		b.WriteString(" " + code(run))
	}
	b.WriteString(" found no defect: its agent concluded that the code already does what the failing outcomes ask, " +
		"and changed nothing.")
	if r := SanitizeInline(strings.TrimSpace(cutRunes(visibleText(n.Reason), verifyNoDefectMaxBytes))); r != "" {
		b.WriteString(" Its reason, in its own words: " + r)
		if !strings.HasSuffix(r, ".") {
			b.WriteString(".")
		}
	}
	b.WriteString(" This is the agent's verdict and may be wrong, so maul stopped fixing on its own. " +
		slashCommand("verify") + " checks again (a failure caused by the preview rather than the code may pass), " +
		slashCommand("verify fix") + " also lets maul fix failures again, and an outcome that then passes only " +
		"on retry can be accepted with " + slashCommand("accept <id>") + ". Otherwise review the change and " +
		"decide.")
	return b.String()
}

func (s *VerifySection) empty() bool {
	return s == nil || (s.Progress.State == "" && len(s.Outcomes) == 0)
}

// VerifyComment is the verification comment on the intent's issue: one per
// round, edited per attempt, headed by NoticeMarker(Namespace, Intent,
// VerifyKey(Round)).
type VerifyComment struct {
	Namespace, Intent string
	Round, Attempt    int32
	Section           VerifySection
}

type verifyRow struct{ ID, Kind, IDCell, KindCell, Outcome, OutcomeCell, Statement string }

type verifyLogView struct{ Part, Block string }

// verifyView is a section as the templates print it.
type verifyView struct {
	Line  string
	Rows  []verifyRow
	Notes []string
	Logs  []verifyLogView
	// Cut is the note on what fitVerify left out; "" for nothing.
	Cut string
	// Outcomes are the outcomes' details, Anomalies the look-around's.
	Outcomes  []outcomeDetail
	Anomalies []string
	// reproduction is the suspected defect's reproduction, which ends the
	// note at Notes[reproNote] when it is shown (withEvidence).
	reproduction string
	reproNote    int
	// shortened: each outcome's statement is cut to
	// verifyStatementShortBytes (fitVerify's last resort).
	shortened bool
}

// verifySectionMaxBytes is the most a pull request's preview comment gives
// its verification section, keeping each comment, with every outcome's
// verdict, under MaxCommentBytes.
const verifySectionMaxBytes = 48 << 10

// cell keeps a value inside one table cell.
func cell(s string) string { return strings.ReplaceAll(s, "|", `\|`) }

// buildVerifyView is s as the templates print it, with all its evidence;
// short cuts each outcome's statement (fitVerify's last resort).
func buildVerifyView(s VerifySection, short bool) verifyView {
	v := verifyView{Line: verifyLine(s.Progress), reproNote: -1, shortened: short}
	outcomeViews(&v, s, short)
	if s.StatusNotPosted {
		v.Notes = append(v.Notes, "The maul App may not post commit statuses, so no `maul/acceptance` status is "+
			"posted: an administrator sets Commit statuses to Read and write in the App's settings and accepts the "+
			"change on the installation. The Project's `AcceptanceStatusUnavailable` condition names the repository.")
	}
	if s.NoDefect != nil {
		v.Notes = append(v.Notes, noDefectNote(s.NoDefect))
	}
	if s.Suspected != nil {
		v.reproNote, v.reproduction = len(v.Notes), suspectedReproduction(s.Suspected)
		v.Notes = append(v.Notes, suspectedText(s.Suspected)+v.reproduction)
	}
	if who := shown(s.Impostor); who != "" {
		v.Notes = append(v.Notes, "Another actor, "+code(who)+", posted a `maul/acceptance` status. "+
			"maul posted its own over it.")
	}
	for i, l := range s.Logs {
		if i == verifyLogsMax {
			break
		}
		tail := strings.TrimSpace(cutRunes(visibleText(l.Tail), VerifyLogMaxBytes))
		if tail == "" {
			continue
		}
		v.Logs = append(v.Logs, verifyLogView{Part: code(shown(l.Part)), Block: fence(tail)})
	}
	return v
}

// renderVerifySection renders the section for a pull request's preview
// comment; "" for none.
func renderVerifySection(s *VerifySection) (string, error) {
	if s.empty() {
		return "", nil
	}
	return fitVerify(*s, verifySectionMaxBytes, func(v verifyView) (string, error) {
		out, err := render("intent_verify_section.md.tmpl", v)
		return strings.TrimRight(out, "\n"), err
	})
}

// RenderVerifyComment renders the verification comment on the issue, held
// to GitHub's limit by fitVerify.
func RenderVerifyComment(c VerifyComment) (string, error) {
	return fitVerify(c.Section, MaxCommentBytes, func(v verifyView) (string, error) {
		return render("intent_verify_issue.md.tmpl", struct {
			Marker         string
			Round, Attempt int32
			verifyView
		}{NoticeMarker(c.Namespace, c.Intent, VerifyKey(c.Round)), c.Round, c.Attempt, v})
	})
}

// evidenceCutLead opens the note on what a verification comment left out.
const evidenceCutLead = "Left out to keep this comment within GitHub's size limit"

// verifyStatementShortBytes bounds each outcome's statement, as visible
// text, once a comment cannot fit with them whole (fitVerify's last
// resort): the approved plan's comment shows them whole.
const verifyStatementShortBytes = 160

// Evidence kinds, most important first: what fitVerify keeps first and
// leaves out last.
const (
	pieceReproduction = iota
	pieceAppOutput
	pieceAnomaly
	pieceLog
)

// evidencePiece is one piece of a view's evidence: its kind, its index in
// that kind's list, and what it adds to the comment, at most.
type evidencePiece struct{ kind, index, cost int }

// pieces are v's evidence, most important first: the suspected defect's
// reproduction, then each failing outcome's app output in plan order, then
// the look-around's anomalies, then the component log tails, each in
// order.
func (v verifyView) pieces() []evidencePiece {
	const overhead = 32 // the blank lines and words around a piece
	var ps []evidencePiece
	if v.reproduction != "" {
		ps = append(ps, evidencePiece{pieceReproduction, 0, len(v.reproduction) + overhead})
	}
	for i, o := range v.Outcomes {
		ps = append(ps, evidencePiece{pieceAppOutput, i, len(o.Lead) + len(o.AppOutput) + overhead})
	}
	for i, a := range v.Anomalies {
		ps = append(ps, evidencePiece{pieceAnomaly, i, len(a) + overhead})
	}
	for i, l := range v.Logs {
		ps = append(ps, evidencePiece{pieceLog, i, len(l.Part) + len(l.Block) + overhead})
	}
	return ps
}

// withEvidence is v showing only the pieces keep marks, and the note on
// what is left out.
func (v verifyView) withEvidence(ps []evidencePiece, keep []bool) verifyView {
	w := v
	w.Notes = slices.Clone(v.Notes)
	w.Outcomes, w.Anomalies, w.Logs = nil, nil, nil
	var left [pieceLog + 1]int
	for i, p := range ps {
		if !keep[i] {
			left[p.kind]++
			continue
		}
		switch p.kind {
		case pieceAppOutput:
			w.Outcomes = append(w.Outcomes, v.Outcomes[p.index])
		case pieceAnomaly:
			w.Anomalies = append(w.Anomalies, v.Anomalies[p.index])
		case pieceLog:
			w.Logs = append(w.Logs, v.Logs[p.index])
		}
	}
	if left[pieceReproduction] > 0 {
		w.Notes[v.reproNote] = strings.TrimSuffix(w.Notes[v.reproNote], v.reproduction)
	}
	w.Cut = cutNote(left, v.shortened)
	return w
}

// cutNote says what a comment left out, by kind ("" for nothing).
func cutNote(left [pieceLog + 1]int, shortened bool) string {
	var parts []string
	for _, k := range []struct {
		kind      int
		one, many string
	}{
		{pieceAppOutput, "app output", "app outputs"},
		{pieceAnomaly, "look-around note", "look-around notes"},
		{pieceLog, "component log tail", "component log tails"},
	} {
		if n := left[k.kind]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, plural(int32(n), k.one, k.many)))
		}
	}
	if left[pieceReproduction] > 0 {
		parts = append(parts, "maul's reproduction of the suspected defect")
	}
	if shortened {
		parts = append(parts, "the end of each outcome's statement (the approved plan's comment shows them whole)")
	}
	if len(parts) == 0 {
		return ""
	}
	return evidenceCutLead + ": " + strings.Join(parts, "; ") + "."
}

// fitVerify renders s within limit bytes however much evidence it carries.
// Whole when it fits; otherwise its evidence shares what is left of the
// comment once maul's own words are rendered (every outcome's verdict
// line, the progress line and the notes), kept most important first
// (pieces), each whole while it still fits (a large one left out does not
// stop a smaller one after it), and the comment names what it left out. Only
// when even that cannot fit is each outcome's statement shortened, the
// verdict lines kept whole. Deterministic: the same section always keeps
// the same evidence.
func fitVerify(s VerifySection, limit int, render func(verifyView) (string, error)) (string, error) {
	whole := buildVerifyView(s, false)
	out, err := render(whole)
	if err != nil || len(out) <= limit {
		return out, err
	}
	for _, short := range []bool{false, true} {
		v := buildVerifyView(s, short)
		out, ok, err := budgetEvidence(v, limit, render)
		if err != nil || ok {
			return out, err
		}
	}
	return "", fmt.Errorf("verification comment is over %d bytes even with no evidence and short statements",
		limit)
}

// budgetEvidence renders v within limit, keeping its evidence pieces most
// important first while each fits in what maul's own words leave; ok is
// false when those alone are over limit.
func budgetEvidence(v verifyView, limit int, render func(verifyView) (string, error)) (string, bool, error) {
	ps := v.pieces()
	keep := make([]bool, len(ps))
	bare, err := render(v.withEvidence(ps, keep))
	if err != nil || len(bare) > limit {
		return "", false, err
	}
	left := limit - len(bare)
	for i, p := range ps {
		if p.cost <= left {
			keep[i], left = true, left-p.cost
		}
	}
	for {
		out, err := render(v.withEvidence(ps, keep))
		if err != nil || len(out) <= limit {
			return out, err == nil, err
		}
		// A cost under-estimated: leave out the least important piece
		// kept, and render again.
		i := len(keep) - 1
		for i >= 0 && !keep[i] {
			i--
		}
		if i < 0 {
			return "", false, nil
		}
		keep[i] = false
	}
}

// PlanAcceptance is the plan comment's acceptance line, for a plan that was
// not asked for outcomes. nil omits the line, and the comment is exactly
// the comment before the field existed.
type PlanAcceptance struct {
	// NotAsked: the Project verifies, but previews no repository its
	// outcomes could be checked on, so the planner was not asked for them.
	NotAsked bool
}

// planAcceptanceLine is the acceptance line's text, "" for nil or for
// nothing to say.
func planAcceptanceLine(a *PlanAcceptance) string {
	if a == nil || !a.NotAsked {
		return ""
	}
	return "not asked for: this Project verifies, but previews no repository a criterion could target, so " +
		"maul could not ask the planner for criteria and none will be checked."
}

// PlanVerification is the Project's verification by the agent evaluator,
// as the plan prompt states it so the planner can write acceptance
// outcomes, plain statements of what must hold (intent-controller's
// planAsk; SPEC S1-11). nil omits the section, and the prompt is exactly
// the prompt without it. intent-controller hands it to a plan Job as
// MAUL_PLAN_VERIFICATION (EncodePlanVerification), and agent-runner reads
// it back (DecodePlanVerification).
type PlanVerification struct {
	// Components are the previewed repositories the outcomes are checked
	// on.
	Components []PlanVerifyComponent `json:"components"`
	// Workspace: the Project has no preview, and its outcomes are checked
	// on a cli or library surface the tool pod builds (SPEC-S2). It is
	// stated only with no Components; nil omits it from the JSON, which is
	// then exactly the JSON before the field existed.
	Workspace *PlanWorkspace `json:"workspace,omitempty"`
}

// PlanWorkspace is a Project's cli or library surface as the plan prompt
// states it: Surface SurfaceCLI with the Binary its build writes, or
// SurfaceLibrary with its Language, go; Repository is the repository built,
// as the Project spells its URL ("" for a Project of one repository).
type PlanWorkspace struct {
	Surface    string `json:"surface"`
	Repository string `json:"repository,omitempty"`
	Binary     string `json:"binary,omitempty"`
	Language   string `json:"language,omitempty"`
}

// valid reports a workspace the plan prompt can state: a cli with a binary
// a cli surface may name, or a go library; and a repository on one line.
func (w *PlanWorkspace) valid() bool {
	if w == nil || w.Repository != oneLine(w.Repository) {
		return false
	}
	switch w.Surface {
	case SurfaceCLI:
		return v1alpha1.ValidCLIBinary(w.Binary) && w.Language == ""
	case SurfaceLibrary:
		return w.Language == checks.LanguageGo && w.Binary == ""
	}
	return false
}

// PlanVerifyComponent is one previewed repository: its key, where it is
// served and its readiness path.
type PlanVerifyComponent struct {
	Key       string `json:"key"`
	Path      string `json:"path"`
	Readiness string `json:"readiness,omitempty"`
}

// Normalize is what of v a plan can be told, the one normaliser behind both
// the handoff (EncodePlanVerification) and the prompt (RenderPlanPrompt):
// each field on one line, only the components with a key and a path, at
// most planVerificationMaxComponents, in order; nil when no component is
// left, and the prompt then has no acceptance section. A valid Workspace
// is kept alone, with no component; an invalid one is dropped.
func (v *PlanVerification) Normalize() *PlanVerification {
	if v == nil {
		return nil
	}
	if v.Workspace.valid() {
		// A Project with no preview: its workspace, never components
		// (SPEC-S2 O6).
		w := *v.Workspace
		return &PlanVerification{Workspace: &w}
	}
	var out *PlanVerification
	for _, c := range v.Components {
		c = PlanVerifyComponent{Key: oneLine(c.Key), Path: oneLine(c.Path), Readiness: oneLine(c.Readiness)}
		if c.Key == "" || c.Path == "" {
			continue
		}
		if out == nil {
			out = &PlanVerification{}
		}
		if len(out.Components) < planVerificationMaxComponents {
			out.Components = append(out.Components, c)
		}
	}
	return out
}

// EncodePlanVerification is the JSON a plan Job hands agent-runner as
// MAUL_PLAN_VERIFICATION: v as the prompt states it (Normalize); "" for
// none.
func EncodePlanVerification(v *PlanVerification) string {
	v = v.Normalize()
	if v == nil {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		// Strings only: never.
		return ""
	}
	return string(b)
}

// DecodePlanVerification reads EncodePlanVerification's JSON back; "" is
// none. Anything EncodePlanVerification would not have written is refused:
// unknown fields, trailing data, no component or more than
// planVerificationMaxComponents, and a component without a key or a path or
// with a field that is not plain one-line text; and a workspace beside a
// component, or not valid.
func DecodePlanVerification(s string) (*PlanVerification, error) {
	if s == "" {
		return nil, nil
	}
	dec := json.NewDecoder(strings.NewReader(s))
	dec.DisallowUnknownFields()
	var v PlanVerification
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("plan verification: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("plan verification: trailing data after the object")
	}
	if v.Workspace != nil {
		if len(v.Components) > 0 || !v.Workspace.valid() {
			return nil, errors.New("plan verification: a workspace is stated alone, as a cli with " +
				"its binary or a go library")
		}
		return &v, nil
	}
	if n := len(v.Components); n == 0 || n > planVerificationMaxComponents {
		return nil, fmt.Errorf("plan verification: %d components, want 1 to %d", n, planVerificationMaxComponents)
	}
	for i, c := range v.Components {
		if c.Key == "" || c.Path == "" {
			return nil, fmt.Errorf("plan verification: component %d has no key or no path", i)
		}
		if c.Key != oneLine(c.Key) || c.Path != oneLine(c.Path) || c.Readiness != oneLine(c.Readiness) {
			return nil, fmt.Errorf("plan verification: component %d is not plain one-line text", i)
		}
	}
	return &v, nil
}

// planVerificationMaxComponents bounds the components the prompt lists.
const planVerificationMaxComponents = 8
