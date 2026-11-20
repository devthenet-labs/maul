## maul setup project

Write a Project manifest, and print the commands that make it Ready

### Synopsis

Write the manifest of a Project: the intent repository whose issues become its
intents, the application repositories its intents build in, and the GitHub logins
whose approval counts. It holds no secret: keep it, and apply it with kubectl.

A repository is owner/repo (on github.com) or https://host/owner/repo. Each
--repo has a key, its short name in the names of the runs built in it: the
repository's name lowercased, unless written as key=owner/repo. The first --repo
is also where intents are planned. The name, keys, URLs, approvers and labels
are held to the Project schema's own rules, so a written manifest is one the
cluster admits. Every other field is left to its default; edit the file for
more. intent-controller creates the trigger label (maul:<name> unless
--trigger-label) and the approve label (maul:approved unless --approve-label)
on the intent repository.

The manifest goes to -o, default <name>.project.yaml, and an existing file is
never replaced without --force; -o - writes it to stdout. To change a Project
later, run the same command with every flag and --force, then apply the file
again. Nothing here talks to a cluster or to GitHub: the command ends with a
numbered next: block on stderr naming the App installation each repository
needs, the kubectl command to apply the file, and the maul check project
command that says whether the Project is Ready.

```
maul setup project <name> [flags]
```

### Examples

```
  maul setup project overdub --intent-repo overdubstudio/intents --repo overdubstudio/app --approver alice -n maul
  maul setup project shop --intent-repo acme/intents --repo acme/shop --repo api=acme/backend-api --approver alice --approver bob
  maul setup project shop --intent-repo https://github.com/acme/intents --repo acme/shop --approver alice -o - | kubectl apply -f -
```

### Options

```
      --approve-label string   the label that approves a plan (default maul:approved)
      --approver stringArray   a GitHub login whose approval counts (repeatable, 1 to 20)
      --force                  replace an existing output file
  -h, --help                   help for project
      --intent-repo string     the repository whose issues are the Project's intents: owner/repo or https://host/owner/repo
  -o, --output string          file to write the Project manifest to, or - for stdout (default <name>.project.yaml)
      --repo stringArray       an application repository, [key=]owner/repo or [key=]https://host/owner/repo (repeatable, 1 to 8)
      --trigger-label string   the label that starts an intent (default maul:<name>)
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

