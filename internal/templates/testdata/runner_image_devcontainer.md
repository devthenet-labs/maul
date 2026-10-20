<!-- maul:runner-image -->
### Agent runner image

`.devcontainer/devcontainer.json` declares `ghcr.io/acme/dev-env:2`. maul pinned it to `ghcr.io/acme/go-env@sha256:cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd` and verified its signature. The agent will run in it, with maul's own tools added under `/maul/bin`.

To change the image, edit `.devcontainer/devcontainer.json`; the [agent image guide](https://devthenet-labs.github.io/maul/integrations/agent-images/) covers what the image needs.
