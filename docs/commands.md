# Commands on issues and pull requests

People steer maul from GitHub with **commands**: comments that start with `/maul`, such as `/maul approve`. On an
intent's issue, two **labels** do the same job as two of the commands. This page lists every command and label, where
each one works and who may use it.

Which commands exist depends on where you comment. maul calls each place a _surface_, and there are three:

| Surface                                                     | What it is                                  | Commands                                                              |
| ----------------------------------------------------------- | ------------------------------------------- | --------------------------------------------------------------------- |
| [An intent's issue](#on-an-intents-issue)                   | The issue a change was asked for in         | `approve`, `replan`, `start`, `verify`, `accept`, `preview`, `cancel` |
| [An intent's pull request](#on-an-intents-pull-request)     | A pull request maul opened for an intent    | `revise`, `retry`, `verify`, `accept`, `preview`                      |
| [A finding's tracking issue](#on-a-findings-tracking-issue) | The issue maul opens for a security finding | `approve`, `retry`, `expedite`, `suspend`, `resume`                   |

A verb that appears on two surfaces means what that surface makes of it: `/maul approve` on an intent's issue approves a
plan, and on a finding's tracking issue it releases a held finding.

## How to write a command

A command is one line:

```text
/maul <verb> [note]
```

- **It must be the first non-blank line of the comment.** A `/maul` line below other text is not a command.
- **Quoted text and code are never commands.** A line that starts with `>`, a command inside a code span or a code
  block, and a line indented by four or more spaces are all plain text.
- `/maul` may be written in any case. The verb is the next word on the line.
- **The note** is everything after the verb: the rest of that line and every line below it. It is cut at 1 KiB, and
  control characters and characters that reorder or hide text are removed. Only some verbs use their note; the tables
  below show `[note]` for those.
- A few verbs take an **argument** instead, such as `/maul accept <id>`. The argument must be on the command's own line.
  maul never guesses an argument: one it does not know gets the help reply.

For example, this comment on an intent's pull request starts a revision round, and the two lines are its note:

```text
/maul revise use the existing date helper instead of a new one
and add a test for the leap-year case
```

!!! tip "If you put the command on a later line"

    On an intent's issue or pull request, an approver who writes `/maul <verb>` below other text gets one reply that
    says where commands go. Nothing is acted on. Post the command again as a new comment.

## On an intent's issue

An intent starts from an issue in a Project's intent repository ([Concepts](concepts.md#intent) defines these words).
[The intent lifecycle](intents/lifecycle.md) walks through when each of these is used.

### Labels

| Label                                                | Effect                                                                  |
| ---------------------------------------------------- | ----------------------------------------------------------------------- |
| The trigger label (by default `maul:<project>`)      | Starts an intent from the issue: maul plans it                          |
| The approve label (by default `maul:approved`)       | Approves the posted plan, and the build starts. Same as `/maul approve` |
| The trigger label applied again                      | Asks for a new plan. Same as `/maul replan`                             |
| The trigger label applied again to a `Failed` intent | Revives it: a new plan, which needs a new approval                      |

The Project names both labels (`spec.labels`), and maul creates them in the intent repository when they are missing. Use
one Project's trigger label per issue.

### Commands

| Command                                                             | Effect                                                                                            | Works while the intent is             |
| ------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------- | ------------------------------------- |
| `/maul approve`                                                     | Approves the posted plan and starts the build                                                     | `AwaitingApproval`                    |
| `/maul replan [note]`                                               | Plans again, using the note and the approvers' comments since the last plan                       | `AwaitingApproval`                    |
| `/maul start`                                                       | Starts the intent now, without waiting for the issues it is [blocked by](intents/dependencies.md) | `Pending`, waiting on its blockers    |
| `/maul verify`, alone or with `fix`, `stop`, `--reset` or `explore` | Checks the change against the plan's outcomes now                                                 | `InReview` or `Revising`              |
| `/maul accept <id>` or `/maul accept all`                           | Accepts an outcome, or every outcome that can be accepted, at the current commits                 | `InReview` or `Revising`              |
| `/maul preview`                                                     | Shows the preview link now, before maul has finished checking the change                          | `InReview` or `Revising`              |
| `/maul cancel`                                                      | Stops work on the intent and closes its issue. Open pull requests are left to you                 | Any phase before the intent has ended |

With per-user model credentials enforced, `/maul approve` (or the approve label) also works on an intent that is
`Blocked` with `CredentialRequired`: an approver who holds a valid credential re-approves the same plan.

`verify`, `accept` and `preview` belong to [the agent evaluator](intents/evaluator.md) and are offered only when it is
on; [the verification commands](#the-verification-commands) below explains them. `/maul preview` is offered only for a
Project that has a preview.

The phases are listed in [State machine & labels](labels.md#intent-phases). A command sent in a phase that does not
admit it changes nothing: maul replies that it does nothing now and lists the commands that do.

`/approve`, without `maul`, is plain text on an intent. It is an alias only on a finding's tracking issue.

### Who may use them

A label or command on an intent's issue counts only when all of these hold:

- the account is listed in the Project's `approvers.logins`;
- the account has write access to the intent repository;
- the account is not a bot.

maul reads who acted from GitHub's API: the actor of a label event, or the author of a comment. A label applied as the
issue is created (by an issue form's `labels:` or by `gh issue create --label`) counts as the issue author's. Such an
issue starts an intent only when its author is an approver.

Anything else is refused once and changes nothing. A refused approve label is removed. A trigger label from anyone who
is not an approver closes the intent before it plans.

## On an intent's pull request

Once maul has opened the pull request, these commands go on the pull request itself.

| Command or action                                                   | Effect                                                                                                 |
| ------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------ |
| A review that **requests changes**                                  | Starts a revision round from the review. The event form of `/maul revise`                              |
| `/maul revise [note]`                                               | Starts a revision round from your review feedback and the note                                         |
| `/maul retry`                                                       | Retries the round that failed. It does nothing unless a round on this pull request's repository failed |
| `/maul verify`, alone or with `fix`, `stop`, `--reset` or `explore` | Checks the change against the plan's outcomes now                                                      |
| `/maul accept <id>` or `/maul accept all`                           | Accepts an outcome, or every outcome that can be accepted, at the current commits                      |
| `/maul preview`                                                     | Shows the preview link now, before maul has finished checking the change                               |

A _round_ is one more agent run on the pull request; [Revision rounds](intents/lifecycle.md#revision-rounds) explains
it. There are no label commands on a pull request.

**Who may use them.** The account must be in the Project's `approvers.logins`, have write access to the application
repository the pull request is in, and not be a bot.

## The verification commands

`/maul verify`, `/maul accept` and `/maul preview` work the same on the intent's issue and on any of its pull requests,
because an intent has one combined change and one preview. They are for approvers only and work while the intent is
`InReview` or `Revising`.

| Command                | Effect                                                                                                                                                          |
| ---------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `/maul verify`         | Starts a new evaluation round, once the current round has ended                                                                                                 |
| `/maul verify fix`     | As `/maul verify`, and lets maul fix failures on its own again after it had stopped                                                                             |
| `/maul verify stop`    | Stops maul fixing failures on its own. It verifies nothing and starts nothing                                                                                   |
| `/maul verify --reset` | Restarts the preview's pods first, which drops their data, then verifies                                                                                        |
| `/maul verify explore` | As `/maul verify`, but the evaluator looks again even where maul's saved checks alone could settle the round                                                    |
| `/maul accept <id>`    | Accepts one outcome: a manual one, one maul could not confirm, one it found failing, or one that passed only on retry. `<id>` is the outcome's id from the plan |
| `/maul accept all`     | Accepts every outcome that can be accepted now                                                                                                                  |
| `/maul preview`        | Shows a held preview link now. Nothing else changes                                                                                                             |

A note for `/maul accept` goes on the lines below the command, and is recorded with the acceptance. An acceptance is for
the commits the pull requests are at when you comment; after a push you accept again.

[Acceptance verification](intents/acceptance.md#commands) has the full rules for each one, and
[The hand-off](intents/evaluator.md#the-hand-off) explains the held preview link.

## On a finding's tracking issue

A _finding_ is a security alert maul is working on, and its tracking issue is the issue maul opens for it
([Concepts](concepts.md#finding)).

| Command                | Effect                                                                  | Works when the finding is                                             |
| ---------------------- | ----------------------------------------------------------------------- | --------------------------------------------------------------------- |
| `/maul approve [note]` | Releases the hold on a held finding, or revives one that was handed off | `AwaitingApproval` or `HandedOff`                                     |
| `/maul retry`          | Retries a failed finding from the state it failed in                    | `Failed`                                                              |
| `/maul expedite`       | Skips the accumulation window, the minimum age and the queue            | `Opened`, `Enhanced`, `Investigating`, `AwaitingApproval` or `Queued` |
| `/maul suspend`        | Pauses the finding's progress through the pipeline                      | Any phase that is not terminal                                        |
| `/maul resume`         | Resumes it after a suspend                                              | Suspended                                                             |

With [per-user model credentials](configuration/user-credentials.md) enforced, `/maul approve` also works on a `Queued`
finding that is held for its approver's credential.

These five mean exactly what the same actions mean on the [status page](status-ui.md) and in the [`maul` CLI](cli.md).
The phases are in [State machine & labels](labels.md#phases).

**The old approve comment.** `/approve` still approves a finding, as a deprecated alias of `/maul approve`. An
Integration can replace it with another alias (`spec.github.issues.approveComment`). The whole comment must be the
alias, or start with the alias and a space.

**No label commands.** The labels on a tracking issue are [projected by maul](labels.md#the-projected-labels) and are
never read back. Closing or reopening the issue, and merging or closing the pull request, do change the finding:
[Human signals](integrations/sources/github.md#human-signals) lists them.

**Who may use them.** The commenter needs write access to the tracking issue's repository: GitHub's `admin`, `maintain`
or `write` permission. Read access is not enough, and neither is organization membership. There is no approvers list
here. A command from a bot is ignored without a reply.

[GitHub code scanning](integrations/sources/github.md#commands) has the details, including how commands are delivered
and ordered.

## How maul answers

- A command maul takes up gets an 👀 reaction, then **one reply** saying what happened.
- **On an intent's issue or a finding's tracking issue**, a command that is not available now gets a reply listing the
  commands that are. A verb the issue does not offer, from someone who may command there, gets a reply listing the
  commands it does offer.
- **Someone who may not command** on one of those issues is told so once: once per intent, or once per finding. Later
  commands from that account get no reply, so nobody can make maul write to GitHub once per comment.
- **An edited comment is never a command on an intent.** GitHub lets anyone with write access edit another person's
  comment and still shows the original author. On the intent's issue maul answers an edited command once, saying so.
  Post it again in a new comment.
- Intent commands are settled when maul next polls GitHub, not instantly.

## Where the commands are defined

The lists on this page come from two packages in the source: `internal/command` (the grammar, and which verbs each
surface offers, in `surface.go`) and `internal/action` (the verb names, and when a finding admits each one).
