# Security model

maul hands untrusted input to a coding agent: issue text, security-alert text, and the code of the repository it works
on. Any of it can carry a prompt injection, and a repository can also choose the image its agent runs in. So maul treats
the agent, and everything the agent writes, as the least-trusted part of the system. This page explains how each part is
contained and where the limits are. [The isolation model](deployment/isolation.md) has the full detail for agent pods.

To report a vulnerability, use the [security policy](https://github.com/devthenet-labs/maul/security/policy) (privately,
through GitHub Security Advisories).

## The short version

- An agent pod holds **no credential**: no GitHub token, and no model key for the claude and codex runners.
- A claude or codex agent pod needs only maul's own services inside the cluster: the artifact server it fetches the code
  from, and the egress broker that carries its model traffic. It cannot reach the Kubernetes API, and with
  repository-declared agent images on (see Repository images under [Agent pods](#agent-pods); intent builds need them)
  its NetworkPolicy permits nothing outside the cluster either. [Narrow network](#agent-pods) says when the policy is
  wider.
- Everything an agent produces is checked by a controller before anything reaches GitHub, and a person approves the plan
  and merges the pull request.
- A preview of a pull request runs agent-written code, so it runs in an isolated namespace on its own browser origin,
  behind sign-in on the preview gateway (on the legacy EKS load balancer path, an IP allowlist, with sign-in a separate
  step).

## Agent pods

An **agent pod** is the Kubernetes pod an agent run happens in: one per investigation, remediation, plan, build or
revision round. maul's controllers create it as a Job in the agents namespace (`maul-agents` by default).

**No credential of any kind.** The pod does not clone the repository. source-controller downloads the repository as a
tarball at a pinned commit and serves it from an in-cluster artifact server; the pod fetches it by an unguessable URL
and checks its sha256 digest before unpacking it. Model traffic for the claude and codex runners goes through the
[egress broker](configuration/egress-broker.md), a maul service that holds the model credential and adds it to each
request on the way out. The pod proves who it is to the broker with a short-lived token bound to the broker's audience,
which no other service, the Kubernetes API included, accepts. The one exception is the copilot runner, which ships
disabled and carries its own GitHub token.

**No Kubernetes access.** The agent's ServiceAccount has no Role, and the pod mounts no ordinary ServiceAccount token.

**Locked down.** The agents namespace enforces the `restricted` Pod Security Standard (the chart labels it), and so
should the release namespace, which you label yourself. maul runs every pod non-root with a read-only root filesystem,
no capabilities and the runtime's default seccomp profile. A Job never restarts on failure; retries are maul's decision.

**Narrow network.** The agents namespace denies all traffic by default. A claude or codex pod needs DNS, the artifact
server and the broker, and with `agent.networkPolicy.dns: none` not even DNS. What the policy permits beyond that
depends on the install: the base policy also has a rule for TCP 443 to any host outside the cluster's own ranges, kept
for the copilot runner. The chart drops it under Cilium or GKE FQDN policies (`broadEgress: auto`). With repository
images on, it refuses to render until the rule is gone: set `agent.networkPolicy.broadEgress: never`, or use mode cilium
or gke. Otherwise (mode none or istio, or `broadEgress: always`) it stays, and a pod could reach any HTTPS host, GitHub
included, as an anonymous client. See [Network egress](deployment/isolation.md#network-egress-the-floor-and-the-fence).
The broker in turn forwards only the model API calls an agent needs and refuses server-side tools that reach the
internet, such as web search. Before a pod running a repository's own image starts the agent, a trusted init container
probes the internet, the Kubernetes API and the cloud metadata service from inside the pod; if any answers, the run
fails and the agent never starts.

**Read-only where it should be.** An investigation and an intent's plan run without a shell on claude, and may write
only their own report. A remediation or a build needs a shell to build and test, so it gets one; the pod is then the
boundary.

**Dependencies.** With narrow egress (repository images on, which builds need), an agent pod has no route to a package
registry, so a repository's dependencies must be in its agent image. An operator may grant one narrow exception, a
[network profile](deployment/isolation.md#network-profiles-the-go-dependency-route): a route through maul's egress proxy
to the Go modules or package registries the operator allows, for builds and remediations on an accepted repository image
only. The proxy holds any registry credential; the pod never does.

**Repository images.** A repository may name the image its builds run in, in `.maul/agent.yaml`
([Bring your own agent image](integrations/agent-images.md)). That image is treated as untrusted code with the agent's
powers. It runs under the same pod security, it is pinned to a digest, and only images from allowlisted registries
(signed, by default) are accepted.

## What reaches GitHub

The agent never writes to GitHub. Its only output is a structured result on its log: a report and, for a build or a
remediation, a **changeset** (the changed files' contents against the pinned commit). A controller reads it and checks
it before it makes any GitHub call:

- The changeset must be based on the exact commit the pod was given, and its paths must be ones git could produce.
- An intent's changeset may never change anything under `.github/`, `.maul/` or `.devcontainer/`: CI definitions, the
  agent image declaration and the devcontainer declaration.
- A changeset from a run on a repository-declared image is held to more: at most 500 changed or deleted files by
  default, no control characters in paths, and no change to `.github/workflows/` or `.github/actions/`. A pushed branch
  can start CI with the repository's secrets before a person has read it, which is why CI files are refused.

A refused changeset makes no GitHub call. The controller then pushes the branch and opens the pull request itself, with
a short-lived token for that one repository. GitHub App credentials live in a Secret only maul's controllers read.

People stay in control at two points. A plan is built only after an approver approves it on the issue, and the build
agent receives only that approved plan, never the issue. And maul never merges: a person reviews and merges every pull
request. A plan can be confidently wrong, about a security header for example; reviewing it is where that is caught.
[The intent lifecycle](intents/lifecycle.md) shows both steps.

## Webhooks and polling

Intents poll GitHub; they need no inbound connection. The findings flow receives webhooks, and integration-controller
authenticates each one before it reads the payload, with the method the source supports:

| Source                               | Authentication                                              |
| ------------------------------------ | ----------------------------------------------------------- |
| GitHub code scanning                 | HMAC-SHA256 signature with the Integration's webhook secret |
| Google Cloud Security Command Center | the OIDC token Pub/Sub signs on each push                   |
| Wiz                                  | a shared bearer token                                       |
| Generic HTTP                         | HMAC signature with that Integration's own secret           |

Each source page explains the setup: [GitHub](integrations/sources/github.md),
[Google Cloud SCC](integrations/sources/google-scc.md), [Wiz](integrations/sources/wiz.md),
[generic](integrations/sources/generic.md).

## Previews

A **preview** runs a pull request's code so people can try it. That code was written by an agent and has not been
reviewed yet, so maul treats it as hostile:

- **Its own origin.** Each preview gets its own host, `https://<project>-<issue>.<host suffix>`, so one preview's pages
  cannot read another's cookies or storage. Previews on paths of one shared host are not offered.
- **An isolated slot.** A preview runs in one of a few fixed slot namespaces. Their NetworkPolicy lets traffic in only
  from the edge in front of them (the preview gateway, or on the legacy EKS path the preview load balancer's subnets)
  and lets traffic out only to DNS: no Kubernetes API, no maul service, no metadata service, no internet. With the agent
  evaluator on, it also admits that round's evaluator to the slot tool pod, and the tool pod to the preview's
  components; nothing else. Admission policies allow a preview's pods only one container, from the operator's preview
  image path, no ServiceAccount token and no Secret references. Slot pods run on dedicated, tainted nodes unless the
  operator explicitly opts out.
- **Separate images.** The preview image path must not overlap the agent image allowlist, so an image built from a pull
  request can never run as an agent.
- **Sign-in.** Viewers sign in through Dex with GitHub, and maul admits them by an access review on the Preview's
  Project. Every recipe of `maul setup previews` ends with sign-in on; on the legacy EKS load balancer path it is a
  separate, staged step ([preview sign-in](intents/preview-sign-in.md)). The app behind it receives only an opaque id,
  stable for one viewer on one preview, never the viewer's login.
- **The preview gateway is on the path of hostile code.** It strips its own cookies and headers from what the app sees,
  refuses cross-preview writes, and filters what the app may set.

A cluster whose network plugin does not enforce NetworkPolicy cannot isolate previews and is not supported.
[Set up previews](intents/set-up-previews.md#security),
[the preview gateway's security notes](configuration/preview-gateway.md#security) and
[preview sign-in](intents/preview-sign-in.md) have the details, including what a stolen session cookie can do.

## The evaluator and its tool pods

An outcome is one result the approved plan promises; a round is one evaluation of the pull request heads, and each round
gets a fresh random key that every tool call must present.

With the [agent evaluator](intents/evaluator.md) on, an agent explores a preview and writes checks. It is split so that
no part holds both the model and the application:

- **The evaluator** is an agent pod that reaches only the broker and its round's tool pod. It has no DNS, no internet
  and no copy of the pull request's code; a strict sandbox probe, DNS included, proves that before it starts.
- **The slot tool pod** runs in the preview's slot. maul's relay in it runs every tool call and records each one in a
  ledger. It has no ServiceAccount token and no DNS. Programs the evaluator writes run in a separate sandbox container
  as their own user, holding nothing secret. A browser, when the Project has one, speaks only to a caller holding the
  round's key.
- **The agents tool pod**, for a command-line or library Project with no preview, runs in the agents namespace. It holds
  no ServiceAccount token and no model credential, and reaches only the artifact server and, if its build has one, its
  network profile. Every call to it needs the round's key.

The evaluator's own report never passes an outcome. maul passes one only on a check it stored and re-ran itself, which a
second model then judged to prove the outcome.

## The status page

The status page's intents views show plans and transcripts, which can contain private code. They run only with OIDC
sign-in and per-Project access reviews; see [the intents dashboard](intents/dashboard.md#two-read-tiers-per-project).
Those tiers bind the page only: anyone with `get` on ConfigMaps in maul's namespace, or on pod logs in the agents
namespace, can read the same content directly. The built-in `view`, `edit` and `admin` roles include both.

## What is not covered

- **Open HTTPS egress**, where the TCP 443 rule above is kept (`broadEgress` resolves to broad: mode none or istio, or
  `broadEgress: always`).
- **A compromised cluster** or a compromised maul controller. The controllers hold the GitHub App credential and the
  broker holds the model credential.
- **DNS**, unless closed. With the cluster resolver open, a determined agent can encode data in DNS queries. Close it
  with `agent.networkPolicy.dns: none`, or bound it with Cilium's DNS rules.
- **The first seconds of a pod** on some network plugins, before its NetworkPolicy attaches. maul's init container waits
  for the policy to take effect, and fails a repository-image run that is still open.
- **The node's kubelet** answers on its port from an agent pod on some platforms. The pod holds no credential it
  accepts; closing the port is a node-level control.
- **What the agent spends.** The broker's limits bound it, but they are off until an operator sets them. See
  [Costs and usage](usage.md#limits-on-spend).

[What the agent policy does not stop](deployment/isolation.md#what-the-agent-policy-does-not-stop) explains each of
these residuals.
