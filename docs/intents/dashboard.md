# The intents dashboard

The status page can show every intent a person may see: a board of intents by phase, each intent's timeline, and a panel
for each agent run that answers the three questions a running intent raises: is it stuck, what has it spent, and when
will it stop. It is read-only. Approval stays a GitHub fact (the approve label or `/maul approve` on the intent issue,
bound to the plan's digest), and nothing on the dashboard can approve, cancel or replan an intent. The one page that
writes, [Project settings](project-settings.md), is a separate switch, off unless the operator turns it on; it changes a
Project's settings, never an intent. With it on, the top bar's **Projects** entry lists the Projects whose settings you
can open, and the board's Project chips say **Settings** and link to them.

It is off by default. Turning it on adds per-Project access, mandatory identity hardening and a stricter browser
envelope for the whole status page; read this page before you do. [Turning it on](#turning-it-on) has the settings.

The screenshots on this page show sample data from the status page's demo build.

## What it shows

**The board** has a column per stage: planning, awaiting approval, building, in review (revise and check-fix rounds
included), done (merged, closed, failed, until the intent expires). A Blocked intent stays in the column of the phase it
was blocked from, with a badge and the reason; the **blocked** column holds only a Blocked intent whose earlier phase is
no longer in its recorded phase history. An intent waiting on the issues it is blocked by
([intent dependencies](dependencies.md)) sits in planning with a **waiting** badge, the reason in fixed words ("waiting
for the issues it is blocked by", "a blocker was closed without being completed", "a blocker cannot be read",
"dependency cycle", "checking dependencies", "waiting for an active slot"), the intent repository's blockers as `#N`,
and a count of any others: a blocker in another repository is never named. A card shows the issue, the plan's one-line
summary, the repositories, every agent run that is pending or running (a multi-repository build runs one per repository
at once), the attempt of the newest run against the two each round gets (an attempt whose agent never ran, such as one
no node could fit, does not count, as it does not for the controller), revision and CI-fix rounds against the Project's
limits (every round started counts, a failed one too, as it does for the limit), recorded spend against the cost
ceiling, its age and time in phase, and its pull request and preview links. A suspended intent is badged: it is not
reconciled while suspended, so its status may be stale.

![The intents board: one intent awaiting approval, one building, one in review and Blocked at its revision limit, and
one merged](../assets/images/screens/intents-board.jpg)

In the board above, `storefront-7` sits in the in-review column with a **blocked** badge, because it was blocked from
review when it reached its revision limit (3 of 3). Each card's bar is its recorded spend against the Project's
per-intent cost ceiling (\$10 by default).

**The timeline** of one intent lists its phase changes; the plan revision and digest, with a link to the plan comment;
the accepted approval with who approved, how (label or command), the plan revision and both digests it is bound to; the
pull requests; the preview; and every run with its stage, round, attempt, outcome, duration, recorded cost and image
(source and digest). Each run row links to that run's panel twice: its underlined name and a trailing "View conversation
→" link ("Open run →" when the panel will show no conversation: for a reader without the transcripts tier, a run still
waiting for a run slot, or a finished run that recorded none).

![An intent's timeline: the issue, requester, recorded spend, plan revision and approval, its phase changes, and its
two runs, a finished plan and a running build](../assets/images/screens/intent-detail.jpg)

**The run panel** shows how long the run has taken against its stage time limit, the Job's deadline and the idle
watchdog's limit (20 minutes without a model turn or tool result, by default), which tool the agent called last and has
no result for yet and for how long, the last activity, and every condition that stops the run, in plain English. The
output-token kill switch is listed as what it is: a count of streamed output tokens, not a bound on spend. The cost
ceiling is checked only before the next launch, so a running run can pass it; the panel says so. A run still waiting for
a run slot can be opened too: its panel follows it live from the moment its agent starts. The activity comes from the
run's transcript, which stops recording at its cap (500 entries or 512 KiB) while the agent goes on; past the cap the
panel says the activity is not known rather than show the last tool and time frozen there.

![A running build's panel: elapsed time against its limits, the command it is waiting on, what stops it, and its live
output](../assets/images/screens/intent-run.jpg)

In the panel above, the build has run 23 minutes of its one-hour stage limit and has been waiting 9 minutes on one
`Bash` command, the test suite whose output streams below. "What stops it" lists each limit in plain words: the stage
time limit, the Job's deadline, the idle watchdog, the turn limit, the output-token kill switch, the attempt count and
the cost ceiling.

**Live output.** While a run is live, a reader with the transcripts tier also sees what the command the agent is waiting
on prints (a test suite, a build), as it prints it. It is one command at a time, the latest: when the agent starts
another, the block switches to it. It is live only and never stored: a viewer who opens the panel while the command runs
starts from its last 200 lines, and a reload after the run ends, or a finished run, shows none. Each line is plain text,
with invisible characters shown as their code point like the rest of the agent's text, cut at 1 KiB; a line the agent
cut at 8 KiB shows as a placeholder. The panel keeps the newest 500 lines and marks with a count wherever lines were
left out (by the agent sampling a long command past its first 64 KiB, the server's bounds or a slow connection). Its
header says whether the command is running or finished, or that the stream stopped before it finished, and adds "live
output limit reached" once one of the agent's limits
([agent-runner](../configuration/agent-runner.md#live-command-output)) stopped the command's live output for good.
Command output is not activity: the turn count, the last activity and the idle watchdog count model turns and tool
results only, so a command printing steadily does not make a stuck agent look busy. A reader with only the intents tier
receives none of it.

**The conversation.** A reader with the transcripts tier sees the agent's conversation turn by turn: its messages, every
tool it called and what came back. While the run is live it streams from the agent pod's log, which the status server
reads through the Kubernetes API (the chart's `statusServer.liveTranscripts`, on by default). When the run ends, its
controller stores the conversation in a ConfigMap owned by the run, and the panel replays that from then on. A
[report repair](../configuration/agent-runner.md#report-repair) continues the same conversation, after a notice saying
why.

What is stored is bounded and scrubbed:

- Each turn's text is capped (2 KiB by default), and a run at 500 turns or 512 KiB in all
  ([agent-runner](../configuration/agent-runner.md)). A conversation cut short ends with a turn naming the cap it
  reached, while the agent itself goes on.
- Any value matching a credential in the pod's environment, the broker token included, is replaced before it leaves the
  pod.
- The activity count is of transcript entries, and a tool call and its result are separate entries, so not every entry
  is a model turn.
- The live command output is never stored: a finished run's panel shows the conversation, which keeps each command's
  result, but no live output.

A stored conversation is deleted with its run when the intent expires.

Spend on the dashboard is recorded spend only: the cost each run reported when it was collected. A run's spend appears
when it ends. There are no live token or dollar figures yet. For spend over time, per person and per Project, see
[Costs and usage](../usage.md).

### What it never shows

Run details and condition messages can quote a scheduler or kubelet message naming private nodes, or an image reference
carrying a private registry host and account ID. The dashboard shows neither. Outcomes and block reasons are fixed
sentences keyed by maul's own vocabulary (`evicted: the agent pod was evicted`,
`the repository's agent image was rejected`); images appear as their source and digest, never their registry. Raw
`kubectl describe` remains the operator's tool for the rest.

Agent text (plans, reports, transcripts, the plan summary) and issue text are shown as plain text only, framed as
written by the agent, never rendered as markdown and never turned into links. Characters that render as nothing (bidi
and zero-width controls, tag characters, terminal escapes) are shown as their code point, `[U+202E]`. Links come only
from fields the controllers write, and only as `https`.

## Two read tiers, per Project

Access is decided per Project by Kubernetes RBAC, through SubjectAccessReviews the status server runs for the signed-in
user, on two **virtual** subresources of `projects.maul.run`:

| Grant                           | Shows                                                                                                                                                                         |
| ------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `get` on `projects/intents`     | The board, timelines and run panels: phases, durations, recorded cost, outcomes, links                                                                                        |
| `get` on `projects/transcripts` | With `projects/intents`: plan text, run reports, transcripts (persisted and live) and live command output; an evaluate run's ledger outputs, evaluator claims and screenshots |

Nothing on the API server serves these subresources, so the rules grant `kubectl` nothing; the status server's reviews
are their only reader. The tiers nest: `projects/transcripts` without `projects/intents` opens nothing. A rule without
`resourceNames` covers every Project; `resourceNames` scopes it to the Projects named:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: maul-intents-content-storefront
  namespace: maul
rules:
  - apiGroups: [maul.run]
    resources: [projects/intents, projects/transcripts]
    resourceNames: [storefront] # the Project's name
    verbs: [get]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: maul-intents-content-storefront
  namespace: maul
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: maul-intents-content-storefront
subjects:
  - kind: Group
    apiGroup: rbac.authorization.k8s.io
    name: github:acme:storefront # a prefixed org:team group
```

`deploy/kustomize/base/rbac.users.example.yaml` has the example roles and bindings, and the chart renders the two
every-Project ClusterRoles (`<release>-intents-viewer`, `<release>-intents-content`) with `statusServer.rbac.userRoles`.

A Project a person may not see is invisible: its intents are absent from the board, and asking for one of them, its runs
or its plans answers 404, exactly as for an intent that does not exist. A person who may see a Project but not its
transcripts gets a 403 for plan text and no report, conversation or command output in the run panel. The live
conversation and command output are stripped on the server for each viewer, so two people watching the same run at
different tiers share one log follow and still receive different streams. In the run panel's conversation, a subagent's
turns are indented under the `Agent` call that started it, each line marked with a bar, beside notices of its start and
end; a turn whose call is not in the record is shown where it falls, its heading marked "subagent".

!!! warning "Native RBAC bypasses the tiers"

    The tiers bind the dashboard only. Plans and transcripts are stored in ConfigMaps in the release namespace, and raw
    agent output is in the agent pods' logs. Anyone with `get` on `configmaps` in `maul` reads every Project's plans
    and transcripts (and every Finding's), and anyone with `get` on `pods/log` in `maul-agents` reads the raw agent
    output, which carries more than the scrubbed transcript does. The built-in `view`, `edit` and `admin` roles include
    both. Give a per-Project viewer only the virtual subresources above, never native reads in these namespaces, and
    check who holds them:

    ```sh
    kubectl auth can-i get configmaps -n maul --as <user> --as-group <group>
    kubectl auth can-i get pods/log -n maul-agents --as <user> --as-group <group>
    ```

Grants are by Project **name**. A Project deleted and created again under the same name while the old one's intents are
still within their TTL hands those intents to the new Project's viewers; give a Project a new name rather than reuse
one.

## The evaluation evidence

With [the agent evaluator](evaluator.md) on, the panel of an `evaluate` run also shows that run's round as maul recorded
it, so a person can see why a hand-off was held or released without reading the evaluator's transcript. It is the same
evidence the [evaluator page](evaluator.md#reading-the-evidence) lists for GitHub and `kubectl`, read-only and per run:
`GET /api/intents/<intent>/runs/<run>/evaluation`.

It is read from the round's `PreviewVerification`, which must be the one the run pins (by name and UID) and must name
this very intent, so a look-alike object is never shown. When that round is the intent's latest, the view also carries
the decided results; for an older round it shows what was recorded and says it is not the current one.

| Section        | Shows                                                                                                          | Tier |
| -------------- | -------------------------------------------------------------------------------------------------------------- | ---- |
| Outcomes       | Each approved outcome, maul's result (`passed`, `failed`, `unsure`, `accepted`) in the comment's words         | 1    |
| Counted calls  | The calls maul counted for each outcome: the ones its verdict cites in the round's ledger                      | 1    |
| Cited calls    | What each counted call sent, the start of its output, its stored screenshot, and whose script it ran           | 2    |
| Anomalies      | What the evaluator noticed while looking around, as its words, with the ledger entries it cites                | 1    |
| Hand-off       | Whether the preview link was released to people, when and why                                                  | 1    |
| Ledger         | One row per tool call: kind, verb, at the base or not, exit, refused, and the sha256 of the result             | 1    |
| Ledger detail  | Each call's request and output from the line that is the index's call; an output that does not hash is dropped | 2    |
| Evaluator says | The evaluator's verdict, the seqs it cited (counted or not) and its note per outcome                           | 2    |
| Screenshots    | The images the evaluator cited, served as PNG only when the bytes start with the PNG signature                 | 2    |

Tier 1 is `projects/intents`, tier 2 is `projects/transcripts`. Everything written by the evaluator or the application
is plain text, framed as such and never rendered as markup. Application output is labelled as the application's, the
evaluator's request as the evaluator's, and a call that ran the evaluator's own page script or program says so. maul
counts a verdict only by the calls it cites; whether a cited call shows what the verdict says is for the reviewers to
judge, and the page says so above the outcomes. The evaluator's verdict is a claim; the result beside it is maul's
decision, and the two can differ. A report that still writes a key the evaluator no longer has (`disputes`, `proposed`,
`retire`, `contrary`) claims nothing, so none of its claims is shown. A screenshot is shown as evidence a person can
look at. Whether one was kept never changes a result, but a `browser screenshot` call counts like any other browser
command, so the evaluator's reading of a screenshot alone can make an outcome seen to hold: see
[the known limits](evaluator.md#known-limits).

The status server reads the verification and its two ConfigMaps (`<verification>-ledger`, `<verification>-evidence`)
with `get` only, uncached, and a ConfigMap another object controls is never served.

## Identity: OIDC with prefixes

The dashboard shows private code, so it runs only under sign-in mode `oidc`, with both claim prefixes set. The server
refuses to start otherwise (and the chart refuses to render an inline `statusServer.auth.config` that is not mode `oidc`
with both prefixes; an `existingSecret` is checked by the server alone):

- modes `none` and `anonymous` are refused: the first bypasses authorization for every visitor, the second gives every
  visitor one identity's grants. For local development only, `--intents-dev-insecure` lets mode `none` run the views on
  a loopback `--listen-addr`.
- `oidc.claims.usernamePrefix` and `oidc.claims.groupsPrefix` are required. Without them a RoleBinding written for a
  dashboard user also matches any cluster identity of the same name (an EKS access entry, another provider's user), and
  a provider group called `system:masters` would reach the access review as exactly that. A prefix inside `system:` is
  refused.
- When the username claim is `email`, `oidc.claims.requireVerifiedEmail: true` is required too, and a token whose
  `email_verified` claim is not true signs no one in.

The prefixes apply to **every** access review the status server runs, the findings page's included. Setting them renames
every identity: rebind the findings roles to the prefixed names in the same change, or their users lose the findings
page.

### Dex with GitHub

[Dex](https://dexidp.io) with its GitHub connector signs people in with their GitHub account and issues `org:team`
groups, which lines dashboard identities up with the GitHub logins in a Project's approvers. A Dex static client for the
status page:

```yaml
staticClients:
  - id: maul-status
    name: maul status
    secretEnv: MAUL_STATUS_CLIENT_SECRET
    redirectURIs: [https://status.maul.example.com/oauth2/callback]
connectors:
  - type: github
    id: github
    name: GitHub
    config:
      clientID: $GITHUB_CLIENT_ID
      clientSecret: $GITHUB_CLIENT_SECRET
      redirectURI: https://dex.example.com/callback
      orgs:
        - name: acme # only members sign in; teams become acme:<team>
      teamNameField: slug
```

and the status server's auth config (`statusServer.auth.existingSecret`, key `config.yaml`):

```yaml
mode: oidc
oidc:
  issuerURL: https://dex.example.com
  clientID: maul-status
  clientSecretFile: /etc/maul/auth/client-secret # or clientSecret
  redirectURL: https://status.maul.example.com/oauth2/callback
  scopes: [openid, offline_access, profile, email, groups]
  claims:
    username: preferred_username # the GitHub login
    groups: groups # acme:<team>
    displayName: name
    usernamePrefix: "github:"
    groupsPrefix: "github:"
```

Bindings then name `github:<login>` and `github:acme:<team>`.

The username here is the GitHub login, not an email, so `email_verified` is not what proves who signed in, and the
server does not require it. What does: Dex authenticated the person with GitHub and asserts the login GitHub returned;
the `orgs` restriction admits only members of your organization; and the mandatory prefix keeps the login apart from
every cluster identity. GitHub logins can be renamed, and a released login can be registered by someone else, so bind
teams (which only organization owners control) rather than individual logins where you can. If you bind by email instead
(`username: email`), set `requireVerifiedEmail: true`; the server insists.

Revoking access: an RBAC change takes effect within 20 seconds, on open streams too (they re-check every 20 seconds and
end when the grant is gone). Removing someone from a team at GitHub takes effect only when their ID token is next
refreshed through Dex; a status page session lasts up to `sessionDuration` (7 days by default) through refresh tokens.
Shorten `sessionDuration` if that is too long.

## Turning it on

With the chart:

```yaml
statusServer:
  intents:
    enabled: true
  auth:
    existingSecret: maul-status-auth # mode oidc with both prefixes, as above
  rbac:
    userRoles: true # optional: the two example ClusterRoles
```

With kustomize, add the component to your overlay:

```yaml
components:
  - ../../components/status-intents
```

Either grants the status server read (`get`, `list`, `watch`) on projects, intents, intentruns and previews in the
release namespace, and `get` on Jobs in the agent namespace, for a live run's clock (it reads the Job's creation and
start time, deadline and idle limit, and never passes the Job itself on). It writes nothing new: no intent, no status,
no Secret. Only the [Project settings page](project-settings.md), turned on by its own switch
(`statusServer.projectSettings.enabled`), adds a write: `patch` on projects, of the settings it offers and one
annotation recording who saved, held by an admission policy (see
[its operator's side](../configuration/status-server.md#project-settings-page)).

### What else changes on the page

With the dashboard on, the whole status page runs a stricter browser envelope, because preview apps run pull request
code on hosts that are same-site with the status host, and same-site requests carry the session cookie:

- every `/api` request and the `/events` stream refuse `Sec-Fetch-Site: same-site` and `cross-site`;
- every write (`POST`) also refuses a request with no `Sec-Fetch-Site` at all, so scripts that post findings actions
  without a browser must send `Sec-Fetch-Site: same-origin`;
- streams are `Cache-Control: no-store`;
- a Content-Security-Policy whose `script-src` admits only the page's own inline scripts, by hash, and
  `Strict-Transport-Security`;
- the findings transcript streams re-check the viewer's grant every 20 seconds too;
- the public `/events` stream holds at most 512 subscribers: one past that is served and the oldest dropped (its page
  reconnects on its own), so filling the stream locks no one out; one person holds at most 6 intents streams at once,
  the server 256.

Every intents stream ends after 10 minutes and the page reopens it through the current session, so a signed-out or
expired session stops streaming within that time. The change signal the board listens on reaches a person only for the
Projects they can see.

Each read of tier 2 content (a plan, a report, a transcript) is logged as one line, `intent content read`, with the
user, Project, intent, run and what was read, never the content.

Completed intents expire with their runs after intent-controller's TTL (14 days by default), and the dashboard forgets
them with them.
