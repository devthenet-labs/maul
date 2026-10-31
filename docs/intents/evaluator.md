# The agent evaluator

With the agent evaluator on, maul checks an intent's change the way a careful tester would, before it tells anyone the
change is ready. The approved plan states **outcomes** in plain words ("rating a battle moves the winner above the
loser"). Once CI is green and the preview runs the pull request heads, maul starts an **evaluator**: a brokered agent
run that reads the issue, the outcomes and the list of changed files, then explores the live preview and writes its own
checks. maul re-runs each check without the model, has a second, cheaper model confirm that the check proves its
outcome, and only then counts the outcome as confirmed.

This page explains how a round works, how to read what it found, what a dispute is, and when the preview link reaches
people. [Acceptance verification](acceptance.md) covers how to turn it on, what a plan declares, the commit status and
the commands.

It is off by default (`intentController.verification.evaluator: false` in the chart). Off, verification runs slice 1's
acceptance criteria, as [Acceptance verification](acceptance.md#without-the-evaluator-acceptance-criteria) describes.

!!! note "What the evaluator's word is worth"

    The evaluator's report alone never passes an outcome. A pass needs a check maul stored, re-ran itself at the
    current heads with a fresh run token and saw pass, and that the check reviewer said proves the outcome. Its words
    go to people; its checks are what maul trusts.

## What runs where

| Piece                   | Where                                                                   | What it holds and reaches                                                                                                                                                                                                                                                                                               |
| ----------------------- | ----------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| The evaluator           | an `evaluate` IntentRun in the agents namespace, on maul's runner image | A pod-bound broker token; the issue, outcomes and changed paths, all framed as data. It reaches the broker and its own tool pod on port 8090, nothing else: no DNS, no internet, no application component, no copy of the pull request's code. Its prepare step proves that with the strict sandbox probe, DNS included |
| The tool pod, `tools`   | the Preview's slot, one per round                                       | maul's relay (`preview-tools`). It runs every tool call, writes the **ledger** (every call and its result, on its own stdout) and runs the checks. No service-account token, no DNS                                                                                                                                     |
| The tool pod, `browser` | the same pod, when the Project declares `surfaces.browser`              | Headless Chromium behind maul's CDP shim, which speaks CDP only to a caller holding the round's key (`tools`). Holds nothing secret                                                                                                                                                                                     |
| The tool pod, `sandbox` | the same pod                                                            | maul's toolbox image (curl, grpcurl, websocat, node, python, go) and the exec shim. Programs the evaluator writes run here, as their own user, with no secret                                                                                                                                                           |
| The check reviewer      | a `checkreview` IntentRun, one per round                                | The round's new checks as maul renders them, and the outcome texts. Never the application, a step's name or the evaluator's notes. Reaches only the broker                                                                                                                                                              |
| Log and event capture   | preview-controller                                                      | Bounded, redacted log tails of each component and the slot's pod events since the round began, handed to `tools`. Neither the evaluator nor the tool pod has any Kubernetes access                                                                                                                                      |

The evaluator acts only through one shell command, `maul-tool`:

| Command                                          | Does                                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| ------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `maul-tool http <METHOD> <path> [json]`          | One request to the component that serves the path on the preview host, with the headers the preview gateway sends                                                                                                                                                                                                                                                                                                                                                   |
| `maul-tool browser <verb> [args]`                | One Playwright CLI action in a session `tools` opened (1280x800): `goto`, `snapshot`, `click`, `fill`, `type`, `press`, `hover`, `select`, `screenshot`, `console`, `requests`, `eval` (a function run in the page, inside the browser sandbox, never on the tools host) and the like. Navigation stays on the preview; `run-code` (Node-side code), request rewriting (`route`), file access (`upload`, `state-load`) and attaching to another browser are refused |
| `maul-tool exec [--file f]... -- argv`           | Runs a program in `sandbox` (a gRPC or websocket client, say); `tools` asserts on its output                                                                                                                                                                                                                                                                                                                                                                        |
| `maul-tool logs <component>`, `maul-tool events` | The captured log tail of a component, and the slot's pod events and container states (restarts, `OOMKilled`, probe failures)                                                                                                                                                                                                                                                                                                                                        |
| `maul-tool check run <file>`                     | Validates a check the evaluator wrote, compiles it, and runs it against the preview now: a dry run, recorded with the check's digest                                                                                                                                                                                                                                                                                                                                |
| `maul-tool check results`                        | The replay's results and each dry-run check's latest run                                                                                                                                                                                                                                                                                                                                                                                                            |

Every call, refused ones included, is a ledger entry. Output separates what maul measured (`[observed]`) from the bytes
the application chose (`[untrusted output]`, always last).

## One round

1. **The gate.** As in slice 1: every open pull request's checks green at its head, no check-fix round due, no human
   round pending, the preview live at exactly those heads, and rounds and evaluation budget left.
2. **The request.** intent-controller creates a `PreviewVerification` for the outcomes and the saved checks. Each
   attempt gets a new one, since a tool pod takes one finish.
3. **The tool pod.** preview-controller re-checks the request against the Preview, renders the tool pod and one ingress
   policy that admits exactly this round's evaluator, and writes the pod's address to `status.tools` once `tools` is
   ready. Only then is the evaluator created: its pod reaches the tool pod by a host entry fixed when the pod starts.
4. **Replay (no model).** `tools` runs every saved check of the intent at the current heads, each alone, in a fresh
   browser context, with its own fresh run token. A failing check is run once more; fail then pass is **quarantined**.
5. **Explore (the model).** The evaluator sees the replay's results, uses its tools, looks at screenshots, and writes a
   report: a verdict per outcome citing the ledger entries that show it, a dispute for any replay failure it judges is
   not a defect, the checks it proposes, and look-around anomalies. A proposed check must have a green
   `maul-tool check run` first: `tools` refuses to confirm a check whose digest has no green dry run. The report's
   frontmatter is decoded strictly (an unknown key refuses it): a verdict names its outcome as `outcome` (the older `id`
   is still read; both, differing, are refused), and a proposal names the check `file` it ran (or the `digest` that run
   printed). agent-runner reads the digest from that file, under the evaluator's scratch directory, and asks `tools` to
   confirm it; a proposal whose file does not resolve to a check of its id and outcome is dropped, with its reason in
   the run's transcript, never the whole report. A refused report's detail lists its frontmatter's key paths (never
   their values), and each repair message restates the exact frontmatter example and the decoder's lines.
6. **Look-around.** After the outcomes, the evaluator visits the routes the change touches (at most 8): a snapshot and a
   screenshot each, the console, then each component's logs and the slot's events for errors, panics, restarts or OOMs
   since the round began.
7. **Confirm (no model).** After the evaluator exits, `tools` runs each proposed check again with a run token of its
   own, alone, in a fresh context, and records the run. That ledger entry, never the evaluator's report, is what maul
   believes.
8. **Review.** One `checkreview` run per round reads each new confirmed check, opening a fresh session per check, and
   answers `proves`, `does_not_prove` or `unclear` for its outcome. Answers are cached by the check's and the outcome's
   digests, so a check is reviewed once. A check reviewed `proves` is stored; any other is dropped.
9. **Collect.** preview-controller reads the `tools` container's log (that container only), holds it to the count and
   sha256 in its termination message, writes an index of every entry to the request's `status.ledger`, and keeps the
   full ledger and the cited screenshots (re-encoded by maul) in ConfigMaps the request owns.
10. **Judge.** maul decides each outcome by the rules below, then routes the round: all confirmed, no anomaly, a clean
    preview: **ready**. A failure the evaluator agrees with: a verify-fix round. A dispute: one model-free replay round.
    Anything else: **maul couldn't confirm this**.
11. **Tear down.** The tool pod, its key, its ConfigMap and its ingress policy go when the request settles.

## What makes a pass

An outcome is **confirmed** only on a check bound to it that:

1. maul stored, is not retired, and is one of maul's forms: an HTTP step list, a browser check in maul's JSON grammar
   (compiled by maul to a Playwright test), or a sandbox program whose output `tools` asserts on;
2. is **substantive**: at least one step acts on the application and at least one assertion is about content (a status
   or exit code alone is not), and every list assertion is filtered to data the check created itself, named with
   `{{run}}`. A check over shared data (a global ranking, a global count) is refused when it is saved;
3. passed in this round's confirmation or replay, run by `tools` at the current heads with a token `tools` made;
4. was reviewed `proves` for this outcome's text;
5. and no other live check of the outcome failed this round.

A browser check never names a snapshot ref or a colour literal. Its `visible` assertion means visible to a person (in
the viewport, a non-zero box, opacity above 0.1, text with at least 1.5:1 contrast against what is painted under it),
not merely present in the page; `legible` is WCAG's 4.5:1 (3:1 for large text); `clickable` means the element itself
receives the click. A background maul cannot judge (an image, a paint effect) makes such an assertion indeterminate: the
check errs, which is unsure, never a fail. A saved check is at most 8 KiB as maul stores it, and an intent keeps at
most 40.

Everything else:

| Situation                                                                           | The outcome                                                                       |
| ----------------------------------------------------------------------------------- | --------------------------------------------------------------------------------- |
| A check failed, and the evaluator reports `fail` for that outcome citing the ledger | **failed**: a verify-fix round with a reproduction                                |
| A check failed, and the evaluator **disputes** it                                   | **unsure** this round (see [Disputes](#disputes))                                 |
| A check failed, and the evaluator neither agrees nor disputes                       | **unsure**: "a check failed, but the evaluator did not agree that it is a defect" |
| The evaluator says it holds (or fails), but no check maul re-ran shows it           | **unsure**: no pass and no fix without a check                                    |
| The only support is a check that passed only on its retry                           | **quarantined**                                                                   |
| A check failed while preview-controller saw a component crash, get evicted or OOM   | **unsure**, the preview's failure; never a fix                                    |
| maul's own machinery failed (a tool pod never ready, a ledger that does not hold)   | the attempt is retried; past its bound, unsure                                    |
| A manual outcome                                                                    | only a person closes it, with `/maul accept <id>`                                 |

## Disputes

A saved check can fail on a correct change: data from an earlier round got in the way, a component was still starting,
or the check was simply wrong. Fixing code for that wastes a round and money. So a failing saved check starts a fix only
when this round's evaluator agrees it shows a defect. If the evaluator **disputes** it instead, it says why and proposes
a replacement check.

- The outcome is **unsure** this round, never failed and never passed.
- Once the replacement is confirmed and reviewed `proves`, maul retires the disputed check (both are shown) and runs one
  **replay round** at the same heads, with no model: that round can pass the outcome. It counts toward
  `maxVerifications`.
- A check that failed in an earlier round is retired only this way, through a dispute with a confirmed replacement.

A dispute only ever moves an outcome from failed to unsure, and the replacement must meet every rule above, so it can
never make a pass on its own.

## Look-around

The outcomes are what the approver asked for; look-around is the rest of "does it look right". The evaluator visits the
routes the change touches and checks each against a short list: text and links readable against their background,
nothing overlapping or clipped or blank, new controls clickable and doing something, new links loading, no console
errors or failed requests, and nothing in the logs or events (errors, panics, restarts, OOMs).

An **anomaly** must cite ledger entries. It downgrades the hand-off to "maul couldn't confirm this: it noticed something
odd while it looked around", and the comment shows the evaluator's description, marked as its words. It never fails an
outcome and never starts a fix: no approved outcome stands behind it, and maul has no reproduction to hand a fix agent.

## The fix round's reproduction

A verify-fix round gets a bug report maul writes from its own ledger, never the evaluator's instructions, notes or other
checks:

- the approved outcome, quoted;
- the failing check's steps, each with what it expects (an http, browser or exec check, in plain words);
- what was observed on the failing run, as the check runner reported it;
- the component's log lines from the failing run's window, redacted, at most 40 lines, starting at the first panic,
  fatal, error, exception or traceback line. The window is between the evaluator's last `maul-tool logs` read before the
  failing run and its first one after it, so there is an excerpt only when the evaluator read the logs after the check
  failed.

`{{run}}` stays a placeholder in it: the fix must make the outcome hold for any token. The next round replays the check
(with a new token) and the evaluator reads the fix's diff for special cases.

## The hand-off

The hand-off is when people get the preview link and hear that the change is ready.

| Mode              | The link                                                                                                                                                                                                                                                                                                                                                        |
| ----------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `gated` (default) | Held until the evaluation decides. The status comment says "The preview is live, and maul holds its link until it has checked the approved outcomes." It is then released as "Ready: maul checked the approved outcomes." once every outcome is confirmed (or accepted by an approver) with no anomaly, or as "maul couldn't confirm this: ..." with the reason |
| `early`           | Posted as soon as the preview runs the heads, labelled as still being checked: slice 1's behaviour                                                                                                                                                                                                                                                              |

`gated` holds the link at most `handoffMax` (default 60 minutes, 10 minutes to 4 hours), counted from when CI first went
green at the first pull request heads; past it the link is released as "maul couldn't confirm this: it ran out of time".
**A human always wins:** an approver's changes-requested review or `/maul revise` or `/maul retry` on a pull request, a
push maul did not make, or `/maul preview` releases the link at once ("Shown while maul is still checking the approved
outcomes."), even before the first evaluation has started, and maul goes on checking. A plain comment, `/maul verify`
and `/maul verify fix` do not release it. The pull requests open as usual, not as drafts, so GitHub still notifies their
watchers; maul only holds its own words.

"maul couldn't confirm this" names one reason:

| Reason                                          | Meaning                                                                               |
| ----------------------------------------------- | ------------------------------------------------------------------------------------- |
| some outcomes are unsure                        | no check proves them yet; read each outcome's line                                    |
| an outcome fails and maul stopped fixing it     | a reproduced failure, and automatic fixing halted (out of fixes, a repeat, a human)   |
| it noticed something odd while it looked around | a look-around anomaly                                                                 |
| it used every round it may run                  | `maxVerifications` is spent                                                           |
| it spent its evaluation budget                  | the intent's evaluation ceiling is reached                                            |
| it ran out of time                              | `handoffMax` passed                                                                   |
| its sandbox was not enforced                    | the evaluator's strict probe found DNS, the API server, metadata or the internet open |
| the preview failed while it was checked         | a component crashed, was evicted or ran out of memory during the round                |
| it could not run its checks                     | maul's own machinery failed every attempt                                             |

"Couldn't confirm" is never a block. The intent stays in review, and an approver decides: accept the outcomes maul could
not confirm with `/maul accept <id>` (or `/maul accept all`), ask for another round with `/maul verify`, or merge.

Set the mode chart-wide with `intentController.verification.defaultHandoff`, or per Project:

```yaml
spec:
  verification:
    enabled: true
    handoff: gated # or early; unset follows the chart
    handoffMax: 60m # 10m to 4h
    maxToolCalls: 300 # 50 to 300 per round, never above the chart's
  surfaces:
    browser:
      entry: / # the path the browser opens first (the evaluator is told to start there); without it the evaluator has http and the sandbox only
  limits:
    maxEvaluateCostMicroUSD: 6000000 # what the intent's evaluate and checkreview runs may spend together
    evaluate: { maxCostMicroUSD: 1500000, maxTurns: 40 } # each evaluate run (limits.evaluate, like limits.build); fewer turns with less money, see below
    checkReview: { maxCostMicroUSD: 300000 } # each checkreview run (limits.checkReview)
```

`surfaces.browser.entry` must be an absolute path on the preview host (at most 256 visible ASCII characters, no space,
backslash, backtick or leading `//`, and no scheme or host); the evaluator's prompt names it as the page to open first.
A value that is not such a path is left out of the prompt.

The evaluation ceiling is counted apart from the intent's own `limits.maxCostMicroUSD`, so evaluation can never spend
what the fix rounds need; the chart's `maxEvaluateCostUSD` caps it for every Project. Under
`egressBroker.limits.unit: cost` each run's own cost budget is stamped on its pod as for every other stage: its stage's
`maxCostMicroUSD`, or the evaluation ceiling when that is unset, never more than the ceiling. Like every other stage,
`limits.evaluate` and `limits.checkReview` lower a run's turns, tokens and `timeout` but never raise them past the
controller's, and an evaluate or checkreview run budget over half the broker's hourly cost limit keeps the Project from
Ready (`RunBudgetOverHourlyLimit`).

**Each request reserves its output bound.** Under the cost unit the broker admits a request only while what the pod
spent plus the request's whole output bound, at the model's output price, fits the pod's budget, so a run can spend at
most its budget less one such reservation. agent-runner therefore bounds what each request of these two stages may
write: 32000 output tokens for evaluate (its largest response is a Write of its report or an exec check's program, each
under 64 KiB) and 8192 for checkreview (a review is one short report). On claude-sonnet-5 (\$15 per million output
tokens) that reserves \$0.48 and \$0.12 a request. A run budget under twice that keeps the Project from Ready
(`RunBudgetUnderRequestReserve`, naming the stage and the least budget it needs): on sonnet at least \$0.96 for evaluate
and \$0.25 for checkreview, on claude-opus-5 \$1.60 and \$0.41, on claude-haiku-4-5 \$0.32 and \$0.08. The example above
(\$1.50 and \$0.30) fits sonnet; on opus raise both. Without the bound a request reserved the CLI's default of 64000
tokens, \$0.96 on sonnet, and a \$1 checkreview run was refused after four calls. A codex run gets no such bound and is
not judged: a request that sets no `max_output_tokens` reserves the broker's `--max-tokens-ceiling`, if any.

**Turns come first.** Every tool call is one agent turn, so an evaluate run's turn cap (`--intent-evaluate-max-turns`,
chart `evaluateMaxTurns`, default 100; a Project's `limits.evaluate.maxTurns` only lowers it) is what it runs out of,
long before the round's 300 tool calls. A run that reaches it still reports: agent-runner gives the run's first part all
but its last few turns (a tenth, at least 6 and at most 12, never more than half: two repair rounds of at least 3 turns
each, enough to read the report, write it again and reply), counted in the CLI's own turns, and when that part ends on
the cap with no report it resumes the agent's own session on the kept turns, telling it to write the report now from
what it has: the checks whose dry run passed proposed, every outcome it did not show unsure. The prompt states the run's
turns and the turn by which to write the report, and asks for the report as soon as every outcome is shown or unsure. A
run stopped by its cost budget or its wall clock gets no such turn (the broker refuses the pod any further request, and
the clock has run out), so keep the turn cap the limit that binds first: live, an evaluate run spent \$1.26-2.51 over
its first 60 turns, 4-7 seconds each, a turn costing more as the session grows, so 100 turns fit the default \$6 run
budget and the 20 minutes. Raise the cost budget and the timeout before the turns, and lower the turns with a smaller
`limits.evaluate.maxCostMicroUSD` (about 40 turns for \$1.50).

## Reading the evidence

- **On GitHub.** The verification comment and each pull request's sticky comment list every outcome with its result
  (`confirmed`, `failed`, `unsure: maul couldn't confirm it`, `quarantined`, `accepted by a human`) and, for a decided
  one, **maul's check** in one line it renders itself, such as
  `http to api: POST /api/players, POST /api/battles, GET /api/leaderboard; 3 expectations`. Application output appears
  only in fenced blocks labelled as the application's, and the evaluator's own words (an anomaly, a note) are marked as
  its words, not checked by maul.
- **In the cluster.** Each round is a `PreviewVerification` in the release namespace
  (`kubectl get previewverifications -l maul.run/intent=<intent>`; maul keeps the newest three per intent). Its
  `status.ledger` indexes every tool call; its `status.tools` names the round's tool pod. The full ledger is in the
  ConfigMap `<verification>-ledger` (`ledger.jsonl`, each call's request and full output, with sha256s) and the cited
  screenshots in `<verification>-evidence`, both deleted with the request. The intent's stored checks are in the
  immutable ConfigMap `<intent>-checks-<n>` that `status.acceptance.checks` names, and the reviewer's answers are cached
  in `status.acceptance.reviews`.
- **On the status page.** With the [intents dashboard](dashboard.md#the-evaluation-evidence) on, an evaluate run's panel
  shows the same round: outcomes with maul's decided result, the checks and their compiled specs, anomalies, the
  hand-off and the ledger. A reader with the transcripts tier also sees each call's request and output, the evaluator's
  own verdicts and notes, and the cited screenshots.
- **Trust order.** Read maul's result first (`confirmed`, `failed`, `unsure`, `quarantined`), then **maul's check**
  beside it, which maul rendered itself. The evaluator's verdict and note are claims: they can disagree with the result,
  and an outcome is green only when maul's own checks support it. A ledger output is shown only where it hashes to the
  index, so an output marked dropped was cut to fit or is missing; treat it as unknown rather than as empty.
- **The runs.** The evaluator and the reviewer are ordinary IntentRuns of stages `evaluate` and `checkreview`, metered
  like every run; their reports are on the run's status.

Nothing of the evaluator reaches a build or fix agent beyond the reproduction above: not its prompt, its notes, the
check store or its transcript.

## codex: built, not verified

The evaluator is verified live on claude. A codex Project's intents are evaluated on codex only with
`--intent-evaluate-codex` (chart `intentController.verification.codexEvaluator: true`, needing the codex runner). Off,
the default, such a Project is not evaluated at all: its hand-off is `early`, and it carries the advisory condition
`EvaluatorUnverified` (reason `CodexEvaluatorOff`). On, the codex path runs as built and tested end to end against a
scripted codex, but no live run has proved it yet, screenshot viewing included: treat it as unverified until one has.

## Limits

| Bound                       | Default                                                            | Where                                                                                                                            |
| --------------------------- | ------------------------------------------------------------------ | -------------------------------------------------------------------------------------------------------------------------------- |
| One evaluate run            | 100 turns (the last few kept for its report), 20 minutes           | `--intent-evaluate-max-turns`, chart `evaluateMaxTurns`; `--intent-evaluate-timeout`, chart `evaluateTimeout`; `limits.evaluate` |
| One checkreview run         | 3 turns per check, 15 minutes                                      | `--intent-checkreview-max-turns`, chart `checkReviewMaxTurns`; `limits.checkReview`                                              |
| Tool calls per round        | 300 (50 to 300)                                                    | `verification.maxToolCalls`, chart `maxToolCalls`                                                                                |
| Look-around                 | 8 routes per round                                                 | fixed                                                                                                                            |
| Evaluation spend per intent | \$6                                                                | `limits.maxEvaluateCostMicroUSD`, chart `maxEvaluateCostUSD`                                                                     |
| Rounds and fixes            | as slice 1: `maxVerifications`, `maxVerifyFixes`                   | the Project's `limits`                                                                                                           |
| Hand-off wait               | 60 minutes                                                         | `verification.handoffMax`, chart `handoffMax`                                                                                    |
| Saved checks                | 40 per intent, 8 KiB each, 30 steps                                | fixed                                                                                                                            |
| Captured logs               | a bounded tail per component per round; 40 lines in a reproduction | fixed                                                                                                                            |

A tool pod lives as long as its evaluate run plus five minutes, under the slot's verification deadline
(`preview.verification.deadlineMaxSeconds`), and needs about 2.5 CPU and 2 GiB beside the Preview: see
[the preview-controller](../configuration/preview-controller.md#the-agent-evaluators-tool-pod).

## Known limits

- **No replan once the pull requests are open.** An approved plan cannot be replanned while its intent is in review:
  `/maul replan` and a re-applied trigger label are answered as not available there, and no notice offers them. To start
  over with a new plan, an approver comments `/maul cancel` on the intent's issue (the pull requests are left as they
  are) and opens a new issue. Replanning from review is an owner decision, deferred to a later release; until then the
  options in review are `/maul revise`, a verify fix and `/maul cancel`.
- **A model judging a screenshot is never support.** The evaluate model (`claude-sonnet-5`) was calibrated on
  screenshots of known defects and failed: it called 5 of 15 defect runs a pass, with confident evidence it invented,
  and failed 3 of 18 correct builds. It was right on gross structure (steps 14.8 px against 32 px, rows visibly offset
  after a swipe) and a coin flip below about 3 px (30 against 32 px, a label overlapping by 1.3 px). So a screenshot
  judged by the model alone never turns an outcome green: the answer is `unsure`. Pixel facts belong to the
  deterministic browser assertions (`size`, `aligned`, `above`), which measure exactly. Citing evidence does not fix
  this, because the citations themselves were invented. The rule would be reopened only with a gate per question class
  and a fresh run of the calibration set per model; a higher screenshot scale factor is untried.
- **codex is unverified and uncalibrated.** See [codex: built, not verified](#codex-built-not-verified). The calibration
  above was not run on codex or on a larger model.
- **After a preview-controller restart,** a browser round's captured logs and events stop refreshing and stay as last
  written, because the key that redacts them is not recoverable from memory. Status keeps updating.
- **The evaluation ceiling counts toward the hourly limit.** With the evaluator on, the default \$6 ceiling counts
  toward `RunBudgetOverHourlyLimit`: a broker `costPerHour` below \$12 keeps such a Project from Ready.
