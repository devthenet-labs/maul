# Acceptance verification

Before maul hands an intent's pull requests to a human, it can have an agent evaluator use the combined change and
report, per approved outcome, whether it saw the change do what the plan said, fixing what it can within a few bounded
rounds. Approving the plan fixes the outcomes the evaluator is asked about, byte for byte, and it uses the change in the
cluster, against the intent's own [preview](../configuration/preview-controller.md), at exactly the pull request heads
under review. maul counts a verdict only when it cites calls maul recorded itself; whether those calls show what the
verdict says is the evaluator's reading, and the reviewers' to judge.

What the plan states depends on one chart switch, `intentController.verification.evaluator`:

- **With the agent evaluator** (on): the plan states **acceptance outcomes** in plain words, and nothing about how to
  check them. An [agent evaluator](evaluator.md) uses the live preview through maul's tools and reports, per outcome,
  what it saw, and maul counts a verdict only when it cites calls of the round that maul recorded itself. By default the
  preview link reaches people only once every outcome is seen to hold, or once maul gives up and says so.
- **Without it** (off, the default): nothing is verified. A Project that asks for verification carries `VerificationOff`
  (reason `EvaluatorOff`), and each intent's status comment says so and links its preview at once. Slice 1's
  **acceptance criteria**, HTTP steps a tester Job ran against the preview, are gone with the tester: see
  [Migrating from slice 1](evaluator.md#migrating-from-slice-1).

Verification is off by default, and it never blocks anything. A failure is a red `maul/acceptance` commit status and a
`VerifyFailing` condition: the intent stays in review, and a human's review, command or push always wins.

!!! note "What a passing status is worth"

    `maul/acceptance` is behavioural evidence, not an attestation. The pull request's author controls the code under
    test, and maul evaluates only what the approved plan states. Treat it as "maul's evaluator saw the change do what
    the plan said, in the calls shown", and review the code as you always did.

## Turning it on

Each switch fails closed: with one off, no acceptance state is written anywhere.

1. **The evaluator.** `intentController.verification.evaluator: true`, verification's one switch, with the three tool
   pod images in `preview.verification.tools` pinned by digest, switches intent-controller (`--intent-evaluator`) and,
   with `previewController.enabled`, preview-controller (`--preview-evaluator`) on, and renders the evaluator's
   admission and network policies and, in the slots, the request channel, the tool pod's admission rules, the per-part
   NetworkPolicy and Job policies, the slot quota for them, and the grants in
   [preview-controller](../configuration/preview-controller.md#acceptance-verification-off-by-default).
   `preview.verification.deadlineMaxSeconds` (60 to 3600, default 1800) bounds a slot Job and also sets both
   controllers' deadline flags, so no request can ask for a Job the slot policy refuses. Without previews
   (`--intent-previews-enabled` off) the evaluator runs only with its agents tool pod (below), and every other verifying
   Project carries the condition `VerificationOff` (reason `PreviewsDisabled`), as each of its intents in review says in
   its status comment. The evaluator needs Kubernetes 1.30+ and `agent.networkPolicy.create`, and is refused under
   `agent.networkPolicy.mode: istio`. The former switches, chart `preview.verification.enabled` and intent-controller
   `--intent-verification`, are removed and refused, as are the deleted tester's image, chart
   `preview.verification.testerImage` and preview-controller `--preview-tester-image`: see
   [Migrating from slice 1](evaluator.md#migrating-from-slice-1). Its chart settings are in
   [the chart's README](https://github.com/devthenet-labs/maul/blob/main/charts/maul/README.md); the tool pod's side is
   in [the preview-controller](../configuration/preview-controller.md#the-agent-evaluators-tool-pod). With
   `intentController.verification.agents.enabled` as well, the evaluator also checks Projects with no preview that
   declare a command-line program or a Go library, previews on or off: see
   [Without a preview](evaluator.md#without-a-preview-a-cli-or-a-library).
2. **The Project**, per Project:

```yaml
spec:
  verification:
    enabled: true
    manualGatesStatus: false # no effect: an open manual item always holds the status pending
    postLogs: false # default: component log tails go on the issue only, never on a pull request
    timeout: 10m # no effect: it was the deleted tester Job's wall clock
    handoff: gated # evaluator only: gated or early; unset follows the chart's defaultHandoff
  limits:
    maxVerifyFixes: 2 # 0..10, automatic fix rounds per approved plan revision
    maxVerifications: 20 # 1..100, automatic verifications per approved plan revision
```

The evaluator's other Project settings (`surfaces.browser`, `verification.handoffMax`, `verification.maxToolCalls`,
`limits.evaluate`, `limits.maxEvaluateCostMicroUSD`) are on [its page](evaluator.md#the-hand-off). While verification is
enabled, no repository of the Project may be named `tester` or `tools`: those name the verification's parts of the
preview slot.

The App needs **Commit statuses: Read & write** on each application repository to post `maul/acceptance`: see
[Create the GitHub App](../getting-started/github-app.md#intents). An App made by `maul setup github-app --intents` has
it. It is never part of `Ready`: a verifying Project whose App lacks it stays `Ready`, verifies and comments as usual,
posts no status, and shows the condition `AcceptanceStatusUnavailable` (naming the repository), as do the verification
comments. To fix it, set Commit statuses to Read and write in the App's settings, then accept the change on the
installation.

## What a plan declares: outcomes

With the evaluator on, the plan agent writes an optional `acceptance` list of at most 20 **outcomes** in its report's
frontmatter: each one thing the change must make true, or must leave true, in words a person could check by using the
running application. It writes no steps, requests or checks; the evaluator decides how to check each one.

```yaml
acceptance:
  - id: rating-moves # ^[a-z][a-z0-9-]{0,31}$, unique, never all (/maul accept reserves it)
    # one line, at most 500 characters; about 200 is the aim
    outcome: "After a battle of two players you create, /api/players rates the winner above 1000, the loser below."
    from: "ratings should update after every battle" # the issue line it answers, copied exactly, at most 300
    kind: change # the default: what the change makes true
  - id: winner-listed-first
    outcome: "On /leaderboard, the winner of a battle between two players you create is listed above the loser."
    from: "ratings should update after every battle" # several outcomes may answer one line
  - id: new-player-listed
    outcome: "A player you add on the players page (/players) is listed there by the name you gave."
    kind: keep # what the change must not break
  - id: sounds-good
    outcome: "The new drum kit sounds good."
    from: "make the drums punchier"
    manual: true # only a person closes it, with /maul accept
    reason: "only a person can judge how it sounds"
```

The evaluator tries every outcome again in each later round, on a preview whose data persists and is shared by every
round (nothing resets it between verifications, so the evaluator creates its own data, named with a token maul makes
unique per evaluation, and judges by that), so the planner is asked to word each outcome so that the evaluator can show
it with data of its own:

- about an item the person checking creates, never about data already there: no "every", "each", "all", "other",
  "existing" or "saved before", and no global rank, count, or first or last row. A request line that says "each X" gets
  an outcome about an X the person creates, two of them where the outcome compares items;
- one claim per outcome: "X and Y", "on the page and in the API" and "before and after" are separate outcomes, which may
  share one `from` line;
- concrete values, other than the defaults, and what a person then sees ("a beat you save at 90 BPM shows 90 bpm");
- where the issue states a rule (what holds for any input of a kind, a bound, a "never" or an "always") and gives
  examples of it, the rule, with an example as one instance ("a beat you save at any tempo from 60 to 200 BPM shows that
  tempo, for example 90 bpm"), quoting in `from` the line that states the rule. The evaluator judges the outcome by that
  rule and tries it beyond the example. The rule is stated as the issue states it and no wider: an issue that gives only
  an example gets an outcome about that example. An outcome whose `from` line states a rule states that rule, the line's
  own example (a README's, a documented value) folded in as "for example", never an outcome of the example alone;
- where the issue promises that something is carried over (a copy, fields kept, the rest unchanged, a round-trip through
  save and load), that promise as an outcome of its own over all of what is promised, set, zero, empty or left absent:
  for "a copy", every field but those the issue says change; where the issue promises no copy and names the fields kept,
  only those; a copy that is a new item beside the original keeps its own id and creation time unless the issue says
  otherwise;
- a `keep` outcome names one action and what a person then sees, never "X still works"; where its line states a rule
  (unchanged for any input), it states that rule with the action as one instance;
- a layout outcome (a size, an alignment, no sideways scroll) needs no data: it names one element of the fixed page and
  states the numbers, an exact size or position with a tolerance ("48 by 48 px, within 1 px") or the bound the request
  gives ("at least 48 by 48 px"), at the browser's fixed 1280x800 window;
- no colour: an outcome states a visible state a person can read, or is manual;
- an outcome only data the person checking cannot create can show (rows the application seeds) is manual;
- a list the application cuts to its first rows (a top 10, the newest 20) holds every round's items, so the person's own
  item need not be in it: an outcome about such a list, `keep` ones too, is stated through what the person sees of their
  own item (its own page or API path, a filter or search narrowed to it, what an action on it changes). One about the
  list itself (its order, its cut, which items it holds) that no such filter narrows to the person's own items, and one
  the request promises nothing a person can see that way, is manual. Read alone, the cut list could look right on a
  wrong build, and the person's own item there would go missing whenever other rounds' items crowd it out;
- where a person sees it, a page's route or an API path, in the application's own words, with the API's name in brackets
  where it differs ("a beat (a pattern, /api/patterns)").

The plan comment lists the outcomes, each with the issue line it answers, then the issue's own list items that no
outcome quotes in `from` (ignoring case, whitespace, trailing punctuation and inline markup such as code spans, emphasis
and link targets; a quote shortened with "..." or "…" counts when its pieces, each at least 4 letters or digits and 12
together, appear in the line in order): "No outcome answers these lines of the issue, so nothing checks them." That not
covered list is the approver's prompt to ask for a new plan before approving, if one of those lines should be checked.
It is advice for the approver only, which never refuses a plan and never decides a result. An outcome worded about rows
the evaluator cannot create ("every existing beat") is one it reports unsure, saying in its note that the outcome needs
wording about data of its own: read the outcomes for that before approving.

A plan that states slice 1's acceptance criteria instead is refused while the evaluator is on, and the planner's retry
is told why. An intent whose approved plan still carries criteria when the evaluator is switched on is no longer
verified: maul posts a notice once. The approved plan cannot change while its pull requests are open (a replan from
review is not available yet: `/maul replan` there is answered as not available), so the pull requests are reviewed as
they are, or an approver comments `/maul cancel` and opens a new issue with the trigger label, which maul plans with
outcomes. maul never turns old criteria into checks.

## What maul does with outcomes

The [evaluator's page](evaluator.md) has the whole round. In short:

1. **Plan approved.** On the first review pass maul reads the approved plan back under its digest and records the
   outcomes on the Intent's status.
2. **The gate.** Every open pull request's named checks green at its head, no check-fix round due, no human review or
   command pending, the preview serving exactly the pull request heads, and the bounds below.
3. **A round.** The evaluator uses the preview through its tool pod, every call recorded in maul's ledger, and reports a
   verdict per outcome (`pass`, `fail` or `unsure`) citing the calls that showed it. An outcome is **seen to hold** only
   on a `pass` that cites a call maul counts: the evaluator's own unrefused call of this round that used the application
   ([What makes a pass](evaluator.md#what-makes-a-pass)).
4. **A failure**, a `fail` that cites such a call, starts a verify-fix round, handed a reproduction maul writes from its
   own record: the outcome, the evaluator's note on it labelled as its reading, the calls it cites with what each sent
   and what came back, the calls that led there, and the components' redacted log lines from that moment.
5. **The hand-off.** Under `gated` (the default) the preview link is held until every outcome is seen to hold ("Ready:
   maul's evaluator saw every approved outcome hold, citing calls maul recorded.") or maul gives up ("maul can't call
   this ready", with the reason), at most `handoffMax` (60 minutes) from CI first going green. A human's review, command
   or push, or `/maul preview`, releases it at once.

A later round that still fails at the heads a fix round that found no defect judged ends automatic fixing as
`Exhausted`: maul and the fix agent disagree, and a human decides. An approver's `/maul verify fix` made after that
conclusion is such a decision: the round it runs, if it still fails, starts a fix round (within `maxVerifyFixes`).

### A fix that finds no defect

A verification can fail for a reason outside the code: the preview was not ready, its network was not open yet, or data
from an earlier round got in the way, or the evaluator misread what a call showed. The verify-fix agent is told to
reproduce each failing outcome against the code first: read what the cited calls exercise, run the tests that cover it
and reproduce its requests where it can. If it concludes, with evidence, that the code already does what the outcomes
ask, it changes nothing and says so explicitly in its report (`no_defect: true`, with a one-line reason). The round then
completes as `no_defect`: nothing is pushed, it is not retried, it still counts toward `maxVerifyFixes`, and automatic
fixing halts with reason `FixFoundNoDefect`. The verification comment shows the agent's reason and says the verdict may
be wrong: `/maul verify` checks again. The conclusion is shown whatever else halted automatic fixing, while the pull
requests stay at the heads it judged, and until the next fix round. A round that changes nothing without that explicit
conclusion is still a failed round, retried like any other.

### Before the first call: the tool pod waits for the slot network

The tool pod is a new Pod on every round, and the slot's NetworkPolicy admits it only once the cluster's policy
controller has programmed the policy for that Pod. On k3s and k3d, kube-router does that about a second after the Pod
starts and rejects its connections until then, which looks exactly like an application that is not listening. So before
it serves its first call the tool pod's relay waits for every component's Service to accept a TCP connection (it sends
nothing), at the Service's ClusterIP, which preview-controller reads and hands it: the relay resolves no name, so
cluster DNS, which every Pod on the node shares, cannot keep it from a component. It retries from 100 ms backing off to
1 s, for at most a minute and never more than a quarter of the round's wall clock, and records the wait per component.

Once every component is reached, the evaluator's calls are served, and a connection error during one is the
application's failure. The window absorbs the policy being programmed; a component still unreachable at its end is the
change's failure too, reason `component-unreachable`. Readiness alone does not prove the tool pod can connect: an
application can pass the kubelet's probe while refusing every other source, so maul never reports such a round as "could
not run the checks". A component Service with no ClusterIP is maul's render error, `service-no-cluster-ip`, before any
tool pod starts.

What happens to a component while the tool pod runs is attributed by its cause. Only Pods of the component's current
revision count (a previous revision's Pods are ignored), and only events between the tool pod's start (less two seconds
of clock skew) and its end. The change is charged only on unambiguous evidence:

| What happened                                                                                                                                                                  | Reason                  | Whose                 |
| ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | ----------------------- | --------------------- |
| Its process exited non-zero or was OOM-killed                                                                                                                                  | `component-crashed`     | The change's (counts) |
| The kubelet evicted its Pod (`status.reason: Evicted`: memory or ephemeral storage pressure)                                                                                   | `component-evicted`     | The change's (counts) |
| It was Ready, nothing above happened, and it accepted no connection from the tool pod for the whole readiness window                                                           | `component-unreachable` | The change's (counts) |
| Anything else: not Ready with no such evidence (a node going NotReady), node loss, a drain, preemption, a deletion, a graceful node shutdown, a clean exit, a bare replacement | `component-disrupted`   | maul's (retried)      |

The application holds no API credential, so it cannot drain, delete or preempt anything. The most it can do without
crashing is make itself not-Ready, which is reported as `component-disrupted`, retried and then `Unavailable`, never a
pass, while its preview goes not-live and shows the error. A restart or eviction from before the tool pod started, or
after it ended, is not counted.

## Rounds, fixes and bounds

### Bounds

| Bound                     | Rule                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| ------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `limits.maxVerifyFixes`   | Automatic fix rounds per approved plan revision (default 2). Then auto-fix halts, reason `MaxVerifyFixes`. A round counts once an agent ran in it, whatever its later attempts did, an agent that crashed or ran out of time included; only a round no agent could have run in (cancelled before its Job, refused, no image or node to run on) does not.                                                                                                                             |
| `limits.maxVerifications` | Automatic verifications per plan revision (default 20). `/maul verify` bypasses the interval, not this cap.                                                                                                                                                                                                                                                                                                                                                                          |
| Repeated failure          | A verification in which everything the last fix round was for still fails exactly as it did halts auto-fix: `RepeatedFailure`. That round is the latest one an agent ran in, whether it pushed, found no defect or failed; rounds at or before a `/maul verify fix` are never compared. With the evaluator maul compares the failing outcomes alone (each outcome as approved, never the calls the evaluator chose to cite), so the same outcomes failing again after a fix halt it. |
| Cost ceiling              | At the intent's cost ceiling auto-fix, retries included, halts: `CostCeiling`. It never blocks the intent.                                                                                                                                                                                                                                                                                                                                                                           |
| Spacing                   | While auto-fix is halted, an automatic re-verification waits five minutes; one `/maul verify` per five minutes.                                                                                                                                                                                                                                                                                                                                                                      |
| Infrastructure            | A tool pod that could not run is retried up to three attempts, uncounted, then reported `Unavailable`.                                                                                                                                                                                                                                                                                                                                                                               |
| Preview wait              | A due verification waits at most an hour for the preview to be live at the heads, then reports `Unavailable`.                                                                                                                                                                                                                                                                                                                                                                        |
| Evaluation ceiling        | With the evaluator: what the intent's evaluate runs may spend together (`limits.maxEvaluateCostMicroUSD`, default \$6), counted apart from the cost ceiling above. Past it no evaluation runs, and a gated hand-off is released as "maul can't call this ready".                                                                                                                                                                                                                     |
| Hand-off wait             | With the evaluator and a gated hand-off: `verification.handoffMax` (default 60 minutes) from CI first going green. Past it the link is released as "maul can't call this ready: it ran out of time"; verification itself goes on.                                                                                                                                                                                                                                                    |

### What halts automatic fixing

Automatic fixing halts, with the reason shown on the issue and the status, for exactly these reasons. A halt never
blocks the intent, verification still runs and reports, and only `/maul verify fix` (or a new approved plan) re-arms it:

| Reason             | Halts when                                                                                         |
| ------------------ | -------------------------------------------------------------------------------------------------- |
| `HumanStop`        | an approver comments `/maul verify stop`: the only command that halts it without starting anything |
| `HumanReview`      | an approver's changes-requested review starts a round                                              |
| `HumanCommand`     | an approver's `/maul revise` or `/maul retry` starts a round                                       |
| `HumanPush`        | a head maul did not push                                                                           |
| `FixFoundNoDefect` | a verify-fix round's agent concluded that the code has no defect (above)                           |
| `MaxVerifyFixes`   | `limits.maxVerifyFixes` is spent                                                                   |
| `RepeatedFailure`  | what the last fix round was for, pushed or not, still fails exactly as it did                      |
| `CostCeiling`      | the intent reached its cost ceiling                                                                |
| `CriterionError`   | the approved criteria cannot be read                                                               |
| `NoOpenPR`         | no failing outcome's repository has an open pull request to fix                                    |

`/maul verify stop` cancels a verify-fix round that is queued but has not started, and lets one that is already running
finish and push, as a human round does; nothing runs in its place.

### The human wins

Auto-fix halts the moment a human acts:

- **`HumanReview`, `HumanCommand`:** an approver's changes-requested review or `/maul revise` runs first, before any
  automatic fix, even when a fix is also due or a review is still in its quiet window. If an automatic fix is queued but
  has not started, it is **cancelled for your review** and your round runs next. One that is already running finishes
  and pushes, then your round runs next, and its text notes the automatic fix.
- **`HumanPush`:** a head maul did not push halts auto-fix. Verification still re-runs and reports; it never fixes there
  unless an approver re-armed auto-fix after that push (below).
- `/maul verify fix` re-arms auto-fix. It does not reset the fix count, but the next failure is never judged a repeat of
  a fix round from before it. To push a change of your own and let maul go on fixing, push first, then comment
  `/maul verify fix`: maul notices a push only at its next poll, a minute or more later, and a re-arm posted after
  GitHub recorded the push (by GitHub's own clock, the comment's time against the push's in the repository's activity)
  keeps auto-fix armed for it, and a fix round may then build on top of your commit. A push GitHub recorded after the
  re-arm, in the same second, or at a time GitHub will not show, still halts it.
- **The preview link.** With the evaluator and a gated hand-off, an approver's changes-requested review or
  `/maul revise` or `/maul retry` on a pull request, a push maul did not make, or `/maul preview` releases a held
  preview link at once, labelled as still being checked, even before the first evaluation has started. A plain comment,
  `/maul verify` and `/maul verify fix` do not release it. maul goes on checking.

## On GitHub

### The `maul/acceptance` commit status

Posted by the maul App (Commit statuses: write) on the current head of each open pull request maul evaluated, with a
description of counts only. In a Project with several repositories that is each pull request in a previewed repository,
or the one in the repository a `cli` or `library` surface builds. A pull request of the same intent in a repository maul
does not evaluate gets no status, at any commit: nothing checked its commits, so nothing is said about them. Do not
require `maul/acceptance` on such a repository; the check would never arrive. (A status an earlier release posted there
is replaced once by the final `error` "maul does not evaluate this repository: nothing here was checked".)

| State     | When                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| --------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `pending` | waiting for checks or the preview ("waiting for the pull requests' checks", "waiting for the preview"); the combined change moved ("combined change moved: re-verifying"); maul is evaluating ("maul is evaluating this: evaluation 1, 3 outcomes, 0 of 2 automatic fixes used"); a plan whose outcomes are all `manual`, not all accepted yet ("0/2 outcomes accepted by hand"); a plan that declares no outcomes, which nobody has accepted ("no outcomes declared; an approver may /maul accept all") |
| `success` | every outcome is seen to hold, or an approver accepted it at the current heads ("3/3 outcomes seen to hold", "3/3 outcomes seen to hold; some accepted by hand by octocat", "2/2 outcomes accepted by hand by octocat", "no outcomes declared; accepted by octocat")                                                                                                                                                                                                                                     |
| `failure` | an outcome is seen not to hold ("2/3 outcomes seen to hold, 1 failing"), a fix runs ("automatic fix 1 of 2 running"), automatic fixes are halted ("automatic fixes halted") or none is left ("no automatic fix left")                                                                                                                                                                                                                                                                                    |
| `error`   | maul gave up on an outcome ("maul can't call this ready: 2/3 outcomes seen to hold"), final until an approver accepts it or the heads move; a check cannot run as approved ("its target is not previewed"); maul could not run the checks; the approved acceptance criteria cannot be read; or verification was switched off ("verification switched off")                                                                                                                                               |

`success` never stands for work nobody looked at. What maul's evaluator did not see hold (a `manual` outcome, one it
could not see hold, one it saw not hold, or a plan that declares no outcomes at all) turns green only through an
approver's `/maul accept` at the current heads, and the description names who accepted. An approver who has looked
accepts one outcome with `/maul accept <id>`, or every open one with `/maul accept all`. Once an approver has accepted
every outcome not seen to hold at the current heads, the status is `success` and the hand-off ready, with no automatic
fix running; accepting only some of them, or at commits the pull requests have since moved from, leaves it as it was.

When verification is switched off for an intent (the chart, the controller, the Project or its preview stops verifying),
maul replaces its last status on each pull request with a final `error`, so a required status never stays pending or
green. No status is posted for an intent maul never verified, or once the intent has ended. The status is re-posted on
**every** pull request when any sibling's head moves, so a green is never stale: from the pass that sees a new head
until a round is about the new heads, it reads "combined change moved: re-verifying". About once per poll interval maul
also lists the statuses at each head: if another account posted `maul/acceptance` after maul's, maul posts its own again
and names that account in the verification section, also once verification is switched off. A later status of maul's own
App in another state (posted by a pass the controller restarted during, before it recorded the post) is posted again
too, naming no one.

Make it count: add a ruleset or branch protection that **requires `maul/acceptance` from the maul App**. Without a
required check, a person can still merge past a red status. With it, every pull request maul evaluates waits for the
status, so a plan with `manual` outcomes or no outcomes needs an approver's `/maul accept` before it can merge.
`maul check project` warns when the branch rules do not name it.

### Comments

- The **issue's status comment** carries one line beside the preview link that counts outcomes ("Evaluation 1 passed:
  3/3 outcomes seen to hold"), and the auto-fix state and reason. Its preview line carries the hand-off: the link held,
  "Ready: maul's evaluator saw every approved outcome hold, citing calls maul recorded.", "maul can't call this ready:
  ..." with the reason, or "Shown while maul's evaluator is still looking at the approved outcomes." once a human
  released it.
- A **verification comment** on the issue per verification, edited per attempt, and a verification section in each
  **previewed pull request's sticky comment**: every outcome with its result and the calls its verdict cites that count,
  each shown as maul's tools recorded it: what it sent and what came back, fenced as the application's output. The
  evaluator's notes and look-around anomalies are marked as its words. Log tails appear on a pull request only with
  `postLogs`. See [Reading the evidence](evaluator.md#reading-the-evidence).
- After a push, from the pass that sees it until a verification is about the new heads, the status comment and the
  sticky comments say maul is evaluating the new commits and show no result; the last verification comment is left as it
  stood, about the heads it checked.
- Notices: auto-fix halted (with the reason), "automatic fix cancelled for your review", infrastructure unavailable and
  `NeedsReplan`.

### Commands

Approvers only, on the intent issue or any of its pull requests, while the intent is in review or revising. Settled on
the next poll, never from a webhook.

| Command                | Effect                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                           |
| ---------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `/maul verify`         | Verify now, at the next gate (once the current round ends), bypassing the spacing but not `maxVerifications`: once it is spent, the reply says so and nothing runs. A request that cannot run (verification off for the Project, or a round that cannot start) is answered "did not run" with the reason, never only acknowledged; send it again once that changes. On a preview that expired at the commits it would check it does not run either, since nothing deploys an expired preview again at the same commits: the earlier result stands, and a push to a pull request or `/maul verify --reset` deploys it again. A request already waiting for the preview when it expires is answered the same way at once, and the verification reads unavailable. A push recorded but not yet verified is a redeploy on its way, so a request then is recorded. A later "did not run" answer is posted where the command was made: the issue, or the pull request. |
| `/maul verify fix`     | As above, and re-arm automatic fixes after a halt, a fix round that found no defect included: a round that still fails then starts a fix round, within `maxVerifyFixes`. On an expired preview it is recorded while a failing result stands at the commits: the fix round it starts pushes, which deploys the preview again.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| `/maul verify stop`    | Halt automatic fixes (`HumanStop`, recording who and when) without verifying or starting anything. A verify-fix round still queued is cancelled; one already running finishes and pushes. `/maul verify fix` re-arms. Plain `/maul verify` never halts anything.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| `/maul verify --reset` | Roll the preview's component pods first, dropping their data, then verify. This is the only reset: nothing resets data automatically. Once per five minutes.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| `/maul accept <id>`    | Accept one outcome at the current heads: a `manual` one, one maul's evaluator did not see hold, or one it saw not hold. One seen to hold, or not decided yet, cannot be accepted, and the reply lists what can. It counts only if maul had recorded the commits it evaluates when you commented and, for a preview, the preview was running them; otherwise it is refused and you accept again once it does. A note may follow on the lines below the command; it is recorded with the acceptance. After the heads move it is shown as accepted earlier and not re-checked.                                                                                                                                                                                                                                                                                                                                                                                      |
| `/maul accept all`     | Accept every outcome that can be accepted now, each recorded as itself. For a plan that declares no outcomes, accept the change as it is at the current heads, which turns `maul/acceptance` green. Again after the heads move.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| `/maul preview`        | With the evaluator and a gated hand-off: show the held preview link now, before maul has finished checking; nothing else changes. Without a held link it does nothing and says so.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                               |

`/maul verify` starts a new evaluation, with the model, spending from the evaluation ceiling; at unchanged heads it does
not clear what an earlier round there found ([A finding holds its commits](evaluator.md#a-finding-holds-its-commits)).

A command from a non-approver, an edited comment or a bot is refused. An unknown id or argument gets the help reply.
With verification switched off in the controller, the commands do nothing and are answered as not available. Each
command is applied once: if maul's reply to it is deleted, maul answers it again but never applies it again.

## When something is not what you expect

| You see                                                                                                     | Meaning                                                                                                                                                                                                                                                                                                                                                                                   |
| ----------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| No `maul/acceptance` status at all                                                                          | Verification is off (chart, controller or Project) and maul never verified the intent, the App lacks Commit statuses write (the Project's `AcceptanceStatusUnavailable`), or the intent has ended                                                                                                                                                                                         |
| `pending`, "outcomes accepted by hand" or "accept all"                                                      | What only a person can close waits for an approver: `/maul accept <id>` for a `manual` outcome, or `/maul accept all` for a plan that declares no outcomes                                                                                                                                                                                                                                |
| `error`, "verification switched off"                                                                        | Verification was turned off for this intent after maul posted a status; nothing more is verified or reported until it is turned back on, when maul starts over at the current heads (an intent of a cli or library surface whose round had settled at those heads keeps its result)                                                                                                       |
| `pending`, "waiting for the preview"                                                                        | No live preview at exactly the pull request heads yet. Check `kubectl get preview`                                                                                                                                                                                                                                                                                                        |
| `error`, "the preview did not become live at these commits in time"                                         | The preview stayed Failed, expired, queued or never served (no preview-controller) for an hour, or expired while your `/maul verify` waited for it, so nothing ran. The Intent's verification is `Unavailable` (`PreviewNotLive`); it verifies as soon as the preview is live. An expired preview is deployed again only by a push or `/maul verify --reset`. Check `kubectl get preview` |
| `error`, "maul could not run the checks"                                                                    | Three tool pod attempts failed for infrastructure reasons (scheduling, image pull, quota, deadline, a component disrupted by the cluster: `component-disrupted`, or a component Service without a ClusterIP: `service-no-cluster-ip`). Not a verdict on the change                                                                                                                        |
| `error`, "the approved acceptance criteria cannot be read"                                                  | The approved plan's stored copy is missing or no longer matches its approval, so maul verifies nothing rather than run an earlier plan's criteria. The Intent's `VerifyFailing` condition (reason `CriteriaUnreadable`) says the same; the plan cannot change in review, so `/maul cancel` and open a new issue                                                                           |
| No preview link, "maul holds its link"                                                                      | The evaluator is checking under a gated hand-off. It is released when the evaluation decides, after `handoffMax`, or at once on your comment, push or `/maul preview`                                                                                                                                                                                                                     |
| "maul can't call this ready"                                                                                | maul gave up before its evaluator saw every outcome hold, for the reason it names. Read each outcome's line, then accept, `/maul verify` or merge: it never blocks. [The reasons](evaluator.md#the-hand-off)                                                                                                                                                                              |
| An outcome "unsure", "the evaluator's verdict cites no call of its own in this evaluation that maul counts" | The evaluator said it holds or fails, but cited only logs, events, source reads, calls at the base, refused calls or a browser command that failed. Read its note and the calls it made, then accept it, `/maul verify` or merge. [What makes a pass](evaluator.md#what-makes-a-pass)                                                                                                     |
| Nothing verified, a notice about acceptance criteria                                                        | The evaluator is on and this intent's approved plan states criteria. Review it as it is, or `/maul cancel` and open a new issue                                                                                                                                                                                                                                                           |
| A codex Project's link shown at once, `EvaluatorUnverified`                                                 | Its intents are not evaluated: the codex evaluator is off (`--intent-evaluate-codex`). [codex](evaluator.md#codex-built-not-verified)                                                                                                                                                                                                                                                     |

A component's data lives in its own pod, and `/maul verify --reset` rolls the pods to drop it.
