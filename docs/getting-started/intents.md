# Intents and previews

Intents are maul's issue-to-pull-request flow. Someone opens an issue in an **intent repository**, the repository where
requests are written. maul plans the change and posts the plan on the issue. An **approver**, a GitHub account the
Project names, approves it. maul then builds exactly that plan in the application's own toolchain image and opens the
pull request. With previews on, each such pull request also runs at a URL of its own until it merges or closes.

This page says what the flow adds to an installation and which page installs it. To install it now, go to the
[quick start](quick-start.md).

## What intents add

Intents run on the same chart as findings. You can install them alone, or switch them on beside a findings install that
already runs. They add:

- **The intent-controller** (`intentController.enabled`). It polls GitHub, so it needs no webhook and no address that
  GitHub can reach.
- **Repository-declared agent images** (`agent.repositoryImages`). A build runs in the application's own toolchain
  image, which the repository declares in `.maul/agent.yaml`, and never in maul's default image, which has no toolchain.
  [Bring your own agent image](../integrations/agent-images.md) describes the file.
- **A `Project` for each application.** It ties an intent repository to the application repositories and names the
  approvers. `maul setup project` writes it as a file, and you apply it. A Project covers one repository or, with
  multi-repository intents on (`intentController.config.multiRepo`), up to eight.
- **The GitHub App's intent permissions**, such as issues on the intent repository, and contents and pull requests on
  each application repository. `maul setup github-app --intents` asks for them, and
  [Create the GitHub App](github-app.md#intents) lists each permission and why it is needed.

Intents do not need GitHub code scanning, a webhook, or any particular cloud.

## What previews add

Previews are optional and build on intents. They add:

- **The preview-controller** (`previewController.enabled`) and the preview foundation (`preview.enabled`): a fixed set
  of isolated namespaces, called slots, that pull request code runs in.
- **A route to the previews, with sign-in.** On any Kubernetes cluster this is the
  [preview gateway](../configuration/preview-gateway.md), and `maul setup previews` writes everything it needs. On EKS
  Auto Mode, previews can instead run behind an Application Load Balancer that the chart and the reference terraform
  module create.
- **A wildcard DNS name and a certificate** for the preview hosts, and **Dex** for sign-in.
- **Nodes set aside for previews**, and a **runtime image** of each pull request in a registry.
- **A `preview` block on the Project**, which says which image to run, on which port, and which path answers when the
  application is ready.

Previews are in the Helm chart only. The kustomize tree has an intent-controller component and no preview one.

## Which page to follow

| Your situation                                              | Page                                                                        |
| ----------------------------------------------------------- | --------------------------------------------------------------------------- |
| A new install, on any cluster                               | [Quick start](quick-start.md)                                               |
| A new install on AWS: EKS Auto Mode, ECR, ACM and terraform | [Deploying intents and previews](../intents/deploying.md)                   |
| maul already runs; add an organization or another Project   | [Add a GitHub organization or a Project](../intents/add-an-organization.md) |
| Another application repository for an existing install      | [Onboarding an application](../intents/onboarding-app.md)                   |
| Previews, on an install that already runs intents           | [Set up previews](../intents/set-up-previews.md)                            |
| maul checking each change against its plan before people do | [The agent evaluator](../intents/evaluator.md)                              |

## Adding intents to a findings install

On an installation you [verified with a finding](verify.md), intents are a values change and three commands:

1. **Upgrade the release** with `intentController.enabled: true` and the `agent.repositoryImages` block.
   [Quick start, step 2](quick-start.md#2-install-maul) shows the keys.
2. **Give the App the intent permissions.** Add the permissions listed in [Create the GitHub App](github-app.md#intents)
   to the App you registered, and accept the change on its installation. Install the App on the intent repository too.
3. **Write a Project, prepare the application repository and check it**, as in quick start steps
   [4](quick-start.md#4-write-the-project) to [6](quick-start.md#6-check-the-project).

The intent-controller may read only the Secrets named in the chart's `intentController.forgeSecrets`, which defaults to
`[maul-github]`, the Secret name [Install with Helm](install.md#create-the-secrets) uses. If your Forge's Secret has
another name, list it there.

Keep one `Forge` for each organization. A second Forge for the same organization makes every repository's Forge
ambiguous; see [GitOps and the maul-config chart](../intents/add-an-organization.md#gitops-and-the-maul-config-chart).
