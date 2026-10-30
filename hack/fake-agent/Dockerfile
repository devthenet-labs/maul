# Copyright 2026 Bitwise Media Group Ltd.
# SPDX-License-Identifier: MIT
#
# The credential-less dev stand-in for the agent-runner image: a shell script
# named agent-runner that prints canned MAUL-EVENT lines (see ./agent-runner).
# The same image serves as the Job's init container, so it carries the tools
# the prepare script needs (curl for the artifact fetch, sha256sum/tar from
# busybox, git for the synthetic base commit). It also carries the real
# maul-tool (cmd/maul-tool), which the scripted evaluate stage drives as the
# agent evaluator would; the build context must hold it, cross-compiled for
# the image's platform (the Dockerfile carries no Go toolchain):
#
#   ctx=$(mktemp -d) && cp hack/fake-agent/* "$ctx" &&
#     CGO_ENABLED=0 GOOS=linux go build -trimpath -o "$ctx/maul-tool" ./cmd/maul-tool &&
#     docker build -t maul/fake-agent:dev "$ctx"
#
# `make dev-colima` builds it this way when MAUL_OVERLAY=dev-fake.
FROM alpine:3.24
RUN apk add --no-cache curl git
COPY agent-runner /usr/local/bin/agent-runner
COPY maul-tool /usr/local/bin/maul-tool
RUN chmod 0755 /usr/local/bin/agent-runner /usr/local/bin/maul-tool
USER 65532:65532
