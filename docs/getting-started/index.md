# Getting started

**maul** runs sandboxed AI coding agents on your own Kubernetes cluster and turns their work into pull requests that
people review and merge. [What is maul](../overview.md) explains the idea. This section installs it.

maul has two flows, and you can start with either:

- **Intents.** Someone opens a GitHub issue that asks for a change. An agent plans it, an approver approves the plan,
  and an agent builds it and opens the pull request. Most installs start here.
- **Findings.** Security alerts from a scanner arrive by webhook. An agent investigates each one, dismisses false
  positives, opens a pull request for a fix it is confident about, and hands the rest to a person.

Both run on one installation of the same Helm chart. What differs is what you switch on and what GitHub must be able to
reach.

## Choose a path

| You want                                               | Start at                                                                                |
| ------------------------------------------------------ | --------------------------------------------------------------------------------------- |
| Issues turned into pull requests, on any cluster       | [Quick start](quick-start.md)                                                           |
| The same on AWS, with EKS Auto Mode, ECR and terraform | [Deploying intents and previews](../intents/deploying.md)                               |
| Security alerts investigated and fixed                 | [The findings path](#the-findings-path), below                                          |
| Intents added to an install that already runs findings | [Adding intents to a findings install](intents.md#adding-intents-to-a-findings-install) |
| A local development cluster on a laptop                | [Local dev on Colima](../deployment/colima.md)                                          |

## What you need

Every install needs these:

| Prerequisite                  | Why                                                                                                                                                                                                                                        |
| ----------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| Kubernetes 1.34 or later      | The chart's `kubeVersion` floor. The chart's agent namespace enforces the `restricted` Pod Security Standard, and maul's own namespace should be labelled `restricted` before you install (see the [quick start](quick-start.md), step 2). |
| Helm 3.8 or later             | The charts are installed from an OCI registry (`helm install oci://…`).                                                                                                                                                                    |
| A GitHub organization's Owner | To create the GitHub App maul signs in with, and to install it on the repositories maul works in.                                                                                                                                          |
| A model credential            | An Anthropic API key or a `claude setup-token` token, which the agents' `claude` CLI uses through the egress broker. Bedrock, Vertex AI and Foundry work too.                                                                              |

Each flow adds its own:

| Flow     | Also needs                                                                                                                                                        |
| -------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Intents  | A container registry for each application's agent image. No webhook: maul polls GitHub, so GitHub never needs to reach the cluster.                               |
| Findings | A finding source, such as GitHub code scanning (CodeQL) enabled on the watched repositories, and an HTTPS address where GitHub can reach maul's webhook receiver. |
| Previews | A wildcard DNS name, a certificate for it, Dex for sign-in, and nodes set aside for previews. [Set up previews](../intents/set-up-previews.md) lists them.        |

!!! tip "Hostname-level egress needs Cilium, GKE Dataplane V2 or Istio"

    The chart always renders baseline `NetworkPolicy` objects, but plain L3/L4 policies cannot match hostnames. The
    default claude runner needs no hostname layer at all: its model traffic goes through the in-cluster egress
    credential broker, so its egress is entirely cluster-local, and so is codex's. For the non-brokered copilot runner,
    `agent.networkPolicy.mode` selects the dialect your infrastructure enforces (`cilium`, `gke` or `istio`) and
    defaults to `auto`, which detects it from the cluster itself. There are deliberately no GitHub hosts in the
    allowlist: the agent pod never talks to a forge. See the [isolation model](../deployment/isolation.md) for what
    each layer requires.

## The intents path

[Quick start](quick-start.md) is the whole path on one page: install the chart with intents on, create the GitHub App,
write a Project, prepare an application repository, and open the first issue. Previews are its last, optional steps.

[Intents and previews](intents.md) explains what the intents flow adds to an installation, piece by piece.

## The findings path

Three pages, in order:

1. **[Create the GitHub App](github-app.md)**: register the App, grant its repository permissions, subscribe its webhook
   events, and collect the App ID, private key and webhook secret.
2. **[Install with Helm](install.md)**: create the Secrets, install the chart from the OCI registry, and switch the
   pipeline on with an `Integration` and a `Forge`. (A [kustomize tree](../deployment/kustomize.md) renders the same
   stack if you prefer.)
3. **[Verify the pipeline](verify.md)**: follow one finding from alert to pull request.

GitHub code scanning is one source of findings. Google Cloud Security Command Center, Wiz and a generic HTTP source are
the others; see [Integrations](../integrations/index.md).

## The parts you will deploy

The chart installs these by default:

| Component                  | Runs as                   | Concern                                                                                        |
| -------------------------- | ------------------------- | ---------------------------------------------------------------------------------------------- |
| `integration-controller`   | Deployment (1 replica)    | The one internet-facing entry point: webhooks in, Findings ingested, tracking issues projected |
| `source-controller`        | Deployment (1 replica)    | `Forge`/`Repository` reconcilers; SHA-pinned repository tarballs served to the agents          |
| `context-controller`       | Deployment (1 replica)    | Enhance freshly opened findings with ownership / infrastructure context                        |
| `investigation-controller` | Deployment (1 replica)    | Gate eligible findings, run analysis agent Jobs, route the verdicts                            |
| `remediation-controller`   | Deployment (1 replica)    | Priority-ordered remediation Jobs, branch push + pull requests, rollup stats and the TTL       |
| `agent-runner`             | Ephemeral Job per attempt | The coding-agent runtime inside each sandboxed pod, running `claude -p` (or `codex exec`)      |
| `egress-broker`            | Deployment (1 replica)    | The agents' model traffic: authenticates the pod, adds the model credential outside it         |
| `status-server`            | Deployment (1 replica)    | The status page: public rollup statistics, and the findings to signed-in users                 |

The findings controllers are installed even when you use only intents. They idle until an `Integration` exists.

These are optional and off by default:

| Component               | Switch                         | Concern                                                                                    |
| ----------------------- | ------------------------------ | ------------------------------------------------------------------------------------------ |
| `intent-controller`     | `intentController.enabled`     | The intents flow: plans issues, posts plans for approval, builds them, opens pull requests |
| `preview-controller`    | `previewController.enabled`    | Runs each intent's pull request as a live preview                                          |
| `preview-auth`          | `previewAuth.enabled`          | Sign-in for previews and, as the preview gateway, the route to them                        |
| `egress-proxy`          | `egressProxy.enabled`          | Lets build agents fetch dependencies from registries you allow                             |
| `evaluation-controller` | `evaluationController.enabled` | Remote skill evaluations for the separate `evolve` tool                                    |

Every controller is a singleton by construction: `replicas: 1` with `strategy: Recreate`. The leader-election Lease each
one takes is insurance against a botched rollout, not a scaling mechanism. Do not scale the Deployments.
