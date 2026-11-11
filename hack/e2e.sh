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
# As one job on CI the suite took ~34 minutes (2030s on main in November
# 2026), well past `go test`'s default -timeout of ten minutes, so a slow run
# would panic with "test timed out". The explicit 45m leaves headroom; a later
# -timeout in "$@" wins, e.g. `mise run e2e -- -timeout 60m`.
#
# MAUL_E2E_SHARD=<index>/<total> (e.g. 1/2) runs one shard of the suite, as
# CI's e2e matrix does: e2e/shard reads the suite's test listing and prints
# the -run pattern of this shard's top-level tests, each test's shard a hash
# of its name, so the shards together run every test exactly once. Unset, the
# whole suite runs, as before.
set -eu

KUBEBUILDER_ASSETS=$(setup-envtest use --bin-dir "${HOME}/.cache/kubebuilder-envtest" -p path)
export KUBEBUILDER_ASSETS
cd e2e
if [ -n "${MAUL_E2E_SHARD:-}" ]; then
	listing=$(go test -list . ./...)
	run=$(printf '%s\n' "$listing" | go run ./shard "$MAUL_E2E_SHARD")
	set -- -run "$run" "$@"
fi
go test ./... -count=1 -timeout 45m "$@"
