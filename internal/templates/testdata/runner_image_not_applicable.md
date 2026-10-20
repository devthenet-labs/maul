<!-- maul:runner-image -->
### Agent runner image

maul did not use `.devcontainer/devcontainer.json` to choose the image the agent runs in:
```text
`.devcontainer/devcontainer.json` builds its image (`build`); maul does not build images. Publish the image and set `image`, or declare one in `.maul/agent.yaml`, which takes precedence.
```

The agent ran in the default runner image instead, which carries no language toolchain. Nothing else about this finding changes.

**To run the agent in your own image**, publish it prebuilt and name it in `image`, or declare it in `.maul/agent.yaml`, which takes precedence. `maul check image <ref>` runs maul's checks on an image before you declare it, and the [agent image guide](https://devthenet-labs.github.io/maul/integrations/agent-images/) covers what the image needs.
