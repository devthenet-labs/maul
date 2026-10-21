## maul get

List maul resources

### Synopsis

List maul resources.

Columns come from the CRDs themselves, so they match `kubectl get` exactly;
-o wide adds the columns marked lower priority (issue and pull-request links).

The resource `all` lists every maul kind, each in its own table, in the
order the pipeline uses them. Kinds with nothing in them are left out.

The resource `usage` is the usage report the status page's Usage view shows:
metered cost, then tokens split by type (input, output, cache read, cache
write), per person and per Project, read from the UsageRollups.
It needs get on the virtual usage/all (the admin usage grant) and filters with
--user (a GitHub login, github-id:<id> or org), --project and --since
(30d, 720h or 2026-09-01; default 30 days, at most 400).

```
maul get <resource|all> [name...] [flags]
```

### Examples

```
  maul get findings
  maul get all
  maul get findings --phase AwaitingApproval --severity critical
  maul get findings --awaiting -o wide
  maul get investigations --finding my-finding
  maul get fnd my-finding -o yaml
  maul get usage --since 90d
  maul get usage --user octocat --project storefront -o json
```

### Options

```
      --awaiting           only findings with an action available
      --finding string     only runs belonging to this finding
  -h, --help               help for get
      --phase strings      only findings in these phases
      --priority strings   only findings at these priorities
      --project string     usage only: one Project
      --repo string        only findings whose repository name contains this
  -l, --selector string    label selector (server-side)
      --severity strings   only these severities: low, medium, high, critical
      --since string       usage only: the window, as days (30d), a duration (720h) or a first day (2026-09-01); default 30d
      --sort-by string     sort by: age, name, severity, priority, or phase (default "age")
      --source string      only findings from this source handler
      --suspended          only suspended findings
      --user string        usage only: one person (a GitHub login, github-id:<id>, or org)
      --verdict strings    only findings with these verdicts: remediate, ignore, manual
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

