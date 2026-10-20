## maul review

Read an agent's report on a finding

### Synopsis

Read what the agent concluded: its ratings, its verdict, and the report it wrote.

On a terminal the report is rendered for reading. Piped, or with -o markdown, it
is emitted as the markdown the agent actually wrote — paste it into an issue and
nothing is lost in translation.

```
maul review <resource> [name] [flags]
```

### Examples

```
  maul review finding my-finding
  maul review investigation --finding my-finding
  maul review investigation --finding my-finding --attempt 2
  maul review remediation my-finding-rem-1 --web
  maul review finding my-finding -o markdown > report.md
```

### Options

```
      --attempt int32    which attempt to read (default: the latest)
      --finding string   select the run by its finding instead of by name
  -h, --help             help for review
      --print-url        print that URL instead of opening it
      --raw              keep the machine frontmatter the agent emitted
      --web              open the tracking issue (investigations) or pull request (remediations)
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

