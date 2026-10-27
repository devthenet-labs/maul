// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	VerifyOutcomePass        = "pass"
	VerifyOutcomeFail        = "fail"
	VerifyOutcomeQuarantined = "quarantined"
	VerifyOutcomeNotRun      = "not-run"
	VerifyOutcomeManual      = "manual"
	VerifyOutcomeAccepted    = "accepted"
)

// verifyOutcomeWords are the outcomes in maul's words; an outcome not listed
// shows as unknown, never as the text it carried.
var verifyOutcomeWords = map[string]string{
	VerifyOutcomePass:        "passed",
	VerifyOutcomeFail:        "failed",
	VerifyOutcomeQuarantined: "quarantined: passed only on retry, not counted as passed",
	VerifyOutcomeNotRun:      "not run yet",
	VerifyOutcomeManual:      "needs a human",
	VerifyOutcomeAccepted:    "accepted by a human, not re-checked",
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
	// Verification is the verification's own number, as its comment's
	// header names it (0 omits it). FixesUsed of MaxFixes count the
	// automatic fix rounds spent against the Project's maxVerifyFixes;
	// they are never called rounds of verification, so the two numbers
	// cannot be read as one.
	Verification, FixesUsed, MaxFixes int32
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
	fixes := fmt.Sprintf("%d of %d automatic %s used", p.FixesUsed, p.MaxFixes, plural(p.MaxFixes, "fix", "fixes"))
	which := "Verification"
	if p.Verification > 0 {
		which = fmt.Sprintf("Verification %d", p.Verification)
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
		head = which + " running: " + join(counts, fixes)
	case VerifyPassed:
		head = which + " passed"
		if p.Total > 0 {
			head = fmt.Sprintf("%s passed: %d/%d checks passed", which, p.Passing, p.Total)
		}
	case VerifyFailing:
		head = which + " failing: " + join(counts, fixes)
	case VerifyExhausted:
		head = which + " failing: " + join(counts, "no automatic fix left")
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

// plural is one when n is 1, else many.
func plural(n int32, one, many string) string {
	if n == 1 {
		return one
	}
	return many
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
	// Mismatches are the parts of the failing step that did not hold.
	Mismatches []VerifyMismatch
	// AcceptedAt is the head set a human accepted the criterion at.
	AcceptedAt string
}

// VerifyMismatch is one part of a failing step that did not hold: its kind
// (one of the Mismatch constants), the pointer, header or capture it names,
// and the expected and observed values, observed text being the
// application's, shown inert and bounded.
type VerifyMismatch struct {
	Kind, Key, Expected, Observed string
	// Absent: nothing was observed there at all.
	Absent bool
}

// Mismatch kinds, as a failure names them.
const (
	MismatchRequest = "request"
	MismatchJSON    = "JSON"
	MismatchHeader  = "header"
	MismatchBody    = "body"
	MismatchCapture = "capture"
)

// Bounds on the mismatches a failure lists.
const (
	verifyMismatchesMax         = 8
	verifyMismatchValueMaxBytes = 256
)

// mismatchLine is one mismatch in maul's words; "" for an unknown kind.
func mismatchLine(m VerifyMismatch) string {
	val := func(s string) string { return code(shown(cutRunes(visibleText(s), verifyMismatchValueMaxBytes))) }
	switch m.Kind {
	case MismatchBody:
		return "body: expected it to contain " + val(m.Expected) + "."
	case MismatchRequest, MismatchJSON, MismatchHeader, MismatchCapture:
	default:
		return ""
	}
	head := m.Kind
	if k := shown(cutRunes(visibleText(m.Key), verifyMismatchValueMaxBytes)); k != "" {
		head += " " + code(k)
	}
	line := head + ": expected " + val(m.Expected)
	if m.Absent {
		return line + ", observed nothing there."
	}
	return line + ", observed " + val(m.Observed) + "."
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
	// Impostor is the login that posted a maul/acceptance status other than
	// maul's, which maul re-posted over; "" for none.
	Impostor string
	// StatusNotPosted: the maul App may not post commit statuses, so no
	// maul/acceptance status is posted.
	StatusNotPosted bool
	// NoDefect is the acceptance-fix round that concluded the code has no
	// defect, which halted automatic fixes; nil for none.
	NoDefect *VerifyNoDefect
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
	b.WriteString(" found no defect: its agent concluded that the code already does what the failing criteria ask, " +
		"and changed nothing.")
	if r := SanitizeInline(strings.TrimSpace(cutRunes(visibleText(n.Reason), verifyNoDefectMaxBytes))); r != "" {
		b.WriteString(" Its reason, in its own words: " + r)
		if !strings.HasSuffix(r, ".") {
			b.WriteString(".")
		}
	}
	b.WriteString(" This is the agent's verdict and may be wrong, so maul stopped fixing on its own. " +
		slashCommand("verify") + " checks again (a failure caused by the preview rather than the code may pass), " +
		slashCommand("verify fix") + " also lets maul fix failures again, and a criterion that then passes only " +
		"on retry can be accepted with " + slashCommand("accept <id>") + ". Otherwise review the change and " +
		"decide.")
	return b.String()
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

type verifyFailure struct {
	ID, Expected, Observed, Body string
	Mismatches                   []string
}

type verifyLogView struct{ Part, Block string }

// verifyView is a section as the templates print it.
type verifyView struct {
	Line     string
	Rows     []verifyRow
	Failures []verifyFailure
	Notes    []string
	Logs     []verifyLogView
	LogsCut  bool
	// Omitted counts the pieces of failure evidence left out to keep the
	// comment within its evidence budget (budgetEvidence).
	Omitted int
}

// Bounds on a whole comment. verifyCommentEvidenceBytes is the one budget
// a comment's failure evidence shares (expected and observed values,
// mismatch lines and body prefixes, as rendered); verifySectionMaxBytes the
// most a pull request's preview comment gives its verification section
// before its log tails are left out. Both keep each comment, with every
// criterion's verdict, under MaxCommentBytes.
const (
	verifyCommentEvidenceBytes = 24 << 10
	verifySectionMaxBytes      = 48 << 10
)

// budgetEvidence keeps the failures' evidence within
// verifyCommentEvidenceBytes, in plan order: each failure's expected and
// observed values, then its mismatch lines, then its body prefix, each kept
// only while it fits; what does not is left out and counted. The verdicts
// (the table, and each failure's own line) are never cut. Deterministic: the
// same failures always keep the same evidence.
func budgetEvidence(fs []verifyFailure) ([]verifyFailure, int) {
	left, omitted := verifyCommentEvidenceBytes, 0
	take := func(n int) bool {
		if n > left {
			omitted++
			return false
		}
		left -= n
		return true
	}
	out := make([]verifyFailure, len(fs))
	for i, f := range fs {
		if (f.Expected != "" || f.Observed != "") && !take(len(f.Expected)+len(f.Observed)+32) {
			f.Expected, f.Observed = "", ""
		}
		var kept []string
		for _, m := range f.Mismatches {
			if take(len(m) + 4) {
				kept = append(kept, m)
			}
		}
		f.Mismatches = kept
		if f.Body != "" && !take(len(f.Body)+2) {
			f.Body = ""
		}
		out[i] = f
	}
	return out, omitted
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
	v.Failures, v.Omitted = budgetEvidence(v.Failures)
	if s.StatusNotPosted {
		v.Notes = append(v.Notes, "The maul App may not post commit statuses, so no `maul/acceptance` status is "+
			"posted: an administrator sets Commit statuses to Read and write in the App's settings and accepts the "+
			"change on the installation. The Project's `AcceptanceStatusUnavailable` condition names the repository.")
	}
	if s.NoDefect != nil {
		v.Notes = append(v.Notes, noDefectNote(s.NoDefect))
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
	var mismatches []string
	for _, m := range c.Mismatches {
		if len(mismatches) == verifyMismatchesMax {
			break
		}
		if line := mismatchLine(m); line != "" {
			mismatches = append(mismatches, line)
		}
	}
	if exp == "" && obs == "" && body == "" && len(mismatches) == 0 {
		return verifyFailure{}, false
	}
	f := verifyFailure{ID: code(shown(c.ID)), Mismatches: mismatches}
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
	for _, logs := range []bool{true, false} {
		v := buildVerifyView(*s, logs)
		v.LogsCut = !logs && len(s.Logs) > 0
		out, err := render("intent_verify_section.md.tmpl", v)
		if err != nil {
			return "", err
		}
		if out = strings.TrimRight(out, "\n"); len(out) <= verifySectionMaxBytes {
			return out, nil
		}
	}
	return "", fmt.Errorf("verification section is over %d bytes even without its log tails",
		verifySectionMaxBytes)
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
	// NotAsked: the Project verifies, but previews no repository a
	// criterion could target, so the planner was not asked for criteria.
	NotAsked bool
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
	if a.NotAsked {
		return "not asked for: this Project verifies, but previews no repository a criterion could target, so " +
			"maul could not ask the planner for criteria and none will be checked."
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
// and the prompt is exactly the prompt without it. intent-controller hands
// it to a plan Job as MAUL_PLAN_VERIFICATION (EncodePlanVerification), and
// agent-runner reads it back (DecodePlanVerification).
type PlanVerification struct {
	// Components are the previewed repositories criteria can target.
	Components []PlanVerifyComponent `json:"components"`
}

// PlanVerifyComponent is one previewed repository: the key a criterion's
// target names, where it is served and its readiness path.
type PlanVerifyComponent struct {
	Key       string `json:"key"`
	Path      string `json:"path"`
	Readiness string `json:"readiness,omitempty"`
}

// Normalize is what of v a plan can be told, the one normaliser behind both
// the handoff (EncodePlanVerification) and the prompt (RenderPlanPrompt):
// each field on one line, only the components with a key and a path, at
// most planVerificationMaxComponents, in order; nil when none is left, and
// the prompt then has no acceptance section.
func (v *PlanVerification) Normalize() *PlanVerification {
	if v == nil {
		return nil
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
// with a field that is not plain one-line text.
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
