<!-- maul:runner-image -->
### Agent runner image

maul could not use `docker.io/library/golang:1.26`, declared in `.maul/agent.yaml` (`NotAllowlisted`):
```text
image `docker.io/library/golang:1.26` is not under an allowlisted registry path (ghcr.io/acme/)
```

maul parked this finding for a human rather than run it on another image, because the operator set `onReject: handoff`. maul will not investigate this finding: it stays with its human owners.

**How to fix it:** correct the image or `.maul/agent.yaml`, then run `maul check image docker.io/library/golang:1.26`, which runs every check maul runs and reports each verdict. The [troubleshooting table](https://devthenet-labs.github.io/maul/integrations/agent-images/#troubleshooting) gives the fix for `NotAllowlisted`. maul reads the declaration once per finding, so the fix applies to the next finding on this repository.
