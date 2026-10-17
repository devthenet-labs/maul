#!/usr/bin/env bash
# Copyright 2026 Bitwise Media Group Ltd.
# SPDX-License-Identifier: MIT

# Runs internal/harness's TestClaudePostureProbe against the REAL claude CLI
# at the version Dockerfile.claude-agent-runner pins (CLAUDE_VERSION
# overrides it): the postured argv against a scratch tree full of hooks, an
# env block, settings and a .mcp.json, with a scripted fake model on
# loopback. No model credential and no egress beyond the download.
#
# The binary is fetched from the same release bucket the image's fetch stage
# uses and refused unless its sha256 matches the version's manifest, then
# cached per version and platform.
#
#   hack/claude-posture-probe.sh                  # always run
#   hack/claude-posture-probe.sh --if-changed REF # run only if the CLI pin or
#                                                 # the posture changed since REF
set -euo pipefail

cd "$(dirname "$0")/.."

watched=(
  Dockerfile.claude-agent-runner
  internal/harness/claude.go
  internal/harness/claude_probe_test.go
  hack/claude-posture-probe.sh
)

if [[ "${1:-}" == "--if-changed" ]]; then
  ref="${2:?--if-changed needs a git ref}"
  if git diff --quiet "$ref" HEAD -- "${watched[@]}"; then
    echo "claude-posture-probe: neither the claude CLI pin nor the posture changed since $ref; skipping"
    exit 0
  fi
fi

version="${CLAUDE_VERSION:-$(sed -n 's/^ARG CLAUDE_VERSION=//p' Dockerfile.claude-agent-runner)}"
if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "claude-posture-probe: no usable CLAUDE_VERSION (got '$version')" >&2
  exit 1
fi

case "$(uname -s)/$(uname -m)" in
  Darwin/arm64) platform=darwin-arm64 ;;
  Darwin/x86_64) platform=darwin-x64 ;;
  Linux/x86_64) platform=linux-x64 ;;
  Linux/aarch64 | Linux/arm64) platform=linux-arm64 ;;
  *)
    echo "claude-posture-probe: unsupported platform $(uname -s)/$(uname -m)" >&2
    exit 1
    ;;
esac

sha256() {
  if command -v sha256sum >/dev/null; then
    sha256sum "$1" | cut -d' ' -f1
  else
    shasum -a 256 "$1" | cut -d' ' -f1
  fi
}

cache="${XDG_CACHE_HOME:-$HOME/.cache}/patchy/claude-cli/$version/$platform"
cli="$cache/claude"
if [[ ! -x "$cli" ]]; then
  base="https://downloads.claude.ai/claude-code-releases/$version"
  tmp="$(mktemp -d "${TMPDIR:-/tmp}/claude-cli.XXXXXX")"
  trap 'rm -rf "$tmp"' EXIT
  curl -fsSL -o "$tmp/manifest.json" "$base/manifest.json"
  want="$(jq -er --arg p "$platform" '.platforms[$p].checksum' "$tmp/manifest.json")"
  if [[ ! "$want" =~ ^[0-9a-f]{64}$ ]]; then
    echo "claude-posture-probe: manifest checksum for $platform is not a sha256" >&2
    exit 1
  fi
  curl -fsSL -o "$tmp/claude" "$base/$platform/claude"
  got="$(sha256 "$tmp/claude")"
  if [[ "$got" != "$want" ]]; then
    echo "claude-posture-probe: claude $version $platform sha256 $got, manifest says $want" >&2
    exit 1
  fi
  chmod 0755 "$tmp/claude"
  mkdir -p "$cache"
  mv "$tmp/claude" "$cli"
fi

echo "claude-posture-probe: $("$cli" --version) ($platform)"
PATCHY_CLAUDE_PROBE_CLI="$cli" go test -count=1 -v -run '^TestClaudePostureProbe$' ./internal/harness
