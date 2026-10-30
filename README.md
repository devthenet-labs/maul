# maul

**Issues in. Shipped PRs out.**

maul runs sandboxed AI coding agents (Claude Code, Codex) on your own Kubernetes cluster, across the software
development lifecycle. Someone describes a change in a GitHub issue; an agent plans it, a human approves the plan, an
agent builds it and opens the pull request, and a human merges it. Kubernetes custom resources carry all of the state.

## What it does

- **Intents.** Open a GitHub issue saying what you want. An agent reads the code and posts a plan on the issue; an
  approver approves it (`/maul approve` or the approve label), and an agent builds exactly that plan in a sandboxed pod,
  in the application's own toolchain image, and opens the pull request. Review feedback and failing checks get further
  rounds. An issue marked **blocked by** others on GitHub waits until they are closed as completed
  ([intent dependencies](docs/intents/dependencies.md); `/maul start` starts it anyway).
- **Previews.** Each intent's pull request is deployed to a live URL of its own until it merges or closes, behind
  optional sign-in.
- **Fixing.** Security alerts (GitHub Advanced Security / CodeQL, Wiz, Google Cloud Security Command Center, or any HTTP
  source through the generic integration) become `Finding` resources with a tracking issue each. An agent investigates
  every one: false positives are dismissed, confident fixes are attempted in priority order and opened as pull requests,
  and everything else is handed to the repository owner. A failing CI check on maul's own pull request gets a fix round
  of its own.
- **A trust model you can audit.** Claude and Codex agent pods hold no credentials: model traffic goes through an
  in-cluster egress broker that injects the key, and the agent never talks to GitHub. The repository arrives as a
  digest-verified tarball. A human approves every intent's plan and merges every pull request.
- **Operable.** The Kubernetes API is the only state store. A status dashboard and the `maul` CLI (also `kubectl maul`)
  show and act on everything, under your RBAC. Optional per-user model credentials, usage and cost tracking, and network
  profiles for dependency access.

## Components

| Binary                     | Concern                                                                                                                                    |
| -------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------ |
| `intent-controller`        | Optional. Plans GitHub issues in a read-only agent Job, posts the plan for approval, builds the approved plan and opens the pull request   |
| `preview-controller`       | Optional. Deploys each intent pull request into an isolated preview slot                                                                   |
| `integration-controller`   | The one internet-facing entry point: validate provider webhooks, ingest alerts into Findings, project tracking issues, apply human signals |
| `source-controller`        | `Forge`/`Repository` reconcilers: resolve credentials, pin the head SHA, serve the repository tarball agents fetch credential-lessly       |
| `context-controller`       | Enhance freshly opened Findings with CMDB ownership / infrastructure context                                                               |
| `investigation-controller` | Gate eligible findings, run analysis agent Jobs, route the verdicts                                                                        |
| `remediation-controller`   | Queue admission, priority-ordered remediation Jobs, changeset push + pull requests, rollup statistics and the finding TTL                  |
| `agent-runner`             | In-pod coding-agent runtime: one stage per Job via `claude -p` or `codex exec`                                                             |
| `egress-broker`            | The reverse proxy all model traffic goes through: adds the credential outside the pod, enforces spend limits, meters usage                 |
| `status-server`            | The human-facing status page: findings, intents and usage views, OIDC sign-in, the RBAC-gated actions                                      |
| `maul`                     | The workstation CLI: list, review and act on findings and intents over your own kubeconfig — never through a controller                    |

Optional extras — `egress-proxy` (dependency access for agents), `preview-auth` (preview sign-in) and
`evaluation-controller` (remote skill evaluation) — are described in [AGENTS.md](AGENTS.md).

See [DESIGN.md](DESIGN.md) for the full requirements and the state machine, and [AGENTS.md](AGENTS.md) for a map of the
repository. Full documentation — getting started, configuration and deployment references — lives at
[devthenet-labs.github.io/maul](https://devthenet-labs.github.io/maul/) (source under [`docs/`](docs/); build locally
with `mise run serve`).

## Building

The toolchain comes from the `.mise/` submodule ([mise](https://mise.jdx.dev) provisions the pinned tools):

```sh
git submodule update --init
make build   # every binary into bin/
make test    # unit tests with race + coverage
make lint    # golangci-lint, govulncheck, license headers, prose linters
make pr      # the full local gate before a pull request
```

## Testing

```sh
make test    # unit tests (race + coverage)
make e2e     # envtest + the real binaries, driven by recorded webhooks against an in-memory GitHub
```

## Deploying

Container images and kustomize manifests live in [`deploy/`](deploy/); `deploy/README.md` covers the GitHub App setup,
the secrets and custom resources to create, and how the agent's isolation actually works.

```sh
kubectl apply -k deploy/kustomize/overlays/dev
```

Helm charts rendering the same stack are published to OCI on every release — [`charts/maul`](charts/maul/README.md) is
the stack; [`charts/maul-config`](charts/maul-config/README.md) carries the Integration/Forge custom resources,
installed as a second release once the CRDs exist:

```sh
helm install maul oci://ghcr.io/devthenet-labs/maul/charts/maul --namespace maul --create-namespace
```

## The CLI

The workstation CLI ships separately from the cluster components, as the `maul-cli` archive attached to each
[release](https://github.com/devthenet-labs/maul/releases), with a keyless cosign signature for each binary:

```sh
gh release download vX.Y.Z --repo devthenet-labs/maul --pattern 'maul-cli_X.Y.Z_darwin_arm64.tar.gz'
```

[`docs/cli.md`](docs/cli.md#install) has the verification and install steps. The Homebrew tap
`bitwise-media-group/tap/maul` is the upstream project's older CLI, without `setup`, `init` or `check project`.

It talks to the Kubernetes API with your own kubeconfig, so what you can do is exactly what your RBAC allows. The
archive also carries `kubectl-maul`, so every command works as `kubectl maul …`, with shell completion for both
spellings. See [`docs/cli.md`](docs/cli.md) for the tour and [`docs/cli/maul.md`](docs/cli/maul.md) for the generated
command reference.

## Status

All components are implemented and tested end to end. The context enhancer is a deliberate placeholder — the plugin seam
(`pkg/enhance`) and a YAML-backed static enhancer ship, and a real CMDB integration implements the same interface.
