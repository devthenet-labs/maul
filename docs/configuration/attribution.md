# Run attribution

Run attribution records whom every agent run is for. It is the first layer of per-user credentials and usage. The org
credential still pays for every run. Attribution only records who caused each one. It is off by default. With it off,
run specs, agent Jobs, issue comments and the chart's admission policies are exactly what they were before it existed.

## Turning it on

| Helm                          | Flag                 | Env                       | Default |
| ----------------------------- | -------------------- | ------------------------- | ------- |
| `attribution.mode: attribute` | `--attribution-mode` | `PATCHY_ATTRIBUTION_MODE` | `off`   |

The flag is shared by the four controllers that launch agent Jobs: investigation-, remediation-, intent- and
evaluation-controller. The chart sets it on all four from `attribution.mode`. It also renders the admission policies
described [below](#admission-policies), which need Kubernetes 1.30 or later. An unknown mode stops the controller at
startup.

Set the chart value rather than the flag on its own. Under `attribute` the Job policy requires every agent Job to carry
the annotation, so a controller left on `off` would have its launches refused. The chart's ConfigMap change rolls the
four controllers. A run launched during that roll by a pod still running the old configuration can be refused. An intent
run refused that way ends `launch_refused` and is retried as its next attempt. To avoid that window, pause the agents
first (`agent.paused`), upgrade, and then resume them.

## What is recorded

Each new `Investigation`, `Remediation`, `IntentRun` and `EvaluationUnit` gets `spec.attributedTo` when it is created.
It names a user (`login` and GitHub numeric `id`) and a `reason`. Every run spec is immutable, so the stamp is written
once and never changes.

| Run                     | Attributed to                                                     | `reason`    |
| ----------------------- | ----------------------------------------------------------------- | ----------- |
| Intent plan             | whoever opened the plan's revision, if they are an approver       | `requester` |
| Intent plan             | otherwise, the organisation                                       | `org`       |
| Intent build            | the plan's approver                                               | `approver`  |
| Intent revise (command) | the approver whose `/patchy revise` or `/patchy retry` started it | `commenter` |
| Intent revise (review)  | the author of the newest approver review the round consumed       | `reviewer`  |
| Intent check-fix        | the plan's approver                                               | `approver`  |
| Finding investigation   | the organisation: no human is behind an investigation             | `org`       |
| Finding remediation     | the Finding's approver when its approval is held, else the org    | `approver`  |
| Evaluation unit         | its Evaluation's submitter                                        | `submitter` |

A plan's revision is opened by the trigger that created the Intent, or by the replan or revival that started it again.
Every attempt of that revision is attributed to the same person. A trigger action consumed while it runs, a refused or
unavailable one included, starts no plan and changes no attempt's attribution.

A person is identified by their GitHub numeric id. A login can be renamed, and a freed login can be taken by someone
else, so the login is kept for display only. patchy records the id wherever GitHub supplies it:

- an Intent's `spec.requestedBy.userID`;
- every consumed trigger action (`status.lastTrigger.userID`);
- the trigger action that opened the current plan revision (`status.input.trigger.userID`);
- the accepted approval (`status.approval.byID`);
- a Finding's approval, retry and expedite made by a `/patchy` comment (`spec.approval.byID` and the same field on
  `retry` and `expedite`).

**Re-attribution.** A person the table names may have no patchy identity, meaning no GitHub id is on record for them.
The run is then attributed to the organisation, with reason `approver-unregistered`, or `submitter-unregistered` for an
evaluation unit. This applies to:

- an approval made on the status page or in the CLI, whose usernames are not GitHub accounts;
- an Intent created before ids were recorded;
- every evaluation submitter, for now, since the evaluation API verifies a username and no GitHub id.

No one is attributed a run on the strength of a login alone.

**On the Job.** Every agent Job, and the pod template its pods are made from, carries the annotation
`patchy.bitwisemedia.uk/attributed-to`. Its value is `org`, or `github-id:<id>` for a person. The launching controller
takes it from the run's own stamp. A run with no stamp (one created before the mode was turned on) is annotated `org`.
investigation-controller only ever writes `org`.

**On the issue.** The intent issue's status comment gains a line beside the cost:
`Attributed to: @login (reason), for the latest run`, or the organisation. The login is shown as code, like every login
in that comment, so it notifies no one.

## Admission policies

With `attribution.mode` other than `off`, the chart renders up to six ValidatingAdmissionPolicies. Each fails closed.

- **`<release>-attribution-jobs`** applies in the agent namespace. A Job created by one of the launching controllers
  must be of that controller's own run kind (`investigation`, `remediation`, `intent`, `evaluation`). It must carry the
  annotation in the grammar on the Job and its pod template alike, and investigation-controller may only write `org`.
  Anyone else's Job may not carry the annotation at all. No update may set, change or remove it.
- **`<release>-attribution-pods`** applies in the agent namespace. A pod carrying the annotation is admitted only when
  the Kubernetes Job controller (`admissionPolicy.jobControllerUsernames`) creates it, with a controlling owner
  reference to a `batch/v1` Job. No update, nor a `pods/status` write, may set, change or remove it.
- **`<release>-attribution-finding-stamps`** applies in the release namespace. Anyone but patchy's controllers, the
  status server and `admissionPolicy.extraExemptSubjects` who sets or changes a Finding's `spec.approval`, `spec.retry`
  or `spec.expedite` must name themselves in `by` (their Kubernetes username) and must not set `byID`. The
  [finding-actions policy](../cli.md#permissions) still decides whether they may take the action at all.
- **`<release>-attribution-evaluation-submitter`** applies in the release namespace, and only with the
  evaluation-controller. An Evaluation created by anyone but the evaluation-controller (whose API stamps the verified
  submitter) must name its creator in `spec.submitter`.

- **`<release>-attribution-run-stamps`** applies in the release namespace. A new `Investigation`, `Remediation`,
  `IntentRun` or `EvaluationUnit` may carry `spec.attributedTo` only when the controller of that kind creates it:
  investigation-, remediation-, intent- or evaluation-controller respectively. Anyone else, another of those controllers
  included, creates a run without a stamp, and its Job is then annotated `org`.
- **`<release>-attribution-intent-requester`** applies in the release namespace, and only with the intent-controller. An
  Intent created by anyone but the intent-controller (which reads the requester from the issue's events) must not carry
  `spec.requestedBy.userID`, so a requester named by login alone is never attributed a run.

Together with the immutable run specs, these mean that only the run controllers decide attribution. A prompt-injected
agent, or anyone holding `create` on pods or Jobs, cannot bill a run to someone else.

## Trusted usage

Under `attribute`, while the egress broker deploys, the chart also turns on trusted usage. Each run's `status.usage`
(`status.stage.usage` on an Investigation or Remediation) is the pod's own report, which a repository image could
misreport. The broker meters the same traffic, and when a run settles its controller now records the broker's figure
beside it:

| Field      | Meaning                                                                                                   |
| ---------- | --------------------------------------------------------------------------------------------------------- |
| `measured` | The broker's figure: tokens by class and per model, the unmetered shortfall apart, a cost and its pricing |
| `source`   | `measured`, `partial` or `reported`: which figure to trust                                                |
| `final`    | The controller has stopped waiting; `measured` and `source` will not change                               |

How a settled run is measured:

- The controller asks the broker's usage endpoint for the run's Job. While the pod still has requests in flight, or the
  broker cannot be reached, it asks again every 10 seconds, for at most 2 minutes.
- `measured` when the broker booted before the Job was created, so its ledger saw the whole pod. `partial` when it
  booted later (a restart mid-run), or the wait ended with requests still in flight. `reported` when it has no entry for
  the Job, never answered, or the run ran on a harness the broker does not carry (codex, copilot): only the pod's own
  counts exist, and they are kept out of trusted totals.
- Only tokens the responses reported are priced. The shortfall the broker charged for cut or usage-less responses is
  shown as unmetered (worst case) and never priced.
- Model ids are normalised to the model registry (dated and provider forms included). A model the registry does not know
  keeps its tokens with no cost, and the run's total cost is then absent rather than understated.
- Prices are the registry's list rates, cache writes at 1.25x (five-minute) and 2x (one-hour) the input rate. Each
  priced figure records the price-table version it used, `<date>.<digest>`, whose digest changes with any rate. A run
  paid by a Claude subscription (`--anthropic-auth token`) is priced `subscription`: the list-price equivalent,
  notional, never money spent.

| Helm                          | Flag                                                  | Env                                                   |
| ----------------------------- | ----------------------------------------------------- | ----------------------------------------------------- |
| `attribution.mode: attribute` | `--usage-measurement`, `--usage-broker-url`           | `PATCHY_USAGE_MEASUREMENT`, `PATCHY_USAGE_BROKER_URL` |
| `attribution.mode: attribute` | `--usage-addr`, `--usage-callers` (the egress broker) | `PATCHY_USAGE_ADDR`, `PATCHY_USAGE_CALLERS`           |

The controllers authenticate to the broker with a projected ServiceAccount token for the `patchy-egress-broker-usage`
audience, which the chart mounts at `/var/run/secrets/patchy/broker-usage/token`. The broker accepts it on the usage
endpoint only, from those ServiceAccounts only; see [the egress broker](egress-broker.md#usage-endpoint). The broker
must run as one replica.

Runs that settled before trusted usage was on are settled as `reported` the first time their controller sees them: the
broker has no record of them.

## Usage rollups

A run's usage dies with the run: Findings expire after their TTL, Intents after theirs. Under `attribute` each run's own
controller also folds the run into durable `UsageRollup` objects in the release namespace, which outlive the runs.

- **Scopes.** One object per scope: `user` (a person by GitHub id, `github-id:<id>`, or `org` for runs no person is
  behind), `user-project` (one person's runs of one Project's intents) and `project` (one Project's intents' runs, by
  anyone). Finding and evaluation runs have no Project, so they count in their user scope only. The object name is
  `usage-<scope>-<16 hex>` of a hash of the scope's keys; `spec.scope` is the truth, and `status.login` the person's
  last login, for display only.
- **Buckets.** Each object holds day buckets (`2026-10-16`, the last 400 days) and month buckets (`2026-10`, at most 120
  months). A run lands in the UTC day and month of its completion (`finishedAt`; its creation when it never finished).
  Every bucket holds the runs, the metered tokens by class, the unmetered shortfall, the list-price cost and, apart, the
  notional cost of subscription-paid tokens, and splits runs, tokens and both costs by run kind (`investigation`,
  `remediation`, `plan`, `build`, `revise`, `check-fix`, `evaluation`). All costs are integer micro-USD.
- **Trusted and reported.** Only a run whose usage is final and `measured` or `partial` adds to the token and cost
  totals (`partialRuns` counts the partial ones). Any other run (a codex or copilot run, no broker figure, measurement
  off, or a run deleted before its usage settled) is counted in `reportedRuns`, with the pod's own tokens and cost in
  `reportedTokens` and `reportedCostMicroUSD`, never in a trusted total. A metered run with tokens of a model that has
  no price is counted in `unpricedRuns`: its tokens are in, its cost is not.
- **Top intents.** A user scope's month bucket also keeps that person's ten most expensive intents of the month (cost,
  then tokens). It is kept as runs arrive: an intent enters a full list only when its runs so far outrank the last
  entry, so the list can miss an intent built up from many cheap runs; the bucket's totals never miss anything.

How a run is folded in, exactly once:

- A run created under `attribute` carries the `patchy.bitwisemedia.uk/usage-rollup` finalizer. It is folded in once it
  is `Complete` or `Failed` and its usage is `final` (at once when measurement is off, or the run never reported a
  stage), or at once when it is deleted. Each rollup's `status.recent` ledger records the run's UID (`u:<uid>`), so a
  crash or a conflict between the rollup write and the run's own write never counts it twice.
- The run is then marked `UsageRolledUp` and the finalizer removed, so its deletion, by TTL or by hand, never waits on
  the rollup. A run created before the feature, or under `off`, has no finalizer and is never folded in or held.
- remediation-controller, which hosts the FindingRollup loop, compacts the rollups hourly under `attribute`: day buckets
  past the 400-day window are dropped. Under `off` the loop does not run, so an install that never opted in neither
  lists nor logs anything about rollups; rollups left from an earlier `attribute` wait until it is turned back on. Month
  totals are never touched: every run is in its month as well as its day.

Each controller is granted only `create` and `get` on `usagerollups` and `get` and `update` on their status (plus `list`
for remediation-controller's compaction), all uncached, by name; the chart grants them only under `attribute`. No one
may delete a rollup through the chart's roles. Rollups live only in etcd, so a cluster rebuild resets them.

`kubectl get usagerollups` (short name `ur`) lists them. The dashboard's Usage page and `patchy get usage` are not built
yet.

## Turning it off

Set `attribution.mode: off`. The controllers stop stamping new runs and annotating new Jobs, stop settling usage, and
the policies, the broker's usage endpoint and the controllers' grants on `usagerollups` are removed. Stamps already
written stay on their runs: the specs are immutable, and the stamps are only records. The rollups stay, and so does
everything already in them.

A run created under `attribute` that settles after the grant is gone cannot be folded in. Its controller marks it
`UsageRolledUp` with reason `RollupForbidden`, logs a warning, and releases it, rather than hold its deletion forever.
To keep those runs' usage, pause the agents and let the runs in flight settle before turning attribution off.
