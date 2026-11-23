# status-server

The status page backend: the embedded web dashboard over the `Finding` state machine and the `FindingRollup` statistics,
the sign-in surface, and the five human actions on a finding (approve, retry, expedite, suspend, resume); optionally the
intents views and the pages built on them, among them the [Project settings page](#project-settings-page). It is a
server, not a controller — it runs no reconcilers and takes no leases; a controller-runtime cache gives it live watches,
and an SSE stream tells open browsers to refetch when anything changes.

```sh
status-server serve --namespace maul --auth-config /etc/maul/auth/config.yaml
```

The exposure contract is deliberately asymmetric — see [the status page](../status-ui.md) for the UI tour and the RBAC
grammar:

- **Rollup statistics are public.** `GET /api/rollups` (and the SSE stream) serve without a session.
- **The findings surface always requires authentication.** `GET /api/findings` (the trimmed list projection),
  `GET /api/findings/{name}` (one finding's full detail — description, alerts, enrichments, the phase log, and the run
  reports the list omits), and every action `POST` demand a signed-in identity whose RBAC passes the corresponding
  access review. With no auth config at all the server runs in the _unconfigured_ posture: rollups only, and the page
  explains that sign-in is not configured.
- **Agent conversations ride the findings gate.**
  `GET /api/findings/{name}/runs/{investigation|remediation}/{attempt}/transcript` is Server-Sent Events in both modes:
  a finished run replays its stored ConfigMap, a running one streams from the agent's pod log. Following a live run
  needs `pods` and `pods/log` read in `--agent-namespace` (through the API server — the server never dials an agent
  pod); without that grant, completed runs' conversations still serve. See
  [Agent conversations](../status-ui.md#agent-conversations).

## Flags

The [shared flags](index.md#shared-flags-every-controller) (`--listen-addr` is the page's own address here — there is no
webhook), plus:

| Flag                | Env                    | Default         | Purpose                                                                                                                      |
| ------------------- | ---------------------- | --------------- | ---------------------------------------------------------------------------------------------------------------------------- |
| `--namespace`       | `MAUL_NAMESPACE`       | `POD_NAMESPACE` | Namespace the Findings and FindingRollups live in                                                                            |
| `--agent-namespace` | `MAUL_AGENT_NAMESPACE` | `maul-agents`   | Namespace the agent Jobs run in; live conversations are followed from their pod logs                                         |
| `--kubeconfig`      | `MAUL_KUBECONFIG`      | in-cluster      | Kubeconfig path for running outside the cluster                                                                              |
| `--health-addr`     | `MAUL_HEALTH_ADDR`     | `:8081`         | healthz/readyz probe listen address                                                                                          |
| `--auth-config`     | `MAUL_AUTH_CONFIG`     | _(unset)_       | Mounted authentication config; absent ⇒ rollups-only (see below)                                                             |
| `--agents-paused`   | `MAUL_AGENTS_PAUSED`   | `false`         | Show the [agent pause](investigation-controller.md#pausing-agents) on the signed-in page; display only. Helm: `agent.paused` |

Two more flags turn on the [intents dashboard](../intents/dashboard.md):

| Flag                     | Env                         | Default | Purpose                                                                                                                                        |
| ------------------------ | --------------------------- | ------- | ---------------------------------------------------------------------------------------------------------------------------------------------- |
| `--intents-enabled`      | `MAUL_INTENTS_ENABLED`      | `false` | Serve the read-only intents views with per-Project access reviews. Needs mode `oidc` with both claim prefixes, and hardens the page (below)    |
| `--intents-dev-insecure` | `MAUL_INTENTS_DEV_INSECURE` | `false` | Development only: let the intents views run in mode `none`, showing every Project to every visitor. Refused unless `--listen-addr` is loopback |

With `--intents-enabled` the server refuses to start in mode `none` (without the development flag) or `anonymous`, with
no auth config, or in mode `oidc` without `claims.usernamePrefix` and `claims.groupsPrefix` (and
`claims.requireVerifiedEmail` when the username claim is `email`). It also runs a stricter browser envelope for the
whole page: `Sec-Fetch-Site` same-site and cross-site refused on every `/api` request and `/events`, a write without the
header refused, streams `no-store`, open findings transcript streams re-authorised every 20 seconds, a cap on `/events`
subscribers, a Content-Security-Policy and HSTS. See the [dashboard page](../intents/dashboard.md) for what it shows,
its RBAC and its limits. With the flag off the server behaves exactly as before it existed.

One flag turns on the [usage views](#usage-views):

| Flag              | Env                  | Default | Purpose                                                                                                                                                                         |
| ----------------- | -------------------- | ------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `--usage-enabled` | `MAUL_USAGE_ENABLED` | `false` | Serve the Usage page: each person's own usage, everyone's for holders of `usage/all`. Needs mode `oidc` as above plus the `federated:id` and `profile` scopes; hardens the page |

One more turns on the [Account page](#account-page):

| Flag                      | Env                          | Default            | Purpose                                                                                                                    |
| ------------------------- | ---------------------------- | ------------------ | -------------------------------------------------------------------------------------------------------------------------- |
| `--account-enabled`       | `MAUL_ACCOUNT_ENABLED`       | `false`            | Serve the Account page: each person sets, replaces and deletes their own model credential. Needs what the usage views need |
| `--credentials-namespace` | `MAUL_CREDENTIALS_NAMESPACE` | `maul-credentials` | The namespace the page writes credential Secrets to (write-only)                                                           |
| `--account-providers`     | `MAUL_ACCOUNT_PROVIDERS`     | `anthropic`        | The providers the page offers, in order: `anthropic` (claude), `openai` (codex). The chart sets those its runners use      |

## Usage views

The Usage page answers "who is producing what, and what does it cost" from the `UsageRollup`s that
[attribution](attribution.md#usage-rollups) writes, so it shows something only with `attribution.mode: attribute`. Helm:
`statusServer.usage.enabled: true` (default `false`).

- **Your own usage.** Every signed-in person sees their own daily cost and tokens over the last 30 or 90 days, stacked
  by Project or by run kind, their top intents, and the totals. The chart opens on cost; tokens are a toggle away and
  open split by type (input, output, cache read, cache write), since a token total is mostly cache reads, which cost a
  tenth of input. No grant is needed: the server reads only the rollups keyed by the caller's own GitHub id.
- **Everyone's usage.** A holder of `get` on the virtual subresource `usage/all` (group `maul.run`, the release
  namespace) also sees every person, the org's runs (no person behind them), every Project and the totals, and can open
  one person or one Project. With `statusServer.rbac.userRoles` the chart renders the `maul-usage-admin` ClusterRole
  (that grant plus `get`/`list` on `usagerollups`, for [`maul get usage`](../cli.md#reading-usage)); bind it with a
  RoleBinding in the release namespace.
- **What the figures are.** Tokens and cost are what the egress broker metered, priced at list rates. Wherever the page
  shows tokens (totals, the daily table, people, Projects) it shows them by type; cache writes are one figure (the
  rollups record five-minute and one-hour writes together). The stacks by Project and by run kind and the top intents
  show one token total: run-kind shares and top intents are recorded without the split, and the chart's stack by token
  type covers each day. A Claude subscription's tokens are priced too, as an _equivalent API cost_, never as money
  spent. The broker's worst-case unmetered shortfall and the runs with only their pod's own (untrusted) report are shown
  in their own panel, never in a total. Top intents come from month buckets, so they cover the whole months the window
  touches.

A person is their **GitHub numeric id**, never a login, which can be renamed and taken by someone else. The page reads
it from the Dex ID token's `federated_claims`: `connector_id` must equal `oidc.claims.githubConnector` (default
`github`) and `user_id` is the id. Dex sends that claim only when asked for the `federated:id` scope, so list it. The
GitHub connector must **not** set `useLoginAsID`: with it, `user_id` is the login, and an all-digit login would read as
whoever holds that numeric id. The server tells the two apart by the login, `preferred_username`, which Dex sends only
when asked for the `profile` scope, so list that too. It trusts no `user_id` without a `preferred_username` beside it,
and none equal to it, so under `useLoginAsID` nobody gets a GitHub id and the views refuse everyone.

```yaml
oidc:
  scopes: [openid, offline_access, profile, email, groups, "federated:id"]
  claims:
    username: preferred_username
    usernamePrefix: "github:"
    groupsPrefix: "github:"
    # githubConnector: github   # the id of Dex's GitHub connector
```

With `--usage-enabled` the server refuses to start unless the config passes the intents views' checks (mode `oidc`, both
claim prefixes, `requireVerifiedEmail` for an `email` username) and lists `federated:id` and `profile`. A request from
an identity without a GitHub id (another Dex connector, or a session from before the scope was added: sign out and in
again) is refused both views with a 403 saying why, even when it holds `usage/all`. The views turn on the same hardened
browser envelope as the intents views. The API is `GET /api/usage?days=N` (own),
`GET /api/usage/all?days=N&user=&project=` (everyone's; `user` is a login, `github-id:<id>` or `org`) and
`GET /api/usage/access` (what the caller may see); `days` is 1 to 400, default 30.

## Account page

The Account page is where each person stores the model credential that pays for the runs attributed to them
([per-user model credentials](user-credentials.md)). Helm: `statusServer.account.enabled: true` (default `false`),
refused unless `egressBroker.userCredentials.mode` is `attribute` or `enforce`, whose store and grants it writes to.

- **What it shows.** For each provider it offers (`--account-providers`; the chart sets Anthropic while claude runs
  anywhere and OpenAI while codex does): the kind (an Anthropic API key or setup-token, an OpenAI API key), the
  fingerprint (the last four characters and a SHA-256 prefix), the health the egress broker's own probe found (`Valid`,
  `Rejected` or `Unknown`), and when the broker last used it and last saw it rejected. Never the credential: no route
  returns it, and the server cannot read a stored one back. A credential stored for a provider the page no longer offers
  (its runner since turned off) still gets its card, so its owner can see and delete it; it cannot be replaced.
- **What it does.** Set or replace the credential (a password field), or delete it. Pasting an Anthropic credential
  picks the kind its prefix names; OpenAI takes an API key only, so its card offers no kind choice. A setup-token must
  be the person's own subscription's: it is used only for runs attributed to them.
- **Who.** A person is their GitHub numeric id, as on the Usage page, so the server refuses to start without what the
  usage views need (mode `oidc`, both claim prefixes, the `federated:id` and `profile` scopes), and an identity without
  a GitHub id is refused with a 403 saying why. Every route also needs an access review on the virtual
  `modelcredentials/self`: bind the chart's `<release>-credentials-self` ClusterRole with a RoleBinding in the release
  namespace (see [who may do what](user-credentials.md#who-may-do-what)). The owner of what is stored is always the
  signed-in GitHub id, never anything in the request.
- **The API.** `GET /api/account` (each provider's facts, or why the caller cannot manage one),
  `POST /api/account/credentials/<provider>` with a JSON body `{"kind": "api-key" | "oauth-token", "credential": "..."}`
  (`oauth-token` for `anthropic` only) (`Content-Type: application/json`, at most 8 KiB, no other field), and
  `POST /api/account/credentials/<provider>/delete` with no body. The credential must be one token of printable
  characters, at most 3072 bytes, surrounding whitespace trimmed; a value carrying the other kind's Anthropic prefix, or
  the other provider's prefix (`sk-ant-` stored as an OpenAI key, an OpenAI `sk-` key stored as an Anthropic one), is
  refused. A provider the page does not offer is a 404.
- **Envelope.** The page turns on the hardened browser envelope of the intents views, so every write needs a same-origin
  `Sec-Fetch-Site`. Each change is one audit line (`model credential stored` or `deleted`, with the person, the
  provider, the kind and the fingerprint, never the value).

With `statusServer.host` set, every notice that a run waits for someone's credential links the page
(`https://<host>/#/account`). What the page looks like to the people who use it is in
[Your model credential](../account.md).

## Project settings page

The Project settings page lets a Project's approvers change its limits, models, evaluator, hand-off, CI checks and each
repository's machine size from the status page, within bounds the operator sets. What it shows and how people use it is
in [Project settings](../intents/project-settings.md); this section is the operator's side. Helm:
`statusServer.projectSettings.enabled: true` (default `false`). Chart only: there is no kustomize component.

```yaml
statusServer:
  intents:
    enabled: true # required: the page is one of the intents views
  auth:
    existingSecret: maul-status-auth # mode oidc, both claim prefixes, the federated:id and profile scopes
  projectSettings:
    enabled: true
    projects: [] # the Projects the page may edit, by name; empty is every Project in the release namespace
    allow:
      approvers: false # approvers may add or remove one approver per save
      disableEvaluator: false # verification off, early hand-off, an evaluator limit under its floor
      emptyChecks: false # an empty checks.fix
    locked: [] # groups shown read-only: approvers, evaluator, handoff, models, cost, rounds, activeIntents, stages, checks, resources
    ceilings:
      maxCostMicroUSD: 25000000 # $25
      maxRevisions: 10
      maxCheckFixes: 10
      maxVerifyFixes: 5
      maxVerifications: 50
      maxActiveIntents: 5
    floors:
      maxEvaluateCostMicroUSD: 2000000 # also under limits.evaluate.maxCostMicroUSD
      evaluateMaxTurns: 40
      evaluateTimeout: 10m
      evaluateTokenBudget: 100000
      maxToolCalls: 100
      maxVerifications: 3
      handoffMax: 30m
admissionPolicy:
  enabled: true # the default, and required
```

### Before you turn it on

Check that intent-controller can tell who your approvers are. An approver is matched by GitHub numeric account id: the
id in the person's sign-in (Dex's `federated_claims`, as for the [usage views](#usage-views)) must equal the id
intent-controller recorded for a login on the approver list. intent-controller asks GitHub's collaborator-permission
endpoint on the intent repository, with the App's issues-read installation token, and records `user.id` from the answer.
That this answer carries `user.id` under an installation token has **not been measured**: it does under a person's own
token, and under the App's token maul has only ever read the `permission` beside it. If it does not, every approver is
recorded as no GitHub account and nobody can save.

From the release that added the page, intent-controller publishes what it found on every Project whether or not the page
is on, so upgrade first and check before you turn the page on. No Secret is read:

```sh
kubectl -n maul get project <project> -o jsonpath='{range .status.approvers.identities[*]}{.login}{"\t"}{.id}{"\t"}{.write}{"\n"}{end}'
kubectl -n maul get project <project> -o jsonpath='{.status.conditions[?(@.type=="ApproversResolved")].message}'
gh api users/<login> --jq .id
```

Each approver with write access should show the id `gh api users/<login>` prints and `true`. A login that exists on
GitHub but shows no id (the field is left out when it is 0), or that the condition's message lists under "no such GitHub
account", means the answer under the App's token lacks the id: leave the page off.

Two more things nothing checks for you:

- **One GitHub host.** Dex's GitHub connector and the Forge of every Project's intent repository must be the same GitHub
  (github.com, or one GitHub Enterprise Server). Ids from two hosts are two id spaces: an id could match nobody, or the
  wrong person. Run `gh api users/<login>` above against the host Dex signs people in with.
- **Dex without `useLoginAsID`.** With it, Dex's `user_id` is the login. The status server then gives nobody a GitHub id
  (see [usage views](#usage-views)), so nobody can save, because it trusts a `user_id` only beside a different
  `preferred_username`. That needs the `profile` scope, which the server refuses to start without.

Once the page is on, an approver who opens it sees their own login marked "you" and can save. An approver refused as
"not an approver" while `status.approvers` lists their login with an id means the two ids differ.

### The page's flags

| Flag                          | Env                              | Default   | Purpose                                                                                                                                             |
| ----------------------------- | -------------------------------- | --------- | --------------------------------------------------------------------------------------------------------------------------------------------------- |
| `--project-settings-enabled`  | `MAUL_PROJECT_SETTINGS_ENABLED`  | `false`   | Serve the page and its save. Needs `--intents-enabled` and what the usage views need                                                                |
| `--project-settings-policy`   | `MAUL_PROJECT_SETTINGS_POLICY`   | _(unset)_ | The bounds as one JSON object `{allow, locked, ceilings, floors}`; unset is the chart's defaults above. Parsed strictly: a bad one refuses to start |
| `--project-settings-projects` | `MAUL_PROJECT_SETTINGS_PROJECTS` | _(unset)_ | Comma-separated Project names the page serves; unset is every Project. The chart sets it from `projects`, so the page and the patch grant agree     |

The server refuses to start with the page on unless `--intents-enabled` is on and the auth config passes the usage
views' checks: mode `oidc`, both claim prefixes, `requireVerifiedEmail` for an `email` username, and `federated:id` and
`profile` in the scopes. Under `--intents-dev-insecure` (mode `none`, loopback only) the page is served read-only and
its save is refused. The chart refuses to render the page without `statusServer.intents.enabled`, without
`admissionPolicy.enabled`, with an inline `statusServer.auth.config` that does not ask for `federated:id` and `profile`
(an `existingSecret` is judged by the server at startup), or with `floors.maxVerifications` above
`ceilings.maxVerifications`.

### The bounds

The four blocks are rendered once into one JSON object, which both the status server and the admission policy below
read, so the two cannot disagree. What each bound means to the people using the page is in
[what the operator allows](../intents/project-settings.md#what-the-operator-allows).

- `allow` holds the three switches, each off by default. `approvers` lets an approver add or remove one approver per
  save, never themselves; a new login counts only once GitHub reports its write access. `disableEvaluator` lets an
  approver turn verification off, set the hand-off to early (or from gated to unset, since your install's default may be
  early) and lower an evaluator limit under its floor. `emptyChecks` lets an approver empty `checks.fix`, after which
  maul waits for and fixes no CI check; even off, an approver can leave one check that never fails, which only the audit
  record shows.
- `locked` takes the lock groups: `approvers`, `evaluator` (`verification.enabled`, `verification.maxToolCalls`),
  `handoff` (`verification.handoff`, `verification.handoffMax`), `models`, `cost` (`limits.maxCostMicroUSD`,
  `limits.maxEvaluateCostMicroUSD`), `rounds` (`maxRevisions`, `maxCheckFixes`, `maxVerifyFixes`, `maxVerifications`),
  `activeIntents`, `stages` (`limits.<stage>.*`), `checks` and `resources` (`agentResourceClass`).
- `ceilings` bound the six limits intent-controller does not clamp. A change is admitted when the new value is at most
  the ceiling, or at most the old one: a higher value you set yourself can be kept or lowered from the page, never
  raised. A cleared field is judged by the default it lands on. Money is in micro-USD.
- `floors` bound the evaluator's limits from below unless `allow.disableEvaluator`: a change is admitted when the new
  value is at least the floor, or at least the old one. `0` (`0s` for a duration) is no floor. The evaluation cost floor
  also holds under the evaluate stage's own cost, because an evaluate run's budget is the lesser of the two. Durations
  are whole hours, minutes and seconds (`10m`, `1h30m`).

Beyond your bounds, the page takes only what intent-controller admits, from the facts it publishes on each Project
(`status.admits`, see [Projects](intent-controller.md#projects)): models it admits for the Project's harness, the
resource classes it defines (none defined: the machine size is read-only), and nothing above its own stage, evaluation
cost and tool-call ceilings. Until `status.admits` is published, no model or class can be chosen and nothing
intent-controller bounds can be raised.

### What the chart renders

- **One write.** Role and RoleBinding `<release>-status-server-project-settings` in the release namespace: `patch` on
  `projects` (group `maul.run`), with `resourceNames` from `projects` when set. No `update`, nothing on
  `projects/status`, no other kind. Project status is a subresource, so the patch cannot change what intent-controller
  publishes there.
- **An admission policy.** ValidatingAdmissionPolicy and binding `<release>-project-settings`, `failurePolicy: Fail`, on
  every update of a Project by the status server's ServiceAccount. It holds each patch to the same bounds again, in the
  API server: every field outside the page's own is pinned without being named (so a field added to Project later is
  pinned too), as are the number, order and every other field of the repositories, the labels, finalizers and owner
  references, and every annotation but `maul.run/settings-edited` (at most 1 KiB); the record of which writer manages
  which field (`managedFields`, which the page never writes but any patch could replace): no other writer's entry added,
  dropped or re-dated, and none gaining or losing a field outside the page's own; the locks, switches, ceilings and
  floors; models and classes against `status.admits`; and intent-controller's own ceilings. A denial reaches the page as
  a refusal of the whole save ("The cluster refused the save as invalid"); its message starts `project settings:`, and
  the status server logs it.
- The ConfigMap keys of the three flags.

People need no new grant: whoever may see a Project's intents (`get` on `projects/intents`) can open its page, and an
approver can save. Such a viewer sees more than the board shows: the repositories' full URLs, the preview image
repository and paths, and the approvers' logins with their GitHub state.

What the policy cannot do: it sees the status server's ServiceAccount, not the person behind a save. Whether the person
is an approver with write access, and is not removing themselves, is checked only in the status server, so a compromised
status server could make any change within your bounds on the Projects its Role names (with `allow.approvers` on, adding
or removing one approver per write) and write a false record annotation. `projects`, `locked: [approvers]` and leaving
`allow.approvers` off narrow it. Such a server could also rewrite who manages a field the page may edit (forging that
Helm, or nobody, set a limit), since the policy cannot tell the ownership a save takes from a forged one; the next
`helm upgrade` would then overwrite that field without a conflict, or conflict on it falsely. Ownership of every other
field is pinned. Project writes by people stay admin-only in RBAC; the policy does not judge them.

### Set each field from one place

A Project written by the maul-config chart (`projects` values) is written by Helm too, and the page's saves and Helm's
upgrades then share the object. Kubernetes records which writer set each field (`managedFields`). The page writes as
field manager `maul-status-server`, a JSON merge patch of only the fields it changed plus its record annotation, so it
takes over exactly those fields and nothing else; the page names every other writer of a field beside it. What the next
apply does with a page change depends on how it applies.

Measured with the status server's real save on API servers 1.36.2 and 1.37.0, and pinned by a test
(`TestProjectSettingsOwnOnlyEditedFields`), for Helm 4's default server-side apply of an unchanged manifest after a
save:

| The page changed                                       | `helm upgrade` (default)                                                                                                                          | `helm upgrade --force-conflicts`           |
| ------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------ |
| a setting the values file does not state               | applies; the page's value stays                                                                                                                   | the same                                   |
| a setting the values file states (here `maxRevisions`) | **fails**, nothing written: `Apply failed with 1 conflict: conflict with "maul-status-server" using maul.run/v1alpha1: .spec.limits.maxRevisions` | applies; the values file's setting is back |
| the same, with the page's value copied into values     | applies; Helm and the page both own the field, and the page's next change of it fails the upgrade again                                           | —                                          |
| added an entry to a list (approvers, `checks.fix`)     | applies; the entry stays                                                                                                                          | the same                                   |
| removed an entry the values file lists                 | applies, **with no error or warning, and the entry is back** (last in the list)                                                                   | the same                                   |

The failure names every such field at once; the page's record annotation is never one of them. Measured once by hand
with kubectl and Helm 4.2.3, not by a test: the upgrade fails the same way whatever else it changes; the real
`helm upgrade` reports `UPGRADE FAILED: conflict occurred while applying object ...` and records the revision as failed;
and client-side apply (`helm install --server-side=false`, or `kubectl apply`) writes as an `Update` manager and keeps
page changes of fields its manifest does not change, but once the manifest changes a list the page also changed, its
next apply replaces the whole list, and an entry the page added is dropped silently. Not measured: Argo CD, Flux,
Helm 3.

!!! warning "An approver removed on the page comes back"

    A login an approver removed on the page while your values file still lists it is restored by the next
    `helm upgrade` of maul-config, forced or not, with no conflict and no warning, and counts as an approver again. The
    same holds for a CI check removed from `checks.fix`. Remove it from the values file too.

So decide per setting where it lives. Leave a setting out of the values file to manage it on the page, or change it in
the values file and lock its group on the page if approvers should not change it. To keep a change made on the page, the
page's **Copy as values** produces a `projects` entry to merge into the values file; it lists every setting the Project
states, so trim it to what the values file should own. And remove an approver in the values file, not only on the page.

### Audit

- Each save stamps the Project's `maul.run/settings-edited` annotation in the same patch: the approver's GitHub id and
  login, the time and the field ids changed. The page shows it as "Last changed here by".
- The status server logs `project settings changed` (Info) with the Project, the user, the GitHub id, the approver's
  login as intent-controller resolved it, the generation decided at and the new one, and each change's field, item, old
  and new value; and `project settings refused` with the reason and each refused field, plus the API server's causes
  when the cluster refused it. A save that changes nothing logs nothing.
- The counter `maul.status.project_settings.writes` counts saves by `result` (`changed`, `unchanged`, each refusal
  reason, `rate-limited`, `bad-request`, `error`).

A person may save at most ten times a minute.

### The API

`GET /api/projects/<project>/settings` serves the page's view: the settings, each field's bounds, the approvers as
GitHub resolved them, the other field managers, the facts and the last edit. `POST` to the same path saves:
`Content-Type: application/json`, at most 16 KiB, unknown fields refused, `{generation, schema, settings, confirm}`, the
whole editable document with the generation the page was read at. Answers: 200 (saved, or nothing to save), 400, 401,
403 (with the reason the person may not save, or the cluster's RBAC refused), 404 (a Project the person may not see,
exactly as one that does not exist), 409 (the Project changed since it was read, the page is out of date, or
confirmations are needed), 413, 415, 422 (refused, with each field and reason), 429. Both routes exist only with the
page on, inside the hardened browser envelope of the intents views.

### Rollback

Set `statusServer.projectSettings.enabled: false` and upgrade: the routes, the patch grant and the admission policy go.
Settings saved from the page stay in the Projects, and so does `maul-status-server` in their `managedFields` for each
field it set. It follows (not run) that a default `helm upgrade` of maul-config still fails on a page-set field its
values state until the values say the same or one upgrade is forced, which puts the values back. To undo a page change,
set the field in the values file and upgrade with `--force-conflicts`.

## Authentication configuration

`--auth-config` points at a YAML file, conventionally a mounted Secret (`maul-status-auth`, key `config.yaml` — the
deployments mount it `optional`, so removing the Secret degrades to rollups-only rather than failing the pod). A
present-but-invalid file is a startup error: a broken configuration never silently downgrades to no authentication.

```yaml
mode: oidc # none | anonymous | oidc
sessionDuration: 168h # absolute session lifetime (default 7 days)
# insecure: true               # separate non-Secure cookies — plain-HTTP local dev ONLY
anonymous: # mode: anonymous only
  username: status-viewer
  groups: [maul-viewers]
oidc: # mode: oidc only
  issuerURL: https://sso.example.com
  clientID: maul-status
  clientSecret: "..." # or clientSecretFile: /path/to/projected/key
  # scopes: [openid, offline_access, profile, email, groups]
  # authURLParams: {}        # extra authorize-endpoint query parameters
  # autoLogin: false         # bounce straight to the provider instead of the sign-in panel
  # redirectURL: ""          # override the derived <scheme>://<host>/oauth2/callback
  claims: # claim NAMES, mapped onto the identity
    username: email # the subject access reviews run for
    groups: groups
    displayName: name
    # usernamePrefix: "oidc:"     # prepended to the username before every access review
    # groupsPrefix: "oidc:"       # prepended to every group
    # requireVerifiedEmail: true  # refuse a token whose email_verified is not true
    # githubConnector: github     # Dex connector whose federated_claims carry a GitHub id (usage views)
```

`usernamePrefix` and `groupsPrefix` work like kube-apiserver's `--oidc-username-prefix` and `--oidc-groups-prefix`: with
them, a binding written for a status page user cannot also match a cluster identity of the same name, and a provider
group named `system:masters` reaches the review as `oidc:system:masters`. A prefix inside `system:` is refused. They
rename every identity for every review, so rebind the existing roles to the prefixed names when you set them. They are
optional unless the intents views are on, and so is `requireVerifiedEmail`, which the views require when the username
claim is `email`. The evaluation API reads the same `claims` block and applies the same rules.

### Modes

- **`none`** — every request is a fixed development identity with authorization bypassed entirely. The dev overlay ships
  this; never expose it.
- **`anonymous`** — every request is the one configured identity, but access reviews still run: cluster RBAC for that
  username/groups decides what every visitor may see and do.
- **`oidc`** — the real SSO flow. The server itself is the OAuth2 client (authorization-code + PKCE); the SPA never sees
  a token.

### Sessions and cookies (mode `oidc`)

There is no server-side session store. The ID token, refresh token, and session start are sealed with AES-256-GCM — the
key is derived (HKDF-SHA256) from the OIDC client secret, so **rotating the client secret signs everyone out** — and
stored in chunked `HttpOnly` cookies (`__Host-maul-auth`, `__Host-maul-auth-1`, …). Every request re-verifies the ID
token; an expired token is renewed via the refresh token in place, but never past `sessionDuration` from the original
sign-in. Three small SPA-readable cookies carry no secrets: `__Host-maul-auth-provider` (how sign-in works),
`__Host-maul-auth-error` (the last failure), `__Host-maul-auth-logout` (pauses `autoLogin` after an explicit sign-out).

All production cookies, including the short-lived `__Host-maul-oauth2-state` CSRF cookie, use `Secure`, `Path=/` and no
`Domain`. The browser-enforced `__Host-` prefix prevents a sibling preview host from injecting a parent-domain cookie
with the same name. Expiry and SPA deletion use the same attributes. See the
[cookie prefix requirements](https://httpwg.org/http-extensions/draft-ietf-httpbis-rfc6265bis.html#name-the-__host-prefix).

Upgrading from the old unprefixed names requires signing in again; an in-flight sign-in must also restart. Legacy
cookies are never accepted as a fallback. Old host-only cookies are expired when encountered, including the old OAuth
state's `/oauth2/` path. Parent-domain cookies cannot be reliably identified from a request and remain ignored.

For explicit `insecure: true` local development over **HTTP only**, both server and SPA use separate `maul-dev-*` names
without `Secure`. Production never reads these names. Do not set `insecure` behind an HTTPS ingress; the SPA selects its
cookie namespace from the browser's protocol, while the server uses operator configuration, not forwarded headers.

The callback URL is derived from `X-Forwarded-Proto` / `X-Forwarded-Host` (or the Host header), which assumes a trusted
fronting proxy — set `oidc.redirectURL` explicitly if yours cannot be trusted to strip those.

## Authorization

Per-user grants are ordinary Kubernetes RBAC, resolved server-side with `SubjectAccessReview`s for the signed-in user
(users need no kubeconfig and never talk to the API server):

- native `get` on `findings` gates **viewing** the findings surface;
- the **custom verbs** `approve`, `retry`, `expedite`, `suspend`, `resume` on `findings.maul.run` gate the action
  buttons, one verb per button.

Grants are namespace-scoped and stamped into the payload as each finding's `userActions`; the client intersects them
with the finding's own state machine, and every `POST` is re-checked server-side. See
`deploy/kustomize/base/rbac.users.example.yaml` for ready-made viewer / approver / operator tiers.

The server's own ServiceAccount is deliberately narrow: `findings` read + **spec** write (approve records
`spec.approval`, retry `spec.retry` and expedite `spec.expedite`; suspend/resume toggle `spec.suspend` — it never writes
`findings/status` and never moves a phase; the owning controllers react to the spec change), `findingrollups` read, and
cluster-scoped `subjectaccessreviews create` for the reviews.

Every spec write the server makes (an action button, the user menu's replay and reset, and the Config view's backfill
trigger) is a JSON merge patch of only the field it sets, carrying the resource version it was decided on, so a write
against stale state is a conflict and is retried on fresh state. The server's field manager therefore owns that one
field and nothing an operator applied: a later `helm upgrade` of the same resources applies without `--force-conflicts`.
These writes set no manager name of their own, so the API server takes it from the binary's file name, which in the
shipped image is `controller`.

The [Project settings page](#project-settings-page), when on, is the exception. With it the server also holds `patch` on
`projects`, and each save is a merge patch of the Project fields it changed plus one annotation,
`maul.run/settings-edited`, sent as field manager `maul-status-server`. Those fields are then the page's: a default
`helm upgrade` of maul-config that still states one of them fails with a conflict naming `maul-status-server` until the
values agree, as [set each field from one place](#set-each-field-from-one-place) describes. It writes no status and no
other kind.
