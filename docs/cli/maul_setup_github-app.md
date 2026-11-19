## maul setup github-app

Create the GitHub App maul authenticates as, and write its Secret

### Synopsis

Create the GitHub App maul authenticates as through GitHub's App manifest flow,
and write its credentials as the Secret manifest the Forge and Integration
resources name in spec.secretRef.

The App asks for the least that the features you choose use, and nothing
else; choose at least one:
  --security  the findings pipeline: code scanning alerts, issues, contents and
              pull requests write, and the webhook events code_scanning_alert,
              issues, issue_comment and pull_request (needs --webhook-url, the
              integration-controller's https://<host>/github/webhooks).
  --intents   intent-driven development: issues, contents, pull requests and
              statuses write. intent-controller polls GitHub, so intents add no
              webhook.
  --checks    with --intents: checks and actions read, which a
              Project's check-fix rounds (spec.checks.fix) need.
  --rerun-failed
              with --checks: actions write, which a Project's
              spec.checks.rerunFailed needs to re-run the failed jobs of the
              Actions runs behind a failed check once before a check-fix round.
Metadata read comes with every App. The intent permissions are the table
intent-controller proves before a Project is Ready.

The flow: maul serves a page on a random 127.0.0.1 port and opens it in your
browser; the page posts the manifest to GitHub's "create a GitHub App" form for
--org (you need to be an owner of it) or, with --user, your own account. Check
the form and click "Create GitHub App": GitHub sends the browser back to the
local page with a one-time code, which maul accepts only with the state it
started the flow with, exchanges for the App's credentials, and then stops
listening. The page is served once: if your browser says it was served
already, something else read it first, so stop maul and run it again.
With --no-browser nothing listens: maul writes the page to a file you open
in any browser, GitHub sends you back to your GitHub Apps settings, and you
paste that page's address (or just its code) into the terminal. A code works
once, within an hour.

With --org and a GitHub token in GH_TOKEN or GITHUB_TOKEN (gh auth token
prints yours), maul first asks GitHub whether you are an Owner of the
organization, and refuses to go on if you are only a member. Without a
token it warns and goes on. The token is used for that one request only.

The Secret manifest (--secret-name, default maul-github-<org> lowercased, or
maul-github with --user, in -n, default maul) holds appID, privateKey and, for
an App with a webhook, webhookSecret.
It is written to -o, default <secret-name>.secret.yaml, with mode 0600 (not
enforced on Windows), and an existing file is never replaced without --force.
-o - writes it to stdout instead, to pipe into an encryption tool such as
sops, and refuses a stdout that is a terminal or a file other users can
read (as a shell's > file is under the usual umask: use -o <file>). The
private key is printed nowhere else and GitHub keeps no copy: create or
encrypt the file, then delete it.

Beside it, maul writes the forge file (--forge-output, default
<secret-name>.forge.yaml in the Secret file's directory, always a file): no
secret, so keep it. It holds the Forge (--forge-name, default the App owner's
login lowercased) covering the App owner's repositories with that Secret
and, with --intents, a Role and RoleBinding that let intent-controller's
ServiceAccount (--intent-controller-sa, default maul-intent-controller, as
in a release named maul) get exactly that one Secret. Once the App exists,
change that subject in the forge file rather than run again: another run
creates another App.

Everything else goes to stderr, ending with a numbered next: block: the
link to install the App ("Only select repositories" is enough: for
intents, the intent repository and every application repository), the
kubectl command that creates the Secret, deletes its file and applies the
forge file, and, with --intents, the maul setup project command to run next.

--dry-run prints the manifest as JSON and creates nothing. github.com only.

```
maul setup github-app [flags]
```

### Examples

```
  maul setup github-app --org acme --intents --checks
  maul setup github-app --org acme --security --webhook-url https://maul.acme.dev/github/webhooks
  maul setup github-app --org acme --intents --dry-run
  maul setup github-app --org acme --intents -o - | sops --encrypt --input-type yaml --output-type yaml /dev/stdin > maul-github-acme.enc.yaml
  maul setup github-app --user --intents --no-browser
```

### Options

```
      --checks                        with --intents: the reads check-fix rounds need
      --dry-run                       print the manifest as JSON; create nothing
      --force                         replace an existing output file
      --forge-name string             name of the Forge written (default the App owner's login lowercased)
      --forge-output string           file to write the Forge (and with --intents its Secret reader Role) to (default <secret-name>.forge.yaml beside the Secret file)
  -h, --help                          help for github-app
      --homepage-url string           the App's homepage (default "https://github.com/devthenet-labs/maul")
      --intent-controller-sa string   with --intents: intent-controller's ServiceAccount, which the Role lets read the Secret (default "maul-intent-controller")
      --intents                       intent-driven development (no webhook)
      --name string                   the App's name, at most 34 characters (default maul-<org>, or maul)
      --no-browser                    open no browser and listen on no port: write the page to a file and paste the code back
      --org string                    organization to create the App under (you must be an owner)
  -o, --output string                 file to write the Secret manifest to, or - for stdout (default <secret-name>.secret.yaml)
      --rerun-failed                  with --checks: actions write, to re-run failed Actions jobs before a check-fix round
      --secret-name string            name of the Secret written (default maul-github-<org> lowercased, or maul-github with --user)
      --security                      the findings pipeline: alerts, tracking issues, remediation PRs
      --timeout duration              how long to wait for GitHub to send the browser back (default 15m0s)
      --user                          create the App under your own account instead of an organization
      --webhook-url string            with --security: the integration-controller's https://<host>/github/webhooks
```

### Options inherited from parent commands

```
  -A, --all-namespaces             work across every namespace
      --context string             kubeconfig context to use
      --kubeconfig string          path to the kubeconfig file
  -n, --namespace string           namespace to work in (default: the context's)
      --no-color                   disable colour and styling
      --request-timeout duration   timeout for a single API call (default 30s)
  -v, --verbose                    log what the CLI is doing to stderr
```

### SEE ALSO

* [maul setup](maul_setup.md)	 - Create what maul needs outside the cluster

