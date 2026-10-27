# Acceptance verification

Before maul hands an intent's pull requests to a human, it can run the combined change and check that it does what the
approved plan said, fixing what it can within a few bounded rounds. The plan carries a list of **acceptance criteria**;
approving the plan approves them byte for byte; maul runs every one it can automate, in the cluster, against the
intent's own [preview](../configuration/preview-controller.md), at exactly the pull request heads under review.

It is off by default, and it never blocks anything. A failing verification is a red `maul/acceptance` commit status and
a `VerifyFailing` condition: the intent stays in review, and a human's review, command or push always wins.

!!! note "What a passing status is worth"

    `maul/acceptance` is behavioural evidence, not an attestation. The pull request's author controls the code under
    test, and the checks run only what the approved plan asked for. Treat it as "the change did what the plan said, on
    the checks it listed", and review the code as you always did.

## Turning it on

Three switches, in this order. Each fails closed: with one off, no acceptance state is written anywhere.

1. **The slots.** `preview.verification.enabled: true` in the chart, with `preview.verification.testerImage` pinned by
   digest, renders the tester's admission rules, the per-part NetworkPolicy and Job policies and the slot quota for
   them, and the grants in
   [preview-controller](../configuration/preview-controller.md#acceptance-verification-off-by-default).
   `preview.verification.deadlineMaxSeconds` (60 to 3600, default 1800) bounds the tester Job and also sets both
   controllers' deadline flags, so no request can ask for a Job the slot policy refuses.
2. **intent-controller.** `--intent-verification` (needs `--intent-previews-enabled`; see
   [its flags](../configuration/intent-controller.md#flags)). Off, its plans carry no acceptance prompt and no state is
   written.
3. **The Project**, per Project:

```yaml
spec:
  verification:
    enabled: true
    manualGatesStatus: false # default: manual items never hold the status pending
    postLogs: false # default: component log tails go on the issue only, never on a pull request
    timeout: 10m # the tester Job's wall clock, clamped by --intent-verify-timeout-max
  limits:
    maxVerifyFixes: 2 # 0..10, automatic fix rounds per approved plan revision
    maxVerifications: 20 # 1..100, automatic verifications per approved plan revision
```

The App also needs **Commit statuses: Read & write** on each application repository, always, whether or not a Project
verifies: see [Create the GitHub App](../getting-started/github-app.md#intents). A Project reports `Ready` only once the
App holds it. An App made by `maul setup github-app --intents` has it.

## What a plan declares

The plan agent writes an optional `acceptance` list in its report's frontmatter, up to 16 criteria and 48 steps in all.
The plan comment says in one line what maul will do with it, for example "2 checked automatically, 1 needs you:
sounds-good (only a human can judge how it sounds)", so the approver sees the split before approving.

```yaml
acceptance:
  - id: battle-leaderboard # ^[a-z][a-z0-9-]{0,31}$, unique
    kind: http # http | command | manual
    statement: a battle's leaderboard ranks its patterns by votes
    target: beatbattle-api # a previewed repository key of the Project
    steps:
      - name: battle
        request: { method: POST, path: /api/battles, json: { name: "maul-{{run}}" } }
        expect: { status: 201 }
        capture: { battle: /id }
      - request: { method: GET, path: "/api/battles/{{capture.battle}}/leaderboard" }
        expect: { status: 200 }
  - id: sounds-good
    kind: manual
    statement: it sounds good
    reason: only a human can judge how it sounds
```

| Kind      | What maul does                                                                                                    |
| --------- | ----------------------------------------------------------------------------------------------------------------- |
| `http`    | The tester sends the steps to the target component's Service inside the slot, and checks status, JSON and headers |
| `manual`  | Nothing. A person closes it with `/maul accept <id>`                                                              |
| `command` | Parsed and shown, reported as "not run" in this release                                                           |

The grammar is checked statically before the plan is posted (path, headers, body size, at most four captures per step,
RFC 6901 pointers, matchers `$present`, `$absent`, `$contains`, `$approx` and `$order`). A `target` that is not a
previewed repository of the Project refuses the plan, and a criterion that can no longer run as approved later puts the
intent in `NeedsReplan`: use `/maul replan`.

Two rules shape good criteria, and the plan prompt states both:

- **The preview is never reset between rounds**, so a check creates its own data, named with `{{run}}`, a token maul
  makes unique per try (`v<round>a<attempt>t<try>-<6 hex>`). Prefer scoped reads over global lists.
- **Only `{{run}}` and `{{capture.<name>}}` may appear in `{{ }}`**; anything else is refused.

## What maul does with them

1. **Plan approved.** On the first review pass maul reads the approved plan back under its digest and records the
   criteria on the Intent's status.
2. **The preview link shows early.** It is posted as soon as the preview is Ready, labelled as verifying. The pull
   requests are not drafts, and `maul/acceptance` is pending on every open head.
3. **The gate.** Verification starts only when every open pull request's named checks are green at its head, no
   check-fix round is due, no human review or command is pending (including inside the review quiet window), the preview
   serves exactly the pull request heads, and the bounds below hold.
4. **The run.** intent-controller creates a `PreviewVerification`; preview-controller re-checks it against the Preview
   and the criteria, runs the tester Job inside the slot, and records only observations. intent-controller judges them.
5. **A failure** starts a verify-fix round, a normal revise round of trigger `verify`, handed the approved plan, the
   failing criteria with expected against observed, and only the target repository's own component log tail. The fix
   pushes new heads, CI runs, the preview rolls, and the loop continues.

A criterion that fails on its first try and passes on a retry is **quarantined**: shown to the human, never counted as
passed, and the status stays pending until a person runs `/maul accept <id>` or a clean re-run passes.

### Bounds

| Bound                     | Rule                                                                                                            |
| ------------------------- | --------------------------------------------------------------------------------------------------------------- |
| `limits.maxVerifyFixes`   | Automatic fix rounds per approved plan revision (default 2). Then auto-fix halts, reason `MaxVerifyFixes`.      |
| `limits.maxVerifications` | Automatic verifications per plan revision (default 20). `/maul verify` bypasses the interval, not this cap.     |
| Repeated failure          | A fix whose result has the same failure fingerprint as the last fix halts auto-fix: `RepeatedFailure`.          |
| Cost ceiling              | At the intent's cost ceiling auto-fix halts, `CostCeiling`. It never blocks the intent.                         |
| Spacing                   | While auto-fix is halted, an automatic re-verification waits five minutes; one `/maul verify` per five minutes. |
| Infrastructure            | A tester that could not run is retried up to three attempts, uncounted, then reported `Unavailable`.            |

### The human wins

Auto-fix halts, with the reason shown on the issue and the status, the moment a human acts:

- **`HumanReview`, `HumanCommand`:** an approver's changes-requested review or `/maul revise` runs first, before any
  automatic fix, even when a fix is also due or a review is still in its quiet window. If an automatic fix is queued but
  has not started, it is **cancelled for your review** and your round runs next. One that is already running finishes
  and pushes, then your round runs next, and its text notes the automatic fix.
- **`HumanPush`:** a head maul did not push halts auto-fix. Verification still re-runs and reports; it never fixes.
- `/maul verify fix` re-arms auto-fix. It does not reset the fix count.

## On GitHub

### The `maul/acceptance` commit status

Posted on every open pull request's current head, by the maul App (Commit statuses: write), with a description of counts
only:

| State     | When                                                                                                                                                                                                                        |
| --------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `pending` | waiting for checks or the preview; verifying ("verifying: round 1 of 2, 3/4 checks"); the combined change moved ("combined change moved: re-verifying"); a pass only on retry; an open manual item with `manualGatesStatus` |
| `success` | every automated check passed at the current heads ("4/4 automated passed", or "automated part verified, 1 manual open")                                                                                                     |
| `failure` | a check failed ("3/4 automated passed, 1 failing"), a fix is running, or none is left ("no automatic fix left")                                                                                                             |
| `error`   | a check cannot run as approved ("replan needed"), or maul could not run the checks                                                                                                                                          |

No status is posted when nothing is checked automatically (no criteria declared, or only manual ones), when the Project
or the preview does not verify, or once the intent has ended. The status is re-posted on **every** pull request when any
sibling's head moves, so a green is never stale. About once per poll interval maul also lists the statuses at each head:
if another account posted `maul/acceptance` after maul's, maul posts its own again and names that account in the
verification section.

Make it count: add a ruleset or branch protection that **requires `maul/acceptance` from the maul App**. Without a
required check, a person can still merge past a red status. `maul check project` warns when the branch rules do not name
it.

### Comments

- The **issue's status comment** carries one line beside the preview link ("verifying: round 1 of 2, 3/4 checks
  passing") and the auto-fix state and reason.
- A **verification comment** on the issue per round, edited per attempt: each criterion's outcome, for a failure what
  was expected and what was observed, a bounded body prefix, and the component log tails, all sanitised; flags for
  quarantined criteria and an impostor status.
- Each **previewed pull request's sticky comment** gets a verification section; log tails appear there only with
  `postLogs`.
- Notices: auto-fix halted (with the reason), "automatic fix cancelled for your review", infrastructure unavailable and
  `NeedsReplan`.

### Commands

Approvers only, on the intent issue or any of its pull requests, while the intent is in review or revising. Settled on
the next poll, never from a webhook.

| Command                | Effect                                                                                                                                                               |
| ---------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `/maul verify`         | Verify now, at the next gate (once the current round ends), bypassing the spacing but not `maxVerifications`: once it is spent, the reply says so and nothing runs.  |
| `/maul verify fix`     | As above, and re-arm automatic fixes after a human halt.                                                                                                             |
| `/maul verify --reset` | Roll the preview's component pods first, dropping their data, then verify. This is the only reset: nothing resets data automatically. Once per five minutes.         |
| `/maul accept <id>`    | Close a manual criterion, a quarantined one or one whose tests changed, at the current heads. After the heads move it reads "accepted at `<heads>`, not re-checked". |

A command from a non-approver, an edited comment or a bot is refused. An unknown id or argument gets the help reply.

## When something is not what you expect

| You see                                  | Meaning                                                                                                                        |
| ---------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------ |
| No `maul/acceptance` status at all       | Verification is off (chart, controller or Project), the plan declared nothing automatable, or the intent has ended             |
| `pending`, "waiting for the preview"     | No live preview at exactly the pull request heads yet. Check `kubectl get preview`                                             |
| `error`, "maul could not run the checks" | Three tester attempts failed for infrastructure reasons (scheduling, image pull, quota, deadline). Not a verdict on the change |
| A failing criterion that passes by hand  | Checks run against a preview that is never reset; one that depends on empty data should be replanned, or use `--reset`         |

This release verifies `http` criteria against a Preview's components. Backing services, seeded data, edges between
components and `command` criteria arrive in a later release; until then a component's data lives in its own pod, and
`/maul verify --reset` rolls the pods to drop it.
