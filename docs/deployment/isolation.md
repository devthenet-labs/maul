# Isolation model

Patchy hands untrusted inputs — repository contents and security-alert text — to a coding agent. The design calls for an
agent with "no internet access and no GitHub credentials", and for the default (claude) runner both halves now hold: its
model traffic goes through the in-cluster [egress credential broker](../configuration/egress-broker.md), so the pod
dials no external host at all. What is delivered is layered — credential absence, RBAC, pod security, and network egress
— and the first layer is the one that matters.

## Credential absence — the real control

The agent pod holds **no credential of any kind, in any container**. There is no init-container clone token, because
there is no clone — the repository arrives as a tarball from the source-controller's in-cluster artifact server: the URL
carries an unguessable 128-bit id, the Job pins the sha256 digest, and the init container verifies it before extracting
and synthesizing the local git base. And for the claude runner there is no model key either: the broker injects or signs
the model credential outbound, and the pod authenticates to it with an audience-bound projected ServiceAccount token —
**an identity document, not a capability**: it names the pod to the broker and is honoured nowhere else, so exfiltrating
it buys model access through an audited chokepoint and nothing more. The one auth-shaped value the claude pod does carry
is a fixed, non-secret placeholder `ANTHROPIC_AUTH_TOKEN=patchy-brokered-placeholder`, set only by patchy because the
CLI refuses to start without some token; the broker strips it with every other inbound credential header, so it is worth
nothing. The per-Job Secret carries only handoff markdown, and `internal/jobs` lists `GITHUB_TOKEN` (and every other
credential channel) as a reserved env name so no configuration can smuggle a credential in.

| Credential                                          | Where it lives                                                                      | Who sees it                                                                                                                |
| --------------------------------------------------- | ----------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------- |
| GitHub App key / token, webhook secret              | `patchy-github` Secret, release ns                                                  | Read on demand through the API by the controllers the `Integration`/`Forge` CRs point at — never mounted into a Deployment |
| Read token (single repo, archive download)          | Minted on demand, never stored                                                      | source-controller only                                                                                                     |
| Write token (single repo, push + PR)                | Minted on demand, never stored                                                      | remediation-controller only                                                                                                |
| Claude model credential (API key or cloud identity) | `patchy-anthropic` Secret, **release ns** — or the broker's cloud workload identity | The egress broker only — **never an agent pod**                                                                            |
| Codex / copilot model credential                    | `patchy-openai` / `patchy-copilot`, agent ns                                        | Those harnesses' agent containers — the only secret value in a non-brokered pod (both runners ship disabled)               |

All GitHub side effects — issue projection, alert dismissal, branch push, pull requests — happen controller-side with
short-lived, per-repository scoped tokens. An agent that reaches `github.com` reaches it as an anonymous member of the
public.

## RBAC

Every controller identity gets a verb-by-verb `Role` in the release namespace (its own CRs and status subresources,
`get` on Secrets, its leader-election Lease); the two job controllers additionally share a Role in `patchy-agents`
covering exactly what `internal/jobs` uses — create/get/list/watch/delete on `batch/jobs`, get/list/watch pods,
`pods/log` (the `PATCHY-EVENT:` stream), and create/get/delete on the per-Job handoff `secrets`.

The agent ServiceAccount (`patchy-agent`) has **no Role whatsoever** and the pods run with
`automountServiceAccountToken: false` — Kubernetes API access from the agent pod would let a prompt-injected agent read
the very Secrets the isolation model depends on. A brokered (claude) pod does mount a **projected** ServiceAccount
token, but one bound to the broker's audience (`patchy-egress-broker`): the API server rejects it as a Kubernetes API
credential, and the broker — which validates it via `TokenReview` — is the only party that accepts it. The broker's own
identity is equally narrow: `create tokenreviews` is its whole RBAC surface, with no Secret API access of any kind (its
credential files arrive as mounted volumes).

## Pod security

Both namespaces enforce the `restricted` Pod Security Standard (the chart labels the agent namespace it creates;
[label the release namespace yourself](../getting-started/install.md#create-the-namespaces)). Every pod — controllers
and agent Jobs — runs as non-root uid 65532 with a read-only root filesystem, all capabilities dropped, and the
`RuntimeDefault` seccomp profile; writable paths are emptyDir mounts (`/tmp`, `/workspace`). Agent Jobs run with
`backoffLimit: 0` and `restartPolicy: Never` — retries belong to the state machine, not to Kubernetes — under an
`activeDeadlineSeconds` kill switch (`--job-deadline`).

## Agent tool postures

Inside the pod, the claude harness renders each stage's sandbox posture into the CLI's tool flags, beside a settings pin
(`--setting-sources user`, `--strict-mcp-config`) that keeps the working tree's own `.claude/` settings and `.mcp.json`
from changing what a stage may run.

- **Read-only** (`investigate`, and an intent's `plan`): no shell. `--tools Read,Glob,Grep,Edit,Write` is the whole
  built-in tool set the run has, and `Bash` is denied by name as well, so neither arbitrary commands nor the CLI's
  built-in read-only shell commands are available, and no subagent, network or notebook tool either. Writes are allowed
  only under the stage's `reports/` directory (a path-scoped `Edit` rule; in `-p` mode a write the rules do not allow is
  refused), which leaves out the repository tree, its `.git` directory, and the settings under `HOME` (the workspace)
  that the CLI reads. The CLI still lets the agent write the few files it manages for itself (its memory and scratch
  files) whatever the rules say, so a read-only run also turns the CLI's auto memory off
  (`CLAUDE_CODE_DISABLE_AUTO_MEMORY=1`). A [report repair](../configuration/agent-runner.md#report-repair), which
  resumes the same session with the same flags, therefore loads no settings or memory the agent wrote.
- **Workspace-write** (`remediate`, and an intent's `build`): the CLI's built-in tools with a shell, so the agent can
  build and test; network tools stay denied.

codex and copilot do not express these postures (see [extending](../extending.md)); on them the pod is the boundary, and
the intent stages run on brokered claude only. An investigation on either still has a shell and unscoped writes, so its
prompt asks the agent to stay read-only without telling it that the sandbox enforces it.

## Repository-declared images

With repository runner images on, the agent container can run an image the target repository names
([the repository-owner guide](../integrations/agent-images.md)), so the image is untrusted code with the agent's powers:
it can read the workspace and the broker caller token, call the broker directly, and write forged `PATCHY-EVENT:` lines.
The posture is built so that none of that reaches further than a default-image run:

- **Same pod, same walls.** The pod security above is forced by patchy whatever the image says: uid 65532, read-only
  root, no capabilities, the image's `USER` and `ENTRYPOINT` ignored, an image-declared `VOLUME` rejected at resolution
  so only patchy's emptyDirs are writable, and both containers bounded by an ephemeral-storage limit (CPU and memory by
  `agent.resources` only, which sets none by default). The image is pinned to a digest once per Repository and only
  allowlisted, and by default signed, images are admitted
  ([source-controller](../configuration/source-controller.md#repository-runner-images)).
- **No Kubernetes identity.** The `patchy-agent` ServiceAccount has no Role and the pods run with
  `automountServiceAccountToken: false`; the one projected token is bound to the broker's audience, is pod-bound, and is
  honoured by the broker alone, whose per-pod limits and route surface are the enforced bound on what it buys.
- **Trusted code first.** The `prepare` init container always runs the default runner image. It fetches and verifies the
  tarball, synthesizes the git base, copies `agent-runner` and the claude CLI into a read-only `/patchy/bin`, and runs
  the **sandbox probe** before any untrusted code starts: from inside the pod it tries the public internet
  (`1.1.1.1:443`), the Kubernetes API server and the cloud metadata service (`169.254.169.254:80`). It retries for about
  20 seconds, because on some CNIs (EKS Auto Mode among them) a new pod's NetworkPolicy attaches a few seconds after the
  pod starts, and it concludes enforcement only after three consecutive rounds in which nothing answered. If egress is
  still open, `prepare` exits 78, the agent container never starts, the run fails `SandboxUnenforced`, and the job
  controller refuses repository images until it restarts. A cluster that does not enforce NetworkPolicy therefore fails
  loudly instead of running untrusted images with open egress.
- **Untrusted output is validated.** A changeset from a repository-image run must be based on the Repository's pinned
  commit, is capped at `--changeset-max-entries` entries, and may not touch `.github/workflows/` or `.github/actions/`,
  because a pushed branch runs CI with the repository's secrets before a human has read it; a refused changeset makes no
  forge call. An `ignore` verdict from such a run is held for a human instead of dismissing the alert.
- **What is not bounded.** DNS to the cluster resolver is open, as for every agent pod, unless it is
  [closed](#closing-dns) with `agent.networkPolicy.dns: none`; otherwise only a Cilium DNS-rules or `toFQDNs` allowlist
  bounds it. The usage and cost the claude CLI reports from a repository-image run are image-controlled; the broker's
  per-pod totals are the trustworthy record.

## Network egress — the floor and the fence

**The baseline NetworkPolicy is the floor.** `patchy-agents` is default-deny in both directions. Egress is re-permitted
for DNS, the artifact port (9790) to source-controller, the broker port (8080) to the egress broker, and TCP 443 with
the cluster's own ranges and the cloud metadata endpoint (`169.254.169.254`) excluded — adjust
`agent.networkPolicy.clusterCIDRs` (Helm) or the `except:` CIDRs in `base/networkpolicy.yaml` (kustomize) to your
cluster's pod/service/node CIDRs. A **claude** pod uses only the first three: DNS, the artifact server, and the broker —
its entire egress is cluster-local, and with [DNS closed](#closing-dns) it is the last two alone. The 443 rule exists
for the non-brokered runners (codex/copilot, both shipped disabled), and be honest about what it is: **a plain
NetworkPolicy is L3/L4 and cannot match a hostname**, so "TCP 443" means every HTTPS host on the internet, not just the
model vendor's.

Pinning egress to hostnames takes one of three optional layers, selected by `agent.networkPolicy.mode` (Helm) or by the
matching kustomize component. Brokered claude needs none of them — it has **no external hosts to allowlist** (its Cilium
policy is cluster-only with no `toFQDNs`, its GKE FQDN policy is skipped, its Istio ServiceEntry set is empty) — so the
hostname layers exist for the codex/copilot runners' model hosts. **No GitHub hosts appear anywhere in any of them** —
the pod never talks to a forge, so there is nothing to allow.

- **Cilium** (`mode: cilium`, or the kustomize `components/cilium` — what the prod overlay uses) — one
  `CiliumNetworkPolicy` per runner: `toFQDNs` naming that runner's model hosts, plus a DNS rule constraining what names
  the pod may resolve at all (the runner's `dnsPatterns` and `*.svc.cluster.local`, so the artifact fetch still
  resolves). The claude policy is cluster-only — DNS, the artifact server, and the broker, with no `toFQDNs` and no
  external names resolvable at all. Requires Cilium with the DNS proxy.
- **GKE Dataplane V2** (`mode: gke`, or `components/gke-fqdn`) — an `FQDNNetworkPolicy` (`networking.gke.io/v1alpha1`)
  naming the same host on 443. Dataplane V2 _is_ Cilium, but it has not honoured the `CiliumNetworkPolicy` CRD since
  1.21.5-gke.1300 and rejects every L7 rule, so the Cilium layer above is inert there — this is its equivalent, with
  `anetd` snooping DNS answers in place of Cilium's proxy. Requires the cluster to be created or updated with
  `--enable-fqdn-network-policy` (Preview; kube-dns or Cloud DNS; at most 50 resolved addresses per name). It cannot
  express DNS or a ClusterIP destination, so DNS and the artifact fetch stay with the base policy — which also means it
  does **not** close the DNS exfiltration channel described below.
- **Istio** (`mode: istio`, or `components/istio`) — the same allowlist as a `Sidecar` in `REGISTRY_ONLY` mode (exposing
  only that runner's model ServiceEntry — none at all for claude — and the release namespace's Services, which covers
  the artifact server and the broker), matched by SNI. Two hard requirements: **native sidecars** (Kubernetes ≥ 1.29 and
  istiod with `ENABLE_NATIVE_SIDECARS=true` — a classic sidecar never terminates, hanging the Job, and blackholes the
  init container's artifact fetch), and the **Istio CNI node agent** (the `restricted` PSS rejects `istio-init`'s
  NET_ADMIN/NET_RAW).

The default, `mode: auto`, resolves this from the cluster's own API surface on every render against a live cluster, so
one set of manifests can serve a GKE cluster and a Cilium cluster unmodified. It picks `gke` when the
`FQDNNetworkPolicy` CRD is present, `cilium` when a real Cilium is (never on GKE, where a `CiliumNetworkPolicy` would
enforce nothing while reading as protection), and nothing otherwise. It never picks `istio`: CRD presence says nothing
about native sidecars. An off-cluster `helm template` sees no cluster and resolves to `none`.

!!! danger "Network policies are additive"

    A packet matching *any* policy that selects the pod is allowed, and no policy can subtract from another. So an FQDN
    allowlist rendered next to a NetworkPolicy that already permits 443 to `0.0.0.0/0` constrains **nothing** — the
    union is still "443 to anywhere". Whenever an FQDN layer is active the chart therefore drops that broad rule
    (`agent.networkPolicy.broadEgress: auto`), and the kustomize components patch it out of the base policy. If you
    assemble these by hand, do the same, or the fence is decorative. `broadEgress: always` keeps it deliberately while
    soaking a newly enabled layer — a layer that turns out not to enforce then fails open rather than blackholing every
    Job.

Enabling both Cilium and Istio fails the chart render. If you have none of the three, the base policy still applies and
is then the whole of the L3/L4 story.

### Why Cilium is preferred

All three render the same allowlist, but they are not equivalent, and the prod overlay picks Cilium deliberately (on GKE
the choice is made for you — `gke` is the only one Dataplane V2 enforces):

- **DNS exfiltration.** Istio enforces at the TCP/TLS layer, but the pod still needs UDP/53 to the cluster resolver, and
  the proxy does not constrain what names it may resolve. A prompt-injected agent can encode data into query names
  (`<chunk>.attacker.example`) and walk it out through the resolver with every other route blocked. Cilium's transparent
  DNS proxy answers only the allowlisted patterns and drops everything else, closing that channel; the learned IPs then
  bound the L3/L4 rules, so skipping DNS and dialling a raw address is blocked too. On any CNI, brokered claude can
  instead [run with no resolver at all](#closing-dns).
- **Enforcement point.** The sidecar and its traffic redirection live inside the pod's own network namespace, where a
  sufficiently privileged process could bypass them. Cilium enforces in eBPF on the node, outside anything the workload
  can touch.
- **Job friction.** The native-sidecar and CNI-node-agent requirements exist purely to make the mesh coexist with a
  short-lived Job; Cilium adds nothing to the pod.

Either way, do not mistake the FQDN policy for the boundary. The missing credential is the boundary; the egress layers
narrow what a compromised agent can talk to, not what it can do to your forge.

!!! warning "kind is not a sandbox"

    kind's default CNI (kindnet) ignores NetworkPolicy entirely. The dev overlay applying cleanly does not mean the
    egress fence works — verify isolation on a CNI that enforces it (k3s on [Colima](colima.md) enforces the L3/L4
    floor; the FQDN layer needs a real Cilium or Istio cluster).

### Closing DNS

Every egress layer above still lets the pod reach a resolver, and the cluster resolver forwards any name it does not own
upstream. A command in the pod can therefore encode data into query names (`<chunk>.attacker.example`) and walk it out
through DNS with every other route blocked. Only Cilium's DNS rules bound that. `agent.networkPolicy.dns: none` (Helm),
or the kustomize component `components/agent-dns-none` (listed after `components/cilium` when both are used), closes it
on any CNI:

- **The pod has no resolver.** Its `dnsPolicy` is `None` and its only nameserver is `127.0.0.1`, its own loopback, where
  nothing answers. A lookup the hosts file cannot answer is refused at once (about 5 ms for curl in the runner image);
  the `timeout:1` and `attempts:1` options bound the libcs that do not see the refusal (musl) to a second.
- **Nothing in the pod can resolve an external name.** That includes the agent's own commands: `npm install`,
  `go mod download`, `pip install`, `apt-get` or a `git clone` of another repository fails on its first lookup, whatever
  the egress policies would admit. Everything a run needs (toolchains, dependencies, module and package caches) must
  already be in the runner image, or in the repository's working tree; a repository whose build fetches at run time
  cannot be built or tested under `dns: none`.
- **The names it needs are pinned.** An agent pod dials exactly two in-cluster names: the artifact server (the prepare
  init's fetch) and the egress broker (the claude CLI's base URL). The job controller that creates the Job resolves the
  hosts in the Job's own URLs with its own resolver, which still reaches cluster DNS, and writes them into the pod's
  hosts file through `hostAliases`, one entry per address. A host that does not resolve fails the launch, which is
  retried; no pod is created that could not reach its endpoints. The names are Services' ClusterIPs, stable for the
  Service's lifetime; a Job keeps the addresses it was created with.
- **The policies drop DNS.** The agent egress NetworkPolicy, and under mode `cilium` the claude `CiliumNetworkPolicy`,
  render no DNS rule, so a command that ignores the resolver configuration and queries the cluster DNS address directly
  is dropped too. The controllers keep theirs: they resolve for the pods.
- **DNS, not addresses.** Closing DNS leaves the policies' other rules as they are, so under mode `none` or `istio` the
  base policy's "TCP 443 to anywhere" rule still admits a raw address. Only brokered claude runs without a resolver, and
  it needs no such rule: set `agent.networkPolicy.broadEgress: never` beside it (the install NOTES say so while the rule
  is kept) and an agent pod reaches the artifact server and the broker alone.
- **Brokered claude only.** codex and copilot dial their vendor's API by name, and an internet name's addresses cannot
  be pinned at launch the way a ClusterIP's can, so enabling either (on any fleet) fails the chart render and the
  controllers refuse to start. The chart refuses mode `istio` too (do not combine the kustomize component with
  `components/istio`): the pod's sidecar must resolve istiod. The fake harness works.

The images resolve through the hosts file. The claude runner image and `agent-base` are built on wolfi-base, whose
`/etc/nsswitch.conf` reads `hosts: files dns`. musl reads the hosts file first whatever that file says, Go's resolver
follows it (hosts file first when it is missing), and glibc with no `hosts:` line falls back to the hosts file once the
dead nameserver refuses. With only the dead nameserver configured, curl and the claude CLI in the runner image were
checked to reach the broker through a hosts-file entry, with and without that `hosts:` line, so a repository-declared
image needs nothing extra.

!!! warning "A DNS policy managed outside patchy keeps the channel open"

    Network policies are additive. A namespace-wide DNS allow policy for `patchy-agents` that you manage yourself (an
    `allow-dns` policy for EKS Auto Mode's node-local resolver, say) still lets an agent pod query the cluster resolver
    directly, whatever the chart renders. Remove it, or narrow its `podSelector` so it selects no agent pod, when you
    turn DNS off. `clusterDNSCIDR` needs nothing: it only adds an address to the chart's own DNS rules, which are dropped.

### What the agent policy does not stop

Two residuals apply to every agent pod, whatever the image and whatever the posture.

- **The policy-attach window at pod start.** On some CNIs (EKS Auto Mode's VPC CNI among them) a new pod's NetworkPolicy
  attaches a few seconds after the pod starts, and until it does the pod's egress is open. The `prepare` init container
  runs first, in the trusted runner image, and both containers share the pod's network, so patchy closes the window by
  holding the agent container until the policy has visibly attached:
  - A [repository-image](#repository-declared-images) Job runs the **strict** sandbox probe, unchanged: three targets,
    and if any is still open when the window closes the run fails `SandboxUnenforced` and the untrusted image never
    starts.
  - Every **default-image** Job (finding investigate and remediate, intent plan and build) ends its `prepare` script
    with the probe in **wait** mode: `agent-runner sandbox-probe --mode=wait --targets=k8s,imds`. It dials the
    Kubernetes API's ClusterIP (from `KUBERNETES_SERVICE_HOST`) and the metadata service (`169.254.169.254:80`), which
    every chart posture blocks. (Under `broadEgress` the Kubernetes API is blocked only because
    `agent.networkPolicy.clusterCIDRs` excludes its address from the open TCP 443 rule: if your service ClusterIPs or
    API endpoints sit outside the default RFC 1918 ranges, a `100.64.0.0/10` secondary CIDR for example, add those
    ranges to `clusterCIDRs`.) It does not dial `1.1.1.1`, because `broadEgress` keeps TCP 443 to the internet open on
    purpose. It returns after three consecutive rounds in which neither answered. That adds about 2 seconds to a Job's
    start against a policy that rejects and about 11 seconds against one that drops (the VPC CNI drops), plus however
    long the policy took to attach. If the 20-second window closes with a target still open, it prints one line to the
    `prepare` log (`kubectl logs <pod> -c prepare`) starting `SANDBOX NOT CONFIRMED`, naming the open targets, and the
    agent starts anyway. The wait probe never fails the Job, never spends an attempt and never trips the sandbox
    breaker, because a default-image run never depended on it. On a cluster that does not enforce NetworkPolicy (kind,
    see above) every default-image Job therefore starts about 20 to 23 seconds later and logs that line, which is the
    signal to fix the CNI. On a CNI that does enforce, the same line naming the Kubernetes API means `clusterCIDRs` does
    not cover the API's address. The line names only the targets, the round count and the elapsed time.
  - **Evaluation Jobs run neither probe.** Their evolve-runner images come from the evolve repository and ship no
    `agent-runner` to run it with, so an evaluation pod may run its first seconds inside the window.
- **The node's own kubelet.** On EKS Auto Mode (VPC CNI) a pod can reach the kubelet of the node it runs on (TCP 10250)
  whatever NetworkPolicy says. The kubelet answers `401 Unauthorized` to an anonymous request, and its read-only port
  (10255) is closed. An agent pod holds no credential the kubelet accepts: `automountServiceAccountToken` is off, the
  only projected token is bound to the egress broker's audience, and the `patchy-agent` ServiceAccount has no Role, so
  even a token the kubelet did accept would authorise nothing. What remains is that an agent can see that a kubelet
  answers on its node. Closing that is a node-level control (a security group or host firewall), outside the chart.

## Network profiles: the Go dependency route

An agent pod has no route to a package registry: every dependency must be in the runner image. An operator can grant one
exception, off by default: a **network profile** (`egressProxy.enabled` and `agent.networkProfiles` in the Helm chart;
there is no kustomize component). The profile is a **delta** on the egress described above, never a new baseline. Only a
pod that is all of these gains anything:

- a Finding's remediate pod, or an Intent's build, revise or check-fix pod;
- running the repository's **accepted** repository-declared image;
- of a repository matching exactly one profile (RE2 patterns matched anchored against the lowercased `owner/name`);
- while `egressProxy.suspendProfiles` is off and the sandbox breaker is not tripped.

It gains exactly one egress destination, the [egress proxy](../configuration/egress-proxy.md) on that profile's port,
and nothing else: no credential, no resolver (under `dns: none` the controller pins the proxy's Service name in the
hosts file, as it does the broker's) and no proxy variable unless the profile lists tunnel hosts
([below](#tunnel-hosts)), when `HTTPS_PROXY` points at that same port. Every other pod, investigate, plan and evaluation
pods and every default-image pod included, gets nothing from a profile: no label, no proxy entry and no proxy
environment. (Two things they do carry are not part of a profile: every default-image pod ends its `prepare` with the
wait probe [above](#what-the-agent-policy-does-not-stop), and an intent plan pod is told, as plain text for its prompt
in `PATCHY_BUILD_NETWORK_PROFILES`, which profile its builds would get.) The proxy forwards only `GET` and `HEAD`
requests for Go modules and checksum-database records, only for the profile's allowlisted module prefixes, and only
tagged releases unless the profile sets `allowPseudoVersions`; it caches nothing. A profile may also be granted
operator-defined registries ([below](#registry-grants)); then the proxy holds those registries' credentials, but the pod
still holds none.

What the egress is without profiles depends on your values; do not read one deployment's as the chart's. Under
`dns: none` with brokered claude only, an agent pod reaches the artifact server and the egress broker and has no
resolver. With `dns: cluster` it also has a DNS rule; under mode `cilium` a non-brokered runner gets `toFQDNs` to its
model API; and with repository images off under mode `none`, the base policy's "TCP 443 to anywhere" rule stays.
Profiles require repository images, which require narrow egress, so that last case never carries a profile.

### How it is enforced

**The port is the identity, checked on both sides.** Each profile has its own proxy listener and its own two rules:

- an agent-side NetworkPolicy, `<release>-agents-network-<profile>` in the agent namespace, selecting only agent pods
  labelled `patchy.bitwisemedia.uk/network-<profile>: "true"` and allowing them egress to the proxy's pods on that
  profile's port, and nothing else (no DNS rule, no `ipBlock`);
- the proxy's own NetworkPolicy, whose ingress admits that port only from agent pods carrying that label in the agent
  namespace (plus the probe port 8081 from anywhere). Its egress is DNS and TCP 443 to `0.0.0.0/0` with the cluster's
  IPv4 ranges, `10/8`, `172.16/12`, `192.168/16`, `169.254/16` (IMDS and EKS Pod Identity) and `100.64/10` excepted, and
  nothing IPv6. The proxy's own dialer refuses the same ranges again at connect time.

Plain `networking.k8s.io/v1` policies only: modes `none`, `gke` and `cilium` all enforce them (Cilium unions them with
the per-harness `CiliumNetworkPolicy`). Mode `istio` is refused by the chart in step 1: the `REGISTRY_ONLY` Sidecar and
port-based identity have not been verified together.

**The label is set in one place and checked in four.** Only the remediation-controller (after it pins the image) and the
intent-controller (for build and revise stages) ask for a profile. `internal/jobs` applies it only to the exact pairs
`(remediate, remediation)` and `(build, intent)`, only when it injects the repository image, and refuses it on any other
Job. With `admissionPolicy.enabled`, a ValidatingAdmissionPolicy refuses a Job in the agent namespace whose labels or
pod-template labels carry a `network-` profile key unless the remediation-controller creates it with kind `remediation`,
or the intent-controller with kind `intent`, and it carries the `runner-image-source: repository` annotation. That
closes "an investigation never gets a profile" against a bug in one controller: the investigation-controller's
ServiceAccount holds `jobs create` there too.

The NetworkPolicies select Pods, not Jobs, so a second policy covers Pods: a Pod in the agent namespace whose labels
carry a `network-` profile key is admitted only when the Kubernetes Job controller creates it
(`admissionPolicy.jobControllerUsernames`, default `system:serviceaccount:kube-system:job-controller`) with a single
controlling owner reference to a `batch/v1` Job, and no update may add or change such a label, through the `pods/status`
subresource as well as the Pod itself (a status write keeps the labels it is sent), while removing one is always
allowed. A bare Pod, or one from a ReplicaSet, StatefulSet or any other controller, carrying the label is refused, so
every labelled Pod was stamped from a Job template the Job policy already judged and that policy stays the one gate. The
default identity is kube-controller-manager's per-controller ServiceAccount under `--use-service-account-credentials`,
which kubeadm, kind, k3s, EKS (Auto Mode included), GKE and AKS all run with; on a control plane that runs every
controller as `system:kube-controller-manager`, set that name instead, and the owner reference check still keeps the
other controllers' Pods out. A wrong name fails closed: profile Jobs are created but their Pods are refused, which the
Job's events show.

Without admission policies (clusters older than 1.30) the label is enforced by controller code alone, and the install
NOTES say so.

**Two kill-switch levers.** `egressProxy.suspendProfiles: true` stops the job controllers applying a profile from their
next Job **and** rolls the proxy into answering every profile request `410 suspended`, so a Job already running can
fetch nothing new from the moment the new proxy pods are Ready (between the switch and then, the old pods still serve).
The policies stay rendered. `egressProxy.enabled: false` removes the proxy and every policy.

### What a profile does not stop

Be plain about the residue; it is why a narrower step 2 (an exact `module@version` allowlist per run) exists.

- **Data in allowed module paths still reaches proxy.golang.org, and from there the origin.** Under a broad prefix
  (`github.com/`), proxy.golang.org fetches from attacker-owned repositories; index.golang.org then lists each newly
  fetched `module@version` publicly, and a checksum lookup appends it permanently to the public log, so code in the pod
  with pre-made tags can signal about a byte per first fetch. Vanity hosts under an allowed prefix
  (`go.opentelemetry.io/`, `k8s.io/`) see `?go-get=1` requests for arbitrary subpaths. **Narrow prefixes**
  (`github.com/google/uuid`, not `github.com/`) close this for everything outside them.
- **Which version is fetched, and the `list` and `@latest` requests themselves, stay visible to Google,** and a
  never-before-fetched tag shows in the index. The version gate closes commit choice, fork-network commits served
  through the parent repository and the version string as a carrier; `allowPseudoVersions: true` reopens those three for
  that profile's modules.
- **Typosquats** under a broad prefix are fetchable. Review `go.mod` in the pull request; its body says the run had a
  profile.
- **Environment is not a boundary.** The job controller sets every go variable non-empty (`GOSUMDB=sum.golang.org`, the
  pattern variables `patchy.invalid`, `GOENV=off`) so neither the image's `ENV` nor its `$GOROOT/go.env` can turn
  checksum verification off by default, but the agent can still run `GOSUMDB=off go get …`. Integrity is restored
  downstream: CI's go command verifies `go.sum` against what proxy.golang.org serves, and a human reviews the diff.
- **Malicious dependency code** runs in the pod when tests run. It gains no reachability beyond the proxy, which is
  bounded by the same allowlist; the final boundary for the dependency is pull-request review.
- **Node-local traffic.** On Cilium, traffic from the local host to local pods is allowed by default, and a CIDR rule
  does not match Cilium's `host` entity, so a hostNetwork workload on the proxy's node may reach the profile ports
  regardless of the ingress rule.
- **The policy-attach window.** On EKS Auto Mode a new pod's NetworkPolicy attaches seconds after it starts, so for a
  few seconds after each proxy pod starts its ingress rule (the identity check) may be missing. The proxy's readiness is
  held for 15 seconds so the Service sends it nothing in that window, but its pod IP is reachable by any pod whose own
  egress allows it. In both cases the impact is bounded: no credential, allowlisted modules and tagged versions only.
- **A compromised proxy** has TCP 443 to the internet, so it could exfiltrate what profile pods send it. That is why it
  is small, separate from the broker, distroless, read-only and non-root, and holds no credential unless registries are
  configured ([below](#registry-grants)). With attribution on it also holds a token that can list pods in the agent
  namespace, and reaches the API server's endpoint IPs (`/32`s only).
- **On a CNI that does not enforce NetworkPolicy** the port is open to every pod, but every repository-image Job's
  sandbox probe already fails such clusters before untrusted code runs.
- **DoS and cost.** One profile pod can use its share of the proxy (global and per-source concurrency, rate, size caps
  and timeouts bound it), and each profile run copies its modules into `/tmp`, so size ephemeral storage with headroom.

### Registry grants

A profile can be granted operator-defined upstream registries (`egressProxy.registries`: Artifactory, Nexus,
CodeArtifact, for Go, npm and pip; see [Registries](../configuration/egress-proxy.md#registries)). What changes, and
what does not:

- **The agent pod: nothing new.** It still reaches only the proxy on its profile's port, gains no resolver and **no
  credential**: the registry credential never enters the agent namespace, the pod's environment, a Job Secret or any
  response. The package manager is pointed at the proxy by environment variables the job controller owns, install
  scripts (npm) and source builds (pip) off unless the grant allows them.
- **The proxy now holds credentials** when credentialed registries are configured: read-only registry tokens from
  Secrets mounted into it alone, or a CodeArtifact token of at most 12 hours that it mints with its own AWS role (no
  long-lived secret at all). It sends a credential only to that registry's exact origin, never on a redirect, a file
  host or a checksum lookup; it drops any `Authorization` the agent sends; every non-200 answer on a registry route
  carries a body the proxy wrote, so no upstream error page reaches the agent; and every non-archive response is scanned
  for the credential.
- **The proxy's egress** may now also reach operator-named private addresses (`internal`: listed CIDRs on one port,
  never IMDS, link-local, loopback, cluster DNS, the API server or the Kubernetes Service address, and only `/32`s
  inside the cluster ranges) and, with `cloudCredentials: podIdentity`, the node-local Pod Identity agent, for the
  credential call alone.

The residue, in plain words:

- **A compromised proxy holds every configured registry's credential** and can reach the public internet on 443, so it
  could exfiltrate them. This is now the proxy's most serious residue. Give it read-only tokens scoped to the one
  repository each registry names, and prefer CodeArtifact's short-lived tokens; a deployment per credentialed registry
  is a possible follow-up if one shared process is too much.
- **Confused deputy.** The proxy's login could read other repositories on the same registry host if a request could
  leave the configured base path. Every forwarded path is the base plus a remainder that passed a strict grammar (npm:
  only the scope's `%2f`; pip: only `%2B` and `%21`, no `.`, `..` or empty segment; no query anywhere), but a virtual
  repository aggregates whatever you configured it to: scope the token to that repository.
- **The name and version are still a channel, narrowed, not closed.** An allowlist fixes the package **name**; npm
  versions must be plain `x.y.z` unless `allowPrerelease`, and a pip file must be a real distribution filename with a
  PEP 440 version. Within an allowlisted name the agent still chooses an npm version (three fields of up to ten digits,
  about 100 bits per request), a pip file path's directory segments and version (up to 1024 bytes), or a Go version
  under the public route's rules. These reach the registry's access logs and, on a miss through a remote or virtual
  repository, its public upstream's logs: places the agent cannot read back, but a registry's staff can. Under
  `packagePolicy: registry` the **name** is free too; that is the stated cost of handing the policy to the registry.
- **Install-time code.** With `scripts` or `sdists` on, dependency code runs at install, as tests already do; it gains
  no reachability beyond the proxy.
- **Dependency confusion.** Under `packagePolicy: registry`, a virtual repository that merges private and public names
  can resolve to a public lookalike. That is the registry's configuration: use its exclude patterns or package-origin
  controls, and review the lockfile diff.
- **Lockfile poisoning.** npm (`replace-registry-host=always`), pip and go write canonical URLs or none, and a changeset
  that names the proxy's in-cluster host in any file is refused before any forge call. uv, poetry and yarn are not
  supported.
- **Archives are not scanned** for the credential (`.tgz`, `.whl`, zip): they are verified by hash, and an
  operator-trusted registry is not assumed to plant a token in them.
- **Rotation gap.** A rotated Secret reaches the proxy within kubelet's sync delay (about one to two minutes); revoke
  the old token only after every proxy pod logs `credential_reloaded`.
- **Hostname deny lists are not IP-level controls.** The registry host, file hosts and redirect patterns are checked
  against the forge and model API hosts (and, at Job launch, every Forge resource's host), but a registry on a CDN
  shared with a denied service cannot be told apart by address; it need not be, since only the configured hostname is
  ever dialled.

### Tunnel hosts

A profile can list **hosts**: exact public host names its pods reach through an HTTPS `CONNECT` tunnel in the proxy, on
443 only (see [Tunnel](../configuration/egress-proxy.md#tunnel)). It is off unless a profile lists hosts. What changes,
and what does not:

- **The agent pod gains no new destination.** The tunnel is served on the profile's existing port, behind the same two
  NetworkPolicies and the same label; a tunnel-only profile gets its first port exactly as a Go profile does. The pod
  still has no resolver and no credential. What it gains is the proxy environment (`HTTPS_PROXY` and `NO_PROXY`, both
  cases, plus the operator's client presets), written by the job controller over whatever the image set; `HTTP_PROXY`
  and `ALL_PROXY` stay blank, and `NO_PROXY` names every in-cluster endpoint, so model traffic still goes straight to
  the broker.
- **What the proxy does on that port.** It opens a TLS tunnel only to a host the profile lists exactly, after checking
  that the ClientHello's server name equals that host and carries no Encrypted Client Hello, and copies bytes. It never
  decrypts a tunnel and holds no credential for one. It never tunnels to a forge, a model API, a managed Kubernetes
  control plane, an in-cluster or special-use name, or a package-publish host; the hosts of every `Forge` resource are
  checked at Job launch.
- **The Kubernetes API.** The proxy never tunnels to it by a managed control-plane name (EKS, AKS, GKE, DigitalOcean,
  Linode), by cluster address, or by the public address the operator configured
  (`egressProxy.attribution.apiServerCIDRs` and `egressProxy.tunnel.denyCIDRs`). An API server that is public under a
  name the deny list does not know is reached only if the operator lists that name, and only stopped by address if its
  addresses are in `tunnel.denyCIDRs`: set them on such an install (the proxy warns at startup while the list is empty).

The residue, in plain words:

- **No package allowlist.** The tunnel is name-level: any package on a listed registry is fetchable, attacker-published
  ones included, and their install scripts (npm lifecycle scripts, Python build backends) run in the pod with the tunnel
  open. Which packages were fetched is visible to the registry, and for some to package owners (PyPI's public download
  dataset, npm's download counts): an attacker who owns packages can read which of them a run fetched, a low-rate side
  channel. Review the lockfile in the pull request. A package allowlist needs a registry-protocol route, as the
  [registry grants](#registry-grants) are.
- **npm and Yarn hosts are a public publish channel.** `npm publish` is a `PUT` to `registry.npmjs.org`, the host reads
  use, and `registry.yarnpkg.com` fronts the same registry. The token travels inside TLS, so the proxy cannot refuse it,
  and patchy holding no credential does not help: a dependency's install script or a prompt-injected agent can bring its
  own npm token and publish the private working tree as a world-readable package. Only the upload caps bound it: 8 MiB
  per source IP, per profile, per proxy replica (so 16 MiB with the default two replicas), compressed, which is a lot of
  source code. **This is not closed**; an npm-protocol route that serves only reads is the fix, and until then list an
  npm host only where that is acceptable. PyPI and NuGet publish on separate hosts, which are denied.
- **Other uploads to a listed host.** Registries accept requests the proxy cannot see. Data sent to a listed host
  reaches its operator's logs, bounded by the same caps.
- **Domain fronting.** The proxy matches the server name to the `CONNECT` host, and refuses ECH so the name cannot be
  hidden, but it cannot see the HTTP `Host` inside TLS. On a shared CDN (Cloudflare for npm and Yarn, Fastly for PyPI,
  Fastly and CloudFront for crates, Azure Front Door for NuGet) a client could name another tenant inside the tunnel.
  Several CDNs refuse a mismatch across tenants, but patchy does not rely on it. **Not closed** without decrypting the
  tunnel, which patchy will not do (a CA in every image, broken pinning, plaintext in the proxy).
- **NuGet revocation is offline.** The `nuget` preset sets `NUGET_CERT_REVOCATION_MODE=offline`, because revocation
  checks go to CRL and OCSP hosts over plain http, which nothing carries: signatures are verified, but a revoked signing
  certificate is not detected.
- **Budgets are per source IP, per profile, per proxy replica, per window.** The window outlasts every Job, so a pod
  cannot pause long enough to refill its budget, and a new pod reusing an IP inherits what is left (less, never more).
- **A ClientHello parser bug** would be the tunnel's own risk: the parser is small, pure, fuzzed and cross-checked
  against Go's `crypto/tls`.
- **A compromised proxy** could tunnel anywhere on public 443, as it could already reach the internet for the Go route.

## Egress proxies

This section is about corporate forward proxies for the controllers' own forge traffic, not the
[egress proxy](#network-profiles-the-go-dependency-route) above.

The **controllers'** NetworkPolicies (separate from the agent sandbox above) allow only their declared egress: DNS,
443/6443, and each controller's specific peers. Routing forge traffic through a corporate forward proxy — the chart's
[`proxy` values](../getting-started/install.md#egress-proxy) or a `spec.proxy` on a Forge/GitHub Integration — adds a
new destination, so a proxy listening on a port like 3128 or 8080 needs a matching
`<controller>.networkPolicy.extraEgress` rule on every controller that talks to GitHub (integration, source,
remediation).

Get `NO_PROXY` right when setting the environment-wide proxy: in-cluster traffic — the artifact endpoint on
source-controller (:9790), the egress broker, the Kubernetes API server — must not detour through an external proxy. The
chart derives `localhost,127.0.0.1,.svc,.cluster.local` automatically and appends your `noProxy`; if you set the
variables another way (kustomize `config.extra`, for instance), carry those suffixes yourself. A `spec.proxy` on a
resource is deliberate and total: it routes **all** of that resource's GitHub traffic through the named proxy, and the
environment's `NO_PROXY` does not punch holes in it.

## What leaves the pod

The agent's only output channel is the `PATCHY-EVENT:` JSONL stream on stdout (parsed by the owning controller from the
pod log), which carries the remediation as a size-capped structured changeset (`PATCHY_CHANGESET_MAX_BYTES`, 5 MiB) —
the changed files' contents against the pinned base SHA, not git objects. The remediation-controller — not the agent —
verifies the commit claim, replays the changeset through the GitHub Git Data API to create the `patchy/<finding>`
branch, and opens the pull request, so every forge side effect passes through code that validates the state machine
first. The stream itself is forgeable by whatever runs in the pod, so the controller validates every changeset before
any forge call — its base must be the pinned commit and its paths git-shaped, plus the stricter
[repository-image rules](#repository-declared-images) when the run used a repository-declared image — and every pull
request still waits for a human to merge it.
