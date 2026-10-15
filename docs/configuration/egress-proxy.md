# egress-proxy

The egress proxy is the operator's dependency route for agent Jobs. It is a GOPROXY-protocol reverse proxy to
`proxy.golang.org` and `sum.golang.org`, and it is optional and off by default. Like the egress broker it is not a
controller: it has no reconcilers and no leases. It forwards only `GET` and `HEAD` requests for Go modules and
checksum-database records, and only for the module path prefixes the operator allows. It holds no credential, caches
nothing and mounts no ServiceAccount token unless attribution is on.

Repositories declare nothing. The operator maps profiles to repositories in the chart's `agent.networkProfiles` values,
and a Job gets a profile only when all of these hold:

- it is a Finding's remediate Job, or an Intent's build, revise or check-fix Job;
- it runs the repository's **accepted** repository-declared image;
- its repository matches exactly one profile's `host` and `repositories` patterns. The patterns are RE2 and matched
  **anchored** against the lowercased `owner/name`, so `devthenet-labs/patchy` does not match
  `devthenet-labs/patchy-target`. A repository that matches two profiles gets none.

Investigate, plan and evaluation Jobs, and every Job on the default runner image, never get a profile. The Helm chart is
the only way to deploy it in step 1; there is no kustomize component (see [Kustomize](../deployment/kustomize.md)).

## How a request flows

```text
profile agent pod (repository image)            egress-proxy (release ns)                   upstream
  label patchy.bitwisemedia.uk/network-go=true   listener :8090 serves profile go only
  GOPROXY=http://…-egress-proxy…:8090/go,        1. method, body, path grammar
          file:///opt/go/pkg/mod/cache/download  2. module prefix allowlist, tagged-release  proxy.golang.org
  GOSUMDB=sum.golang.org                            version gate                             sum.golang.org
                                                 3. limits (queue, then 503)
                                                 4. forward to a fixed https base; status,
                                                    header and size checks on the way back
```

**The port is the identity.** Each profile has its own listener. A NetworkPolicy on the proxy admits each port only from
agent pods carrying that profile's label, in the agent namespace, and the profile's own agent-side policy lets only
those pods out to it. No header, token or source address decides anything.

## Routes

| Path (after the listener's `/go` prefix)            | What happens                                                         |
| --------------------------------------------------- | -------------------------------------------------------------------- |
| `/<module>/@v/list`, `/<module>/@latest`            | forwarded to `proxy.golang.org` when the module is allowed           |
| `/<module>/@v/<version>.info`, `.mod`, `.zip`       | forwarded when the module is allowed and the version passes the gate |
| `/sumdb/sum.golang.org/supported`                   | answered by the proxy itself: 200                                    |
| `/sumdb/sum.golang.org/latest`, `/tile/…`           | forwarded to `sum.golang.org` (not module-specific)                  |
| `/sumdb/sum.golang.org/lookup/<module>@<version>`   | forwarded when the module is allowed and the version passes the gate |
| any other well-formed path the go command could ask | `410 Gone` with a one-line reason; the upstream is never contacted   |

Module prefixes match on path-element boundaries: `golang.org/x` admits `golang.org/x/mod` but never `golang.org/xyz`. A
prefix that would admit the Go toolchain modules (`golang.org` itself, or anything under `golang.org/toolchain`) is
refused by the chart and at startup.

**The version gate.** For `.info`, `.mod`, `.zip` and checksum lookups the version must be a canonical tagged release.
Pseudo-versions (`v0.0.0-<time>-<commit>`), branch and commit queries, and arbitrary strings get `410` with the reason
`version`. A profile can set `allowPseudoVersions: true` to admit pseudo-versions of its allowed modules; it is off by
default because it reopens the channels described in the
[isolation model](../deployment/isolation.md#network-profiles-the-go-dependency-route). `list` and `@latest` carry no
version and are not gated.

**Every refusal is 410, never 403.** The go command moves on to its next `GOPROXY` entry only on 404 or 410, and it
probes every path prefix (`github.com/@v/…`) before the module itself, so any other status would abort the whole
`go get`. Over the limits the answer is `503`, deliberately not a 410, so overload is not mistaken for "not found".
Upstream statuses other than 200, 404 and 410 (redirects included) become `502`; no `Location` header ever leaves the
proxy.

## The go environment a profile Job gets

The job controller writes these into the agent container, replacing anything the image or the controller configuration
set, so neither the image's `ENV` nor its `$GOROOT/go.env` can change them:

| Variable                                            | Value                                                            | Why                                                                                                                                                                                                                                                             |
| --------------------------------------------------- | ---------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `GOPROXY`                                           | `http://<proxy>:<port>/go,file://<bakedModCache>/cache/download` | Proxy **first**, so `go get -u` and `@latest` see current versions; a refused module falls through to the image's baked module cache, so what the image holds still builds. No `direct`, no `off`. The `file://` entry is omitted when `bakedModCache` is empty |
| `GOSUMDB`                                           | `sum.golang.org`                                                 | The checksum database is reached through the proxy (`/sumdb/`); this also overrides an image's `GOSUMDB=off`                                                                                                                                                    |
| `GONOSUMDB`, `GONOPROXY`, `GOPRIVATE`, `GOINSECURE` | `patchy.invalid`                                                 | Non-empty and matching no module. An empty value falls through to `$GOROOT/go.env`, which the image controls, so a line such as `GOPRIVATE=*` there would otherwise win                                                                                         |
| `GOENV`                                             | `off`                                                            | The user go env file is never read                                                                                                                                                                                                                              |
| `GOAUTH`                                            | `off`                                                            | No credential helper runs                                                                                                                                                                                                                                       |
| `GOTOOLCHAIN`                                       | `local`                                                          | Never download a toolchain                                                                                                                                                                                                                                      |
| `GOPATH`, `GOMODCACHE`                              | `/tmp/go`, `/tmp/go/pkg/mod`                                     | The checksum database state lives under `$GOPATH/pkg/sumdb` and must be writable; the baked cache is on the read-only root                                                                                                                                      |
| `PATCHY_NETWORK_PROFILE`                            | the profile name                                                 | Tells the agent runner to describe the route in the prompt                                                                                                                                                                                                      |

`GOFLAGS` is left to the image (the scaffold sets `-mod=mod`); no go flag can turn off checksum verification.
`HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY` and `ALL_PROXY` stay reserved and blank: the route is a GOPROXY URL, not a proxy
variable.

Set `bakedModCache` to the image's `GOMODCACHE` (the scaffold and the Go example image use `/opt/go/pkg/mod`, the
default). A wrong value is harmless: the build is slower, because modules then come over the network, but still correct.

**A refused module reads as "not found".** The go command reports the last not-found error across its `GOPROXY` entries,
so with the baked cache second, a module the proxy refuses surfaces as `reading file:///…: no such file or directory`,
not as the proxy's reason. The reason is in the proxy's audit line. The agent is told that a module outside the
operator's allowlist, or a non-release version, fails this way, and to report it rather than work around it.

**Version queries on a module outside the allowlist fail.** Loading retractions needs a checksum lookup of the latest
`go.mod`, which the allowlist refuses, so `go list -m -u` on such a module fails. `go build`, `go test` and
`go mod tidy` over modules already in `go.sum` are unaffected.

## Storage cost

The baked cache is on the read-only root, so the go command copies every module a build touches (`.zip`, `.mod`, `.info`
and the extracted tree) into `/tmp/go/pkg/mod`, whether it came from the proxy or the baked cache. A profile run's first
build therefore uses roughly the baked module cache again in `/tmp`, an emptyDir counted against the Job's
ephemeral-storage limit (`agent.repositoryImages.ephemeralStorage`), on top of the build cache. Size that limit for the
profiled repositories at about **twice the baked module cache plus the build cache**, or the pod is evicted. Runs
without a profile read the baked cache in place.

## The kill switch

`egressProxy.suspendProfiles: true` pulls two levers at once:

- **Next Job.** Both job controllers apply no profile from their next Job on (`PATCHY_NETWORK_PROFILES_ENABLED=false`).
- **Immediate.** The proxy rolls (its `checksum/config` changes) and answers every profile request `410 suspended`, so
  Jobs already running can fetch nothing new from the moment the new proxy pods are Ready. Modules baked into the image
  still build.

The proxy and every policy stay rendered, so lifting the switch does not race policy attachment. Between the switch and
the new pods being Ready, the old ones still serve. `egressProxy.enabled: false` removes the proxy and every policy.

## Flags

As everywhere, each flag is also the matching `PATCHY_*` environment variable. The chart renders the proxy's ConfigMap
from its own values, never from the global `proxy` block: a corporate HTTPS proxy for the egress proxy's own upstream
traffic is **not supported** in step 1.

| Flag                               | Default                    | Purpose                                                                                       |
| ---------------------------------- | -------------------------- | --------------------------------------------------------------------------------------------- |
| `--network-profiles`               | required                   | The profile set as the chart renders it (a JSON array); refused when empty or invalid         |
| `--health-addr`                    | `:8081`                    | `/healthz` and `/readyz`; no profile may use its port                                         |
| `--go-proxy-upstream`              | `https://proxy.golang.org` | Module proxy every go route forwards to (https only)                                          |
| `--go-sumdb-upstream`              | `https://sum.golang.org`   | Checksum database every sumdb route forwards to (https only)                                  |
| `--deny-cidrs`                     | —                          | CIDRs never dialled, beside the built-in private, link-local and reserved ranges              |
| `--max-concurrent`                 | `128`                      | Requests in flight across every source                                                        |
| `--max-concurrent-per-source`      | `32`                       | Requests in flight from one source IP                                                         |
| `--requests-per-second-per-source` | `100`                      | Request rate of one source IP                                                                 |
| `--burst-per-source`               | `200`                      | Request burst of one source IP                                                                |
| `--limit-wait`                     | `30s`                      | How long a request queues for the limits above before a 503                                   |
| `--request-timeout`                | `10m`                      | Bound on one whole request, the body stream included                                          |
| `--upstream-header-timeout`        | `2m`                       | Bound on the wait for an upstream's response headers                                          |
| `--max-zip-bytes`                  | `524288000`                | Largest module zip passed through                                                             |
| `--suspended`                      | `false`                    | Answer every profile request `410 suspended` (the immediate lever)                            |
| `--attribute-pods`                 | `false`                    | Attribute source IPs to agent pods in the audit line (best-effort; needs read access to pods) |
| `--agent-namespace`                | `patchy-agents`            | Namespace of the agent pods attributed                                                        |
| `--kubeconfig`                     | in-cluster                 | Kubeconfig for attribution                                                                    |

The chart sets `--deny-cidrs` to `agent.networkPolicy.clusterCIDRs` plus `clusterDNSCIDR`. Each `egressProxy.limits`
value renders only when set; 0 or empty keeps the default above.

The proxy dials IPv4 only, on port 443 only, and refuses private, link-local, CGNAT, loopback, reserved and
`--deny-cidrs` addresses both before the dial and on the connected address, so a DNS answer that changes between lookups
cannot steer it inside the cluster. `/readyz` turns ready only after every profile listener is bound and never calls an
upstream.

## Audit and metrics

One `slog` line per request, `msg` `proxied` or `refused`, with `profile`, `port`, `source_ip`, `route` (`go` or
`sumdb`), `kind`, `module`, `version`, `method`, `status`, `upstream_status`, `bytes`, `duration`, `outcome` and
`reason`. With attribution on, `pod`, `finding`, `owner`, `run_kind`, `attempt` and `repo` are filled best-effort; they
never decide anything. Header and body values are never logged.

| `outcome`        | Meaning                                                                             |
| ---------------- | ----------------------------------------------------------------------------------- |
| `proxied`        | forwarded and answered                                                              |
| `not_allowed`    | the module is outside the profile's prefixes, or the version failed the gate        |
| `bad_path`       | the path broke the grammar                                                          |
| `bad_request`    | a body, a query or an absolute-form URI                                             |
| `method`         | not GET or HEAD                                                                     |
| `limited`        | over a limit after waiting (503)                                                    |
| `upstream_error` | the upstream failed, answered another status, or resolved to a denied address (502) |
| `too_large`      | the response passed its size cap                                                    |
| `suspended`      | the kill switch is on                                                               |

**Expect `not_allowed` lines during normal builds.** With the proxy first, every baked module outside the allowlist is
asked of the proxy, refused, and then served from the image's cache. Those lines are the fall-through working, not an
attack.

Metrics: `patchy.egress_proxy.requests` (by `profile`, `route`, `outcome`), `patchy.egress_proxy.bytes` (by `profile`,
`route`) and `patchy.egress_proxy.inflight` (by `profile`).

## Troubleshooting

- **A build fails with `no such file or directory` under `file:///…`.** The proxy refused the module or version and the
  baked cache did not have it. Find the `refused` audit line for the pod's source IP: `reason` says `prefix` (add the
  module path to the profile's `modules`, narrowly) or `version` (name a tagged release, or set `allowPseudoVersions` on
  the profile if the repository genuinely needs one).
- **`410 … suspended`.** `egressProxy.suspendProfiles` is on.
- **`502 … upstream answered <code>` or `denied_address`.** proxy.golang.org or sum.golang.org failed, or their names
  resolved to an address the proxy refuses to dial. Check the cluster's DNS and outbound HTTPS from the release
  namespace.
- **`503`.** A limit was reached and the wait ran out; raise `egressProxy.limits` if this is steady load.
- **A Job has no profile.** Check the remediation's or intent run's `runnerImage.networkProfile` and the controller's
  log line for the launch: no accepted image, no matching repository, two matching profiles, or the kill switch.
