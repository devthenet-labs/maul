<!-- maul:runner-image -->
### Agent runner image

`.maul/agent.yaml` declares `ghcr.io/acme/go-env:1.26`. maul pinned it to `ghcr.io/acme/go-env@sha256:cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd`. Runs have launched in it, with maul's own tools added under `/maul/bin`.

The investigation (attempt 2) stopped before the agent started: the image failed the agent's preflight (`image_incompatible`):
```text
preflight: /maul/bin/claude --version: exit status 127: /lib/ld-linux-aarch64.so.1: not found (a musl image cannot run the claude CLI)
```

The image must be glibc-based, have `bash`, `sh` and `git` on its `PATH`, and let uid 65532 run them. `maul check image ghcr.io/acme/go-env:1.26 --run` runs the same preflight in a local container.

To change the image, edit `.maul/agent.yaml`; the [agent image guide](https://devthenet-labs.github.io/maul/integrations/agent-images/) covers what the image needs.
