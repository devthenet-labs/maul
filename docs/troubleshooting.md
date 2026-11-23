# Troubleshooting and FAQ

This page collects the problems people run into most often, what each one means, and what to do. It gathers tables that
also appear on the pages for each part of maul, and links there for the details.

## Where to look first

maul keeps all of its state in Kubernetes objects, so `kubectl` (or the [maul CLI](cli.md)) shows what it knows. The
examples use maul's default namespaces, `maul` for maul and `maul-agents` for agent pods.

```sh
kubectl get intents -n maul                                # phase, pull requests, revisions, cost
kubectl describe intent <name> -n maul                     # conditions: why it waits or is Blocked
kubectl get intentruns -n maul -l maul.run/intent=<name>   # each agent run and its outcome
maul check project <project> -n maul                       # is the Project set up correctly?
maul check previews -n maul                                # is the preview install working?
maul describe finding <name>                               # a Finding's state, runs and spend
```

- An intent's **issue** carries a status comment that maul keeps up to date: the phase, the cost, and why it waits.
- The [intents dashboard](intents/dashboard.md) shows each run's time limits, last activity and outcome in plain words.
- An agent pod's `prepare` init container logs the artifact fetch and the sandbox probe:
  `kubectl -n maul-agents logs <pod> -c prepare`.

## Setting up a Project

`maul check project` names what is wrong and how to fix it. The most common reports:

| `maul check project` reports                | Fix                                                                                                                                                           |
| ------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `ready`: intent-controller has not reported | Wait a minute and run the check again. If it stays, check that intent-controller runs in that namespace: `kubectl -n maul get deploy maul-intent-controller`. |
| `ready`: `ForgeSecretUnreadable`            | The App's credential Secret is missing, or intent-controller may not read it. Create the Secret, or apply the forge file `maul setup github-app` wrote.       |
| `ready`: `AppNotInstalled`                  | The GitHub App is not installed on that repository. Add the repository to the App's installation, at the address the message gives.                           |
| `forge`: `no forge matches repository`      | No Forge covers that organization. Run `maul setup github-app` for it, or apply its forge file.                                                               |
| `forge`: `ambiguous forge match`            | Two Forges list the same organization. Keep one and delete the other.                                                                                         |
| `agent-image`: declares no agent image      | The repository has no `.maul/agent.yaml`, so builds have no image to run in. Plans still run. Follow [Onboarding an application](intents/onboarding-app.md).  |
| `not found`                                 | The check is looking in another namespace. Pass `-n maul`; `kubectl get projects.maul.run -A` shows where the Project is.                                     |

[When a check fails](intents/add-an-organization.md#when-a-check-fails) has the full messages and fixes, including a
lost App private key and a Helm release not named `maul`.

## An intent does not move

**It stays in `Pending`.** Two common reasons:

- Its issue is blocked by other issues on GitHub. The intent waits until every blocker is closed as completed; the
  dashboard shows a **waiting** badge. An approver can start it anyway with `/maul start`. See
  [Intent dependencies](intents/dependencies.md).
- The Project already has its maximum of active intents (`limits.maxActiveIntents`, 2 by default). The dashboard says
  "waiting for an active slot"; the intent starts when another ends.

**Nothing happened after a command or a label.** maul reads GitHub by polling, so a label or a `/maul` command takes
effect at the next poll, not at once. A command must be on the first line of its own comment, and only an approver's
counts. See [Commands on issues and PRs](commands.md).

**It is `Blocked`.** The intent carries a condition that names the reason.
[Why an intent is Blocked](labels.md#why-an-intent-is-blocked) lists every reason with the wording the dashboard uses,
and what clears the common ones. The ones people hit most:

| Condition              | What to do                                                                                                                                                                                         |
| ---------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `BudgetExhausted`      | The intent spent its Project's cost ceiling (`limits.maxCostMicroUSD`, \$10 by default). Raise the ceiling on the Project.                                                                         |
| `RevisionLimitReached` | The intent used its revision rounds (`limits.maxRevisions`, 3 by default). Raise the limit, or merge the pull request.                                                                             |
| `ChecksFailing`        | CI-fix rounds did not fix a failing check. A person needs to look.                                                                                                                                 |
| `ImageRequired`        | The build has no accepted agent image. Declare or fix the repository's image: [Bring your own agent image](integrations/agent-images.md).                                                          |
| `BranchConflict`       | The branch `maul-intent/<intent>` is in a state maul will not build on. maul never force-pushes. For most reasons the block lifts once the branch, or the pull request maul did not open, is gone. |
| `CredentialRequired`   | The person the next run is for has no valid model credential. They store one on the [Account page](account.md).                                                                                    |

**No agent starts at all.** Check whether the operator has paused agents (next section).

## Agents are paused

The chart value `agent.paused: true` is a cluster-wide pause. While it is on, no new investigation, remediation, intent
or evaluation Job starts. Work queues in order and nothing fails or spends an attempt; Jobs already running finish. The
status page shows a banner to signed-in users, an intent's status comment says "Agents paused", and each held run
carries the condition `AgentsPaused`. Set `agent.paused: false` to let the queue drain. To pause one Finding, intent or
Project instead, set its `spec.suspend`. See [Pausing agents](configuration/investigation-controller.md#pausing-agents).

## A run failed

Each agent run ends with an **outcome**. The dashboard shows it in fixed words; `kubectl get intentruns` shows the code.
Most failed runs are retried as the next attempt, so one failure does not end the intent.

| Outcome                            | Meaning and what to do                                                                                                                                                                                                                                                                                                                                                  |
| ---------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `timeout`                          | The run reached its stage time limit, or made no progress for 20 minutes (no model turn and no tool result; often one command that never returned). The run panel names the command. A Project can lower a stage's `timeout`; raising it past the operator's limit needs the chart.                                                                                     |
| `budget_exceeded`                  | The run hit a spend or token limit: its output-token budget, or one of the egress broker's limits. The run's detail (`kubectl get intentrun <name> -o yaml`, `status.detail`) names the limit; the dashboard shows only the fixed wording. See [Costs and usage](usage.md#limits-on-spend).                                                                             |
| `report_invalid`, `report_missing` | The agent's report was missing or did not follow the format. On claude, maul first asks the same agent to repair its report, up to two times, before the attempt fails ([report repair](configuration/agent-runner.md#report-repair)).                                                                                                                                  |
| `unschedulable`                    | No node could fit the agent pod for 10 minutes. It does not count as an attempt and is not retried: a plan or build blocks the intent with `ResourcesUnavailable` until the Project changes or intent-controller restarts. Give the repository another resource class, or add nodes that fit ([resource classes](configuration/intent-controller.md#resource-classes)). |
| `changeset_rejected`               | maul refused the change before any push, for example because it touched `.github/`, `.maul/` or `.devcontainer/`. See [Security model](security.md#what-reaches-github).                                                                                                                                                                                                |
| `dependency_refused`               | The build needed a dependency the egress proxy does not allow for its network profile. See [network profiles](deployment/isolation.md#network-profiles-the-go-dependency-route).                                                                                                                                                                                        |
| `launch_refused`                   | The cluster refused to create the agent Job, for example an admission policy during an upgrade. It is retried as the next attempt.                                                                                                                                                                                                                                      |

A run on a repository image that carries the condition `SandboxRefused` (reason `SandboxUnenforced`) means the sandbox
probe found the pod's network open: the cluster does not enforce NetworkPolicy, or not yet. The attempt does not count,
but the controller that launched it (investigation-, remediation- or intent-controller) refuses repository images until
it restarts. Fix the network plugin first. On a default-image run, a `SANDBOX NOT CONFIRMED` line in the `prepare` log
is the same warning; the run goes on.

## Previews

**The preview is Ready but the app refuses every request (often with 403).** Your app checks the `Host` header. A
preview reaches the app by its preview host name, `<project>-<issue>.<host suffix>`, while the readiness check reaches
the pod by its address, so the pod looks healthy and every real request is refused. Allow the preview host suffix in the
app's host allowlist (for example Django's `ALLOWED_HOSTS`, Rails host authorization or a dev server's allowed hosts).

**The Project refuses a readiness path such as `/health.json`.** `readinessPath` allows only letters, digits, `/`, `_`
and `-`, so a path with a dot is refused. Use a path without one.

**A second preview asks me to sign in again, or flashes a redirect.** Each preview host keeps its own session. While
your sign-in session lasts, opening another preview signs you in through a short redirect without a prompt.

**`maul check previews` fails.** Its lines name the failing piece: DNS, the certificate, the gateway, Dex or the viewer
bindings. [Checking it](configuration/preview-gateway.md#checking-it) and [Set up previews](intents/set-up-previews.md)
explain each check.

## Acceptance verification

With the [agent evaluator](intents/evaluator.md) on, maul posts a `maul/acceptance` commit status on the pull request.

| You see                                                       | Meaning                                                                                                                                                                                                                                                                                    |
| ------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| No `maul/acceptance` status at all                            | Verification is off for this intent, the App lacks Commit statuses write (the Project shows `AcceptanceStatusUnavailable`), or the intent has ended                                                                                                                                        |
| `pending`, "waiting for the preview"                          | There is no live preview at exactly the pull request heads yet. Check `kubectl get preview -n maul`                                                                                                                                                                                        |
| `error`, "maul could not run the checks"                      | The tool pod failed three times for infrastructure reasons (scheduling, image pull, quota, deadline). It is not a verdict on the change                                                                                                                                                    |
| No preview link, "maul holds its link"                        | The evaluator is still checking, and maul holds the preview link until it decides (a gated hand-off). The link is released when it decides, after the Project's `handoffMax`, or at once on your comment, a push or `/maul preview`. See [The hand-off](intents/evaluator.md#the-hand-off) |
| "maul couldn't confirm this"                                  | maul gave up without confirming every outcome, for the reason it names. It never blocks: accept the outcome, run `/maul verify`, or merge                                                                                                                                                  |
| An outcome "unsure", the evaluator disputes its failing check | A saved check failed and the evaluator judged that it shows no defect. No fix round runs. See [Disputes](intents/evaluator.md#disputes)                                                                                                                                                    |

[When something is not what you expect](intents/acceptance.md#when-something-is-not-what-you-expect) has the full table.

## The findings flow

| Symptom                                    | Likely cause                                                                                                         |
| ------------------------------------------ | -------------------------------------------------------------------------------------------------------------------- |
| Webhook deliveries show `401`              | The webhook secret differs between the App and the `maul-github` Secret, or no Integration exists                    |
| Findings stall at `Enhanced`               | The accumulation window or the minimum age has not passed yet. `kubectl describe finding` shows the timestamps       |
| Findings bounce `Investigating → Enhanced` | A Job launch or stage failed and is retried, up to two attempts, then the Finding is `Failed`                        |
| `ForgeResolved: False`                     | No Forge matches the repository (`NoForgeMatch`), or two match equally (`Ambiguous`)                                 |
| Issue projection or artifacts failing      | The `maul-github` credential is a placeholder, or the App lacks a permission                                         |
| Agent pods `CreateContainerConfigError`    | The copilot runner's credential Secret is missing in `maul-agents`. claude and codex pods mount no credential Secret |

[Verify the pipeline](getting-started/verify.md#troubleshooting) has the findings flow's own table.

## Model traffic and the egress broker

- **Every claude run fails at once, with `401 Invalid bearer token`.** With a Claude setup-token as the organisation
  credential, the Secret does not hold a valid token: often it holds the command's whole output, or a token with a line
  break in it. A setup-token starts `sk-ant-oat01-` and contains no whitespace. Recreate the Secret; the broker reads it
  on every request, so no restart is needed.
- **Runs fail with broker errors.** The broker is not ready. Check its `/readyz`: the model credential Secret may be
  missing from the release namespace, or its cloud identity unusable.
- **`egress broker not reachable yet` in a controller's startup log.** Logged once when the controller started before
  the broker. Harmless if runs then succeed.
- **A request is refused with `413`.** The agent's context holds many images (screenshots, diagrams) and passed the
  broker's 2 MiB request limit. Raise `egressBroker.limits.maxAnthropicRequestBytes`.
- **A codex run over a spend limit ends `runtime_error`.** codex does not pass the broker's message on, so the run
  cannot say `budget_exceeded`. The broker's audit line shows the refusal.

[egress-broker: Troubleshooting](configuration/egress-broker.md#troubleshooting) and its
[known limitations](configuration/egress-broker.md#known-limitations) have more.

## FAQ

**Why is the token count so much larger than the cost suggests?** An agent re-sends its whole conversation every turn,
and the part the model has seen is counted as cache reads, which cost a tenth of an input token. A long run is mostly
cache reads. [Sizing limits for large features](intents/sizing.md#why-cache-reads-dominate-long-context-runs) explains
it with measured runs.

**Can an agent change our CI workflows?** An intent's change may never touch `.github/`, `.maul/` or `.devcontainer/`,
and a Finding remediation on a repository-declared image may not change `.github/workflows/` or `.github/actions/`; maul
refuses such a change before anything is pushed. A Finding remediation on the default image may change CI files, and its
pull request, like every other, waits for a person to merge it.

**Why did the plan not run our tests?** Plans are read-only: the planner reads the code and writes the plan. On claude
it has no shell at all; a codex plan has one, but is told to stay read-only, and nothing it changes leaves the pod. The
build runs the tests, in the repository's own agent image.

**Can the agent install a dependency during a build?** Not by default: with narrow egress (repository images on, which
builds need), an agent pod has no route to a package registry, so dependencies must be in the agent image. An operator
can grant a [network profile](deployment/isolation.md#network-profiles-the-go-dependency-route) for allowlisted
packages.

**A flaky CI check started a CI-fix round.** CI-fix rounds act on the checks the Project names in `spec.checks.fix`.
Leave flaky checks out of that list, or set `spec.checks.rerunFailed: true`, which re-runs a failed GitHub Actions job
once before maul spends a fix round on it (it needs the App's Actions write permission).

**Is a local cluster's sandbox real?** Only if its network plugin enforces NetworkPolicy. Older kind releases' kindnet
ignores NetworkPolicy, so check the sandbox probe's `SANDBOX NOT CONFIRMED` line in an agent pod's `prepare` log before
trusting a local install; k3s enforces it.
