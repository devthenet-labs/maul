// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package envelope

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Prefix marks an event line on the agent-runner's stdout.
const Prefix = "MAUL-EVENT: "

// Version is the current envelope schema version. Version 2 replaced the
// git-bundle payload with the structured Changeset; version 3 added the
// investigation event (exploitability/likelihood/impact analyses) and keys
// events by finding name; version 4 turned the investigation's max_turns and
// token_budget into the unclamped estimated_max_turns/estimated_token_budget
// and added hold_reasons beside await_approval.
//
// New event types are additive within a version: Decode accepts any
// non-empty type at Version and every collector switches on the type,
// ignoring the ones it does not own, so the intent plan event joined
// version 4 without a bump, and the evaluator's evaluation and checkreview
// events after it.
const Version = 4

// Type discriminates events.
type Type string

// The event types: one per completed stage, plus fatal for a runner that
// could not produce a stage result at all. An intent's build stage reports
// with TypeRemediation — it produces the same changeset payload — and its
// plan stage with TypePlan; the evaluator's evaluate stage reports with
// TypeEvaluation and its check reviewer with TypeCheckReview.
const (
	TypeInvestigation Type = "investigation"
	TypeRemediation   Type = "remediation"
	TypePlan          Type = "plan"
	TypeEvaluation    Type = "evaluation"
	TypeCheckReview   Type = "checkreview"
	TypeFatal         Type = "fatal"
)

// Outcome describes how a stage ended.
type Outcome string

// Stage outcomes. Only OutcomeOK carries a trusted report; every other
// outcome routes the issue to humans.
const (
	OutcomeOK                Outcome = "ok"
	OutcomeRuntimeError      Outcome = "runtime_error"
	OutcomeTimeout           Outcome = "timeout"
	OutcomeBudgetExceeded    Outcome = "budget_exceeded"
	OutcomeReportMissing     Outcome = "report_missing"
	OutcomeReportInvalid     Outcome = "report_invalid"
	OutcomeCommitFailed      Outcome = "commit_failed"
	OutcomeChangesetTooLarge Outcome = "changeset_too_large"
	// OutcomeImageIncompatible is the preflight's verdict on a
	// repository-declared runner image: the injected claude CLI, git or bash
	// failed to run there (a musl or bash-less image), so the stage never
	// started. It is a compatibility check, not an integrity one — the image
	// supplies the loader and libc the CLI runs on.
	OutcomeImageIncompatible Outcome = "image_incompatible"
	// OutcomeChangesetRejected is set by the remediation collector, never
	// emitted by the pod: the changeset arrived intact but failed
	// controller-side validation (the entry cap, path shape, or CI
	// definitions on a repository-image run), and zero forge calls were
	// made. Distinct from OutcomeChangesetTooLarge, the pod's own byte cap.
	OutcomeChangesetRejected Outcome = "changeset_rejected"
)

// Usage is the stage's agent accounting (all fields concrete: the envelope
// reports what was measured, zeros where the harness didn't say).
type Usage struct {
	InputTokens         int     `json:"input_tokens"`
	OutputTokens        int     `json:"output_tokens"`
	CacheReadTokens     int     `json:"cache_read_tokens"`
	CacheCreationTokens int     `json:"cache_creation_tokens"`
	CostUSD             float64 `json:"cost_usd"`
}

// Stage carries what every stage reports regardless of kind.
type Stage struct {
	Outcome   Outcome `json:"outcome"`
	Harness   string  `json:"harness"`
	Model     string  `json:"model"`
	SessionID string  `json:"session_id,omitempty"`
	NumTurns  int     `json:"num_turns,omitempty"`
	// FirstEditTurn is the 1-based agent turn on which the main agent run
	// first used a file-editing tool, 0 when it edited nothing or the
	// harness cannot tell. Unlike NumTurns, report-repair rounds never add
	// to it. Additive within version 4: an older reader ignores it.
	FirstEditTurn  int     `json:"first_edit_turn,omitempty"`
	Usage          Usage   `json:"usage"`
	ElapsedSeconds float64 `json:"elapsed_seconds"`
	// Detail explains a non-ok outcome for humans.
	Detail string `json:"detail,omitempty"`
}

// AnalysisResult is one investigation dimension: a rating plus a short
// justification (the full reasoning lives in the report markdown).
type AnalysisResult struct {
	// Rating is none|low|medium|high|critical.
	Rating string `json:"rating"`
	// Summary is the agent's short justification.
	Summary string `json:"summary,omitempty"`
}

// Investigation is the analysis-stage event payload (version 4): the agent's
// exploitability, likelihood, and impact assessments plus its verdict. The
// runner never decides continuation — the controller routes on the verdict.
type Investigation struct {
	Stage
	ReportMarkdown string         `json:"report_markdown,omitempty"`
	Exploitability AnalysisResult `json:"exploitability"`
	Likelihood     AnalysisResult `json:"likelihood"`
	Impact         AnalysisResult `json:"impact"`
	Recommendation string         `json:"recommendation,omitempty"`
	Priority       string         `json:"priority,omitempty"`
	Severity       string         `json:"severity,omitempty"`
	Confidence     float64        `json:"confidence,omitempty"`
	// RemediationModel is the stage-2 model the analysis suggested; the
	// controller clamps it to the allowlist before launching the job.
	RemediationModel string `json:"remediation_model,omitempty"`
	// EstimatedMaxTurns/EstimatedTokenBudget are the analysis's PREDICTION of
	// what the fix will cost, reported verbatim. They never bind the
	// remediation — it runs on the grant the controller resolves — and they
	// are never clamped, because the approval gate and the calibration
	// averages both read the raw prediction.
	EstimatedMaxTurns    int `json:"estimated_max_turns,omitempty"`
	EstimatedTokenBudget int `json:"estimated_token_budget,omitempty"`
	// AwaitApproval marks an approval hold; HoldReasons says why.
	AwaitApproval bool         `json:"await_approval"`
	HoldReasons   []HoldReason `json:"hold_reasons,omitempty"`
}

// HoldReason names why a remediate verdict must wait for a human. It mirrors
// v1alpha1.HoldReason; the controller maps one onto the other.
type HoldReason string

// Approval-hold reasons the runner can raise. Low confidence is absent by
// design: the controller owns that threshold, not the pod.
const (
	HoldBreakingChangeAvailable HoldReason = "breakingChangeAvailable"
	HoldExceedsAutomatedTurns   HoldReason = "exceedsAutomatedTurns"
	HoldExceedsAutomatedTokens  HoldReason = "exceedsAutomatedTokens"
)

// FileChange is one file created or modified on the remediation branch.
type FileChange struct {
	Path string `json:"path"`
	// Mode is the git file mode: "100644", "100755", or "120000".
	Mode string `json:"mode"`
	// ContentB64 is the base64-encoded blob; for a symlink ("120000") it is
	// the encoded link target.
	ContentB64 string `json:"content_b64"`
}

// Changeset expresses the agent's committed change as file contents so the
// controller can push it through the GitHub API without git.
type Changeset struct {
	// BaseSHA is the commit the clone was pinned to; the pushed commit's
	// parent.
	BaseSHA string `json:"base_sha"`
	// CommitMessage carries the agent's commit message(s); multiple local
	// commits squash into one API commit.
	CommitMessage string       `json:"commit_message"`
	Upserts       []FileChange `json:"upserts,omitempty"`
	Deletes       []string     `json:"deletes,omitempty"`
}

// Remediation is the stage-2 event payload, and an intent build's: the
// stage's report and the changeset the controller pushes.
type Remediation struct {
	Stage
	ReportMarkdown string  `json:"report_markdown,omitempty"`
	Success        bool    `json:"success"`
	Confidence     float64 `json:"confidence,omitempty"`
	// Branch is the local branch carrying the fix; Changeset is its content
	// diffed against Changeset.BaseSHA. An intent build leaves Branch empty
	// and reports no confidence: the intent controller names the pushed
	// branch itself (maul-intent/<intent>), and a build report carries
	// its tests rather than a confidence.
	Branch    string     `json:"branch,omitempty"`
	Changeset *Changeset `json:"changeset,omitempty"`
	// Reproduction is the Finding remediation's account of proving the bug
	// with its regression test, from the report's reproduction block; nil
	// when the report has none, and always on an intent build. Additive
	// within version 4: an older reader ignores it, and a newer one reads
	// its absence as "not reported".
	Reproduction *Reproduction `json:"reproduction,omitempty"`
}

// Reproduction is what a remediation agent reports about reproducing the
// bug before fixing it (report.Reproduction): self-reported, bounded by the
// runner, and bounded again by the controller, which cannot trust a pod
// running a repository-declared image.
type Reproduction struct {
	// Reproduced: the regression test failed on the unfixed code for the
	// expected reason and passes with the fix.
	Reproduced bool `json:"reproduced"`
	// Command ran the regression test.
	Command string `json:"command,omitempty"`
	// Before and After are short excerpts of its output without and with
	// the fix.
	Before string `json:"before,omitempty"`
	After  string `json:"after,omitempty"`
}

// Plan is an intent's plan-stage event payload: the plan report exactly as
// the agent wrote it, frontmatter included, plus that frontmatter parsed.
// The report is the contract — it is what the approver reads and what the
// build follows — so a controller re-derives the fields from it
// (agentresult.FromPlan) rather than trusting these; they ride the event so
// a reader of the log sees the plan's shape without parsing markdown.
type Plan struct {
	Stage
	ReportMarkdown  string   `json:"report_markdown,omitempty"`
	Summary         string   `json:"summary,omitempty"`
	Repositories    []string `json:"repositories,omitempty"`
	NewDependencies []string `json:"new_dependencies,omitempty"`
	Questions       []string `json:"questions,omitempty"`
	Confidence      float64  `json:"confidence,omitempty"`
	// EstimatedMaxTurns/EstimatedTokenBudget are the plan's PREDICTION of
	// what its build will spend, reported verbatim and never binding.
	EstimatedMaxTurns    int `json:"estimated_max_turns,omitempty"`
	EstimatedTokenBudget int `json:"estimated_token_budget,omitempty"`
}

// Evaluation is an intent evaluate stage's event payload: the evaluation
// report exactly as the agent wrote it, its frontmatter parsed, and the
// ledger entries agent-runner chose to have the tools container emit as
// evidence. Every field is the model's claim: a controller re-derives the
// claims from the report (agentresult.FromEvaluation) and takes every fact
// (what ran, what passed, which bytes are evidence) from the tools ledger,
// never from this event, which a codex pod can write itself.
type Evaluation struct {
	Stage
	ReportMarkdown string           `json:"report_markdown,omitempty"`
	Verdicts       []OutcomeVerdict `json:"verdicts,omitempty"`
	Disputes       []Dispute        `json:"disputes,omitempty"`
	// Proposed names the checks the agent asks maul to save: an id, the
	// outcome it is bound to and the digest of its body. Bodies never
	// travel here; the tools ledger holds the one that was dry-run.
	Proposed  []ProposedCheck `json:"proposed,omitempty"`
	Retire    []Retire        `json:"retire,omitempty"`
	Anomalies []Anomaly       `json:"anomalies,omitempty"`
	// Cited are the ledger seqs, among those the report cites, whose
	// screenshots agent-runner asked the tools container to emit.
	Cited []int32 `json:"cited,omitempty"`
}

// OutcomeVerdict is the evaluator's verdict on one approved outcome (pass,
// fail or unsure) with the ledger entries it rests on.
type OutcomeVerdict struct {
	ID      string  `json:"id"`
	Verdict string  `json:"verdict"`
	Ledger  []int32 `json:"ledger,omitempty"`
	Note    string  `json:"note,omitempty"`
}

// Dispute is the evaluator's claim that a failing saved check is not a
// defect, naming the proposed check that should replace it.
type Dispute struct {
	CheckDigest string  `json:"check_digest"`
	Reason      string  `json:"reason"`
	Replacement string  `json:"replacement,omitempty"`
	Ledger      []int32 `json:"ledger,omitempty"`
}

// ProposedCheck names one check the evaluator proposes to save.
type ProposedCheck struct {
	ID      string `json:"id"`
	Outcome string `json:"outcome"`
	Digest  string `json:"digest"`
	// Expect is fail for a check proposed to show a defect.
	Expect string `json:"expect,omitempty"`
}

// Retire is the evaluator's request to retire a saved check.
type Retire struct {
	CheckDigest string `json:"check_digest"`
	Reason      string `json:"reason"`
}

// Anomaly is something the look-around found outside the outcomes, with
// the ledger entries that show it.
type Anomaly struct {
	Summary string  `json:"summary"`
	Route   string  `json:"route"`
	Ledger  []int32 `json:"ledger,omitempty"`
}

// CheckReview is an intent checkreview stage's event payload: one answer
// per new check, each from its own fresh session, keyed by the check's and
// its outcome's digests (agent-runner's, from its handoff; the agent never
// writes them). The answers are the reviewer's claims, held to their
// grammar by agentresult.FromCheckReview.
type CheckReview struct {
	Stage
	Answers []ReviewAnswer `json:"answers,omitempty"`
}

// ReviewAnswer is the reviewer's answer on whether one check proves one
// outcome: proves, does_not_prove or unclear.
//
// Unreviewed marks an answer agent-runner wrote itself, unclear, for a
// check whose review session ended without a report it accepts (refused
// after its repairs, or the session failed): the check is not saved this
// round, and the answer is not the reviewer's to cache.
type ReviewAnswer struct {
	CheckDigest   string `json:"check_digest"`
	OutcomeDigest string `json:"outcome_digest"`
	Answer        string `json:"answer"`
	Reason        string `json:"reason"`
	Unreviewed    bool   `json:"unreviewed,omitempty"`
}

// Event is one envelope line.
type Event struct {
	V    int  `json:"v"`
	Type Type `json:"type"`
	// Finding context so events are self-contained.
	Repo string `json:"repo"`
	// Finding names the owning Finding resource — on an intent run, the
	// owning IntentRun (the Job's MAUL_FINDING).
	Finding string `json:"finding,omitempty"`

	Investigation *Investigation `json:"investigation,omitempty"`
	Remediation   *Remediation   `json:"remediation,omitempty"`
	Plan          *Plan          `json:"plan,omitempty"`
	Evaluation    *Evaluation    `json:"evaluation,omitempty"`
	CheckReview   *CheckReview   `json:"checkreview,omitempty"`
	// Error is set on fatal events.
	Error string `json:"error,omitempty"`
}

// Encode renders the event as one stdout line (prefix included, newline
// excluded).
func (e Event) Encode() (string, error) {
	e.V = Version
	raw, err := json.Marshal(e)
	if err != nil {
		return "", fmt.Errorf("envelope: encode: %w", err)
	}
	return Prefix + string(raw), nil
}

// Decode recovers an event from one log line; ok is false for any line that
// is not an envelope event.
func Decode(line []byte) (Event, bool) {
	rest, found := bytes.CutPrefix(bytes.TrimSpace(line), []byte(Prefix))
	if !found {
		// Kubernetes log lines may carry timestamps or the runtime may
		// have wrapped the line; find the prefix anywhere.
		if i := strings.Index(string(line), Prefix); i >= 0 {
			rest = line[i+len(Prefix):]
		} else {
			return Event{}, false
		}
	}
	var e Event
	if err := json.Unmarshal(rest, &e); err != nil {
		return Event{}, false
	}
	if e.V != Version || e.Type == "" {
		return Event{}, false
	}
	return e, true
}
