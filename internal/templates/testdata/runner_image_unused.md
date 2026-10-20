<!-- maul:runner-image -->
### Agent runner image

`.maul/agent.yaml` declares `ghcr.io/acme/go-env:1.26`. maul pinned it to `ghcr.io/acme/go-env@sha256:cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd` and verified its signature. No run has used it: each ran in the default runner image instead, which carries no language toolchain. That happens when the operator has switched repository images off, when the sandbox check refused a repository image on an earlier run, when the runner is not claude, or once a human has revived the finding.

To change the image, edit `.maul/agent.yaml`; the [agent image guide](https://devthenet-labs.github.io/maul/integrations/agent-images/) covers what the image needs.
