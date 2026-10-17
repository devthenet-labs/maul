## patchy setup

Create what patchy needs outside the cluster

### Synopsis

Create the things patchy needs before it is installed, from your workstation. Nothing
here talks to a cluster unless setup previews --detect asks it to read one: otherwise the
kubeconfig flags are inert, and -n only names the namespace a written manifest carries.

### Options

```
  -h, --help   help for setup
```

### Options inherited from parent commands

```
  -A, --all-namespaces             work across every namespace
      --context string             kubeconfig context to use
      --kubeconfig string          path to the kubeconfig file
  -n, --namespace string           namespace to work in (default: the context's)
      --no-color                   disable colour and styling
  -o, --output string              output format: table, wide, json, yaml, name, or markdown (default "table")
      --request-timeout duration   timeout for a single API call (default 30s)
  -v, --verbose                    log what the CLI is doing to stderr
```

### SEE ALSO

* [patchy](patchy.md)	 - Work with patchy security findings from the terminal
* [patchy setup github-app](patchy_setup_github-app.md)	 - Create the GitHub App patchy authenticates as, and write its Secret
* [patchy setup previews](patchy_setup_previews.md)	 - Write the files that expose previews and sign viewers in to them

