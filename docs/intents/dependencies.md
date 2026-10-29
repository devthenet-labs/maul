# Intent dependencies

An intent can depend on work that has not landed yet. Mark that on GitHub: open the intent's issue, and in its
**Relationships** sidebar add the issue it needs as **Blocked by**. maul then waits. It does not plan the intent until
every issue it is blocked by is closed as completed, so the plan is written against code that already holds what it
needs.

## What waits, and how you see it

When an approver applies the trigger label to a blocked issue, maul accepts the trigger as usual, then holds the intent
in `Pending`:

- it starts no agent Job and holds none of the Project's active-intent slots (`limits.maxActiveIntents`);
- its status comment on the issue says it is waiting and lists each blocker, with the command that overrides the wait;
- the intents dashboard shows a **waiting** badge on its card;
- `kubectl describe intent <name>` shows the `WaitingOnDependencies` condition and `status.dependencies`.

A trigger from someone who is not an approver is still refused at once. It never waits.

## What releases it

The intent starts planning once every blocker is closed as **completed**. An issue closed with no reason counts as
completed: that is how GitHub records issues closed before it kept reasons.

| The blocker is                                                      | The intent                                                              |
| ------------------------------------------------------------------- | ----------------------------------------------------------------------- |
| Open                                                                | Waits                                                                   |
| Closed as completed (or with no reason)                             | No longer waits for it                                                  |
| Closed as not planned, or as a duplicate                            | Keeps waiting: what it was for never landed. The status comment says so |
| Another intent, merged                                              | No longer waits: maul closes a merged intent's issue as completed       |
| Another intent, cancelled or closed with its pull requests unmerged | Keeps waiting: maul closes that issue as not planned                    |
| Another intent that failed                                          | Keeps waiting: a failed intent's issue stays open                       |

When another intent of the same Project ends, every waiting intent of that Project looks again at once, so a chain of
intents (C blocked by B, B blocked by A) plans one after another within seconds of each merge. A blocker you close by
hand is noticed on the intent's next poll, every five poll intervals (five minutes by default).

A released intent still needs a free slot. If the Project already has `maxActiveIntents` intents under way, it waits for
one of them to finish (the status comment says "waiting for an active slot"), and the oldest waiting intent goes first.

## Starting anyway: `/maul start`

An approver comments `/maul start` on the issue to start the intent without waiting. maul records who started it, and
the status comment keeps that as history. The run is still attributed to the person whose trigger created the intent.
`/maul start` works only while the intent is `Pending`; it is also the way out of a blocker closed as not planned, of a
blocker maul cannot read, and of a dependency cycle. You can also remove the dependency on GitHub.

## A blocker added later

Only the first start waits. If someone adds a blocker after planning began, maul does not stop or pause the intent: its
plan and pull requests cannot include work that has not merged. It posts one notice on the issue and lists the blocker
in the status comment. Comment `/maul cancel` to stop, or, while the plan awaits approval, `/maul replan` once the
blocker is closed. A replan, and the revival of a failed intent, never wait on dependencies.

## Blockers in other repositories

A blocker may be in any repository. maul reads the issue's dependencies with a token for the intent repository alone, so
GitHub may count a blocker elsewhere without showing it. maul treats such a blocker as **unreadable**: the intent waits,
and the status comment says how many blockers maul cannot read. Install the GitHub App on those repositories, remove the
dependencies, or comment `/maul start`.

The status comment names a blocker's repository only when it is the intent repository or one of the Project's
repositories. Any other repository is counted ("1 issue in another repository"), never named, so a private repository's
name never appears on a public intent issue.

## Cycles

If two intents of the same Project block each other, neither can start. Both wait, and both status comments say
"Dependency cycle" and name the other issue (or the whole cycle, for a longer one). Remove one of the dependencies, or
comment `/maul start` on one of the issues: it plans, and once it merges the other is released.

## When GitHub cannot answer

| GitHub answers the dependencies read with                          | The intent                                             |
| ------------------------------------------------------------------ | ------------------------------------------------------ |
| 404 or 410 (a GitHub Enterprise Server without issue dependencies) | Starts at once, without waiting                        |
| 403 (the App may not read them)                                    | Starts at once, without waiting                        |
| A server error, a timeout or a secondary rate limit                | Waits and reads again; starts after 30 minutes without |

maul never waits forever on an error. The 30 minutes are counted from the first failure, recorded on the Intent, so a
restart of the controller does not start the count again. In every one of these cases the intent's status comment says
it started without its dependencies, and the Project's `DependenciesUnavailable` condition says why. An intent that
started this way gets no "added later" notice: maul cannot tell which of its blockers were there all along.

## Turning it off

Dependencies are honoured by default. An operator turns them off with the chart value
`intentController.dependencies.enabled: false` (the controller's `--intent-dependencies=false`); see
[intent-controller](../configuration/intent-controller.md#intent-dependencies).
