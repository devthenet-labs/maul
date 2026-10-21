## maul mirror

Mirror upstream helm charts and OCI artifacts into a platform registry

### Synopsis

Maintain a vendored mirror store: a git tree where mirror.yaml holds global
defaults and every charts/<name>/ or artifacts/<name>/ directory pins one
upstream chart or OCI artifact — vendored for review, digest-locked,
provenance-verified, scanned, and published signed.

`upgrade` moves pins forward and regenerates the derived state; `sync`
converges the registry onto the committed state (idempotent, never replacing
an existing tag); `validate` proves the committed state is self-consistent
without touching the tree. maul never runs git: commits, branches and PRs
belong to the calling pipeline.

Flags also resolve from MAUL_MIRROR_* environment variables and an optional
.maul.yaml (mirror: block) in the working directory.

### Options

```
  -C, --directory string   mirror store directory (walks up to mirror.yaml; default: the working directory)
  -h, --help               help for mirror
      --workers int        concurrent registry operations per stage (default 4)
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
* [maul mirror add](maul_mirror_add.md)	 - Scaffold a new chart or artifact entry
* [maul mirror sync](maul_mirror_sync.md)	 - Publish committed charts and images to the mirror registries
* [maul mirror upgrade](maul_mirror_upgrade.md)	 - Move pins to newer upstream versions and regenerate derived state
* [maul mirror validate](maul_mirror_validate.md)	 - Prove the committed store is current, verified and clean

