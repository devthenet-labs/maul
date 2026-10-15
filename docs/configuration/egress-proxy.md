# egress-proxy

The egress proxy is the operator's dependency route for agent Jobs. It is optional and off by default. Like the egress
broker it is not a controller: it has no reconcilers and no leases. It forwards only `GET` and `HEAD` requests, on two
kinds of route:

- the **public Go route**: GOPROXY and checksum-database requests to `proxy.golang.org` and `sum.golang.org`, only for
  the module path prefixes the operator allows;
- **registry routes**: Go, npm or PyPI (simple index) requests below the fixed base URL of an operator-defined upstream
  registry, such as JFrog Artifactory, Sonatype Nexus or AWS CodeArtifact, under the package policy of the profile's
  grant. See [Registries](#registries).

And, for a profile that lists **hosts**, an HTTPS `CONNECT` **tunnel** to exactly those public host names on 443, for
ecosystems without a route (npm, pnpm, Yarn, pip, uv, NuGet, Cargo, Maven, Gradle). It checks the TLS ClientHello's
server name and never decrypts the tunnel. See [Tunnel](#tunnel).

It holds a credential only for the registries the operator configured (a Secret mounted read-only, or a CodeArtifact
token it mints with its own AWS identity), sends it only to that registry's origin, and never passes it, an inbound
credential or an upstream error body to an agent. It caches nothing and mounts no ServiceAccount token unless
attribution is on.

Repositories declare nothing. The operator maps profiles to repositories in the chart's `agent.networkProfiles` values,
and a Job gets a profile only when all of these hold:

- it is a Finding's remediate Job, or an Intent's build, revise or check-fix Job;
- it runs the repository's **accepted** repository-declared image;
- its repository matches exactly one profile's `host` and `repositories` patterns. The patterns are RE2 and matched
  **anchored** against the lowercased `owner/name`, so `devthenet-labs/patchy` does not match
  `devthenet-labs/patchy-target`. A repository that matches two profiles gets none.

Investigate, plan and evaluation Jobs, and every Job on the default runner image, never get a profile. The Helm chart is
the only way to deploy it; there is no kustomize component (see [Kustomize](../deployment/kustomize.md)).

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
traffic is **not supported**.

| Flag                                 | Default                            | Purpose                                                                                       |
| ------------------------------------ | ---------------------------------- | --------------------------------------------------------------------------------------------- |
| `--registries`                       | —                                  | The registries as the chart renders them (a JSON array); empty: no registry route             |
| `--network-profiles`                 | required                           | The profile set as the chart renders it (a JSON array); refused when empty or invalid         |
| `--health-addr`                      | `:8081`                            | `/healthz` and `/readyz`; no profile may use its port                                         |
| `--go-proxy-upstream`                | `https://proxy.golang.org`         | Module proxy every go route forwards to (https only)                                          |
| `--go-sumdb-upstream`                | `https://sum.golang.org`           | Checksum database every sumdb route forwards to (https only)                                  |
| `--deny-cidrs`                       | —                                  | CIDRs never dialled, beside the built-in private, link-local and reserved ranges              |
| `--max-concurrent`                   | `128`                              | Requests in flight across every source                                                        |
| `--max-concurrent-per-source`        | `32`                               | Requests in flight from one source IP                                                         |
| `--requests-per-second-per-source`   | `100`                              | Request rate of one source IP                                                                 |
| `--burst-per-source`                 | `200`                              | Request burst of one source IP                                                                |
| `--limit-wait`                       | `30s`                              | How long a request queues for the limits above before a 503                                   |
| `--request-timeout`                  | `10m`                              | Bound on one whole request, the body stream included                                          |
| `--upstream-header-timeout`          | `2m`                               | Bound on the wait for an upstream's response headers                                          |
| `--max-zip-bytes`                    | `524288000`                        | Largest module zip passed through                                                             |
| `--suspended`                        | `false`                            | Answer every profile request `410 suspended` (the immediate lever)                            |
| `--attribute-pods`                   | `false`                            | Attribute source IPs to agent pods in the audit line (best-effort; needs read access to pods) |
| `--agent-namespace`                  | `patchy-agents`                    | Namespace of the agent pods attributed                                                        |
| `--kubeconfig`                       | in-cluster                         | Kubeconfig for attribution                                                                    |
| `--registry-credentials-dir`         | `/etc/patchy/registry-credentials` | The mounted static credentials: `<registry>/token`, or `<registry>/username` and `password`   |
| `--registry-ca-dir`                  | `/etc/patchy/registry-ca`          | The registries' CA bundles, `<registry>.pem`                                                  |
| `--max-artifact-bytes`               | `536870912`                        | Largest npm tarball, pip file or Go zip on a registry route, unless the registry sets its own |
| `--credential-poll-interval`         | `30s`                              | How often the mounted credentials are re-read (at least `5s`)                                 |
| `--always-deny-cidrs`                | —                                  | CIDRs never dialled even when an internal registry lists them                                 |
| `--deny-hosts`                       | —                                  | Hosts no registry, file host or redirect hop may be, each with every name below it            |
| `--tunnel-max-concurrent`            | `512`                              | Open CONNECT tunnels across every source                                                      |
| `--tunnel-max-concurrent-per-source` | `64`                               | Open tunnels from one source IP                                                               |
| `--tunnel-rate-per-source`           | `50`                               | New tunnels per second from one source IP                                                     |
| `--tunnel-burst-per-source`          | `200`                              | New-tunnel burst of one source IP                                                             |
| `--tunnel-limit-wait`                | `10s`                              | How long a CONNECT queues for the tunnel limits before a 503                                  |
| `--tunnel-dial-timeout`              | `10s`                              | Bound on a tunnel's resolve and connect                                                       |
| `--tunnel-hello-timeout`             | `10s`                              | Bound on the wait from the 200 to a complete TLS ClientHello                                  |
| `--tunnel-idle-timeout`              | `2m`                               | A tunnel with no byte either way for this long is closed                                      |
| `--tunnel-max-duration`              | `30m`                              | Absolute lifetime of one tunnel                                                               |
| `--tunnel-max-upload-bytes`          | `1048576`                          | Client-to-upstream bytes on one tunnel (a profile's `upload.perTunnel` wins)                  |
| `--tunnel-max-download-bytes`        | `2147483648`                       | Upstream-to-client bytes on one tunnel                                                        |
| `--tunnel-source-upload-budget`      | `8388608`                          | Upload bytes of one source IP on one profile per window (a profile's `upload.perSource` wins) |
| `--tunnel-source-download-budget`    | `8589934592`                       | Download bytes of one source IP on one profile per window                                     |
| `--tunnel-source-budget-idle`        | `2h`                               | A source's budget entry expires this long after its last tunnel closed                        |
| `--tunnel-min-budget-idle`           | `2h`                               | Floor of the window above (the longest Job deadline); startup refuses less                    |
| `--tunnel-max-sources`               | `4096`                             | Budget-table entries per profile; full refuses a new source, never evicts                     |
| `--tunnel-progress-interval`         | `5m`                               | Period of an open tunnel's progress line; `0` turns it off                                    |
| `--tunnel-deny-cidrs`                | —                                  | CIDRs only the tunnel never dials (the Kubernetes API server's addresses)                     |

The chart sets `--deny-cidrs` to `agent.networkPolicy.clusterCIDRs` plus `clusterDNSCIDR`. Each `egressProxy.limits`
value renders only when set; 0 or empty keeps the default above. With registries configured it also sets `--registries`
(as `PATCHY_EGRESS_REGISTRIES`; the binary also reads `PATCHY_REGISTRIES`), `--deny-hosts` to the hosts of the egress
broker's base-URL overrides (`PATCHY_{ANTHROPIC,BEDROCK,VERTEX,FOUNDRY}_BASE_URL` in `config.extra` or
`egressBroker.config.extra`) and, when a registry is `internal`, `--always-deny-cidrs` to `clusterDNSCIDR` plus the
attribution `apiServerCIDRs`. The Kubernetes API's Service address (`KUBERNETES_SERVICE_HOST`, which kubelet injects
into every container) is always denied too.

The `--tunnel-*` flags bound the CONNECT tunnel a profile with `hosts` serves on its own port. The chart sets them from
`egressProxy.tunnel` only while some profile lists hosts; with none they change nothing. See
[Tunnel limits](#tunnel-limits).

The chart stamps the proxy's `PATCHY_LOG_LEVEL` from `egressProxy.config.logLevel`, or else the shared `config.logLevel`
but never quieter than `info`: the audit and credential lines below are written at `info`, so at the chart's default
`warn` they would all be dropped.

The proxy dials IPv4 only, on port 443 only, and refuses private, link-local, CGNAT, loopback, reserved and
`--deny-cidrs` addresses both before the dial and on the connected address, so a DNS answer that changes between lookups
cannot steer it inside the cluster. `/readyz` turns ready only after every profile listener is bound and never calls an
upstream.

## Registries

A profile can be granted one or more operator-defined **registries**: fixed https upstreams that may need a login, for
the Go (GOPROXY), npm and pip (PEP 503 simple index) protocols. The agent's package manager is pointed at
`http://<proxy>:<profile port>/r/<registry>/…`; the proxy forwards `GET` and `HEAD` below the registry's base URL and
adds the credential, which no agent pod ever holds. Registries are defined once, in `egressProxy.registries`, and
granted per profile, in `agent.networkProfiles.<profile>.registries`. Repositories still declare nothing.

```yaml
egressProxy:
  enabled: true
  cloudCredentials: podIdentity # for the codeartifact registry below
  registries:
    art-npm:
      ecosystem: npm
      url: https://acme.jfrog.io/artifactory/api/npm/npm-virtual/
      auth: { type: bearer, secretName: patchy-registry-art }
    ca-pypi:
      ecosystem: pypi
      url: https://patchy-gate-377946145366.d.codeartifact.us-east-1.amazonaws.com/pypi/pypi-store/simple/
      auth:
        type: codeartifact
        codeartifact: { domain: patchy-gate, domainOwner: "377946145366", region: us-east-1 }
agent:
  networkProfiles:
    web:
      port: 8091
      repositories: [acme/web]
      registries:
        art-npm:
          packagePolicy: registry # the registry's own curation decides
        ca-pypi:
          packages: [six, requests, urllib3, idna, certifi, charset-normalizer]
```

A profile holds **at most one grant per ecosystem** (each package manager takes one index from the environment). A
profile with grants needs no `route`; it may also keep `route: go` and its `modules` beside a go grant. Grants are
judged per listener: the port is still the identity, and a profile cannot reach another profile's registry. A registry
that is not granted to the profile answers `410` (a go-shaped path) or `403` with the outcome `not_granted`, without
naming it.

### Registry fields

| Key                     | Purpose                                                                                                                                                     |
| ----------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `ecosystem`             | `go`, `npm` or `pypi` (CodeArtifact: `npm` or `pypi`; it has no Go support)                                                                                 |
| `url`                   | The https base URL. For `pypi`, the **index URL**, ending `/simple/`. Port 443 unless `internal`                                                            |
| `auth.type`             | `none`, `bearer`, `basic` or `codeartifact` ([Credentials](#credentials))                                                                                   |
| `auth.secretName`       | `bearer` and `basic` only: the Secret the proxy reads the credential from                                                                                   |
| `auth.codeartifact`     | `codeartifact` only: `domain`, `domainOwner` (quoted), `region`, and `durationSeconds` (900-43200, default 43200)                                           |
| `fileHosts`             | `pypi`: further exact https hosts the index links files to (such as `files.pythonhosted.org`), fetched **without** the credential; at most 8                |
| `redirectHosts`         | Hosts a `3xx` may lead to, exact or `*.suffix`, followed **without** the credential ([Redirects](#redirects-and-file-hosts)); at most 8                     |
| `internal`              | Opt-in for a private or in-cluster registry: `cidrs`, `podSelector`, `targetPort` ([Private and in-cluster registries](#private-and-in-cluster-registries)) |
| `ca`                    | `{configMap, key}`: a CA bundle in a release-namespace ConfigMap; the registry then trusts **only** it                                                      |
| `privateModules`        | `go`, **required** (may be `[]`): module prefixes only this registry serves ([Go registries](#go-registries))                                               |
| `publicChecksumLookups` | `go`, **required**: whether other modules this registry serves may be checked against the public `sum.golang.org`                                           |
| `maxArtifactBytes`      | Largest tarball, wheel or zip passed through; `0` is the proxy's default (512 MiB); at most 4 GiB (large ML wheels need a raise)                            |
| `healthPackage`         | Optional: a package the credential can always read, for the credential self-check and the default `probe` package                                           |

### Grants and package policy

Each grant must say what it admits; there is no default, and the chart refuses a grant that says neither or both:

- `packages: [...]`, an **allowlist** judged before any upstream contact: Go module prefixes (matched on path-element
  boundaries, as `modules` are), npm names or `@scope/*` (every name in that scope), pip project names (compared after
  PEP 503 normalisation). The allowlist must name **transitive** dependencies too, and, for source builds, the build
  backend (see `sdists` below).
- `packagePolicy: registry`: the registry's own curation (virtual-repository include and exclude patterns, JFrog
  Curation, CodeArtifact upstreams and package-origin controls) is the policy, and the proxy forwards any well-formed
  name. It is an explicit acknowledgement in values, so it shows up in review; the
  [isolation model](../deployment/isolation.md#registry-grants) says what it gives up.

Per-ecosystem switches, each refused on another ecosystem's grant:

| Switch                | Ecosystem | Default | Effect                                                                                                              |
| --------------------- | --------- | ------- | ------------------------------------------------------------------------------------------------------------------- |
| `scripts`             | npm       | `false` | Run install scripts (`NPM_CONFIG_IGNORE_SCRIPTS=false`)                                                             |
| `allowPrerelease`     | npm       | `false` | Admit prerelease and build tarball versions (`-rc.1`, `+sha.abc`); off, only `MAJOR.MINOR.PATCH` is fetched         |
| `sdists`              | pypi      | `false` | Allow source distributions and builds (`PIP_ONLY_BINARY=:none:`); off, wheels only (`:all:`), so no `setup.py` runs |
| `allowPseudoVersions` | go        | `false` | Admit `v0.0.0-<time>-<commit>` versions, as for a profile's public route                                            |

### npm

The registry `url` is the npm registry base, such as `https://acme.jfrog.io/artifactory/api/npm/npm-virtual/` or a
CodeArtifact `…/npm/<repository>/`. The proxy accepts packuments (`/<name>`, and `/@scope%2fname` for a scoped package,
sent upstream escaped exactly so) and tarballs in three shapes (`/<name>/-/<name>-<v>.tgz`,
`/@s/<name>/-/<name>-<v>.tgz` and `/@s/<name>/-/@s/<name>-<v>.tgz`), each either bare or below the upstream's base path.
Every other path (`dist-tags`, `/-/whoami`, a version document) is refused `403 bad_path`; a refusal is a `403` with a
JSON `{"error":"egress-proxy: …"}` body, which npm prints. Two requests never reach the npm route at all: any request
with a query string (`npm search` sends `?text=…`) is refused `400 bad_request` (reason `query`, a plain-text body), and
any request that is not `GET` or `HEAD` (`npm audit` posts) `405 method`, as on every route.

npm needs no response rewriting: with `NPM_CONFIG_REPLACE_REGISTRY_HOST=always` npm fetches every tarball through the
configured registry whatever host the packument or lockfile names, and `package-lock.json` keeps the registry's
**canonical** `resolved` URLs, never the proxy's. Scoped registries in a repository's `.npmrc` (`@acme:registry=…`)
cannot be overridden from the environment and point at a host the pod cannot reach, so such a scope fails to fetch.

### pip

The registry `url` is the **index URL**, ending `/simple/`. The proxy serves `/simple/<project>/` (the PEP 503
normalised name, with the trailing slash pip always sends) and refuses the root index. The simple page is the one
response it rewrites: it parses the upstream page and generates a new one whose every link is
`/r/<registry>/f/<origin>/<path>`, where origin 0 is the registry's own host (credentialed, under the index's path) and
origins 1-n are the `fileHosts` (never credentialed). A link on any other host, with a query or userinfo, or whose path
breaks the file grammar (only `%2B` and `%21` escapes, no `.`, `..` or empty segment, a wheel, sdist or `.metadata`
filename with a PEP 440 version) is **dropped** and counted (`dropped_links`); a `sha256`/`sha384`/`sha512` hash
fragment is kept. A file request is judged again on its own, so a hand-made URL gets only what a page could have
offered. pip writes no URL into anything, and `pip-compile` must be run with `--no-emit-index-url` (the changeset guard
refuses the proxy's host in any file, below).

PyPI-shaped indexes link files on another host (`files.pythonhosted.org`, a CDN, `download.pytorch.org`): list those in
`fileHosts`, or their links are dropped and pip reports "no matching distribution". A pip refusal is a `403`; pip never
shows the body, so the proxy's audit line is where its reason is.

### Go registries

For Artifactory or Nexus Go repositories (CodeArtifact has no Go support). The proxy serves
`/r/<registry>/<module>/@v/…`, `@latest` and `/r/<registry>/sumdb/sum.golang.org/…` with the public route's grammar, its
`410` rule and the grant's `allowPseudoVersions`. A go registry must state two things, with no default:

- `privateModules` (may be `[]`): module prefixes that exist only in this registry. They become `GONOSUMDB` in the pod,
  are **never** looked up in `sum.golang.org`, and are refused (`410 private_module`) on the profile's public `/go`
  route, so a registry miss can never fall through to Google.
- `publicChecksumLookups`: whether a module this registry served, outside `privateModules`, may be checked against the
  public `sum.golang.org` on the registry's say-so. `false` is the safe answer for a registry with any in-house
  repository whose paths you have not listed.

`GOPROXY` lists the registry first, then the public route (if the profile has `route: go`), then the baked cache. The go
command picks the first entry that answers `supported` for its checksum lookups, so every lookup arrives at the registry
route and is judged in this order: a `privateModules` path is `410 private_module`; a module the profile's public
`modules` admit is forwarded to `sum.golang.org`; with `publicChecksumLookups: true`, a module the grant's `packages`
admit (or, under `packagePolicy: registry`, one whose `.mod` the registry serves) is forwarded; anything else is
`410 checksum_not_public`. The build then fails with `verifying <module>@<version>: 410 Gone`: add the prefix to
`privateModules` if it is in-house, or turn on `publicChecksumLookups` if the registry serves only public modules.

### The environment a grant gives a Job

As for the go variables above, the job controller replaces whatever the image or the controller configuration set, every
value non-empty, on profile Jobs only. `U` is the profile's proxy URL.

| Ecosystem | Variable                                                            | Value                                                                                       |
| --------- | ------------------------------------------------------------------- | ------------------------------------------------------------------------------------------- |
| go        | `GOPROXY`                                                           | `U/r/<registry>`, then `U/go` with `route: go`, then the baked cache; no `direct`, no `off` |
| go        | `GONOSUMDB`                                                         | the registry's `privateModules`, comma-joined, or `patchy.invalid`                          |
| npm       | `NPM_CONFIG_REGISTRY`                                               | `U/r/<registry>/` (the environment beats every `.npmrc`)                                    |
| npm       | `NPM_CONFIG_REPLACE_REGISTRY_HOST`                                  | `always`                                                                                    |
| npm       | `NPM_CONFIG_AUDIT`, `NPM_CONFIG_FUND`, `NPM_CONFIG_UPDATE_NOTIFIER` | `false`                                                                                     |
| npm       | `NPM_CONFIG_IGNORE_SCRIPTS`                                         | `true` unless the grant sets `scripts`                                                      |
| npm       | `NPM_CONFIG_CACHE`, `NPM_CONFIG_STRICT_SSL`                         | `/tmp/npm-cache`, `true`                                                                    |
| pypi      | `PIP_CONFIG_FILE`                                                   | `/dev/null`: no `pip.conf` of any level is read                                             |
| pypi      | `PIP_INDEX_URL`                                                     | `U/r/<registry>/simple/`                                                                    |
| pypi      | `PIP_EXTRA_INDEX_URL`, `PIP_FIND_LINKS`                             | one space (an empty value would fall through to a config file)                              |
| pypi      | `PIP_TRUSTED_HOST`                                                  | the proxy's `host:port` (pip refuses a plain-http index otherwise)                          |
| pypi      | `PIP_ONLY_BINARY`                                                   | `:all:`, or `:none:` when the grant sets `sdists`                                           |
| pypi      | `PIP_NO_INPUT`, `PIP_DISABLE_PIP_VERSION_CHECK`, `PIP_CACHE_DIR`    | `1`, `1`, `/tmp/pip-cache`                                                                  |

`PATCHY_NETWORK_ECOSYSTEMS` and `PATCHY_NETWORK_GRANTS` tell the agent runner which ecosystems the run can fetch and
under what policy, for the prompt. Each Job's changeset is refused, before any forge call, if any file's path or content
names the proxy's in-cluster host: lockfiles and configuration must keep the registry's canonical URLs.

**With `scripts: true`** install scripts run with no network but the proxy. `node-gyp` downloads Node headers from
`nodejs.org` and `prebuild-install` fetches binaries from GitHub releases; neither is reachable. Such a grant works only
with an image that bakes the headers (point `npm_config_nodedir` at them in the image's `ENV`; the profile does not own
that variable) and packages that build from source or ship their binaries in the tarball.

**With `sdists: true`, or a project installed from source** (`pip install .`, `pip install -e .`), PEP 517 build
isolation fetches the build backend (`setuptools`, `wheel`, `hatchling`, `flit-core`, `poetry-core`, …) from the same
index, so under a `packages` allowlist those must be listed too. The alternative is an image that bakes them and a build
with `--no-build-isolation`.

**Unsupported clients.** uv and poetry write the index URL into their lockfiles and yarn classic records tarball URLs,
so they are not supported; pnpm reads `npm_config_registry` and may work, but is not tested. A repository
`requirements.txt` carrying `--index-url` or `--extra-index-url` points pip at an index the pod cannot reach, so the
fetch fails and nothing leaks.

### Credentials

| `auth.type`    | Secret keys            | Sent upstream                                                                    |
| -------------- | ---------------------- | -------------------------------------------------------------------------------- |
| `none`         | —                      | no `Authorization` (a public registry behind the grant's package policy)         |
| `bearer`       | `token`                | `Authorization: Bearer <token>` (Artifactory access tokens, Nexus user tokens)   |
| `basic`        | `username`, `password` | `Authorization: Basic …` (Nexus, Artifactory, any user plus API key)             |
| `codeartifact` | —                      | npm: `Bearer <token>`; pip: `Basic` of `aws:<token>`; minted by the proxy itself |

A credential is sent **only** to the registry URL's exact `scheme://host:port`: never to a `fileHosts` host, a redirect
hop or the public checksum database, and never to an agent. An `Authorization` header the agent sends (npm sends one
when a repository's `.npmrc` has a token) is always dropped. Every non-200 answer on a registry route carries a body the
proxy wrote, so an upstream error page never reaches the agent, and every non-archive response is scanned for the
credential (`502 credential_echo` on a match).

**Static credentials** (`bearer`, `basic`). Create the Secret in the **release** namespace yourself, or with External
Secrets; the chart never creates it, and the install NOTES list the ones it expects:

```sh
kubectl -n patchy create secret generic patchy-registry-art --from-literal=token="$ARTIFACTORY_TOKEN"
kubectl -n patchy create secret generic patchy-registry-nexus --from-literal=username=patchy --from-literal=password="$NEXUS_TOKEN"
```

The chart projects each Secret's needed keys into the proxy alone, at `/etc/patchy/registry-credentials/<registry>/`,
read-only and mode `0440` (readable through the pod's group). No other pod mounts them and no ServiceAccount gains a
Secret read. A missing Secret holds a new proxy pod in `ContainerCreating`, loudly, while the surge-only rollout keeps
the old pods serving. Give the proxy a token that can read **only** the repository it names: the grammar keeps requests
below the registry's base path, but a token that can read more is more to lose (see the
[isolation model](../deployment/isolation.md#registry-grants)).

**Rotation needs no restart.** The proxy re-reads the files every 30 seconds (`--credential-poll-interval`), and kubelet
refreshes a projected Secret within its sync period, about one to two minutes on EKS defaults. A value that is empty,
over 8 KiB, or holds a control character (or a `:` in a username) is refused and the last good value kept
(`credential_invalid` in the log). To rotate:

1. create the new token at the registry;
2. update the Secret (first command below);
3. wait until **every** proxy pod logs `credential_reloaded` for the registry (second command below), or
   `patchy.egress_proxy.credential.refreshes{result=reloaded}` counts it;
4. then revoke the old token.

```sh
kubectl -n patchy create secret generic patchy-registry-art --from-literal=token="$NEW" --dry-run=client -o yaml | kubectl apply -f -
kubectl -n patchy logs -l app.kubernetes.io/name=egress-proxy --prefix --tail=-1 --since=10m --max-log-requests=10 | grep credential_reloaded
```

An upstream `401` makes the proxy re-read at once (at most once per poll interval). A plain `403` does not: it is what
registry curation answers, and reads to the agent as `registry_refused`. A revoked token can look like a run of `403`s,
so give the registry a `healthPackage` (a package the credential can always read, such as `left-pad`, `six` or a module
path): after three `403`s within a minute the proxy fetches it once and, if that answers `401` or `403`, re-reads the
credential and logs `credential_rejected`. Without `healthPackage` a revoked token shows only as `registry_refused`
lines and a failing `probe`.

### CodeArtifact

`auth.type: codeartifact` needs no Secret: the proxy mints the authorization token with its own AWS identity, set by
`egressProxy.cloudCredentials` (the chart refuses a CodeArtifact registry while it is `none`):

- **`podIdentity`**: create an EKS Pod Identity association for the `patchy-egress-proxy` ServiceAccount in the release
  namespace. The chart adds egress to the node-local Pod Identity agent (`169.254.170.23:80`; under mode `cilium` a
  `CiliumNetworkPolicy` to the `host` entity, as for the egress broker). The proxy's dialer admits that address for the
  credential call alone, never for a forwarded request.

  ```sh
  aws eks create-pod-identity-association --cluster-name <cluster> --namespace patchy \
    --service-account patchy-egress-proxy --role-arn arn:aws:iam::<account>:role/patchy-egress-proxy
  ```

- **`irsa`**: set `egressProxy.serviceAccount.annotations` to `eks.amazonaws.com/role-arn: <role ARN>` (the chart
  refuses `irsa` without it on a chart-created ServiceAccount). STS is reached on public 443, which the proxy's policy
  already allows.

Either way no long-lived secret exists, and the proxy's ServiceAccount token stays unmounted (the webhooks inject their
own token volume). The role needs exactly:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": "codeartifact:GetAuthorizationToken",
      "Resource": "arn:aws:codeartifact:<region>:<owner>:domain/<domain>"
    },
    {
      "Effect": "Allow",
      "Action": "sts:GetServiceBearerToken",
      "Resource": "*",
      "Condition": { "StringEquals": { "sts:AWSServiceName": "codeartifact.amazonaws.com" } }
    },
    {
      "Effect": "Allow",
      "Action": "codeartifact:ReadFromRepository",
      "Resource": "arn:aws:codeartifact:<region>:<owner>:repository/<domain>/<repository>"
    }
  ]
}
```

No publish, no other domain or repository. The URL's host must be the domain's endpoint,
`<domain>-<owner>.d.codeartifact.<region>.amazonaws.com` (or `….on.aws`); the chart and the proxy refuse any other, so a
typo can never send the token elsewhere. The token is minted at startup in the background (the routes answer
`503 credential_unavailable` until then, and every other route works), refreshed at half its lifetime (or 15 minutes
before expiry, if earlier) with jitter, and kept in use through mint failures (backing off from 30 seconds to 5 minutes)
until it expires. Each pod logs `credential_minted` with `expires_at`, or `credential_mint_failed` with the AWS error
code and the class of the endpoint address it tried.

**VPC interface endpoints are not supported.** With a `codeartifact.api`, `codeartifact.repositories` or `sts` interface
endpoint that has private DNS, those names resolve to private addresses, which the proxy's dialer and policy refuse:
`credential_mint_failed` then names a `private` address class, and `probe` fails. Use an endpoint without private DNS,
or none.

### Private and in-cluster registries

A registry on a private address, a non-443 port or an in-cluster name opts in with `internal`:

```yaml
egressProxy:
  registries:
    nexus-pypi:
      ecosystem: pypi
      url: https://nexus.tools.svc.cluster.local:8443/repository/pypi-all/simple/
      auth: { type: basic, secretName: patchy-registry-nexus }
      ca: { configMap: corp-root-ca, key: ca.crt }
      internal:
        cidrs: [10.100.12.34/32] # the Service's ClusterIP
        podSelector: { namespace: tools, matchLabels: { app: nexus } }
        targetPort: 8081 # the pods' container port
```

- `cidrs` are the **only** addresses the proxy dials for that registry, and only on the URL's port: IPv4, `/24` or
  narrower, and a `/32` for anything inside `agent.networkPolicy.clusterCIDRs` (so a range can never admit
  `kubernetes.default` or its neighbours). The always-denied set can never be listed: `0/8`, `127/8`, `169.254/16`
  (IMDS, Pod Identity), `224/4`, `240/4`, `clusterDNSCIDR`, the attribution `apiServerCIDRs`, and the Kubernetes API's
  Service address. Every other registry keeps the public policy (public IPv4 on 443).
- The chart renders a proxy egress rule to `cidrs` on the URL's port and, with `podSelector`, one to the registry's pods
  on `targetPort`. NetworkPolicy judges a Service's traffic **after** DNAT, by pod IP and container port, so a Service
  on 8443 that targets 8081 needs `targetPort: 8081`; the `cidrs` rule stays on the URL port, which is what the dialer
  dials.
- `ca`: a ConfigMap in the release namespace; the registry then trusts only that bundle, never the system roots. It is
  read at startup, so after editing it roll the proxy: `kubectl -n patchy rollout restart deploy/patchy-egress-proxy`.

### Redirects and file hosts

Artifactory with cloud storage and some CDN-fronted registries answer artifact requests with a `302` to a presigned URL.
A redirect is followed only to a `redirectHosts` entry, with a fresh request and **no** `Authorization`, through the
public dial policy, at most 3 hops, the presigned query kept; any other is `502 redirect_refused`, and no `Location`
header ever reaches the agent. An entry is an exact host or a `*.suffix` pattern with at least three labels after the
`*` (`*.s3.us-east-1.amazonaws.com`). The chart and the proxy refuse a pattern that is a shared cloud suffix or a parent
of one (`*.amazonaws.com`, `*.s3.amazonaws.com`, `*.googleapis.com`, `*.blob.core.windows.net`, …) or that overlaps a
denied host. `fileHosts` entries are exact hosts only, since the agent chooses the file paths there.

**Keep registries off forge and model hosts.** The registry URL host, every `fileHosts` entry and every `redirectHosts`
pattern are checked against a deny list: forges (`github.com`, `githubusercontent.com`, each profile's `host`), model
APIs (`anthropic.com`, `openai.com`, the Azure, Bedrock and Vertex endpoints, and the egress broker's base-URL
overrides), the Kubernetes API and, without `internal`, in-cluster names and IP literals. A redirect hop is checked
again at request time. A GitHub Enterprise host known only as a `Forge` resource (in `patchy-config`) is invisible to
the chart and the proxy: the job controllers check every grant's hosts against every Forge at launch and refuse the
profile (the run settles as `launch_refused`, naming the host).

### What the chart refuses

The render fails, naming the value, on: a grant of an undefined registry; two grants of one ecosystem in a profile; a
grant with neither or both of `packages` and `packagePolicy`; a CodeArtifact registry while `cloudCredentials` is
`none`, on `go`, or whose URL is not its domain's endpoint; a denied URL host; a port other than 443 without `internal`;
`internal.cidrs` overlapping the always-denied set or a non-`/32` inside the cluster ranges; `bearer`/`basic` without
`secretName`, or `none`/`codeartifact` with one; `privateModules`/`publicChecksumLookups` off `go`, and an ecosystem
switch on another ecosystem's grant; `targetPort` without `podSelector`; a `go` registry missing either checksum key; a
denied or wildcard `fileHosts` entry and a denied, shared or two-label `redirectHosts` pattern; a `pypi` URL not ending
`/simple/`; and `irsa` without the role annotation. The proxy validates all of it again at startup and refuses to start
on a grant naming an unknown registry or a grant whose ecosystem, hosts or `privateModules` differ from the registry's.

### Caps

| Response                         | Cap                                                          |
| -------------------------------- | ------------------------------------------------------------ |
| npm packument                    | 64 MiB                                                       |
| npm tarball, pip file, Go zip    | `maxArtifactBytes`, default `--max-artifact-bytes` (512 MiB) |
| pip simple page, pip `.metadata` | 16 MiB                                                       |
| other Go kinds                   | the public route's                                           |

## Tunnel

For ecosystems without a route of their own (npm, pnpm and Yarn, pip and uv, NuGet, Cargo, Maven and Gradle) a profile
can list **hosts**: exact public host names its Jobs reach over an HTTPS `CONNECT` tunnel through the proxy, on 443
only. The tunnel is served on the profile's existing port, so a tunnel adds no port, Service, NetworkPolicy or label:
the proxy answers `CONNECT` on that listener and everything else as before. Nothing tunnels until a profile lists hosts.

```yaml
agent:
  networkProfiles:
    py:
      route: tunnel # a tunnel only; route go may list hosts too, for both on one port
      port: 8092
      repositories: [acme/web]
      hosts: [pypi.org, files.pythonhosted.org] # exact names: no wildcard, no IP, https only
      clients: [pip] # environment presets: npm, yarn, pip, uv, nuget, cargo, jvm
      upload: # optional, bytes; defaults egressProxy.tunnel
        perTunnel: 1048576
        perSource: 8388608
```

A `route: tunnel` profile takes no `modules`, `bakedModCache` or `allowPseudoVersions`. A `route: go` profile may list
`hosts` too: one repository that needs Go modules and npm packages uses one profile, since a repository that matches two
profiles gets none. A profile's tunnel hosts may not be the Go route's own upstreams (`proxy.golang.org`,
`sum.golang.org`) beside `route: go`, nor a host of one of its registry grants, since the tunnel would bypass that
route's or grant's package policy.

**The tunnel is name-level.** The proxy checks the `CONNECT` line and the TLS ClientHello and then copies bytes. It
never decrypts a tunnel, so it cannot see which package or version is fetched: any package on a listed host is
fetchable, and the pull request's lockfile is where a reviewer sees what came in. See the
[isolation model](../deployment/isolation.md#tunnel-hosts) for the residue.

### How a CONNECT is judged

In order, each refusal written as a raw `HTTP/1.1 <code> egress-proxy: <reason>` status line with the same line as the
body (so a client that shows only one of them still shows the reason) and audited:

1. **Suspended**: `410 egress-proxy: profile routes are suspended by the operator`, the Go route's words.
2. **Shape** (`400`, `bad_request`): HTTP/1.1 only, the target in authority form (`host:443`, no scheme, path, userinfo
   or IPv6 bracket), no body. A port other than 443 is `403 port`.
3. **Host policy** (`403`): an IP address in any form is `ip_literal`; a name that is not an exact lowercase host is
   `bad_host`; a name on the deny list below is `hard_denied`; any other name the profile does not list exactly is
   `not_allowed`, phrased `egress-proxy: <host> is not in profile <p>'s hosts`. There is no suffix match:
   `registry.npmjs.org` never admits `x.registry.npmjs.org`.
4. **Limits** (`503 egress-proxy: over the tunnel limit (<reason>)`, `limited`): see [Tunnel limits](#tunnel-limits).
5. **Resolve and dial** (`502`): the name is resolved as a fully qualified name (the proxy pod's search domains never
   apply) and dialled through the same guarded dialer as the Go route, every address judged before the dial and again on
   the connected socket; `denied_address` when it resolves somewhere the proxy never dials, `dial_error` otherwise. The
   dial happens before the `200`, so these are readable answers, not torn connections.
6. **`200 Connection established`**, then the ClientHello: one TLS handshake record, one `server_name` equal to the
   `CONNECT` host, and no Encrypted Client Hello (or legacy ESNI). A refused hello gets one TLS alert (`sni_missing`,
   `sni_mismatch`, `ech`, `bad_hello`) and **no byte goes upstream**. Its audit line still says `status=200`: the status
   is the `CONNECT` answer, which was sent before the hello arrived; `outcome` says what happened to the hello.
7. The hello goes upstream verbatim and both directions are copied, counted against the caps, until either side closes,
   a cap trips or a deadline passes.

The listener speaks HTTP/1 only (no h2c), and a rolling restart closes the old pods' open tunnels; clients reconnect to
the new pods. `openssl s_client -proxy` sends its `CONNECT` as HTTP/1.0, which the shape check refuses (`400`, reason
`proto`, `400 egress-proxy: CONNECT must be HTTP/1.1`), so it cannot probe a tunnel; use the Python or curl probe under
[Debugging a refused host](#debugging-a-refused-host).

### The hard-deny list

A host is refused when it equals or is below an entry, whatever the profile lists. The chart refuses such a profile at
render time, and the proxy and both job controllers at startup.

| Class                               | Entries                                                                                                                                                                                                                                                                                                       |
| ----------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Forges                              | `github.com`, `githubusercontent.com`, `ghcr.io`, `githubcopilot.com`, `gitlab.com`, `bitbucket.org`, `codeberg.org`, `dev.azure.com`, `visualstudio.com`, `sr.ht`; every profile's `host`; and, at Job launch, the web host of every `Forge` resource (the run settles as `launch_refused`, naming the host) |
| Model APIs                          | `anthropic.com`, `claude.ai`, `claude.com`, `openai.com`, `chatgpt.com`, `openai.azure.com`, `services.ai.azure.com`, `cognitiveservices.azure.com`, `aiplatform.googleapis.com`, `generativelanguage.googleapis.com`, Bedrock and regional Vertex endpoints, and the egress broker's base-URL overrides      |
| Managed Kubernetes control planes   | `eks.amazonaws.com`, `azmk8s.io`, `container.googleapis.com`, `k8s.ondigitalocean.com`, `linodelke.net`, and the in-cluster `kubernetes.default…` names                                                                                                                                                       |
| Special-use and in-cluster names    | single labels, `localhost`, `local`, `internal` (so `metadata.google.internal`), `svc`, `cluster.local`, `home.arpa`, `in-addr.arpa`, `ip6.arpa`, `invalid`, `test`, `example`                                                                                                                                |
| Publish endpoints on their own host | `upload.pypi.org`, `www.nuget.org`                                                                                                                                                                                                                                                                            |

Addresses are denied whatever a name resolves to: private, link-local (IMDS, EKS Pod Identity), CGNAT, loopback,
reserved, `--deny-cidrs` and the Kubernetes Service address, as on every route, plus, for tunnels only,
`--tunnel-deny-cidrs`. The chart sets that to `attribution.apiServerCIDRs` (when listed) plus
`egressProxy.tunnel.denyCIDRs`, so the tunnel never reaches the Kubernetes API by its public address either. A public
EKS, AKS or GKE endpoint is covered by name; **an install whose API endpoint is public under another name** (a
self-managed control plane) must list its addresses in `egressProxy.tunnel.denyCIDRs`. The proxy logs a warning at
startup while profiles tunnel and that list is empty.

### Hosts per ecosystem

Copy what you need. What a tunnel cannot fetch must be baked into the image (see
[Bring your own agent image](../integrations/agent-images.md#operator-network-profiles)).

| Ecosystem                                 | Hosts                                                                                                         | Bake into the image instead                                                                          |
| ----------------------------------------- | ------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------- |
| npm, pnpm, yarn classic (publish channel) | `registry.npmjs.org`                                                                                          | the package manager itself (corepack would fetch it); Node headers (`nodejs.org`) for native modules |
| Yarn Berry (publish channel)              | `registry.yarnpkg.com` (Berry's default; or point the repository at `registry.npmjs.org`)                     | Yarn itself (or list `repo.yarnpkg.com`, which corepack fetches it from)                             |
| pip, uv                                   | `pypi.org`, `files.pythonhosted.org`                                                                          | the Python interpreter (uv downloads interpreters from GitHub, which is denied)                      |
| NuGet                                     | `api.nuget.org` (restore and vulnerability audit; never `www.nuget.org`, the push host)                       | —                                                                                                    |
| Cargo                                     | `index.crates.io`, `static.crates.io`                                                                         | git dependencies (forges are denied); the toolchain                                                  |
| Maven                                     | `repo.maven.apache.org`                                                                                       | Maven itself                                                                                         |
| Gradle                                    | `repo.maven.apache.org`, `plugins.gradle.org`, `plugins-artifacts.gradle.org` (+ `dl.google.com` for Android) | the Gradle distribution (`services.gradle.org` redirects to github.com)                              |

**The npm and Yarn rows are public publish channels.** `npm publish` is a `PUT` to `registry.npmjs.org` itself, the host
reads use, and `registry.yarnpkg.com` fronts the same registry. The token travels inside TLS, so the proxy cannot see or
refuse it: a dependency's install script, or a misled agent, that brings its own npm token can publish the working tree
as a world-readable package, bounded only by the upload caps below. PyPI and NuGet do not have this problem: their
publish hosts are separate and denied. The install NOTES name every profile that lists one of these two hosts. The real
fix is an npm-protocol route that serves only reads; until then, list an npm host only where that risk is acceptable.

### The environment a tunnel gives a Job

On a profile Job with hosts, the job controller writes these into the agent container (never the prepare init),
replacing anything the image or the controller configuration set. `P` is the profile's proxy URL
(`http://<proxy>:<port>`), `N` the no-proxy list.

| Variable                     | Value                                                                                                                                                                                  |
| ---------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `HTTPS_PROXY`, `https_proxy` | `P`                                                                                                                                                                                    |
| `NO_PROXY`, `no_proxy`       | `N`: every in-cluster endpoint the pod dials (the artifact server, the broker's routes, the proxy itself, IP literals kept), `localhost`, `127.0.0.1`, `::1`, `.svc`, `.cluster.local` |
| `PATCHY_NETWORK_ROUTES`      | `tunnel`, or `go,tunnel`                                                                                                                                                               |
| `PATCHY_NETWORK_HOSTS`       | the hosts, sorted, for the agent's prompt                                                                                                                                              |

`HTTP_PROXY`, `http_proxy`, `ALL_PROXY` and `all_proxy` stay blank, so plain http goes direct and the NetworkPolicy
drops it. `NO_PROXY` is load-bearing: Claude Code sends all its traffic to the first proxy variable set unless
`NO_PROXY` names the host, so it must name the broker. In the live fixtures, `claude -p` with this environment reached
the broker directly and the proxy saw **no traffic at all** from it, not even a refusal.

Then each preset the profile lists in `clients`, in this order. The preset names are not reserved: an image may set
them, and they are replaced only on a Job whose profile lists that client.

| Client  | Sets                                                                                                                                                                                                                                                                                                                   | Why                                                                                                 |
| ------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------- |
| `npm`   | `npm_config_https_proxy=P`, `npm_config_noproxy=N`, `npm_config_update_notifier=false`, `npm_config_fund=false`, `npm_config_audit=false`, `COREPACK_ENABLE_DOWNLOAD_PROMPT=0`; for pnpm 11, which reads only its own names, `pnpm_config_https_proxy=P`, `pnpm_config_noproxy=N`, `pnpm_config_update_notifier=false` | for npm the environment beats a project `.npmrc`; audit off keeps uploads small; no corepack prompt |
| `yarn`  | `YARN_HTTPS_PROXY=P`, `YARN_ENABLE_TELEMETRY=0`                                                                                                                                                                                                                                                                        | Yarn Berry does not reliably read `HTTPS_PROXY`                                                     |
| `pip`   | `PIP_PROXY=P`, `PIP_DISABLE_PIP_VERSION_CHECK=1`, `PIP_NO_INPUT=1`                                                                                                                                                                                                                                                     | `PIP_PROXY` beats a pip config file                                                                 |
| `uv`    | `UV_PYTHON_DOWNLOADS=never`                                                                                                                                                                                                                                                                                            | uv's interpreters come from GitHub                                                                  |
| `nuget` | `NUGET_CERT_REVOCATION_MODE=offline`, `DOTNET_CLI_TELEMETRY_OPTOUT=1`, `DOTNET_NOLOGO=1`, `DOTNET_CLI_WORKLOAD_UPDATE_NOTIFY_DISABLE=1`                                                                                                                                                                                | revocation checks go to CRL and OCSP hosts over plain http, which nothing carries                   |
| `cargo` | `CARGO_HTTP_PROXY=P`, `CARGO_REGISTRIES_CRATES_IO_PROTOCOL=sparse`                                                                                                                                                                                                                                                     | a git-config proxy would beat `HTTPS_PROXY`; the git index is on GitHub                             |
| `jvm`   | `JAVA_TOOL_OPTIONS=-Dhttps.proxyHost=<host> -Dhttps.proxyPort=<port> -Dhttp.nonProxyHosts=<N, "\|"-joined, ".svc" as "*.svc">`                                                                                                                                                                                         | the JVM ignores `HTTPS_PROXY`                                                                       |

Notes, each from the live fixtures or the clients' documentation:

- **pnpm and `pnpm-workspace.yaml`** (pnpm 11.1 and 11.28): an `httpsProxy` in the file loses to the environment, but a
  `noProxy` there is honoured, and pnpm then dials that registry directly. In a pod the NetworkPolicy drops that dial,
  so a repository can break its own install this way, but cannot reach a host the profile does not list.
- **`jvm` replaces the image's `JAVA_TOOL_OPTIONS`** (it cannot be merged): put heap and agent flags in
  `gradle.properties` (`org.gradle.jvmargs`) or `.mvn/jvm.config` instead. Every JVM prints
  `Picked up JAVA_TOOL_OPTIONS: …` on stderr; that line is expected.
- **NuGet revocation is offline**: package signatures are still verified, but a revoked signing certificate is not
  detected. Drop the `nuget` preset to accept `NU3028` restore failures instead.
- **Unverified clients.** The `nuget` and `jvm` presets have never run against a real `dotnet restore`, `mvn` or
  `gradle` (the live fixtures exist but no machine with a JDK or .NET SDK has run them yet). Maven 3.9.0 ignored the JVM
  proxy properties (MNG-7721; fixed in 3.9.1), so Maven may need a `settings.xml` proxy in the image instead. Treat both
  presets as untested until the fixtures run.

### Tunnel limits

The tunnel has its own limits, separate from the Go and registry routes' request limits, so neither starves the other.
Upload, client to host, is the exfiltration direction, so its caps are small. The defaults come from the live client
fixtures (`mise run live-tunnel`), which ran each package manager's install of a realistic dependency tree through the
tunnel and counted the bytes on the wire:

| Client | Fixture                              | Tunnels | Peak open | Upload, total | Upload per package |
| ------ | ------------------------------------ | ------- | --------- | ------------- | ------------------ |
| yarn 4 | `express`, `eslint` (162 packages)   | 312     | 50        | 602 KB        | 3.7 KB             |
| pip    | `requests`, `flask`, `boto3` (18)    | 2       | 2         | 33 KB         | 1.8 KB             |
| pnpm   | `express`, `eslint` (162)            | 50      | 50        | 189 KB        | 1.2 KB             |
| npm    | `express`, `eslint` (162), audit off | 15      | 15        | 172 KB        | 1.1 KB             |
| uv     | `requests`, `flask`, `boto3` (18)    | 6       | 4         | 9 KB          | 0.5 KB             |
| cargo  | `tokio`, `serde`, `serde_json` (48)  | 2       | 2         | 7 KB          | 0.14 KB            |

The largest single tunnel sent 21 KB (pip, one keep-alive tunnel to `pypi.org`). Yarn Berry opens a fresh tunnel for
almost every request, about 1.9 KB of TLS handshake each, which is why its upload per package is about three times
npm's.

| Value (`egressProxy.tunnel.*`)             | Default                                       | Meaning                                                                                          |
| ------------------------------------------ | --------------------------------------------- | ------------------------------------------------------------------------------------------------ |
| `maxUploadBytes`                           | 1 MiB                                         | upload on one tunnel; a profile's `upload.perTunnel` overrides it                                |
| `sourceUploadBudget`                       | 8 MiB                                         | upload of every tunnel of one source IP on one profile, per window; `upload.perSource` overrides |
| `maxDownloadBytes`, `sourceDownloadBudget` | 2 GiB, 8 GiB                                  | the same, downstream                                                                             |
| `maxConcurrent`                            | 512                                           | open tunnels across every source                                                                 |
| `maxConcurrentPerSource`                   | 64                                            | open tunnels from one source IP (pnpm 11 and yarn 4 opened 50)                                   |
| `ratePerSource`, `burstPerSource`          | 50/s, 200                                     | new tunnels from one source IP (yarn 4 opened 183 a second)                                      |
| `limitWait`                                | `10s`                                         | queueing before a `503`                                                                          |
| `dialTimeout`, `helloTimeout`              | `10s`                                         | resolve and connect; the `200` to a complete ClientHello                                         |
| `idleTimeout`, `maxDuration`               | `2m`, `30m`                                   | no byte either way; absolute lifetime of one tunnel                                              |
| `sourceBudgetIdle`                         | `2h`, or the longest Job deadline when longer | how long after a source's last tunnel its spent budget is forgotten                              |
| `maxSources`                               | 4096                                          | budget-table entries per profile; a full table refuses a new source, never evicts one            |
| `progressInterval`                         | `5m`                                          | period of an open tunnel's progress line; `"0"` turns it off                                     |
| `denyCIDRs`                                | `[]`                                          | addresses the tunnel never dials, beside `attribution.apiServerCIDRs`                            |

These keys reach the proxy only while some profile lists hosts. What the defaults allow:

- **1 MiB a tunnel** is about 50 times the largest tunnel measured, and covers a long keep-alive npm socket in a
  2,000-package install (about 135 KB).
- **8 MiB a source** is about 13 times the largest install measured. At 3.7 KB a package, a yarn tree of about 2,000
  packages sends about **7.4 MB**; npm or pnpm about 2.2 MB, pip about 3.6 MB. A yarn monorepo above that needs
  `upload.perSource` raised on its profile. 16 MiB would cover larger yarn trees out of the box, but doubles what a pod
  can upload, so the default stays 8 MiB.
- **The budget is per source IP, per profile, per proxy replica, per window.** With two replicas a pod can upload up to
  twice the budget. The window outlasts every Job (the chart holds `sourceBudgetIdle` at or above the longest
  `agent.jobDeadline` and, with intents on, `intentController.config.jobDeadline`, and the proxy refuses a shorter one
  at startup), so a pod cannot pause long enough to refill its own budget. A new pod that reuses a recent IP inherits
  what is left: that errs towards less, never more.
- **The budget is held in memory.** A proxy replica that restarts, crashes or is rolled starts with an empty table, so
  every source gets a fresh budget on that replica. Every Helm upgrade that changes the proxy's configuration rolls its
  pods. Measured live: 20 minutes after a source spent its budget, the replica that had not restarted still refused it
  (5 of 5 tunnels) while a restarted one admitted it (3 of 3). A pod that can make the proxy restart, or outlasts an
  upgrade, can upload more than the budget.
- **Over a cap** a tunnel is closed at once (`upload_cap`, `download_cap`); over a budget the next `CONNECT` is refused
  `503 egress-proxy: over the tunnel limit (source_upload_budget)`.
- **The concurrency and rate limits** keep one pod from exhausting the proxy, not data in (the byte budget does that).
  At the old 10 a second, a yarn install of 162 packages took 27 seconds instead of 2, and pnpm's 50 parallel tunnels
  queued it from 1.5 to 61 seconds; the defaults sit above both.

A keep-alive tunnel idle for 2 minutes is closed and the client reconnects (`idle_timeout`): expected, not an error.

### Tunnel audit lines and metrics

Every tunnel writes up to three kinds of line, all at `info` with `route=tunnel`, never a header or a payload byte:

- `tunnel opened`, just before the `200`: `profile`, `port`, `source_ip`, the attribution fields, `host` and
  `upstream_ip`. It is written before the client can send anything, so a container crash still leaves it in
  `kubectl logs --previous`; a pod that is deleted, evicted or replaced by a roll takes its lines with it.
- `tunnel progress`, every `progressInterval` while a tunnel stays open, with running `bytes_up`, `bytes_down` and
  `duration`.
- one close line: `tunnel` once the hello went upstream, whatever ended it, or `refused` for a refusal before that, with
  `sni`, `alpn`, `upstream_ip`, `bytes_up`, `bytes_down`, `duration`, `status`, `outcome` and `reason`.

| `outcome`                                         | Meaning                                                                                                                                                      |
| ------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `tunnelled`                                       | opened and closed normally                                                                                                                                   |
| `not_allowed`                                     | a host the profile does not list                                                                                                                             |
| `hard_denied`, `ip_literal`, `bad_host`, `port`   | the deny list, an IP address, not a host name, a port other than 443                                                                                         |
| `limited`                                         | over a limit; `reason` is `source_concurrency`, `source_rate`, `global_concurrency`, `source_upload_budget`, `source_download_budget` or `source_table_full` |
| `denied_address`, `dial_error`                    | the host resolves somewhere the proxy never dials; resolving or connecting failed                                                                            |
| `sni_missing`, `sni_mismatch`, `ech`, `bad_hello` | the ClientHello was refused; nothing went upstream                                                                                                           |
| `upload_cap`, `download_cap`                      | a cap or budget cut an open tunnel                                                                                                                           |
| `idle_timeout`, `max_duration`, `shutdown`        | ended by a deadline or a proxy shutdown                                                                                                                      |
| `bad_request`, `suspended`                        | a malformed `CONNECT` (`reason` `proto`, `target` or `body`); the kill switch                                                                                |

A request other than `CONNECT` on a port whose profile has only a tunnel (no Go route, no registry grant) is not a
tunnel line: it is a request audit line with `route=none outcome=method`, answered `405 egress-proxy: only CONNECT`.

Lines to expect, which are not an attack:

- `not_allowed` for a registry the repository's configuration names but the operator did not list (a project `.npmrc`
  `registry=` line, an extra index in `pyproject.toml`): the allowlist working;
- `idle_timeout` on long-lived npm and NuGet keep-alive tunnels;
- `hard_denied` for `api.anthropic.com` would be the claude CLI's feature checks; with the broker's environment
  (`CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`) the live fixtures saw none.

Metrics: `patchy.egress_proxy.tunnels` (by `profile`, `outcome` and `host`, the host only when it is listed, so the
cardinality is bounded), `patchy.egress_proxy.tunnel_bytes` (by `profile`, `host`, `direction`) and
`patchy.egress_proxy.tunnels_open` (by `profile`), with telemetry configured.

### Debugging a refused host

**Ask the run first.** The agent is told that when a download fails it should ask the proxy directly and put the host
and the proxy's exact line in its final message and report, which the run keeps (`IntentRun` and `Remediation`
`status.report`): `kubectl get intentrun <name> -o yaml` shows it with no log access at all. The probe it runs:

```sh
curl -sSv -x "$HTTPS_PROXY" https://<host>/ 2>&1 | grep -i egress-proxy
python3 -c 'import http.client,os,urllib.parse as u; p=u.urlsplit(os.environ["HTTPS_PROXY"]); c=http.client.HTTPSConnection(p.hostname,p.port,timeout=10); c.set_tunnel("<host>",443); c.request("HEAD","/"); print(c.getresponse().status)'
```

On a refusal the Python form fails with
`OSError: Tunnel connection failed: 403 egress-proxy: <host> is not in profile <p>'s hosts`, and it needs nothing but
the standard library.

**What each client prints by itself**, measured with the live fixtures (one listed host removed):

| Client                | Shows           | Output (trimmed)                                                                                                                                            |
| --------------------- | --------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------- |
| pip                   | code and reason | `Tunnel connection failed: 403 egress-proxy: files.pythonhosted.org is not in profile live's hosts` (after 5 retries)                                       |
| npm                   | code and reason | `npm error 403 403 egress-proxy: registry.npmjs.org is not in profile live's hosts - GET https://registry.npmjs.org/left-pad`                               |
| Yarn 4                | code only       | `YN0001: │ RequestError: Bad response: 403`                                                                                                                 |
| cargo                 | code only       | `CONNECT tunnel failed, response 403`                                                                                                                       |
| pnpm 11               | neither         | `GET https://registry.npmjs.org/left-pad error (0). Will retry in 10 seconds.` then `ERR_PNPM_META_FETCH_FAIL … fetch failed` (about 70 seconds of retries) |
| uv                    | neither         | `tunnel error: unsuccessful`, after naming the URL it was fetching                                                                                          |
| dotnet, Maven, Gradle | unverified      | the fixtures have not run (no JDK or .NET SDK on the machine that ran them)                                                                                 |

For Yarn, cargo, pnpm and uv the client's own output does not say why; the probe above does. Transcripts keep only the
first 2 KB of each tool result, so the error at the end of a long install log is often cut off: the agent is asked to
quote the proxy's line itself.

Seen live: an intent build that needed `download.pytorch.org`, which its profile did not list, ended `Failed` with the
proxy's `403` line quoted in the IntentRun's `status.detail` and `status.report`. It spent a second build attempt on the
same refusal first: a refused host does not change between attempts, so the retry cannot help (a known gap).

**On the operator side** the proxy's logs are the only record of what it decided, and they live only as long as the pod
(see [Debugging without a log sink](#debugging-without-a-log-sink)):

```sh
kubectl -n patchy logs -l app.kubernetes.io/name=egress-proxy --prefix --tail=-1 --since=2h --max-log-requests=10 | grep 'route=tunnel' | grep -v 'outcome=tunnelled'
```

### Version skew

The chart pins the proxy and both job controllers to one tag. A proxy or controller image older than the tunnel, given a
profile with `hosts`, refuses to start (it refuses unknown fields), rather than silently dropping the tunnel; only an
operator who pins an old image by hand meets this.

## Audit and metrics

One `slog` line per request, `msg` `proxied` or `refused`, with `profile`, `port`, `source_ip`, `route` (`go`, `sumdb`
or `r/<registry>`), `kind`, `module`, `version`, `method`, `status`, `upstream_status`, `bytes`, `duration`, `outcome`
and `reason`. With attribution on, `pod`, `finding`, `owner`, `run_kind`, `attempt` and `repo` are filled best-effort;
they never decide anything. Header and body values are never logged.

A registry line adds, after `reason`: `ecosystem`, `registry`, `package` (npm name or pip project; a Go registry line
keeps `module` and has no `package`), `credential` (`none`, `static` or `codeartifact`, **never** the value),
`rewritten_links` and `dropped_links` (pip pages), `redirects` and `redirect_host`. `kind` is then `packument`,
`tarball`, `page`, `file` or `metadata` (or a Go kind). A public-route line is exactly as before.

| `outcome`                | Meaning                                                                                  |
| ------------------------ | ---------------------------------------------------------------------------------------- |
| `proxied`                | forwarded and answered (an upstream `404`/`410` too, with reason `not_found`)            |
| `not_allowed`            | the package or module is outside the allowlist, or the version failed the gate           |
| `bad_path`               | the path broke the grammar                                                               |
| `bad_request`            | a body, a query or an absolute-form URI (400)                                            |
| `method`                 | not GET or HEAD; on a tunnel-only port, not `CONNECT` (`route=none`)                     |
| `limited`                | over a limit after waiting (503)                                                         |
| `upstream_error`         | the upstream failed, answered another status, or resolved to a denied address (502)      |
| `too_large`              | the response passed its size cap                                                         |
| `suspended`              | the kill switch is on                                                                    |
| `not_granted`            | the profile holds no grant for that registry                                             |
| `prerelease`             | an npm prerelease or build version without `allowPrerelease` (403)                       |
| `registry_refused`       | the registry answered `403`: its own curation refused it (npm/pip 403, go 410)           |
| `upstream_auth`          | the registry answered `401`: the credential was refused (502; the credential is re-read) |
| `credential_unavailable` | no valid credential yet, or the CodeArtifact token expired (503)                         |
| `credential_echo`        | a response contained the credential (502, or the stream aborted)                         |
| `redirect_refused`       | a redirect to a host not in `redirectHosts`, over 3 hops, or not https (502)             |
| `denied_host`            | a redirect hop to a denied host (502)                                                    |
| `private_module`         | a `privateModules` path on the public route or a checksum lookup of one (410)            |
| `checksum_not_public`    | a checksum lookup the registry's `publicChecksumLookups` does not allow (410)            |

**Credential lifecycle lines** carry the registry and its kind, never a value: `credential_loaded`,
`credential_reloaded`, `credential_invalid` (with the reason), `credential_minted` (with `expires_at`),
`credential_mint_failed` (with the AWS error code and the endpoint's address class) and `credential_rejected` (the
`healthPackage` self-check). At startup each registry logs `registry configured` with its ecosystem, host and credential
kind.

**Expect `not_allowed` lines during normal builds.** With the proxy first, every baked module outside the allowlist is
asked of the proxy, refused, and then served from the image's cache. Those lines are the fall-through working, not an
attack.

Metrics: `patchy.egress_proxy.requests` (by `profile`, `route`, `outcome`, plus `ecosystem` and `registry` on registry
routes), `patchy.egress_proxy.bytes` (by `profile`, `route`), `patchy.egress_proxy.inflight` (by `profile`),
`patchy.egress_proxy.credential.refreshes` (by `registry`, `kind`, `result`), `patchy.egress_proxy.credential.expiry`
(seconds until a CodeArtifact token expires, by `registry`) and `patchy.egress_proxy.index.links` (by `registry`,
`result` `rewritten` or `dropped`). They are exported only with telemetry configured. The proxy's NetworkPolicy allows
only public 443 out, so an OTLP endpoint in the cluster also needs an `egressProxy.networkPolicy.extraEgress` rule; a
hosted collector on public 443 needs none.

## Debugging without a log sink

Everything below works on a cluster that ships no container logs anywhere, with nothing added to the chart. What is lost
without a sink is long history, not the recent past.

**Why did this fetch fail?** Look at the run first. Go `410` and npm `403` bodies name the rule, and npm and the go
command print them, so they are in the run's transcript, which the status page's run panel and `patchy describe` show.
pip never prints a body, so for pip the proxy's audit line is the answer.

**Which requests did the proxy refuse or forward?** Every request is one audit line:

```sh
kubectl -n patchy logs -l app.kubernetes.io/name=egress-proxy --prefix --tail=-1 --since=2h --max-log-requests=10 | grep -E 'outcome=(not_allowed|not_granted|prerelease|registry_refused|upstream_auth|credential_|redirect_refused|denied_host|private_module|checksum_not_public)'
```

`--tail=-1` matters: with a label selector kubectl shows only the last **10** lines per pod by default. `--prefix` names
the pod each line came from. The audit lines are written at `info`, which the chart keeps for the proxy whatever the
shared `config.logLevel` (see [Flags](#flags)).

**Is the credential working, and when does it expire?**

```sh
kubectl -n patchy logs -l app.kubernetes.io/name=egress-proxy --prefix --tail=-1 --max-log-requests=10 | grep -E 'msg=(credential_|"registry configured")'
kubectl -n patchy exec deploy/patchy-egress-proxy -- /usr/local/bin/controller probe --registry ca-pypi --package six
```

`probe` runs inside a proxy pod (the image is distroless: there is no shell, and the binary is
`/usr/local/bin/controller`). It builds the engine from the pod's own configuration, loads or mints the registry's
credential itself, sends one real request for the package (the registry's `healthPackage` without `--package`) through
the first profile granting it, and prints one line such as
`registry=ca-pypi profile=web path=/r/ca-pypi/simple/six/ status=200 upstream_status=200 outcome=ok reason= credential=codeartifact`.
It never prints a credential or a response body, and exits non-zero unless the answer was `200`: the line is then
followed on stderr by a log line with `level=ERROR msg=fatal error="probe failed"`, and the first line's `outcome` and
`reason` say why. Before sending, it waits up to `--probe-credential-wait` (default `30s`) for the registry's credential
to load or mint; if none arrives in that time it sends the request anyway, which then shows
`outcome=credential_unavailable`.

**How far back can I see?** Only each container's **current** log file, which kubectl reads; rotated files need node
access. On EKS that file rotates at 10 MiB, and an audit line is roughly 500-700 bytes, so a pod keeps about 15-20
thousand recent requests; an `npm ci` makes about two requests per dependency, so a busy day can rotate past the
morning. `kubectl logs --previous` adds the prior container of the **same** pod after a crash, not after the pod is
replaced (a config roll, a `suspendProfiles` flip, a node drain, an upgrade), and a replaced pod's logs are gone.

To keep a whole investigation, stream for its duration, starting before the run and restarting after any roll:

```sh
kubectl -n patchy logs -f -l app.kubernetes.io/name=egress-proxy --prefix --tail=-1 --max-log-requests=10 > proxy-$(date +%s).log &
```

If history ever matters, run a log shipper outside the patchy chart (a Fluent Bit DaemonSet, or the EKS observability
add-on); it is cluster-level and changes nothing in this chart.

## Troubleshooting

- **A build fails with `no such file or directory` under `file:///…`.** The proxy refused the module or version and the
  baked cache did not have it. Find the `refused` audit line for the pod's source IP: `reason` says `prefix` (add the
  module path to the profile's `modules`, narrowly) or `version` (name a tagged release, or set `allowPseudoVersions` on
  the profile if the repository genuinely needs one).
- **`410 … suspended`.** `egressProxy.suspendProfiles` is on.
- **`502 … upstream answered <code>` or `denied_address`.** proxy.golang.org or sum.golang.org failed, or their names
  resolved to an address the proxy refuses to dial. Check the cluster's DNS and outbound HTTPS from the release
  namespace.
- **`503`.** A limit was reached and the wait ran out; raise `egressProxy.limits` if this is steady load. On a registry
  route, `503 credential_unavailable` means the credential is not there yet: a missing or invalid Secret key
  (`credential_invalid`), or a CodeArtifact token not yet minted or expired (`credential_mint_failed`).
- **npm `403 … registry_refused`, pip `HTTP error 403`, go `410 … registry_refused`.** The registry's own curation
  refused the package. If every request is refused, the credential may be revoked: run `probe`, and set a
  `healthPackage`.
- **`502 … upstream_auth`.** The registry refused the credential: rotate it, then check `credential_reloaded`.
- **pip finds "no matching distribution" for a package the index has.** Its files are on a host missing from
  `fileHosts`, or their links broke the file grammar: look for `dropped_links` above 0 on the page's audit line.
- **`410 … checksum_not_public` from the go command.** See [Go registries](#go-registries).
- **The proxy pods stay in `ContainerCreating`.** A registry's Secret (or a `ca` ConfigMap) is missing in the release
  namespace: `kubectl -n patchy describe pod -l app.kubernetes.io/name=egress-proxy` names it.
- **A tunnel install fails.** Run the probe under [Debugging a refused host](#debugging-a-refused-host). `not_allowed`:
  add the host to the profile's `hosts` (or point the package manager at a listed one); `hard_denied`: the host is on
  the deny list and cannot be listed; `503 … (source_upload_budget)`: the source spent its upload budget (raise
  `upload.perSource` on the profile if the install is genuinely large); `idle_timeout` on its own is a client
  reconnecting, not a failure.
- **A Job has no profile.** Check the remediation's or intent run's `runnerImage.networkProfile` and the controller's
  log line for the launch: no accepted image, no matching repository, two matching profiles, or the kill switch. A run
  that settles as `launch_refused` names a grant host that is, or is below, a Forge's web host (its `baseURL` host, and
  for an `api.` host the host above it, as on GitHub Enterprise Cloud with data residency).
