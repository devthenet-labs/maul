## maul can-i

Show which actions your RBAC allows

### Synopsis

Show what you may do in this namespace.

Each action is a custom RBAC verb, granted independently — holding 'approve'
says nothing about 'suspend'. The per-finding verbs (approve, retry, expedite,
suspend, resume) live on findings; the integration-scoped verbs (backfill,
replay, reset) live on integrations. With no argument this prints the whole
matrix, which is the fastest answer to "why was that refused?".

```
maul can-i [verb] [flags]
```

### Examples

```
  maul can-i
  maul can-i approve
  maul can-i backfill
  maul can-i approve -n maul
```

### Options

```
  -h, --help   help for can-i
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

