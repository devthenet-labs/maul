<!-- maul:runner-image -->
### Agent runner image

`.maul/agent.yaml` declares `ghcr.io/acme/go-env:1.26`. maul pinned it to `ghcr.io/acme/go-env@sha256:cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd` and verified its signature. Runs have launched in it, with maul's own tools added under `/maul/bin`.

To change the image, edit `.maul/agent.yaml`; the [agent image guide](https://devthenet-labs.github.io/maul/integrations/agent-images/) covers what the image needs.
