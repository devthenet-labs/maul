# The intent lifecycle

This page follows one intent from the issue to the merge, from the point of view of the people involved: the person who
writes the issue, and the approver who approves the plan and reviews the pull request. It says what to do at each step
and what maul does in reply.

An **intent** is one change a person asks for in a GitHub issue. An **approver** is a GitHub account listed in the
Project's `approvers.logins`; only an approver's labels, commands and reviews have any effect.
[Concepts](../concepts.md) defines the other words, [How it works](../how-it-works.md#the-intent-flow) shows the same
flow as a diagram, and [Commands on issues and pull requests](../commands.md) lists every command used below.

| Step | Who      | What happens                                                                        | The intent's phase             |
| ---- | -------- | ----------------------------------------------------------------------------------- | ------------------------------ |
| 1    | Anyone   | [Writes the issue](#1-write-the-issue)                                              |                                |
| 2    | Approver | [Adds the trigger label](#2-start-it-the-trigger-label)                             | `Pending`, then `Planning`     |
| 3    | maul     | [Posts the plan](#3-read-the-plan)                                                  | `AwaitingApproval`             |
| 4    | Approver | [Approves, or asks for a new plan](#4-approve-or-ask-for-a-new-plan)                | `Building`                     |
| 5    | maul     | [Builds and opens the pull request](#5-the-build-and-the-pull-request)              | `InReview`                     |
| 6    | Both     | [Rounds on the pull request](#6-rounds-on-the-pull-request): revisions and CI fixes | `Revising`, back to `InReview` |
| 7    | maul     | [Checks the change and hands it off](#7-the-evaluator-and-the-hand-off) (optional)  | `InReview`                     |
| 8    | A person | [Merges or closes](#8-merge-or-close)                                               | `Merged` or `Closed`           |

The phases are explained in [State machine & labels](../labels.md#intent-phases).

## 1. Write the issue

Open an issue in the Project's intent repository and say what you want changed. Anyone who can open an issue there can
write one; it starts nothing until an approver labels it.

How maul uses the issue tells you how to write it:

- **The planner reads the issue's title and body.** That text is the request. Put everything the change needs in it.
- **The build never sees the issue.** The build agent receives only the approved plan. If something matters, check that
  the plan says it.
- **An ambiguous request becomes a question.** The planner is told to plan the most reasonable reading and to list what
  it assumed as questions for the approver. Saying what you mean up front saves a second plan.
- **Do not edit the issue after the plan is posted.** An approval counts only if the issue still reads as it did when
  the plan was made. If you need to change the request, change it and ask for a new plan.
- **Three directories are off limits.** A build that changes anything under `.github/`, `.maul/` or `.devcontainer/` is
  refused, and the planner is told never to plan changes there. Make those changes yourself.
- **New dependencies need attention.** The build runs in the repository's own agent image and can use only what is
  already in that image, plus anything the operator lets the build fetch. The plan names each new dependency, and the
  plan comment says which ones must be added to the image before you approve.
- **Keep it to a size one build can finish.** A build has limits on turns, tokens and time. The planner is told to plan
  the part of a request that fits and to say what is left. [Sizing limits](sizing.md) covers larger features.
- **With the evaluator on, write requirements as list items.** The plan states _outcomes_, and an outcome can quote the
  line of the issue it answers. The plan comment lists the issue's list items that no outcome quotes, so you can see
  what will not be checked.

If the change must wait for other issues, mark this issue as blocked by them on GitHub. The intent then waits until they
are closed as completed. See [Intent dependencies](dependencies.md).

## 2. Start it: the trigger label

An approver adds the Project's **trigger label** to the issue. By default it is `maul:<project>`, for example
`maul:shop`. maul finds the labelled issue the next time it polls GitHub and creates an `Intent` named
`<project>-<issue number>`, such as `shop-42`.

The label counts only when an approver with write access to the intent repository applied it. A trigger from anyone else
gets one refusal, and the intent is closed before anything is planned. A label added as the issue is created counts as
the issue author's, so an issue form that adds the trigger label works only for authors who are approvers.

A Project runs a limited number of intents at once (`limits.maxActiveIntents`, 2 by default). A new one past that waits
for a free slot.

## 3. Read the plan

maul starts a **plan run**: an agent that reads the code and cannot change it. When it finishes, maul posts the plan as
a comment on the issue, and the intent is `AwaitingApproval`.

The plan comment has these parts:

- **A summary**: one line saying what the change does.
- **Acceptance outcomes**, when the evaluator is on: what maul will check, and the issue lines nothing checks.
- **Before you approve**: things maul wants you to notice. They include new dependencies, the planner's questions, and a
  line saying that the plan has notes for the builder.
- **To approve**: the label and the command, with the plan's revision (`r1`, `r2`, …) and digest.
- **The plan**: the planner's report in a code block, exactly as the build agent will read it, byte for byte. GitHub
  does not wrap a code block, so scroll sideways to read long lines to the end.

The plan itself is written under the headings Approach, Steps, Test plan, Risks and New dependencies.

### Notes for the builder

A plan may end with a `## Notes for the builder` section. It holds what the build agent should know that the steps do
not say: where things are, the commands to use, the pitfalls the planner found.

The notes are part of the plan. The build receives them with it, so read them as carefully as the steps. When a plan has
them, the comment says so under "Before you approve".

## 4. Approve, or ask for a new plan

To approve, an approver adds the **approve label** (`maul:approved` by default) or comments `/maul approve`. maul then
builds exactly the plan in the comment. Nothing is built before this step.

An approval counts only if all of these hold:

- it is newer than the plan comment;
- the plan comment has not been edited since maul posted it;
- the issue still reads as it did when the plan was made;
- for the label, the label is still on the issue.

If the plan comment or the issue was edited, maul refuses the approval and says so. Ask for a new plan, then approve
that one.

**To get a different plan**, say what should change in a comment, then comment `/maul replan` or apply the trigger label
again. You can also put the change in the command itself: `/maul replan keep the public API as it is`. If the planner
asked questions, answer them the same way.

A replan does not start from nothing. The new planner is given the previous plan and the approvers' comments on the
issue, and the request stays the authority on what to build. Each new plan gets the next revision number and needs its
own approval.

`/maul cancel` stops work on the intent and closes the issue.

### Replanning in review

Once the pull requests are open, the approved plan normally stays as it is: review it, revise it with `/maul revise`, or
cancel. With **replan in review** on (`intentController.replanInReview.enabled`, off by default for now), an approver
can also replace the plan while the intent is `InReview` or `Revising`, or `Blocked` from either: comment
`/maul replan <what should change>` on the issue, or apply the trigger label again. maul then:

1. stops any revise round or evaluation that is running (nothing more is pushed for the old plan);
2. withdraws its verdict: the `maul/acceptance` status on each pull request becomes an `error`, "superseded by a
   replan";
3. comments once on each open pull request ("Closed by `/maul replan` from `you` …") and closes it. Its branch is left
   as it is, and its preview comment says a replan closed it. A pull request maul cannot close is named in the reply:
   close it yourself, as maul no longer follows it;
4. replies once on the issue, and writes a new plan from the issue.

If every pull request is merged before maul closes them, nothing is planned again: maul replies saying so, and the
intent ends as merged.

The new planner is given the previous plan, the pull requests' links and states, the approvers' reviews and comments on
them, and what the evaluator found, as context; your note is part of the request. The new plan needs its own approval
(the approve label is taken off before it is posted), and it builds new pull requests on a branch of its own,
`maul-intent/<intent>-p<revision>`. The revision, CI-fix and acceptance-fix limits start again for the new plan; the
intent's cost keeps counting.

maul refuses a replan in review, with one reply, when a pull request of the intent has already merged (what merged is on
its default branch, and a new plan cannot replace it), when the intent is at its cost ceiling (raise
`limits.maxCostMicroUSD` first), or when it has no plan revision left. With the setting off, `/maul replan` in review is
answered as not available, as before.

## 5. The build and the pull request

maul starts a **build run** in the application repository's own agent image, so the agent can build the code and run its
tests. When the build succeeds, maul pushes the changes to the branch `maul-intent/<intent>` and opens a pull request.
The intent is now `InReview`.

- The pull request body says `Part of <intent repository>#<n>`. It carries no closing keyword; maul closes the issue
  itself when the pull request merges.
- maul creates the branch once and never force-pushes it.
- The issue has a **status comment** that maul keeps up to date. It shows where the intent is and, once there is a pull
  request, how many revision rounds have been used ("Revisions: 1 of 3").

If the plan or the build runs out of attempts, the intent is `Failed` and maul removes the trigger label. An approver
can revive it by applying the trigger label again. A revived intent gets a new plan, which needs a new approval.

## 6. Rounds on the pull request

While the pull request is open, maul can run more agent runs on it. Each one is called a **round**. A round starts from
the pull request's current head, runs in the same image as the build, and, when it succeeds, pushes a new commit to the
same branch. The intent is `Revising` while a round runs and goes back to `InReview` when it ends.

Each round posts one comment on the pull request saying what kind of round it was and what it pushed.

### Revision rounds

A revision round is how a reviewer asks maul to change the pull request. An approver starts one in either of two ways:

- **Submit a review with "Request changes".** maul waits until reviews have been quiet for two minutes (and at most ten
  minutes from the first one), so a review written in several parts is read whole.
- **Comment `/maul revise <what to change>` on the pull request.**

The agent reads the approvers' reviews and comments on that pull request. The ones since the last successful round are
its feedback, and the earlier ones are context. Feedback that a failed round never acted on is given again.

`limits.maxRevisions` on the Project bounds revision rounds (3 by default). A round that failed still counts, unless it
failed because it found no usable feedback. When the limit is reached, the next request does not start a round: the
intent is `Blocked` with `RevisionLimitReached` until the limit is raised.

If a round fails, the intent stays in review. `/maul retry` on the pull request retries the round that failed.

Only an approver with write access to the application repository can start a round. A review or command from anyone else
starts nothing.

### CI-fix rounds

A CI-fix round is maul fixing a failed check on its own. It is off until the Project names the checks to fix:

```yaml
spec:
  checks:
    fix: [test] # check-run names or commit-status contexts
```

When a named check fails on a commit maul pushed, an agent reads the check's output and tries to fix it. With no checks
named, maul never fixes one on its own; an approver can still use `/maul revise`.

- `limits.maxCheckFixes` bounds CI-fix rounds (2 by default). They are counted apart from revision rounds.
- **The same failure twice stops automatic fixing.** If a CI-fix round does not fix the failure, the intent is `Blocked`
  with `ChecksFailing`, for a person to look at.
- With `checks.rerunFailed: true`, maul first re-runs the failed GitHub Actions jobs once, so a flaky test that passes
  the second time costs no round. This needs the GitHub App's Actions permission at read and write.

### Working notes

Every build and round is asked to end its report with a `## Working notes` section for the next agent on the intent:
where the code is, which commands worked, what it decided and what it tried and rejected. The next round on the same
repository is handed those notes, so it does not have to find everything again.

[On the pull request](../configuration/intent-controller.md#on-the-pull-request) has the full rules for all of these.

## 7. The evaluator and the hand-off

With the optional [agent evaluator](evaluator.md) on, an agent uses the change before maul tells people it is ready.
Once the pull request's checks are green, an evaluator agent uses the running change through maul's tools and reports,
for each of the plan's outcomes, whether it saw it hold, citing the calls that showed it. maul counts a verdict only
when it cites a call maul recorded that used the application; what the call shows is for the reviewers, who see each
one. A `fail` that cites such a call starts a **verify-fix round**, within its own limit (`limits.maxVerifyFixes`, 2 by
default). The result is posted as the `maul/acceptance` commit status and in comments. Verification never blocks the
intent: a person's review, command or push always wins. [Acceptance verification](acceptance.md) covers turning it on
and the `/maul verify` and `/maul accept` commands.

The **hand-off** is the moment maul gives people the preview link and says whether the change is ready. A
[preview](set-up-previews.md) is the pull request's application running in the cluster at its own address. Without the
evaluator, the link is posted as soon as the preview is live. With it, the default is to hold the link until every
outcome is seen to hold or accepted by an approver, or until maul gives up and says why ("maul can't call this ready:
…"); it waits at most 60 minutes by default. An approver's `/maul preview`, changes-requested review or `/maul revise`
releases the link at once. See [The hand-off](evaluator.md#the-hand-off).

## 8. Merge or close

maul never merges a pull request. A person does.

| What a person does                                 | What happens                                                                       |
| -------------------------------------------------- | ---------------------------------------------------------------------------------- |
| Merges the pull request                            | The intent is `Merged`, and maul closes the issue as completed                     |
| Closes the pull request without merging            | The intent is `Closed`                                                             |
| Closes the issue, or comments `/maul cancel` on it | The intent is `Closed`. Open pull requests are left open for you to merge or close |

A merged or closed intent takes no more commands; to ask for more, open a new issue. The `Intent` resource is kept for
14 days by default and then deleted.

maul never deletes an intent's branch. Delete the branch of a merged intent, or let GitHub delete head branches on
merge. If the same issue becomes an intent again later and the old branch is still there, the new intent is `Blocked`
with `BranchConflict` until someone deletes it.

## Intents that change several repositories

With the operator's multi-repository switch on (`intentController.config.multiRepo: true` in the chart), a Project may
list up to eight application repositories, and one intent can change several of them. Without the switch, a Project that
lists several repositories is not Ready.

What changes for the people involved:

- **One plan, one approval.** The planner reads every repository and names only the ones that must change. The plan
  comment lists them above the plan.
- **One build and one pull request per repository.** Each build runs in its own repository's agent image and is given
  the same approved plan. The pull requests open only once every build has pushed, so a failed build leaves no pull
  request behind. Each pull request says it is one of several, and maul comments on each with links to the others.
- **Rounds run one at a time.** A review or `/maul revise` on a pull request revises that pull request's repository, and
  a failed check fixes its own. The revision and CI-fix limits are counted per intent, not per repository.
- **Merged means all merged.** The intent is `Merged` once every pull request has merged. If one is closed without
  merging, the intent stays in review while any other is open and then ends `Closed`. maul posts a notice of what merged
  and what did not. It reverts nothing and closes no other pull request.

[Several repositories](../configuration/intent-controller.md#several-repositories) has the details.

## When an intent stops

An intent that cannot go on is `Blocked`, and a condition says why: a limit was reached, a check keeps failing, or
something it needs is missing. `Blocked` is not an end. When the cause is fixed, the intent returns to the phase it was
blocked from. [Why an intent is Blocked](../labels.md#why-an-intent-is-blocked) lists the reasons.
