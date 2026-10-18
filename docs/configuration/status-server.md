# status-server

The status page backend: the embedded web dashboard over the `Finding` state machine and the `FindingRollup` statistics,
the sign-in surface, and the three human actions (approve, suspend, resume). It is a server, not a controller — it runs
no reconcilers and takes no leases; a controller-runtime cache gives it live watches, and an SSE stream tells open
browsers to refetch when anything changes.

```sh
status-server serve --namespace patchy --auth-config /etc/patchy/auth/config.yaml
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

| Flag                | Env                      | Default         | Purpose                                                                                                                      |
| ------------------- | ------------------------ | --------------- | ---------------------------------------------------------------------------------------------------------------------------- |
| `--namespace`       | `PATCHY_NAMESPACE`       | `POD_NAMESPACE` | Namespace the Findings and FindingRollups live in                                                                            |
| `--agent-namespace` | `PATCHY_AGENT_NAMESPACE` | `patchy-agents` | Namespace the agent Jobs run in; live conversations are followed from their pod logs                                         |
| `--kubeconfig`      | `PATCHY_KUBECONFIG`      | in-cluster      | Kubeconfig path for running outside the cluster                                                                              |
| `--health-addr`     | `PATCHY_HEALTH_ADDR`     | `:8081`         | healthz/readyz probe listen address                                                                                          |
| `--auth-config`     | `PATCHY_AUTH_CONFIG`     | _(unset)_       | Mounted authentication config; absent ⇒ rollups-only (see below)                                                             |
| `--agents-paused`   | `PATCHY_AGENTS_PAUSED`   | `false`         | Show the [agent pause](investigation-controller.md#pausing-agents) on the signed-in page; display only. Helm: `agent.paused` |

Two more flags turn on the [intents dashboard](../intents/dashboard.md):

| Flag                     | Env                           | Default | Purpose                                                                                                                                        |
| ------------------------ | ----------------------------- | ------- | ---------------------------------------------------------------------------------------------------------------------------------------------- |
| `--intents-enabled`      | `PATCHY_INTENTS_ENABLED`      | `false` | Serve the read-only intents views with per-Project access reviews. Needs mode `oidc` with both claim prefixes, and hardens the page (below)    |
| `--intents-dev-insecure` | `PATCHY_INTENTS_DEV_INSECURE` | `false` | Development only: let the intents views run in mode `none`, showing every Project to every visitor. Refused unless `--listen-addr` is loopback |

With `--intents-enabled` the server refuses to start in mode `none` (without the development flag) or `anonymous`, with
no auth config, or in mode `oidc` without `claims.usernamePrefix` and `claims.groupsPrefix` (and
`claims.requireVerifiedEmail` when the username claim is `email`). It also runs a stricter browser envelope for the
whole page: `Sec-Fetch-Site` same-site and cross-site refused on every `/api` request and `/events`, a write without the
header refused, streams `no-store`, open findings transcript streams re-authorised every 20 seconds, a cap on `/events`
subscribers, a Content-Security-Policy and HSTS. See the [dashboard page](../intents/dashboard.md) for what it shows,
its RBAC and its limits. With the flag off the server behaves exactly as before it existed.

One flag turns on the [usage views](#usage-views):

| Flag              | Env                    | Default | Purpose                                                                                                                                                          |
| ----------------- | ---------------------- | ------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `--usage-enabled` | `PATCHY_USAGE_ENABLED` | `false` | Serve the Usage page: each person's own usage, everyone's for holders of `usage/all`. Needs mode `oidc` as above plus the `federated:id` scope; hardens the page |

One more turns on the [Account page](#account-page):

| Flag                      | Env                            | Default              | Purpose                                                                                                                    |
| ------------------------- | ------------------------------ | -------------------- | -------------------------------------------------------------------------------------------------------------------------- |
| `--account-enabled`       | `PATCHY_ACCOUNT_ENABLED`       | `false`              | Serve the Account page: each person sets, replaces and deletes their own model credential. Needs what the usage views need |
| `--credentials-namespace` | `PATCHY_CREDENTIALS_NAMESPACE` | `patchy-credentials` | The namespace the page writes credential Secrets to (write-only)                                                           |

## Usage views

The Usage page answers "who is producing what, and what does it cost" from the `UsageRollup`s that
[attribution](attribution.md#usage-rollups) writes, so it shows something only with `attribution.mode: attribute`. Helm:
`statusServer.usage.enabled: true` (default `false`).

- **Your own usage.** Every signed-in person sees their own daily cost and tokens over the last 30 or 90 days, stacked
  by Project or by run kind, their top intents, and the totals. The chart opens on cost; tokens are a toggle away and
  open split by type (input, output, cache read, cache write), since a token total is mostly cache reads, which cost a
  tenth of input. No grant is needed: the server reads only the rollups keyed by the caller's own GitHub id.
- **Everyone's usage.** A holder of `get` on the virtual subresource `usage/all` (group `patchy.bitwisemedia.uk`, the
  release namespace) also sees every person, the org's runs (no person behind them), every Project and the totals, and
  can open one person or one Project. With `statusServer.rbac.userRoles` the chart renders the `patchy-usage-admin`
  ClusterRole (that grant plus `get`/`list` on `usagerollups`, for [`patchy get usage`](../cli.md#reading-usage)); bind
  it with a RoleBinding in the release namespace.
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
whoever holds that numeric id. The server refuses a `user_id` equal to `preferred_username` (the login) for that reason,
so under `useLoginAsID` nobody gets a GitHub id and the views refuse everyone.

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
claim prefixes, `requireVerifiedEmail` for an `email` username) and lists `federated:id`. A request from an identity
without a GitHub id (another Dex connector, or a session from before the scope was added: sign out and in again) is
refused both views with a 403 saying why, even when it holds `usage/all`. The views turn on the same hardened browser
envelope as the intents views. The API is `GET /api/usage?days=N` (own), `GET /api/usage/all?days=N&user=&project=`
(everyone's; `user` is a login, `github-id:<id>` or `org`) and `GET /api/usage/access` (what the caller may see); `days`
is 1 to 400, default 30.

## Account page

The Account page is where each person stores the model credential that pays for the runs attributed to them
([per-user model credentials](user-credentials.md)). Helm: `statusServer.account.enabled: true` (default `false`),
refused unless `egressBroker.userCredentials.mode` is `attribute` or `enforce`, whose store and grants it writes to.

- **What it shows.** For each provider (Anthropic in v1): the kind (API key or setup-token), the fingerprint (the last
  four characters and a SHA-256 prefix), the health the egress broker's own probe found (`Valid`, `Rejected` or
  `Unknown`), and when the broker last used it and last saw it rejected. Never the credential: no route returns it, and
  the server cannot read a stored one back.
- **What it does.** Set or replace the credential (a password field), or delete it. Pasting a credential picks the kind
  its Anthropic prefix names. A setup-token must be the person's own subscription's: it is used only for runs attributed
  to them.
- **Who.** A person is their GitHub numeric id, as on the Usage page, so the server refuses to start without what the
  usage views need (mode `oidc`, both claim prefixes, the `federated:id` scope), and an identity without a GitHub id is
  refused with a 403 saying why. Every route also needs an access review on the virtual `modelcredentials/self`: bind
  the chart's `<release>-credentials-self` ClusterRole with a RoleBinding in the release namespace (see
  [who may do what](user-credentials.md#who-may-do-what)). The owner of what is stored is always the signed-in GitHub
  id, never anything in the request.
- **The API.** `GET /api/account` (each provider's facts, or why the caller cannot manage one),
  `POST /api/account/credentials/<provider>` with a JSON body `{"kind": "api-key" | "oauth-token", "credential": "..."}`
  (`Content-Type: application/json`, at most 8 KiB, no other field), and
  `POST /api/account/credentials/<provider>/delete` with no body. The credential must be one token of printable
  characters, at most 3072 bytes, surrounding whitespace trimmed; a value carrying the other kind's Anthropic prefix is
  refused.
- **Envelope.** The page turns on the hardened browser envelope of the intents views, so every write needs a same-origin
  `Sec-Fetch-Site`. Each change is one audit line (`model credential stored` or `deleted`, with the person, the
  provider, the kind and the fingerprint, never the value).

With `statusServer.host` set, every notice that a run waits for someone's credential links the page
(`https://<host>/#/account`). What the page looks like to the people who use it is in
[Your model credential](../account.md).

## Authentication configuration

`--auth-config` points at a YAML file, conventionally a mounted Secret (`patchy-status-auth`, key `config.yaml` — the
deployments mount it `optional`, so removing the Secret degrades to rollups-only rather than failing the pod). A
present-but-invalid file is a startup error: a broken configuration never silently downgrades to no authentication.

```yaml
mode: oidc # none | anonymous | oidc
sessionDuration: 168h # absolute session lifetime (default 7 days)
# insecure: true               # separate non-Secure cookies — plain-HTTP local dev ONLY
anonymous: # mode: anonymous only
  username: status-viewer
  groups: [patchy-viewers]
oidc: # mode: oidc only
  issuerURL: https://sso.example.com
  clientID: patchy-status
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
stored in chunked `HttpOnly` cookies (`__Host-patchy-auth`, `__Host-patchy-auth-1`, …). Every request re-verifies the ID
token; an expired token is renewed via the refresh token in place, but never past `sessionDuration` from the original
sign-in. Three small SPA-readable cookies carry no secrets: `__Host-patchy-auth-provider` (how sign-in works),
`__Host-patchy-auth-error` (the last failure), `__Host-patchy-auth-logout` (pauses `autoLogin` after an explicit
sign-out).

All production cookies, including the short-lived `__Host-patchy-oauth2-state` CSRF cookie, use `Secure`, `Path=/` and
no `Domain`. The browser-enforced `__Host-` prefix prevents a sibling preview host from injecting a parent-domain cookie
with the same name. Expiry and SPA deletion use the same attributes. See the
[cookie prefix requirements](https://httpwg.org/http-extensions/draft-ietf-httpbis-rfc6265bis.html#name-the-__host-prefix).

Upgrading from the old unprefixed names requires signing in again; an in-flight sign-in must also restart. Legacy
cookies are never accepted as a fallback. Old host-only cookies are expired when encountered, including the old OAuth
state's `/oauth2/` path. Parent-domain cookies cannot be reliably identified from a request and remain ignored.

For explicit `insecure: true` local development over **HTTP only**, both server and SPA use separate `patchy-dev-*`
names without `Secure`. Production never reads these names. Do not set `insecure` behind an HTTPS ingress; the SPA
selects its cookie namespace from the browser's protocol, while the server uses operator configuration, not forwarded
headers.

The callback URL is derived from `X-Forwarded-Proto` / `X-Forwarded-Host` (or the Host header), which assumes a trusted
fronting proxy — set `oidc.redirectURL` explicitly if yours cannot be trusted to strip those.

## Authorization

Per-user grants are ordinary Kubernetes RBAC, resolved server-side with `SubjectAccessReview`s for the signed-in user
(users need no kubeconfig and never talk to the API server):

- native `get` on `findings` gates **viewing** the findings surface;
- the **custom verbs** `approve`, `suspend`, `resume` on `findings.patchy.bitwisemedia.uk` gate the action buttons, one
  verb per button.

Grants are namespace-scoped and stamped into the payload as each finding's `userActions`; the client intersects them
with the finding's own state machine, and every `POST` is re-checked server-side. See
`deploy/kustomize/base/rbac.users.example.yaml` for ready-made viewer / approver / operator tiers.

The server's own ServiceAccount is deliberately narrow: `findings` read + **spec** write (approve records
`spec.approval`; suspend/resume toggle `spec.suspend` — it never writes `findings/status` and never moves a phase; the
owning controllers react to the spec change), `findingrollups` read, and cluster-scoped `subjectaccessreviews create`
for the reviews.

Every spec write the server makes (an action button, the user menu's replay and reset, and the Config view's backfill
trigger) is a JSON merge patch of only the field it sets, carrying the resource version it was decided on, so a write
against stale state is a conflict and is retried on fresh state. The server's field manager therefore owns that one
field and nothing an operator applied: a later `helm upgrade` of the same resources applies without `--force-conflicts`.
It sets no manager name of its own, so the API server takes it from the binary's file name, which in the shipped image
is `controller`.
