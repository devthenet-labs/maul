#!/usr/bin/env sh
# Copyright 2026 Bitwise Media Group Ltd.
# SPDX-License-Identifier: MIT
#
# Run the end-to-end suite: an envtest kube-apiserver carries the CRD state
# machine, the real controller binaries run against it, and a fake GitHub
# API stands in at the network boundary. setup-envtest downloads the
# kube-apiserver/etcd binaries once into ~/.cache; the suite skips itself
# when KUBEBUILDER_ASSETS is empty, so plain `go test` stays fast.
#
# On CI the suite takes close to 30 minutes (1722s on main before the
# agents-namespace evaluator suite was added), well past `go test`'s default
# -timeout of ten minutes, so a slow run would panic with "test timed out".
# The explicit 45m leaves headroom; a later -timeout in "$@" wins, e.g.
# `mise run e2e -- -timeout 60m`.
set -eu

KUBEBUILDER_ASSETS=$(setup-envtest use --bin-dir "${HOME}/.cache/kubebuilder-envtest" -p path)
export KUBEBUILDER_ASSETS
cd e2e
go test ./... -count=1 -timeout 45m "$@"
