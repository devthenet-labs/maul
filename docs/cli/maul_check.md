## maul check

Check something the way maul will judge it, before the pipeline does

### Synopsis

Judge something you are handing to maul the way the pipeline will judge it,
from your workstation, and report every verdict as a PASS, FAIL or SKIP line.

check image needs no cluster: it checks an agent image a repository means to
declare, and the kubeconfig flags are inert for it. check project reads the
cluster with your own kubeconfig, and GitHub and the registry with your own
credentials, to tell whether a Project is ready for its first intent. check
previews reads the cluster too, to tell whether the preview install every
Project's previews stand on is ready, before any Project exists.

### Options

```
  -h, --help   help for check
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
* [maul check image](maul_check_image.md)	 - Check an agent image a repository means to declare
* [maul check previews](maul_check_previews.md)	 - Check that the preview install is ready, before any Project uses it
* [maul check project](maul_check_project.md)	 - Check that a Project is ready for its first intent

