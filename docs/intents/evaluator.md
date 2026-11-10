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
Switched off as a rollback, an open intent whose approved plan states outcomes is not verified at all (state `Off`): its
`maul/acceptance` becomes the final error "verification switched off", never a green, until the evaluator is back (maul
starts over at the current heads) or a plan stating criteria is approved.

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
| The check reviewer      | a `checkreview` IntentRun, one per round                                | The round's new checks as maul renders them; the outcome texts, the issue line each answers and its kind; the request, as the evaluator gets it (on the green question only); and the previewed components. Never the application, a step's name or the evaluator's notes and verdicts. Reaches only the broker         |
| Log and event capture   | preview-controller                                                      | Bounded, redacted log tails of each component and the slot's pod events since the round began, handed to `tools`. Neither the evaluator nor the tool pod has any Kubernetes access                                                                                                                                      |

A Project with no preview but a command-line program or a Go library has its tool pod in the agents namespace instead:
see [Without a preview: a CLI or a library](#without-a-preview-a-cli-or-a-library).

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
   `maul-tool check run` first: `tools` refuses to confirm a check whose digest has no green dry run. A check proposed
   to show a defect (`expect: fail`) needs a dry run red by assertion instead, and none green (see
   [A defect found in round 1](#a-defect-found-in-round-1)). The report's frontmatter is decoded strictly (an unknown
   key refuses it): a verdict names its outcome as `outcome` (the older `id` is still read; both, differing, are
   refused), and a proposal names the check `file` it ran (or the `digest` that run printed). agent-runner reads the
   digest from that file, under the evaluator's scratch directory, and asks `tools` to confirm it; a proposal whose file
   does not resolve to a check of its id and outcome is dropped, with its reason in the run's transcript, never the
   whole report. A refused report's detail lists its frontmatter's key paths (never their values), and each repair
   message restates the exact frontmatter example and the decoder's lines. The evaluator is shown the reviewer's rule
   word for word and works in two passes: first a covering check for every outcome a person does not judge, then
   strengthening each against a list (every clause asserted, inputs other than the defaults and away from what a new,
   empty item shows, both sides and both sides of a bound, before and after, nothing changed for a claim that something
   stays, nothing the outcome does not state, the left-out item created and asserted absent beside a kept one, a whole
   compared whole), and last naming the wrong builds a careless change could ship and finding the assertion that would
   go red on each, within a turn budget per outcome. The reviewer's rule names the checks a plausible wrong build would
   still pass (S1 demo: every outcome left unconfirmed on a correct build had one). An outcome it leaves unsure with no
   check names the clause it could not exercise. It does not run again a saved check that replayed green.
6. **Look-around.** After the outcomes, the evaluator visits the routes the change touches (at most 8): a snapshot and a
   screenshot each, the console, then each component's logs and the slot's events for errors, panics, restarts or OOMs
   since the round began.
7. **Confirm (no model).** After the evaluator exits, `tools` runs each proposed check again with a run token of its
   own, alone, in a fresh context, and records the run. That ledger entry, never the evaluator's report, is what maul
   believes.
8. **Review.** One `checkreview` run per round reads each new confirmed check, opening a fresh session per check, and
   answers `proves`, `does_not_prove` or `unclear` for its outcome. Answers are cached by the check's and the outcome's
   digests (and whether the red question was asked), so a check is reviewed once. A check reviewed `proves` is stored;
   any other is dropped. On the green question the reviewer is given the issue line the outcome answers, its kind, the
   request and the previewed components, and learns the application's own words from the request (a "beat" on the page
   may be a "pattern" in the API), but judges the check against the outcome only: nothing in the request relaxes the
   rule. The red question gets neither the request nor the line, so a fix never starts outside the approved outcome.
   Each answer is kept with the check's id and the reviewer's reason, and every later round's evaluator is told the
   newest 40 checks refused on the green question by outcome, id and reason: a form once refused is never reviewed
   again, so it strengthens the check rather than proposes it again. Each session may take 6 turns, the last 3 kept for
   one repair of its report; a review whose report is refused and not repaired makes that check alone `unclear` (not
   stored, its answer not cached), and the run goes on to the next check.
9. **Collect.** preview-controller reads the `tools` container's log (that container only), holds it to the count and
   sha256 in its termination message, writes an index of every entry to the request's `status.ledger`, and keeps the
   full ledger and the cited screenshots (re-encoded by maul) in ConfigMaps the request owns.
10. **Judge.** maul decides each outcome by the rules below, then routes the round: all confirmed, no anomaly, a clean
    preview: **ready**. A failure the evaluator agrees with: a verify-fix round. A dispute: one model-free replay round.
    Anything else: **maul couldn't confirm this**. A failing environment keeps the round from ready even when every
    outcome passed, and no later acceptance by an approver changes that at the same heads. A replay round whose `tools`
    finished before its replay's end (its wall clock, or its container told to stop) is maul's trouble (`Infra`,
    `replay-incomplete`), never judged.
11. **Tear down.** The tool pod, its key, its ConfigMap and its ingress policy go when the request settles.

## What makes a pass

An outcome is **confirmed** only on a check bound to it that:

1. maul stored, is not retired, and is one of maul's forms: an HTTP step list, a browser check in maul's JSON grammar
   (compiled by maul to a Playwright test), or a sandbox program whose output `tools` asserts on;
2. is **substantive**: at least one step acts on the application and at least one assertion is about content (a status
   or exit code alone is not), and every list assertion is filtered to data the check created itself, named with
   `{{run}}`. A check over shared data (a global ranking, a global count) is refused when it is saved. A browser check
   that writes nothing and asserts only the layout of the page's fixed markup, at least once by a measure (a size, an
   alignment, a box relation, legibility), on elements it finds by role, label, test id or CSS rather than by their text
   or position, needs no data of its own, and the reviewer's green question says so; its failure is always held for a
   person;
3. passed in this round's confirmation or replay, run by `tools` at the current heads with a token `tools` made;
4. was reviewed `proves` for this outcome's text (a check saved red: on the red question);
5. and no other live check of the outcome failed this round;
6. and the evaluator did not say it saw the outcome fail this round: a `fail` verdict, or a verdict listing `contrary`
   ledger seqs, where it saw the outcome as worded not hold, whatever it believes caused it. A failure that was there
   before the change still counts: a check that passes only by leaving out what failed tests less than the outcome. maul
   remembers every check that passed in such a round (`status.acceptance.narrowed`): from then on none of them confirms
   that outcome, neither in the replay round at the same heads, which has no evaluator, nor in a later round whose
   evaluator says nothing contrary. The outcome is confirmed only on another check, or by an approver.

And every saved check of the outcome that the round was to replay has a replay run at these heads. One that has none
(the round was cut short before it) may be the very check that fails, so the outcome is unsure with the reason
`not-replayed`, whatever its other checks show.

A browser check never names a snapshot ref or a colour literal. Its `visible` assertion means visible to a person (in
the viewport, a non-zero box, opacity above 0.1, text with at least 1.5:1 contrast against what is painted under it),
not merely present in the page; `legible` is WCAG's 4.5:1 (3:1 for large text); `clickable` means the element itself
receives the click. A background maul cannot judge (an image, a paint effect) makes such an assertion indeterminate: the
check errs, which is unsure, never a fail. Its `size` assertion takes an exact `width` or `height`, within the
assertion's `tolerance`, or bounds: `minWidth`, `maxWidth`, `minHeight` and `maxHeight`, the same tolerance as slack. An
axis takes an exact value or its bounds, never both, and a minimum is never over its maximum. So an outcome that says
"at least 48 px" is checked by `minHeight: 48`, never by an exact 48, which would fail a correct 56 px pad and which the
reviewer refuses. The box relations (`left-of`, `above`, `no-overlap`, `equal-size`, `aligned`) and `size` measure what
is drawn, where the page left it (the check's own scrolls are kept): each box is cut by every ancestor whose overflow
clips it and by the page's extent (the viewport, for a fixed element). `left-of`, `above` and `no-overlap` judge the
drawn parts; `equal-size`, `aligned` and `size` (exact or bounded) judge the whole box, and a fail on a box partly
clipped is indeterminate. An element clipped away entirely, scrolled out of its container say, is not drawn, so a
relation on it is indeterminate, never a fail. A saved check is at most 8 KiB as maul stores it, and an intent keeps at
most 40.

Everything else:

| Situation                                                                                     | The outcome                                                                                                                                     |
| --------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------- |
| A check failed, and the evaluator reports `fail` for that outcome citing the ledger           | **failed**: a verify-fix round with a reproduction (a check that writes data of its own run; else unsure, a suspected defect a person confirms) |
| A check proposed to show a defect, red by assertion in maul's own run, reviewed yes           | **unsure**, a suspected defect a person confirms (see [A defect found in round 1](#a-defect-found-in-round-1))                                  |
| A check proposed to show a defect that maul's own run did not see red by assertion            | **unsure**: never failed, and never passed by that run                                                                                          |
| A check failed, and the evaluator **disputes** it                                             | **unsure** this round (see [Disputes](#disputes))                                                                                               |
| A check failed, and the evaluator neither agrees nor disputes                                 | **unsure**: "a check failed, but the evaluator did not agree that it is a defect"                                                               |
| The evaluator says it holds (or fails), but no check maul re-ran shows it                     | **unsure**: no pass and no fix without a check                                                                                                  |
| A check passed, but the evaluator saw the outcome as worded fail (`fail`, or `contrary` seqs) | **unsure**: "a check passed, but the evaluator saw the outcome as worded fail"; never failed by that alone                                      |
| The only support is a check that passed only on its retry                                     | **quarantined**                                                                                                                                 |
| A check failed while preview-controller saw a component crash, get evicted or OOM             | **unsure**, the preview's failure; never a fix                                                                                                  |
| maul's own machinery failed (a tool pod never ready, a ledger that does not hold)             | the attempt is retried; past its bound, unsure                                                                                                  |
| A manual outcome                                                                              | only a person closes it, with `/maul accept <id>`                                                                                               |

## A defect found in round 1

A check saved green can show only a regression: a defect present from the first round would fail its check, and a check
with no green dry run could never be proposed. So the evaluator may also propose a check that **shows a defect**: the
check of the outcome as approved, which would pass where the outcome holds and fails on this build. It proposes it with
`expect: fail`, under an outcome it reports `fail`.

**maul never fixes a defect found this way on its own.** A wrong fail starts a fix of code that works, and four
independent reviews kept finding ways a correct build could still look red, so such an outcome is judged **unsure**, a
_suspected defect_, and the commit status and the hand-off treat it as unsure. maul holds the gated hand-off (the note
says so only while it is held: once released, by a person or `handoffMax`, it no longer claims a hold) and shows the
evidence on the issue: the check it saved, maul's reproduction and that the evaluator reported the outcome fails, saying
maul believes this is a defect and a person should confirm it. The evidence stays on the issue whatever else the round
found: beside another outcome that is failing, too. An approver's `/maul verify fix` then starts the fix round exactly
as an automatic one would, handed that reproduction and any failure of a check saved green the round found beside it;
the commit status and the status comment then say a person confirmed the defect and a fix is running. Only a
`/maul verify fix` created after maul recorded the suspected defect confirms it: one written earlier (say, while the
round was still being evaluated) keeps its earlier meaning, a request that re-arms automatic fixes, so no fix starts
before a person can have seen the evidence. `/maul verify` and `/maul verify fix --reset` never confirm it; nor does
anything once the pull requests' heads have moved; and while it waits, the hand-off's own time limit still releases the
link. A regression of a check saved **green** keeps starting the automatic fix as before, but only for a check that
writes data of its own run (`{{run}}` in a POST, PUT or PATCH, a browser fill or its program). A saved check that writes
none reads whatever the application shows (a page's layout, shared rows), so rows other runs made can move it: its
failure, green or red, cleared or not, is a suspected defect held for a person exactly as below, never an automatic fix.
What may be saved green is unchanged. Every one of these must hold before a defect is even suspected:

1. **Red by assertion.** The check's dry run failed one assertion the outcome is the judge of, about data the check's
   own run made, in a check that otherwise ran: every step before passed, and both attempts failed the same assertion of
   the same step. When in doubt maul counts nothing: unsure beats catching a defect. What counts, by form:
   - `http`: only content: a JSON pointer (never the whole document) that resolves to a different value, not by a list
     matcher (`$order`, `$includes`); a header that is present with a different value; or body text missing from a body
     that is not an HTML page. And only when the step reads the run's own data: its path (not its query) names `{{run}}`
     or a value captured from one of the check's own writes (a POST, PUT or PATCH whose path, less its query, or whose
     body names `{{run}}` or a value captured from an earlier such write, so a battle of the check's own patterns is its
     own too; a JSON body by a value, never by an object key, which `{{run}}` is never substituted into), or the
     expected value names one. No status counts, a 500 included, nor a pointer or header that resolves to nothing, a
     total or a count over shared rows, HTML where JSON was expected, no answer, or a capture with nothing to keep. The
     check may list only success statuses.
   - `exec`: only a program that says so itself, printing `{{marker}} <id>: <what it saw>` at the start of a line of its
     output (stdout and stderr combined), `<id>` naming the assertion (lowercase letters, digits and hyphens, starting
     with a letter, at most 128 characters). maul replaces `{{marker}}` in the program's files, and nowhere else, with a
     line start drawn afresh for every attempt (`MAUL-ASSERTION-FAILED-` and 32 random hex characters), so text the
     application answers with that the program echoes (a body, a traceback) can never pass for one; only lines carrying
     that attempt's own marker count, the first with a valid id naming the assertion. A line that starts with the marker
     fails the check whatever its exit code and its expectation, so a program that prints it and goes on to exit 0 is
     never a pass. A run whose output is cut at its 64 KiB bound is never a pass either (a marker past the cut would be
     lost): it errs, unless a marker was kept. An exit code alone, a crash, `127` or a traceback never makes a check red
     by assertion. Neither does a marker line with no valid id after it: the run fails, and its detail says the marker
     named no valid assertion id, so the evaluator can rename it. On a valid id, what the program printed after its
     marker (`<id>: <what it saw>`, each such line, the marker itself left out, at most 1 KiB) is kept in the failing
     step's message, so the issue, the pull request and the reproduction show the value the program observed, as the
     application's data.
   - `browser`: an `expect` step's maul assertion or Playwright matcher on an element that was there, its locator, text
     or URL keyed by `{{run}}`, or an assertion that measures the page's layout (`size`, `left-of`, `above`,
     `no-overlap`, `equal-size`, `aligned`, `legible`, `clickable`, `visible`), keyed or not; never a count, a timeout,
     an action, a locator that resolved nothing or more than one element, an element with no box, a trial click or an
     indeterminate judgement. The failing step is read from the compiled spec's own stack frame. maul's own runs of such
     a check (its confirmation, and its replays once saved) wait 15 seconds for each judgement instead of 5, so a slow
     but correct page is not read as the defect.

   `maul-tool` prints `red by assertion at step <n> (<assertion>)` for such a run. The check must also write data of its
   own run: `{{run}}` in a POST, PUT or PATCH (its path or body; in a JSON body, a value, never an object key), a
   browser fill or its program. The one exception is a browser check whose failing assertion measures the page's layout:
   it may write nothing, since a read of the page's own layout has no shared data to be wrong about, and a person
   confirms what it finds (a pad 30 px tall where the outcome says at least 48, rows that scroll apart). Its reviewer is
   asked whether the element it measures is the one the outcome is about on any data. A check that breaks these rules is
   refused `red-check-rule`, its reason in the ledger; `no-red-dry-run` means only that no dry run failed an assertion,
   or one ran green.

2. **The expectation is the proposal's.** `tools` confirms a check proposed `expect: fail` only against a dry run red by
   assertion, never one that also ran green, and never a saved check this round's replay saw green (as it confirms a
   green proposal only against a green dry run, never one the replay saw red).
3. **The red question.** The reviewer is told the check failed, at which step and by which assertion, and that the
   application's state was not fresh. It answers `proves` only if the check encodes the outcome as approved, would pass
   on a build where the outcome holds, and that assertion is the outcome itself and about data the check created, not
   setup or something incidental. For an `exec` check it is shown the marker its program printed and answers whether the
   program prints it only when the approved outcome is violated, never inside a retry or poll before its final
   judgement, on a connection error, a status code, a timeout or an exception. The answer counts only for that
   assertion: another failing assertion of the check needs a review of its own. Anything else and the check is not
   stored; the outcome stays unsure.
4. **maul's own run.** After the evaluator exits, `tools` runs the check again alone, in a fresh context, under a fresh
   run token per attempt; only a run red by the same assertion as the dry run makes the outcome **failed**. A run that
   passes, fails then passes, errs or fails some other way leaves it unsure, and never passes it.
5. **No vetted check disagrees.** If another check of the outcome, valid and reviewed `proves`, passed cleanly in the
   same round, the two disagree: the outcome is unsure. A check maul recorded as narrowed in an earlier round
   (`status.acceptance.narrowed`) is not such a check: it tests less than the outcome, so its pass says nothing against
   a failure, and the failing check is judged by the rules above alone. The round that records the narrowing still
   counts it, so that round is judged alike every time. A check that both passed and failed at the same heads in one
   round is flaky: unsure, never failed. Both hold for every check, not only red ones.

The check is then stored (marked as saved red, with the assertion it failed), and once an approver confirms the defect
the verify-fix round is handed its failure as the reproduction. After the fix, the same check replays at the new heads;
green, it is asked the green question too (the red review never asked whether a pass could be special-cased or merely
shows a call worked), and only once that answer is `proves` does it pass the outcome. A replay that clears it counts
from the next round on, so the round and its fix are decided on the same facts. Once cleared and reviewed `proves` with
the green question, it has been proven against a working build: from then on it is a saved green check, and a later
regression of it starts the automatic fix, with no person's confirmation, when it writes data of its own run. A layout
read that writes nothing is never promoted: like every check that writes no run data, its failures always wait for a
person, cleared or not. Until it has replayed green once, its later steps and its other assertions have never run, so a
replay or a new confirmation that fails other than by that same assertion is not evidence: never a second fix, never a
block. Another proven check may pass the outcome, and a dispute with a proven replacement can still retire it. A check
saved red that already replayed green and is proposed red again is unsure. Where a base is available, a check of a new
outcome that fails at base too is expected (the feature did not exist), and is not a reason to refuse it.

Known limits, which is why a person confirms: maul's own run happens on the preview the exploration used, so a check
whose failing assertion reads state the evaluator left behind could go red deterministically (the write rule, the rule
that the failing assertion reads the run's own record, a capture counting as the run's own only from a write that names
`{{run}}`, rule 2's run-scoped list assertions, the reviewer and the red question narrow it; they do not close it). An
http or exec check reads right after it writes, with no patience, so an application that updates asynchronously or
serves a cached read for a few seconds can fail the same way on both attempts. A `clickable` check judges one moment, so
a toast or spinner passing over the element can too. An exec program's marker is its own word: one printed from a
catch-all on a connection error would look like a defect; the reviewer is asked about exactly that. A layout read that
writes nothing measures whatever the page shows: an element whose size or place depends on rows other runs made (a list
that grows, a long name from another run) can measure wrong; the reviewer is asked about that, and a person confirms.

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
  failed. For a check saved red, maul's failing run comes after the evaluator exits, so its window is that of the red
  dry run it confirmed.

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
watchers; maul only holds its own words. A released link is labelled "Ready" only while the pull requests are at the
heads maul judged: after a push it reads as still being checked until a round is about the new heads.

"maul couldn't confirm this" names one reason:

| Reason                                                                         | Meaning                                                                                 |
| ------------------------------------------------------------------------------ | --------------------------------------------------------------------------------------- |
| some outcomes are unsure                                                       | no check proves them yet; read each outcome's line                                      |
| the evaluator judged 2 outcomes not to hold, but no check maul re-ran shows it | maul proved no failure: the outcomes are unsure, never failed; read each outcome's line |
| an outcome fails and maul stopped fixing it                                    | a reproduced failure, and automatic fixing halted (out of fixes, a repeat, a human)     |
| it noticed something odd while it looked around                                | a look-around anomaly                                                                   |
| it used every round it may run                                                 | `maxVerifications` is spent                                                             |
| it spent its evaluation budget                                                 | the intent's evaluation ceiling is reached                                              |
| it ran out of time                                                             | `handoffMax` passed                                                                     |
| its sandbox was not enforced                                                   | the evaluator's strict probe found DNS, the API server, metadata or the internet open   |
| the preview failed while it was checked                                        | a component crashed, was evicted or ran out of memory during the round                  |
| it could not run its checks                                                    | maul's own machinery failed every attempt                                               |

The outcomes in doubt lead: with an outcome neither confirmed nor accepted, an anomaly is named after the reason ("...,
and it noticed something odd while it looked around"), never in its place. When every outcome was confirmed or accepted
and only an anomaly or a failing environment kept the round unconfirmed, the commit status and the progress line name
that cause instead of reading as all confirmed, for example "maul couldn't confirm this: 3/3 outcomes confirmed, but it
noticed something odd while it looked around".

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

- **On GitHub.** The verification comment and each pull request's sticky comment list every outcome once, with its kind
  (`change` or `keep`), its result (`confirmed`, `failed`, `unsure: maul couldn't confirm it`, `quarantined`,
  `accepted by a human`), why maul could not confirm it and, for a decided one, **maul's check** in one line it renders
  itself, such as `http to api: POST /api/players, POST /api/battles, GET /api/leaderboard; 3 expectations`. Application
  output appears only in fenced blocks labelled as the application's output of the outcome they name, and the
  evaluator's own words (an anomaly, a note) are marked as its words, not checked by maul. Results are shown only for
  the round that decided them, at the heads it checked: a round that settles without a judgement (its attempts all
  failed, or maul's own build of the head did not finish) lists every outcome as `unsure`, with no earlier round's
  results, and a push clears them until the new heads are judged: from the pass that sees the push, the status comment
  and the sticky comments read "maul is checking the new commits" with no result, and the last round's verification
  comment is left as it stood, about the heads it checked.
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

## Without a preview: a CLI or a library

A Project with no preview is evaluated too when it says what to build: a command-line program (`surfaces.cli`) or a Go
library (`surfaces.library`). The evaluator is the same run, judged by the same rules; what changes is where its hands
are and one rule more. intent-controller runs the round's tool pod itself, in the agents namespace, and that pod builds
the pull request **twice**, in the repository's accepted runner image: at its head, and at its **base**, the merge base
of the head and the repository's default branch (read from GitHub when the round starts, so a merge of the default
branch into the pull request moves it). Every check runs at both, and a check of a `change` outcome counts only if it
tells the change from its absence.

It is off by default: chart `intentController.verification.agents.enabled` (intent-controller
`--intent-evaluator-agents`), beside the evaluator, with repository images on; previews need not be. Off, such a Project
carries `VerificationOff` (reason `AgentsToolPodNotEnabled`) and its plans are not asked for outcomes; switched off
under open intents, their views stay link-less and say "maul couldn't confirm this", never offering a preview. The
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
  pull request declares. A repository with no accepted image is never evaluated: the round settles "maul couldn't
  confirm this: the repository has no accepted runner image to build the pull request in", never a block, and while
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

| Command                                         | Does                                                                                                                                                                                                                                              |
| ----------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `maul-tool exec [--base] [--file f]... -- argv` | Runs a program in the head's sandbox (or the base's), in a fresh scratch directory of its own; `{{bin}}` (the built binary) and `{{tree}}` (the tree's writable copy, `/work/tree`) are substituted                                               |
| `maul-tool files ls`, `grep`, `read` `[--base]` | Looks at the tree as fetched, never as a build rewrote it: a listing, a literal-substring search (at most 200 hits) or the first 64 KiB of one file. Nothing reaches outside the tree                                                             |
| `maul-tool run <dir> [--base] [-- args]`        | Compiles the Go program in `<dir>` (at most 8 `.go` files at its top, no subdirectories) against the side's tree, then runs it: how a library is used, or a checker that reads what the binary wrote                                              |
| `maul-tool check run [--base] <file>`           | A dry run of a check the evaluator wrote, as on a preview. A cli check runs the binary (an `exec` whose `argv[0]` is `{{bin}}`, or a `run` whose program names it), a library check is a `run`, and each asserts on its standard output's content |

A `run` program is compiled offline against the tree's own module graph: maul writes a minimal `go.mod` (module
`maulcheck`, the tree's `go` line) beside the program and makes a `go work` workspace of it and the tree, then builds
with no module proxy, the module files read-only and no user `go env` file, so the compile never reaches the network and
never changes the tree's `go.mod` or `go.sum`. A tree with a `vendor` directory, a `replace` by an absolute path or a
directory outside the tree, or no `go.mod` maul can read refuses every `run` by name. Each exploratory call gets a fresh
scratch directory, but `HOME` and the writable tree are shared by every call at that side while the evaluator explores.
maul's own runs of a check (a dry run, a replay, a confirmation, at either side) get a fresh scratch directory and run
token each, and start from the tree, the build output and `HOME` exactly as that side's build left them: after the build
the sandbox keeps a copy of the three (at most 768 MiB) and their digests, and before each check run it puts them back
unless they still match. The caches (Go's build cache, npm's) are too large to copy: the sandbox records each entry the
build left (its type, mode, size, inode and change time, which no program can set) and before each check run removes
every entry that is not exactly one of those, so a check sees only the build's own entries, and one deleted stays
deleted, a cache miss, never another answer. Each check run also gets an empty `TMPDIR` of its own. What exploration or
an earlier check changed there never reaches a check. When a copy itself no longer matches its digest (or the build's
output could not be kept) the check run does not run: it is an error, which can never pass. Only what a program writes
to `/tmp` by its absolute path, and a network profile's module and package caches under `/tmp`, are shared between check
runs.

### Head and base

The pod builds both trees first, in parallel, each in its own sandbox, under its own user, with its own tree, caches and
socket: code at the head cannot touch the base's files, and nothing a program leaves behind survives into the next run
(the sandbox kills every process of its user before it answers). maul reaches each side only through a socket that
side's user serves: code that swaps its socket for a link to the other side's, or for a socket of another user, gets
maul's calls refused, never answered by the other side, so the outcomes it touches are unsure, never judged. Then:

- **A head that does not build** runs nothing more: the pod finishes there, before it is ready, so no evaluator is
  launched and none is paid for. CI built that head green, so it is maul's trouble, never the change's: every outcome is
  unsure ("its own build of the pull request did not finish"), and no fix round starts.
- **A base that does not build** is the base's fact (its build ran to its own end and refused the tree: a normal exit of
  1 to 125 (a shell reports a killed child as 128 or more), nothing left running, its output whole, and no sign of an
  environmental failure such as a killed compiler, memory, disk or a module fetch): every check's base run is recorded
  as having no base, and a pass rests on the other rules, marked **no base comparison**. A base build that timed out,
  was killed, failed for an environmental reason or that maul could not run is not that: it gives no base evidence
  (below).
- **Every check runs at both**, the head then the base, each alone under its own fresh run token: the replay of every
  saved check, and the confirmation of every proposed one. A check has no side: the same text runs at both. At most 80
  base runs are recorded per round.

How a check's base runs count, for an outcome a check passed at the head by every other rule:

| At the base, the check...                                                                                                             | `change` outcome                     | `keep` outcome |
| ------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------ | -------------- |
| failed by assertion (its stdout lacked what it asserts, or the program printed its marker)                                            | **tells the change apart**: may pass | no pass        |
| passed                                                                                                                                | no pass: it does not tell them apart | may pass       |
| could not run there for a reason of the base tree itself: the program did not compile, or the base did not build                      | **no base comparison**: may pass     | may pass       |
| failed with no assertion (a wrong exit, a crash), timed out, left processes behind, cut its output, or hit a bind or connection error | no pass                              | may pass       |
| has no run at the base (the round's base runs were spent)                                                                             | no pass                              | no pass        |
| passed on one attempt and failed on another, or is quarantined                                                                        | no pass                              | no pass        |

A "no pass" here is unsure with the reason `no-base-evidence`, never a fail. **"Did not compile" is narrow on purpose.**
A compile counts as the base tree's only when the Go compiler ran to its end and refused the program's own files: a
clean exit 1, output that is not cut, the `# maulcheck` header first, and then only diagnostics located in the program's
own `.go` files (and their continuation lines), with no sign of anything else (a killed compiler, memory, disk, the
module cache, the network, the toolchain). Anything else gives no base evidence, so a pass is never resting on an
environmental failure. The cost: a check whose program imports a package the change adds fails to load at the base
(`cannot find module providing package`), which is not a compile refusal, so it leaves its outcome unsure. Prefer a
check that uses the new symbol from a package that already exists at the base, or an `exec` of the binary.

A check run that errs at the base carries its cause (`compile`, `vendored-tree`, `replace-outside-tree`, `no-module`,
`base-unbuildable`) on the request's `status.base.runs`, the cause the check reviewer sees too. Only those causes mean
no base comparison; an error without one means no base evidence.

A confirmed `change` outcome's check line says which it rested on: `...; discriminates: fails at base by assertion`, or
`...; no base comparison: does not compile at base` (or `the base did not build`, `the base tree is vendored`, and so
on).

### What changes for people

- **No link.** The hand-off holds maul's word, not a link: the status comment says maul is checking the approved
  outcomes "in its own build of the pull request" and holds its hand-off to the reviewers until it has. A review asking
  for changes, `/maul revise` or a push maul did not make hands it over at once; `/maul preview` does nothing, and says
  so.
- **Accepting an outcome.** `/maul accept <id>` binds only when maul had started a round at exactly those heads before
  the comment; otherwise maul replies that it was not checking those commits when the approver commented.
- **Couldn't confirm** has three more reasons: "its own build of the pull request did not finish" (the head did not
  build in the tool pod), "the repository has no accepted runner image to build the pull request in", and "it could not
  fetch the source of the pull request or of its base" (a tree did not become ready within 15 minutes, or GitHub has no
  such commit or does not let maul see it). The intent's `status.verification.unavailableReason` records the last as
  `TreeUnavailable` (the round is retried while the heads stay), the first two as `ToolsBuildFailed` and
  `SandboxImageUnavailable` beside the state `Unconfirmed`. A failing environment there reads "its own checking tools
  failed while it checked", never a preview.
- **A suspected defect's note** names the held hand-off, not a preview link, and only while it is held; its known limits
  are those of the exec and run checks a workspace runs.
- **In the cluster.** The round is a `PreviewVerification` with `spec.placement: agents` and its `spec.workspace` (the
  repository, the head and base commits and their Repositories, the image, the network profile, the surface). Its status
  carries the builds (`status.ledger.builds`), the base side (`status.base`: whether a base was required, whether it was
  unbuildable, each base run with `tree: base`), and the full ledger in `<verification>-ledger`; there are no
  screenshots. The two trees are Intent-owned Repositories pinned to exact commits (`spec.ref.commit`). The status page
  shows the tree of each run, the builds and the base runs.

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
| One checkreview run         | 6 turns per check (3 kept for a repair), 15 minutes                | `--intent-checkreview-max-turns`, chart `checkReviewMaxTurns`; `limits.checkReview`                                              |
| Tool calls per round        | 300 (50 to 300)                                                    | `verification.maxToolCalls`, chart `maxToolCalls`                                                                                |
| Look-around                 | 8 routes per round                                                 | fixed                                                                                                                            |
| Evaluation spend per intent | \$6                                                                | `limits.maxEvaluateCostMicroUSD`, chart `maxEvaluateCostUSD`                                                                     |
| Rounds and fixes            | as slice 1: `maxVerifications`, `maxVerifyFixes`                   | the Project's `limits`                                                                                                           |
| Hand-off wait               | 60 minutes                                                         | `verification.handoffMax`, chart `handoffMax`                                                                                    |
| Saved checks                | 40 per intent, 8 KiB each, 30 steps                                | fixed                                                                                                                            |
| Captured logs               | a bounded tail per component per round; 40 lines in a reproduction | fixed                                                                                                                            |
| Agents tool pods at once    | 2; at the bound a round waits, never a verdict                     | `--intent-max-tool-pods`, chart `verification.agents.maxToolPods`                                                                |
| One tree's build (agents)   | 10 minutes, 1 to 20                                                | `surfaces.cli.buildTimeout`, capped by chart `verification.agents.buildTimeoutMax`                                               |
| Base runs (agents)          | 80 per round                                                       | fixed                                                                                                                            |

A tool pod lives as long as its evaluate run plus five minutes, under the slot's verification deadline
(`preview.verification.deadlineMaxSeconds`), and needs about 2.5 CPU and 2 GiB beside the Preview: see
[the preview-controller](../configuration/preview-controller.md#the-agent-evaluators-tool-pod). An agents tool pod lives
as long as its evaluate run, plus one tree's build, plus five minutes, at most 75 minutes, and needs about 2.25 CPU and
4.25 GiB: see [the agents tool pod](../configuration/intent-controller.md#the-agents-tool-pod).

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
- **Head and base share one pod's loopback and IPC.** Kubernetes cannot give two containers of one pod their own
  loopback or their own POSIX message queues and SysV IPC. A process the head left listening on a loopback port could
  make a base run fail, which would read as "fails at base". So the sandbox kills every process of its user before it
  answers, a run naming a bind or connection error is an error (no base evidence, never a failure by assertion), and the
  isolation probe checks a leftover head listener is gone before the base runs. Base code would have to open a name the
  head chose to meet it over IPC, which Go builds and maul's check forms do not do. The full fix is one pod per side.
- **A program can lie about its own output.** It runs as its sandbox's user, as the shim does; it cannot write the
  `tools` container's log, the ledger, or the other side's files. The check reviewer reads every program in full.
- **A compile refusal that leaves no trace.** A base compile the compiler ended for an environmental reason that left
  nothing of it in the output would still read as a compile refusal (no base comparison). None is known.
- **The evaluation ceiling counts toward the hourly limit.** With the evaluator on, the default \$6 ceiling counts
  toward `RunBudgetOverHourlyLimit`: a broker `costPerHour` below \$12 keeps such a Project from Ready.
