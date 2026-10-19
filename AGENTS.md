# Agent orientation

> **Work in progress:** intent-driven development is mid-flight. Before starting, read [HANDOFF.md](HANDOFF.md) for
> the current state, the next steps, the gates and the security invariants.

Fast map of this repository so a new session can act without re-exploring. For _what_ the system must do — the
requirements, the state machine, and the end-to-end flow — read [DESIGN.md](DESIGN.md); for end-user usage read
[README.md](README.md). This file is the "where things are"; DESIGN.md is the "what it must do".

## What this is

`patchy` is an end-to-end pipeline (module `github.com/bitwise-media-group/patchy`) for triaging and remediating
security findings, using **Kubernetes custom resources as the state machine** — the
`patchy.bitwisemedia.uk/v1alpha1` kinds carry all state, etcd is the only state store, and GitHub issues are a
one-way human-facing projection. GHAS/CodeQL alerts arrive via webhook, accumulate into `Finding` resources for an
hour, get context-enhanced, then a sandboxed `claude -p` run investigates each one; high-confidence verdicts are
remediated in priority order into pull requests, everything else routes to humans. Completed findings expire on a
TTL; `FindingRollup` resources keep the all-time statistics, and (under `attribution.mode: attribute`) `UsageRollup`
resources each person's, Project's and the org's usage by day and month.

Fourteen binaries, one module. "Not monolithic" means separate binaries/deployments with shared `internal/` code:

- `cmd/integration-controller` — the single internet-facing entry point, driven by `Integration` CRs: validates
  provider webhooks (`/github/webhooks` HMAC, `/google-cloud/webhooks` Pub/Sub OIDC, `/wiz/webhooks` bearer
  token, the `/generic/{name}/webhooks` wildcard with strictly per-name HMAC; per-Integration secrets), ingests
  scanner alerts into Findings (accumulation, duplicate merge), projects Findings out as tracking issues
  (trackingRef falls back to the namespace's issues-enabled Integration for non-github sources), applies human
  signals (issue close, `/patchy` commands, PR merge) back onto Findings, and POSTs dismissal verdicts to generic
  integrations' resolver endpoints. With `--repository-images` it also keeps the runner-image sticky comment
  (what patchy did with a repository-declared agent image), reading each finding's Repository.
- `cmd/source-controller` — `Forge` + `Repository` reconcilers: validates forge credentials, pins
  each Repository's head SHA once, downloads the tarball archive at that SHA (pure HTTP, no git binary), and
  serves it from the artifact endpoint (`:9790`) agent pods fetch credential-lessly. With
  `--repository-images` on, it also reads the tree's runner-image declaration out of that stored tarball and
  pins the image to a digest exactly once beside the SHA (`status.runnerImage`, its only writer).
- `cmd/context-controller` — runs the enhancer chain (CMDB placeholder + the Cloud Asset Inventory lookup, whose
  config is the `cloudAssetInventory` block on the `google-cloud` Integration, + the AWS and Azure resource-tags
  lookups, whose configs are the `resourceTags` blocks on the `aws` and `azure` Integrations, + the generic HTTP
  fan-out — one signed synchronous call per `generic` Integration with `enhance` on, N instances, each attributed
  by its own name) over `Opened` Findings, writes enrichments/owners to status, transitions to `Enhanced`. No
  GitHub access at all; reads Integrations read-only plus generic signing Secrets by name.
- `cmd/investigation-controller` — the gate (admits accumulated, aged findings; creates the Repository and one
  immutable `Investigation` per attempt) plus the analysis scheduler (bounded concurrency, launches agent Jobs,
  routes verdicts onto the Finding).
- `cmd/remediation-controller` — queue admission (approvals/revivals), the priority scheduler, remediation agent
  Jobs, changeset push + PR via the forge write seam (the finding flow's only write credential), and hosts the
  rollup/TTL loop (and the hourly `UsageRollup` compaction).
- `cmd/agent-runner` — the in-pod coding-agent runtime: one stage per Job (`investigate` or `remediate`) via
  `claude -p` (or `codex exec`), results emitted as a `PATCHY-EVENT:` JSONL stream on stdout, beside the `PATCHY-TURN:`
  transcript and the live, never-persisted `PATCHY-OUTPUT:` command output. Never talks to GitHub or the Kubernetes API;
  a claude or codex pod holds no credential at all (model traffic goes through the egress broker, authenticated by a
  projected SA token read fresh per stage; codex gets it as `PATCHY_BROKER_TOKEN` in its own environment and the broker
  as a `patchy` provider on its command line, `harness.Gateway`), a copilot pod only its token. Two subcommands share
  the binary: `sandbox-probe` (the
  `internal/sandboxprobe` check the trusted prepare init runs: `--mode strict`, the default, for a repository image,
  exit 78 when egress is open; `--mode wait --targets k8s,imds` for every other non-evaluation Job, exit 0 either way)
  and `preflight` (what `patchy check image --run` drives).
- `cmd/egress-broker` — the egress credential broker (NOT a controller: no reconcilers, no leases): the reverse
  proxy all claude and codex model traffic goes through, one route per provider (anthropic — key or `claude
  setup-token` bearer via `--anthropic-auth` — plus bedrock SigV4, vertex OAuth, foundry key/entra, and the OpenAI
  Responses route `/openai` the codex runner runs on, off unless `--openai-api-key-file`, chart
  `egressBroker.openai.apiKey`: POST `/v1/responses` and GET `/v1/models` only, its own dialect in
  `internal/broker/openai.go` for inspection (client-run tools only, a `namespace` of them included, as codex sends),
  usage from the terminal event up to 8 MiB, and OpenAI's error envelope, a limit as `insufficient_quota`). Validates caller
  tokens via TokenReview (its only Kubernetes access but the pod read and, with per-user credentials on, the credential
  store, both below), strips them, injects/signs the model credential
  outbound, streams SSE with idle keep-alive pings, audits one slog line per request. Spend limits count tokens, or
  with `--limit-unit=cost` (chart `egressBroker.limits.unit`; default tokens) micro-USD priced by
  `model.MeasuredCostUSD` (an unpriced model at the dearest rate), each pod capped by the smaller of `--cost-per-pod`
  and its `patchy.bitwisemedia.uk/budget-micro-usd` annotation (`provider.BudgetAnnotation`), read under `cost` (or
  with per-user credentials on) by `get pods` in the agent namespace, UID-checked and cached with the verdict. With `--usage-addr`
  (chart: under `attribution.mode: attribute`; off by default) it also serves its per-Job, per-model usage ledger on a
  listener of its own (`GET /v1/usage/jobs/<job>`, port 8082) to the run controllers' ServiceAccounts only
  (`--usage-callers`, their own token audience, `internal/usagewire`). Engine in
  `internal/broker`; deployed by the chart when a claude or codex runner is enabled (claude or codex ⇒ broker;
  proxy-only, no in-pod credential mode; codex also needs `egressBroker.openai.apiKey`). One replica, always: the usage
  figures depend on it.
  Per-user model credentials (design L3; chart `egressBroker.userCredentials.mode` `off` by default, `attribute` or
  `enforce`, chart only, needing `attribution.mode: attribute` and, while claude runs anywhere, the anthropic provider,
  a setup-token org credential refused unless `orgCredentialIsPersonal: acknowledged`) render the credential store: the
  `ModelCredential` kind (release namespace, named `<sha256(owner)[:16]>-<provider>`, provider `anthropic` (API key or
  setup-token) or `openai` (API key only, `ModelCredentialKinds`), owner and provider immutable, no Secret pointer; the
  naming contract, the per-provider value rules and the fingerprint in `api/v1alpha1/modelcredential_names.go`), one
  Secret per person and provider in
  `patchy-credentials` (`cred-<record name>`, owner-hash label), the status server's write-only Secret grant (create,
  update, delete; no patch, whose response is the stored object), the broker's read grants, the
  `-credentials-self`/`-credentials-admin` ClusterRoles (virtual `modelcredentials/self` and `/all`) and three policies
  (`templates/user-credentials-policy.yaml`), and pass the broker `--user-credentials` (`internal/broker/usercred.go`).
  It then reads each caller pod's attributed-to annotation with the verdict (the budget's UID-checked get); under
  `attribute` the org credential pays and the audit line records the person, under `enforce` a person's run carries that
  person's own credential for the route's provider (one resolver and one health probe per route, anthropic and openai,
  each reading only its own provider's records; record and Secret found by derived names through the `CredentialStore`
  port: adapter
  `internal/broker/credstore`, a controller-runtime cache of the records and the Secrets' metadata only, the Secret
  read live at most once a minute while its version holds) or is refused with a distinct 403 reason, never the org
  key; an org-attributed run keeps the org credential. Health comes only from the broker's own throttled probes
  (`credhealth.go`; `AnthropicProbe`, `OpenAIProbe` `GET /v1/models`), written by a patch conditional on the
  record's resourceVersion and only while the probed Secret version (`status.secretVersion`,
  `ModelCredentialStatus.HealthFor`) is current; readiness stays the org credential's. Startup refuses a setup-token
  anthropic org credential unless acknowledged, and any route but anthropic and openai
  (docs/configuration/user-credentials.md). Under `enforce` remediation- and intent-controller
  (`--user-credentials`, `internal/credgate`) never create a run attributed to a person whose record for the run's
  harness's provider (`credgate.ProviderFor`: claude anthropic, codex openai) is not `Valid`: the Finding stays
  `Queued` with `CredentialRequired`, `status.credentialHold` and `status.credentialHoldProvider`, the Intent is
  `Blocked` (`CredentialRequired`,
  `status.credentialHold`); the issue names the person and links `--account-url`, and only a re-approval by an
  approver holding a valid credential replaces the attribution (a Finding's spawner keeps the hold on the person held
  otherwise; an approval with no GitHub id is refused while held). The status page's Account page
  (`--account-enabled`, offering `--account-providers`, which the chart sets to the providers its runners use) is where
  people store theirs. Operator guide (modes, the subscription guard, rollout
  `attribute` then `enforce`, rollback) docs/configuration/user-credentials.md; the people's guide docs/account.md.
- `cmd/egress-proxy` — OPTIONAL (default-off; chart `egressProxy`, chart-only, no kustomize component): the
  operator's Go dependency route (NOT a controller: no reconcilers, no leases). A GOPROXY-protocol reverse proxy to
  proxy.golang.org and sum.golang.org with one listener per operator network profile (`agent.networkProfiles`);
  the port is the identity, enforced by NetworkPolicy on both sides (only pods labelled
  `patchy.bitwisemedia.uk/network-<name>` reach that port). Forwards GET/HEAD only, for the profile's module
  prefixes and tagged releases (pseudo-versions only with `allowPseudoVersions`); every refusal is a 410 with a
  reason, never contacting the upstream. No cache, no SA token unless `--attribute-pods`. Step 2: a profile granted
  an operator registry (`egressProxy.registries`, `--registries`; Go, npm, pip; Artifactory, Nexus, CodeArtifact)
  is also served `/r/<registry>/…` below that registry's fixed base URL, under the grant's `packages` allowlist or
  `packagePolicy: registry`. It then HOLDS that registry's credential, and only it does: a Secret the chart projects
  into the proxy alone (re-read every 30s, `credfile`) or a CodeArtifact token it mints with its own IRSA/Pod
  Identity role (`awsca`); sent only to the registry's origin, never to an agent, a redirect hop or a file host.
  Hard denies on forge, model API (plus the broker's base-URL overrides, `--deny-hosts`) and in-cluster hosts.
  `--suspended` answers every profile request 410 (the chart's `suspendProfiles` immediate lever). Step 3: a profile
  listing `hosts` (`route: tunnel`, or beside `route: go`) is also served HTTPS `CONNECT` on the same port: a
  hostname-allowlisted tunnel to exactly those public names on 443, SNI checked against the CONNECT host, ECH refused,
  never decrypted, with its own limits and per-source byte budgets (`--tunnel-*`, chart `egressProxy.tunnel`, rendered
  only while a profile lists hosts) and a tunnel-only deny list for the API server's addresses (`--tunnel-deny-cidrs`).
  An npm or Yarn registry host is a publish channel bounded only by the upload caps. Engine in
  `internal/egressproxy` (+ `podindex` for opt-in attribution, `pyindex`, `credfile`, `awsca`); `probe --registry`
  sends one request from inside the pod; flags and the no-log-sink runbook in docs/configuration/egress-proxy.md.
- `cmd/preview-auth` — OPTIONAL (default-off; chart `previewAuth`, two stages: `permit` admits the pinned sign-in
  annotations, `require` puts every slot Ingress behind them, refused by a `lookup` until the permit stage is live;
  chart-only, no kustomize component): the preview sign-in relay (NOT a controller: no reconcilers, no leases), the
  OpenID provider every preview host's ALB signs viewers in through. It
  signs a viewer in once per browser session at Dex (one fixed redirect URI, `<relay>/dex/callback`), admits them by a
  SubjectAccessReview for get on `projects/previews` named for the Preview's `spec.project`, and hands the ALB only
  opaque, pairwise, short-lived values. Its Kubernetes access is Previews (get/list/watch, release namespace; no
  Intents), SubjectAccessReviews and get/update on its one code-ledger Lease; its keys come from a mounted Secret.
  Every authorize error is a relay page, never a redirect; a transient token failure is 503, never `invalid_grant`.
  Per-address rate limit; a background probe of every Ready preview host without credentials
  (`patchy.preview_auth.unprotected_hosts`). Operator guide `docs/intents/preview-sign-in.md` (stages, rotation,
  rollback, the probe gate before the allowlist goes), reference `docs/configuration/preview-auth.md`; the terraform
  module's `preview_auth` puts its host on the edge. Flags carry a `preview-auth-` prefix (`PATCHY_PREVIEW_AUTH_*`).
  Engine in `internal/previewauth` (core), `internal/previewauth/adapters/*` and `internal/previewauth/httpapi`.
  With `--preview-auth-gateway` (default off; chart `previewAuth.gateway`: its Pods labelled
  `patchy.bitwisemedia.uk/preview-gateway`, the one label the slot NetworkPolicy admits, and one exposure of Service
  `<fullname>-preview-gateway`: Ingress, HTTPRoute or LoadBalancer, the last optionally over its own TLS by SNI,
  `--preview-auth-gateway-tls-dir`, adapter `previewauth/adapters/tlsdir`) it is also the preview gateway, beside
  the unchanged provider endpoints: `/start` on its sign-in host (the issuer's by default) issues a single-use handoff
  code to `https://<label>.<suffix>/.patchy/callback`, which sets a per-host session; every preview host is then proxied
  to the Services `status.serving` names, held to the proxy hygiene contract (origin rule, no gateway cookie or identity
  header upstream, pairwise `X-Patchy-Viewer`, Set-Cookie and cache filtering), and its streams are closed on expiry,
  revocation or a Preview change. Reference `docs/configuration/preview-auth.md#the-preview-gateway-off-by-default`;
  operator guide (platform recipes, the release N migration rolled back by DNS, checks, security)
  `docs/configuration/preview-gateway.md`. Its host probe judges by `--preview-auth-probe-mode` (`alb`, `either`,
  `gateway`; empty derives alb or either from the gateway switch; the chart sets gateway with
  `preview.legacyIngress: false`), and in gateway mode also asks `patchy-probe-0.<suffix>`, which must be the gateway's
  404. The e2e suite builds it with the `e2edial` tag, whose `dial_e2e.go` adds `--e2e-gateway-dial-map` (slot Service
  names to loopback listeners); a released binary has no such flag (`dial.go`).
- `cmd/evaluation-controller` — OPTIONAL (default-off in the chart): remote skill-evaluation execution for
  evolve. Hosts the bearer-authenticated HTTP API (`pkg/evaluation` wire contract: workspace upload streamed to
  source-controller's `:9791` blob endpoint, submission, snapshot, SSE monitoring, cancel; OIDC verify + SAR on
  the `evaluations` resource, native verbs only) and the reconcilers: gate (Evaluation → EvaluationUnit
  children), unit scheduler (bounded concurrency over the same sandboxed agent-Job machinery; pods run
  `evolve exec-unit` and emit `EVOLVE-EVENT:` JSONL), and the TTL loop. Patchy never learns eval semantics —
  bounded summaries land on unit status, the opaque results entry in a per-unit ConfigMap.
- `cmd/intent-controller` — OPTIONAL (default-off in the chart, an opt-in kustomize component): intent-driven
  development (docs/design/intent-driven-development.md). Polls GitHub instead of taking webhooks: each
  `Project`'s intent repository (ETag listing) for issues carrying its trigger label, and each `Intent`'s issue for the
  facts its phase waits on. It plans in a read-only agent Job on the default image, posts the plan verbatim for an
  approver, builds only the approved plan in the repository's accepted image (`runnerguard.PinFor`), pushes it
  two-phase to `patchy-intent/<intent>` (create-only, never forced) and opens the PR. It closes the issue on merge.
  With `--intent-multi-repo` (slice 3; off, a multi-repository Project is not Ready and its intents are held Blocked)
  a Project lists up to eight repositories: one plan Job reads them all (the others as digest-pinned `Trees`, recorded
  on the run), each repository the plan names gets its own build run (`<intent>-bld-r<rev>-<key>-a<n>`, its own image)
  and PR, rounds are serialised per Intent and keyed by repository, and the Intent is Merged only when every PR is.
  The second forge-writing code path: in the release namespace, `secrets get` is restricted by `resourceNames` to the
  Forge Secrets. Its agent-jobs Role can get, create, update and delete any Secret in the agents namespace, including
  model keys, image-pull credentials and other Jobs' handoffs. It uses a GitHub token per operation
  (`forge.Store.TokenWith`), has no ClusterRole and no inbound surface. Only
  writer of Project status, Intent and IntentRun. Its own flags carry an `intent-` prefix (`PATCHY_INTENT_*`); it runs
  on brokered claude only (or the fake harness in dev). A Project's stage `timeout` (clamped to the stage flag) sets
  each run's wall clock and its Job deadline (timeout + 15m, never under `--intent-job-deadline`, capped by
  `--intent-job-deadline-max`, under which every stage flag plus 15m must fit; the floor is not tied to the stage
  timeouts, so a longer stage stretches only its own Jobs). With `--broker-limit-unit cost` (chart:
  `egressBroker.limits.unit: cost`; default tokens, nothing stamped) every run's immutable grant records a cost
  budget (`limits.<stage>.maxCostMicroUSD`, else the per-intent ceiling) stamped on its Job and pod as
  `provider.BudgetAnnotation`, the chart's `budget-policy.yaml` letting only intent-controller set it; with
  `--broker-cost-per-hour` a Project whose run budget exceeds half of it is not Ready (`RunBudgetOverHourlyLimit`).
- `cmd/preview-controller` — OPTIONAL (default-off): renders one `Preview` per eligible Intent PR into a fixed,
  isolated slot namespace. It uses only the Kubernetes API, with namespaced Roles and no GitHub, AWS, ECR or Secret
  access. It owns Preview status, fixed Deployment/Service/Ingress objects, bounded retries, slot cleanup and a
  periodic orphan sweep. Intent-controller projects the operator's Project preview config and recorded PR head into
  Preview spec only when explicitly enabled: one component per previewed repository (at most four, path-routed on one
  host), a repository with no PR running its default-branch head from `status.previewBases`, all derived by the one
  pure `v1alpha1.DesiredPreviewComponents` the writer and the controller's re-check share. The writer also stamps the
  Intent's Project into `spec.project` (set once, then immutable by CEL; backfilled onto older Previews), which the
  re-check holds to the Intent's, so a per-Project reader needs only Previews. The intent reconciler reads
  Preview status (one uncached get per pass, never written) to link the preview from the issue's status comment and one
  sticky comment per previewed PR, only once it checks out (`preview_view.go`: UID, observed generation, derived
  revisions, a bare `https://<intent>.<suffix>`). `status.serving` is what a Preview serves, for the preview gateway to
  route by (generation, UID, per component its Service, path and port): written at Ready, kept through a redeploy,
  cleared on a retry, Failed, Expired, release or deletion; the Service naming and the Ingress-`Prefix` path matcher it
  is matched with (`v1alpha1.PreviewComponentServiceName`, `PreviewPathMatches`, `PreviewRoute`) are shared, pinned to
  the rendered Ingress by a seeded property. Node isolation is generic (`--preview-node-selector` JSON map plus
  `--preview-taint-key`, `--preview-node-isolation=required|none`; the NodePool/NodeClass flags fill EKS's two keys, so
  renders stay byte-identical; chart `preview.nodeIsolation.{selector,mode,eksAutoMode}`). `--preview-legacy-ingress`
  (default true, today's path; chart `preview.legacyIngress`) off renders no slot Ingress, ignores target health and
  deletes the controller's own slot Ingresses. See `docs/configuration/preview-controller.md`.
- `cmd/status-server` — the human-facing status page (NOT a controller: no reconcilers, no leases): the embedded SPA +
  JSON projection of Findings/FindingRollups, SSE refetch signal, OIDC sign-in, the access-review-gated
  approve/retry/expedite/suspend/resume actions, and the user-menu demo tooling (replay → Integration `spec.replay`;
  reset → delete all pipeline CRs). Rollup statistics are public; the findings surface always requires auth. Writes SPEC
  only (`spec.approval`, `spec.suspend`, `spec.replay`) — never status, never a phase — each as a JSON merge patch of
  only the field it sets, with the resourceVersion as an optimistic lock (`specPatch`, #193), so it never takes
  ownership of fields Helm applied (field manager: the binary's name, `controller` in-image). `--agents-paused` (chart
  `agent.paused`) shows a pause banner to signed-in users only; display only. With `--intents-enabled` (chart
  `statusServer.intents.enabled`, kustomize component `status-intents`; default off) it also serves the read-only
  intents views (board, timeline, run panel; docs/intents/dashboard.md): per-Project access reviews on the virtual
  subresources `projects/intents` and `projects/transcripts`, mode oidc with both claim prefixes required, and a
  hardened browser envelope for the whole page. No intent write of any kind. With `--usage-enabled` (chart
  `statusServer.usage.enabled`; default off, chart only) it serves the Usage page and `/api/usage*`
  (internal/web/usage.go): a person's own usage keyed by their GitHub numeric id (`auth.Identity.GitHubID`, read from
  Dex `federated_claims` of the configured GitHub connector; any other identity refused), everyone's behind an access
  review on the virtual `usage/all`; startup needs `auth.Config.RequireForUsage` (oidc, prefixes, `federated:id`). Read
  only, over a cached `UsageRollup` informer. With `--account-enabled` (chart `statusServer.account.enabled`; default
  off, chart only, refused without `egressBroker.userCredentials.mode`) it serves the Account page and `/api/account*`
  (internal/web/account.go): a person sets, replaces and deletes their own model credential (POST only, JSON capped at
  8 KiB, owner stamped from the GitHub id, an access review on the virtual `modelcredentials/self`; one card per
  `--account-providers` entry, default `anthropic`, the chart adding `openai` while codex runs), and sees its
  fingerprint, health and last use, never the credential. Its store (`internal/web/credstore`) writes the Secret in
  `--credentials-namespace` by create or unconditional update and never reads one: the status server's first Secret
  write, and its only write outside spec.
- `cmd/patchy` — the workstation CLI (the only binary not deployed): `patchy <verb> <noun>` over the caller's own
  kubeconfig, no channel through any controller. get/describe/review/browse/can-i plus the five action verbs; `get
  usage` is the usage report (not a kind; `internal/usageview`), admin-only by a SelfSubjectAccessReview on `usage/all`.
  Writes SPEC only, same as status-server, as the same merge patches (`specPatch`, native verb `patch`; field manager
  the binary's file name, `patchy` or `kubectl-patchy`); enforcement of the custom verbs for direct API writes is the
  ValidatingAdmissionPolicy in `deploy/kustomize/base/admission-policy.yaml`, NOT the CLI's own SelfSubjectAccessReview
  (that is ergonomics). Five cluster-free commands ride along: `dev` (the generic-integration test harness), `mirror`
  (vendored chart/artifact mirroring over `internal/mirror`; kubeconfig flags inert, git never touched), `check image`
  (source-controller's runner-image checks through `resolve.Inspect`, plus `--run`: `agent-runner preflight` in a local
  docker shaped like the agent pod; engine in `cmd/patchy/internal/imagecheck`), `setup github-app` (creates the GitHub
  App through the manifest flow with exactly `intentperm.ForApp`'s permissions and events for
  `--security`/`--intents`/`--checks`/`--rerun-failed`, then writes its ghsecret-keyed Secret manifest to a 0600 file or
  a pipe, never to the cluster and never the private key to a terminal or a file other users can read; engine in
  `cmd/patchy/internal/ghapp`, plain net/http, no GitHub client) and `init app` (scaffolds an application repository's
  agent image, CI builds and split trusted ECR publishers from embedded templates; engine in
  `cmd/patchy/internal/scaffold`, golden trees checked as their own CI would by `mise run scaffold-check`). `setup
  previews` (`.claude/plans/preview-setup-design.md`; engine in `cmd/patchy/internal/previewsetup`; operator guide
  `docs/intents/set-up-previews.md`, its recipes' reference `docs/configuration/preview-gateway.md`) takes every
  answer as a flag and, on a terminal, asks for the ones left open (`setuppreviews_prompt.go`: each prompt names its
  flag, a credential only ever by its file's path; off a terminal, with `--non-interactive` or with
  `--dns-credentials-from-stdin`, a missing answer is a usage error naming its flags). It contacts no cluster unless
  `--detect` is given: then `cmd/patchy/internal/previewdetect` reads Nodes, the kubernetes and kube-dns Services, the
  API server's EndpointSlice, IngressClasses, GatewayClasses (unstructured), two CRDs by name, kube-system's Pod images
  (and the VPC CNI policy agent's command line), `kube-system/amazon-vpc-cni` and the release's ConfigMaps (never a
  Secret, nothing written, pinned by a fake client that fails on either; no read reaching the API server is an error),
  and its pure `Apply` fills only answers still open, even recorded ones (empty, `auto`, a false "installed"), never a
  flag's (seeded property). For `--edge tailscale` it also asks this machine's own Tailscale client for the gateway's
  device (`cmd/patchy/internal/tailnet`: `Local` runs `tailscale status --json`, read-only, bounded; pure
  `ParseStatus`/`Find` keep only what validates and find exactly one Running device of that hostname carrying the tag,
  seeded property; stdlib only, a test pins that), because the operator records its address only in the proxy's state
  Secret beside its keys; `previewdetect.ApplyTailnet` fills `--tailscale-address` while open and prints the exact DNS
  records (`previewsetup.DNSRecords`). It writes, never applies, the files
  that expose previews: `answers.yaml` (no secret; a re-run starts from it), `patchy-previews.values.yaml` (layered
  after the operator's own), `manifests/`, the relay's Dex client secret as two Secret manifests under `secrets/`
  (ghapp's 0600 writer and Secret renderer; read back on a re-run, drawn again only with `--rotate dex-client-secret`)
  and `NEXT-STEPS.md`. It refuses a sign-in host or Dex issuer on the host suffix's registrable domain (publicsuffix)
  unless `--shared-domain acknowledged` (recorded in `answers.yaml`; a terminal explains the residual risk and asks for
  the word; the values then carry `previewAuth.sharedSiteAcknowledged: <registrable domain>`, `Answers.SharedSite`;
  under the suffix is refused however acknowledged, and on Cloudflare it needs `--cloudflare-acm`), a re-run shows
  each file's diff and replaces a changed one only with `--force` or `-y` (on a terminal it asks, no by default). All
  four recipes have landed: `existing` (R4: an Ingress class or a Gateway API parent), `gateway-tls` (R2:
  the gateway's own Local LoadBalancer Service terminating TLS, `forwardedHops: 0`) and `tailscale` (R3: that Service
  as ClusterIP with the Tailscale operator's `expose`, hostname and tag annotations, a public DNS record to the device's
  100.x address, the tailnet policy as the allowlist; `--tailscale-oauth-dir` reads the operator's OAuth client from
  its files `client_id` and `client_secret`, held redacted in `TailscaleOAuth` (an auth key or API token refused), and
  writes it only into `secrets/tailscale-operator-oauth.secret.yaml`, read back on a re-run) and `cloudflare-tunnel`
  (R1: `manifests/cloudflared.yaml`, a locally-managed tunnel's cloudflared in namespace `patchy-cloudflared` (the
  recipe's own; `--cloudflared-namespace`, a new one: never the release, Dex's, cert-manager's, Tailscale's, `default`
  or a `kube-` namespace) at the digest-pinned image, both names to the gateway's Service and a 404 for the rest, egress
  only to DNS, the gateway Pods' 8082 and any public address on 443/7844 (no Cloudflare range pinned), no SA token;
  gateway exposure none, `forwardedHops: 1`, `previewAuth.ingressFrom: []` (cloudflared's Pod addresses must reach
  8082), the load balancer's `--inbound-cidr`/`--load-balancer-class` refused (the allowlist is Cloudflare's); the
  `--tunnel-credentials` file (cert.pem refused) held redacted in `TunnelCredentials`, written only into
  `secrets/cloudflared-credentials.secret.yaml` and read back on a re-run, its tunnel ID recorded; names deeper
  than a zone's first level refused unless `--cloudflare-acm`, since free Universal SSL does not cover them). R2 and R3
  write the wildcard Certificate, a namespaced ACME
  Issuer with one DNS-01 solver (`--dns`, `--dns-option key=value`, never a secret; a ClusterIssuer
  `preview-dns01-<namespace>` when the solver takes no stored credential, since cert-manager lends its ambient identity
  to no namespaced Issuer by default; Route53's `hostedZoneID` refused, the two names never sharing a zone; `--dns
  manual` names the operator's issuer instead) and, unless already running, values for cert-manager and the Tailscale
  operator at the chart versions `pins.go` pins (each, like R1's cloudflared image tag and digest, a constant under a
  renovate comment that `.github/renovate.json5`'s regex managers read, held to them by `TestRenovateTracksPins`; `mise
  run preview-pins`, CI's `preview-pins` job, checks each exists, renders with what is written, holds every golden
  Issuer and Certificate to cert-manager's CRDs at that version, unknown fields included, and runs the cloudflared pin's
  offline `ingress validate`/`ingress rule` over the R1 configs). A DNS credential comes only from
  `--dns-credentials-file` or `--dns-credentials-from-stdin`, is held redacted and lands only in
  `secrets/preview-dns01-credentials.secret.yaml` (read back on a re-run; a new one replaces it). Its golden trees are
  rendered through the chart in a test. The
  values file sets every key of `preview.nodeIsolation` that picks nodes (an EKS Auto Mode pair only with
  `--eks-auto-mode`, else cleared), so an operator's own isolation is never merged into it, and only the chart's edge
  class or an `alb` class leaves TLS to the edge (ingress-nginx or Traefik on EKS gets the Certificate).
  `check project` is `check image`'s cluster-reading sibling: a read-only Project preflight (engine in
  `cmd/patchy/internal/projectcheck`) that reads Ready/IntentNameConflict, resolves every repository over the Forge CRs,
  and judges agent and preview images with the caller's own credentials; its preview DNS, TLS and sign-in checks are
  `cmd/patchy/internal/previewcheck` (`Checker.Host`, `Checker.Auth`, `LoadSettings`), shared with the install-level
  `check previews` so the two cannot drift. Those judge DNS and TLS and, while the chart's
  `previewAuth` is on, the relay's discovery, Dex's acceptance of the relay's one redirect URI and (once sign-in is
  required) each Ready preview host's unauthenticated redirect to the relay for its own slot (`previewauth.JudgeProbe`).
  With the preview gateway on it judges the mode in force (`previewauth.ProbeModeFor` of the relay's gateway switch and
  preview-controller's legacy Ingress switch; `previewcheck/gateway.go`): DNS against `publishedAddresses` or the
  gateway's Ingress/Service status (a private edge not in this resolver is a SKIP; a Service carrying the Tailscale
  operator's `tailscale.com/expose`, under `check previews` only, is looked up through the caller's Tailscale client,
  `Config.Tailnet`, and a missing or wrong record then fails with the exact records to create), the gateway's 404 on
  `<project>-0.<suffix>` and its `/start` redirect on each Ready preview, and `preview-gateway-allowlist` (a spoofed
  `X-Forwarded-For` from a refused address must stay refused). `check previews` (`previewcheck.Install`) runs those
  for the install as a whole, before any Project exists: against `patchy-probe-0.<suffix>` and every Ready preview of
  any Project, plus `preview-config` (gateway-only, sign-in host outside the suffix, the suffix's own registrable
  domain by `x/net/publicsuffix`: shared is a WARN, INFO once the relay ConfigMap's
  `preview-shared-site-acknowledged` annotation, the chart's `previewAuth.sharedSiteAcknowledged`, names that domain),
  `preview-viewers` (the bindings of `<release>-preview-viewer`, since a SelfSubjectAccessReview cannot ask for
  someone else) and `preview-nodes` (selector and NoExecute taint together on the nodes; the selector parsed by
  preview-controller's own `preview.ParseNodeSelector`). Only a build with the `e2edial` tag (the e2e suite's,
  `cli/checkpreviews_e2e.go`) adds `check previews`' hidden `--e2e-dial-map` and `--e2e-ca-file`, which point its
  resolver, TLS dialer and HTTP client at loopback listeners and a test CA; a released binary has neither
  (`checkpreviews_net.go`).
  None reads a Secret, so never the App key. Every `check` noun renders through `cmd/patchy/internal/checkreport`
  (PASS/FAIL/SKIP lines, plus WARN and INFO, which never fail a run and only `check previews` reports; `-o json|yaml`,
  inert reasons). Builds for windows too, and ships a `kubectl-patchy` alias.
  Ships no container image: it is distributed as its own `patchy-cli` release archive (separate from the cluster
  binaries' `patchy` archive) and as a Homebrew cask in bitwise-media-group/homebrew-tap.

## Layout

```text
api/v1alpha1/       The CRD types: one <kind>_types.go per kind, transitions.go (the phase table +
                    SetPhase), conditions.go, generated deepcopy. `mise run codegen` regenerates
                    deepcopy + the CRD manifests (kustomize + helm); CI fails on drift.
cmd/<binary>/       package main, thin: build root command, delegate to internal/cli.Execute.
                    cmd/patchy is the exception — the CLI, with its own internal/ tree
                    (cli = one file per VERB, render = one file per NOUN; the two are separate
                    axes on purpose, since `get` resolves nouns through a registry at runtime).
                    cmd/patchy/internal/tools/docgen is dev tooling, not a binary: it renders
                    docs/cli from the cobra tree. It sits there because the internal rule puts
                    cmd/patchy/internal/cli out of reach of the repo root, and NOT under cmd/
                    because hack/build.sh builds everything there.
internal/           All private code, one package per concern (see "Packages" below).
pkg/                PUBLIC plugin seams only: pkg/source (finding sources), pkg/enhance (context
                    enhancers), pkg/generic (the generic integration's HTTP wire contract, importable
                    by external processes), pkg/evaluation (the remote-evaluation wire contract —
                    submissions, the in-pod EVOLVE-EVENT stream, the SSE monitor — stdlib-only,
                    imported by evolve). Exported signatures must not reference internal/ types.
deploy/             kustomize base/overlays; deploy/README.md is the operator doc. The container
                    Dockerfile.* live at the repo root (goreleaser dockers_v2 builds them).
charts/             Helm rendering of the same stack, pushed to ghcr OCI on release
                    (.github/workflows/helm.yaml): charts/patchy (CRDs + controllers) and
                    charts/patchy-config (the Integration/Forge CRs — a separate chart because
                    helm validates CRs against CRDs that must already exist). release-please
                    stamps both Chart.yaml versions. Lint/render with `mise run helm-lint`.
e2e/                SEPARATE Go module: envtest carries the CRDs, the real binaries run against it,
                    fakegithub (in-memory API) stands in at the network edge, recorded webhook
                    fixtures + the replay tool drive it (`make e2e`). envtest has no kubelet, so
                    Finding Jobs never run there; the intent tests register a fake kubelet
                    (kubelet_test.go) that runs hack/fake-agent for run-kind=intent Jobs (staging
                    the working tree, a plan Job's trees and the handoff as the prepare init
                    does) — or, for the phases a test opts into (useAgentRunner), the real
                    agent-runner driving hack/fake-agent/claude, a scripted claude CLI, so
                    in-pod behaviour such as report repair runs end to end — beside an
                    in-memory OCI registry (registry_test.go) serving the
                    repository runner image. fakegithub's refs and PR listings are per
                    repository, so intent_multirepo_test.go runs multi-repository intents end to
                    end; cluster.stoppableController restarts a binary with other flags.
                    preview_auth_test.go runs preview-auth and preview-controller together,
                    with previewauth/fakedex as a TLS Dex (--preview-auth-dex-ca-file);
                    preview_gateway_test.go does the same with the gateway on and the legacy
                    Ingress off, preview-auth built by buildTagged with the e2edial tag so
                    slot Service names dial local listeners (no kube-proxy under envtest).
                    preview_setup_test.go configures that gateway from `patchy setup previews`
                    (gateway-tls): helm renders the wizard's values, the rendered ConfigMaps
                    become the controllers' environment (cluster.serveBinaryEnv, as envFrom),
                    a viewer signs in over the gateway's own TLS, and an e2edial `patchy check
                    previews` passes against it; fakedex.Register holds Dex's redirect URIs.
                    broker_cost_test.go runs egress-broker (cluster.serveBinary: not a
                    controller, so no --namespace) on envtest's real TokenReview with
                    pod-bound tokens and a fake model upstream of heavy cache-read turns:
                    the token unit cuts the run, the cost unit admits it, a budget
                    annotation still bounds it. user_credentials_test.go runs per-user
                    credentials under enforce: the broker on a fake Anthropic upstream that
                    records each request's credential, intent-controller's CredentialRequired
                    hold, and status-server's Account page signed in through fakeoidc (Dex's
                    federated_claims for a user with a GitHub id under federated:id); it
                    asserts the hold, the resume once the broker's probe says Valid, the
                    person's key on their pod and the org key on an org pod, the fail-closed
                    refusals, a replaced key no longer paying within the 60s cap though
                    the broker's Secret cache is warm, and a deleted credential refused.
docs/ overrides/    Zensical docs site (zensical.toml at the root; patchy-branded theme in
                    docs/stylesheets/extra.css + overrides/). `mise run serve` to preview,
                    `mise run docs-build` to build; the reusable release workflow publishes it
                    to GitHub Pages (oss.bitwisemedia.uk/patchy). uv provisions zensical
                    (pyproject.toml / uv.lock). docs/cli/ is GENERATED (docgen, above) — edit
                    the commands' Short/Long/Example, not the markdown; docs/cli.md beside it
                    is the hand-written tour.
completions/        GENERATED shell completions, committed so the Homebrew cask installs them
                    as static files (executing a freshly-downloaded binary at install time
                    trips Gatekeeper). `mise run docs` writes both this and docs/cli/.
                    kubectl_complete-patchy is the exception — hand-written, and the reason
                    `kubectl patchy` completes: kubectl ignores a plugin's completion script
                    and instead runs kubectl_complete-<plugin> from PATH.
.mise/              Shared toolchain submodule (bitwise-media-group/toolchain): pinned dev CLIs +
                    the go-cli task archetype. Makefile is a one-line forwarder; repo-local tasks
                    (multi-binary build, e2e, envtest, codegen, replay) live in tasks.toml.
.claude/plans/      The living implementation plan (git-ignored).
```

## Packages (`internal/`)

- `controller/` — one engine per controller binary; the binaries are thin wiring over these:
  `controller/integration` (receiver, ingest, projection, human signals), `controller/source` (Forge +
  Repository reconcilers), `controller/context` (the enhancer chain), `controller/investigation` (gate +
  analysis scheduler), `controller/remediation` (spawner + priority scheduler + push/PR), `controller/rollup`
  (all-time stats + finding TTL, and the `UsageCompactor` that drops `UsageRollup` day buckets past 400 days; hosted
  by the remediation binary), `controller/evaluation` (Evaluation gate +
  unit scheduler + evaluation TTL; single writer for both evaluation kinds — their phases are local enums,
  never part of the Finding transition table), `controller/intent` (Project validation + discovery, the Intent
  phase machine and its GitHub writes, the IntentRun scheduler with launch/collect/push, the Intent TTL; one
  writer reconciler per status and its own phase table, `v1alpha1.SetIntentPhase`, beside Finding's; `doc.go` holds
  the single-writer table and the durable-settle rules; `go_requirements.go` reads the root go.mod at the build's base
  and pushed commit with `x/mod/modfile` for the PR body's requirements section, a refused, missing or non-inline
  go.mod (`ghclient.ErrNotInline`) leaving the body as before; `planBuildProfiles` hands a plan Job its builds'
  profiles as `PATCHY_BUILD_NETWORK_PROFILES`, a fact for the prompt, never a profile of its own),
  `controller/preview` (fixed slot workload renderer,
  bounded rollout, cleanup and orphan sweep; no forge access; with `--preview-auth-required`, default off, it renders
  each slot's pinned sign-in annotations from the chart's JSON, the one the slot admission policy compares, and the
  sweep reports a slot Ingress without the current or previous generation's set (auth-session-timeout judged, as the
  policy judges it, as a ceiling), deleting it only after three poll
  intervals; an Ingress write refused at admission or by RBAC waits and requeues, never spending a retry).
- `kube` — the controller-runtime manager wrapper: scheme, kubeconfig/in-cluster config, leader election,
  multi-namespace cache, health probes, logr↔slog bridge. Secrets are never cached; a controller that needs only
  its own ConfigMaps confines their informer by label (`ConfigMapSelector`; intent-controller does, so the Finding
  transcripts beside them are never in its memory).
- `forge` — the shared forge seam: resolve a repository URL to its covering `Forge` CR (host → orgs → repo
  regexes; most-constrained wins) and mint scoped read/write tokens. Consumers: source (read), remediation
  (write), intent (a token per operation, one repository and one permission each: `TokenWith`; opening a pull
  request also reads contents, which GitHub needs in a private repository). `ghclient`, `ghpush`, `ghsecret` sit
  beneath it.
- `schedule`, `priority`, `stats` — pure logic: slot picking with anti-starvation aging, the 0–100 scheduling
  score, rollup delta arithmetic + OTel taps. `stats/usage.go` is the `UsageRollup` side: `UsageDeltaFrom` (trusted
  figures only from a final `measured`/`partial` usage, the pod's own report kept apart as reported), `ApplyUsage`
  (the `u:<run uid>` ledger, UTC day and month buckets, the bounded top-intents list), `CompactUsage` and
  `patchy.usage.tokens`/`patchy.usage.cost` (`user` attribute); seeded properties.
- `labels` — the trimmed human-facing label vocabulary the issue projection renders (one-way; never parsed back
  into state).
- `templates` — the finding handoff/issue body, the stage prompts (investigate, remediate, and the intent plan and
  build), and the PR body, rendered from embedded templates with golden tests. Also the intent side, which
  intent-controller renders:
  the plan comment, which shows the plan report VERBATIM (its exact bytes) in a ```markdown block whose fence no line of
  it can close, only patchy's header outside it, and refuses (`ErrPlanRefused`, with a notice to post instead) a plan
  over GitHub's comment limit, not UTF-8, or holding characters no block can show (tag characters, bidi controls, stray
  variation selectors: `planview.go`), whose header otherwise counts what the block hides at a glance (other invisible
  characters less an emoji's, lines past 100 columns, long blank runs); `Sanitize`/`SanitizeInline`, the pass every
  OTHER piece of agent text bound for GitHub takes (hidden markup shown literally, tables as text, characters that
  render as nothing as their code point; mentions, issue references on any host and so closing keywords made inline
  code); both with seeded properties checked against goldmark as a stand-in for GitHub; and the intent status comment,
  notices, the preview comment (`intent_preview.go`: the host linked only while live and only as bare DNS labels, the
  preview controller's message never shown), PR body ("Part of", never a closing keyword), PR title and commit message,
  over plain values. The last three can land on the default branch as plain text (a squash commit copies the title
  and, if the repo says so, the body), where inline code protects nothing, so each also `defang`s every reference and
  mention, code spans included; seeded properties read them raw. The Finding side takes the same passes
  (`finding_text.go`): the remediation report in the PR body is `Sanitize`d and `defang`ed, a stage report comment on
  the tracking issue `Sanitize`d, the agent's commit message `defang`ed (`RemediationCommitMessage`), and a PR body
  that cannot render falls back to patchy's own words (`PRBodyFallback`), never the report; `fitReport` cuts a report
  to GitHub's size limit BEFORE cleaning it, so a cut can never leave a code span unclosed. The intent PR body also
  lists the root go.mod requirements a build added or changed, marking any at another version than the plan's
  `new_dependencies` and any direct one the plan did not name (#200; sanitised and defanged, agent-written), and the
  plan prompt states which network profile each repository's build would get (`BuildProfile`,
  `Encode`/`DecodeBuildProfiles`, carried as `PATCHY_BUILD_NETWORK_PROFILES`).
- `webhook`, `telemetry`, `cli`, `version` — service plumbing (the webhook server is used by
  integration-controller only).
- `attribution` — run attribution's pure core (per-user credentials design, L1): the `--attribution-mode` switch
  (`off`, the default, stamps nothing anywhere; `attribute`), the re-attribution rule (`To`: a human with no GitHub
  numeric id on record, such as every status-page or CLI approval and every evaluation submitter, makes the run the
  org's, reason `approver-unregistered` or `submitter-unregistered`), and the `patchy.bitwisemedia.uk/attributed-to`
  annotation grammar (`org` | `github-id:<id>`, `ValuePattern`, which the chart's policy repeats; seeded properties).
  Each run controller stamps a new run's immutable `spec.attributedTo` at creation per design §2 and puts its
  annotation on the Job and pod template (`jobs.Spec.AttributedTo`; investigation-controller only `org`); the intent
  issue's status comment names the latest run's person as code. A plan run is attributed from the trigger that opened
  its input revision (`status.input.trigger`), never from `lastTrigger`. Under `attribute` the chart renders up to six
  policies (`templates/attribution-policy.yaml`: Job per launcher, Pod only from the Job controller for its Job, Finding
  approval/retry/expedite `by` pinned to the writer with no `byID`, Evaluation `submitter` pinned to its creator, a
  run's `spec.attributedTo` only from its own kind's controller on CREATE, an Intent's `requestedBy.userID` only from
  intent-controller; a render test pins the Job policy's grammar to `ValuePattern`).
  Stdlib and `api/v1alpha1` only (a test pins that). Operator guide `docs/configuration/attribution.md`.
- `credgate` — the run controllers' side of per-user credentials (design L3 "Fallback", revision 1), PURE (stdlib,
  `api/v1alpha1` and the pure `internal/model` registry, a test pins that): the `--user-credentials` mode (`off`,
  `attribute`, `enforce`), `ProviderFor` (the one place a run's credential provider is decided: its harness's vendor in
  the model registry, `model.HarnessProvider`, claude anthropic, codex openai, the fake harness as claude, copilot
  none), `Judge`/`Check`/`Holds` (under `enforce` a run attributed to a person is held unless that person's own
  `ModelCredential` for the provider is `Valid`; org runs and the other modes never; seeded property), the hold's
  reasons and `Message` (naming the provider), and
  `CheckAccountURL` (`--account-url`, written into Markdown). `credgate/kubecred` is the `Lookup` over an uncached
  reader (get by derived name only). Consumers: remediation's spawner (`credential_hold.go`), intent
  (`credential_hold.go`), and integration-controller's held-Finding comment (`templates.RenderCredentialHoldComment`).
- `action` — the human-action vocabulary (the custom verbs) and the state-machine gating behind each one:
  `Apply` mutates spec (`ApplyWithID` also records a GitHub commenter's numeric id as `byID`, which attribution keys
  on), `Available` reports what a finding currently admits. `approve` is also admitted on a `Queued` finding held
  for its approver's model credential (`CredentialRequired`), replacing the approval. Owned here so the status
  server and the CLI cannot drift; `web/authz` re-exports the verb constants. It decides whether an action
  is MEANINGFUL, never whether the caller may take it. The intent verbs (replan/cancel/revise) are constants
  here too, deliberately in none of the Finding verb lists.
- `command` — the one GitHub command grammar: `/patchy <verb> [note]` on a comment's first non-blank line,
  plus the legacy `/approve` (or configured approveComment) alias, honoured only by a `Parser` whose Surface is
  the Finding issue and matched exactly as the Finding webhook handler matches it today; and which verbs each
  surface (Finding issue, intent issue, intent PR) offers, with the help replies for an unknown verb (`Help`)
  and one the current phase does not admit (`HelpFor`); `Note` is the one note rule, event aliases included.
  Pure: it imports only the standard library and `action` (a test pins that); availability and authorisation
  are the caller's. Consumed by integration-controller for Finding tracking issues (Signals records a command on
  `status.commands.pending`, slots shared by account; the projection's `settleCommands` authorises it by
  `ghclient.CanWrite`, applies it via `action.Apply`, reacts and replies at most once without listing the thread,
  then consumes it), and by intent-controller on its poll path (Surface `IntentIssue`: approve, replan, cancel).
- `web` (+ `web/auth`, `web/authz`) — the status-server backend: wire types mirroring the SPA's
  `ui/src/types.ts` (keep the two in lockstep), the action handlers, SSE broker + cache-informer watcher, and
  the embedded UI (`internal/web/ui`, Vite/Preact, single-file build embedded behind the `withui` tag; `mise run
  ui` builds it, bare `go build` compiles a stub). `auth` = who you are (OIDC/none/anonymous/unconfigured,
  cookie sessions, zero k8s imports; claim prefixes and verified email applied once, in `MapClaims`); `authz` = what
  you may do (SubjectAccessReviews for the custom verbs approve/retry/expedite/suspend/resume + native get, and
  `ProjectReviewer`'s per-Project read tiers, plus `Allowed` for a single subresource such as `projects/previews`, the
  preview viewer grant). The intents side (`intents*.go`, `envelope.go`) reads every ConfigMap
  through `guardedConfigMap` (the intent's label and a controller reference to its very owner), strips the live run
  stream per subscriber (turns, and the `PATCHY-OUTPUT` command output the tail hub keeps on its own replay ring and
  channel: tier 2 only, live only, never part of the activity), and pins its wire types to `types.ts` by parsing it
  (`TestIntentWireTypesMatchTypeScript`). The Account page (`account.go`) writes through the `CredentialStore` port
  (adapter `web/credstore`: Secret, then record, then status, never a Secret read); its wire types are pinned by
  `TestAccountWireTypesMatchTypeScript`.
- `sealed` — the shared authenticated-encryption primitives for sign-in surfaces: `PurposeKey` (HKDF-SHA256 of one
  secret, one key per purpose), `Seal`/`Open` (AES-256-GCM of a JSON value with caller-supplied AAD, so a token of one
  kind never opens as another; every failure is `ErrOpen`) and `RandomToken`. Stdlib only (a test pins that), so a pure
  core may import it; callers bound a blob's length before opening it.
- `previewauth` — the pure core of the preview sign-in relay (no HTTP server, Kubernetes client, Dex client or
  signing key; those are `cmd/preview-auth`'s adapters, below). The redirect-URI grammar (`Callbacks`: exactly
  `https://<label>.<suffix>/oauth2/idpresponse`), the per-slot ALB clients and their client secrets, the `KeyRing`
  (current and previous generation) that seals codes, access and refresh tokens, login states and relay sessions as
  `pa1.<kind>.<gen>.<blob>` with the kind and generation in the AAD and strict base64, the binding every token is
  checked against (client, slot, Preview UID, label: `Bound.Matches`), the pairwise subject, the login double-submit
  (a cookie name per sealed state), the OAuth input checks and the access-review input (`ReviewFor`, refused for a
  Preview with no Project), and the probe verdicts: `JudgeProbe` (is a preview host's unauthenticated answer the ALB's
  redirect to this relay for its own slot), `JudgeGatewayProbe` (the gateway's redirect to `/start`), `ProbeMode` and
  `ProbeExpect.Live` (which of the two the mode in force accepts), `JudgeGatewayUnknown` (the gateway's own 404 for a
  host no Preview owns, by the page marks `PageCSP` and `NoPreviewTitle` httpapi renders with) and
  `IsAllowlistRefusal`. Access tokens carry no identity. Seeded properties; stdlib, `sealed` and `api/v1alpha1`
  only (a test pins that). Its adapters, one package each under `previewauth/adapters/`: `kubeview` (the
  `PreviewLookup` over the Preview cache), `access` (the `Authorizer` over `web/authz.ProjectReviewer.Allowed`),
  `ledger` (single-use codes on one Lease, resourceVersion compare-and-swap, capped), `dex` (go-oidc client of Dex,
  lazy retried discovery, `web/auth.MapClaims` under the intents-views claims posture), `signer` (RS256, JWKS of
  current and previous key, RFC 7638 kids), `keydir` (the mounted keys Secret), `tlsdir` (the gateway's own TLS pairs,
  one projected subdirectory each, by SNI, re-read keep-last-good) and `hostprobe` (the unauthenticated host probe,
  critique F5). `previewauth/httpapi` is the HTTP surface (endpoints, pages, envelope headers, rate
  limit, audit line, OTel counters, and the preview gateway: `gateway*.go` and `streams.go`, off unless
  `Config.Gateway` is set); `previewauth/fakedex` is test support only (an in-memory Dex: `Register` makes a client's
  redirect URIs exact, and a bare authorization request gets its login page, as Dex answers `check previews`). The
  gateway's pure side is in the core: `gateway.go` (the state, handoff code, gateway login and host session kinds t, o,
  g and h, and `Handoff.Redeem`, the fixation check; a code failing `CheckState` is spent at once) and `hygiene.go` (the
  `Hosts` grammar and `Location`, `CheckOrigin`, `CheckNavigation`, `CleanRequest`, `CleanResponse`; the sign-in
  endpoints `/authorize`, `/start` and `/dex/callback` are held to `CheckNavigation`, so a same-site preview's
  background request never reaches the relay session), each with seeded properties;
  `View.Routes` are read from `status.serving`.
- `rbactest` — test support only: `WaitAllowed` polls SubjectAccessReviews until a real API server's RBAC authorizer
  has observed a grant a suite just stored, so envtest suites (action policy, authz, chart, e2e) never act inside the
  informer window and flake on a refusal.
- `usageview` — the pure projection of `UsageRollup`s into the one usage report the status page and `patchy get usage`
  render (stdlib + `api/v1alpha1`, a test pins that): a window of UTC days, per person, Project, both or everyone, each
  day stacked by Project (plus a "no project" remainder) and by run kind, top intents from month buckets, trusted,
  notional, unmetered and reported figures apart. Seeded conservation property over `stats.ApplyUsage`. Its types are
  the wire types, pinned to `ui/src/types.ts` (`TestUsageWireTypesMatchTypeScript`).
- `intentview` — the pure public projection of intents for the status page: board columns, fixed public wording
  for outcomes and block reasons (never a run's detail or a condition's message), limits with schema defaults, cost
  parsing, and `Text` (templates.VisibleText plus a cap) for every shown string. Copies of intent-controller facts
  are pinned to their originals by `internal/controller/intent/intentview_pin_test.go`.
- `ghas`, `enhancers` — the built-in `pkg/source` and `pkg/enhance` implementations.
- `generic` — the generic integration's behavior over the `pkg/generic` wire contract: the validating source
  handler (source id = the Integration's NAME; N integrations coexist) and the HMAC-signing outbound client behind
  both the verdict resolver and the enhancer call. The enhancer fan-out itself is
  `enhancers.DynamicGeneric`/`MultiEnhancer` (internal seam; `pkg/enhance` stays one-plugin-one-identity).
- `harness`, `runner` — adapted from evolve: harness builds argv, runner executes (observe-and-collect with a
  token-budget kill switch), harness parses stdout. Keep that separation. The claude postures (`claudeTools`: tool
  grammar, `--setting-sources user`, `--permission-mode dontAsk`, `--strict-mcp-config`, and `--restricted` for the
  read-only one) are probed against the REAL pinned CLI by `TestClaudePostureProbe` (`mise run claude-posture-probe`;
  CI runs it when the `CLAUDE_VERSION` pin or the posture changes): a scratch tree whose hooks, env block, settings and
  `.mcp.json` must stay inert, a scripted fake model on loopback, no credential. Codex renders the broker as a `patchy`
  Responses provider from `PromptRequest.Gateway` (`-c model_providers.patchy.*`, names of the token and placeholder
  variables only, never a value; name not "OpenAI", which would turn on zstd bodies), plus analytics, OTel metrics,
  plugins and web search off; its argv and stream are pinned from the real pinned CLI (rust-v0.153.4) run against a
  recording Responses server (`internal/harness/testdata/codex-0.153.4-*.jsonl`).
- `agentrun` — the in-pod stage flow (`investigate` | `remediate`, and the intent stages `plan` | `build`, which
  reuse investigate's and remediate's configuration and helpers and run on brokered claude only);
  `report`/`envelope` are its contracts (frontmatter schemas in, JSONL events out — a `plan` event beside the
  others at v4); `agentresult` converts envelope results onto CR status (`FromPlan` re-derives a plan from its
  report). A missing or refused report is first repaired in the agent's own session (`repair.go`: the optional
  `harness.Resumer`, claude and fake only; at most 2 bounded rounds; one transcript per stage; a writable stage's
  clone fingerprinted so a repair may change only the report and commit.sh). In the intent stages, a running
  foreground command's output is printed live as `PATCHY-OUTPUT:` chunks (`output.go`: read from the file the CLI
  keeps it in, via the optional `harness.TaskWatcher`; bounded per command and per process by constants; never
  persisted, never a turn, never idle-watchdog progress; `jobs.scanLog` skips it), and one lock serialises every
  stdout line.
- `jobs` — the Kubernetes Job the agent runs in. The isolation model lives here, and it STRENGTHENED with the
  broker: a brokered (claude, codex) pod holds no credential of any kind — its projected SA token (audience-bound,
  agent container only, never the init) is an identity document, not a capability; claude's fixed, non-secret
  `ANTHROPIC_AUTH_TOKEN` placeholder (`provider.PlaceholderAuthToken`) exists only to get past the CLI's login
  gate and is stripped by the broker (codex's is set per stage by agent-runner, `PATCHY_CODEX_PLACEHOLDER_KEY`, so a
  codex pod's env carries only `PATCHY_CODEX_BASE_URL` and the token file) — while the non-brokered copilot runner
  keeps the one SecretKeyRef; a codex evaluation Job is refused (`ErrCodexEvaluation`: evolve cannot be brokered
  yet); the init container fetches the digest-verified artifact tarball, identity-free.
  `reservedEnv` covers every credential channel plus the provider gateway names; `Runner.Env` is the per-runner
  gateway env (wins over `Config.Env`, can never name a credential). `eval.go` is the evaluation Job flavour
  (same posture; `evolve exec-unit` instead of `agent-runner` — wrapped in a capture-once token export when
  brokered — no git init, unit.json handoff); `ResultLines` is the envelope-agnostic log reader the evaluation
  collector decodes its own events from; both report `ErrLogsGone` for a finished Job whose log can never be read
  (deleted pod, removed container, a line too long to scan, or every read failing for `LogReadGrace` from the first
  failure this process saw), which every collector settles as
  its no-result outcome instead of retrying. The one exception to "no network beyond the artifact server and the
  broker" is an operator network profile (`network.go`): only on a `(remediate, remediation)` or `(build, intent)`
  Job that injects an accepted repository image, behind `Config.NetworkProfiles`, it adds the profile label, the
  egress proxy in hostAliases under `dns: none`, and the proxy-first go environment (every go variable non-empty);
  any other pair is refused (`ErrNetworkProfileRefused`), and `Create` reports the profile from the Job's label.
  A profile with registry grants also gets each granted ecosystem's environment, never empty: npm's
  (`NPM_CONFIG_REGISTRY` at the proxy's `/r/<registry>/`, `REPLACE_REGISTRY_HOST=always`, scripts off unless granted,
  no audit/fund/notifier, cache in /tmp) and pip's (`PIP_CONFIG_FILE=/dev/null`, `PIP_INDEX_URL` its `/simple/`, the
  list variables a single space, wheels only unless `sdists`), the go grant's route first in `GOPROXY`, plus
  `PATCHY_NETWORK_ECOSYSTEMS` and `PATCHY_NETWORK_GRANTS` (the `netprofile.PodGrant` JSON: names, bounded allowlist,
  switches, no URL or credential) beside `PATCHY_NETWORK_PROFILE`, for agent-runner's prompt (those three are
  reserved per-Job names). The prepare init runs `agent-runner sandbox-probe`: strict inside the injection script on a
  repository image, else the wait probe (`--mode=wait --targets=k8s,imds`, `|| echo`, never fails the Job) as the
  script's last step, both with `PATCHY_SANDBOX_PROBE_TIMEOUT` (default 20s); evaluation Jobs run neither.
- `sandboxprobe` — the negative-connectivity check behind `agent-runner sandbox-probe`, stdlib only: dial the targets
  (`internet` 1.1.1.1:443, `k8s` from `KUBERNETES_SERVICE_HOST`, `imds` 169.254.169.254:80) until three consecutive
  rounds find all closed or the window (`PATCHY_SANDBOX_PROBE_TIMEOUT`, 20s) ends. `ModeStrict` (no arguments; every
  target) exits `ExitUnenforced` (78), which `jobs` re-exports and the collectors map to `SandboxUnenforced`;
  `ModeWait` (default targets `k8s,imds`) exits 0 either way with one `SANDBOX NOT CONFIRMED` line. Window, clock and
  dialer injectable.
- `runnercfg` — the job controllers' shared agent-runner flag surface and startup validation (investigation,
  remediation, intent, evaluation): per-harness runner images and credential Secrets, harness restriction, repository
  images, network profiles, agent resources, agent DNS, `--agents-paused`, `--attribution-mode`, the `--usage-*`
  flags (`UsageMeter`), the brokered claude
  provider config, the brokered codex runner (`Brokered` from `model.HarnessBrokered`, the predicate the usage meter
  reads too; `--codex-secret*` removed, hidden and refused with the migration where codex would run;
  `--evolve-codex-image` refused, `ErrCodexEvaluations`), and
  `Resolve` (every allowlisted model runnable; then one advisory 5-second broker `/readyz` probe that logs
  `egress broker not reachable yet; brokered (claude, codex) jobs will fail until it is` and never gates startup;
  intent-controller uses
  `ResolveWithoutBrokerProbe`).
- `broker`, `provider` — the egress-broker engine (TokenReview auth + verdict cache, per-route credential
  strategies, per-user credential resolution and health probes behind `Config.UserCredentials` with its adapter in
  `broker/credstore`, SSE-safe reverse proxy, a dialect per route (`dialect.go`: inspection, usage, error envelope;
  Anthropic-shaped or OpenAI Responses), audit, and the ledger: per pod name and UID the limits' counters plus,
  per bare model id and the kind of credential that paid, observed input/output/cache-read/5m-and-1h cache-write tokens
  kept apart from the estimated shortfall, served by `UsageHandler`) and the pure logic of brokered claude runners
  (gateway env, canonical→provider model-id maps with per-provider derived defaults, the `PATCHY_MODEL_MAP` codec both
  the controllers and agentrun share). `provider.Names` is the broker's closed route table, `provider.ClaudeNames` the
  four a claude runner can front (not openai). `provider.BrokerTokenHeader` is the one definition of the caller-token
  header.
- `usagewire`, `usagemeter` — trusted usage (per-user credentials design, revision 1, L2). `usagewire` is the
  stdlib-only wire contract of the broker's usage endpoint (Job-name keying, `PodOfJob`, audience, credential kinds,
  reserved model keys). `usagemeter` is the run controllers' side, behind `--usage-measurement` (off; chart on under
  `attribution.mode: attribute`): at a terminal run whose usage is not `final`, `Settle` asks the broker for the run's
  Job (projected usage-audience token, read per call), waits up to 2 minutes while requests are in flight or the broker
  is unreachable, and decides `measured` (broker booted before the Job), `partial` or `reported` (no entry, no answer,
  or a non-brokered harness, `model.HarnessBrokered` false — claude and codex are brokered — judged from the
  controller's own launch harness, never the pod's); `Price` normalises ids
  with `model.Resolve` and prices observed tokens only at `model.PriceTableVersion` (cache writes 1.25x/2x; unknown
  model: tokens kept, no cost; subscription: notional). Each of the four controllers writes `status.usage` (or
  `status.stage.usage`) `measured`/`source`/`final` itself. `usagemetertest` is a fake broker for their tests.
- `usagerollup` — durable usage (L2): each run's own controller (the four job controllers) folds its runs into
  `UsageRollup`s. A run created with `spec.attributedTo` carries the `usage-rollup` finalizer (`Finalizers`); once
  settled with its usage final (or the meter off, or no stage), or when deleted (a run deleted unsettled is first
  measured, bounded, while the meter is on and it launched a Job: `Meter.PendingDeleted`), `Settle` applies it to the
  user, user x project and project scopes through `stats.ApplyUsage`, marks it `UsageRolledUp` and releases the
  finalizer. A run without the finalizer is not applicable. Reads and writes go through the API reader (no informer,
  no list/watch); a `Forbidden` rollup write (the chart grants it only under `attribute`) releases the run as
  `RollupForbidden`. `kinds.go` adapts the four run kinds (an IntentRun's Project from its Intent, UID-checked).
- `httpguard` — the pre-auth HTTP plumbing the broker and the egress proxy share: `SourceIP` (the connection's
  address, never a forwarding header; a rate key, never an identity), `IPLimiter` (per-source token bucket plus an
  in-flight cap, with an optional bounded wait; the broker keeps `InFlight == Burst, Wait == 0`, pinned by
  `TestBrokerLimiterEquivalence`) and `CountingWriter`. Stdlib plus `x/time/rate` only (a test pins that).
- `netprofile` — operator network profiles, PURE (stdlib only, a test pins that): `Parse` (the chart's JSON,
  unknown fields refused), `Validate` (name grammar, route `go`, ports, lowercase host, RE2 repository patterns,
  module prefixes that never admit the Go toolchain, a clean `bakedModCache`), `Set.For` (host plus RE2 matched
  ANCHORED against the lowercased `owner/name`; two matches are no match), `Prefixes.Allows` (path-element
  boundaries) and `LabelPrefix`. Step 2 adds `ParseRegistries`/`Registry.Validate` (the registries JSON: credential
  by type only, CodeArtifact host agreement, internal CIDRs clear of the always-denied set), `Grant` on `Profile` (at
  most one per ecosystem, `packages` or `packagePolicy: registry`), `CheckGrants`, the npm and pip grammars
  (`NPMName`, `NPMVersion`, `PyFilePath`, `PyProjectOfFile`) and the deny table (`DeniedHost`, `DeniedPattern`,
  `TooWideSuffixes`). Step 3 adds route `tunnel`, `Hosts` (exact names: `ValidHost`,
  `NormalizeHost`, `IPLiteral`, `TunnelHostPattern`), the `Clients` presets, `Upload` caps and `DeniedTunnelHost`
  (`DeniedHost` over every suffix plus more forges and model APIs, managed control planes, special-use names and
  publish hosts). The chart's schema, label and deny tables are pinned to it
  (`TestChartProfileSchemaMatchesValidator`, `TestChartRegistrySchemaMatchesValidator`, `TestDeniedHostMatchesChart`
  and `TestChartTunnelDenyMatchesNetprofile`, which run the chart's own deny helpers through helm,
  `TestSchemaHasNoLookahead`, `TestChartLabelPrefix`); cmd/egress-proxy's `TestChartTunnelDefaultsMatchFlags` holds
  `egressProxy.tunnel`'s defaults to the flags.
- `egressproxy` — the egress proxy's engine (no Kubernetes imports in the core, a test pins that): the pure GOPROXY
  and sumdb path parsers over x/mod, the per-request pipeline (suspended, method, body, path grammar, allowlist and
  tagged-release gate, limits, forward to a fixed https base), status mapping and header/size caps, the guarded
  dialer (tcp4, port 443, no private/link-local/reserved/denied address, judged at connect time), the audit line and
  the `patchy.egress_proxy.*` metrics. `podindex` is the opt-in client-go attribution adapter (transform-stripped
  pod cache; audit garnish, never a decision). `TestLiveGo` (`mise run live-go`) runs the real go command through it.
  Step 2's registry routes live here too (`/r/<registry>`: grant check, npm and Go routes, `RawPath` forwarding, the
  per-ecosystem status table with proxy-written error bodies, redirects only to `redirectHosts` without the
  credential, the credential-echo scan, the redacted `Credential` type and the `CredentialSource` port), with three
  subpackages: `pyindex` (PURE: regenerates a pip simple page so every link points back into the proxy, sharing
  `netprofile.PyFilePath` with the file route; seeded properties), `credfile` (the file-backed static credentials,
  polled, kubelet `..data` swap, keep-last-good) and `awsca` (the CodeArtifact token minter over aws-sdk-go-v2, with
  an API client that dials only public 443 and a Pod Identity client that dials only `169.254.170.23:80`).
  `TestLiveRegistry`/`TestLiveGoRegistry` (`mise run live-registry`) drive real npm, pip and go through it.
  `TestLiveTunnel` (`mise run live-tunnel`, step 3) runs every package manager found on PATH (pip, uv, npm, pnpm,
  Yarn Berry, cargo, dotnet, Maven, Gradle) and the claude CLI through the CONNECT tunnel with `jobs.TunnelEnv`'s
  environment against the public registries, recording upstream bytes and what each prints on a refused CONNECT.
- `artifact` — the tarball store + HTTP handler source-controller serves agent fetches from, plus the
  content-addressed workspace-blob side: sha256-named bundles (64-hex files beside the 32-hex repo tarballs),
  index rebuilt from disk on restart, last-access retention sweep, the `:9791` internal upload handler, and
  the `Client` other processes reach it with.
- `evalapi` — the evaluation controller's HTTP surface (`pkg/evaluation` contract): bearer OIDC verify (claims
  via `web/auth.MapClaims`), SAR authorization (`web/authz.ResourceReviewer`, native verbs on `evaluations`),
  workspace upload proxy, submission validation, snapshot, SSE monitor (replay + change re-emit + explicit
  `end`). `evalresults` is the per-unit results ConfigMap store (transcriptstore's sibling).
- `ghpush` — replays the agent's changeset through the GitHub Git Data API (blob → tree → commit → ref); the
  finding flow's only place a write credential is exercised. intent-controller pushes through `ghclient` directly
  (`CreateCommit`, then `CreateBranchRef`, create-only and never forced). No git binary anywhere controller-side.
- `changeset` — the pure changeset validator run before any forge call (`Validate`: pinned base, path shape,
  upsert modes/content; on a repository-declared image also the entry cap, control characters, CI
  definitions), shared so the controllers that push never import each other: remediation holds a Finding's
  changesets to `Rules` without a deny list, intent-controller an intent's to `IntentRules` (plus `.github`,
  `.patchy`, `.devcontainer` refused). Imports only the stdlib and `envelope` (a test pins that).
- `intentperm` — the one table of the GitHub App permissions patchy needs, in two views of the same rows. `For`: what
  a Project needs per repository (issues write on the intent repository; contents and pull requests write and issues
  read on each app repository, the last being the token reviewer permissions and the rate budget are read with;
  checks, statuses and actions read there too when `spec.checks.fix` is set, and actions write beside them when
  `spec.checks.rerunFailed` is too). `ForApp`: the permissions and webhook events an App registered for a set of
  features (`security`, `intents`, `checks`, `rerun-failed`, the last its own feature so check fixes never widen to a
  write on CI) must hold, metadata read included, and nothing more. Pure (stdlib + `api/v1alpha1`, a test pins that), so
  the CLI can read it without linking a GitHub client; intent-controller's Ready mints a token per grant of `For`, and
  an App manifest is built from `ForApp`, so the App holds exactly what a Project is checked for. Every token
  intent-controller mints must be a grant of `For` for its repository (`TestEveryTokenIsInTheTable`), so a new GitHub
  call cannot widen what Ready proves. One read-only exception is deliberate: an open intent's pull request in a
  repository the Project no longer lists is still read (pull requests read, plus issues read for the rate check) where
  the installation allows, so its merge counts; nothing is written there, and once refused there it is not asked again
  for a while (`readDepartedPullRequest`).
- `runnerimage` (+ `runnerimage/resolve`) — repository-declared agent runner images. The parent is the pure
  core (declaration files out of a tar.gz stream, `.patchy/agent.yaml` and the devcontainer.json fallback with
  precedence, reference grammar and strict digests, the allowlist `Policy`, PATH/ENV/VOLUME checks, the
  `Resolver` seam and `Rejection`); `resolve` is the go-containerregistry implementation source-controller
  wires in (one HEAD then digest-only calls, index enumeration, host-selected keychain, in-process cosign
  verification in bundle and legacy forms, a digest-keyed verdict cache). No Kubernetes types in either.
- `runnerguard` — the job controllers' side of repository-declared images, shared by investigation,
  remediation and intent: whether a launch may run the Repository's pin (kill switch, not revived by a human, sandbox
  breaker), the pull fail-fast gated on the Job's `runner-image-source` annotation (never on config), and the
  in-memory sandbox breaker a prepare exit 78 trips until restart (`patchy.sandbox.breaker` gauge). `PinFor`
  (beside the untouched `Pin`) is the same decision for a launch that requires the image (intent build and
  revise runs): "" only when it copied an accepted pin, a reason otherwise. It has no revival rule: an intent
  brought back by its trigger label is a new plan and a new approval, not a Finding revival.
- `mirror` — the engine behind `patchy mirror` (CLI-only, except `imageref`, which `runnerimage` also builds
  on; no controller consumes the rest): vendored mirroring of
  upstream helm charts and OCI artifacts into one or more platform registries (mirror.yaml lists them; every
  entry publishes to all, lock files record targets per registry name, and each registry may carry its own
  wholesale `signing` override — `sync --registry` restricts a run). One concern per subpackage: `spec` (the
  mirror.yaml/manifest/lock schema + glob discovery — the tree is the registry of entries), `imageref`,
  `semverpick` (constraints + cooldown walks), `yamledit` (comment-preserving byte-splice edits, never
  re-encode), `helmchart` (pull/extract/tree-diff/push), `render` (byte-stable `helm template` equivalent —
  helm SDK pinned; upgrades are deliberate, the validate gate byte-compares output), `discover` (4-pass image
  discovery), `distro` (distribution-manifests → generated images.extra.yaml sidecar), `verify`/`sign`
  (shell out to the cosign binary: upstream provenance in, bundle-referrers signatures out; KMS support is
  the installed cosign build's), `scan` (pluggable: osv + grype/kubescape, all shell-outs; every image
  scanner defaults off, and an image scan with none enabled is a hard error), `allowlist` (derive with
  keep-expiry/drop-stale rules). The engine never touches git — the calling pipeline owns
  commits/branches/PRs — and only the upgrade path may consult the wall clock, keeping `validate`'s
  byte-identity gate deterministic. cosign (≥ v3) and osv-scanner (v2) are runtime dependencies of the
  respective verbs, never linked.

## Conventions

- Go 1.26; cobra + viper (`PATCHY_` env prefix); `log/slog` to stderr (stdout is reserved — agent-runner's event
  stream lives there); OpenTelemetry with an otelslog fanout that never fails startup.
- Every package has a `doc.go`; every file starts with the MIT SPDX header (enforced by revive + addlicense).
- Table-driven stdlib tests, no testify; fakes over mocks; controller-runtime fake client for reconciler tests;
  envtest suites skip without `KUBEBUILDER_ASSETS` (`mise run envtest`, `mise run e2e`).
- Conventional Commits; release-please + goreleaser drive releases; `make pr` is the local gate.
- The harness/runner packages are adapted from `../evolve` (`internal/harness`, `internal/runner`) — keep their
  "harness builds argv, runner executes, harness parses stdout" separation intact.

## State machine (the heart of the system)

`api/v1alpha1` owns the phase taxonomy and legal transitions (`transitions.go`: `CanTransition`, `Terminal`,
`SetPhase`); no phase edge has two writer components. The flow:
`Opened → Enhanced → Investigating → Queued → Remediating → InReview → Remediated`, with `AwaitingApproval`
before `Queued` on holds, and `Dismissed`/`HandedOff`/`Failed` terminal (`HandedOff` revivable by approval,
`Failed` by human retry back to the pre-failure state; `spec.expedite` skips accumulation/min-age and jumps both
schedulers' queues).
Accumulation is a condition (`AccumulationComplete`), not a phase. See DESIGN.md for the full flow and
.claude/plans/ for the transition table with writers.
