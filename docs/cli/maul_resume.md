## maul resume

Resume a suspended finding

### Synopsis

Resume a suspended finding.

The action writes to the finding's spec only — a controller observes it and
moves the phase. Repeating an action that has already taken effect is a no-op,
so this is safe to re-run.

Your RBAC decides whether it lands: the CLI checks the 'resume' verb before
writing, and the cluster's admission policy enforces it regardless of client.

```
maul resume finding <name...> [flags]
```

### Examples

```
  maul resume finding my-finding
  maul resume finding -l maul.run/severity=critical
  maul resume finding my-finding --dry-run
```

### Options

```
      --dry-run           report what would change without writing
  -h, --help              help for resume
  -l, --selector string   act on every finding matching this label selector
  -y, --yes               do not prompt when acting on more than one finding
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

* [maul](maul.md)	 - Work with maul security findings from the terminal

