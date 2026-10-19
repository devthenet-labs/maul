# egress-broker

The egress credential broker: a reconciler-less reverse proxy that agent pods send **all** claude model traffic through.
It is what makes claude agent pods fully credential-less — the pod carries only an audience-bound projected
ServiceAccount token, and the broker injects or signs the real model credential outbound. It also makes 3rd-party model
providers work: Amazon Bedrock, GCP Vertex AI, and Microsoft Foundry each get a route the claude CLI's documented
gateway mode points at, with the broker holding the cloud identity that authenticates the provider-native requests.

There is no in-pod credential mode for claude — claude-without-broker is not a configuration. The codex and copilot
runners (both disabled by default) keep their in-pod Secret channels for now. The broker already has an
[OpenAI Responses route](#the-openai-route) (off by default) that a later release moves the codex runner onto; nothing
uses it yet.

## How a request flows

```text
agent pod (claude CLI)                 egress-broker (release ns)              upstream
  ANTHROPIC_BEDROCK_BASE_URL=            1. TokenReview(X-Patchy-Broker-Token)
    http://…-egress-broker:8080/bedrock  2. audit line (pod, route, path,        bedrock-runtime.<region>.amazonaws.com
  CLAUDE_CODE_SKIP_BEDROCK_AUTH=1           status, duration — never bodies)     api.anthropic.com
  X-Patchy-Broker-Token: <projected      3. strip caller headers; SigV4-sign /   <region>-aiplatform.googleapis.com
    ServiceAccount token>                   inject key / attach OAuth token      <resource>.services.ai.azure.com
                                         4. stream the response back, flushing
                                            every write; SSE keep-alive pings
```

The caller token is validated via `TokenReview` (audience `patchy-egress-broker`; only
`system:serviceaccount:<agent-ns>:<agent-sa>` is accepted), its verdict cached briefly (`--verdict-ttl`, 1m) so the API
server sees one review per token, not one per request, and the header is stripped before anything is forwarded. The
audit identity — the calling pod's name — comes from the pod-bound token's claims.

## Routes and credentials

A route exists exactly when its identifying flag is set; at least one is required.

| Route        | Enable with                            | Upstream (default)                               | Credential                                                                                                                                                                                                      |
| ------------ | -------------------------------------- | ------------------------------------------------ | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `/anthropic` | `--anthropic-api-key-file`             | `https://api.anthropic.com`                      | The mounted Secret file, read per request so rotation propagates without a restart; `--anthropic-auth key` injects it as `x-api-key`, `token` as an `Authorization` bearer (a `claude setup-token` OAuth token) |
| `/bedrock`   | `--bedrock-region`                     | `https://bedrock-runtime.<region>.amazonaws.com` | SigV4, signed with the broker's ambient AWS identity (IRSA / EKS Pod Identity / env credentials)                                                                                                                |
| `/vertex`    | `--vertex-region` + `--vertex-project` | `https://<region>-aiplatform.googleapis.com`     | OAuth bearer from Application Default Credentials (GKE Workload Identity in-cluster), cached/refreshed; only requests naming that project and location (the region) are admitted                                |
| `/foundry`   | `--foundry-resource`                   | `https://<resource>.services.ai.azure.com`       | `--foundry-auth key`: `x-api-key` from a file; `entra`: a cached, auto-refreshed Entra bearer                                                                                                                   |
| `/openai`    | `--openai-api-key-file`                | `https://api.openai.com`                         | An OpenAI API key from the mounted Secret file, read per request, sent as an `Authorization` bearer; see [The openai route](#the-openai-route)                                                                  |

Per-user model credentials (`--user-credentials`, chart `egressBroker.userCredentials.mode`, off by default) are the one
exception to one credential per route: on the anthropic route, under `enforce`, a run attributed to a person uses that
person's own API key or setup-token, or is refused with a reason; a run attributed to the org keeps the org credential.
See [Per-user model credentials](user-credentials.md).

Cross-cutting behavior: responses stream back with an immediate flush per write; when an event-stream goes quiet for
`--sse-ping-interval` (30s) the broker injects a spec-legal `: ping` comment frame so intermediaries do not idle-close a
long thinking pause; the Bedrock route buffers request bodies (SigV4 signs a payload hash) up to `--max-request-bytes`
(10 MiB). Errors are emitted as Anthropic-style JSON error envelopes, except on the openai route, which answers in
OpenAI's.

`/healthz` is liveness; `/readyz` additionally requires every configured route's credential source to be usable — a
missing Anthropic key file or unresolvable cloud credentials fail readiness. This replaces the job controllers' old
claude-Secret probing as "is the model credential present": the controllers enable the claude harness by configuration
and probe `readyz` advisorily at startup.

That probe is one request, made once, by the investigation- and remediation-controller only (intent-controller does not
probe), with a 5-second timeout and no retry. When it cannot reach the broker, typically because the broker's pods are
not yet Ready during an install or a roll, the controller logs one warning,
`egress broker not reachable yet; claude jobs will fail until it is`, and starts anyway; when the broker answers but not
`200`, the warning is `egress broker is not ready; check its /readyz and credential sources`. Neither is repeated, and
nothing is gated on them: if claude Jobs then run and succeed, the warning was a startup race and can be ignored.

## Flags

As everywhere, each flag is also the matching `PATCHY_*` environment variable (`--tokens-per-pod` is
`PATCHY_TOKENS_PER_POD`).

| Flag                            | Default                | Purpose                                                                                              |
| ------------------------------- | ---------------------- | ---------------------------------------------------------------------------------------------------- |
| `--listen-addr`                 | `:8080`                | The proxy listener                                                                                   |
| `--health-addr`                 | `:8081`                | `/healthz` and `/readyz`                                                                             |
| `--token-audience`              | `patchy-egress-broker` | Audience callers' projected tokens must be bound to (the job controllers' `--broker-token-audience`) |
| `--verdict-ttl`                 | `1m`                   | How long one caller token's TokenReview verdict is cached                                            |
| `--sse-ping-interval`           | `30s`                  | Idle keep-alive period for event-stream responses; negative disables ping injection                  |
| `--agent-namespace`             | `patchy-agents`        | Namespace whose agent ServiceAccount callers must be                                                 |
| `--agent-service-account`       | `patchy-agent`         | The only ServiceAccount the broker answers to                                                        |
| `--max-request-bytes`           | `10485760` (10 MiB)    | Largest request body the payload-signing route (bedrock) accepts                                     |
| `--max-anthropic-request-bytes` | `2097152` (2 MiB)      | Largest request body every other route accepts; bodies are buffered so they can be inspected         |

The route flags are in [Routes and credentials](#routes-and-credentials): `--anthropic-api-key-file`, `--anthropic-auth`
(`key`), `--anthropic-base-url` (`https://api.anthropic.com`), `--bedrock-region`, `--bedrock-base-url`,
`--vertex-region`, `--vertex-project` (the only GCP project the vertex route admits; required with `--vertex-region`),
`--vertex-base-url`, `--foundry-resource`, `--foundry-base-url`, `--foundry-auth` (`key`), `--foundry-api-key-file`,
`--openai-api-key-file` and `--openai-base-url` (`https://api.openai.com`).

The per-user credential flags (see [Per-user model credentials](user-credentials.md)):

| Flag                             | Default              | Purpose                                                                                               |
| -------------------------------- | -------------------- | ----------------------------------------------------------------------------------------------------- |
| `--user-credentials`             | `off`                | `off`, `attribute` (record the person, org credential pays) or `enforce` (the person's own, or a 403) |
| `--credential-records-namespace` | `patchy`             | Namespace of the `ModelCredential` records (the release namespace)                                    |
| `--credentials-namespace`        | `patchy-credentials` | Namespace of the credential Secrets                                                                   |
| `--org-credential-is-personal`   | empty                | `acknowledged` admits a setup-token org credential under either mode; refused otherwise               |
| `--credential-probe-model`       | `claude-haiku-4-5`   | Model a setup-token health probe asks for one output token of                                         |

### Limit flags

Every limit is off at `0` (or empty), so an unconfigured broker behaves as it always has; the Helm chart sizes them.

| Flag                            | Default  | Purpose                                                                                                                                                                                                  |
| ------------------------------- | -------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `--requests-per-pod`            | `0`      | Requests one agent pod may make in its lifetime                                                                                                                                                          |
| `--concurrent-per-pod`          | `0`      | In-flight requests one agent pod may hold                                                                                                                                                                |
| `--concurrency-wait`            | `2s`     | How long a request over `--concurrent-per-pod` waits for one of the pod's slots before it is refused; `0` takes the default, negative refuses at once                                                    |
| `--tokens-per-pod`              | `0`      | Tokens (input, cache creation, cache read and output) one agent pod may consume                                                                                                                          |
| `--tokens-per-hour`             | `0`      | Broker-wide trailing-hour token ceiling across every pod                                                                                                                                                 |
| `--limit-unit`                  | `tokens` | What the spend limits count: `tokens` (the two token flags) or `cost` (the two cost flags, with the token flags kept as optional backstops); see [Cost limits](#cost-limits)                             |
| `--cost-per-pod`                | `0`      | Micro-USD one agent pod may spend (`60000000` is $60), lowered per pod by its budget annotation; required with `--limit-unit=cost`, refused otherwise                                                    |
| `--cost-per-hour`               | `0`      | Broker-wide trailing-hour cost ceiling in micro-USD; `--limit-unit=cost` only                                                                                                                            |
| `--max-tokens-ceiling`          | `0`      | Largest `max_tokens` (on the openai route, `max_output_tokens`) a request may set                                                                                                                        |
| `--model-allowlist`             | empty    | Comma-separated model ids pods may name (canonical or wire form; dated variants, OpenAI's `-YYYY-MM-DD` snapshots included, and the `claude-haiku` helper family are admitted); empty admits every model |
| `--beta-denylist`               | empty    | Comma-separated `anthropic-beta` glob patterns to strip; empty uses the built-in list (`mcp-client-*`, `web-fetch-*`, `code-execution-*`, `files-api-*`, `context-1m-*`), `none` strips nothing          |
| `--preauth-requests-per-second` | `0`      | Per-source-IP request rate admitted before authentication                                                                                                                                                |
| `--preauth-burst`               | `0`      | Per-source-IP burst and in-flight cap before authentication                                                                                                                                              |
| `--token-reviews-per-second`    | `0`      | Broker-wide TokenReview rate, with a short queue                                                                                                                                                         |

## Enforcement

Under a [repository-declared agent image](../integrations/agent-images.md) the agent pod runs code the repository
controls, and the caller token is readable by anything in the pod, so the in-pod budget is advisory. The broker is the
enforcement point for what a pod may ask of the model API and how much, in layers that all run before any upstream
contact:

1. **Pre-authentication.** A per-source-IP token bucket and in-flight cap (`--preauth-*`), then a syntactic check of the
   token: three base64url segments, an `aud` naming the broker's audience, an unexpired `exp`, and a `kubernetes.io.pod`
   claim. **Callers must present pod-bound tokens** — the projected ServiceAccount token the kubelet mounts into an
   agent pod; a token minted with `kubectl create token` or the TokenRequest API without a pod binding is refused here,
   before any TokenReview. A failed authentication counts against the source IP.
2. **Authentication.** The TokenReview, its verdict cached for `--verdict-ttl` and rate-limited broker-wide
   (`--token-reviews-per-second`), so a pod varying its token header degrades only itself. With any per-pod limit set, a
   review without the `authentication.kubernetes.io/pod-name` extra is refused rather than counted in an anonymous
   bucket.
3. **Route surface.** Each route forwards a positive list of methods and paths and answers 404 to everything else, so
   the Files, Batches and other upstream APIs are unreachable: `POST /v1/messages`, `POST /v1/messages/count_tokens` and
   `GET /v1/models` on anthropic; `invoke` and `invoke-with-response-stream` on bedrock; `rawPredict` and
   `streamRawPredict` in the configured project and location on vertex; the messages API on foundry;
   `POST /v1/responses` and `GET /v1/models` on openai. On bedrock and vertex the model id is read from the path and
   checked against `--model-allowlist` there.
4. **Body inspection.** Every body is buffered (`--max-anthropic-request-bytes`, 2 MiB, on every route but bedrock,
   which keeps `--max-request-bytes`) and refused when it asks for a server-side tool that reaches the internet or a
   container (`web_search_*`, `web_fetch_*`, `code_execution_*`, `mcp_toolset`), carries `mcp_servers`, `container`, a
   `file_id` or a `source.type: file` block, names a model off the allowlist, or sets `max_tokens` above
   `--max-tokens-ceiling`. Denied `anthropic-beta` entries are stripped. **The 2 MiB cap can refuse a legitimate request
   with 413** when the agent's context carries many images (screenshots, diagrams); raise
   `--max-anthropic-request-bytes` if runs fail that way.
5. **Spend.** Per-pod request, concurrency and token counters and the broker-wide hourly ceiling are checked before
   proxying. Tokens are charged from the usage every response reports (`message_start` and `message_delta` on a stream,
   `usage` on a JSON body), as it streams, so a stream cut after `message_start` has already paid for its input. A
   response whose usage never becomes final — a cut stream, a usage-less body, or any body the broker cannot parse — is
   charged its worst case: the larger of the reported input and a request-size estimate (body bytes / 4), plus the
   larger of the reported output and `max_tokens` (else the ceiling, else bytes streamed / 4). **Bedrock event streams
   (`invoke-with-response-stream`) are binary and never parsed, so every such response is charged the request estimate
   plus its `max_tokens`**, which overstates a normal run's spend; size `--tokens-per-pod` for it on bedrock.

Over any limit the broker answers 429 in the Anthropic error envelope, with a message starting
`egress broker: per-pod limit`, which `agent-runner` maps to the `budget_exceeded` outcome. The audit line carries the
pod's running totals, so the limits can be sized from observed runs; the `patchy.broker.tokens` and
`patchy.broker.preauth.rejections` counters carry the same figures. Aggregate spend is bounded by
`max concurrent Jobs x --tokens-per-pod x Job turnover`, and by `--tokens-per-hour` when set. Evaluation pods run under
the same ServiceAccount and share the per-pod limits, so size them for the longest legitimate evaluation too.

### The openai route

The `/openai` route fronts the OpenAI Responses API with an org API key (`--openai-api-key-file`; chart
`egressBroker.openai.apiKey`, a Secret in the release namespace). It is off unless that flag is set, and no runner uses
it yet: the codex runner still holds its own key in the agent pod until a later release brokers it. API keys only;
ChatGPT-plan sign-in and Codex access tokens are not brokered.

- **Surface.** `POST /v1/responses` (metered, the model read from the body) and `GET /v1/models`. Everything else is 404
  before the upstream, including `/v1/responses/compact`, the stored-response endpoints (`/v1/responses/{id}`, its input
  items and cancel), input-token counting, Files, Conversations and Chat Completions, and any request asking for a
  protocol upgrade (Responses over WebSocket).
- **Headers.** Beside the broker token, `Authorization` and `x-api-key`, the broker strips `OpenAI-Organization`,
  `OpenAI-Project`, `ChatGPT-Account-Id` and `OpenAI-Beta` on every route, so a pod cannot bill another organization or
  project the key can reach.
- **Body inspection.** Tools are an allowlist of the ones the client runs itself: `function`, `custom`, `local_shell`,
  and `shell` and `apply_patch` executed locally, plus a `namespace` that only groups such tools (codex sends its
  sub-agent tools as one; a namespace may not nest). A request is refused (403) when it lists any other tool type (the
  hosted ones, such as `web_search` and its dated variants, `file_search`, `code_interpreter`, `mcp`, `image_generation`
  and `computer_use`, and any server tool OpenAI adds later) or a tool naming a `connector_id`, a `server_url`, a hosted
  `environment` or a `container`; when it sets `background: true`, `previous_response_id`, `conversation` or a stored
  `prompt`; or when, anywhere outside a function tool's `parameters` or a `json_schema` format's `schema`, it carries an
  `item_reference` input item (a stored item loaded by id), a `file_id`, a `file_url` or an `image_url` that is not an
  inline `data:` URI. `max_output_tokens` is the request's output bound and is held to `--max-tokens-ceiling`.
- **Usage.** Usage arrives once, in the terminal `response.completed`, `response.incomplete` or `response.failed` event
  (or a JSON body's `usage`). Cached input (`input_tokens_details.cached_tokens`) is charged as cache reads and the rest
  of `input_tokens` as input; `output_tokens` (reasoning included) as output. OpenAI reports no separate cache writes,
  so there are none. The terminal event carries the whole response, so its line may be up to 8 MiB (the Anthropic routes
  skip lines over 64 KiB); a larger one, a stream cut before its terminal event, or a terminal event without usage is
  charged its worst case, bounded by `max_output_tokens`. Dated snapshots (`gpt-5.3-codex-2026-02-05`) are priced as
  their model, at its registry rates.
- **Errors.** In OpenAI's envelope, `{"error": {"message", "type", "code", "param"}}`. A spend-limit refusal keeps the
  `egress broker: per-pod limit` message prefix and carries type and code `insufficient_quota`, which the client treats
  as final rather than retrying; any other 429 is `rate_limit_exceeded`.
- **Per-user credentials** stay Anthropic only: the broker refuses to start with `--user-credentials` beside the openai
  route, and the chart refuses to render the two together.

### Cost limits

Token limits weigh every token alike, but a long-context agent's volume is mostly cache reads, which cost a tenth of an
input token, so a token cap sized for a short run cuts a long one early. `--limit-unit=cost` (chart
`egressBroker.limits.unit: cost`, off by default) expresses the per-pod and hourly limits in micro-USD instead:

- Every charge is priced per model with the same function the run controllers price measured usage with, at the built-in
  registry's list rates: input, cache reads, the five-minute and one-hour cache writes at their multipliers, and output.
  A model the registry cannot price is charged at the dearest rate of each class, so an unknown id fails safe.
  Subscription (`--anthropic-auth token`) traffic is priced the same way, notionally.
- A request reserves its `max_tokens` (else `--max-tokens-ceiling`) at its model's output price until it settles, and a
  response whose usage never becomes final is charged its shortfall at the output price.
- A pod's cap is the smaller of `--cost-per-pod` and the pod's own `patchy.bitwisemedia.uk/budget-micro-usd` annotation
  (one to sixteen digits). The broker reads it with one `get` of the caller's pod in `--agent-namespace` when it reviews
  the token, checks the pod UID against the TokenReview, and caches it with the verdict (`--verdict-ttl`). A missing,
  malformed or zero annotation, a gone pod or a different UID falls back to `--cost-per-pod`, never to unlimited; a read
  the API server does not answer refuses the request with 503 and caches nothing. The annotation can only lower the cap.
  intent-controller stamps it on every intent run under the cost unit, from the Project's stage limits
  ([stage limits](intent-controller.md#stage-limits)), and admission policies let only it set the annotation; a Finding
  Job carries none and runs under `--cost-per-pod`.
- `--tokens-per-pod` and `--tokens-per-hour` still apply when set, as absolute backstops.

A refusal reads `egress broker: per-pod limit: cost per pod (N micro-USD) reached`, or
`broker-wide hourly cost ceiling (N micro-USD) reached`, and maps to `budget_exceeded` as every other limit does. Cost
is counted under either unit: the audit line carries `cost_micro_usd` (this request), `pod_cost_micro_usd` (the pod's
total) and `pod_cost_limit_micro_usd` (its cap, `0` under the token unit), and `patchy.broker.cost` counts micro-USD by
route and class (`observed`, `estimated`), so a cost limit can be sized from live runs before an install switches. The
chart's cost unit renders a Role granting `get` on pods in the agent namespace, nothing else; setting
`PATCHY_LIMIT_UNIT` through `config.extra` instead leaves the broker without it, and every request then fails with 503.
[Sizing limits for large features](../intents/sizing.md) has the measured runs behind these limits, a "large" preset and
the steps to switch an install.

## The controller side

The job controllers (and the evaluation controller, via the shared flags) point claude runners at the broker with
`--broker-url` — required whenever a claude runner is configured — and describe the provider with `--claude-provider`,
`--claude-provider-region`, `--claude-provider-region-prefix`, `--claude-provider-project-id`, `--claude-model-map`, and
`--claude-provider-env`. Canonical model ids stay canonical everywhere controller-side; the provider-specific
translation (Bedrock `us.anthropic.<model>` inference profiles, Vertex bare ids, Foundry deployment names) is rendered
into `PATCHY_MODEL_MAP` and applied in-pod. Bedrock and Vertex ids are derived per registry model; **Foundry has no
derivable ids**, so `--claude-model-map` must cover every claude-resolving allowlisted model and stage default — the
controllers refuse to start otherwise, which turns a would-be mid-run failure into a startup error.

## Usage endpoint

The broker's ledger also records what each pod consumed per model. It keeps the tokens the responses reported (input,
output, cache reads, and cache writes split into five-minute and one-hour parts) apart from the worst-case shortfall it
charged for a cut or usage-less response. The limits count both; only the reported tokens are ever priced. The run
controllers read this record when a run settles ([trusted usage](attribution.md#trusted-usage)).

The record is served on a listener of its own, off unless `--usage-addr` is set:

| Flag               | Default                      | Purpose                                                                                               |
| ------------------ | ---------------------------- | ----------------------------------------------------------------------------------------------------- |
| `--usage-addr`     | empty (off)                  | Listen address of the usage endpoint (`:8082` in the chart)                                           |
| `--usage-callers`  | empty                        | Comma-separated `<namespace>:<serviceaccount>` identities it answers to; required with `--usage-addr` |
| `--usage-audience` | `patchy-egress-broker-usage` | Audience the callers' projected tokens must be bound to; must differ from `--token-audience`          |

`GET /v1/usage/jobs/<job>` answers with the broker's boot time and the ledger entry of each of that Job's pods (named
`<job>-<five characters>`): the pod name and UID, when the ledger first saw it, its requests still in flight, and per
model the route, the credential kind (`api-key`, `oauth-token` for a Claude subscription, `cloud-identity`), the tokens
by class and the shortfall. The caller sends a pod-bound projected token as `Authorization: Bearer`; it passes the same
pre-authentication as the proxy, then a TokenReview whose username must be one of `--usage-callers`. The agent
ServiceAccount can never be one, and an agent pod's broker token is bound to the other audience, so an agent can read no
usage, and a controller's usage token opens nothing on the proxy. The endpoint reads the in-memory ledger only: the
broker still writes nothing to Kubernetes. Under the chart it is on exactly while `attribution.mode` is `attribute`, and
its NetworkPolicy admits port 8082 from the run controllers' pods only.

**One replica.** The usage figures depend on one ledger seeing all of a pod's traffic. The chart runs one broker; a
second replica would hold half of each pod's usage.

## Workload identity

For bedrock/vertex/foundry-entra the broker's ServiceAccount is the cloud-identity attachment point, the same recipe as
the context-controller enhancers:

- **AWS (Bedrock)** — IRSA: annotate with `eks.amazonaws.com/role-arn`; or associate EKS Pod Identity and allow the
  agent's link-local endpoint (`169.254.170.23:80`) in `egressBroker.networkPolicy.extraEgress`. Scope the role to
  `bedrock:InvokeModel*`.
- **GCP (Vertex)** — GKE Workload Identity: annotate with `iam.gke.io/gcp-service-account`; the metadata server
  (`169.254.169.254:80`) may need an `extraEgress` rule on clusters that police link-local egress. Grant
  `aiplatform.endpoints.predict` (roles/aiplatform.user).
- **Azure (Foundry, Entra mode)** — Workload Identity: annotate with `azure.workload.identity/client-id` and add the
  `azure.workload.identity/use: "true"` pod label via `egressBroker.podLabels`. Grant Cognitive Services User.

Invoke-class permissions only — the broker needs to call models, never to manage them.

## Hardening posture

- RBAC: `create tokenreviews` is the broker's entire Kubernetes surface — no Secret API access (credential files are
  mounted volumes), no writes, no leases — except under the cost limit unit, which adds `get` on pods in the agent
  namespace (no list, no watch) for the budget annotation, and under per-user credentials, which adds that same pod
  read, reads of the `ModelCredential` records and the credential Secrets (their own namespace only), and patches of the
  records' status (health) only.
- The Deployment runs under the restricted Pod Security posture like every other patchy component: non-root 65532,
  read-only root filesystem, no capabilities.
- Its NetworkPolicy admits the proxy port from the agent namespace only, the usage port (when on) from the run
  controllers only, and egress to DNS, TCP 443, and the API server.
- Every request produces one audit log line — caller pod, route, method, path, status, duration, bytes, and under
  per-user credentials the attributed person and the paying credential's fingerprint — and never bodies, headers or a
  credential.
- In-cluster traffic to the broker is plaintext HTTP carrying an audience-scoped identity token, the same posture as the
  artifact server; broker TLS or mesh mTLS is a hardening follow-up.

## Troubleshooting

- **Every claude run fails at once; the agent's transcript says `API Error: 401 Invalid bearer token`** (and the
  broker's audit line shows `status=401` on `POST /v1/messages`). With `--anthropic-auth token`, the Secret does not
  hold a valid `claude setup-token` token: most often it holds the command's whole output, or a token pasted with line
  breaks. Check its shape without printing it — a setup-token starts `sk-ant-oat01-` and holds no whitespace — and
  recreate it as [the install guide](../getting-started/install.md) shows. The broker reads the file per request, so a
  corrected Secret takes effect without a restart once the kubelet refreshes the mount.
- **A `401` on `HEAD /api/hello` with `reason=preauth_token`** is the claude CLI's connectivity probe, which carries no
  caller token. It is expected and harmless.

## Known limitations

- Codex and copilot remain in-pod-credentialed (both ship disabled). The openai route is the first step for codex;
  moving the codex runner onto it, per-user OpenAI keys and copilot are follow-ups.
- The limit counters live in memory, per replica, evicted after 24 hours: a broker restart resets every pod's totals,
  and more than one replica multiplies the effective limits (the chart runs one). A restart also empties the usage
  record; the boot time it reports keeps a controller from taking what is left for a whole run (`source: partial`).
- Bedrock streaming responses are metered by estimate only (see [Enforcement](#enforcement)); the other routes are
  metered from the usage they report.
- Bedrock/Vertex runs may report no `total_cost_usd`; cost accounting then falls back to the registry's first-party
  rates, which approximates provider pricing.
