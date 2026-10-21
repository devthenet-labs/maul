## maul dev

Local test harnesses for generic-integration authors

### Synopsis

Test a generic integration against the real maul contract without a cluster:
`dev generic` hosts the receiver locally and drives the enhancer and resolver
calls; `dev enhance` and `dev resolve` fire a single outbound exchange for
authors whose process is only an enhancer or resolver.

Every flag also resolves from a MAUL_DEV_* environment variable and an
optional .maul.yaml (or .yml/.json) in the working directory, with explicit
flags winning over the environment, and the environment over the file.

### Options

```
  -h, --help   help for dev
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
* [maul dev enhance](maul_dev_enhance.md)	 - Send an enhancement request to a generic enhancer
* [maul dev generic](maul_dev_generic.md)	 - Host a local receiver to test a generic integration
* [maul dev resolve](maul_dev_resolve.md)	 - Send a verdict write-back to a generic resolver

