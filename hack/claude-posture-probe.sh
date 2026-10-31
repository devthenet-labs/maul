#!/usr/bin/env bash
# Copyright 2026 Bitwise Media Group Ltd.
# SPDX-License-Identifier: MIT

# Runs internal/harness's TestClaudePostureProbe against the REAL claude CLI
# at the version Dockerfile.claude-agent-runner pins (CLAUDE_VERSION
# overrides it): the postured argv against a scratch tree full of hooks, an
# env block, settings and a .mcp.json, with a scripted fake model on
# loopback, and the evaluate posture against a scratch layout shaped like the
# evaluator pod (a stand-in token, maul-tool and screenshot). No model
# credential and no egress beyond the download.
#
# The binary is fetched, verified and cached by hack/claude-cli.sh.
#
#   hack/claude-posture-probe.sh                  # always run
#   hack/claude-posture-probe.sh --if-changed REF # run only if the CLI pin or
#                                                 # the posture changed since REF
set -euo pipefail

cd "$(dirname "$0")/.."

watched=(
  Dockerfile.claude-agent-runner
  internal/harness/harness.go
  internal/harness/claude.go
  internal/harness/claude_probe_test.go
  hack/claude-posture-probe.sh
  hack/claude-cli.sh
)

if [[ "${1:-}" == "--if-changed" ]]; then
  ref="${2:?--if-changed needs a git ref}"
  if git diff --quiet "$ref" HEAD -- "${watched[@]}"; then
    echo "claude-posture-probe: neither the claude CLI pin nor the posture changed since $ref; skipping"
    exit 0
  fi
fi

cli="$(./hack/claude-cli.sh)"
MAUL_CLAUDE_PROBE_CLI="$cli" go test -count=1 -v -run '^TestClaudePostureProbe$' ./internal/harness
