## maul dev resolve

Send a verdict write-back to a generic resolver

### Synopsis

Fire one signed verdict write-back per finding at a resolver endpoint. The
verdict is the one maul sends in production — ignore, "false positive" —
because dismissal after investigation is the only path that resolves today.

Production sends the write-back once per finding at dismissal, carrying every
accumulated alert; this command does not emulate accumulation, so it sends one
alert per call. Any 2xx answer is success, and maul retries failures — the
endpoint must treat an already-closed alert as success, not an error.

Payload files use the same envelope the webhook receives; flags also resolve
from MAUL_DEV_* and .maul.yaml/.yml/.json.

```
maul dev resolve <payload-file>... [flags]
```

### Examples

```
  maul dev resolve --url http://127.0.0.1:9000/resolve --secret dev-secret findings.json
  maul dev resolve --secret-file ./webhook.secret findings.json   # url from .maul.yaml
```

### Options

```
  -h, --help                 help for resolve
      --name string          integration name sent as the request's integration field (default "dev")
      --secret string        shared HMAC secret the endpoint verifies
      --secret-file string   file holding the shared HMAC secret
      --timeout duration     timeout per call (default 1m0s)
      --url string           resolver endpoint to call
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

* [maul dev](maul_dev.md)	 - Local test harnesses for generic-integration authors

