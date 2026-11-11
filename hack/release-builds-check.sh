#!/bin/sh
# Copyright 2026 Bitwise Media Group Ltd.
# SPDX-License-Identifier: MIT
#
# Hold .goreleaser.yaml's builds to cmd/: every binary directory under cmd/
# must be the main of exactly one goreleaser build, and every build's main
# must be a directory under cmd/. goreleaser itself refuses a build whose main
# is gone, but a new binary nobody added to the config builds nowhere and
# ships in no archive and no image, silently. The CI snapshot job runs this
# before `mise run snapshot`. yq comes pinned from mise.
set -eu

cd "$(dirname "$0")/.."

built=$(yq '.builds[].main' .goreleaser.yaml | sed 's|^\./||; s|/$||' | sort)
present=$(find cmd -mindepth 1 -maxdepth 1 -type d | sort)

status=0
dupes=$(printf '%s\n' "$built" | uniq -d)
if [ -n "$dupes" ]; then
	printf 'built by more than one goreleaser build:\n%s\n' "$dupes" >&2
	status=1
fi
missing=$(printf '%s\n' "$present" | while read -r dir; do
	printf '%s\n' "$built" | grep -qxF -- "$dir" || printf '%s\n' "$dir"
done)
if [ -n "$missing" ]; then
	printf 'under cmd/ but built by no goreleaser build (add it to .goreleaser.yaml builds):\n%s\n' "$missing" >&2
	status=1
fi
stale=$(printf '%s\n' "$built" | uniq | while read -r main; do
	printf '%s\n' "$present" | grep -qxF -- "$main" || printf '%s\n' "$main"
done)
if [ -n "$stale" ]; then
	printf 'a goreleaser build whose main is not a directory under cmd/:\n%s\n' "$stale" >&2
	status=1
fi
if [ "$status" -eq 0 ]; then
	printf 'every one of the %s binaries under cmd/ is a goreleaser build\n' "$(printf '%s\n' "$present" | wc -l | tr -d ' ')"
fi
exit "$status"
