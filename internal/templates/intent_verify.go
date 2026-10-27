// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"fmt"
	"strings"
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
	// VerifyNeedsReplan: an approved criterion cannot run as approved, so
	// only a replan can change it.
	VerifyNeedsReplan = "needs-replan"
	// VerifyNotDeclared: the approved plan declares no acceptance criteria.
	VerifyNotDeclared = "not-declared"
	// VerifyNotChecked: the approved plan declares criteria, none of which
	// maul checks automatically.
	VerifyNotChecked = "not-checked"
	// VerifyUnreadable: the approved plan's acceptance criteria cannot be
	// read, so nothing is verified until a plan is approved again.
	VerifyUnreadable = "unreadable"
)

// Automatic fix states.
const (
	FixQueued  = "queued"
	FixRunning = "running"
	FixHalted  = "halted"
)

// Criterion outcomes as a comment shows them.
const (
	VerifyOutcomePass         = "pass"
	VerifyOutcomeFail         = "fail"
	VerifyOutcomeQuarantined  = "quarantined"
	VerifyOutcomeNotRun       = "not-run"
	VerifyOutcomeManual       = "manual"
	VerifyOutcomeAccepted     = "accepted"
	VerifyOutcomeChangedTests = "changed-tests"
)

// verifyOutcomeWords are the outcomes in maul's words; an outcome not listed
// shows as unknown, never as the text it carried.
var verifyOutcomeWords = map[string]string{
	VerifyOutcomePass:         "passed",
	VerifyOutcomeFail:         "failed",
	VerifyOutcomeQuarantined:  "quarantined: passed only on retry, not counted as passed",
	VerifyOutcomeNotRun:       "not run yet",
	VerifyOutcomeManual:       "needs a human",
	VerifyOutcomeAccepted:     "accepted by a human, not re-checked",
	VerifyOutcomeChangedTests: "pending: the change edits the tests this check relies on",
}

// Bounds on what a comment quotes.
const (
	// VerifyEvidenceMaxBytes bounds one failure's expected, observed and
	// body prefix together.
	VerifyEvidenceMaxBytes = 2048
	// verifyValueMaxBytes bounds the expected and the observed value each.
	verifyValueMaxBytes = 512
	// VerifyLogMaxBytes bounds one component's log tail; verifyLogsMax the
	// tails a comment carries.
	VerifyLogMaxBytes = 3072
	verifyLogsMax     = 8
	// verifyReasonMaxBytes bounds an auto-fix reason and a manual
	// criterion's reason.
	verifyReasonMaxBytes = 120
)

// VerifyProgress is where verification stands, as one clause.
type VerifyProgress struct {
	// State is one of the Verify state constants; "" shows nothing.
	State string
	// Round and MaxRounds count verification rounds (MaxRounds 0 omits the
	// limit).
	Round, MaxRounds int32
	// Passing of Total automated checks pass; Total 0 omits the count.
	Passing, Total int32
	// Quarantined and ManualOpen count checks held for a human.
	Quarantined, ManualOpen int32
	// AutoFix is "", FixQueued, FixRunning or FixHalted; AutoFixReason is
	// the controller's short fixed reason for a halt.
	AutoFix       string
	AutoFixReason string
}

// verifyRestingLine is the sentence of a state nothing is checked in:
// whole for one that never carries counts, else its head; "" for any other
// state.
func verifyRestingLine(state string) (line string, whole bool) {
	switch state {
	case VerifyWaitingForCI:
		return "Verification waits for the pull requests' checks to pass", false
	case VerifyNeedsReplan:
		return "Verification cannot run: an approved check's target is not previewed, so only " +
			slashCommand("replan") + " can change it.", true
	case VerifyNotDeclared:
		return "No acceptance criteria were declared.", true
	case VerifyUnreadable:
		return "Verification stopped: maul cannot read the approved plan's acceptance criteria, so nothing is " +
			"verified. " + slashCommand("replan") + " to approve a plan again.", true
	case VerifyNotChecked:
		return "Nothing is checked automatically", false
	case VerifyPreviewNotLive:
		return "Verification unavailable: the preview did not become live at the pull requests' heads in time, so " +
			"the checks could not run. Verification resumes once the preview is live; `kubectl get preview` " +
			"says why it is not.", true
	}
	return "", false
}

// verifyLine is the progress as one sentence; "" for none.
func verifyLine(p VerifyProgress) string {
	head, whole := verifyRestingLine(p.State)
	if whole {
		return head
	}
	rounds := fmt.Sprintf("round %d", p.Round)
	if p.MaxRounds > 0 {
		rounds += fmt.Sprintf(" of %d", p.MaxRounds)
	}
	counts := ""
	if p.Total > 0 {
		counts = fmt.Sprintf("%d/%d checks passing", p.Passing, p.Total)
	}
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
	case VerifyRunning:
		head = "Verifying: " + join(rounds, counts)
	case VerifyPassed:
		head = "Verified"
		if p.Total > 0 {
			head = fmt.Sprintf("Verified: %d/%d checks passed", p.Passing, p.Total)
		}
	case VerifyFailing:
		head = "Verification failing: " + join(counts, rounds)
	case VerifyExhausted:
		head = "Verification failing: " + join(counts, "no automatic fix left")
	case VerifyUnavailable:
		head = "Verification unavailable: maul could not run the checks"
	default:
		return ""
	}
	if p.Quarantined > 0 {
		head += fmt.Sprintf(", %d quarantined", p.Quarantined)
	}
	if p.ManualOpen > 0 {
		head += fmt.Sprintf(", %d for a human", p.ManualOpen)
	}
	switch p.AutoFix {
	case FixQueued, FixRunning:
		head += "; automatic fix " + p.AutoFix
	case FixHalted:
		head += "; automatic fix halted"
		if r := shown(cutRunes(visibleText(p.AutoFixReason), verifyReasonMaxBytes)); r != "" {
			head += " (" + code(r) + ")"
		}
	}
	return head + "."
}

// VerifyCriterion is one acceptance criterion's state.
type VerifyCriterion struct {
	// ID, Kind and Statement are the approved criterion's.
	ID, Kind, Statement string
	// Outcome is a VerifyOutcome constant.
	Outcome string
	// Expected, Observed and Body describe a failure: maul-rendered
	// expected and observed values, and a prefix of the response body.
	Expected, Observed, Body string
	// AcceptedAt is the head set a human accepted the criterion at.
	AcceptedAt string
}

// VerifyLog is one component's log tail (already redacted by the caller).
type VerifyLog struct {
	Part, Tail string
}

// VerifySection is the verification as a comment shows it. A zero Progress
// and no criteria show nothing.
type VerifySection struct {
	Progress VerifyProgress
	Criteria []VerifyCriterion
	// Logs are shown only when the caller passes them: the issue always,
	// a pull request only under the Project's postLogs.
	Logs []VerifyLog
	// ChangedTests: the change edits tests a check relies on.
	ChangedTests bool
	// ServicesRestarted: a service pod restarted and lost its data.
	ServicesRestarted bool
	// Impostor is the login that posted a maul/acceptance status other than
	// maul's, which maul re-posted over; "" for none.
	Impostor string
	// StatusNotPosted: the maul App may not post commit statuses, so no
	// maul/acceptance status is posted.
	StatusNotPosted bool
}

func (s *VerifySection) empty() bool {
	return s == nil || (s.Progress.State == "" && len(s.Criteria) == 0)
}

// VerifyComment is the verification comment on the intent's issue: one per
// round, edited per attempt, headed by NoticeMarker(Namespace, Intent,
// VerifyKey(Round)).
type VerifyComment struct {
	Namespace, Intent string
	Round, Attempt    int32
	Section           VerifySection
}

type verifyRow struct{ ID, Kind, IDCell, KindCell, Outcome, Statement string }

type verifyFailure struct{ ID, Expected, Observed, Body string }

type verifyLogView struct{ Part, Block string }

// verifyView is a section as the templates print it.
type verifyView struct {
	Line     string
	Rows     []verifyRow
	Failures []verifyFailure
	Notes    []string
	Logs     []verifyLogView
	LogsCut  bool
}

// cell keeps a value inside one table cell.
func cell(s string) string { return strings.ReplaceAll(s, "|", `\|`) }

func buildVerifyView(s VerifySection, withLogs bool) verifyView {
	v := verifyView{Line: verifyLine(s.Progress)}
	for _, c := range s.Criteria {
		out, ok := verifyOutcomeWords[c.Outcome]
		if !ok {
			out = "unknown"
		}
		if c.Outcome == VerifyOutcomeAccepted && shown(c.AcceptedAt) != "" {
			out = "accepted by a human at " + shortRevision(c.AcceptedAt) + ", not re-checked"
		}
		v.Rows = append(v.Rows, verifyRow{
			ID: code(shown(c.ID)), Kind: code(shown(c.Kind)), Outcome: out, Statement: SanitizeInline(c.Statement),
			IDCell: cell(code(shown(c.ID))), KindCell: cell(code(shown(c.Kind))),
		})
		if c.Outcome == VerifyOutcomeFail {
			if f, ok := failureView(c); ok {
				v.Failures = append(v.Failures, f)
			}
		}
	}
	if s.ChangedTests {
		v.Notes = append(v.Notes, "This change edits tests that a check relies on, so maul does not count that "+
			"check as passed until a human accepts it with "+slashCommand("accept <id>")+".")
	}
	if s.ServicesRestarted {
		v.Notes = append(v.Notes, "Services restarted: a service pod restarted and lost its data, "+
			"so the checks ran against fresh services.")
	}
	if s.StatusNotPosted {
		v.Notes = append(v.Notes, "The maul App may not post commit statuses, so no `maul/acceptance` status is "+
			"posted: an administrator sets Commit statuses to Read and write in the App's settings and accepts the "+
			"change on the installation. The Project's `AcceptanceStatusUnavailable` condition names the repository.")
	}
	if who := shown(s.Impostor); who != "" {
		v.Notes = append(v.Notes, "Another actor, "+code(who)+", posted a `maul/acceptance` status. "+
			"maul posted its own over it.")
	}
	if withLogs {
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
	}
	return v
}

// failureView bounds one failure's evidence: the expected and observed
// values each, then the body prefix in what is left of the budget.
func failureView(c VerifyCriterion) (verifyFailure, bool) {
	exp := shown(cutRunes(visibleText(c.Expected), verifyValueMaxBytes))
	obs := shown(cutRunes(visibleText(c.Observed), verifyValueMaxBytes))
	left := VerifyEvidenceMaxBytes - len(exp) - len(obs)
	body := ""
	if left > 0 {
		body = strings.TrimSpace(cutRunes(visibleText(c.Body), left))
	}
	if exp == "" && obs == "" && body == "" {
		return verifyFailure{}, false
	}
	f := verifyFailure{ID: code(shown(c.ID))}
	if exp != "" {
		f.Expected = code(exp)
	}
	if obs != "" {
		f.Observed = code(obs)
	}
	if body != "" {
		f.Body = fence(body)
	}
	return f, true
}

// renderVerifySection renders the section for a pull request's preview
// comment; "" for none.
func renderVerifySection(s *VerifySection) (string, error) {
	if s.empty() {
		return "", nil
	}
	out, err := render("intent_verify_section.md.tmpl", buildVerifyView(*s, true))
	return strings.TrimRight(out, "\n"), err
}

// RenderVerifyComment renders the verification comment on the issue. A
// comment over GitHub's limit is rendered again without its log tails and
// says so.
func RenderVerifyComment(c VerifyComment) (string, error) {
	for _, logs := range []bool{true, false} {
		v := buildVerifyView(c.Section, logs)
		v.LogsCut = !logs && len(c.Section.Logs) > 0
		out, err := render("intent_verify_issue.md.tmpl", struct {
			Marker         string
			Round, Attempt int32
			verifyView
		}{NoticeMarker(c.Namespace, c.Intent, VerifyKey(c.Round)), c.Round, c.Attempt, v})
		if err != nil {
			return "", err
		}
		if len(out) <= MaxCommentBytes {
			return out, nil
		}
	}
	return "", fmt.Errorf("verification comment of round %d is over %d bytes even without its log tails",
		c.Round, MaxCommentBytes)
}

// PlanAcceptance is a plan's acceptance criteria as the plan comment's header
// counts them, from the parsed frontmatter only. nil omits the line, and the
// comment is exactly the comment before the field existed.
type PlanAcceptance struct {
	// Off: verification is off for the Project.
	Off bool
	// Automated counts the criteria maul checks (http); NotRun the ones it
	// parses and shows but does not run until their slice (command).
	Automated, NotRun int
	// Manual are the criteria that need a human.
	Manual []PlanManualCriterion
}

// PlanManualCriterion is a criterion a human closes.
type PlanManualCriterion struct{ ID, Reason string }

// planAcceptanceLine is the acceptance line's text, "" for nil.
func planAcceptanceLine(a *PlanAcceptance) string {
	if a == nil {
		return ""
	}
	if a.Off {
		return "not checked: verification is off for this Project."
	}
	var parts []string
	if a.Automated > 0 {
		parts = append(parts, fmt.Sprintf("%d checked automatically", a.Automated))
	}
	if a.NotRun > 0 {
		parts = append(parts, fmt.Sprintf("%d not run until command checks ship", a.NotRun))
	}
	if n := len(a.Manual); n > 0 {
		items := make([]string, n)
		for i, m := range a.Manual {
			items[i] = SanitizeInline(m.ID)
			if r := SanitizeInline(cutRunes(visibleText(m.Reason), verifyReasonMaxBytes)); r != "" {
				items[i] += " (" + r + ")"
			}
		}
		need := "1 needs you: "
		if n > 1 {
			need = fmt.Sprintf("%d need you: ", n)
		}
		parts = append(parts, need+strings.Join(items, ", "))
	}
	if len(parts) == 0 {
		return "the plan declares no acceptance criteria."
	}
	return strings.Join(parts, ", ") + "."
}

// PlanVerification is the Project's verification, as the plan prompt states
// it so the planner can write acceptance criteria. nil omits the section,
// and the prompt is exactly the prompt without it.
type PlanVerification struct {
	// Components are the previewed repositories criteria can target.
	Components []PlanVerifyComponent
}

// PlanVerifyComponent is one previewed repository: the key a criterion's
// target names, where it is served and its readiness path.
type PlanVerifyComponent struct{ Key, Path, Readiness string }

// planVerificationMaxComponents bounds the components the prompt lists.
const planVerificationMaxComponents = 8

func (v *PlanVerification) quotable() *PlanVerification {
	if v == nil {
		return nil
	}
	out := &PlanVerification{}
	for i, c := range v.Components {
		if i == planVerificationMaxComponents {
			break
		}
		out.Components = append(out.Components, PlanVerifyComponent{
			Key: oneLine(c.Key), Path: oneLine(c.Path), Readiness: oneLine(c.Readiness),
		})
	}
	return out
}
