## maul

Work with maul security findings from the terminal

### Synopsis

maul lists, inspects, reviews and acts on the custom resources that carry the
maul pipeline's state machine.

It talks to the Kubernetes API with your own kubeconfig — never through a
controller or the status page — so what you can do is what your RBAC allows.
Run `maul can-i` to see your grants.

### Options

```
  -A, --all-namespaces             work across every namespace
      --context string             kubeconfig context to use
  -h, --help                       help for maul
      --kubeconfig string          path to the kubeconfig file
  -n, --namespace string           namespace to work in (default: the context's)
      --no-color                   disable colour and styling
  -o, --output string              output format: table, wide, json, yaml, name, or markdown (default "table")
      --request-timeout duration   timeout for a single API call (default 30s)
  -v, --verbose                    log what the CLI is doing to stderr
```

### SEE ALSO

* [maul approve](maul_approve.md)	 - Approve a finding, releasing a hold or reviving a handed-off finding
* [maul backfill](maul_backfill.md)	 - Backfill an integration's pre-existing open alerts into findings
* [maul browse](maul_browse.md)	 - Browse to a resource's page in a browser
* [maul can-i](maul_can-i.md)	 - Show which actions your RBAC allows
* [maul check](maul_check.md)	 - Check something the way maul will judge it, before the pipeline does
* [maul completion](maul_completion.md)	 - Generate the autocompletion script for the specified shell
* [maul describe](maul_describe.md)	 - Show the full detail of one resource
* [maul dev](maul_dev.md)	 - Local test harnesses for generic-integration authors
* [maul expedite](maul_expedite.md)	 - Expedite a finding past the accumulation window and the queue
* [maul get](maul_get.md)	 - List maul resources
* [maul init](maul_init.md)	 - Scaffold what a repository needs to work with maul, without a cluster
* [maul mirror](maul_mirror.md)	 - Mirror upstream helm charts and OCI artifacts into a platform registry
* [maul resume](maul_resume.md)	 - Resume a suspended finding
* [maul retry](maul_retry.md)	 - Retry a failed finding from the state it failed in
* [maul review](maul_review.md)	 - Read an agent's report on a finding
* [maul setup](maul_setup.md)	 - Create what maul needs outside the cluster
* [maul suspend](maul_suspend.md)	 - Suspend a finding, pausing its progress through the pipeline

