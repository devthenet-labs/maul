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
   written. Without `--intent-previews-enabled` the controller still starts, logs a warning and leaves verification
   `Off`: each verifying Project carries the condition `VerificationOff` (reason `PreviewsDisabled`), and each of its
   intents in review says so in its status comment.
3. **The Project**, per Project:

```yaml
spec:
  verification:
    enabled: true
    manualGatesStatus: false # no effect: an open manual item always holds the status pending
    postLogs: false # default: component log tails go on the issue only, never on a pull request
    timeout: 10m # the tester Job's wall clock, clamped by --intent-verify-timeout-max
  limits:
    maxVerifyFixes: 2 # 0..10, automatic fix rounds per approved plan revision
    maxVerifications: 20 # 1..100, automatic verifications per approved plan revision
```

The App needs **Commit statuses: Read & write** on each application repository to post `maul/acceptance`: see
[Create the GitHub App](../getting-started/github-app.md#intents). An App made by `maul setup github-app --intents` has
it. It is never part of `Ready`: a verifying Project whose App lacks it stays `Ready`, verifies and comments as usual,
posts no status, and shows the condition `AcceptanceStatusUnavailable` (naming the repository), as do the verification
comments. To fix it, set Commit statuses to Read and write in the App's settings, then accept the change on the
installation.

## What a plan declares

The plan agent writes an optional `acceptance` list in its report's frontmatter, up to 16 criteria and 48 steps in all.
Their observations must also fit a size budget at every value's largest, so that no answer from the application can
outgrow what maul reads of the tester's output; a plan over it is refused. The plan comment says in one line what maul
will do with it, for example "2 checked automatically, 1 needs you: sounds-good (only a human can judge how it sounds)",
so the approver sees the split before approving.

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
| `command` | Parsed and shown, reported as "not run" in this release; a person closes it with `/maul accept <id>`              |

The grammar is checked statically before the plan is posted (path, headers, body size, at most four captures per step,
RFC 6901 pointers, matchers `$present`, `$absent`, `$contains`, `$approx` and `$order`). While the plan will be verified
(the Project sets `verification.enabled`, intent-controller runs with verification and previews on, and the Project
previews at least one repository), a `target` that is not a previewed repository of the Project refuses the plan, and
the planner's retry is told why. Otherwise verification is optional and never fails an intent: the criteria are kept,
and the plan comment shows them as not checked. A criterion that can no longer run as approved later puts the intent in
`NeedsReplan`: use `/maul replan`.

The plan comment's header states what the approver is approving: how many criteria maul checks automatically and which
need a person, that the plan declares none, that they are not checked because verification is off, or that maul could
not ask the planner for criteria because the Project previews no repository a criterion could target.

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
passed, and the status stays pending until a person runs `/maul accept <id>` or a clean re-run passes. A retry starts a
second and a half after the failed try, so one blip does not fail both.

### Before the first check: the tester waits for the slot network

The tester is a new Pod on every run, and the slot's NetworkPolicy admits it only once the cluster's policy controller
has programmed the policy for that Pod. On k3s and k3d, kube-router does that about a second after the Pod starts and
rejects its connections until then, which looks exactly like an application that is not listening. So before its first
criterion the tester waits for every component's Service to accept a TCP connection (it sends nothing), retrying from
100 ms backing off to 1 s, for at most a minute and never more than a quarter of the tester's budget. The wait per
component, how long and how many attempts were refused, is recorded in the `PreviewVerification`'s `status.readiness`.

Once every component is reached, the checks run as before, and a connection error during a check is the application's
failure. The window absorbs the policy being programmed; a component still unreachable at its end is the change's
failure too, reason `component-unreachable` ("unreachable from the tester for 60s while Ready"): it counts as a
verification and can start a fix round, whose handoff names the component and how long it was unreachable. Readiness
alone does not prove the tester can connect: an application can pass the kubelet's probe while refusing every other
source, so maul never reports such a run as "could not run the checks".

A component that exits during the attempt and comes back Ready (its restart count rose since the tester started) has
crashed: reason `component-restarted`, the change's failure. Restarts from before the attempt are not counted.

### Bounds

| Bound                     | Rule                                                                                                            |
| ------------------------- | --------------------------------------------------------------------------------------------------------------- |
| `limits.maxVerifyFixes`   | Automatic fix rounds per approved plan revision (default 2). Then auto-fix halts, reason `MaxVerifyFixes`.      |
| `limits.maxVerifications` | Automatic verifications per plan revision (default 20). `/maul verify` bypasses the interval, not this cap.     |
| Repeated failure          | A fix whose result has the same failure fingerprint as the last fix halts auto-fix: `RepeatedFailure`.          |
| Cost ceiling              | At the intent's cost ceiling auto-fix, retries included, halts: `CostCeiling`. It never blocks the intent.      |
| Spacing                   | While auto-fix is halted, an automatic re-verification waits five minutes; one `/maul verify` per five minutes. |
| Infrastructure            | A tester that could not run is retried up to three attempts, uncounted, then reported `Unavailable`.            |
| Preview wait              | A due verification waits at most an hour for the preview to be live at the heads, then reports `Unavailable`.   |

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

| State     | When                                                                                                                                                                                                                                                                                                                                                                                                                             |
| --------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `pending` | waiting for checks or the preview; verifying ("verifying: round 1 of 2, 3/4 checks"); the combined change moved ("combined change moved: re-verifying"); a pass only on retry; an item only a person closes and nobody has accepted yet ("4/4 automated passed, 1 to accept by hand", "0/2 accepted by hand"); a plan with no criteria nobody has accepted ("no acceptance criteria declared; an approver may /maul accept all") |
| `success` | every criterion passed at the current heads or a person accepted it there ("4/4 automated passed", "4/4 automated passed; 1 accepted by hand by octocat", "2/2 accepted by hand by octocat", "no acceptance criteria declared; accepted by octocat")                                                                                                                                                                             |
| `failure` | a check failed ("3/4 automated passed, 1 failing"), a fix is running, or none is left ("no automatic fix left")                                                                                                                                                                                                                                                                                                                  |
| `error`   | a check cannot run as approved ("replan needed"), maul could not run the checks, the approved criteria cannot be read, or verification was switched off ("verification switched off")                                                                                                                                                                                                                                            |

`success` never stands for work nobody checked: what maul does not run (a `manual` or `command` criterion, one that
passed only on retry, or a plan that declares no criteria at all) turns green only through an approver's `/maul accept`
at the current heads, and the description names who accepted. When verification is switched off for an intent (the
chart, the controller, the Project or its preview stops verifying), maul replaces its last status on each pull request
with a final `error`, so a required status never stays pending or green. No status is posted for an intent maul never
verified, or once the intent has ended. The status is re-posted on **every** pull request when any sibling's head moves,
so a green is never stale. About once per poll interval maul also lists the statuses at each head: if another account
posted `maul/acceptance` after maul's, maul posts its own again and names that account in the verification section.

Make it count: add a ruleset or branch protection that **requires `maul/acceptance` from the maul App**. Without a
required check, a person can still merge past a red status. With it, every pull request of a verifying Project waits for
the status, so a plan with manual items or no criteria needs an approver's `/maul accept` before it can merge.
`maul check project` warns when the branch rules do not name it.

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

| Command                | Effect                                                                                                                                                                                                                                                                                                                                                                           |
| ---------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `/maul verify`         | Verify now, at the next gate (once the current round ends), bypassing the spacing but not `maxVerifications`: once it is spent, the reply says so and nothing runs.                                                                                                                                                                                                              |
| `/maul verify fix`     | As above, and re-arm automatic fixes after a human halt.                                                                                                                                                                                                                                                                                                                         |
| `/maul verify --reset` | Roll the preview's component pods first, dropping their data, then verify. This is the only reset: nothing resets data automatically. Once per five minutes.                                                                                                                                                                                                                     |
| `/maul accept <id>`    | Close a manual criterion, a `command` one (not run in this release), a quarantined one or one whose tests changed, at the current heads. It counts only if the preview was live at those heads when you commented, and still is; otherwise it is refused and you accept again once the preview runs them. After the heads move it reads "accepted at `<heads>`, not re-checked". |
| `/maul accept all`     | Only for a plan that declares no acceptance criteria: accept the change as it is at the current heads, which turns `maul/acceptance` green. Again after the heads move.                                                                                                                                                                                                          |

A command from a non-approver, an edited comment or a bot is refused. An unknown id or argument gets the help reply.
With verification switched off in the controller, the commands do nothing and are answered as not available. Each
command is applied once: if maul's reply to it is deleted, maul answers it again but never applies it again.

## When something is not what you expect

| You see                                                    | Meaning                                                                                                                                                                                                                                                                                         |
| ---------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| No `maul/acceptance` status at all                         | Verification is off (chart, controller or Project) and maul never verified the intent, the App lacks Commit statuses write (the Project's `AcceptanceStatusUnavailable`), or the intent has ended                                                                                               |
| `pending`, "to accept by hand" or "accept all"             | Everything maul runs passed; what only a person can close waits for an approver's `/maul accept <id>`, or `/maul accept all` for a plan with no criteria                                                                                                                                        |
| `error`, "verification switched off"                       | Verification was turned off for this intent after maul posted a status; nothing more is verified or reported                                                                                                                                                                                    |
| `pending`, "waiting for the preview"                       | No live preview at exactly the pull request heads yet. Check `kubectl get preview`                                                                                                                                                                                                              |
| `error`, "the preview was not live within 60 minutes"      | The preview stayed Failed, expired, queued or never served (no preview-controller) for an hour, so nothing ran. The Intent's verification is `Unavailable` (`PreviewNotLive`); it verifies as soon as the preview is live. Check `kubectl get preview`                                          |
| `error`, "maul could not run the checks"                   | Three tester attempts failed for infrastructure reasons (scheduling, image pull, quota, deadline, or a tester image whose output format does not match the controllers: `tester-version-mismatch`; keep `preview.verification.testerImage` at the chart's release). Not a verdict on the change |
| `error`, "the approved acceptance criteria cannot be read" | The approved plan's stored copy is missing or no longer matches its approval, so maul verifies nothing rather than run an earlier plan's criteria. The Intent's `VerifyFailing` condition (reason `CriteriaUnreadable`) says the same; `/maul replan`                                           |
| A failing criterion that passes by hand                    | Checks run against a preview that is never reset; one that depends on empty data should be replanned, or use `--reset`                                                                                                                                                                          |

This release verifies `http` criteria against a Preview's components. Backing services, seeded data, edges between
components and `command` criteria arrive in a later release; until then a component's data lives in its own pod, and
`/maul verify --reset` rolls the pods to drop it.
