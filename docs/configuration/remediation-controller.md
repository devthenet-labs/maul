# remediation-controller

Queue admission (approvals and revivals), the priority scheduler (bounded concurrency, aging against starvation),
remediation agent Jobs, the changeset push + pull request — the only place a forge **write** credential is exercised —
and the rollup/TTL loop, which makes it the one deleter of expired Findings.

```sh
remediation-controller serve --namespace patchy \
  --claude-agent-image ghcr.io/devthenet-labs/patchy/claude-agent-runner:v0.9.0 # match your installed release
```

## Pipeline flags

The [shared flags](index.md#shared-flags-every-controller), plus:

| Flag                               | Env                                     | Default                                      | Purpose                                                                                                                                                                                                                            |
| ---------------------------------- | --------------------------------------- | -------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `--max-attempts`                   | `PATCHY_MAX_ATTEMPTS`                   | `2`                                          | Remediation attempts per finding before it fails                                                                                                                                                                                   |
| `--max-concurrent-remediations`    | `PATCHY_MAX_CONCURRENT_REMEDIATIONS`    | `1`                                          | Simultaneously running remediation Jobs                                                                                                                                                                                            |
| `--agents-paused`                  | `PATCHY_AGENTS_PAUSED`                  | `false`                                      | Launch no new remediation Job ([pausing agents](investigation-controller.md#pausing-agents))                                                                                                                                       |
| `--attribution-mode`               | `PATCHY_ATTRIBUTION_MODE`               | `off`                                        | `attribute` stamps each new Remediation with whom it is for and annotates its Jobs ([run attribution](attribution.md))                                                                                                             |
| `--user-credentials`               | `PATCHY_USER_CREDENTIALS`               | `off`                                        | `enforce` holds a remediation attributed to a person with no `Valid` model credential (the Finding stays `Queued`, `CredentialRequired`; [holding a run](user-credentials.md#holding-a-run)); needs `--attribution-mode attribute` |
| `--account-url`                    | `PATCHY_ACCOUNT_URL`                    | _(unset)_                                    | The status page's Account page, named in the hold's condition                                                                                                                                                                      |
| `--usage-measurement`              | `PATCHY_USAGE_MEASUREMENT`              | `false`                                      | settle each finished Remediation's usage from the egress broker's meter ([trusted usage](attribution.md#trusted-usage)). Helm: on with `attribution.mode: attribute`                                                               |
| `--usage-broker-url`               | `PATCHY_USAGE_BROKER_URL`               | empty                                        | the broker's usage endpoint (its own port, 8082 in the chart); required with `--usage-measurement`                                                                                                                                 |
| `--usage-token-file`               | `PATCHY_USAGE_TOKEN_FILE`               | `/var/run/secrets/patchy/broker-usage/token` | projected token for the `patchy-egress-broker-usage` audience, read per request                                                                                                                                                    |
| `--priority-aging-interval`        | `PATCHY_PRIORITY_AGING_INTERVAL`        | `24h`                                        | Wait per effective-priority point of aging boost                                                                                                                                                                                   |
| `--priority-aging-cap`             | `PATCHY_PRIORITY_AGING_CAP`             | `25`                                         | Maximum aging boost                                                                                                                                                                                                                |
| `--priority-weight-severity`       | `PATCHY_PRIORITY_WEIGHT_SEVERITY`       | `0.3`                                        | Scheduling-priority weight of the scanner severity                                                                                                                                                                                 |
| `--priority-weight-exploitability` | `PATCHY_PRIORITY_WEIGHT_EXPLOITABILITY` | `0.3`                                        | Weight of the assessed exploitability                                                                                                                                                                                              |
| `--priority-weight-likelihood`     | `PATCHY_PRIORITY_WEIGHT_LIKELIHOOD`     | `0.2`                                        | Weight of the assessed likelihood                                                                                                                                                                                                  |
| `--priority-weight-impact`         | `PATCHY_PRIORITY_WEIGHT_IMPACT`         | `0.2`                                        | Weight of the assessed impact                                                                                                                                                                                                      |
| `--finding-ttl`                    | `PATCHY_FINDING_TTL`                    | `336h` (14 days)                             | How long completed findings are kept before deletion; `0` keeps them forever                                                                                                                                                       |

The four weights combine the investigation's ratings into the 0–100 scheduling priority the queue sorts on (severity 30%
/ exploitability 30% / likelihood 20% / impact 20% by default).

## Agent Job flags

The same per-harness runner flags as the [investigation-controller](investigation-controller.md#agent-job-flags):
`--claude-agent-image` / `--codex-agent-image` / `--copilot-agent-image` / `--fake-agent-image`, `--harnesses`, the
broker/provider flags for the brokered runners (`--broker-url` for claude and codex, `--claude-provider*`,
`--claude-model-map`), the credential triple for the non-brokered copilot runner (`--copilot-secret{,-key,-env}`; the
`--codex-secret*` flags are removed, see [Codex runs brokered](agent-runner.md#codex-runs-brokered)),
`--agent-namespace`, `--agent-service-account`, `--job-deadline`, `--job-ttl`, and the default CPU and memory of every
agent Job (`--agent-cpu-request`, `--agent-memory-request`, `--agent-cpu-limit`, `--agent-memory-limit`, all unset by
default), and `--agent-dns` ([agent DNS](investigation-controller.md#agent-dns), `cluster` by default). Each
non-brokered `--<harness>-secret-env` is validated against the credential env vars that harness accepts (copilot:
`COPILOT_GITHUB_TOKEN` / `GH_TOKEN` / `GITHUB_TOKEN`) and the controller refuses to start on a mismatch, on a missing
credential for an enabled non-brokered harness, on a claude or codex runner without `--broker-url`, on a leftover
`--codex-secret*` value while it would run codex, on `--user-credentials` other than `off` while codex is enabled
(per-user credentials cover Anthropic only for now), on a foundry model map that misses a claude-resolving allowlisted
model, or on an allowlisted model no enabled harness can run. The flag's own `--help` enumerates the accepted set,
rendered from the harness definition, so it cannot drift from what the validation allows.

`--repository-images` and `--agent-ephemeral-storage` work exactly as on the investigation-controller (see
[repository-declared runner images](investigation-controller.md#repository-declared-runner-images)): the remediation
runs the image the Repository pinned, under the same conditions, probe and pull-failure handling.

One difference: a remediation never falls back to the default runner image silently when the repository declares an
accepted image. The default image carries none of the repository's toolchain, so a fix made there cannot be built or
tested. When a launch cannot use the pinned image (`--repository-images` off, the sandbox breaker tripped, or a human
revived the finding with `approve` or `retry`), no Job is created: the run is marked `DefaultImageHeld` (outcome
`default_image_held`, not counted toward `--max-attempts`) and the finding goes to `HandedOff` with the reason on the
tracking issue. Approving it is accepting an untested fix: the next attempt runs on the default image. A repository that
declares nothing, or whose declaration was rejected under `onReject: default`, remediates on the default image as
before. Either way, a pull request from a default-image run opens with a warning that patchy did not build or test the
change.

Every remediation pull request also says whether the agent reports reproducing the bug: that its regression test failed
on the unfixed code and passes with the fix, with the command and short output excerpts it quoted, or a warning that
patchy could not reproduce the bug before fixing it. On a default-image run a claimed reproduction gets that warning
too, since that image cannot run the repository's tests. The reproduction is self-reported (patchy runs no test itself)
and is recorded on the Remediation's `status.reproduction`; see
[How it works](../how-it-works.md#7-remediation-in-priority-order).

| Flag                        | Env                              | Default | Purpose                                                                                                                                   |
| --------------------------- | -------------------------------- | ------- | ----------------------------------------------------------------------------------------------------------------------------------------- |
| `--repository-images`       | `PATCHY_REPOSITORY_IMAGES`       | `false` | Run a Repository's pinned repository-declared image (brokered claude runner only); the kill switch                                        |
| `--agent-ephemeral-storage` | `PATCHY_AGENT_EPHEMERAL_STORAGE` | —       | Ephemeral-storage request and limit on both agent containers (e.g. `8Gi`); **required** with `--repository-images`                        |
| `--changeset-max-entries`   | `PATCHY_CHANGESET_MAX_ENTRIES`   | `500`   | Most files (upserts plus deletes) a changeset from a repository-image run may touch; more is rejected before any forge call (must be > 0) |

### Network profiles

An operator network profile gives a remediate Job that runs the repository's accepted image, of a repository matching
exactly one profile, one more destination: the [egress proxy](egress-proxy.md) on that profile's port, with its go
command pointed there. The investigate Job never gets one. The Helm chart stamps all three flags from `egressProxy` and
`agent.networkProfiles`; the Remediation records the profile that ran (`status.runnerImage.networkProfile`), and the
pull request and `patchy describe remediation` say so.

| Flag                         | Env                               | Default | Purpose                                                                                                                                 |
| ---------------------------- | --------------------------------- | ------- | --------------------------------------------------------------------------------------------------------------------------------------- |
| `--network-profiles-enabled` | `PATCHY_NETWORK_PROFILES_ENABLED` | `false` | Apply operator network profiles; the next-Job kill switch (`egressProxy.suspendProfiles` sets it false). Requires `--repository-images` |
| `--network-profiles`         | `PATCHY_NETWORK_PROFILES`         | —       | The profile set as the chart renders it (a JSON array); refused at startup when empty or invalid while enabled, ignored while disabled  |
| `--egress-proxy-url`         | `PATCHY_EGRESS_PROXY_URL`         | —       | The egress proxy's base URL, `http://<host>` with no port or path; each profile's port is joined to it. Required while enabled          |

## Stage flags

The investigation report requests its own model, turn count, and token budget for the fix — but the turn and token
figures are an **estimate**, not a request that can be granted downward. They never shrink a run.

The budget has three levels:

- **Ceiling** (`--remediate-max-turns`, `--remediate-token-budget`) — what every remediation gets, whatever the
  investigation predicted and including when it predicted nothing. It is also the **approval threshold**: an estimate
  above it holds the finding in `AwaitingApproval` rather than queueing it.
- **Hard cap** (`--remediate-max-turns-hard`, `--remediate-token-budget-hard`) — the absolute limit. Approving an
  over-ceiling estimate _grants that estimate_, so the hard cap bounds what a single approval can authorize. It must be
  at or above the ceiling; the runner refuses to start otherwise.
- **Estimate** — advisory. Its only operational effect is the approval gate; everything else it does is reporting
  (estimate against granted against actual on the run, and the skew averages in the rollups, which feed back into the
  next investigation's prompt).

The model suggestion is still clamped: the spawner holds it to the allowlist and resolves the harness that runs it (the
model's provider decides the runner image and credential). `--remediate-model` is the canonical fallback when the
report's suggestion is missing or off the allowlist; its harness is derived from it.

| Flag                            | Env                                  | Default                     | Purpose                                            |
| ------------------------------- | ------------------------------------ | --------------------------- | -------------------------------------------------- |
| `--model-allowlist`             | `PATCHY_MODEL_ALLOWLIST`             | canonical ids               | Canonical model ids remediation may run            |
| `--remediate-model`             | `PATCHY_REMEDIATE_MODEL`             | `anthropic/claude-sonnet-5` | Canonical default when the report requests none    |
| `--remediate-timeout`           | `PATCHY_REMEDIATE_TIMEOUT`           | `45m`                       | Wall-clock limit for the remediation stage         |
| `--remediate-idle-timeout`      | `PATCHY_REMEDIATE_IDLE_TIMEOUT`      | `20m`                       | End a run with no progress for this long; `0s` off |
| `--remediate-max-turns`         | `PATCHY_REMEDIATE_MAX_TURNS`         | `80`                        | Turns every run gets; approval threshold above     |
| `--remediate-token-budget`      | `PATCHY_REMEDIATE_TOKEN_BUDGET`      | `400000`                    | Output tokens every run gets; approval threshold   |
| `--remediate-max-turns-hard`    | `PATCHY_REMEDIATE_MAX_TURNS_HARD`    | `240`                       | Most turns an approval can grant                   |
| `--remediate-token-budget-hard` | `PATCHY_REMEDIATE_TOKEN_BUDGET_HARD` | `1200000`                   | Most output tokens an approval can grant           |

Token budgets are enforced live — the runner watches the harness's streamed usage events and kills the process group
when the cumulative output-token count is exceeded; the harness CLI has no such flag itself. The idle timeout is the
runner's no-progress watchdog ([agent-runner](agent-runner.md#the-idle-watchdog)): a remediation that waits that long on
a command that never returns ends as a `timeout` naming the command, instead of holding its slot until the wall clock.

## Behavior

- **Queue admission** — `AwaitingApproval → Queued` and `HandedOff → Queued` on an accepted approval (`spec.approval`,
  written by the status page, the CLI, or the integration-controller's projection once a `/patchy approve` on the
  tracking issue is authorised).
- **Scheduling** — `Queued → Remediating` when a slot frees, highest effective priority first; waiting findings gain +1
  effective priority per `--priority-aging-interval` up to `--priority-aging-cap`, so low-priority work cannot starve.
  Each grant creates one immutable `Remediation` child and its agent Job.
- **Verification, then push** — the agent's `commit.sh` must run cleanly and leave real commits; the controller then
  replays the changeset through the GitHub Git Data API (blob → tree → commit → ref) onto the `patchy/<finding>` branch
  with a scoped write token — no git binary, no clone — opens the pull request, and moves the finding to `InReview`. A
  recoverable failure re-queues (`Remediating → Queued`) within `--max-attempts`; exhaustion is `Failed`.
- **Changeset validation** — before any forge call, every changeset is checked against what a legitimate run produces:
  its base must be the Repository's pinned commit (`status.resolvedSHA`), every path relative and git-shaped (no empty,
  `.` or `..` component, nothing under `.git/`, no NUL, valid UTF-8, at most 4096 bytes), and every upsert a regular,
  executable or symlink mode with base64 content. A run held to the repository-image rules — it, or the investigation
  whose report it acts on, ran a repository-declared image — is also refused more than `--changeset-max-entries`
  entries, a control character in a path, and any change under `.github/workflows/` or `.github/actions/`, because a
  pushed branch triggers CI with the repository's secrets before a human has read it. A refused changeset fails the
  attempt `changeset_rejected` with the reason on the finding, and no forge call is made. Default-image runs keep the
  last three unchecked, so upgrading with repository images off changes nothing.
- **Rollups and the TTL** — on terminal-phase entry the stage statistics are aggregated exactly-once (finalizer-backed)
  into the per-scope `FindingRollup` objects; completed findings older than `--finding-ttl` are deleted, and the rollups
  remain the durable record.
- **Usage rollups** — under `--attribution-mode attribute` each settled remediation is folded into its person's (or the
  org's) `UsageRollup`, and, in that mode only, the same loop compacts every `UsageRollup` hourly, dropping day buckets
  past 400 days; see [usage rollups](attribution.md#usage-rollups).
