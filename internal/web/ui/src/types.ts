// Mirrors the status-server payload (future maul internal/web/data.go).
// Field names match the Finding / FindingRollup CRD json tags in
// maul api/v1alpha1; optional Go pointers become optional fields here.

// Phase is the finding lifecycle position (api/v1alpha1/transitions.go).
export type Phase =
  | "Opened"
  | "Enhanced"
  | "Investigating"
  | "AwaitingApproval"
  | "Queued"
  | "Remediating"
  | "InReview"
  | "Remediated"
  | "Failed"
  | "Dismissed"
  | "HandedOff";

// PHASE_ORDER is the happy path plus the approval detour, in rail order.
export const PHASE_ORDER: Phase[] = [
  "Opened",
  "Enhanced",
  "Investigating",
  "AwaitingApproval",
  "Queued",
  "Remediating",
  "InReview",
  "Remediated",
];

// TERMINAL_PHASES mirrors transitions.go; Dismissed and HandedOff are
// revivable terminals (issue reopen / approve).
export const TERMINAL_PHASES: ReadonlySet<Phase> = new Set([
  "Remediated",
  "Failed",
  "Dismissed",
  "HandedOff",
]);

export type Level = "low" | "medium" | "high" | "critical";
export type Rating = "none" | "low" | "medium" | "high" | "critical";
export type Recommendation = "remediate" | "ignore" | "manual";

// ActionVerb is a user action on a finding. The server resolves which verbs
// the requesting user may invoke (per-finding, from their authorization) and
// injects them as Finding.userActions; the UI never decides authorization.
export type ActionVerb = "approve" | "retry" | "expedite" | "suspend" | "resume";

// AdminVerb is a namespace-wide action in the user menu (demo tooling):
// replay redelivers the webhook delivery log, reset deletes all pipeline
// state. Granted verbs arrive as DatasetUser.adminActions.
export type AdminVerb = "replay" | "reset";

// IntegrationVerb is an integration-scoped action (RBAC verbs on
// integrations): backfill lists pre-existing open alerts into findings;
// replay/reset are the demo tooling. Granted verbs arrive as
// DatasetUser.integrationActions.
export type IntegrationVerb = "backfill" | "replay" | "reset";

export interface Location {
  path: string;
  startLine?: number;
  endLine?: number;
  snippet?: string;
}

export interface Alert {
  id: string;
  url?: string;
  locations?: Location[];
}

export interface FindingRepository {
  type: "github";
  url: string;
  name?: string; // "owner/repo"
  defaultBranch?: string;
}

// FindingCloudResource identifies the cloud resource an infrastructure
// finding was raised against.
export interface FindingCloudResource {
  provider: "google";
  name: string; // the platform's canonical resource identifier
  type?: string; // e.g. "google.cloud.storage.Bucket"
  project?: string;
  location?: string;
  displayName?: string;
}

export interface RelatedFinding {
  name: string;
  relationship: "duplicate-of" | "successor-of" | "related-to";
}

export interface Approval {
  by: string;
  at: string;
  note?: string;
}

// ActionRequest is a recorded human retry/expedite request.
export interface ActionRequest {
  by: string;
  at: string;
}

export interface PhaseTime {
  phase: Phase;
  at: string;
}

export interface TrackingStatus {
  issueNumber?: number;
  url?: string;
  state?: string;
}

export interface Enrichment {
  enhancer: string;
  owners?: string[];
  attributes?: Record<string, string>;
  markdown?: string;
  appliedAt?: string;
}

// Usage is token and cost accounting — one run's, or the finding's total
// across every attempt of both stages. Cost is integer micro-USD, matching
// StageAggregate.
export interface Usage {
  inputTokens?: number;
  outputTokens?: number;
  cacheReadTokens?: number;
  cacheCreationTokens?: number;
  costMicroUSD?: number;
}

// HoldReason is why a finding stopped in AwaitingApproval instead of queueing.
export type HoldReason =
  | 'breakingChangeAvailable'
  | 'lowConfidence'
  | 'exceedsAutomatedTurns'
  | 'exceedsAutomatedTokens';

// Estimate is an investigation's prediction of a remediation's cost. It never
// limits the run — see Budget.
export interface Estimate {
  maxTurns?: number;
  tokenBudget?: number;
}

// Budget is a remediation's predicted-vs-granted budget. What it actually
// spent is the run's own numTurns and usage.outputTokens, so the three-way
// comparison is estimated -> granted -> actual.
export interface Budget {
  estimated?: Estimate;
  grantedMaxTurns?: number;
  grantedTokenBudget?: number;
}

// InvestigationSummary mirrors the finding's own investigation status —
// the bounded verdict fields the list rows render.
export interface InvestigationSummary {
  name?: string;
  attempt?: number;
  outcome?: string;
  recommendation?: Recommendation;
  confidence?: string; // decimal string in [0,1], like the CRD
  exploitability?: Rating;
  likelihood?: Rating;
  impact?: Rating;
  awaitApproval?: boolean;
  holdReasons?: HoldReason[];
  estimate?: Estimate; // what it predicted the remediation would cost
  completedAt?: string;
}

// InvestigationDetail adds what the detail route lifts from the
// Investigation child (absent once it expires).
export interface InvestigationDetail extends InvestigationSummary {
  report?: string;
  harness?: string;
  model?: string;
  numTurns?: number;
  usage?: Usage;
  sessionID?: string;
  transcript?: TranscriptSummary;
}

// RemediationSummary mirrors the finding's own remediation status.
export interface RemediationSummary {
  name?: string;
  attempt?: number;
  outcome?: string;
  success?: boolean;
  branch?: string;
  completedAt?: string;
}

// RemediationDetail adds what the detail route lifts from the Remediation
// child (absent once it expires).
export interface RemediationDetail extends RemediationSummary {
  report?: string;
  harness?: string;
  model?: string;
  numTurns?: number;
  budget?: Budget;
  usage?: Usage;
  sessionID?: string;
  transcript?: TranscriptSummary;
}

// TranscriptSummary says whether a run has a captured conversation worth
// opening; the turns themselves stream from the transcript endpoint.
export interface TranscriptSummary {
  turns: number;
  truncated?: boolean;
}

export type TurnRole = "assistant" | "user" | "system";
export type TurnKind = "text" | "thinking" | "tool_use" | "tool_result" | "notice";

// TranscriptTurn is one entry of an agent's conversation, normalised across
// harnesses. Mirrors internal/web.Turn; keep the two in lockstep. call is the
// id of the tool call a tool_use turn makes; parent is the call a subagent's
// turn ran under (src/transcript.ts nests by them).
export interface TranscriptTurn {
  seq: number;
  at?: string;
  role: TurnRole;
  kind: TurnKind;
  tool?: string;
  text?: string;
  truncated?: boolean;
  call?: string;
  parent?: string;
}

export interface PullRequestStatus {
  number: number;
  url?: string;
  state?: "open" | "merged" | "closed";
  mergedAt?: string;
}

export interface AttemptCounts {
  investigation?: number;
  remediation?: number;
}

export interface ActiveRun {
  kind: "investigation" | "remediation";
  name: string;
}

// FindingSummary is the trimmed list row GET /api/findings ships: what the
// findings table, filters, and stat tiles render, and nothing whose size is
// unbounded (description, alert snippets, enrichment markdown, run reports —
// those come from the per-finding detail route when a finding is opened).
export interface FindingSummary {
  name: string; // metadata.name
  createdAt?: string;
  // spec
  integration?: string;
  source?: string;
  repository?: FindingRepository; // absent for repo-less (e.g. cloud) findings
  // Set when the finding is about infrastructure rather than repository code.
  // A finding with a cloudResource and no repository is one whose resource
  // carried no ownership labels to resolve a repository from.
  cloudResource?: FindingCloudResource;
  advisories: string[]; // [0] is authoritative (GHSA > CVE > CWE)
  ruleID?: string;
  title?: string;
  severity?: Level;
  suspend?: boolean;
  // status
  phase?: Phase;
  firstObservedAt?: string;
  accumulateUntil?: string;
  owners?: string[];
  priority?: Level;
  investigation?: InvestigationSummary;
  remediation?: RemediationSummary;
  pullRequest?: PullRequestStatus;
  attempts?: AttemptCounts;
  activeRun?: ActiveRun;
  lastFailureReason?: string;
  completedAt?: string;
  // authorization: verbs the requesting user may invoke on this finding.
  // Empty or absent means read-only; the action bar does not render.
  userActions?: ActionVerb[];
}

// Finding is the full per-finding projection behind GET /api/findings/{name}:
// the summary plus the unbounded detail, with the run summaries widened to
// carry the lifted child fields (report markdown, usage, transcript).
export interface Finding extends FindingSummary {
  description?: string;
  alerts?: Alert[];
  overflowAlerts?: number;
  related?: RelatedFinding[];
  approval?: Approval;
  retry?: ActionRequest;
  expedite?: ActionRequest;
  phaseTimes?: PhaseTime[];
  tracking?: TrackingStatus;
  enrichments?: Enrichment[];
  investigation?: InvestigationDetail;
  remediation?: RemediationDetail;
  totalUsage?: Usage; // summed across every attempt of both stages
}

// ---- Rollups (api/v1alpha1/findingrollup_types.go) ----
//
// Rollups are sharded in-cluster: one object per scope value. The payload
// carries only scope + status; rows are identified by scope.key ("" = total),
// never by object name (repository object names are sanitized hashes).

export type ScopeType = "total" | "repository" | "harness" | "model";

export interface RollupScope {
  type: ScopeType;
  key?: string; // "" for total, "owner/repo", harness ID, model ID
}

// StageAggregate accumulates one stage's runs. Averages and rates are
// computed client-side from sum ÷ count.
export interface StageAggregate {
  runs?: number;
  succeeded?: number;
  outcomes?: Record<string, number>;
  inputTokens?: number;
  outputTokens?: number;
  cacheReadTokens?: number;
  cacheCreationTokens?: number;
  costMicroUSD?: number;
  elapsedMilliseconds?: number;
  turns?: number;
  estimate?: EstimateAggregate;
}

// EstimateAggregate is predicted-against-actual cost over the runs that
// carried an estimate. Both sides cover the same runs, so the skew
// (actual / predicted - 1) is like-for-like.
export interface EstimateAggregate {
  runs?: number;
  predictedTurns?: number;
  actualTurns?: number;
  predictedOutputTokens?: number;
  actualOutputTokens?: number;
}

// RollupBucket: total and repository scopes carry everything; harness and
// model scopes carry only stages (a finding has no single harness/model
// owner) — their findings count is legitimately absent, not zero.
export interface RollupBucket {
  findings?: number;
  phases?: Record<string, number>; // terminal phases: remediated, failed, …
  recommendations?: Record<string, number>;
  attempts?: number;
  stages?: Record<string, StageAggregate>; // "investigation" | "remediation"
}

export interface MonthlyBucket {
  findings?: number;
  runs?: number;
  costMicroUSD?: number;
}

export interface Rollup {
  scope: RollupScope;
  firstProcessed?: string;
  lastProcessed?: string;
  bucket: RollupBucket;
  monthly?: Record<string, MonthlyBucket>; // total scope only, keyed "2026-07"
}

// DatasetUser is the signed-in identity the top bar renders. Absent when
// unauthenticated; loggedIn is false for fixed-identity auth modes that have
// nothing to sign out of.
export interface DatasetUser {
  name: string;
  loggedIn: boolean;
  // Granted namespace-wide verbs the user menu renders; absent means none.
  adminActions?: AdminVerb[];
  // Whether the configuration view is reachable (native get on
  // integrations); the nav link renders only when true.
  configView?: boolean;
  // Granted integration-scoped verbs the configuration view's triggers
  // check; absent means none.
  integrationActions?: IntegrationVerb[];
}

// Dataset is the payload behind GET /api/findings (trimmed summaries) and
// GET /api/rollups (findings empty, no user — the always-public statistics
// surface). One flat list per concern; all filtering, sorting, and
// derivation is client-side so the server stays a thin read-only projection.
export interface Dataset {
  generatedAt: string;
  namespace?: string;
  version?: string;
  user?: DatasetUser;
  findings: FindingSummary[];
  rollups?: Rollup[];
  // The operator's agent pause (agent.paused): no new agent Jobs launch until
  // it is lifted. Signed-in findings payload only.
  agentsPaused?: boolean;
}

// OfflineDataset is a Dataset whose findings are full: what the demo kit and
// the embedded snapshot carry, since neither has a detail route to lazily
// fetch from.
export interface OfflineDataset extends Dataset {
  findings: Finding[];
}

// ---- Configuration view (internal/web/config.go) ----
//
// ConfigDataset is the payload behind GET /api/config: the configured
// Forges, Integrations and enhancers. Never public — the route requires a
// signed-in identity holding native get on integrations.

// ForgeConfig is one Forge's configuration surface.
export interface ForgeConfig {
  name: string;
  provider: string;
  baseURL?: string;
  orgs?: string[];
  repositories?: string[];
  secretRef?: string;
  interval?: string;
  suspend?: boolean;
  // Ready condition status ("True" | "False" | "Unknown"); absent until
  // the controller reports.
  ready?: string;
  readyMessage?: string;
}

// RedeliveryStatus mirrors an Integration's status.redelivery.
export interface RedeliveryStatus {
  lastSweepAt?: string;
  scanned?: number;
  redelivered?: number;
  truncated?: boolean;
  error?: string;
}

// BackfillStatus mirrors an Integration's status.backfill plus the pending
// spec.backfill echo the trigger renders.
export interface BackfillStatus {
  lastRunAt?: string;
  listed?: number;
  ingested?: number;
  truncated?: boolean;
  error?: string;
  requestedBy?: string;
  requestedAt?: string;
  // A request the controller has not consumed yet.
  pending?: boolean;
}

// IntegrationConfig is one Integration's configuration surface, including
// the backfill trigger's state.
export interface IntegrationConfig {
  name: string;
  provider: string;
  webhookPath?: string;
  secretRef?: string;
  interval?: string;
  suspend?: boolean;
  ready?: string;
  readyMessage?: string;
  capabilities?: string[];
  redelivery?: RedeliveryStatus;
  backfill?: BackfillStatus;
  // Whether the backfill trigger can do anything here (github provider
  // with code scanning enabled).
  backfillSupported?: boolean;
}

// EnhancerConfig is one context-enhancer instance derived from the
// Integrations (the static-context enhancer is a controller flag, invisible
// here).
export interface EnhancerConfig {
  id: string;
  integration?: string;
  enabled: boolean;
  // A singleton capability enabled by more than one Integration — a
  // configuration error the view surfaces rather than resolving.
  ambiguous?: boolean;
}

export interface ConfigDataset {
  generatedAt: string;
  namespace?: string;
  forges: ForgeConfig[];
  integrations: IntegrationConfig[];
  enhancers: EnhancerConfig[];
}

// ---- intents views --------------------------------------------------------
// Mirrors internal/web/intents_wire.go; TestIntentWireTypesMatchTypeScript
// keeps the field names of every interface below in lockstep with the Go
// struct of the same name. Every string that is not the server's own fixed
// wording arrives already made visible (invisible characters as [U+XXXX]);
// the views render all of it as plain text nodes, never as markdown.

// IntentPhase is the lifecycle of one intent (api/v1alpha1 IntentPhase).
export type IntentPhase =
  | "Pending"
  | "Planning"
  | "AwaitingApproval"
  | "Building"
  | "InReview"
  | "Revising"
  | "Blocked"
  | "Merged"
  | "Closed"
  | "Failed";

// IntentColumn is a board column (internal/intentview Columns).
export type IntentColumn = "planning" | "approval" | "building" | "review" | "blocked" | "done";

// IntentTier is how much of a Project the caller may read.
export type IntentTier = "intents" | "transcripts";

export type IntentStage = "plan" | "build" | "revise" | "evaluate" | "checkreview";

export interface Me {
  name: string;
  loggedIn: boolean;
  projects: ProjectAccess[];
}

export interface ProjectAccess {
  name: string;
  tier: IntentTier;
  // the Project settings page is served for this Project (on, and a
  // Project it is for); not whether the caller may save there: the page's
  // canEdit says that.
  settings: boolean;
}

export interface IntentBoard {
  generatedAt: string;
  projects: BoardProject[];
  intents: IntentCard[];
}

export interface BoardProject {
  name: string;
  tier: IntentTier;
  suspended?: boolean;
  repositories?: ProjectRepo[];
  limits: IntentLimits;
}

export interface ProjectRepo {
  key: string;
  slug?: string;
}

export interface IntentLimits {
  maxRevisions: number;
  maxCheckFixes: number;
  maxCostMicroUSD: number;
  maxAttempts: number;
}

export interface IntentCard {
  name: string;
  project: string;
  issue: number;
  issueURL?: string;
  phase: IntentPhase;
  column: IntentColumn;
  blockedFrom?: IntentPhase;
  blockedReasons?: string[];
  waiting?: IntentWaiting;
  suspended?: boolean;
  summary?: string;
  repositories?: string[];
  pullRequests?: IntentPR[];
  preview?: PreviewLink;
  runningRuns?: RunningRun[];
  attempt?: AttemptCount;
  revisions: number;
  checkFixes: number;
  costMicroUSD: number;
  requestedBy?: string;
  requestedAt?: string;
  phaseSince?: string;
  completedAt?: string;
}

export interface IntentWaiting {
  reason: string;
  refs?: string[];
  others?: number;
}

export interface IntentPR {
  repository: string;
  number: number;
  url?: string;
  state?: string;
  mergedAt?: string;
}

export interface PreviewLink {
  phase?: string;
  url?: string;
  revision?: string;
  lastDeployedAt?: string;
}

export interface RunningRun {
  name: string;
  stage: IntentStage;
  repository?: string;
  round: number;
  attempt: number;
  phase: string;
  startedAt?: string;
}

export interface AttemptCount {
  stage: IntentStage;
  current: number;
  max: number;
}

export interface IntentPhaseTime {
  phase: IntentPhase;
  at: string;
}

export interface IntentDetail extends IntentCard {
  tier: IntentTier;
  limits: IntentLimits;
  phaseTimes?: IntentPhaseTime[];
  input?: IntentInputView;
  plan?: IntentPlanView;
  approval?: IntentApproval;
  rounds?: number;
  runs: IntentRunRow[];
  planTexts?: number[];
}

export interface IntentInputView {
  revision: number;
  digest: string;
}

export interface IntentPlanView {
  revision: number;
  digest: string;
  summary?: string;
  repositories?: string[];
  postedAt?: string;
  commentURL?: string;
}

export interface IntentApproval {
  by: string;
  source: string;
  at: string;
  planRevision: number;
  planDigest: string;
  inputDigest: string;
}

export interface IntentRunRow {
  name: string;
  stage: IntentStage;
  trigger?: string;
  repository?: string;
  round: number;
  attempt: number;
  phase?: string;
  outcome?: string;
  reason?: string;
  createdAt?: string;
  startedAt?: string;
  finishedAt?: string;
  costMicroUSD?: number;
  usage?: Usage;
  baseSHA?: string;
  pushedCommit?: string;
  imageSource?: string;
  imageDigest?: string;
  transcript?: TranscriptSummary;
  grant?: RunGrant;
  running?: boolean;
}

export interface RunGrant {
  maxTurns?: number;
  tokenBudget?: number;
  timeoutMilliseconds?: number;
}

export interface IntentRunDetail extends IntentRunRow {
  intent: string;
  project: string;
  tier: IntentTier;
  // countedAttempt is the attempt as counted toward limits.maxAttempts;
  // attempt is the ordinal, which runs ahead once an attempt did not count.
  countedAttempt: number;
  lastAttempt?: boolean;
  limits: IntentLimits;
  intentCostMicroUSD: number;
  job?: RunJobClock;
  report?: string;
}

export interface RunJobClock {
  createdAt?: string;
  startedAt?: string;
  deadlineSeconds?: number;
  idleTimeoutSeconds?: number;
}

export interface IntentPlanText {
  intent: string;
  revision: number;
  digest?: string;
  current?: boolean;
  text: string;
}

// RunActivity is what a live run is doing, derived server-side from its
// transcript without any of its text; every tier receives it.
export interface RunActivity {
  turns: number;
  lastAt?: string;
  openTool?: string;
  openToolSince?: string;
  live: boolean;
  // capped: the transcript recorder reached its cap at lastAt and records
  // nothing more; what the agent has done since is not known.
  capped?: boolean;
}

// RunOutput is one chunk of what the command a live run is running prints,
// sent only to a transcripts-tier reader. It is never persisted, and only
// the latest command's output is replayed to a late viewer. line is the
// number of lines[0] in the command's output; a chunk that starts past the
// previous one's end means lines were left out. Mirrors
// internal/web.RunOutput.
export interface RunOutput {
  task: string;
  line: number;
  lines: string[];
  // done: the command's last chunk; truncated: a limit of the agent's
  // stopped the command's live output for good (a gap in the line numbers
  // alone is a sample, not a limit).
  done?: boolean;
  truncated?: boolean;
}

export interface StreamNotice {
  reason?: string;
}

// RunEvaluation is an evaluate run's evidence view
// (GET /api/intents/{name}/runs/{run}/evaluation). Every tier gets the
// outcomes, maul's decision on each with the calls it counted (cites), and
// the ledger index; ledger arguments and outputs, the own-script labels,
// evidence images and the evaluator's claims (verdict, note, cited) only a
// transcripts-tier reader. Agent and app text is plain text.
export interface RunEvaluation {
  intent: string;
  run: string;
  tier: IntentTier;
  verification: string;
  round: number;
  attempt: number;
  phase?: string;
  reason?: string;
  // current: the Intent's latest verification, so results, anomalies and
  // the hand-off are this round's.
  current?: boolean;
  // lead: maul's sentence on what it counts and what it leaves to the
  // reviewers, set while results are shown.
  lead?: string;
  // withdrawn: maul's sentence while an earlier result is withdrawn
  // because the rules for judging outcomes changed.
  withdrawn?: string;
  // placement: "agents" for a round run in the agents namespace, which has
  // no preview (so no preview link); unset for a slot round.
  placement?: string;
  builds?: EvaluationBuild[];
  handoff?: EvaluationHandoff;
  outcomes: EvaluationOutcome[];
  anomalies?: EvaluationAnomaly[];
  ledger?: EvaluationLedger;
  capture?: EvaluationCapture;
}

export interface EvaluationHandoff {
  mode: string;
  released?: boolean;
  releasedAt?: string;
  reason?: string;
}

// EvaluationOutcome: result, reason, words and cites are maul's decision
// (current round only); cites are the seqs of the calls it counted. verdict,
// note and cited are the evaluator's claims (cited: every seq its verdict
// named, counted or not).
export interface EvaluationOutcome {
  id: string;
  outcome: string;
  kind: string;
  manual?: boolean;
  result?: string;
  reason?: string;
  words?: string;
  cites?: number[];
  verdict?: string;
  note?: string;
  cited?: number[];
}

export interface EvaluationBuild {
  tree: string;
  result: string;
  seq: number;
  exitCode?: number;
  durationMs?: number;
}

export interface EvaluationAnomaly {
  summary: string;
  route?: string;
  ledger?: number[];
  // An earlier round at the same commits found it; this round carries it.
  foundInRound?: number;
}

export interface EvaluationLedger {
  count: number;
  sha256: string;
  // outputs: the ledger ConfigMap was read (transcripts tier).
  outputs?: boolean;
  entries: EvaluationLedgerEntry[];
}

// EvaluationLedgerEntry: side "base" is a call at an agents round's base
// tree, which never counts; own marks a call that ran the evaluator's own
// page script or program (transcripts tier).
export interface EvaluationLedgerEntry {
  seq: number;
  kind: string;
  verb: string;
  side?: string;
  own?: boolean;
  exit?: number;
  refused?: boolean;
  resultSHA256: string;
  artifacts?: EvaluationArtifact[];
  args?: string;
  output?: string;
  outputDropped?: boolean;
}

// EvaluationArtifact: image names the stored evidence image, served by
// /api/intents/{name}/runs/{run}/evidence/{image}.
export interface EvaluationArtifact {
  sha256: string;
  image?: string;
}

export interface EvaluationCapture {
  components?: EvaluationComponentCapture[];
  events?: number;
}

export interface EvaluationComponentCapture {
  name: string;
  bytes: number;
  truncated?: boolean;
}

// ---- usage views ----------------------------------------------------------
// Mirrors internal/usageview (the report, shared with `maul get usage`) and
// internal/web/usage.go (UsageAccess); TestUsageWireTypesMatchTypeScript
// keeps the field names in lockstep. Costs are integer micro-USD. Trusted
// figures are the broker's metered tokens; notional cost is a subscription's
// list-rate equivalent, never money spent; unmetered tokens are the broker's
// worst-case shortfall and reported figures the pods' own, both shown apart.

export type UsageView = "self" | "user" | "project" | "user-project" | "all";

export interface UsageAccess {
  name: string;
  self: boolean;
  all: boolean;
  reason: string;
}

export interface UsageFigures {
  runs: number;
  partialRuns: number;
  reportedRuns: number;
  unpricedRuns: number;
  tokens: number;
  inputTokens: number;
  outputTokens: number;
  cacheReadTokens: number;
  cacheWriteTokens: number;
  unmeteredTokens: number;
  costMicroUSD: number;
  notionalCostMicroUSD: number;
  reportedTokens: number;
  reportedCostMicroUSD: number;
}

export interface UsageSegment {
  key: string;
  runs: number;
  tokens: number;
  costMicroUSD: number;
  notionalCostMicroUSD: number;
}

export interface UsageDay extends UsageFigures {
  day: string;
  projects: UsageSegment[];
  kinds: UsageSegment[];
}

export interface UsageIntent {
  intent: string;
  project: string;
  runs: number;
  tokens: number;
  costMicroUSD: number;
  notionalCostMicroUSD: number;
}

export interface UsagePerson extends UsageFigures {
  user: string;
  login: string;
}

export interface UsageProject extends UsageFigures {
  project: string;
}

export interface UsageReport {
  view: UsageView;
  user: string;
  login: string;
  project: string;
  days: number;
  from: string;
  to: string;
  totals: UsageFigures;
  daily: UsageDay[];
  projectKeys: string[];
  kindKeys: string[];
  topIntents: UsageIntent[];
  topIntentMonths: string[];
  // people: views all and project; projects: views all, self and user.
  people?: UsagePerson[];
  projects?: UsageProject[];
}

// ---- Account page (internal/web/account.go) --------------------------------
// Write-only: no field ever carries a stored credential.

export type CredentialKind = "api-key" | "oauth-token";

export type CredentialHealth = "Valid" | "Rejected" | "Unknown";

export interface AccountCredential {
  provider: string;
  // kinds the provider takes, in the order the page offers them:
  // Anthropic ["api-key", "oauth-token"], OpenAI ["api-key"]. Empty for a
  // stored credential whose provider the page no longer offers: it can be
  // seen and deleted, never replaced.
  kinds: CredentialKind[];
  stored: boolean;
  kind: CredentialKind | "";
  fingerprint: string;
  health: CredentialHealth | "";
  lastUsedAt: string;
  lastRejectedAt: string;
}

export interface Account {
  name: string;
  self: boolean;
  allowed: boolean;
  reason: string;
  credentials: AccountCredential[];
}

// ---- Project settings page (internal/projectsettings, served by -----------
// internal/web/project_settings.go). Durations are whole seconds and money
// micro-USD, both integers; 0 is unset where the Project's own field says so.
// The check reviewer's model and limits are not on the page.

export type ProjectSettingsGroup =
  | "approvers"
  | "evaluator"
  | "handoff"
  | "models"
  | "cost"
  | "rounds"
  | "activeIntents"
  | "stages"
  | "delegation"
  | "checks"
  | "resources";

export type ProjectSettingsKind =
  | "count"
  | "microUSD"
  | "seconds"
  | "bool"
  | "handoff"
  | "model"
  | "logins"
  | "checks"
  | "class";

export type ProjectSettingsEffect = "live" | "new-runs" | "new-intents" | "next-round";

// why a field cannot be changed at all; "" when it can. "not-admitted": a
// delegation switch, off, that intent-controller does not allow.
export type ProjectSettingsLocked =
  | ""
  | "locked"
  | "not-allowed"
  | "admits-unpublished"
  | "no-classes"
  | "not-admitted";

export type ProjectSettingsApproverState = "ok" | "no-write-access" | "no-account" | "unresolved";

// why the viewer may not save: the gate's reasons, or "read-only" for a page
// served with no write path; "" when they may.
export type ProjectSettingsReason =
  | ""
  | "no-github-identity"
  | "not-approver"
  | "no-write-access"
  | "approvers-unresolved"
  | "approvers-stale"
  | "read-only";

// "Apply": server-side apply (Helm 4's default); its next apply that still
// states the field fails with a conflict. "Update": client-side helm,
// kubectl apply; its next apply replaces whole lists. A manager recorded
// under both is named once, as "Apply".
export type ProjectSettingsManagerOperation = "Apply" | "Update";

export type ProjectSettingsHandoff = "" | "gated" | "early";

export interface ProjectSettings {
  approvers: string[];
  verification: ProjectSettingsVerification;
  models: ProjectSettingsModels;
  limits: ProjectSettingsLimits;
  stages: ProjectSettingsStages;
  delegation: ProjectSettingsDelegation;
  checks: ProjectSettingsChecks;
  repositories: ProjectSettingsRepository[];
}

export interface ProjectSettingsVerification {
  enabled: boolean;
  handoff: ProjectSettingsHandoff;
  handoffMaxSeconds: number;
  maxToolCalls: number;
}

export interface ProjectSettingsModels {
  plan: string;
  build: string;
  evaluate: string;
}

export interface ProjectSettingsLimits {
  maxCostMicroUSD: number;
  maxEvaluateCostMicroUSD: number;
  // null is the schema default (unset for maxVerifyFixes); 0 is a value.
  maxRevisions: number | null;
  maxCheckFixes: number | null;
  maxVerifyFixes: number | null;
  maxVerifications: number;
  maxActiveIntents: number;
}

export interface ProjectSettingsStages {
  plan: ProjectSettingsStage;
  build: ProjectSettingsStage;
  revise: ProjectSettingsStage;
  evaluate: ProjectSettingsStage;
}

// 0 is the controller's own ceiling for the stage (for the cost, the
// ceiling it is spent against).
export interface ProjectSettingsStage {
  maxTurns: number;
  tokenBudget: number;
  maxCostMicroUSD: number;
  timeoutSeconds: number;
}

// spec.delegation: the Project's opt-in, which takes effect only where
// options.subagents / options.workflows say the controller allows it.
// workflows needs subagents.
export interface ProjectSettingsDelegation {
  subagents: boolean;
  workflows: boolean;
}

export interface ProjectSettingsChecks {
  fix: string[];
  timeoutSeconds: number;
  rerunFailed: boolean;
}

export interface ProjectSettingsRepository {
  name: string;
  agentResourceClass: string;
}

export interface ProjectSettingsView {
  project: string;
  generation: number;
  observedGeneration: number;
  schema: number;
  canEdit: boolean;
  reason: ProjectSettingsReason;
  settings: ProjectSettings;
  fields: ProjectSettingsField[];
  approvers: ProjectSettingsApprover[];
  approversResolvedAt: string;
  approversStale: boolean;
  options: ProjectSettingsOptions;
  allow: ProjectSettingsAllow;
  facts: ProjectSettingsFacts;
  lastEdit: ProjectSettingsEdit | null;
  conditions: ProjectSettingsCondition[];
}

export interface ProjectSettingsField {
  // the path below spec ("limits.maxRevisions"); the per-repository field
  // is "repositories.agentResourceClass".
  id: string;
  group: ProjectSettingsGroup;
  kind: ProjectSettingsKind;
  min: number;
  max: number;
  default: number;
  ceiling: number;
  floor: number;
  locked: ProjectSettingsLocked;
  effect: ProjectSettingsEffect;
  managers: ProjectSettingsManager[];
}

export interface ProjectSettingsManager {
  name: string;
  operation: ProjectSettingsManagerOperation;
}

export interface ProjectSettingsApprover {
  login: string;
  state: ProjectSettingsApproverState;
  you: boolean;
  managers: ProjectSettingsManager[];
}

export interface ProjectSettingsOptions {
  published: boolean;
  models: string[];
  defaultModels: ProjectSettingsModels;
  resourceClasses: ProjectSettingsClass[];
  evaluator: boolean;
  costBudgets: boolean;
  // intent-controller lets the Project's delegation switches take effect.
  subagents: boolean;
  workflows: boolean;
}

export interface ProjectSettingsClass {
  name: string;
  requestCPU: string;
  requestMemory: string;
  limitCPU: string;
  limitMemory: string;
}

export interface ProjectSettingsAllow {
  approvers: boolean;
  disableEvaluator: boolean;
  emptyChecks: boolean;
}

export interface ProjectSettingsFacts {
  intentRepository: string;
  repositories: ProjectSettingsRepositoryFact[];
  triggerLabel: string;
  approveLabel: string;
  harness: string;
  surfaces: string[];
  suspend: boolean;
  requireRepositoryImage: boolean;
  manualGatesStatus: boolean;
  postLogs: boolean;
  activeIntents: number;
}

export interface ProjectSettingsRepositoryFact {
  name: string;
  url: string;
  previewed: boolean;
  previewPath: string;
  previewImage: string;
  previewPort: number;
  previewReadinessPath: string;
  classManagers: ProjectSettingsManager[];
}

export interface ProjectSettingsEdit {
  githubID: number;
  login: string;
  at: string;
  fields: string[];
  more: number;
}

// in fixed wording; never a condition's message.
export interface ProjectSettingsCondition {
  type: string;
  status: string;
  reason: string;
  text: string;
}

export interface ProjectSettingsRequest {
  generation: number;
  schema: number;
  settings: ProjectSettings;
  confirm: string[];
}

// reason: "" when made (or nothing changed), a gate reason, or
// "generation-changed", "schema-changed", "invalid-document", "refused",
// "confirm-required".
export interface ProjectSettingsResult {
  reason: string;
  generation: number;
  changes: ProjectSettingsChange[];
  refusals: ProjectSettingsRefusal[];
  confirm: ProjectSettingsConfirmation[];
}

export interface ProjectSettingsChange {
  field: string;
  item: string;
  group: ProjectSettingsGroup;
  from: string;
  to: string;
  added: string[];
  removed: string[];
  effect: ProjectSettingsEffect;
}

export interface ProjectSettingsRefusal {
  field: string;
  item: string;
  reason: string;
  limit: number;
  value: string;
}

export interface ProjectSettingsConfirmation {
  key: string;
  kind: string;
  field: string;
  value: string;
}
