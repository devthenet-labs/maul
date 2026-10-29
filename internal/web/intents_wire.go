// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package web

// The intents views' wire types. Each mirrors an interface of the same name
// in ui/src/types.ts; TestIntentWireTypesMatchTypeScript keeps the two in
// lockstep. Every string in them that is not the server's own fixed wording
// has passed through intentview.Text, and every link through
// intentview.SafeURL.

// Me is GET /api/me: who the caller is and which Projects the intents views
// show them, at which tier. It exists only while the intents views are
// enabled, so its absence is how the SPA knows they are off.
type Me struct {
	Name     string          `json:"name"`
	LoggedIn bool            `json:"loggedIn"`
	Projects []ProjectAccess `json:"projects"`
}

// ProjectAccess is one Project the caller may see and how much of it.
type ProjectAccess struct {
	Name string `json:"name"`
	// Tier is "intents" (the board, timeline and run numbers) or
	// "transcripts" (plans, reports and transcripts too).
	Tier string `json:"tier"`
}

// IntentBoard is GET /api/intents: the granted Projects and their intents.
type IntentBoard struct {
	GeneratedAt string         `json:"generatedAt"`
	Projects    []BoardProject `json:"projects"`
	Intents     []IntentCard   `json:"intents"`
}

// BoardProject is one granted Project's header on the board.
type BoardProject struct {
	Name         string        `json:"name"`
	Tier         string        `json:"tier"`
	Suspended    bool          `json:"suspended,omitempty"`
	Repositories []ProjectRepo `json:"repositories,omitempty"`
	Limits       IntentLimits  `json:"limits"`
}

// ProjectRepo is one of a Project's repositories: its key and owner/name.
type ProjectRepo struct {
	Key  string `json:"key"`
	Slug string `json:"slug,omitempty"`
}

// IntentLimits are the Project's limits with the schema defaults applied,
// plus the attempts a stage's round gets.
type IntentLimits struct {
	MaxRevisions    int32 `json:"maxRevisions"`
	MaxCheckFixes   int32 `json:"maxCheckFixes"`
	MaxCostMicroUSD int64 `json:"maxCostMicroUSD"`
	MaxAttempts     int32 `json:"maxAttempts"`
}

// IntentCard is one intent on the board.
type IntentCard struct {
	Name     string `json:"name"`
	Project  string `json:"project"`
	Issue    int64  `json:"issue"`
	IssueURL string `json:"issueURL,omitempty"`
	Phase    string `json:"phase"`
	// Column is the board column (intentview.ColumnFor).
	Column string `json:"column"`
	// BlockedFrom is the phase a Blocked intent resumes to, when known.
	BlockedFrom string `json:"blockedFrom,omitempty"`
	// BlockedReasons are the public reasons, never a condition's message.
	BlockedReasons []string `json:"blockedReasons,omitempty"`
	// Suspended is spec.suspend: nothing launches, and the status may be
	// stale, since a suspended intent is not reconciled.
	Suspended bool `json:"suspended,omitempty"`
	// Summary is the current plan's one-line summary (agent-written, shown
	// as plain text; the issue's status comment carries it too).
	Summary      string        `json:"summary,omitempty"`
	Repositories []string      `json:"repositories,omitempty"`
	PullRequests []IntentPR    `json:"pullRequests,omitempty"`
	Preview      *PreviewLink  `json:"preview,omitempty"`
	RunningRuns  []RunningRun  `json:"runningRuns,omitempty"`
	Attempt      *AttemptCount `json:"attempt,omitempty"`
	// Revisions and CheckFixes are the review/command and check-fix rounds
	// started that count against the Project's limits, failed ones included
	// (intentview.RevisionRounds, CheckFixRounds): what the limits are
	// enforced with, not the completed rounds status.revisions counts.
	Revisions    int32  `json:"revisions"`
	CheckFixes   int32  `json:"checkFixes"`
	CostMicroUSD int64  `json:"costMicroUSD"`
	RequestedBy  string `json:"requestedBy,omitempty"`
	RequestedAt  string `json:"requestedAt,omitempty"`
	PhaseSince   string `json:"phaseSince,omitempty"`
	CompletedAt  string `json:"completedAt,omitempty"`
}

// IntentPR is one pull request maul opened for an intent.
type IntentPR struct {
	Repository string `json:"repository"`
	Number     int64  `json:"number"`
	URL        string `json:"url,omitempty"`
	State      string `json:"state,omitempty"`
	MergedAt   string `json:"mergedAt,omitempty"`
}

// PreviewLink is an intent's preview, when there is one.
type PreviewLink struct {
	Phase string `json:"phase,omitempty"`
	// URL is set only while the preview is Ready.
	URL string `json:"url,omitempty"`
	// Revision is the commit the preview serves, cut to 12 characters.
	Revision       string `json:"revision,omitempty"`
	LastDeployedAt string `json:"lastDeployedAt,omitempty"`
}

// RunningRun is one IntentRun whose agent is pending or running. A
// multi-repository build runs one per repository at once, so the board lists
// them all rather than the intent's single activeRun.
type RunningRun struct {
	Name       string `json:"name"`
	Stage      string `json:"stage"`
	Repository string `json:"repository,omitempty"`
	Round      int32  `json:"round"`
	Attempt    int32  `json:"attempt"`
	Phase      string `json:"phase"`
	StartedAt  string `json:"startedAt,omitempty"`
}

// AttemptCount is the attempt of the intent's newest run, as counted toward
// the attempts its round gets (intentview.CountedAttempt), against them.
type AttemptCount struct {
	Stage   string `json:"stage"`
	Current int32  `json:"current"`
	Max     int32  `json:"max"`
}

// IntentDetail is GET /api/intents/{name}: the card plus everything the
// timeline tells, at the caller's tier for its Project.
type IntentDetail struct {
	IntentCard
	Tier       string           `json:"tier"`
	Limits     IntentLimits     `json:"limits"`
	PhaseTimes []PhaseTime      `json:"phaseTimes,omitempty"`
	Input      *IntentInputView `json:"input,omitempty"`
	Plan       *IntentPlanView  `json:"plan,omitempty"`
	Approval   *IntentApproval  `json:"approval,omitempty"`
	Rounds     int32            `json:"rounds,omitempty"`
	Runs       []IntentRunRow   `json:"runs"`
	// PlanTexts are the plan revisions whose text a tier 2 reader can open.
	PlanTexts []int32 `json:"planTexts,omitempty"`
}

// IntentInputView is the issue snapshot the current plan was made from.
type IntentInputView struct {
	Revision int32  `json:"revision"`
	Digest   string `json:"digest"`
}

// IntentPlanView is the current plan's metadata; its text is tier 2.
type IntentPlanView struct {
	Revision     int32    `json:"revision"`
	Digest       string   `json:"digest"`
	Summary      string   `json:"summary,omitempty"`
	Repositories []string `json:"repositories,omitempty"`
	PostedAt     string   `json:"postedAt,omitempty"`
	CommentURL   string   `json:"commentURL,omitempty"`
}

// IntentApproval is the accepted approval and exactly what it approved. It
// is a GitHub fact the controller recorded; the dashboard only shows it.
type IntentApproval struct {
	By           string `json:"by"`
	Source       string `json:"source"`
	At           string `json:"at"`
	PlanRevision int32  `json:"planRevision"`
	PlanDigest   string `json:"planDigest"`
	InputDigest  string `json:"inputDigest"`
}

// IntentRunRow is one IntentRun in the timeline.
type IntentRunRow struct {
	Name       string `json:"name"`
	Stage      string `json:"stage"`
	Trigger    string `json:"trigger,omitempty"`
	Repository string `json:"repository,omitempty"`
	Round      int32  `json:"round"`
	Attempt    int32  `json:"attempt"`
	Phase      string `json:"phase,omitempty"`
	Outcome    string `json:"outcome,omitempty"`
	// Reason is the outcome in public wording; never the run's detail.
	Reason       string `json:"reason,omitempty"`
	CreatedAt    string `json:"createdAt,omitempty"`
	StartedAt    string `json:"startedAt,omitempty"`
	FinishedAt   string `json:"finishedAt,omitempty"`
	CostMicroUSD int64  `json:"costMicroUSD,omitempty"`
	Usage        *Usage `json:"usage,omitempty"`
	BaseSHA      string `json:"baseSHA,omitempty"`
	PushedCommit string `json:"pushedCommit,omitempty"`
	// ImageSource is "repository" or "default"; ImageDigest the first 12
	// hex of the image digest when it was pinned by one. The image reference
	// itself (its registry host) is never sent.
	ImageSource string `json:"imageSource,omitempty"`
	ImageDigest string `json:"imageDigest,omitempty"`
	// Transcript says whether a recorded conversation exists (tier 2 opens it).
	Transcript *TranscriptSummary `json:"transcript,omitempty"`
	Grant      *RunGrant          `json:"grant,omitempty"`
	Running    bool               `json:"running,omitempty"`
}

// RunGrant is what the run was granted.
type RunGrant struct {
	MaxTurns            int32 `json:"maxTurns,omitempty"`
	TokenBudget         int64 `json:"tokenBudget,omitempty"`
	TimeoutMilliseconds int64 `json:"timeoutMilliseconds,omitempty"`
}

// IntentRunDetail is GET /api/intents/{name}/runs/{run}: the run panel.
type IntentRunDetail struct {
	IntentRunRow
	Intent  string `json:"intent"`
	Project string `json:"project"`
	Tier    string `json:"tier"`
	// CountedAttempt is this run's attempt as counted toward the attempts
	// its round gets (intentview.CountedAttempt). Attempt is its ordinal,
	// which runs ahead once an attempt did not count (its agent never ran).
	CountedAttempt int32 `json:"countedAttempt"`
	// LastAttempt: if this attempt fails, the intent fails (or, for a
	// revise round, the round ends).
	LastAttempt bool         `json:"lastAttempt,omitempty"`
	Limits      IntentLimits `json:"limits"`
	// IntentCostMicroUSD is the intent's recorded spend so far, the figure
	// the cost ceiling is checked against before the next launch.
	IntentCostMicroUSD int64 `json:"intentCostMicroUSD"`
	// Job is the agent Job's clock, while the run is live and the server can
	// read it.
	Job *RunJobClock `json:"job,omitempty"`
	// Report is the run's report, tier 2 only, as plain text.
	Report string `json:"report,omitempty"`
}

// RunJobClock is the agent Job's clock and the limits that stop the run.
type RunJobClock struct {
	CreatedAt          string `json:"createdAt,omitempty"`
	StartedAt          string `json:"startedAt,omitempty"`
	DeadlineSeconds    int64  `json:"deadlineSeconds,omitempty"`
	IdleTimeoutSeconds int64  `json:"idleTimeoutSeconds,omitempty"`
}

// IntentPlanText is GET /api/intents/{name}/plans/{revision}: one plan
// revision's text, tier 2 only, as plain text.
type IntentPlanText struct {
	Intent   string `json:"intent"`
	Revision int32  `json:"revision"`
	Digest   string `json:"digest,omitempty"`
	Current  bool   `json:"current,omitempty"`
	Text     string `json:"text"`
}

// RunActivity is the run stream's activity event: what a live run is doing,
// derived from its transcript without any of its text. Every tier gets it.
type RunActivity struct {
	Turns int `json:"turns"`
	// LastAt is the newest turn's time: "no activity since".
	LastAt string `json:"lastAt,omitempty"`
	// OpenTool is the tool the agent called last and has had no result for
	// yet, with when it was called.
	OpenTool      string `json:"openTool,omitempty"`
	OpenToolSince string `json:"openToolSince,omitempty"`
	Live          bool   `json:"live"`
	// Capped: the transcript recorder reached its cap (LastAt is when) and
	// records nothing more, while the agent may go on working, so what it
	// is doing since is not known.
	Capped bool `json:"capped,omitempty"`
}

// RunOutput is the run stream's output event: one chunk of what the command
// the agent is running prints, for a tier 2 reader only. It is live only:
// never persisted, never part of the transcript or the activity, and only
// the latest command's output is replayed to a viewer who joins late. Line
// is the number of Lines[0] in the command's output; a chunk starting past
// the previous one's end means lines were left out. Done marks the
// command's last chunk; Truncated says the agent stopped forwarding its
// output at its budget.
type RunOutput struct {
	Task      string   `json:"task"`
	Line      int      `json:"line"`
	Lines     []string `json:"lines"`
	Done      bool     `json:"done,omitempty"`
	Truncated bool     `json:"truncated,omitempty"`
}

// StreamNotice is the run stream's unavailable and end events' payload.
type StreamNotice struct {
	Reason string `json:"reason,omitempty"`
}

// RunEvaluation is GET /api/intents/{name}/runs/{run}/evaluation: the
// evidence of one evaluate run's round, read from its PreviewVerification
// (pinned by the run's UID) and, when that verification is the Intent's
// current one, the Intent's decided results. Every tier sees the outcomes,
// the checks and the ledger index; the ledger's outputs and arguments, the
// evidence images and the evaluator's claims (its verdicts, notes and
// dispute reasons) are tier 2 only. Agent and application text is plain
// text, never markup.
type RunEvaluation struct {
	Intent string `json:"intent"`
	Run    string `json:"run"`
	Tier   string `json:"tier"`
	// Verification is the PreviewVerification's name.
	Verification string `json:"verification"`
	Round        int32  `json:"round"`
	Attempt      int32  `json:"attempt"`
	Rung         string `json:"rung,omitempty"`
	// Phase and Reason are the verification's, from fixed vocabularies.
	Phase  string `json:"phase,omitempty"`
	Reason string `json:"reason,omitempty"`
	// Current: the verification is the Intent's latest, so the decided
	// results, anomalies, disputes and hand-off below are this round's.
	Current   bool                `json:"current,omitempty"`
	Handoff   *EvaluationHandoff  `json:"handoff,omitempty"`
	Outcomes  []EvaluationOutcome `json:"outcomes"`
	Checks    []EvaluationCheck   `json:"checks"`
	Anomalies []EvaluationAnomaly `json:"anomalies,omitempty"`
	Disputes  []EvaluationDispute `json:"disputes,omitempty"`
	Ledger    *EvaluationLedger   `json:"ledger,omitempty"`
	Capture   *EvaluationCapture  `json:"capture,omitempty"`
}

// EvaluationHandoff is the hand-off of the preview link to humans.
type EvaluationHandoff struct {
	Mode       string `json:"mode"`
	Released   bool   `json:"released,omitempty"`
	ReleasedAt string `json:"releasedAt,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// EvaluationOutcome is one approved outcome: its text, maul's decided
// result while the round is current, and (tier 2) the evaluator's claim.
type EvaluationOutcome struct {
	ID      string `json:"id"`
	Outcome string `json:"outcome"`
	Kind    string `json:"kind"`
	Manual  bool   `json:"manual,omitempty"`
	// Result, Basis, Line and Reason are maul's decision (current only).
	Result string `json:"result,omitempty"`
	Basis  string `json:"basis,omitempty"`
	Line   string `json:"line,omitempty"`
	Reason string `json:"reason,omitempty"`
	// Verdict, Note and Cited are the evaluator's claims, tier 2 only.
	Verdict string  `json:"verdict,omitempty"`
	Note    string  `json:"note,omitempty"`
	Cited   []int32 `json:"cited,omitempty"`
}

// EvaluationCheck is one check of the round: a saved check replayed, or a
// proposed one confirmed. Steps is its canonical body, indented; Compiled
// the Playwright spec maul compiled from a browser check, with the run
// token it was compiled with.
type EvaluationCheck struct {
	ID            string              `json:"id"`
	Outcome       string              `json:"outcome"`
	Form          string              `json:"form"`
	Digest        string              `json:"digest"`
	Source        string              `json:"source"`
	Steps         string              `json:"steps"`
	Compiled      string              `json:"compiled,omitempty"`
	CompiledToken string              `json:"compiledToken,omitempty"`
	Replay        *EvaluationCheckRun `json:"replay,omitempty"`
	Confirmation  *EvaluationCheckRun `json:"confirmation,omitempty"`
	Disputed      bool                `json:"disputed,omitempty"`
}

// EvaluationCheckRun is one run of a check as the ledger recorded it.
type EvaluationCheckRun struct {
	Result    string `json:"result"`
	Token     string `json:"token"`
	Seq       int32  `json:"seq"`
	DryRunSeq int32  `json:"dryRunSeq,omitempty"`
}

// EvaluationAnomaly is one look-around anomaly (current round only). Its
// summary and route are the evaluator's, shown as plain text.
type EvaluationAnomaly struct {
	Summary string  `json:"summary"`
	Route   string  `json:"route,omitempty"`
	Ledger  []int32 `json:"ledger,omitempty"`
}

// EvaluationDispute is a saved check the evaluator disputed. Reason,
// Replacement and Ledger are the evaluator's, tier 2 only.
type EvaluationDispute struct {
	CheckDigest string  `json:"checkDigest"`
	Reason      string  `json:"reason,omitempty"`
	Replacement string  `json:"replacement,omitempty"`
	Ledger      []int32 `json:"ledger,omitempty"`
}

// EvaluationLedger is the tools container's verified ledger: the index on
// the verification's status, every entry, and (tier 2, Outputs) each
// entry's arguments and output read from its ledger ConfigMap.
type EvaluationLedger struct {
	Count   int32                   `json:"count"`
	SHA256  string                  `json:"sha256"`
	Outputs bool                    `json:"outputs,omitempty"`
	Entries []EvaluationLedgerEntry `json:"entries"`
}

// EvaluationLedgerEntry is one ledger entry. Output is shown only where its
// sha256 is the index's; OutputDropped marks one that is not (cut to fit
// the ConfigMap, or missing).
type EvaluationLedgerEntry struct {
	Seq           int32                `json:"seq"`
	Kind          string               `json:"kind"`
	Verb          string               `json:"verb"`
	Exit          int32                `json:"exit,omitempty"`
	Refused       bool                 `json:"refused,omitempty"`
	ResultSHA256  string               `json:"resultSHA256"`
	Artifacts     []EvaluationArtifact `json:"artifacts,omitempty"`
	Args          string               `json:"args,omitempty"`
	Output        string               `json:"output,omitempty"`
	OutputDropped bool                 `json:"outputDropped,omitempty"`
}

// EvaluationArtifact is one artifact an entry recorded. Image names its
// stored evidence image (tier 2), served by
// /api/intents/{name}/runs/{run}/evidence/{image}.
type EvaluationArtifact struct {
	SHA256 string `json:"sha256"`
	Image  string `json:"image,omitempty"`
}

// EvaluationCapture is what was captured from the slot for the evaluator.
type EvaluationCapture struct {
	Components []EvaluationComponentCapture `json:"components,omitempty"`
	Events     int32                        `json:"events,omitempty"`
}

// EvaluationComponentCapture is one component's captured log tail's size.
type EvaluationComponentCapture struct {
	Name      string `json:"name"`
	Bytes     int32  `json:"bytes"`
	Truncated bool   `json:"truncated,omitempty"`
}
