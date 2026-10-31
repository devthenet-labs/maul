#!/usr/bin/env bash
# Copyright 2026 Bitwise Media Group Ltd.
# SPDX-License-Identifier: MIT

# Prints the path of the REAL claude CLI at the version
# Dockerfile.claude-agent-runner pins (CLAUDE_VERSION overrides it), fetching
# it first if it is not cached. The probes that run the real CLI
# (hack/claude-posture-probe.sh, hack/evaluator-report-probe.sh) share it.
#
# The binary is fetched from the same release bucket the image's fetch stage
# uses and refused unless its sha256 matches the version's manifest, then
# cached per version and platform. Everything but the path goes to stderr.
set -euo pipefail

cd "$(dirname "$0")/.."

version="${CLAUDE_VERSION:-$(sed -n 's/^ARG CLAUDE_VERSION=//p' Dockerfile.claude-agent-runner)}"
if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "claude-cli: no usable CLAUDE_VERSION (got '$version')" >&2
  exit 1
fi

case "$(uname -s)/$(uname -m)" in
  Darwin/arm64) platform=darwin-arm64 ;;
  Darwin/x86_64) platform=darwin-x64 ;;
  Linux/x86_64) platform=linux-x64 ;;
  Linux/aarch64 | Linux/arm64) platform=linux-arm64 ;;
  *)
    echo "claude-cli: unsupported platform $(uname -s)/$(uname -m)" >&2
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

cache="${XDG_CACHE_HOME:-$HOME/.cache}/maul/claude-cli/$version/$platform"
cli="$cache/claude"
if [[ ! -x "$cli" ]]; then
  base="https://downloads.claude.ai/claude-code-releases/$version"
  tmp="$(mktemp -d "${TMPDIR:-/tmp}/claude-cli.XXXXXX")"
  trap 'rm -rf "$tmp"' EXIT
  curl -fsSL -o "$tmp/manifest.json" "$base/manifest.json"
  want="$(jq -er --arg p "$platform" '.platforms[$p].checksum' "$tmp/manifest.json")"
  if [[ ! "$want" =~ ^[0-9a-f]{64}$ ]]; then
    echo "claude-cli: manifest checksum for $platform is not a sha256" >&2
    exit 1
  fi
  curl -fsSL -o "$tmp/claude" "$base/$platform/claude"
  got="$(sha256 "$tmp/claude")"
  if [[ "$got" != "$want" ]]; then
    echo "claude-cli: claude $version $platform sha256 $got, manifest says $want" >&2
    exit 1
  fi
  chmod 0755 "$tmp/claude"
  mkdir -p "$cache"
  mv "$tmp/claude" "$cli"
fi

echo "claude-cli: $("$cli" --version) ($platform)" >&2
echo "$cli"
