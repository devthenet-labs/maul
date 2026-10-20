<!-- maul:runner-image -->
### Agent runner image

maul could not use the declaration in `.maul/agent.yaml` (`InvalidDeclaration`):
```text
`.maul/agent.yaml` has unknown key `build`; `image` is the only key
```

The agent ran in the default runner image instead, which carries no language toolchain.

**How to fix it:** correct the image or `.maul/agent.yaml`, then run `maul check image <ref>`, which runs every check maul runs and reports each verdict. The [troubleshooting table](https://devthenet-labs.github.io/maul/integrations/agent-images/#troubleshooting) gives the fix for `InvalidDeclaration`. maul reads the declaration once per finding, so the fix applies to the next finding on this repository.
