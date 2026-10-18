# The patchy CLI

`patchy` works with the pipeline's custom resources from a terminal: list findings, read what an agent concluded, and
approve or suspend work.

It talks to the Kubernetes API with **your** kubeconfig — never through a controller, and never through the status
server. There is no patchy-specific auth, no separate endpoint to expose, and no service account acting on your behalf:
what you can do is exactly what your RBAC allows.

This page is the tour. For every command, flag and default, generated from the binary itself, see the
[command reference](cli/patchy.md).

## Install

The supported install is the `patchy-cli` archive attached to each release of
[`devthenet-labs/patchy`](https://github.com/devthenet-labs/patchy/releases), for linux, macOS and windows on amd64 and
arm64. Install the CLI of the release your cluster runs: `init app` and `check image --run` pin images to the CLI's own
release, and `init app` the terraform module too. [Deploying intents and previews](intents/deploying.md) needs 0.12.16
or later; the newest release is `gh release view --repo devthenet-labs/patchy --json tagName --jq .tagName`.

```sh
version=X.Y.Z        # the release you deploy, without the v: 0.12.16 or later
os=darwin arch=arm64 # linux | darwin, amd64 | arm64 (windows: see below)
gh release download "v$version" --repo devthenet-labs/patchy \
  --pattern "patchy-cli_${version}_${os}_${arch}.tar.gz" \
  --pattern "patchy_${os}_${arch}.sigstore.json" \
  --pattern "kubectl-patchy_${os}_${arch}.sigstore.json" \
  --pattern checksums.txt
shasum -a 256 --check --ignore-missing checksums.txt # sha256sum on linux
tar -xzf "patchy-cli_${version}_${os}_${arch}.tar.gz"
```

The archive holds `patchy`, `kubectl-patchy`, the `LICENSE` and the shell completions under `completions/`. Each binary
is signed with a keyless cosign signature, made by the release workflow from the release's own commit, and the
`.sigstore.json` bundles cover the **extracted binaries**, not the archive. `checksums.txt` is not signed, so it only
catches a damaged download; the signature is what proves where a binary came from. Verify both binaries before you
install them, with cosign v3 (the bundles are Sigstore's v0.3 bundle format):

```sh
sha=$(gh api "repos/devthenet-labs/patchy/commits/v$version" --jq .sha)
for bin in patchy kubectl-patchy; do
  cosign verify-blob --bundle "${bin}_${os}_${arch}.sigstore.json" \
    --certificate-identity-regexp '^https://github.com/bitwise-media-group/github-workflows/\.github/workflows/release\.yaml@' \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com \
    --certificate-github-workflow-repository devthenet-labs/patchy \
    --certificate-github-workflow-sha "$sha" \
    "$bin"
done
install -m 0755 patchy kubectl-patchy /usr/local/bin/
patchy --version
```

The identity is the reusable release workflow in `bitwise-media-group/github-workflows`, which signs on behalf of the
repository it runs for; `--certificate-github-workflow-repository` and `--certificate-github-workflow-sha` pin that to
`devthenet-labs/patchy` at the release's commit. Each prints `Verified OK`, and `patchy --version` then names the
release (`patchy version 0.12.16 (...)`).

On Windows (`os=windows`, in a POSIX shell such as Git Bash) the archive is the same `.tar.gz`, but the binaries in it
are `patchy.exe` and `kubectl-patchy.exe`: the bundles keep their names (`patchy_windows_amd64.sigstore.json`), so pass
`"$bin.exe"` as the last argument of `cosign verify-blob`, and put the two `.exe` files on your `PATH` instead of the
`install` line.

!!! warning "Not Homebrew, not `go install`"

    The Homebrew tap `bitwise-media-group/tap/patchy` is the upstream project's, an older release without `setup`,
    `init` or `check project`. `go install` builds an unstamped development build: it knows no release registry, so
    `init app` needs `--agent-base` and `check image --run` needs `--runner-image`, and neither can pin anything to a
    release. To build from source, clone this repository and run `BUILD_TAGS= PATCHY_IMAGE_REGISTRY=ghcr.io/devthenet-labs/patchy
    hack/build.sh` (it writes `bin/patchy` beside the other binaries), which stamps the registry; the empty
    `BUILD_TAGS` leaves out the status page's web UI, which a fresh clone has not built. Built anywhere but at a
    release tag, it is still a development build to `init app`.

The archive carries `kubectl-patchy`, so every command below also works as `kubectl patchy …` once it is on your `PATH`:

```sh
kubectl patchy get findings
```

## Shell completion

Completion covers verbs, nouns, and the enumerated flag values (phases, severities, output formats). Install what your
shell reads — the scripts ship pre-generated in the archive under `completions/`:

```sh
install -m 0644 completions/patchy.zsh "${fpath[1]}/_patchy"                     # zsh
install -m 0644 completions/patchy.bash /usr/local/etc/bash_completion.d/patchy  # bash
install -m 0644 completions/patchy.fish ~/.config/fish/completions/patchy.fish   # fish
```

`patchy completion <shell>` prints the same script, and adds `powershell`.

`kubectl patchy …` completes through a different mechanism, and needs one more file. kubectl never reads a plugin's own
completion script: it strips its own global flags, then looks for an executable named `kubectl_complete-<plugin>` on
your `PATH` and asks that for candidates. So the hook belongs in `bin`, not in a completion directory:

```sh
install -m 0755 completions/kubectl_complete-patchy /usr/local/bin/kubectl_complete-patchy
```

It is a one-line forward to `kubectl-patchy __complete`, so both spellings complete from the same command tree and
cannot drift. Requires kubectl 1.26 or newer.

## Grammar

```text
patchy <verb> <noun> [name...] [flags]
```

Verbs and nouns are separate axes: every verb accepts any noun it makes sense for, so learning one verb teaches you all
of them. Nouns take the same short names the CRDs declare, which means `patchy get fnd` and `kubectl get fnd` always
mean the same thing.

| Noun            | Also accepts                                |
| --------------- | ------------------------------------------- |
| `finding`       | `findings`, `fnd`                           |
| `investigation` | `investigations`, `inv`                     |
| `remediation`   | `remediations`, `rem`                       |
| `findingrollup` | `findingrollups`, `fr`, `rollup`, `rollups` |
| `repository`    | `repositories`, `repo`, `repos`             |
| `intent`        | `intents`                                   |
| `intentrun`     | `intentruns`, `irun`                        |
| `integration`   | `integrations`                              |
| `forge`         | `forges`                                    |
| `project`       | `projects`, `proj`                          |

`get` takes one more: `all`, meaning every kind in the table above. It is the CLI's spelling of `kubectl get patchy`
(every CRD declares the `patchy` category), and no other verb accepts it — there is nothing sensible to describe, review
or approve collectively.

`get` also takes `usage`, which is not a kind but the usage report: metered tokens and cost per person and per Project,
read from the `UsageRollup`s (see [Reading usage](#reading-usage)).

The intent nouns (`intent`, `intentrun`, `project`) are for reading only: intents are driven from their GitHub issue
(labels and `/patchy` commands, see [intent-controller](configuration/intent-controller.md)), and no CLI verb acts on
them.

## Global flags

```text
    --kubeconfig string    path to the kubeconfig file (default: $KUBECONFIG, then ~/.kube/config)
    --context string       kubeconfig context to use
-n, --namespace string     namespace to work in (default: the context's, exactly as kubectl resolves it)
-A, --all-namespaces       work across every namespace
-o, --output string        table | wide | json | yaml | name | markdown   (default table)
    --no-color             disable colour and styling
    --request-timeout dur  timeout for a single API call (default 30s)
-v, --verbose              log what the CLI is doing to stderr
```

Namespace resolution follows kubectl's rules exactly, including its fallback to `default`, so the two tools never
disagree about where they are looking. If your findings live in `patchy`, either set that on your context or pass
`-n patchy`.

## Reading

```sh
patchy get findings
patchy get findings -o wide                      # adds the issue and pull-request links
patchy get findings --phase AwaitingApproval
patchy get findings --severity critical,high --sort-by severity
patchy get findings --awaiting                   # only findings you could act on right now
patchy get findings --repo billing --suspended
patchy get investigations --finding my-finding
```

Columns come from the CRDs' own print columns, so they match `kubectl get` and always will; `-o wide` adds the ones the
CRDs mark lower priority. Filters that map to labels (`--severity`, `--source`, `--finding`, `-l`) run on the API
server; the rest (`--phase`, `--verdict`, `--repo`, `--suspended`, `--awaiting`) need the object and run locally.

### Reading usage

```sh
patchy get usage                                  # everyone, the last 30 days
patchy get usage --since 90d                      # or 720h, or a first day: 2026-09-01 (at most 400 days)
patchy get usage --user octocat                   # one person: a GitHub login, github-id:<id>, or org
patchy get usage --project storefront
patchy get usage --user octocat --project storefront -o json
```

The report is the one the status page's Usage view shows (`-o json` is the same document as its API), built from the
`UsageRollup`s that attribution writes ([attribution](configuration/attribution.md#usage-rollups)). It is an admin view:
the CLI first asks for `get` on the virtual `usage/all` and stops, exit 4, without it; the rollups are then read with
your own kubeconfig, so RBAC on `usagerollups` applies too. A person's own usage is on the status page. `COST` is
metered tokens at list rates, `EQUIVALENT` what a Claude subscription's runs would have cost at those rates (never money
spent), `UNMETERED` the broker's worst-case shortfall (never priced) and `REPORTED` the runs with only their pod's own,
untrusted figure, which no total includes. A login two GitHub accounts have held is refused: name one as
`github-id:<id>`.

`patchy get all` is the whole pipeline in one screen — every kind, each in its own table, in the order the pipeline uses
them:

```text
Findings
NAME    REPO      SEVERITY   PRIORITY   PHASE    VERDICT     AGE
fnd-1   billing   high       high       Queued   remediate   4h

Investigations
NAME          FINDING   ATTEMPT   STATE      VERDICT     AGE
fnd-1-inv-1   fnd-1     1         Complete   remediate   3h

Forges
NAME   PROVIDER   READY   AGE
gh     github     True    9d
```

Kinds with nothing in them are left out rather than printed as an empty table. `all` lists, and only lists: it takes no
names, and it refuses the finding-only filters (`--phase`, `--verdict`, `--repo`, `--suspended`, `--awaiting`,
`--finding`) rather than narrow one table and leave the rest whole. The label filters (`-l`, `--severity`, `--source`)
mean the same thing on every kind, so those do apply. `-o json` and `-o yaml` give you one stream of every object;
`-o name` gives you fully-qualified references you can pipe back into `kubectl`.

```sh
patchy describe finding my-finding               # state, timeline, owners, alerts, runs, spend, runner image
patchy describe investigation my-finding-inv-1
patchy describe repository my-finding-src        # pinned commit, artifact, runner image
```

With [repository-declared agent images](integrations/agent-images.md) on, a finding and its repository snapshot also
show the runner image: the file that declared it, the digest it was pinned to, whether the runs used it (`repository`)
or the default runner image (`default`), and the reason a declaration was rejected or not applicable.

## Reviewing an agent's work

```sh
patchy review finding my-finding                     # both stages together
patchy review investigation --finding my-finding     # the latest attempt
patchy review investigation --finding my-finding --attempt 2
patchy review remediation my-finding-rem-1
```

On a terminal the report is rendered for reading. Piped, or with `-o markdown`, you get the markdown the agent actually
wrote — so pasting it into a ticket loses nothing:

```sh
patchy review finding my-finding -o markdown > report.md
```

`--raw` keeps the machine frontmatter, which is a contract between the investigate and remediate stages and is stripped
by default.

## Opening the human-facing page

```sh
patchy browse finding my-finding          # the tracking issue
patchy browse remediation my-finding-rem-1 # the pull request
patchy browse finding my-finding --print-url
```

The verb is `browse` rather than `open` because `Opened` is a real phase — `patchy open finding` would read like a state
transition. `patchy review … --web` is the same behaviour without leaving the review.

## Acting on a finding

```sh
patchy approve  finding my-finding [--note "shipping despite the break"]
patchy suspend  finding my-finding
patchy resume   finding my-finding
patchy retry    finding my-finding
patchy expedite finding my-finding
```

Every action writes to the finding's **spec only**. A controller observes the change and moves the phase — the CLI never
writes status and never transitions a finding itself, which is what keeps each phase edge single-writer.

Actions are idempotent. Approving an already-approved finding succeeds and changes nothing, so re-running a script is
safe:

```sh
patchy suspend finding my-finding --dry-run                              # report, write nothing
patchy suspend finding -l patchy.bitwisemedia.uk/severity=critical -y    # bulk, no prompt
```

Bulk operations prompt above one finding unless you pass `-y`, report each finding individually, and exit non-zero if
any failed — one unavailable finding never stops the rest.

## Backfilling an integration

One action acts on an Integration rather than a finding: the
[manual backfill](integrations/sources/github.md#the-manual-backfill), which lists the provider's open alerts and
ingests the ones that predate webhook coverage.

```sh
patchy backfill gh                                      # the credential's full scope
patchy backfill gh --repo acme/                         # one owner
patchy backfill gh --repo acme/shop --repo acme/billing # exact repositories (required with a PAT)
```

Like every action it writes spec only (`spec.backfill`); the integration-controller runs the walk on its next reconcile
and reports on `status.backfill` — watch for `truncated`, which means the page budget ran out and a narrower `--repo`
prefix is needed to reach the rest.

## Testing a generic integration

The `dev` commands are the one part of the CLI that never touches a cluster: a local test harness for authors of
[generic integrations](integrations/sources/generic.md).

```sh
patchy dev generic --secret dev-secret --enhance-url http://127.0.0.1:9000/enhance
```

hosts the real generic webhook receiver on your workstation — the same HMAC authentication, deduplication, and
validation the integration-controller runs — retains ingested findings in memory, and drives the enhancer call and
resolver write-back at your endpoints, so one signed POST tests every exchange of the contract. For a process that is
only an enhancer or resolver, `patchy dev enhance` / `patchy dev resolve` fire a single signed exchange from a findings
payload file, no server involved.

Uniquely in the CLI, every `dev` flag also resolves from `PATCHY_DEV_*` environment variables and an optional
`.patchy.yaml` in the working directory (flag beats environment beats file). See the
[generic integration guide](integrations/sources/generic.md#testing-your-integration-locally) for the full walkthrough.

## Maintaining a chart and image mirror

The `mirror` commands are the other cluster-free group: they maintain a vendored mirror store — a git repository where
`mirror.yaml` holds global defaults and every `charts/<name>/` or `artifacts/<name>/` directory pins one upstream helm
chart or OCI artifact, vendored for PR review, digest-locked, provenance-verified, scanned, and published signed to one
or more platform registries. `mirror.yaml` lists the registries; every entry publishes to all of them, each signed with
that registry's own `signing` block when it has one (a KMS key, say) and the global default otherwise.

```sh
patchy mirror upgrade --check -o json   # what would move, as data
patchy mirror upgrade --all             # move pins, regenerate everything derived
patchy mirror validate --all            # the CI gate: current, verified, clean
patchy mirror sync --all                # converge every registry, idempotently
patchy mirror sync --all --registry ghcr   # just the named registry
```

Signing, verification, and image scanning shell out to external binaries rather than linking them: `sync` and `validate`
need `cosign` (v3 or later) on `PATH`, and the image scanners are all opt-in shell-outs — enable `scan.scanners.osv`
(`osv-scanner` v2) or `scan.scanners.grype` (`grype`) in `mirror.yaml`, since an image scan with neither enabled is an
error; `scan.enabled: false` turns image scanning off deliberately. `kubescape` remains the optional configuration
scanner. Existing stores that relied on the old built-in default should add `scan.scanners.osv.enabled: true` and
install `osv-scanner` (mise/brew).

`upgrade` mutates files and nothing else — patchy never runs git, so branches, commits, and pull requests belong to the
calling pipeline. Entries that share a `lockstep` group bump together, holding at the lowest version every member has
published. `sync` never replaces an existing chart tag and skips anything already current and signed, so re-runs are
safe. `validate` regenerates the derived state out-of-tree and byte-compares it, which is why upgrade is the only verb
allowed to consult the wall clock (tracked-tag cooldowns, allowlist expiry stamping).

Like `dev`, the `mirror` flags also resolve from `PATCHY_MIRROR_*` environment variables and `.patchy.yaml` (`mirror:`
block); `-C` points at a store checkout from anywhere.

## Checking an agent image

`patchy check image` is the third cluster-free command, for repository owners about to declare an
[agent image](integrations/agent-images.md) in `.patchy/agent.yaml` or `.devcontainer/devcontainer.json`. It runs
source-controller's own checks, through the same code, and reports every verdict rather than the first failure:

```sh
patchy check image ghcr.io/acme/shop-agent:1                                   # platform, size, VOLUME, ENV, PATH
patchy check image ghcr.io/acme/shop-agent:1 --allow ghcr.io/acme/ --cosign-key cosign.pub
patchy check image ghcr.io/acme/shop-agent:1 --run                             # plus the agent's preflight in docker
```

`--allow` stands in for the operator's registry allowlist and `--cosign-key` for their signing key; without the key the
signature line is skipped, because source-controller admits an unverified image only when the operator allows unsigned
images. Registry credentials are your own docker credentials. `--run` needs a local docker: it copies `agent-runner` and
the claude CLI out of the claude runner image released with this CLI (for a development build, which has none, the
newest `vX.Y.Z` release in the registry, never `latest`), pinned to the digest its tag names in the registry so a stale
local copy of the tag never stands in for it, and reports the image and digest on a `runner-image` line of their own.
The registry is the one the CLI's release published its images to, stamped in when it was built; a plain `go build`
stamps none (`hack/build.sh` does when `PATCHY_IMAGE_REGISTRY` is set). `--runner-image` overrides the choice and is
used as given; when no image can be chosen, because the registry is unreachable or the CLI knows none, the line fails
and says to pass one. It then runs the image the way the agent pod does, with uid 65532, a read-only root filesystem, no
network, no capabilities and bounded processes, memory and CPU, then runs the preflight a stage runs before its first
model call, followed by `bash -c true` and `git --version`. A pod may land on a node of any platform the image serves,
so this runs once per platform: the docker host's own natively, any other under docker's emulation (Docker Desktop has
it; on Linux, binfmt_misc with QEMU), and a platform the host cannot emulate is reported as SKIP rather than passed
over. Each check prints one line (PASS, FAIL or SKIP, the platform for a `--run` check, then the reason); `-o json`
prints the report as data, and the exit status is non-zero when any check fails.

`--deny` stands in for the registry paths the operator carves out of the allowlist
(`--repository-image-denied-registries`). The chart denies its preview image prefix there, so an image under it fails
the allowlist line even inside an allowed path, as it would in source-controller. It needs `--allow`.

`check image` takes an image reference, never a repository. To check what a repository declares before any Project
points at it, read the reference out of its `.patchy/agent.yaml`, from a checkout or from GitHub (once a Project exists,
[`check project`](#checking-a-project) does this for you):

```sh
image=$(awk '$1 == "image:" {print $2}' .patchy/agent.yaml)
# or, without a checkout:
image=$(gh api -H 'Accept: application/vnd.github.raw' repos/acme/Shop.Web/contents/.patchy/agent.yaml \
  | awk '$1 == "image:" {print $2}')
patchy check image "$image" --allow 123456789012.dkr.ecr.us-west-2.amazonaws.com/patchy/app-envs/ --run
```

For ECR, your docker credentials means one of two things. Either the ECR credential helper,
`docker-credential-ecr-login` (from `amazon-ecr-credential-helper`), named for the registry in `~/.docker/config.json`;
it reads the AWS SDK's default chain, so `AWS_PROFILE` selects the account:

```json
{ "credHelpers": { "123456789012.dkr.ecr.us-west-2.amazonaws.com": "ecr-login" } }
```

Or a login, which lasts twelve hours:

```sh
aws ecr get-login-password --region us-west-2 \
  | docker login --username AWS --password-stdin 123456789012.dkr.ecr.us-west-2.amazonaws.com
```

`--run` pulls the image with your local docker, so it needs the same. (`check project`, by contrast, reads ECR through
the AWS SDK directly.)

## Creating the GitHub App

`patchy setup github-app` is the fourth cluster-free command. It creates the GitHub App patchy authenticates as through
GitHub's
[App manifest flow](https://docs.github.com/en/apps/sharing-github-apps/registering-a-github-app-from-a-manifest), and
writes its credentials as the Secret manifest a `Forge` and an `Integration` read
([what the App needs, and why](getting-started/github-app.md)):

```sh
patchy setup github-app --org acme --intents --checks                    # intents, with check-fix rounds
patchy setup github-app --org acme --intents --checks --rerun-failed     # and re-runs of failed Actions checks
patchy setup github-app --org acme --security --webhook-url https://patchy.acme.dev/github/webhooks
patchy setup github-app --org acme --intents --dry-run                   # print the manifest; create nothing
patchy setup github-app --org acme --intents -o - | sops --encrypt --input-type yaml --output-type yaml /dev/stdin
```

Choose what the App is for. It asks for the least each chosen feature uses, and nothing else:

| Flag                               | Permissions                                                         | Webhook events                                                   |
| ---------------------------------- | ------------------------------------------------------------------- | ---------------------------------------------------------------- |
| `--security`                       | Code scanning alerts, Issues, Contents, Pull requests: read & write | `code_scanning_alert`, `issues`, `issue_comment`, `pull_request` |
| `--intents`                        | Issues, Contents, Pull requests: read & write                       | none: intent-controller polls GitHub                             |
| `--checks` (with `--intents`)      | Checks, Commit statuses, Actions: read                              | none                                                             |
| `--rerun-failed` (with `--checks`) | Actions: read & write                                               | none                                                             |

Metadata read comes with every App. The table is `internal/intentperm`, the one intent-controller proves a `Project`'s
grants against before it is Ready, so an App created with `--intents` (and `--checks` for a Project with
`spec.checks.fix`, `--rerun-failed` for one with `spec.checks.rerunFailed`) passes that check once it is installed.
`--rerun-failed` is its own switch so that check-fix rounds alone never widen the App to a write on a repository's CI.
`--security` needs `--webhook-url`, the integration-controller's `https://<host>/github/webhooks`; an App without it has
no webhook.

The flow: patchy listens on a random `127.0.0.1` port and opens a page in your browser that posts the manifest to
GitHub's "create a GitHub App" form, for `--org` (you must be an owner of it) or, with `--user`, your own account. Check
the form and click **Create GitHub App**. GitHub sends the browser back with a one-time code, which patchy accepts only
with the random state it started the flow with; it exchanges the code for the App's ID, private key and webhook secret
(`POST /app-manifests/{code}/conversions`, which needs no credential) and stops listening. The page carries that state,
so it is served once: if your browser says it was served already, something else on the machine read it first, so stop
patchy and run it again. A code for an App that an account other than `--org` owns is refused, and nothing is written.
On a machine without a browser, `--no-browser` writes the page to a file you open anywhere, and you paste back the
address GitHub sends you to (it asks again until it gets one from this run, and after a bare code GitHub does not
accept). A code works once, within an hour; if the run ends without one, the error says where to delete the App or how
to finish it.

The Secret (`appID`, `privateKey`, and `webhookSecret` for an App with a webhook; `--secret-name`, default
`patchy-github`, in `-n`, default `patchy`) is written to `<secret-name>.secret.yaml` with mode 0600, and an existing
file is never replaced without `--force`; a file patchy could not write, in a directory it cannot write to, is refused
before anything is created. `-o -` writes it to stdout for a pipe, and refuses a stdout that is a terminal or a file
other users can read, as a shell's `> file` is under the usual umask: name the file with `-o <file>` instead. The
private key appears nowhere else, and GitHub keeps no copy: apply or encrypt the file, then delete it. Finally, install
the App with the link printed on stderr, on the repositories patchy works on: for intents, the intent repository and
every application repository. Only github.com is supported, and nothing here talks to a cluster.

## Checking a Project

`patchy check project <name>` is the preflight for a [Project](configuration/intent-controller.md) before its first
intent. Unlike `check image` it reads the cluster, with your kubeconfig, and it reports the same way: one line per
check, PASS, FAIL or SKIP, then what the check is about (a repository's key, `intent-repository`, or `-` for the Project
as a whole) and the reason. It looks at every repository the Project lists, and every previewed one, never just the
first:

```sh
patchy check project shop -n patchy
GH_TOKEN=$(gh auth token) AWS_PROFILE=prod patchy check project shop -n patchy
patchy check project shop -o json | jq '.checks[] | select(.status == "FAIL")'
```

| Check           | What it reads, and what it says                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| --------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `ready`         | The Project's `Ready` condition, with its reason and message: intent-controller's own verdict, which it reaches by minting the App's scoped tokens in-cluster. A condition older than the spec fails.                                                                                                                                                                                                                                                                                                                |
| `intent-names`  | The `IntentNameConflict` condition.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| `forge`         | Each repository, the intent repository included, resolved over the namespace's Forge CRs by the controllers' own `forge.Resolve`, and that Forge's `Ready` condition.                                                                                                                                                                                                                                                                                                                                                |
| `labels`        | The trigger and approve labels, from `Ready`: intent-controller ensures them last, so they exist exactly when it is True.                                                                                                                                                                                                                                                                                                                                                                                            |
| `agent-image`   | Each repository's `.patchy/agent.yaml` (or `.devcontainer/devcontainer.json`) at its default-branch head, judged by `check image`'s checks under source-controller's live allowlist, size cap and signing key. Only a regular file counts, as for source-controller: a symlink is not a declaration. When source-controller requires a signature but its key is not in a ConfigMap labelled for it (the chart's layout), an image that passes everything else is a SKIP, and `check image --cosign-key` verifies it. |
| `previews`      | That intent-controller writes Previews and preview-controller is configured, from their ConfigMaps.                                                                                                                                                                                                                                                                                                                                                                                                                  |
| `preview-image` | Each previewed repository's image repository: under preview-controller's prefix, one valid leaf, reachable, and holding `sha-<default-branch head>`, the tag a preview runs for a component an intent leaves be. With one previewed repository no preview runs that tag (only the pull request's head), so its absence is a SKIP.                                                                                                                                                                                    |
| `preview-dns`   | That `<project>-0.<host suffix>` resolves to the preview load balancer, the placeholder Ingress's address. No intent is numbered 0, so the wildcard covers that name and no Preview owns it. It fails when the name does not exist (NXDOMAIN) or resolves elsewhere, and when the placeholder Ingress has no load balancer address; a lookup that times out, or a placeholder you cannot read, is a SKIP.                                                                                                            |
| `preview-tls`   | That the name serves a certificate trusted for it. The preview load balancer admits only `preview.inboundCIDRs` (behind the gateway, its edge may admit only `previewAuth.gateway.inboundCIDRs`), so from any other address the handshake times out, and that is a SKIP, not a FAIL.                                                                                                                                                                                                                                 |

While preview sign-in is on, `preview-auth`, `preview-auth-dex` and `preview-auth-host` check the relay, Dex's redirect
URI and each preview host's answer without credentials ([Preview sign-in](intents/preview-sign-in.md#check-it)). With
the [preview gateway](configuration/preview-gateway.md#checking-it) on, `preview-dns` and `preview-auth-host` judge the
gateway's path instead (its edge's address, its `404` page for a host no Preview owns, its redirect to `/start`), and
`preview-gateway-allowlist` checks that a spoofed `X-Forwarded-For` cannot get past `previewAuth.gateway.inboundCIDRs`.

The check never reads a Secret, so it never holds the App's private key: whether the App is installed with the
permissions intents use, and whether the labels exist, come from the conditions the controller computed in the cluster.
What only your own identity can see, it reads with that identity and says so: GitHub with `GH_TOKEN` (else
`GITHUB_TOKEN`, else anonymously, which sees public repositories only), a GitHub Enterprise Server repository with
`GH_ENTERPRISE_TOKEN` (else `GITHUB_ENTERPRISE_TOKEN`) only when `GH_HOST` names its host and anonymously otherwise, and
registries with your cloud and docker credentials (ECR through the AWS SDK's default chain, so `AWS_PROFILE` works). The
Project, which someone else may have written, names the repositories' hosts, so a github.com token never leaves
github.com and an enterprise token never leaves `GH_HOST`. A PASS there means _you_ can read the image; whether
source-controller's and the preview nodes' own credentials can is shown by a Repository's `status.runnerImage` and a
Preview's status. The exit status is 1 when any check fails, 3 when the Project does not exist and 4 when you may not
read it.

## Setting up previews

`patchy setup previews` writes the files that expose [previews](configuration/preview-gateway.md) at
`https://<project>-<issue>.<host suffix>` and sign viewers in to them. It applies nothing and calls no provider API: it
writes patchy's chart values, the manifests the edge needs, the relay's Dex client secret as two Secret manifests,
`answers.yaml` and an ordered `NEXT-STEPS.md` into `-o` (default `./patchy-previews`, created 0700, every file 0600).
[Set up previews](intents/set-up-previews.md) is the walkthrough: what an install needs, and which recipe to pick.

```sh
patchy setup previews                       # asks for every answer on a terminal
patchy setup previews --detect -n patchy    # reads the cluster first to fill in what it can
patchy setup previews --non-interactive --platform k3s --edge tailscale --dns route53 \
  --dns-option region=us-east-1 --host-suffix acme-previews.dev --sign-in-host preview-auth.patchy.acme.dev \
  --dex-issuer https://dex.patchy.acme.dev --viewer-team acme:qa --registry ghcr.io
```

Every answer is a flag. On a terminal the command asks for each one the flags leave open, and each question names the
flag that answers it, so a session can be repeated with `--non-interactive`. Without a terminal, or with
`--non-interactive`, a missing answer is an error naming its flag. A credential is never typed at a prompt, only the
path of the file that holds it (`--tunnel-credentials`, `--dns-credentials-file`).

The edge (`--edge`) picks the recipe: `cloudflare-tunnel` (cloudflared, TLS at Cloudflare, no open port), `gateway-tls`
(the gateway's own load balancer, a cert-manager DNS-01 certificate), `tailscale` (a tailnet device through the
Tailscale operator) or `existing` (an Ingress class or Gateway API parent you already run). The sign-in host and Dex
must sit on a registrable domain other than the host suffix's, because preview code would otherwise run same-site with
sign-in.

**`--detect`** is the only way the command reads a cluster. It uses your kubeconfig, read-only, and never reads a
Secret. It reads the nodes, the `kubernetes` Service and the API server's EndpointSlice, the `kube-dns` Service,
IngressClasses, GatewayClasses, the cert-manager and Tailscale operator CRDs, kube-system's Pod images, the
`kube-system/amazon-vpc-cni` ConfigMap and the release's ConfigMaps in `-n`. A cluster it cannot reach at all is an
error. It prints what it found:

- the platform;
- the API server's and cluster DNS's addresses;
- the edges already installed;
- what enforces NetworkPolicy (the VPC CNI counts only once both its network policy controller and its agent are
  switched on, since it ships with them off);
- which nodes carry the preview label and taint;
- whether the release already serves previews (and whether it still uses the legacy load balancer path), or that it
  could not tell, when the release's ConfigMaps could not be read.

It then fills in only the answers still open, never one a flag gives: an empty answer, `--api-server-cidr auto`, or a
false `--cert-manager-installed` or `--tailscale-operator-installed`, even one an earlier run recorded in
`answers.yaml`. What it fills: the platform, an existing install's host suffix, sign-in host and Dex issuer, the
`kubernetes` Service's `/32` as `--api-server-cidr` on EKS, and `--cert-manager-installed` and
`--tailscale-operator-installed`. The status page's Dex is not read, because its settings live in a Secret.

A re-run starts from `answers.yaml`, and flags given override it. It shows how each file would change and writes a
changed file only with `--force` or `-y`; on a terminal it asks first, and the default is no. The Dex client secret is
read back from the Secret files and never drawn again, unless you pass `--rotate dex-client-secret`. `--dry-run` prints
the files and their differences (never a secret) and writes nothing. The last step in `NEXT-STEPS.md` is
`patchy check previews`.

## Checking the preview install

`patchy check previews -n <release namespace>` is the install-level preflight for
[previews](configuration/preview-gateway.md): it judges what every Project's previews stand on, before any Project
exists, and reports the same way as `check project`, with one more status, WARN, for something that works but is weaker
than it should be. A WARN never fails the run.

```sh
patchy check previews -n patchy
patchy check previews -n patchy -o json | jq '.checks[] | select(.status != "PASS")'
patchy check previews -n patchy --isolation   # also probe slot isolation: creates, then deletes, four Pods
```

| Check                           | What it reads, and what it says                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    |
| ------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `preview-config`                | The controllers' ConfigMaps: previews and sign-in are on and agree on the host suffix, the sign-in host sits outside the suffix, and the suffix has a registrable domain of its own. FAIL when the legacy slot Ingress serves preview hosts without sign-in (`previewAuth.stage: permit`). WARN while the legacy load balancer path still serves preview hosts, and when the suffix shares its registrable domain with the sign-in host, the relay's issuer or Dex.                                                                                                                                                                                                                                                                |
| `preview-dns`, `preview-tls`    | As in `check project`, for `patchy-probe-0.<host suffix>`: the relay's own probe host, which no Preview owns. With the gateway on, a `signInHost` other than `previewAuth.host` gets a line of each too. On a Cloudflare Tunnel edge (the `cloudflare-tunnel` recipe), whose gateway publishes no address, `preview-dns` finds the recipe's cloudflared ConfigMap (never its credentials) and passes a record that is a visible CNAME to that tunnel. A proxied record resolves to Cloudflare's addresses whatever it targets, so it is a SKIP, and `preview-auth-host` proves the routing; another tunnel or another address is a WARN naming the `cloudflared tunnel route dns` command, and a tunnel it cannot identify a SKIP. |
| `preview-auth`, `-dex`, `-host` | As in `check project`, but `preview-auth-host` asks the probe host and every Ready preview, whatever its Project.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| `preview-gateway-allowlist`     | As in `check project`.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             |
| `preview-viewers`               | The RoleBindings in the release namespace and the ClusterRoleBindings of the chart's `<release>-preview-viewer` ClusterRole: at least one User or Group carrying the relay's claim prefix. None is a WARN, not a FAIL: every sign-in is then refused `403` unless another role grants `get` on `projects/previews`, which this cannot see. Subjects the relay can never sign in (a ServiceAccount, a name without the prefix) are named.                                                                                                                                                                                                                                                                                           |
| `preview-nodes`                 | The nodes, with node isolation required: at least one carries preview-controller's node selector and its `<taint key>=true:NoExecute` taint, and none carries the selector without that taint (any Pod could run beside preview code there). A node with the taint but not the selector is a WARN (another install's preview node, or a label typo). With an EKS Auto Mode NodePool, no node yet is a SKIP (it launches them on demand); `preview.nodeIsolation.mode: none` is a WARN.                                                                                                                                                                                                                                             |
| `preview-upstreams`             | Deployments: each one running cert-manager's controller, the Tailscale operator or cloudflared (by its image's repository path, mirrors included) is compared with the version `setup previews` pins, by the image's tag, else its `app.kubernetes.io/version` label. Older is a WARN: it still works, but it is no longer what patchy recommends. No readable version is a SKIP; none of them running reports no line.                                                                                                                                                                                                                                                                                                            |
| `preview-netpol`                | Only with `--isolation`, and the one check that writes to the cluster: see below.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |

The cluster-only checks (`preview-config`, `preview-viewers`, `preview-nodes`, `preview-upstreams`) run first, then the
network ones. The check never reads a Secret, and without `--isolation` it never writes. Its cluster reads are
ConfigMaps (cloudflared's included, in any namespace), the preview placeholder and gateway Ingresses, the gateway's
Service, Previews, RoleBindings, ClusterRoleBindings, Nodes and Deployments; a read your kubeconfig may not make is a
SKIP, not a FAIL. The exit status is 1 when any check fails.

### Proving slot isolation: `--isolation`

`--isolation` adds `preview-netpol`, the Go port of
[`ingress.sh --refused-is-blocked`](https://github.com/devthenet-labs/patchy/blob/main/hack/preview-isolation-probe/README.md#the-ingress-direction-only-the-preview-gateway-reaches-a-slot).
It proves, from inside the cluster, that the slot NetworkPolicy lets only the preview gateway open a connection to a
slot Pod, on any CNI, whether its policy drops a denied connection or refuses it (kube-router, so k3s and k3d). It
creates four short-lived netprobe Pods, in `ingress.sh`'s exact shape, and judges them in a fixed order:

1. A listener in `patchy-preview-<--isolation-slot>`, on the preview nodes (preview-controller's selector and taint).
2. A client in the release namespace carrying the gateway's label runs alone and must **reach** the listener. If it does
   not, the run stops: a refusal could then be a dead listener's.
3. Only then, a client in the agents' namespace and an unlabelled one in the release namespace run, and both must be
   **blocked**. A refused connection counts as blocked here, and only here.
4. The listener must still be the same Running Pod, its container never restarted, so a listener that died in between
   can never pass as a block.

Anything short of that is a FAIL; the slot policy also admitting address ranges (the legacy load balancer's subnets) is
a WARN. Every Pod the run created is deleted at the end, also on failure, timeout or Ctrl-C; one that could not be is
named, to delete by hand. It refuses to start while any of the four Pods exists (a concurrent `ingress.sh` or check).

The image must be under preview-controller's image prefix as `<prefix>netprobe:sha-<40 hex>`, the only shape a slot
admits. By default it is that tag at the CLI's own commit; `--isolation-image` names another. Build it from
`hack/preview-isolation-probe` at the same commit as the CLI and push or import it as its README says: an older image
prints its usage instead of a summary, and the line then says so. `--isolation-timeout` (default `5m`) bounds each Pod's
start and finish. Your kubeconfig needs `create`, `get` and `delete` on Pods in the slot, release and agents namespaces,
`get` on Pod logs there, and `get` on the slot's NetworkPolicy.

## Scaffolding an application repository

`patchy init app` is the fifth cluster-free command. It writes the files an application repository needs before patchy
can build intents in it and preview its pull requests, from templates built into the CLI, and makes no GitHub call:

```sh
patchy init app hello-web --repo acme/Hello.Web --registry 123456789012.dkr.ecr.us-east-1.amazonaws.com
patchy init app --existing --registry 123456789012.dkr.ecr.us-east-1.amazonaws.com   # in a checkout, beside its code
```

| Path                                                                         | What it is                                                                                                                                                      |
| ---------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `.patchy/agent.yaml`                                                         | the [agent image](integrations/agent-images.md) the repository declares: `<registry>/patchy/app-envs/<image-name>:toolchain-v1`                                 |
| `.patchy/Dockerfile`                                                         | that image's recipe: patchy's agent base, pinned by digest, plus the toolchain and the dependencies, offline                                                    |
| `.github/workflows/ci.yml`                                                   | `test`: tests the exact head and builds the runtime image as an artifact, with no credential and no OIDC                                                        |
| `.github/workflows/agent-image.yml`                                          | `agent image`: builds the agent image from the default branch, reproducibly, also uncredentialed                                                                |
| `.github/workflows/publish-images.yml`                                       | the `workflow_run` dispatcher: holds no role and runs no step, and calls each kind's publisher behind its own gate variable                                     |
| `.github/workflows/publish-runtime.yml`                                      | the trusted runtime publisher (`workflow_call`), the only workflow that assumes `RUNTIME_ROLE_ARN`                                                              |
| `.github/workflows/publish-agent.yml`                                        | the trusted agent publisher (`workflow_call`), the only workflow that assumes `AGENT_ROLE_ARN`                                                                  |
| `.github/actions/publish/`                                                   | the publishers' scripts (`guard.cjs`, `validate_oci.py`, `check-config.sh`, `copy-image.sh`), their tests, and a README listing the variables and the workflows |
| `Dockerfile`, `.dockerignore`, `README.md`, `.gitignore`, a small Go service | for a new application only: a runtime image that meets the preview contract (uid 65532, a read-only root filesystem, one port, a readiness path)                |

No generated workflow or script names an account, region, role or repository ID: the publishers read them from
repository variables, and AWS role trust (the repository's numeric IDs, the default branch and the publisher's own
`job_workflow_ref`) is the boundary. Only `.patchy/agent.yaml` names the registry, because source-controller reads the
image from it. The variables are `PUBLISH_REPOSITORY_ID`, `PUBLISH_OWNER_ID`, `AWS_REGION`, `ECR_REGISTRY`,
`AGENT_IMAGE_REPOSITORY`, `AGENT_ROLE_ARN`, `RUNTIME_IMAGE_REPOSITORY` and `RUNTIME_ROLE_ARN`, then the two gates:
`AGENT_PUBLISH_ENABLED` publishes the agent image (a repository without previews still needs it), and
`PREVIEW_PUBLISH_ENABLED`, set last, publishes runtime images.

The next steps `init app` prints on stderr start with the block for patchy's reference terraform module,
`deploy/terraform/aws/modules/app`, pinned to the CLI's own release (`?ref=vX.Y.Z`; a development build says to pin it):
it creates both ECR repositories and both publisher roles, and its `github_variables_dotenv` output holds the
configuration variables, which one command sets:

```sh
terraform output -raw patchy_app_hello_web_variables | gh variable set -f - --repo acme/Hello.Web
# or, with the platform module, from a root output that re-exports its github_variables_dotenv:
terraform output -json github_variables_dotenv | jq -r '."hello-web"' | gh variable set -f - --repo acme/Hello.Web
gh variable set AGENT_PUBLISH_ENABLED --repo acme/Hello.Web --body true
```

(0.12.16's `init app` prints no module block, and a `gh variable set` command per variable instead.)

Set them on the repository, never on the organization: an organization variable reaches every repository that has these
workflows, a template repository and each copy of it included. [Onboarding an application](intents/onboarding-app.md) is
the whole procedure, the gates' order included.

The repository defaults to the checkout's `origin` remote and the default branch to the one `origin`'s HEAD names, both
read from `.git` with no git binary. `--image-name`, the leaf of both registry repositories, defaults to the repository
name made image-safe (`Hello.Web` becomes `hello-web`). The agent base is the one released with this CLI, pinned to the
digest its tag names in the registry now; `--agent-base` overrides it (a digest-pinned reference is used as given), and
a development build, which has no agent base of its own, needs it.

An existing file is never overwritten without `--force`, and a symbolic link never is: every path is checked before any
is written. Nor does `--force` rewrite the agent toolchain: an existing `.patchy/agent.yaml` or `.patchy/Dockerfile` is
the repository's own once written, its tag bumped with every toolchain change, so a re-run that picks up newer
publishers keeps both as they are rather than rolling the declaration back to `toolchain-v1`. A kept
`.patchy/agent.yaml` must still declare a `toolchain-v<N>` tag of the agent repository the options publish to, or
nothing is written; remove both files to generate them again. `--existing` writes only `.patchy/` and the CI publishers,
builds the runtime image in a workflow of its own (`runtime-image.yml`, named `runtime image`) so the application's CI
is untouched, and ends its next steps with what the application must be adapted to.

`--existing` is for an application `init app` did not scaffold. Over a full scaffold it leaves that scaffold's `ci.yml`
building a runtime image nothing publishes any more (the dispatcher now follows `runtime image`), and that `ci.yml`'s
actionlint list leaves out `runtime-image.yml`; `init app` says so when it finds one. To retarget a copy of a template
repository, which still names the template's image, remove the two toolchain files that `--force` keeps and scaffold it
in full:

```sh
rm .patchy/agent.yaml .patchy/Dockerfile
patchy init app --force --repo acme/Shop.Web --registry 123456789012.dkr.ecr.us-west-2.amazonaws.com
```

`--force` then rewrites the application's files too, which in a fresh copy are the template's.

Published tags are immutable. To change the agent toolchain, edit `.patchy/Dockerfile` (or the dependencies) and bump
the tag in `.patchy/agent.yaml` to `toolchain-v2`, `toolchain-v3` and so on, in the same commit: the agent publisher
reads the tag from that file, and refuses a changed image at a published tag with a message saying to bump it.

## Permissions

Each action is a **custom RBAC verb**, granted independently: holding `approve` says nothing about `suspend`. The
per-finding verbs (`approve`, `retry`, `expedite`, `suspend`, `resume`) live on `findings.patchy.bitwisemedia.uk`; the
integration-scoped ones (`backfill`, `replay`, `reset`) on `integrations.patchy.bitwisemedia.uk`. To see yours:

```sh
patchy can-i                # the whole matrix, findings and integrations
patchy can-i approve        # one verb; exit code answers, for shell conditionals
patchy can-i backfill       # integration-scoped verbs resolve on integrations
```

Two things enforce those verbs, and only one of them matters:

- The CLI runs a `SelfSubjectAccessReview` before writing. This is **ergonomics** — a fast, clear failure naming the
  verb you lack instead of an opaque server rejection. It carries no security weight and is trivially bypassed by not
  using the CLI.
- A **`ValidatingAdmissionPolicy`** in the cluster binds each spec field to its verb. This runs inside the API server's
  admission chain, so it applies identically to `patchy`, `kubectl edit`, `kubectl patch`, server-side apply and raw
  `curl`. This is the actual enforcement. There are two policies: one on findings, one on integrations (gating
  `spec.backfill`/`spec.replay`/`spec.reset` while leaving the operator's configuration surface freely updatable).

That second piece exists because Kubernetes RBAC has no notion of a field: `update` on findings grants the whole object.
Without the policy, letting a developer suspend a finding would also let them rewrite its severity or forge an approval.
See [Kustomize](deployment/kustomize.md) for the manifest and the role ladder.

With [run attribution](configuration/attribution.md) on (chart `attribution.mode: attribute`), one more policy holds the
identity in what you write. An approval, retry or expedite you set must name you in `by` (your Kubernetes username,
which the CLI already writes) and carry no `byID`. A remediation you approve from the CLI is attributed to the
organisation, because a Kubernetes username is not a GitHub account.

Every action is written as a JSON merge patch of the one spec field it sets, guarded by the resource version the CLI
read, so the native verb it needs is `patch`. The patch also means the CLI's field manager owns that field and nothing
else: a `helm upgrade` of the Integrations that `patchy backfill` stamped applies without conflicts. The CLI sets no
field manager name of its own, so the API server takes it from the binary's file name: `patchy` as shipped,
`kubectl-patchy` when run as `kubectl patchy`, or whatever a renamed binary is called.

!!! warning "Requires Kubernetes 1.30+"

    `ValidatingAdmissionPolicy` reached GA in 1.30. On an older cluster the policy will not install and
    enforcement degrades silently to "whoever holds `update` owns the whole resource". Check with
    `kubectl api-resources | grep validatingadmissionpolic`.

Because the policy enumerates the spec fields it freezes, contributors adding a field to `FindingSpec` must extend it —
see [Extending](extending.md#adding-a-field-to-findingspec).

## Exit codes

| Code | Meaning                                                                   |
| ---- | ------------------------------------------------------------------------- |
| `0`  | success                                                                   |
| `1`  | runtime failure — unreachable cluster, bad response                       |
| `2`  | usage error                                                               |
| `3`  | the named resource does not exist                                         |
| `4`  | RBAC refused, or the action is unavailable in the finding's current phase |

## A triage session

```sh
patchy get findings --awaiting -o wide      # what needs a human
patchy review finding <name>                # what the agent concluded, and why
patchy browse finding <name>                # the tracking issue, if you want the thread
patchy approve finding <name>               # release the hold
```

## Output and piping

Rendered reports use the dark theme by default. On a light terminal, set the same variable `glow` and the other charm
tools read:

```sh
export GLAMOUR_STYLE=light     # or dark, dracula, tokyo-night, notty, ascii
```

There is no automatic detection: probing a terminal's background needs a query/response round trip that only an
interactive event loop can service, and `patchy` is a one-shot command. Tables are unaffected — they use the ANSI 0-15
palette, so they inherit whatever theme your terminal already has.

`stdout` carries data, `stderr` carries narration — so `-v` never corrupts a pipe, and "no findings found" goes to
stderr rather than into your `jq`. Styling turns itself off whenever stdout is not a terminal, and also honours
`--no-color`, `NO_COLOR`, and `TERM=dumb`.

```sh
patchy get findings -o json | jq '.items[] | select(.status.priority == "critical") | .metadata.name'
patchy get findings -o name | xargs -n1 patchy describe finding
```
