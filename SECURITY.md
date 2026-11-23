# Security Policy

## Reporting a vulnerability

Please report vulnerabilities privately via
[GitHub Security Advisories](https://github.com/devthenet-labs/maul/security/advisories/new). Do not open public issues
for security reports.

## Threat model (summary)

maul is a set of Kubernetes services that run coding agents on untrusted input (issue text, scanner-alert text and the
code of the repositories they work on) inside short-lived Jobs, and turn the agents' results into GitHub pull requests.
The agent is the least-trusted component. The full model is on the docs site:
[Security model](https://devthenet-labs.github.io/maul/security/) and
[Isolation model](https://devthenet-labs.github.io/maul/deployment/isolation/). maul defends against:

- **Credential exposure to the agent.** An agent pod holds no credential of any kind. It does not clone: the repository
  arrives as a tarball from source-controller's in-cluster artifact server, verified against a pinned sha256 digest.
  Model traffic for the claude and codex runners goes through the egress broker, which holds the model credential; the
  pod authenticates to it with a projected ServiceAccount token bound to the broker's audience, which nothing else
  accepts. The agent ServiceAccount has no Role. The optional copilot runner, disabled by default, is the one exception:
  its pod carries its own GitHub token.
- **Agent network access.** Agent pods run under a default-deny NetworkPolicy. A claude or codex pod needs only DNS
  (which can be closed), the artifact server and the broker; the base policy's TCP 443 rule for the copilot runner is
  dropped automatically under Cilium or GKE FQDN policies; with repository images on, the chart refuses to render until
  it is dropped (`agent.networkPolicy.broadEgress: never`, or mode cilium or gke). Where it stays, it lets a pod reach
  external HTTPS hosts without any credential. The broker forwards only the model API calls an agent needs, and refuses
  server-side tools that reach the internet.
- **Agent-authored outputs.** Everything an agent emits is data. Reports are schema-checked, and a changeset is
  validated by a controller before any GitHub call: its base must be the pinned commit, an intent's changeset may not
  touch `.github/`, `.maul/` or `.devcontainer/`, and a run on a repository-declared image may not change CI
  definitions. All GitHub writes happen controller-side with short-lived, single-repository tokens. A person approves
  each intent's plan, and maul never merges a pull request.
- **Hostile preview code.** Previews of unreviewed pull requests run in isolated slot namespaces on their own browser
  origins, with DNS as their only egress, behind sign-in on the preview gateway (a separate, staged step on the legacy
  EKS load balancer path).
- **Forged webhooks.** Each finding source's deliveries are authenticated before any payload is parsed: an HMAC-SHA256
  signature for GitHub and generic sources, the Pub/Sub OIDC token for Google Cloud SCC, a bearer token for Wiz. The
  intent flow polls GitHub and takes no webhook.
- **Tampered release artifacts.** Releases ship `checksums.txt`, SBOMs, keyless Sigstore (cosign) signatures and a SLSA
  build-provenance attestation; container images get the same attestation, and the Helm chart is cosign-signed.

Out of scope: the repositories maul operates on and the model provider itself; a compromise of the release workflow's
signing identity; and a compromise of the cluster maul runs in or of its controllers, which hold the GitHub App and
model credentials.

## Code scanning triage

CodeQL findings are triaged in [`security/code-scanning/index.md`](security/code-scanning/index.md), with a report per
finding recording why it was dismissed or how it was remediated.
