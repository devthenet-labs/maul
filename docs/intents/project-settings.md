# Project settings

The status page can let a Project's approvers change some of its settings themselves: its spend and round limits, each
stage's model and run limits, the evaluator and hand-off, the CI checks maul waits for and fixes, and the machine size
each repository's agent runs get. The settings stay in the Project, where intent-controller reads them, so a save works
exactly as if the operator had changed the Project by hand. Everything else about a Project (where agents are pointed,
its labels, its harness, its previews) stays the operator's.

The page is off by default. The operator turns it on and sets how far approvers may go: see
[Turning it on](../configuration/status-server.md#project-settings-page) for the operator's side.

## Opening the page

With the page on, the board's and the timeline's Project chips link to it (`#/projects/<project>/settings`) for every
Project the page serves. Anyone who may see a Project on the [intents dashboard](dashboard.md) (`get` on
`projects/intents`) can open its page and read every setting, the approvers and their GitHub state, and the facts the
operator set. A Project you may not see answers "not found", as on the dashboard.

## Who can save

Only an approver can save, matched the way `/maul` commands match them:

- your sign-in carries a GitHub account id (the status page reads it from Dex, as the Usage and Account pages do);
- intent-controller has asked GitHub who each login in the Project's approver list is, and one of them is your account;
- that login is still on the Project's approver list;
- GitHub says that account has write access to the intent repository.

No login, name or email from your sign-in is trusted: the match is by GitHub's numeric account id, which a renamed or
re-registered login cannot take over. When you cannot save, the page shows its settings read-only and says why:

| Reason                       | What it means                                                                                                                |
| ---------------------------- | ---------------------------------------------------------------------------------------------------------------------------- |
| no GitHub account id         | Your sign-in carries no GitHub id: another sign-in method, or a session from before the operator set it up. Sign out and in. |
| not an approver              | Your account is not one of the Project's approvers.                                                                          |
| no write access              | You are an approver, but GitHub gives your account no write access to the intent repository.                                 |
| approvers not matched yet    | intent-controller has not yet asked GitHub about every approver, for example one just added.                                 |
| approvers not checked lately | intent-controller has not checked the approvers with GitHub for over 30 minutes, so nobody can save.                         |

intent-controller asks GitHub again whenever the approver list changes and every ten minutes. So a login removed from
the list stops counting at once; an account that loses write access stops within ten minutes; and if intent-controller
is down, or GitHub cannot be read, for more than 30 minutes, nobody can save until it recovers. The page states each
approver's state: matched with write access, without write access, no such GitHub account, or not matched yet.

## What you can change

Each setting says when a change takes effect. A field you clear goes back to its default; for a stage limit, unset means
intent-controller's own ceiling for that stage (see [stage limits](../configuration/intent-controller.md#stage-limits)).

| Card                   | Setting                                                                  | Project field                                    | Range                                                                      | Takes effect                          |
| ---------------------- | ------------------------------------------------------------------------ | ------------------------------------------------ | -------------------------------------------------------------------------- | ------------------------------------- |
| Approvers              | Who may approve plans and save this page                                 | `approvers.logins`                               | 1 to 20 GitHub logins                                                      | at once                               |
| Evaluator and hand-off | Verify each intent with the agent evaluator                              | `verification.enabled`                           | on or off                                                                  | at once, open intents included        |
|                        | Hand-off                                                                 | `verification.handoff`                           | gated, early, or unset (the install's default)                             | intents whose hand-off is not decided |
|                        | Longest wait for the evaluation                                          | `verification.handoffMax`                        | 10m to 4h, default 60m                                                     | at once                               |
|                        | Tool calls per evaluation round                                          | `verification.maxToolCalls`                      | 50 to 300, default 300                                                     | from the next round                   |
| Models                 | Plan, build (revise rounds too), evaluate                                | `models.plan`, `models.build`, `models.evaluate` | a model intent-controller admits, or unset                                 | runs started after the save           |
| Spend                  | Most one intent may cost                                                 | `limits.maxCostMicroUSD`                         | up to $1000, default $10                                                   | at once                               |
|                        | Most one intent's evaluations may cost                                   | `limits.maxEvaluateCostMicroUSD`                 | up to $1000, default $6                                                    | at once                               |
| Rounds                 | Revision rounds                                                          | `limits.maxRevisions`                            | 0 to 20, default 3                                                         | at once                               |
|                        | CI-fix rounds                                                            | `limits.maxCheckFixes`                           | 0 to 20, default 2                                                         | at once                               |
|                        | Automatic verify-fix rounds                                              | `limits.maxVerifyFixes`                          | 0 to 10, default 2                                                         | at once                               |
|                        | Automatic verifications                                                  | `limits.maxVerifications`                        | 1 to 100, default 20                                                       | at once                               |
|                        | Intents running at once                                                  | `limits.maxActiveIntents`                        | 1 to 20, default 2                                                         | at once                               |
| Stage limits           | Turns, tokens, cost and time per run of plan, build, revise and evaluate | `limits.<stage>.*`                               | turns 0 to 1000, tokens up to 100M, cost up to the intent's, time 1m to 4h | runs started after the save           |
| Delegation             | Subagents in build and revise runs                                       | `delegation.subagents`                           | on or off, where intent-controller allows it                               | runs started after the save           |
|                        | Workflows in build and revise runs                                       | `delegation.workflows`                           | on or off, where allowed; needs subagents                                  | runs started after the save           |
| CI checks              | Checks maul waits for and fixes                                          | `checks.fix`                                     | up to 32 check names                                                       | at once                               |
|                        | Longest wait for the checks                                              | `checks.timeout`                                 | 1m to 6h, default 30m                                                      | at once                               |
|                        | Re-run a failed check once before a CI-fix round                         | `checks.rerunFailed`                             | on or off                                                                  | at once                               |
| Machine size           | Each repository's agent size                                             | `repositories[].agentResourceClass`              | one of the sizes the operator defines, or unset                            | runs started after the save           |

Money is in dollars on the page and in micro-USD in the Project ($10 is `10000000`). A stage's cost limit counts only
while the egress broker counts cost; it can never be more than the intent's own limit. Raising the revision limit, the
CI-fix limit or the intent's cost limit past what an intent has used lifts the block that stopped it, and an intent that
was blocked on a `/maul revise` then runs that round. A change to a model, a stage limit or a machine size never changes
a run already started: each run keeps what it was granted at its start.

The evaluator's settings are described with the evaluator itself, in [the hand-off](evaluator.md#the-hand-off) and
[its limits](evaluator.md#limits). Turning verification on is refused while one of the Project's repositories is named
`tester` or `tools`. Re-running failed checks needs the App's Actions write on every application repository; without it
the Project stops being Ready after the save, and the page shows that.

Not on the page, and never changed by it: the intent repository, the labels, a repository's name, URL and preview, the
number and order of repositories, the harness, previews, surfaces, `suspend`, `requireRepositoryImage`,
`verification.manualGatesStatus`, `verification.postLogs` and `verification.timeout`. The check reviewer's model and
limits (`models.checkReview`, `limits.checkReview`) are not on it either. The "Set by the operator" card shows the ones
a person may need to know.

## Delegation

The Delegation card holds the Project's opt-in to delegation in its builds. With **Subagents in builds** on, each new
build and revise run (CI-fix and verify-fix rounds included) may hand parts of its work to the claude CLI's subagents,
in the foreground, on the run's own model, under the build's own tools. With **Workflows in builds** on as well, it may
also run the CLI's inline and built-in workflows. Workflows need subagents: turning subagents off turns workflows off
with it, and the page refuses workflows alone. Plans, the evaluator and the check reviewer never delegate.

Every subagent and workflow agent spends from the run's own cost budget, which is what bounds the run and everything it
starts together; a workflow's agents count against neither the turn limit nor the token limit. Size the build and revise
cost limits for what one run may spend with its subagents, not on its main thread alone. The operator's guide has the
details and the two accepted limits of workflows:
[Subagents and workflows in builds](../configuration/intent-controller.md#subagents-and-workflows-in-builds).

Each run records what it was granted when it is created, so a change applies only to runs created after the save. A run
created before it keeps what it recorded, even one still waiting to launch (for a free slot, its repository or a
credential): turning a switch off does not reach it, and neither does turning one on.

A switch is locked, with the reason beside it, while intent-controller does not allow it: the operator has not turned it
on for this install, or the Project runs on codex, whose runs never delegate. A switch that is on is never locked by
that, so you can always turn it off; while it is not allowed it grants nothing, and the page says so.

## What the operator allows

The operator sets bounds the page cannot pass. The page shows each one beside its setting, and refuses a save that
crosses one; the cluster refuses it again, so no change to the page can get past them.

- **Ceilings.** Six limits intent-controller does not bound itself have a ceiling: the intent's cost, the four round
  limits, and intents running at once. You may raise one up to its ceiling. A value the operator set higher may be kept
  or lowered, never raised from the page.
- **intent-controller's own ceilings.** A stage's turns, tokens and time, the evaluation cost and the tool calls cannot
  go above what intent-controller grants (shown as the placeholder). Only the models it admits can be chosen, and only
  the machine sizes it defines.
- **Floors.** The evaluator's limits have floors, because an evaluator starved of turns, time or money ends every round
  unconfirmed, which is the evaluator turned off by another name: the evaluation cost (it also holds under the evaluate
  stage's own cost), the evaluate stage's turns, tokens and time, the tool calls, the automatic verifications and the
  hand-off wait. You may lower one down to its floor. A value already lower may be kept or raised.
- **Switches.** Three changes need the operator's permission, each off unless the operator turns it on:
  - changing the approver list;
  - turning the evaluator off, handing previews over early (or going from gated to the install's default, which may be
    early), or lowering an evaluator limit under its floor;
  - emptying the CI checks, so maul waits for and fixes none.
- **intent-controller's switches.** A delegation switch can be turned on only where intent-controller allows it (see
  [Delegation](#delegation)).
- **Locks.** The operator can lock any group of settings (approvers, evaluator, hand-off, models, spend, rounds, intents
  running at once, stage limits, delegation, CI checks, machine size). A locked group is read-only.

The chart's defaults, which your operator may have changed (the page shows the ones in force):

| Ceiling                 | Default | Floor                              | Default    |
| ----------------------- | ------- | ---------------------------------- | ---------- |
| Most one intent costs   | $25     | Most one intent's evaluations cost | $2         |
| Revision rounds         | 10      | Evaluate turns                     | 40         |
| CI-fix rounds           | 10      | Evaluate tokens                    | 100,000    |
| Verify-fix rounds       | 5       | Evaluate time per run              | 10 minutes |
| Automatic verifications | 50      | Tool calls per round               | 100        |
| Intents running at once | 5       | Automatic verifications            | 3          |
|                         |         | Longest wait for the evaluation    | 30 minutes |

## Saving

The page saves everything at once. Change what you need, then **Review changes** lists each change, old value to new,
with when it takes effect and any warning about another tool that also writes it (below). **Save** sends it.

- **Confirmations.** Some changes need a second click that says what they do: adding or removing an approver, turning
  the evaluator off, an early hand-off, a limit under its floor, and emptying the CI checks. The ones that weaken
  verification count the Project's open intents.
- **Approvers.** You may add or remove one approver per save, and never remove yourself (another approver can). A new
  approver counts only once GitHub reports their write access to the intent repository; intent-controller asks as soon
  as the list changes, and until it has, they see "approvers not matched yet". Approvals already recorded stand when an
  approver is removed.
- **Refused.** A value the page or the cluster refuses is marked beside its setting with the reason: out of range, above
  a ceiling, under a floor, a model not admitted, a locked setting. A refusal of the save as a whole (for example the
  cluster's) is shown above the cards. Nothing is saved when anything is refused.
- **Someone saved first.** If the Project changed since you opened the page, the save is refused and the page reloads
  the Project, keeps your edits on top of it, and points out any setting the other change touched too. Review and save
  again.
- **After the save.** The page waits for intent-controller to read the new settings and then shows the Project's state:
  Ready, or why not. A save that changes nothing writes nothing.

The page takes at most ten saves a minute from one person.

## Settings another tool also writes

A Project is usually written by the operator's tooling too: the maul-config chart's `projects` values with Helm, or
`kubectl apply`. Both the page and that tool then write the same object, and Kubernetes records which of them set each
field. The page shows every other writer of a setting beside it, and in Review changes says what that writer's next run
does with your change:

- **A Helm upgrade (server-side apply, Helm 4's default) of a setting its values state.** The next `helm upgrade` of
  maul-config stops with a conflict on that field, whatever else the operator changed, until the values file says the
  same as the page, or the upgrade is forced, which puts the values file's setting back.
- **A list Helm's values state (approvers, CI checks).** An entry you add here stays. An entry you remove here that the
  values file still lists **comes back** on the next upgrade, with no error and no warning.
- **`kubectl apply` or client-side Helm.** Its next apply that sets the field puts its own value back, and replaces a
  whole list with its own.

!!! warning "Removing an approver the values file lists"

    A login removed on the page while the operator's values file still lists it is back on the next `helm upgrade` of
    maul-config, silently, and counts as an approver again. Ask the operator to remove it from the values file as well.

So set each setting from one place: leave it out of the values file to manage it here, or change it in the values file
and not here. **Copy as values** turns the page's settings into a `projects` entry for the maul-config values file, to
hand to the operator when a change made here should stay. It lists every setting the Project states, not only the ones
you changed: whatever the values file then states, Helm writes, and a later change of it here stops the next upgrade
again. The operator's side of this is in
[set each field from one place](../configuration/status-server.md#set-each-field-from-one-place).

## What is recorded

Each save records who made it on the Project itself (the `maul.run/settings-edited` annotation: your GitHub id and
login, the time and the settings changed), and the page shows it as "Last changed here by". The status server also logs
every save and every refusal with the person, the Project, and each change's old and new value.
