# Quick start: intents for a new organization

This page takes you from a cluster without maul to your first intent: an issue that an agent plans, you approve, and an
agent builds into a pull request. Two optional steps at the end add a live preview for each pull request.

It is one path through the commands, with the detail left on the pages each step links to. If a step fails, follow its
link: the detailed page has the reasons and the fixes.

The example uses a GitHub organization `acme`, an **intent repository** `acme/intents` (where people open issues that
ask for a change), one **application repository** `acme/shop` (where the change is built), a **Project** named `shop`
(the resource that ties the two together) and an **approver** `alice` (a GitHub account whose approval of a plan
counts). Substitute your own names.

| Step | What you do                                                                 | Command                 |
| ---- | --------------------------------------------------------------------------- | ----------------------- |
| 1    | [Install the CLI](#1-install-the-cli)                                       |                         |
| 2    | [Install maul with intents on](#2-install-maul)                             | `helm upgrade`          |
| 3    | [Create the GitHub App](#3-create-the-github-app)                           | `maul setup github-app` |
| 4    | [Write the Project](#4-write-the-project)                                   | `maul setup project`    |
| 5    | [Prepare the application repository](#5-prepare-the-application-repository) | `maul init app`         |
| 6    | [Check the Project](#6-check-the-project)                                   | `maul check project`    |
| 7    | [Open the first issue](#7-open-the-first-issue)                             |                         |
| 8    | [Set up previews](#8-set-up-previews-optional) (optional)                   | `maul setup previews`   |
| 9    | [Check the previews](#9-check-the-previews)                                 | `maul check previews`   |

## Before you start

You need:

- **A Kubernetes cluster**, version 1.34 or later, with a network plugin that enforces `NetworkPolicy`. The agent
  sandbox is made of network policies, and on a cluster that does not enforce them they do nothing.
- **Cluster admin rights.** The chart installs custom resource definitions and admission policies.
- **To be an Owner of the GitHub organization.** GitHub lets only an Owner create an organization's App.
- **A model credential**: an Anthropic API key, or a token from `claude setup-token`.
  [Install with Helm](install.md#create-the-secrets) shows how to store a token, and
  [the provider recipes](../deployment/helm.md#model-providers-brokered-claude) cover Amazon Bedrock, Google Vertex AI
  and Microsoft Foundry.
- **A container registry** the cluster can pull from. It holds the application's _agent image_: the image, with the
  application's toolchain in it, that the build agent runs in. Step 5 says which registries `maul init app` supports.
- **Tools on your workstation:** `kubectl` and `helm` (3.8 or later) signed in to the cluster, and `gh` signed in to
  GitHub (`gh auth status`).
- **The two repositories.** Any repository can be the intent repository. A dedicated one keeps requests apart from code:

  ```sh
  gh repo create acme/intents --private
  ```

You do **not** need GitHub code scanning or an HTTPS address that GitHub can reach. Those belong to the
[findings path](index.md#the-findings-path). The intents flow polls GitHub and receives no webhook.

## 1. Install the CLI

The `maul` CLI is a release archive on GitHub. [Install](../cli.md#install) has the download and signature check. Use
the CLI of the release you install in the cluster. This page needs release 0.14.13 or later, the first with
`maul setup project`.

```sh
gh release view --repo devthenet-labs/maul --json tagName --jq .tagName   # the newest release, such as v0.14.15
maul --version
```

`X.Y.Z` below stands for that release, without the `v`.

## 2. Install maul

Create the namespace, with the `restricted` Pod Security labels the chart expects, and the Secret that holds the model
credential:

```sh
kubectl create namespace maul
kubectl label namespace maul \
  pod-security.kubernetes.io/enforce=restricted \
  pod-security.kubernetes.io/audit=restricted \
  pod-security.kubernetes.io/warn=restricted

kubectl -n maul create secret generic maul-anthropic --from-literal=api-key="$ANTHROPIC_API_KEY"
```

The model credential lives in maul's own namespace, where only the egress broker reads it. No agent pod ever holds it.

Write a values file. These are the keys an intents install sets:

```yaml
# maul-values.yaml
egressBroker:
  anthropicSecret: maul-anthropic
  anthropicAuth: key # `token` for a `claude setup-token` token

agent:
  # A build runs in the image the application repository declares in .maul/agent.yaml.
  repositoryImages:
    enabled: true
    # The registry path every agent image must sit under. Use your own registry.
    registries:
      - 123456789012.dkr.ecr.us-east-1.amazonaws.com/maul/app-envs/
    # Admit unsigned agent images for now. An explicit opt-out: registry write access
    # alone then decides which image an agent runs.
    allowUnsigned: true
    ephemeralStorage: 8Gi
  networkPolicy:
    # Agents need no route to the internet: model traffic goes through the broker.
    broadEgress: never

intentController:
  enabled: true
```

```sh
helm upgrade --install maul oci://ghcr.io/devthenet-labs/maul/charts/maul --version X.Y.Z \
  --namespace maul -f maul-values.yaml
kubectl -n maul get deploy
```

Every Deployment should become available, `maul-intent-controller` among them. The controllers of the findings flow are
installed too, and they idle until you give them an `Integration`.

!!! note "This values file is a starting point"

    The keys above are taken from
    [Deploying intents and previews](../intents/deploying.md#6-helm-stage-1-controllers-and-intents), which is the
    complete path on EKS Auto Mode and explains each one. Three things it covers that this page does not:

    - **Your cluster's own settings.** On EKS Auto Mode, for example, the chart also needs `clusterDNSCIDR`.
    - **Registry access.** maul's source-controller reads each agent image from the registry, and the nodes pull it. For
      a private registry, give both access with a pull secret or the cluster's workload identity; see
      [Repository runner images](../deployment/helm.md#repository-runner-images).
    - **Spend limits.** The broker can cap what an agent pod spends, and every limit is off by default. Set a spend
      limit on the key at your model provider, and see
      [Egress broker limits](../deployment/helm.md#egress-broker-limits).

    Signing the agent images, so that `allowUnsigned` can go, is in
    [Signing the agent image](../intents/onboarding-app.md#signing-the-agent-image).

## 3. Create the GitHub App

maul signs in to GitHub as a GitHub App. One command creates it with exactly the permissions intents use:

```sh
GH_TOKEN=$(gh auth token) maul setup github-app --org acme --intents --checks -n maul
```

`--checks` adds the read permissions that let maul fix a failing check on its own pull request. A browser page opens.
Check the form and click **Create GitHub App**. The command then writes two files and prints a numbered `next:` block:

1. **Install the App.** Open the link it prints. Choose the organization, then **Only select repositories**, and tick
   `acme/intents` and `acme/shop`.
2. **Give the cluster the credential.** Paste the `kubectl` line it prints:

   ```sh
   kubectl create -f maul-github-acme.secret.yaml && rm -f maul-github-acme.secret.yaml && kubectl apply -f maul-github-acme.forge.yaml
   ```

The first file is a Secret with the App's private key. It is the only copy, and the line above deletes the file once the
cluster holds it. The second file is a **Forge**: the resource that tells maul which App credential covers the
organization's repositories. It holds no secret, so keep it in git.

[Add a GitHub organization or a Project](../intents/add-an-organization.md#add-a-github-organization) shows the output
and what to do when a step is refused.

## 4. Write the Project

```sh
maul setup project shop --intent-repo acme/intents --repo acme/shop --approver alice -n maul
kubectl apply -f shop.project.yaml
```

The first command talks to neither the cluster nor GitHub. It writes `shop.project.yaml`, which holds no secret: keep it
in git too. A Project name is at most 25 characters.

Approvers are the only accounts whose labels and commands count. Repeat `--approver` for each one, and include your own
GitHub login if you will open the first issue yourself.

Each approver needs **write** access to the intent repository. A label or command from anyone without it is refused:

```sh
gh api -X PUT repos/acme/intents/collaborators/alice -f permission=push
```

There are no labels to create. maul creates the trigger label `maul:shop` and the approve label `maul:approved` on the
intent repository itself.

## 5. Prepare the application repository

An agent can plan a change in any repository, but it builds only in the repository's own agent image, which the
repository declares in the file `.maul/agent.yaml`. Until that image exists, plans run and builds wait.

`maul init app` writes that file, the image's Dockerfile and the GitHub workflows that build and publish it. Run it in a
checkout of the application repository:

```sh
cd shop
maul init app --existing --registry 123456789012.dkr.ecr.us-east-1.amazonaws.com
git add -A && git commit -m "chore: scaffold with maul init app" && git push
```

`--existing` is for a repository that already has code. Without it, the command also writes a small Go service for a new
application. The command prints its own next steps: create the registry repositories and the publisher roles, set the
repository's Actions variables, and run the `agent image` workflow.
[Onboarding an application](../intents/onboarding-app.md) is the whole procedure, and
[Publishing the images](../intents/onboarding-app.md#publishing-the-images) is the part that puts the agent image in the
registry.

!!! warning "`maul init app` targets Amazon ECR"

    `--registry` must be an ECR registry host, and the workflows it writes publish to ECR through IAM roles that maul's
    reference terraform module creates. With another registry, do not run `init app`: build an agent image, push it
    under a path your values file lists in `agent.repositoryImages.registries`, and commit a `.maul/agent.yaml` that
    names it. [Bring your own agent image](../integrations/agent-images.md) describes the file and what the image must
    contain.

## 6. Check the Project

```sh
GH_TOKEN=$(gh auth token) maul check project shop -n maul
```

It prints one line per check, each `PASS`, `FAIL` or `SKIP`, and every `FAIL` names its fix. maul validates a new
Project within about a minute, so if `ready` says it has not reported yet, run the check again. `agent-image` fails
until the agent image from step 5 is published.

[When a check fails](../intents/add-an-organization.md#when-a-check-fails) lists each failure and its fix.

## 7. Open the first issue

Open an issue in the intent repository that says what you want, and add the trigger label. The label counts only when an
approver with write access applies it. A label added as the issue is created counts as the issue author's, so run this
as an approver:

```sh
gh issue create --repo acme/intents --label maul:shop \
  --title "Add a /healthz endpoint" \
  --body "The service should answer GET /healthz with 200 and the body ok."
```

Then, on the issue:

1. **maul posts a plan** as a comment. It polls GitHub about once a minute, so allow a little time.
2. **Read the plan and approve it**: add the label `maul:approved`, or comment `/maul approve`. To ask for a different
   plan instead, comment on the issue and then `/maul replan`.
3. **maul builds the plan and opens a pull request** in `acme/shop`.
4. **Review the pull request as you would any other.** A review that requests changes, or the comment
   `/maul revise <what to change>` on the pull request, starts another agent round.
5. **Merge it.** maul closes the issue.

`/maul cancel` on the issue stops the intent and closes the issue.
[On the issue](../configuration/intent-controller.md#on-the-issue) and
[On the pull request](../configuration/intent-controller.md#on-the-pull-request) describe every action.

Intents work from here on. The remaining steps add previews.

## 8. Set up previews (optional)

With previews, each intent's pull request also runs at a URL of its own, `https://<project>-<issue>.<host suffix>`,
behind sign-in. It is removed when the intent ends, and it expires 72 hours after its last deployment; the next push
deploys it again. Previews work on any Kubernetes cluster. They need more than intents do, so have these ready first:

- **A wildcard DNS name** for the preview hosts, `*.<host suffix>`, and one more host name for sign-in, ideally on a
  different registrable domain.
- **A certificate** for both, or an edge that holds one.
- **[Dex](https://dexidp.io/)** with its GitHub connector, for sign-in.
- **Nodes for previews**, labelled and tainted so that only previews run there.
- **A runtime image** of the application for each pull request, pushed to a registry under the tag `sha-<commit>`. The
  workflows `maul init app` writes publish it once the repository variable `PREVIEW_PUBLISH_ENABLED` is set, which is
  the last of the publishing gates; see [The gates, in order](../intents/onboarding-app.md#the-gates-in-order).
  [The runtime contract](../intents/onboarding-app.md#the-runtime-contract) says what that image must do.

[What every install needs](../intents/set-up-previews.md#what-every-install-needs) explains each one. Then run:

```sh
maul setup previews --detect -n maul
```

On a terminal the command asks for each answer it needs. With `--detect` it also reads the cluster, without changing it,
to fill in what it can. It writes a directory, `./maul-previews/`, with the chart values, the manifests and a file
`NEXT-STEPS.md` that lists what to do in order for your cluster. Follow that file. The files under `secrets/` hold
credentials: apply or encrypt them, and never commit them as they are.

[Set up previews](../intents/set-up-previews.md) describes the four ways to expose previews (a Cloudflare Tunnel, a load
balancer, a Tailscale device, or an ingress you already run) and how to choose.

Last, tell the Project how to run the application. Add a `preview` block under `spec` in `shop.project.yaml` and apply
the file again:

```yaml
spec:
  # intentRepository, approvers and repositories as written
  preview:
    imageRepository: 123456789012.dkr.ecr.us-east-1.amazonaws.com/maul/previews/shop
    port: 8080
    readinessPath: /healthz
```

Once you have edited the file by hand, keep editing it by hand: `maul setup project --force` writes it anew from its
flags and drops the block. [The Project](../intents/onboarding-app.md#the-project) describes these fields.

## 9. Check the previews

`NEXT-STEPS.md` ends with this check. It judges the preview install as a whole, before any preview exists:

```sh
maul check previews -n maul
```

Every line should be `PASS`. A `WARN` or `INFO` line does not fail the run, but read it. Then prove once that preview
slots are isolated from each other. This creates four short-lived Pods and deletes them:

```sh
maul check previews -n maul --isolation
```

Run `GH_TOKEN=$(gh auth token) maul check project shop -n maul` again as well: it now also checks the Project's preview
settings. The next intent's pull request gets a preview link.
[Checking the preview install](../cli.md#checking-the-preview-install) says what each check reads.

## Where to go next

- **[Onboarding an application](../intents/onboarding-app.md)**: the steps you repeat for each application repository.
- **[Add a GitHub organization or a Project](../intents/add-an-organization.md)**: another Project takes three commands.
- **[The agent evaluator](../intents/evaluator.md)**: have maul test each change against its plan before people review
  it.
- **[The intents dashboard](../intents/dashboard.md)**: a board of every intent on the status page.
- **[Deploying intents and previews](../intents/deploying.md)**: the complete reference path on AWS, with terraform.
- **[Sizing limits for large features](../intents/sizing.md)**: budgets and limits for larger changes.
