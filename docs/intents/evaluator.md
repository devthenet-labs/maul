# The agent evaluator

With the agent evaluator on, an independent agent uses an intent's change the way a careful reviewer would, before maul
tells anyone the change is ready. The approved plan states **outcomes** in plain words ("rating a battle moves the
winner above the loser"). Once CI is green and the preview runs the pull request heads, maul starts an **evaluator**: a
brokered agent run that reads the issue, the outcomes and the list of changed files, then uses the live preview through
maul's tools, every call recorded by maul in the round's **ledger**. It reports, for each outcome, whether it saw it
hold, saw it not hold, or could not tell, citing the calls that showed it. maul counts a verdict only when it cites a
call of that ledger which used the application; whether the call shows what the verdict says is for the reviewers, who
see every cited call.

This page explains how a round works, how maul judges a verdict, how to read what the evaluator found, and when the
preview link reaches people. [Acceptance verification](acceptance.md) covers how to turn it on, what a plan declares,
the commit status and the commands.

It is off by default (`intentController.verification.evaluator: false` in the chart), and it is verification's one
switch: off, verification is off. No intent is verified, a Project that asks for verification carries `VerificationOff`
(reason `EvaluatorOff`), and each intent's status comment says verification is off and links its preview at once (the
early hand-off). Slice 1's acceptance criteria no longer run: see [Migrating from slice 1](#migrating-from-slice-1).
Switched off as a rollback, an open intent whose approved plan states outcomes is not verified at all: its
`maul/acceptance` becomes the final error "verification switched off", never a green, and its preview link is handed
over at once, until the evaluator is back on.

!!! note "What the evaluator's word is worth"

    An outcome "seen to hold" means the evaluator said it held and cited at least one call it made through maul's
    tools in this round that used the application, recorded by maul itself. maul checks the citation, never whether
    the call shows what the evaluator says: that is the evaluator's reading, and the reviewers' to judge, so every
    cited call is shown to them. Read it as "the evaluator saw this", not as proof, and review the code as you always
    did.

## What runs where

| Piece                   | Where                                                                   | What it holds and reaches                                                                                                                                                                                                                                                                                               |
| ----------------------- | ----------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| The evaluator           | an `evaluate` IntentRun in the agents namespace, on maul's runner image | A pod-bound broker token; the issue, outcomes and changed paths, all framed as data. It reaches the broker and its own tool pod on port 8090, nothing else: no DNS, no internet, no application component, no copy of the pull request's code. Its prepare step checks that with the strict sandbox probe, DNS included |
| The tool pod, `tools`   | the Preview's slot, one per round                                       | maul's relay (`preview-tools`). It runs every tool call and writes the **ledger** (every call and its result, on its own stdout). No service-account token, no DNS                                                                                                                                                      |
| The tool pod, `browser` | the same pod, when the Project declares `surfaces.browser`              | Headless Chromium behind maul's CDP shim, which speaks CDP only to a caller holding the round's key (`tools`). Holds nothing secret                                                                                                                                                                                     |
| The tool pod, `sandbox` | the same pod                                                            | maul's toolbox image (curl, grpcurl, websocat, node, python, go) and the exec shim. Programs the evaluator writes run here, as their own user, with no secret                                                                                                                                                           |
| Log and event capture   | preview-controller                                                      | Bounded, redacted log tails of each component and the slot's pod events since the round began, handed to `tools`. Neither the evaluator nor the tool pod has any Kubernetes access                                                                                                                                      |

A Project with no preview but a command-line program or a Go library has its tool pod in the agents namespace instead:
see [Without a preview: a CLI or a library](#without-a-preview-a-cli-or-a-library).

The evaluator acts only through one shell command, `maul-tool`:

| Command                                          | Does                                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| ------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `maul-tool http <METHOD> <path> [json]`          | One request to the component that serves the path on the preview host, with the headers the preview gateway sends                                                                                                                                                                                                                                                                                                                                                   |
| `maul-tool browser <verb> [args]`                | One Playwright CLI action in a session `tools` opened (1280x800): `goto`, `snapshot`, `click`, `fill`, `type`, `press`, `hover`, `select`, `screenshot`, `console`, `requests`, `eval` (a function run in the page, inside the browser sandbox, never on the tools host) and the like. Navigation stays on the preview; `run-code` (Node-side code), request rewriting (`route`), file access (`upload`, `state-load`) and attaching to another browser are refused |
| `maul-tool exec [--file f]... -- argv`           | Runs a program in `sandbox` (a gRPC or websocket client, say) and records what it printed and its exit status                                                                                                                                                                                                                                                                                                                                                       |
| `maul-tool logs <component>`, `maul-tool events` | The captured log tail of a component, and the slot's pod events and container states (restarts, `OOMKilled`, probe failures)                                                                                                                                                                                                                                                                                                                                        |

Every call, refused ones included, is a ledger entry, and prints its seq (`[maul-tool ledger seq N]`), which is what a
verdict cites. Output separates what maul measured (`[observed]`) from the bytes the application chose
(`[untrusted output]`, always last), and every call shows the time left of the evaluator's run
(`[maul-tool time left]`). Past the round's `maxToolCalls` every call is refused.

## One round

1. **The gate.** Every open pull request's checks green at its head, no check-fix round due, no human round pending, the
   preview live at exactly those heads, and rounds and evaluation budget left.
2. **The request.** intent-controller creates a `PreviewVerification` for the approved outcomes. Each attempt gets a new
   one, since a tool pod takes one finish.
3. **The tool pod.** preview-controller re-checks the request against the Preview, renders the tool pod and one ingress
   policy that admits exactly this round's evaluator, and writes the pod's address to `status.tools` once `tools` is
   ready. Only then is the evaluator created: its pod reaches the tool pod by a host entry fixed when the pod starts.
4. **Explore (the model).** The evaluator reads the issue, the outcomes, the changed paths, the previous round's results
   and, after a fix, the fix's diff, then uses its tools, looks at screenshots, and writes a report. Its prompt sets the
   rules it judges by:
   - `pass` is "I saw it hold, as worded", `fail` "I saw it not hold, as worded", `unsure` "I could not tell". If any
     call it made showed the outcome not hold, the verdict is `fail`, citing that call, or `unsure`: never `pass`,
     whatever its other calls showed. A failure that was there before the change is still a failure of the outcome.
   - It reproduces a failure before reporting it, and cites both calls.
   - It creates the data an outcome speaks of itself, named with a token maul makes unique per evaluation, and judges by
     that data: the preview's data persists between rounds and is shared.
   - It judges the rule an outcome states, not only its example, on inputs of its own beyond the example, and stays
     within what the outcome promises: an input the rule does not cover, or a value its words do not decide, is never a
     failure. A break of the rule an outcome's line of the request states beyond its example makes that outcome
     `unsure`, never `fail`: a fail and its fix are only for what the approved outcome states. A promise that something
     is carried over is tried on the values a careless copy loses (unset, zero, empty, absent).
   - It observes each outcome through the surface it names (the page by using the page, the API by calling it). A page
     script it runs with `browser eval` or a program it runs with `exec` counts, and is shown to the reviewers as its
     own; it uses one to read what a person could see or the API answers, never to set up the state it then judges.
   - It spends its turns on the outcomes first, within a turn budget per outcome, and writes its report as soon as every
     outcome has a verdict.

   The report's frontmatter is decoded strictly: `verdicts` (per outcome its `outcome` id, the older `id` still read,
   both differing refused; `verdict`; `ledger`, the seqs it cites, at most 8 that count kept; an optional one-line
   `note`) and `anomalies`. Any other key refuses the whole report, among them the keys an earlier release's evaluator
   wrote ([Migrating from saved checks](#migrating-from-saved-checks)), even as empty lists. A refused or missing report
   is repaired in the agent's own session, at most twice; a refused report's detail lists its frontmatter's key paths
   (never their values), and each repair message restates the exact frontmatter example. The evaluate prompt is bounded
   at 256 KiB, on claude and codex alike: both read it on their standard input, never as an argument of their command
   line (which Linux bounds at 128 KiB). A prompt over the bound leaves out half the changed paths at a time, each time
   saying so; the request, the fix diff, the outcomes and the previous round's results always stay, and a prompt still
   over the bound fails the run (maul's own failure, never a verdict).

5. **Look-around.** After the outcomes, the evaluator visits the routes the change touches (at most 8): a snapshot and a
   screenshot each, the console, then each component's logs and the slot's events for errors, panics, restarts or OOMs
   since the round began.
6. **Finish.** After the evaluator exits, agent-runner sends `tools` one finish naming at most 6 screenshots the report
   cites, its verdicts' first, and `tools` ends the ledger.
7. **Collect.** preview-controller reads the `tools` container's log (that container only), holds it to the count and
   sha256 in its termination message, writes an index of every entry to the request's `status.ledger`, and keeps the
   full ledger and the cited screenshots (re-encoded by maul) in ConfigMaps the request owns. A log that does not hold
   is maul's trouble (`Infra`, `ledger-mismatch`), never judged, and so is a round whose ledger holds no call from the
   evaluator.
8. **Judge.** maul decides each outcome by the rules below, then routes the round: every outcome seen to hold and a
   clean preview: **ready**, whatever look-around anomalies the evaluator noted. An outcome seen not to hold: a
   verify-fix round. Anything else: **maul can't call this ready**. A failing environment keeps the round from ready
   even when every outcome was seen to hold, and no later acceptance by an approver changes that at the same heads.
9. **Tear down.** The tool pod, its key, its ConfigMap and its ingress policy go when the request settles.

## What makes a pass

maul never reads the evaluator's words to decide a result. It reads, for each outcome, the evaluator's first verdict on
it and the seqs that verdict cites, and holds each cited seq to the round's own ledger index (`status.ledger`). A
citation **counts** only when all of these hold:

1. the round's ledger is verified: its log held to the count and sha256 of `tools`' termination message;
2. the seq is an entry of that index;
3. the entry is the evaluator's own call through `maul-tool`, never one of maul's (the relay's readiness, a build, the
   browser session, the finish);
4. maul did not refuse it (a malformed call, a missing key, a call past `maxToolCalls`);
5. it used the application: `maul-tool http`, `browser`, `exec` or `run`. A `logs`, `events` or `files` call may be
   cited, but never counts on its own;
6. it ran at the head: on a Project with no preview, a call with `--base` never counts;
7. a `browser` command exited 0 (a failed command showed nothing). An `exec` or `run` whose program ran counts whatever
   its exit status: a program may exit non-zero by design;
8. it got its answer: an `http` request that got no response (DNS, refused, a timeout, TLS, a reset) never counts, nor
   does a `run` whose program did not compile and so never ran, nor an `exec` or `run` whose program the sandbox could
   not start (exit 127 or 126, its output only maul's `[maul: could not start ...]`). The round's ledger index marks
   each `noAnswer`.

Whether a screenshot of the call was kept, and whether its output still fits the full ledger, does not matter: a cited
call whose output was dropped to fit still counts, and is shown as "Its output was not kept by maul." A page script run
with `browser eval`, a program the evaluator wrote and ran with `exec`, and every `run` count like any other call, and
the comments label them "the evaluator's own page script" or "the evaluator's own program". The label fails closed: a
`browser` call whose ledger line maul did not keep, or kept with its arguments dropped to fit, is labelled a page
script, since nothing shows it was not one.

Then, per outcome, the first rule that matches decides:

| Situation                                                                     | Result                                              | Reason               |
| ----------------------------------------------------------------------------- | --------------------------------------------------- | -------------------- |
| The round's ledger is not verified                                            | maul's trouble: the attempt is retried, then unsure | `infra`              |
| No preview: maul's own build of the head did not finish                       | unsure, and no evaluator runs                       | `tools-build-failed` |
| A manual outcome                                                              | unsure: only a person closes it, `/maul accept`     | `manual`             |
| The environment failed: a component crashed, was evicted or ran out of memory | unsure, never a fix                                 | `env-failed`         |
| maul's own tools or the cluster failed during the round                       | unsure, never a fix                                 | `infra`              |
| The report gives the outcome no verdict maul can read                         | unsure                                              | `not-reported`       |
| The evaluator reports it `unsure`                                             | unsure                                              | `evaluator-unsure`   |
| A `pass` or `fail` that cites no call that counts                             | unsure                                              | `no-valid-evidence`  |
| A `fail` that cites a call that counts                                        | **failed**: a verify-fix round                      | `seen-not-to-hold`   |
| A `pass` that cites a call that counts, on an outcome a finding holds         | unsure                                              | `held-finding`       |
| A `pass` that cites a call that counts, on an outcome an anomaly names        | unsure                                              | `contrary-note`      |
| A `pass` that cites a call that counts                                        | **passed**                                          | `seen-to-hold`       |

The calls kept for an outcome are the ones that count, in seq order, each once, at most 8 (the lowest seqs), recorded on
the Intent as `status.verification.outcomes[].cites` whatever the result. A round is ready only when every outcome
passed (or an approver accepted it at these heads) in a clean environment. maul never reads a verdict's note, the
report's body, or one outcome's verdict for another. Of the anomalies it reads only which outcome each names, if any
(`anomalies[].outcome`), and only to keep that outcome from passing ([Look-around](#look-around)).

**What maul does not check** is whether a cited call shows what the verdict says. A call that counts can still be about
something else, or read wrong: that is the evaluator's reading, and every surface that shows a result says so and shows
the calls, so a reviewer can judge it in a minute or two per outcome. See [Known limits](#known-limits) for what this
leaves open.

## Look-around

The outcomes are what the approver asked for; look-around is the rest of "does it look right". The evaluator visits the
routes the change touches and looks at each against a short list: text and links readable against their background,
nothing overlapping or clipped or blank, new controls clickable and doing something, new links loading, no console
errors or failed requests, and nothing in the logs or events (errors, panics, restarts, OOMs).

An **anomaly** must cite at least one call that counts, by the rules above, or it is dropped. It is a **note for the
reviewers**: the verification comment, the pull requests' sticky comments and the run panel show the evaluator's
description, marked as its words, beside the outcomes. It never holds a round, never downgrades the hand-off, never
keeps `maul/acceptance` from success and never starts a fix. The evaluator answers one question, whether the work is
correct, and the approved outcomes alone answer it: a round whose outcomes were all seen to hold is Passed with its
notes shown.

The one way an anomaly bears on a result is about an outcome, not the round: an anomaly that names an approved outcome
(its optional `anomalies[].outcome`, which the evaluator is told to add when what it noticed shows one not holding) is
the evaluator's own word against that outcome, so the outcome is not passed on that report, whatever its verdict says.
It is unsure (`contrary-note`), even when the anomaly cites nothing that counts; a `fail` that cites a call that counts
still fails. maul does not look for an outcome's id in an anomaly's words.

A defect in what an outcome promises is not an anomaly. The evaluator is told to report a defect in what an outcome
states, on any input, as that outcome's `fail`, and a break of the rule an outcome's line of the request states beyond
its example as that outcome's `unsure`. What no outcome promises (a regression elsewhere, an odd log line, a layout
blemish on a page the change only touched, a line of the issue the plan left uncovered, which the plan comment lists for
the approver) is for the reviewer to weigh: maul shows it and the human decides.

maul's own objects are not the application's. The slot capture that `maul-tool events` reads leaves out every event
about the tool pod and its Job (a readiness probe blip on the tool pod's `:8090/readyz`, for example), so the evaluator
never reads one as the application's trouble. An event about a component's Pod, or about any object maul did not render,
is kept.

## A finding holds its commits

What a round finds about an outcome is a fact about the commits it evaluated. An outcome seen not to hold **holds those
commits**: no later round at the same heads can pass it (`held-finding`), whatever its evaluator reports, until one of
these:

- the heads move (a fix, or a person's push);
- an approver accepts it at those heads (`/maul accept <id>`, or `/maul accept all`);
- the fix round handed that failure at those heads concludes, with its reason, that the code has no defect. It releases
  what it was handed, so a round after `/maul verify` can pass once what failed was the environment; it releases nothing
  once a later round there sees the failure again.

A later round at those heads that reports the outcome unsure, or gives it no verdict, keeps the hold on record, and one
that sees it not hold again makes a new finding. At unchanged code an evaluator that no longer sees a failure an earlier
one saw has not shown it is gone. `/maul verify --reset` does not end a hold: it changes the preview's data, not the
commits.

**A look-around anomaly holds nothing.** It is carried onto every later round at those heads as a note
(`status.verification.anomalies`, each with the `round` that found it), so the reviewers still see it, and the comment
names the round that found it; once the heads move it is gone.

## The fix round's reproduction

A verify-fix round gets a bug report maul writes from its own record of the round, never the evaluator's prompt, its
report's body, another outcome's note, its anomalies or its transcript. For each outcome seen not to hold:

- the approved outcome, quoted;
- the evaluator's one-line note on it, under "The evaluator's reading, not checked by maul";
- the evaluator's own calls before the first cited one, in order, at most the last 8, each with what it sent (the path
  that led there; their output is not shown);
- the cited calls that count, each with what it sent, its exit status and what came back (up to 8 KiB of output each, 48
  KiB per outcome), as maul's tools recorded them;
- each component's log lines from the first cited call's window, redacted, at most 40, starting at the first panic,
  fatal, error, exception or traceback line. The window is between the evaluator's last `maul-tool logs` read before the
  call and its first one after it, so there is an excerpt only when the evaluator read the logs after the call.

The fix agent is told the calls are the evaluator's reading, to reproduce them where it can, and to fix the behaviour
the outcome states, never the calls or their data. The next round is a new evaluation, whose evaluator reads the fix's
diff for special-casing of those calls. A diff of up to 32 KiB, with the names of any files it does not show in full, is
quoted whole. A longer one is cut to at most 28 KiB, at a commit, file or hunk boundary where it can be, keeping 4 KiB
for those names (with several repositories, each under its repository's), and the prompt says that the evaluator has not
seen the whole fix: how many bytes it shows of how many maul read, which files it does not show in full, which
repositories' diffs GitHub had already cut, and which could not be read at all (a compare that failed is said to be
unavailable, never left out). It tells the evaluator that finding no special-casing in what is shown proves nothing, and
to judge every outcome the fix may touch with data of its own.

## The hand-off

The hand-off is when people get the preview link and hear that the change is ready.

| Mode              | The link                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| ----------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `gated` (default) | Held until the evaluation decides. The status comment says "The preview is live, and maul holds its link until its evaluator has looked at the approved outcomes." It is then released as "Ready: maul's evaluator saw every approved outcome hold, citing calls maul recorded." once every outcome is seen to hold (or accepted by an approver), whatever notes the evaluator made, or as "maul can't call this ready: ..." with the reason |
| `early`           | Posted as soon as the preview runs the heads, labelled as still being looked at                                                                                                                                                                                                                                                                                                                                                              |

`gated` holds the link at most `handoffMax` (default 60 minutes, 10 minutes to 4 hours), counted from when CI first went
green at the first pull request heads; past it the link is released as "maul can't call this ready: it ran out of time".
**A human always wins:** an approver's changes-requested review or `/maul revise` or `/maul retry` on a pull request, a
push maul did not make, or `/maul preview` releases the link at once ("Shown while maul's evaluator is still looking at
the approved outcomes."), even before the first evaluation has started, and maul goes on evaluating. A plain comment,
`/maul verify` and `/maul verify fix` do not release it. The pull requests open as usual, not as drafts, so GitHub still
notifies their watchers; maul only holds its own words. A released link is labelled "Ready" only while the pull requests
are at the heads maul judged: after a push it reads as still being looked at until a round is about the new heads.

"maul can't call this ready" names one reason:

| Reason                                                      | Meaning                                                                                    |
| ----------------------------------------------------------- | ------------------------------------------------------------------------------------------ |
| some outcomes are unsure                                    | no verdict maul counts says they hold yet: read each outcome's line                        |
| an outcome fails and maul stopped fixing it                 | an outcome seen not to hold, and automatic fixing halted (out of fixes, a repeat, a human) |
| it used every round it may run                              | `maxVerifications` is spent                                                                |
| it spent its evaluation budget                              | the intent's evaluation ceiling is reached                                                 |
| it ran out of time                                          | `handoffMax` passed                                                                        |
| its sandbox was not enforced                                | the evaluator's strict probe found DNS, the API server, metadata or the internet open      |
| the preview failed during the evaluation                    | a component crashed, was evicted or ran out of memory during the round                     |
| its own evaluation environment failed during the evaluation | maul's tools or the cluster failed during the round, not the change                        |
| it could not run its checks                                 | maul's own machinery failed every attempt                                                  |

A look-around anomaly is never a reason: it is a note in the comment. When every outcome was seen to hold or accepted
and only a failing environment kept the round from ready, the commit status and the progress line name that cause
instead of reading as all seen to hold, for example "maul can't call this ready: 3/3 outcomes seen to hold, but the
preview failed during the evaluation".

"maul can't call this ready" is never a block. The intent stays in review, and an approver decides: accept the outcomes
maul's evaluator did not see hold with `/maul accept <id>` (or `/maul accept all`), ask for another round with
`/maul verify`, or merge. Another round at the same commits is a new evaluation, spends from the evaluation ceiling, and
does not clear what an earlier one found there (see [A finding holds its commits](#a-finding-holds-its-commits)).

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
    maxEvaluateCostMicroUSD: 6000000 # what the intent's evaluate runs may spend together
    evaluate: { maxCostMicroUSD: 1500000, maxTurns: 40 } # each evaluate run (limits.evaluate, like limits.build); fewer turns with less money, see below
```

`surfaces.browser.entry` must be an absolute path on the preview host (at most 256 visible ASCII characters, no space,
backslash, backtick or leading `//`, and no scheme or host); the evaluator's prompt names it as the page to open first.
A value that is not such a path is left out of the prompt.

The evaluation ceiling is counted apart from the intent's own `limits.maxCostMicroUSD`, so evaluation can never spend
what the fix rounds need; the chart's `maxEvaluateCostUSD` caps it for every Project. Under
`egressBroker.limits.unit: cost` each run's own cost budget is stamped on its pod as for every other stage: its stage's
`maxCostMicroUSD`, or the evaluation ceiling when that is unset, never more than the ceiling. Like every other stage,
`limits.evaluate` lowers a run's turns, tokens and `timeout` but never raises them past the controller's, and an
evaluate run budget over half the broker's hourly cost limit keeps the Project from Ready (`RunBudgetOverHourlyLimit`).

**Each request reserves its output bound.** Under the cost unit the broker admits a request only while what the pod
spent plus the request's whole output bound, at the model's output price, fits the pod's budget, so a run can spend at
most its budget less one such reservation. agent-runner therefore bounds what each evaluate request may write: 32000
output tokens (its largest response is a Write of its report or of a program, each under 64 KiB). On claude-sonnet-5-5
(\$10 per million output tokens) that reserves \$0.32 a request. A run budget under twice that keeps the Project from
Ready (`RunBudgetUnderRequestReserve`, naming the stage and the least budget it needs): on sonnet at least \$0.64, on
claude-opus-5-5 \$1.28, on claude-haiku-5-5 \$0.16. The example above (\$1.50) fits each. Without the bound a request
reserves the CLI's default (128000 tokens on the 5.5 models, \$1.28 on sonnet). A codex run gets no such bound and is
not judged: a request that sets no `max_output_tokens` reserves the broker's `--max-tokens-ceiling`, if any.

**Cost bounds the run; turns keep it its report.** Every tool call is one agent turn, so an evaluate run's turn cap
(`--intent-evaluate-max-turns`, chart `evaluateMaxTurns`, default 100; a Project's `limits.evaluate.maxTurns` only
lowers it) comes long before the round's 300 tool calls. A run that reaches it still reports: agent-runner gives the
run's first part all but its last few turns (a tenth, at least 6 and at most 12, never more than half: two repair rounds
of at least 3 turns each, enough to read the report, write it again and reply), counted in the CLI's own turns, and when
that part ends on the cap with no report it resumes the agent's own session on the kept turns, telling it to write the
report now from what it has: each outcome the verdict its calls already show, every outcome it did not see unsure. The
prompt states the run's turns and the turn by which to write the report, and asks for the report as soon as every
outcome has a verdict. The wall clock is kept back the same way: the first part runs on all but the last fifth of the
stage's `--intent-evaluate-timeout` (at least 4 and at most 10 minutes, never more than half; none for a stage under 8
minutes), so of the default 20 minutes it gets 16, which its prompt states, and when its clock stops it with no report
its session is resumed on the kept minutes, told its time ran out. Every `maul-tool` call prints the time left of the
run it is in (`[maul-tool time left]`, from `MAUL_TOOLS_DEADLINE`), and the prompt asks for the report by the time an
eighth of the clock is left. A run stopped by its cost budget gets no such turn (the broker refuses the pod any further
request), and one whose resumed session still writes no report ends `timeout`, saying why. So set the cost budget to
what a round may spend, and keep the turn cap just inside it, so the run stops on its turns, with its report, before its
money runs out: live, an evaluate run spent \$1.26-2.51 over its first 60 turns, 4-7 seconds each, a turn costing more
as the session grows, so 100 turns fit the default \$6 run budget and the 20 minutes. Lower the turns with a smaller
`limits.evaluate.maxCostMicroUSD` (about 40 turns for \$1.50).

## Reading the evidence

- **On GitHub.** The verification comment and each pull request's sticky comment say once what maul counts: "maul counts
  a verdict only when it cites calls the evaluator made in this evaluation through maul's tools; whether a cited call
  shows what the verdict says is for the reviewers to judge." Then they list every outcome once, with its kind (`change`
  or `keep`) and its result: "seen to hold by maul's evaluator", "seen not to hold by maul's evaluator", "unsure" with
  why (the reasons above, in words), or "accepted by a human, not re-checked", and the calls it cites that count ("it
  cites calls 4 and 6"). Under the outcome each cited call that counts is shown in one fenced, defanged block, as maul's
  tools recorded it: "Call 9, an http request:", what it sent, the exit status of a browser command or a program, and up
  to 1 KiB of its output, marked as the application's; a page script or program of the evaluator's own is headed "the
  evaluator's own page script" or "the evaluator's own program", "and what it returned", and a call whose output maul
  did not keep says "Its output was not kept by maul." A cited call that does not count (a `logs`, `events` or `files`
  read, a call at the base, a refused one, one of maul's own) is not listed in the comment at all; the status page shows
  every seq the evaluator cited to a reader with the transcripts tier. The evaluator's note on an outcome, of any
  result, is shown as "The evaluator's note on `<id>` (its words, not checked by maul)", and a look-around anomaly as
  "Look-around: maul's evaluator noticed (its words, not checked by maul)". A comment always fits GitHub's size limit:
  when the evidence would not, maul keeps it most important first (the cited calls in plan order, then the evaluator's
  notes, then the anomalies, then the component log tails), each piece whole while it still fits (a large one left out
  does not stop a smaller one after it), and the comment names what it left out. Every outcome's line and maul's own
  notes are always shown whole; only a plan whose statements alone leave no room has them shortened, and the comment
  says so. Results are shown only for the round that decided them, at the heads it evaluated: a round that settles
  without a judgement (its attempts all failed, or maul's own build of the head did not finish) lists every outcome as
  `unsure`, with no earlier round's results, and a push clears them until the new heads are judged: from the pass that
  sees the push, the status comment and the sticky comments read "maul is evaluating the new commits" with no result,
  and the last round's verification comment is left as it stood, about the heads it evaluated.
- **In the cluster.** Each round is a `PreviewVerification` in the release namespace
  (`kubectl get previewverifications -l maul.run/intent=<intent>`; maul keeps the newest three per intent). Its
  `status.ledger` indexes every tool call (its seq, kind, verb, exit, whether it was refused and, at an agents round's
  base, `side: base`); its `status.tools` names the round's tool pod. The full ledger is in the ConfigMap
  `<verification>-ledger` (`ledger.jsonl`, each call's request and full output, with sha256s) and the cited screenshots
  in `<verification>-evidence`, both deleted with the request. Each outcome's result, reason and counted calls are on
  the Intent, in `status.verification.outcomes`.
- **On the status page.** With the [intents dashboard](dashboard.md#the-evaluation-evidence) on, an evaluate run's panel
  shows the same round: outcomes with maul's decided result and the calls it counted for each, anomalies, the hand-off
  and the ledger. A reader with the transcripts tier also sees what each counted call sent and returned, which ones ran
  the evaluator's own page script or program, each call's request and output, the evaluator's own verdicts and notes,
  and the cited screenshots.
- **Trust order.** Read maul's result first, then the calls it counted: what each sent and what came back, and whether
  that shows the outcome as approved. That is the judgement maul leaves to you. The evaluator's note is its reading of
  those calls, and a call labelled as its own page script or program shows what the evaluator's code returned, not what
  a person sees. A ledger output is shown only where it hashes to the index, so an output marked not kept was cut to fit
  or is missing; treat it as unknown rather than as empty.
- **The runs.** The evaluator is an ordinary IntentRun of stage `evaluate`, metered like every run; its report is on the
  run's status.

Nothing of the evaluator reaches a build or fix agent beyond the reproduction above: not its prompt, its report's body,
its anomalies or its transcript.

## Without a preview: a CLI or a library

A Project with no preview is evaluated too when it says what to build: a command-line program (`surfaces.cli`) or a Go
library (`surfaces.library`). The evaluator is the same run, judged by the same rules; what changes is where its hands
are. intent-controller runs the round's tool pod itself, in the agents namespace, and that pod builds the pull request
**twice**, in the repository's accepted runner image: at its head, and at its **base**, the merge base of the head and
the repository's default branch (read from GitHub when the round starts, so a merge of the default branch into the pull
request moves it). Every call runs at the head; the evaluator may make the same call at the base (`--base`) to see how
the code behaved before the change, if that helps it judge, but a call at the base never counts.

It is off by default: chart `intentController.verification.agents.enabled` (intent-controller
`--intent-evaluator-agents`), beside the evaluator, with repository images on; previews need not be. Off, such a Project
carries `VerificationOff` (reason `AgentsToolPodNotEnabled`) and its plans are not asked for outcomes; switched off
under open intents, their views stay link-less and say "maul can't call this ready", never offering a preview. The
operator's side (the pod, its privileges, capacity and rollback) is in
[the agents tool pod](../configuration/intent-controller.md#the-agents-tool-pod).

```yaml
spec:
  repositories:
    - name: render
      url: https://github.com/acme/bbrender
  verification:
    enabled: true
  surfaces:
    cli:
      repository: render # optional when the Project has one repository
      build: [go, build, -o, /work/bin/bbrender, ./cmd/bbrender] # run at the tree's root; writes under /work/bin
      binary: /work/bin/bbrender # at most two names below /work/bin
      buildTimeout: 10m # each tree's build, 1m to 20m, never above the chart's buildTimeoutMax
    # or, in place of cli:
    # library: { repository: render, language: go }
```

- **One surface, no preview.** A Project has at most one of `cli` and `library`, never beside `surfaces.browser`, and
  never with `spec.preview` or a repository's `preview` (the API server refuses each; a Project that combines them
  anyway is `VerificationOff`, `SurfaceCombinationUnsupported`). One repository per tool pod: outcomes about another
  repository get no tools. `language` is `go` only.
- **The build.** `build` is 1 to 16 arguments, run in the tree's writable copy under the build's own
  [network profile](../configuration/intent-controller.md#network-profiles), and one of them must name `/work/bin` as a
  directory. A library's build is `go build ./...`. `maul check project` checks the surface, the build and the profile
  before anything runs (checks `surface`, `surface-build`, `surface-profile`).
- **The image.** Both sandboxes run the image the repository's latest build ran in (its accepted pin), never one the
  pull request declares. A repository with no accepted image is never evaluated: the round settles "maul can't call this
  ready: the repository has no accepted runner image to build the pull request in", never a block, and while
  `--repository-images` is off the Project shows `RepositoryImageRequired` from the start. An approver's `/maul verify`
  at commits an earlier round already judged changes nothing when the round cannot run (no accepted image, a tree that
  cannot be fetched, the evaluation budget spent): that round's result stands, on every surface, and the reply says the
  request did not run and why (for a missing image, as the only reply, never after an acknowledgement; for what is found
  only after the request was acknowledged, such as a tree that cannot be fetched, as a reply saying it was recorded but
  did not run after all and replaces the acknowledgement); send it again once that changes. Turning repository images
  off on intent-controller alone, as a rollback, also needs `MAUL_NETWORK_PROFILES_ENABLED: "false"` while the egress
  proxy is on, or intent-controller does not start
  ([how](../configuration/intent-controller.md#repository-declared-images)). Only `GOMODCACHE` and `GOPATH` from the
  image's own environment reach the programs (a network profile's own win); everything else in it, `GOFLAGS` or
  `GOPROXY` say, does not.

### What the evaluator can do there

The tool pod has no preview, so `http`, `browser`, `logs` and `events` are refused. Every call carries the tool pod's
per-evaluation key, which agent-runner stages where the model's own file tools cannot read it; a call without it is
refused and recorded.

| Command                                         | Does                                                                                                                                                                                                           |
| ----------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `maul-tool exec [--base] [--file f]... -- argv` | Runs a program in the head's sandbox (or the base's), in a fresh scratch directory of its own; `{{bin}}` (the built binary) and `{{tree}}` (the tree's writable copy, `/work/tree`) are substituted            |
| `maul-tool files ls`, `grep`, `read` `[--base]` | Looks at the tree as fetched, never as a build rewrote it: a listing, a literal-substring search (at most 200 hits) or the first 64 KiB of one file. Nothing reaches outside the tree. Never counts on its own |
| `maul-tool run <dir> [--base] [-- args]`        | Compiles the Go program in `<dir>` (at most 8 `.go` files at its top, no subdirectories) against the side's tree, then runs it: how a library is used, or a checker that reads what the binary wrote           |

At the head an `exec` or a `run` whose program ran counts whatever its exit status. An `exec` of the built binary alone
(its first argument `{{bin}}` or the binary's path, no files passed) is not labelled as the evaluator's own while
nothing of the evaluator's has run at that side; any other `exec`, and every `run`, is shown as the evaluator's own
program. `/work` is writable, so once an earlier `exec` labelled as its own, or a `run`, ran at a side (or an `exec`
whose line maul did not keep), every later `exec` of the built binary there is labelled the evaluator's own too: what it
ran may be what that call left (see [Known limits](#known-limits)). A refused `exec` or `run` counts here as one that
ran, since a refusal can come after the call was sent (the sandbox did not answer, or refused it part way). The
exceptions are a call without the per-evaluation key and one past `maxToolCalls`: the relay refuses those before sending
them anywhere (`ranNothing` in the ledger index), so they change no label.

A `run` program is compiled offline against the tree's own module graph: maul writes a minimal `go.mod` (module
`maulcheck`, the tree's `go` line) beside the program and makes a `go work` workspace of it and the tree, then builds
with no module proxy, the module files read-only and no user `go env` file, so the compile never reaches the network and
never changes the tree's `go.mod` or `go.sum`. A tree with a `vendor` directory, a `replace` by an absolute path or a
directory outside the tree, or no `go.mod` maul can read refuses every `run` by name. Each call gets a fresh scratch
directory, but `HOME` and the writable tree are shared by every call at that side while the evaluator explores. Nothing
is copied or put back between calls: the tree, the build output, `HOME`, the caches and the tmp root are that side's,
and each call sees what the build and the calls before it left there, so the evaluator reads a call's output knowing
what it ran before.

### Head and base

The pod builds both trees first, in parallel, each in its own sandbox, under its own user, with its own tree, caches and
socket: code at the head cannot touch the base's files, and nothing a program leaves behind survives into the next call
(the sandbox kills every process of its user before it answers). maul reaches each side only through a socket that
side's user serves: code that swaps its socket for a link to the other side's, or for a socket of another user, gets
maul's calls refused, never answered by the other side. Then:

- **A head that does not build** runs nothing more: the pod finishes there, before it is ready, so no evaluator is
  launched and none is paid for. CI built that head green, so it is maul's trouble, never the change's: every outcome is
  unsure ("its own build of the pull request did not finish"), and no fix round starts.
- **A base that does not build** (or whose build did not end in a failure maul can tell is the tree's) changes nothing
  but the base: every `exec --base` and `run --base` is then refused, and recorded; `files --base` still reads the
  base's source as fetched.
- **The base never decides a result.** Every call at the base is recorded with `side: base` in the round's ledger index,
  as context a reviewer can read beside the head's calls in the round's ledger.

### What changes for people

- **No link.** The hand-off holds maul's word, not a link: the status comment says maul is evaluating the approved
  outcomes "in its own build of the pull request" and holds its hand-off to the reviewers until it has. A review asking
  for changes, `/maul revise` or a push maul did not make hands it over at once; `/maul preview` does nothing, and says
  so.
- **Accepting an outcome.** `/maul accept <id>` binds only when maul had started a round at exactly those heads before
  the comment; otherwise maul replies that it was not evaluating those commits when the approver commented.
- **"maul can't call this ready"** has three more reasons: "its own build of the pull request did not finish" (the head
  did not build in the tool pod), "the repository has no accepted runner image to build the pull request in", and "it
  could not fetch the source of the pull request or of its base" (a tree did not become ready within 15 minutes, or
  GitHub has no such commit or does not let maul see it). The intent's `status.verification.unavailableReason` records
  the last as `TreeUnavailable` (the round is retried while the heads stay), the first two as `ToolsBuildFailed` and
  `SandboxImageUnavailable` beside the state `Unconfirmed`. A round whose every attempt ended with its tool pod's
  sandbox open or its tool pod lost (evicted, or past its deadline) is `Unavailable` and records `SandboxUnenforced` or
  `ToolsLost`, which the hand-off and the `maul/acceptance` status name. A failing environment there reads "its own
  tools failed during the evaluation", never a preview.
- **In the cluster.** The round is a `PreviewVerification` with `spec.placement: agents` and its `spec.workspace` (the
  repository, the head and base commits and their Repositories, the image, the network profile, the surface). Its status
  carries the builds (`status.ledger.builds`) and the ledger index, each base call marked `side: base`, and the full
  ledger is in `<verification>-ledger`; there are no screenshots. The two trees are Intent-owned Repositories pinned to
  exact commits (`spec.ref.commit`). The status page shows each call's side and the builds.

## codex: built, not verified

The evaluator has run live on claude, though the judging rule on this page has not yet been run against a live install.
A codex Project's intents are evaluated on codex only with `--intent-evaluate-codex` (chart
`intentController.verification.codexEvaluator: true`, needing the codex runner). Off, the default, such a Project is not
evaluated at all: its hand-off is `early`, and it carries the advisory condition `EvaluatorUnverified` (reason
`CodexEvaluatorOff`). On, the codex path runs as built and tested end to end against a scripted codex, but no live run
has proved it yet, screenshot viewing included: treat it as unverified until one has.

## Limits

| Bound                       | Default                                                            | Where                                                                                                                            |
| --------------------------- | ------------------------------------------------------------------ | -------------------------------------------------------------------------------------------------------------------------------- |
| One evaluate run            | 100 turns (the last few kept for its report), 20 minutes           | `--intent-evaluate-max-turns`, chart `evaluateMaxTurns`; `--intent-evaluate-timeout`, chart `evaluateTimeout`; `limits.evaluate` |
| Tool calls per round        | 300 (50 to 300)                                                    | `verification.maxToolCalls`, chart `maxToolCalls`                                                                                |
| Cited calls kept            | 8 per outcome, the lowest seqs that count                          | fixed                                                                                                                            |
| Look-around                 | 8 routes per round                                                 | fixed                                                                                                                            |
| Evaluation spend per intent | \$6                                                                | `limits.maxEvaluateCostMicroUSD`, chart `maxEvaluateCostUSD`                                                                     |
| Rounds and fixes            | as slice 1: `maxVerifications`, `maxVerifyFixes`                   | the Project's `limits`                                                                                                           |
| Hand-off wait               | 60 minutes                                                         | `verification.handoffMax`, chart `handoffMax`                                                                                    |
| Captured logs               | a bounded tail per component per round; 40 lines in a reproduction | fixed                                                                                                                            |
| Agents tool pods at once    | 2; at the bound a round waits, never a verdict                     | `--intent-max-tool-pods`, chart `verification.agents.maxToolPods`                                                                |
| One tree's build (agents)   | 10 minutes, 1 to 20                                                | `surfaces.cli.buildTimeout`, capped by chart `verification.agents.buildTimeoutMax`                                               |

A tool pod lives as long as its evaluate run plus five minutes, under the slot's verification deadline
(`preview.verification.deadlineMaxSeconds`), and needs about 2.5 CPU and 2 GiB beside the Preview: see
[the preview-controller](../configuration/preview-controller.md#the-agent-evaluators-tool-pod). An agents tool pod lives
as long as its evaluate run, plus one tree's build, plus five minutes, at most 75 minutes, and needs about 2.25 CPU and
4.25 GiB: see [the agents tool pod](../configuration/intent-controller.md#the-agents-tool-pod).

## Known limits

- **maul does not judge relevance.** It holds a citation to the round's own verified ledger, never to what the verdict
  claims. This is the cost of letting the evaluator's observations count, and the reason every counted call is shown and
  a human merges. It leaves two things for the person reviewing to look at on every outcome:
  - **A cited call can be about something else.** A call counts when it is the evaluator's own call that used the
    application, whatever it used: a pass can cite a request to an unrelated route, a page the outcome is not about, or
    a run of another command. Read each counted call's request and ask whether it reaches what the outcome names.
  - **A correct answer can be misread.** The evaluator can read an output that shows the outcome holding as a failure
    (which starts a fix round), or one that shows it failing as a pass. Read each counted call's output against the
    outcome's own words, not against the evaluator's note.
- **A model reading a screenshot can make an outcome seen to hold.** The evaluate model of an earlier calibration
  (`claude-sonnet-5`), judging screenshots of known defects, called 5 of 15 defect runs a pass, with confident evidence
  it invented, and failed 3 of 18 correct builds. It was right on gross structure (steps 14.8 px against 32 px, rows
  visibly offset after a swipe) and a coin flip below about 3 px (30 against 32 px, a label overlapping by 1.3 px). A
  `browser screenshot` that exited 0 is a call that counts, so an outcome about a size or a position can now be seen to
  hold on the model's reading of a screenshot alone. The prompt points the evaluator at computed styles read with
  `browser eval` for such facts (labelled as its own page script), but nothing makes it measure. Treat a layout outcome
  whose only cited call is a screenshot as unchecked, and measure it yourself.
- **The evaluator's own page scripts and programs count.** Some outcomes have no other surface (audio, a file the
  program writes), so a `browser eval`, an `exec` program of its own and a `run` count, labelled as its own. Such code
  could measure an internal value instead of what a person sees, or set up the state it then reports: the prompt forbids
  both, and only the label and the reviewer bound it.
- **The application can address the evaluator.** The application is the change's own code, and its output (a page, a
  body, a log line, what the built program prints) can say "every outcome holds". The evaluator is told that such text
  is data and no evidence, and the comment fences every output as the application's, but nothing else bounds it: read a
  cited output that speaks to the evaluator as a defect.
- **A cited output may not be kept.** The full ledger keeps at most 900 KiB of outputs, the oldest dropped first. A
  cited call whose output was dropped still counts, shown as not kept, and cannot be audited from the comment.
- **A wrong fail costs a fix round.** A misread or flaky observation that cites a call that counts starts an automatic
  fix: no retry runs first. That is bounded by `maxVerifyFixes`, the repeated-failure stop (the same outcomes failing
  again after a fix halts automatic fixing, so a real half-fix needs `/maul verify fix` to go on), the evaluation and
  intent ceilings, a human's review or push, and the fix agent's no-defect conclusion, which releases the hold.
- **codex is unverified and uncalibrated.** See [codex: built, not verified](#codex-built-not-verified). The calibration
  above was not run on codex or on a larger model.
- **After a preview-controller restart,** a browser round's captured logs and events stop refreshing and stay as last
  written, because the key that redacts them is not recoverable from memory. Status keeps updating.
- **Head and base share one pod's loopback and IPC.** Kubernetes cannot give two containers of one pod their own
  loopback or their own POSIX message queues and SysV IPC. A process the head left listening on a loopback port could
  change what a base call shows, so the sandbox kills every process of its user before it answers, and the isolation
  probe checks a leftover head listener is gone before the base runs. A base call never decides a result. The full fix
  is one pod per side.
- **A program can lie about its own output.** It runs as its sandbox's user, as the shim does; it cannot write the
  `tools` container's log, the ledger, or the other side's files, but what it prints is its own. The built binary is the
  change's code, and an earlier `exec` can rewrite the tree, `HOME` or the build output that later calls at that side
  read: after one, a later run of the built binary at that side is labelled as the evaluator's own program.
- **The evaluation ceiling counts toward the hourly limit.** With the evaluator on, the default \$6 ceiling counts
  toward `RunBudgetOverHourlyLimit`: a broker `costPerHour` below \$12 keeps such a Project from Ready.

## Migrating from slice 1

Slice 1 verified the plan's acceptance **criteria** (scripted HTTP steps) from a tester Job in the preview's slot. It is
retired: the agent evaluator is the only way maul verifies an intent, and its switch,
`intentController.verification.evaluator`, is verification's only switch. What changed for the operator:

| Before                                                                 | Now                                                                                                                                                                                                                                 |
| ---------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Chart `preview.verification.enabled` turned verification on            | Removed. Set at all, `true` or `false`, the render fails naming `intentController.verification.evaluator`. Drop the key: with the evaluator on, verification runs in the preview slots whenever `previewController.enabled` is true |
| intent-controller `--intent-verification` (`MAUL_INTENT_VERIFICATION`) | Removed. Set to true, intent-controller refuses to start with a message naming this section; `--intent-evaluator` turns verification on                                                                                             |
| preview-controller `--preview-tester-image` alone ran criteria         | Removed with the tester (the `preview-tester` binary and image are no longer built). Set to anything, preview-controller refuses to start with a message naming this section                                                        |
| Chart `preview.verification.testerImage`, which the release stamped    | Removed. Set at all, even empty, the render fails naming this section. Drop the key, and any step of your own that stamps it                                                                                                        |
| Evaluator off meant slice 1's criteria                                 | Evaluator off means verification is off: a verifying Project carries `VerificationOff` (`EvaluatorOff`), its intents are not verified, and their status comments say so and link the preview at once                                |

To move a slice-1 install: drop `preview.verification.enabled` and `preview.verification.testerImage` from the chart
values (and `--intent-verification` and `--preview-tester-image` from any kustomize or hand-written configuration), set
`intentController.verification.evaluator: true` with the three `preview.verification.tools` images (the release stamps
them), and upgrade. A plan approved before the evaluator states criteria, which the evaluator does not run, and this
release no longer starts while such an intent is open under the evaluator (the census below): before upgrading, on the
release you run, `/maul cancel` each open intent whose approved plan states criteria (a new issue starts over with
outcomes) and delete each PreviewVerification that asks criteria. An intent whose Project comes to be verified only
later, after the upgrade and while intent-controller runs, stops for a replan instead (its `maul/acceptance` the error
"the approved plan states criteria, not outcomes: nothing is verified"); a change that verifies it through a restart is
refused by the census (below). With the evaluator left off, nothing is verified.

**The criteria fields are removed.** This release removes slice 1's fields from the CRDs (a PreviewVerification's
`spec.criteria` and `criteriaDigest`, its tester observations, and an Intent's `status.acceptance.criteria` and
`status.verification.failing`), so the API server prunes them from every stored object, and a PreviewVerification must
now state outcomes. Two checks stop an upgrade that would leave records nothing can verify:

- **The chart's pre-upgrade guard.** A render that reaches the cluster (`helm upgrade`, `helm install`, or
  `helm template --dry-run=server`) fails while any PreviewVerification in the release namespace asks criteria (or,
  pruned, states no outcomes), naming each one. Its spec is immutable, so it cannot be migrated: delete it
  (`kubectl -n <namespace> delete previewverification <name>`) and upgrade again. A render without a cluster (plain
  `helm template`, Argo CD, Flux) cannot look, and the census below catches the same records instead.
- **The startup census.** Before it starts its controllers, intent-controller counts what slice 1 left in its namespace,
  reading only: every open (non-terminal) intent, of a Project the evaluator verifies, whose acceptance on record is of
  slice 1's era (a criteria digest and no outcomes digest, which is all pruning leaves of its criteria), and every
  PreviewVerification stating no outcomes. It reports the count as the `maul.intent.slice1_records` gauge (`kind`
  `intent` or `previewverification`) and, while it counts any, **refuses to start**, naming each record and its fix.
  intent-controller is not running then, so the fix cannot be a `/maul` command: delete the intent
  (`kubectl -n <namespace> delete intent <name>`; neither kind holds a finalizer of intent-controller's, so the delete
  completes without it), or go back to how intent-controller last ran (the previous release, or without the evaluator
  verifying the intent's Project), `/maul cancel` the intent there (a new issue starts over with outcomes) and try
  again. A census that cannot list refuses too. Without the evaluator its role may not list PreviewVerifications, and
  the line says `previewverifications_unlisted`.

A closed or merged intent's record is never judged again and is not counted: it keeps working (its status and finalizers
still update) with its criteria pruned. Nor is an open intent of a Project the evaluator does not verify: a plan stating
criteria there is recorded by its digest alone, which nothing runs and the pruning does not change. Should the evaluator
come to verify that Project while intent-controller runs (the Project's `verification` turned on), the intent stops for
a replan. Should it come to after a restart (turning the evaluator or previews on), the census counts the intent and
refuses to start: `/maul cancel` such intents first, before the change.

**Upgrade runbook.** Before upgrading from 0.13: list the PreviewVerifications that still ask criteria and delete them
(`kubectl -n <namespace> get previewverifications -o json | jq -r '.items[] | select(.spec.criteria) | .metadata.name'`),
and cancel (or delete) each open intent whose approved plan states criteria. The chart's guard and the census name
anything left.

## Migrating from saved checks

Earlier releases trusted an outcome only through a check: the evaluator wrote checks and maul re-ran them, a second
model (the check reviewer) said whether each proved its outcome, maul stored and replayed the proven ones without a
model (the replay rounds and their cost ladder), a failing check could be disputed, a check that showed a defect was a
suspected defect for a person to confirm, a check that passed only on its retry was quarantined, and on a Project with
no preview a check counted only if it also told the head from the base. All of that is gone. The evaluator writes no
check: each verdict counts when it cites calls of the round's own verified ledger
([What makes a pass](#what-makes-a-pass)), and the base is for exploration only.

**An install must pass through 0.14.18.** 0.14.18 retired saved checks and settled what they had left: on its first pass
over each open intent whose verification an earlier release had judged (no `status.verification.judge`), it withdrew
that result once and started one fresh evaluation, and it stopped every checkreview run still running, its Job deleted.
0.14.19 removes the retired fields and that withdrawal with them, so it cannot read such a record. Coming from a release
before 0.14.18, upgrade to 0.14.18 first, let intent-controller run until it has settled every open intent, then upgrade
to 0.14.19.

**The guard and the census.** A `helm upgrade` (or `helm template --dry-run=server`) of 0.14.19 fails before any CRD
changes, and intent-controller refuses to start (reporting `maul.intent.check_era_records`), while any of these is left,
each named with its fix:

- an open Intent whose verification has no judge marker (`status.verification.judge: observed/1`, which every
  verification 0.14.18 or later writes carries) and still holds what an earlier release judged (a round in flight,
  outcome results, a pass or fail count, anomalies). Run 0.14.18 until it withdraws it. 0.14.18 does so only while the
  evaluator verifies the intent's Project and the plan has an outcome maul evaluates; where it cannot, `/maul cancel`
  the intent under 0.14.18, or delete it.
- an open Intent 0.14.18 withdrew whose fresh evaluation has not started yet (the `VerificationSuperseded` condition, no
  judge marker; its status comment adds "An earlier result was withdrawn because maul's rules for judging the approved
  outcomes changed; it is not shown."). Run 0.14.18 until that round starts, or cancel or delete the intent.
- an unfinished checkreview IntentRun. Run 0.14.18, which stops it, or delete it.

Neither counts an intent that has ended (its result stands as the earlier rules reached it, and its comments keep that
release's words), an intent 0.14.18 superseded (marked, even while its fresh round runs and its comments still say the
earlier result was withdrawn), or a finished checkreview run (its spend still counts toward its intent's evaluation
ceiling). A fresh install has nothing to count. A render without a cluster (`helm template`, Argo CD, Flux) sees
nothing, and the census refuses to start instead.

**What 0.14.19 removes.**

- The CRD fields the saved checks wrote, which nothing has read since 0.14.18: an Intent's `status.acceptance.checks`,
  `reviews`, `redCleared` and `narrowed`; its `status.verification.quarantined`, `quarantinedIDs`, `suspected`,
  `disputed`, `rung`, `modelRound` and `escalation`, an outcome result's `basis`, `line` and `objected`, and a request's
  `explore`; a `PreviewVerification`'s `spec.checks`, `spec.checksDigest`, `spec.rung` and `status.replay`,
  `confirmations`, `confirmed` and `base`; an IntentRun's `spec.evaluation.rung`; a Project's `spec.models.checkReview`
  and `spec.limits.checkReview`. The API server prunes them from every stored object.
- The spellings a stored object may still carry stay in the schema, so it keeps validating on its next write: the stage
  `checkreview` (no run of it is created), the outcome result `quarantined`, the verification reasons
  `replay-incomplete`, `kept-copy-failed`, `stale-criteria` and `tester-output-invalid`, and the part role `tester`.
- intent-controller's `--intent-checkreview-model` and `--intent-checkreview-max-turns` are no longer flags: drop them.
  The chart's `intentController.verification.checkReviewModel` and `checkReviewMaxTurns` are no longer in its values
  schema, so either one set fails the render; an upgrade with `helm upgrade --reuse-values` from a release that still
  had them fails too: upgrade with your values files, or with `--reset-then-reuse-values`.
- A Project manifest, or a maul-config values file, that still states `spec.models.checkReview` or
  `spec.limits.checkReview`: maul-config's schema refuses it, and the API server prunes the field with a warning, or
  refuses it under strict field validation (`kubectl apply --validate=strict`, a server-side apply). Remove both before
  the upgrade.
- `/maul verify explore` stays a plain `/maul verify`.

**The leftover check stores.** Each intent's saved checks are in immutable ConfigMaps `<intent>-checks-<n>` it owns.
Nothing reads them, and intent-controller cannot delete them: they go with their Intent. To remove them sooner, match
the name against each ConfigMap's own intent label, since other ConfigMaps of an intent carry that label too:

```sh
kubectl -n <namespace> get configmaps -l maul.run/intent \
  -o jsonpath='{range .items[*]}{.metadata.name} {.metadata.labels.maul\.run/intent}{"\n"}{end}' \
  | awk '$1 ~ ("^" $2 "-checks-[0-9]+$") {print "configmap/" $1}' \
  | xargs -r kubectl -n <namespace> delete
```

**Rolling back.** `helm rollback` to 0.14.18 re-applies its CRDs. Every object 0.14.19 wrote still takes 0.14.18's
writes: a verification or evaluate run it created names no rung, which 0.14.18's schema requires, but its spec is
immutable and unchanged, so the API server's validation ratcheting (Kubernetes 1.30 and later) admits it. The pruned
fields do not come back; 0.14.18 reads none of them.
