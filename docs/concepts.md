# Concepts

This page defines the words the rest of the documentation uses. Each entry says what the thing is in a sentence or two,
gives an example where it helps, and links to the page that covers it in depth.

Many of these words are also Kubernetes **custom resources**: objects maul stores in the cluster, which you can list
with `kubectl get <kind>`. Those are written in code font, like `Project`. Everything maul knows is in these resources;
there is no other database. [How it works](how-it-works.md) shows how they fit together.

## Intents

These words belong to the intent flow, where an issue becomes a pull request.

### Project

A `Project` is the configuration for one stream of intents. It names:

- the **intent repository**, where people open issues;
- the **application repositories**, where the changes are built (one, or up to eight when multi-repository intents are
  turned on);
- the **approvers**, the GitHub accounts whose labels, commands and reviews count;
- limits, such as how much one intent may spend and how many revision rounds it may have.

An operator writes a Project; `maul setup project` produces the manifest. Example: a Project named `shop` with the
intent repository `acme/intents`, the application repository `acme/shop` and the approver `alice`.

See [Add a GitHub organization or a Project](intents/add-an-organization.md) and the
[Project reference](configuration/intent-controller.md#projects).

### Intent repository

The GitHub repository whose issues are a Project's intents. Several Projects may share one intent repository; their
trigger labels tell them apart.

### Trigger label and approve label

Two GitHub labels drive an intent, and both count only when an approver applies them:

- the **trigger label** starts an intent from an issue. By default it is `maul:<project>`, for example `maul:shop`;
- the **approve label** approves the plan maul posted. By default it is `maul:approved`. The comment `/maul approve`
  does the same.

### Intent

An `Intent` is one requested change: one issue that carries a Project's trigger label. It is named
`<project>-<issue number>`, for example `shop-42`. It records the intent's **phase** (such as `Planning`,
`AwaitingApproval` or `InReview`), its pull requests and what it has spent.

The phases are listed in [State machine & labels](labels.md#intent-phases).

### IntentRun

An `IntentRun` is one attempt of one agent stage for an intent. A run that launches is one Kubernetes Job. The stages
are:

| Stage         | What the agent does                                                                     |
| ------------- | --------------------------------------------------------------------------------------- |
| `plan`        | Reads the code, changes nothing, and writes the plan.                                   |
| `build`       | Builds the approved plan in the repository's own image.                                 |
| `revise`      | Works on the open pull request: a revision round, a CI-fix round or a verify-fix round. |
| `evaluate`    | The evaluator: tests the change against the plan's outcomes.                            |
| `checkreview` | The check reviewer: judges whether each new check proves its outcome.                   |

A run is `Pending`, `Running`, `Complete` or `Failed`, and a finished run records an outcome, such as `ok` or `timeout`.
When an attempt fails, the next attempt is a new `IntentRun`.

### Plan

The document a plan run writes: what will change and how. maul posts it on the issue exactly as written, and the build
receives exactly the plan that was approved. If the issue or the plan comment changes after the plan was posted, an
approval is refused.

### Approver

A GitHub account listed in the Project's `approvers.logins`. Only an approver's trigger label, approve label, `/maul`
commands and reviews have any effect. The account must also have write access to the repository it acts on, and must not
be a bot.

### Round

A round is one more pass of work on an intent after its pull request is open. The word is used for four things:

- a **revision round**: an approver's review requests changes, or an approver comments `/maul revise`, and an agent
  changes the pull request;
- a **CI-fix round**: a check the Project names fails on a commit maul pushed, and an agent tries to fix it;
- a **verify-fix round**: the evaluator found a failure maul could reproduce, and an agent tries to fix it;
- an **evaluator round** (also called a verification): one pass of checking the change against its outcomes.

The first three are `revise` runs: when one succeeds, it pushes a new commit to the pull request. Each kind has its own
limit on the Project.

### Preview

A `Preview` is a running copy of an intent's pull request, deployed inside the cluster so that people can try the change
before merging. It is served at `https://<intent>.<host suffix>`, for example `https://shop-42.preview.example.com`.
Each preview runs in one of a fixed number of isolated **slots**; when every slot is taken, a preview waits as `Queued`.

Previews are optional. See [Set up previews](intents/set-up-previews.md) and
[preview-controller](configuration/preview-controller.md).

### Outcome

An outcome is one plain sentence in the plan that says what the change must make true, or must leave true. Example: "A
player you add on the players page is listed there by the name you gave." A plan may state up to 20.

Outcomes are what the evaluator checks. An outcome marked `manual` is one only a person can judge ("the new drum kit
sounds good"); an approver closes it with `/maul accept`. Outcomes exist only when the agent evaluator is on. See
[What a plan declares](intents/acceptance.md#what-a-plan-declares-outcomes).

### Evaluator

The agent evaluator is an optional agent that tests an intent's change the way a careful tester would. It explores the
running preview, writes checks for the outcomes, and reports what it found. maul does not take its word: an outcome is
**confirmed** only on a check that maul ran again itself and that a second model, the **check reviewer**, said proves
the outcome.

A Project with no preview can be evaluated too, if it builds a command-line program or a Go library. See
[The agent evaluator](intents/evaluator.md).

### Tool pod

The pod the evaluator works through. The evaluator itself can reach only the model and its tool pod; the tool pod sends
the requests, drives the browser and runs the checks. It keeps a **ledger**: a record of every call and its result,
which is what maul trusts.

### Saved check

A saved check is a check the evaluator wrote that maul keeps for the intent, after maul re-ran it and the check reviewer
accepted it. A check is bound to one outcome. It is a list of HTTP requests with expected answers, a list of browser
steps with assertions, or a program whose output is asserted on.

Saved checks are replayed at the start of every later round, without a model.

### Rung

The rung is how much of an evaluation one evaluator round runs. There are two:

- `full`: the saved checks are replayed, then the evaluator explores and may write new checks;
- `replay`: only the saved checks are replayed. No model runs, so the round costs nothing in model spend.

maul picks `replay` only when the commits and outcomes are those of the last full round, that round's evaluator raised
nothing, and every outcome that is not manual already has a proven saved check. See
[The cost ladder](intents/evaluator.md#the-cost-ladder-replay-rounds).

### PreviewVerification

A `PreviewVerification` is the request for one attempt of one evaluator round, and the record of what its tool pod
observed. Its phase is an observation, never a verdict: `Observed` means the ledger was read, not that the change
passed. maul decides pass or fail from the ledger afterwards.

### `maul/acceptance`

The commit status maul posts on an evaluated pull request. It is green only when every outcome is confirmed, or an
approver accepted it, at the pull request's current commits. See
[The `maul/acceptance` commit status](intents/acceptance.md#the-maulacceptance-commit-status).

### Hand-off

The hand-off is the moment maul gives people the preview link and says whether the change is ready. With the evaluator
on, the default is a `gated` hand-off: the link is held until the outcomes are confirmed or maul gives up and says why.
An `early` hand-off posts the link as soon as the preview is live. See
[The hand-off](intents/evaluator.md#the-hand-off).

## Findings

These words belong to the findings flow, where a security alert becomes a fix.

### Finding

A `Finding` is one security problem in one repository, built from one or more scanner alerts. Alerts for the same
repository and the same advisory family that arrive within the accumulation window (one hour by default) are merged into
one Finding. It records the finding's phase, from `Opened` to `Remediated`, and maul opens one GitHub tracking issue for
it.

See [The findings flow](how-it-works.md#the-findings-flow) and [State machine & labels](labels.md#phases).

### Integration

An `Integration` connects maul to one outside system that sends or enriches findings: GitHub code scanning, Google Cloud
Security Command Center, Wiz, or any system that speaks the generic HTTP contract. It holds that system's webhook secret
or credential. See [Integrations](integrations/index.md).

### Investigation and Remediation

An `Investigation` is one attempt at analysing a finding; a `Remediation` is one attempt at fixing it. They are to a
Finding what an `IntentRun` is to an Intent.

## Shared

### Forge

A `Forge` tells maul which credential covers which repositories: for example, that the GitHub App stored in one
Kubernetes Secret covers the organization `acme`. It is the only resource whose credential downloads or writes
repository contents. GitHub is the supported forge. See [The GitHub Forge](integrations/forges/github.md).

### Repository

A `Repository` is a copy of one repository at one exact commit. The source-controller downloads it once and serves it
inside the cluster as an archive with a checksum. Agent pods fetch that archive without any credential. maul creates
these resources itself; you do not write them.

### Agent Job

Every agent run is a Kubernetes Job in the agents namespace. Its pod is sandboxed: for Claude and Codex it holds no
GitHub token and no model key, and its network reaches only what maul allows. See the
[isolation model](deployment/isolation.md).

### Agent image

The container image an agent Job runs in. maul's default image carries no language toolchain. A repository can declare
its own image, holding its compiler and test tools, in a committed file (`.maul/agent.yaml`); intent builds need one by
default. See [Bring your own agent image](integrations/agent-images.md).

### Harness

The harness is the coding-agent program maul runs inside the pod. A Project picks `claude` (Claude Code, the default) or
`codex` (OpenAI Codex, which the operator must turn on first). See [The codex runner](configuration/codex.md).

### Egress broker

A proxy inside the cluster that holds the model credential. A Claude or Codex agent pod sends its model requests to the
broker, which adds the credential on the way out, so the pod never holds it. See
[egress-broker](configuration/egress-broker.md).

### Network profile

By default an agent pod cannot reach a package registry, so every dependency must already be in its image. A network
profile is an exception the operator grants to chosen repositories: it lets the Jobs that build or fix their code fetch
dependencies through maul's egress proxy, and nothing else. Repositories declare nothing; the operator maps profiles to
repositories in the Helm values. Profiles are off by default. See
[Network profiles](deployment/isolation.md#network-profiles-the-go-dependency-route) and
[egress-proxy](configuration/egress-proxy.md).

### `/maul` commands

Comments that start with `/maul`, such as `/maul approve` or `/maul revise`, are how people steer maul from GitHub. A
command is read only on the first non-blank line of a comment. Which commands exist depends on where you comment: a
finding's tracking issue, an intent's issue or an intent's pull request. On an intent, only an approver's commands are
acted on.

[Commands on issues and pull requests](commands.md) lists them all.
