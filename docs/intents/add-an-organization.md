# Add a GitHub organization or a Project

This page connects maul to a GitHub organization and gives it its first Project, in five typed commands and two clicks
on GitHub. Each command prints exactly what comes next, so you can follow the terminal as well as this page. A second
Project in the same organization takes three commands.

The example throughout is the organization `overdubstudio`, its intent repository `overdubstudio/intents`, one
application repository `overdubstudio/app`, a Project named `overdub` and an approver `alice`. Substitute your own.

## What the words mean

- **The GitHub App** is how maul signs in to GitHub. Each organization gets its own App. maul mints a short-lived token
  from it for every operation, scoped to one repository.
- **A Forge** tells maul which GitHub App credential (a Kubernetes Secret) covers which organization's repositories.
- **A Project** ties an **intent repository** (where people open issues describing a change) to the **application
  repositories** the changes are built in, and names the **approvers** whose approval of a plan counts.

You never write any of these by hand. The `maul setup` commands write them as plain Kubernetes manifests, and you apply
those with `kubectl`.

## Before you start

- **maul is running in the cluster, with intents on.** If not, install it first:
  [Deploying intents and previews](deploying.md) is the whole path, and its step 2 is the first command below. To check,
  with your kubeconfig pointing at the cluster:

  ```sh
  kubectl -n maul get deploy maul-intent-controller
  ```

  It should show `1/1` ready. A release that is not named `maul` changes that name; see
  [A release not named maul](#a-release-not-named-maul).

- **You are an Owner of the organization**, not only a member. GitHub lets only an Owner create an organization's App.
- **Tools on your workstation:** the [maul CLI](../cli.md#install) at the release the cluster runs, `kubectl` signed in
  to the cluster with rights to create Secrets, Roles and maul's resources in its namespace, and `gh` signed in to
  GitHub (`gh auth status`).
- **The repositories exist:** the intent repository (any repository works; a dedicated one keeps issues apart from code,
  `gh repo create overdubstudio/intents --private`) and each application repository.

## Add a GitHub organization

### 1. Create the GitHub App

```sh
GH_TOKEN=$(gh auth token) maul setup github-app --org overdubstudio --intents --checks -n maul
```

`--intents` asks for exactly the permissions intents use, and `--checks` adds the reads that let maul fix a failing
check on its own pull request. With your token, maul first asks GitHub whether you are an Owner of the organization:

- An Owner sees `maul: you are an Owner of overdubstudio` and goes on.
- A member is refused before anything is created:
  `you are a member, not an Owner, of overdubstudio: ask an Owner to run this`.
- Without a token, maul warns that you must be an Owner and goes on. The token is used for that one request only, and is
  never stored or printed.

**First click on GitHub.** A browser page opens and hands GitHub the App's manifest. Check the form and click **Create
GitHub App**. GitHub sends the browser back to maul, which prints:

```text
maul: created GitHub App "maul-overdubstudio" (ID 1234567) owned by overdubstudio: https://github.com/apps/maul-overdubstudio
maul: wrote Secret maul/maul-github-overdubstudio (appID, privateKey) to maul-github-overdubstudio.secret.yaml, mode 0600. It holds the only copy of the private key: create or encrypt it, then delete it.
maul: wrote Forge maul/overdubstudio and the Role letting intent-controller read Secret maul-github-overdubstudio to maul-github-overdubstudio.forge.yaml. It holds no secret: keep it.
maul: next:
  1. Install the App on overdubstudio (choose the organization, not your own account):
     https://github.com/apps/maul-overdubstudio/installations/new
     Select the intent repository and every application repository.
  2. kubectl create -f maul-github-overdubstudio.secret.yaml && rm maul-github-overdubstudio.secret.yaml && kubectl apply -f maul-github-overdubstudio.forge.yaml
     Run it once maul is installed: it needs namespace maul and maul's CRDs.
  3. maul setup project <name> --intent-repo overdubstudio/<intent-repo> --repo overdubstudio/<app-repo> --approver <github-login> -n maul
maul: the Role binds ServiceAccount maul-intent-controller in maul; pass --intent-controller-sa if your release is not named maul.
maul: for the App just created, change the subject's name in maul-github-overdubstudio.forge.yaml instead (kubectl -n maul get serviceaccounts lists it): another run creates another App.
```

It wrote two files:

| File                                    | Holds                                                                                                                                                                               | What to do with it                                                                       |
| --------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------- |
| `maul-github-overdubstudio.secret.yaml` | The Secret `maul-github-overdubstudio`: the App's ID and **private key**. Mode 0600. GitHub keeps no copy of the key.                                                               | Create it in the cluster (command 2), which also deletes the file.                       |
| `maul-github-overdubstudio.forge.yaml`  | No secret. The Forge `overdubstudio` covering the organization's repositories with that Secret, and a Role and RoleBinding that let intent-controller read exactly that one Secret. | Apply it (command 2), and **keep it**: commit it to git beside your other cluster files. |

**Second click on GitHub.** Open the install link from step 1 of the `next:` block,
`https://github.com/apps/maul-overdubstudio/installations/new`. Choose the organization, not your own account, then
**Only select repositories**, and tick the intent repository and every application repository. Click **Install**. An App
installed on all repositories could act on all of them; select only the ones maul works in.

### 2. Give the cluster the credential and the Forge

Paste step 2 of the `next:` block:

```sh
kubectl create -f maul-github-overdubstudio.secret.yaml && rm maul-github-overdubstudio.secret.yaml && kubectl apply -f maul-github-overdubstudio.forge.yaml
```

`kubectl create` never replaces a Secret that already exists: if it refuses, nothing after it runs and the file is kept.
The Secret file is deleted as soon as the cluster holds the Secret; the forge file is kept.

### 3. Write the Project

```sh
maul setup project overdub --intent-repo overdubstudio/intents --repo overdubstudio/app --approver alice -n maul
```

This command talks to neither the cluster nor GitHub. It holds the name, repositories, approvers and labels to the
Project's own rules, so a file it writes is one the cluster accepts, and prints:

```text
maul: wrote Project maul/overdub to overdub.project.yaml (trigger label maul:overdub, approve label maul:approved; intent-controller creates both)
maul: next:
  1. The App must be installed on every repository listed. Add any it cannot reach at:
     https://github.com/organizations/overdubstudio/settings/installations
     (an account that is a person's, not an organization's: https://github.com/settings/installations)
  2. Each approver (alice) needs write access to https://github.com/overdubstudio/intents
  3. kubectl apply -f overdub.project.yaml
  4. GH_TOKEN=$(gh auth token) maul check project overdub -n maul
maul: no Forge for overdubstudio yet? run maul setup github-app --org overdubstudio --intents -n maul first
```

A Project name is at most 25 characters (its intents are named `overdub-<issue>`). Each `--repo` gets a short key, the
repository's name lowercased; a name that is not a valid key (`acme/Shop.Web`, say) is refused with the fix, so write it
as `shop-web=acme/Shop.Web`. Repeat `--repo` for up to eight repositories (several need multi-repository intents on; see
[Several repositories in one Project](deploying.md#several-repositories-in-one-project)) and `--approver` for each
approver.

Each approver needs **write** access to the intent repository (Write, not Triage): a label or command from anyone
without it is refused. Grant it if they lack it:

```sh
gh api -X PUT repos/overdubstudio/intents/collaborators/alice -f permission=push
```

### 4. Apply the Project

```sh
kubectl apply -f overdub.project.yaml
```

Keep `overdub.project.yaml` too: it holds no secret.

### 5. Check it

```sh
GH_TOKEN=$(gh auth token) maul check project overdub -n maul
```

One line per check, each `PASS`, `FAIL` or `SKIP`, and every `FAIL` names its fix ([table below](#when-a-check-fails)).
intent-controller validates a new Project within about a minute: if `ready` says it has not reported yet, run the check
again. The command reads private repositories with your GitHub token; without `GH_TOKEN` it reads anonymously and
`agent-image` is a `SKIP`.

When `ready` passes, open an issue in `overdubstudio/intents` and add the label `maul:overdub`. maul posts a plan on the
issue; an approver adds `maul:approved` or comments `/maul approve`, and the pull request follows.

## Add a Project to an organization you already set up

The organization's App and Forge already exist, so start at command 3:

```sh
maul setup project web --intent-repo overdubstudio/intents --repo overdubstudio/web --approver alice -n maul
kubectl apply -f web.project.yaml
GH_TOKEN=$(gh auth token) maul check project web -n maul
```

**One click on GitHub, only if the repository is new to the App.** If `overdubstudio/web` was not selected when the App
was installed, add it at the installations page the command prints,
`https://github.com/organizations/overdubstudio/settings/installations` (**Configure**, then **Repository access**).
Until then `ready` fails with `AppNotInstalled`.

## Change a Project

To add a repository or an approver, run the same `maul setup project` command again with **every** flag, the new ones
included, and `--force`, then apply and check:

```sh
maul setup project overdub --intent-repo overdubstudio/intents --repo overdubstudio/app --repo overdubstudio/api \
  --approver alice --approver bob -n maul --force
kubectl apply -f overdub.project.yaml
GH_TOKEN=$(gh auth token) maul check project overdub -n maul
```

`--force` writes the file anew from the flags. If you edited the file by hand since (a `checks` block, a `preview`, an
`agentResourceClass`), a re-run drops those edits: edit the file instead, and apply it the same way. The fields a
Project takes are in [intent-controller](../configuration/intent-controller.md) and
[Onboarding an application](onboarding-app.md#the-project).

## Good to know

- **You must be an Owner of the organization** to create its App. Otherwise GitHub creates nothing, or creates the App
  under your own account, which maul then refuses.
- **There is no label step.** intent-controller creates the trigger label (`maul:<project>`) and the approve label
  (`maul:approved`) on the intent repository itself, once the App is installed there.
- **Keep the `.forge.yaml` and `.project.yaml` files in git.** They hold no secret, and they are what a rebuilt cluster
  re-applies. The Secret file is deleted once it is applied; the private key then exists only in the cluster. If the
  cluster is lost, generate a new private key on the App's settings page
  (`https://github.com/organizations/<org>/settings/apps/<app>`) and create the Secret again with the same name and
  keys.
- **An application repository with no agent image can be planned but not built.** A build runs in the application's own
  toolchain image, which the repository declares in `.maul/agent.yaml`. `maul check project` fails `agent-image` until
  it exists. [Onboarding an application](onboarding-app.md) sets it up; for a repository that already has code, run
  `maul init app --existing` in it.
- **A personal account** works too: `maul setup github-app --user --intents` creates the App under your own account,
  with the Secret named `maul-github`.

## When a check fails

| `maul check project` reports                                                                                                                                                   | Fix                                                                                                                                                                                                                                                                                 |
| ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `ready`: `intent-controller has not reported on this Project: ...`, or `Ready describes generation ...`                                                                        | Wait a minute and run the check again. If it stays, intent-controller is not running in that namespace (`kubectl -n maul get deploy maul-intent-controller`).                                                                                                                       |
| `ready`: `ForgeSecretUnreadable: the credential Secret of the Forge covering <repo> does not exist (...)`                                                                      | The Secret was never created, or was created in another namespace. Run command 2 (`kubectl create -f <secret-name>.secret.yaml`). If that file is gone too, generate a new private key on the App's settings page and create the Secret by hand with keys `appID` and `privateKey`. |
| `ready`: `ForgeSecretUnreadable: intent-controller may not read the credential Secret of the Forge covering <repo> (...)`                                                      | Apply the forge file (`kubectl apply -f <secret-name>.forge.yaml`). If it is applied, the error names the ServiceAccount the cluster refused: when that is not the one in the forge file's RoleBinding, your release is not named `maul` ([below](#a-release-not-named-maul)).      |
| `ready`: `AppNotInstalled: the App cannot act on <repo> with ...; install or extend the App's installation: https://github.com/organizations/<org>/settings/installations ...` | Add the repository to the App's installation at that address (**Configure**, then **Repository access**). The same fix applies to `the label ... cannot be created on <repo>`.                                                                                                      |
| `forge`: `no forge matches repository: <url>; ...; create one: maul setup github-app --org <owner> --intents -n maul`                                                          | No Forge covers that organization: run command 1 for it, or apply its forge file if you already did.                                                                                                                                                                                |
| `forge`: `ambiguous forge match: <url> matched by <a>, <b>; ...` (and `ready`: `ForgeUnresolved`)                                                                              | Two Forges list the same organization, typically an older one from the `maul-config` chart beside the forge file's. Keep one: delete the other (`kubectl -n maul delete forge <name>`, or remove it from the `maul-config` values and upgrade that release).                        |
| `agent-image`: `... declares no agent image: ...; scaffold one in that repository: maul init app --existing --registry <registry> (docs/intents/onboarding-app.md)`            | Follow [Onboarding an application](onboarding-app.md): `maul init app --existing` in the repository, then publish its agent image. Plans run without it; builds wait for it.                                                                                                        |

## A release not named maul

The forge file's RoleBinding lets the ServiceAccount `maul-intent-controller` read the Secret: intent-controller's name
in a Helm release named `maul` and in the kustomize component. A Helm release named otherwise names it after the
release: `platform-maul-intent-controller` for a release `platform`, `maul-prod-intent-controller` for `maul-prod`.
`kubectl -n maul get serviceaccounts` lists it.

- **Before command 1**, pass it: `--intent-controller-sa platform-maul-intent-controller`.
- **After command 1**, change the RoleBinding's `subjects[0].name` in the forge file and apply it again. Do not run
  command 1 again: another run creates another App.

## GitOps and the maul-config chart

The files are plain Kubernetes resources: commit them and apply them with whatever applies the rest of your cluster
(Argo CD, Flux, `kubectl apply -k`). Kustomize installs use them unchanged.

If you keep Forges and Projects in the `maul-config` chart's values instead, paste each file's `spec` into its `forges`
or `projects` list, and never also apply the file under the same name: the next `helm upgrade` then fails with
`exists and cannot be imported`. The Role and RoleBinding have no place in those values; apply them from the forge file,
or list the Secret in the `maul` chart's `intentController.forgeSecrets`.
