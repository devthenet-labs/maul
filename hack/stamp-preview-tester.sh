#!/usr/bin/env sh
# Copyright 2026 Bitwise Media Group Ltd.
# SPDX-License-Identifier: MIT
#
# Stamp the preview tester's image, by digest, into a chart values file:
#
#   hack/stamp-preview-tester.sh <values.yaml> <repository> <sha256:digest>
#
# The release's helm job (.github/workflows/helm.yaml) runs it before
# packaging charts/maul. The tester runs inside preview slots, whose admission
# policy admits it only by exact digest, so the chart carries it by digest,
# never by tag.
#
# It replaces, in place, the one values line marked
# `# x-maul-preview-tester-image` (the testerImage line under
# preview.verification, marked as release-please marks its versions), so the
# file keeps its layout and comments, then reads the key back with yq
# (mikefarah v4). Anything amiss fails: a malformed repository or digest, no
# marked line or more than one, or the key not holding the image afterwards.
set -eu

if [ "$#" -ne 3 ]; then
  echo "usage: $0 <values.yaml> <repository> <sha256:digest>" >&2
  exit 2
fi
values=$1
repository=$2
digest=$3
marker='# x-maul-preview-tester-image'

if ! printf '%s\n' "$digest" | grep -Eqx 'sha256:[0-9a-f]{64}'; then
  echo "stamp: '$digest' is not a sha256 digest" >&2
  exit 1
fi
# A registry host (optionally with a port) and lowercase path components:
# nothing sed or YAML would read as syntax.
if ! printf '%s\n' "$repository" | grep -Eqx '[a-z0-9.-]+(:[0-9]+)?(/[a-z0-9._-]+)+'; then
  echo "stamp: '$repository' is not an image repository" >&2
  exit 1
fi
image="$repository@$digest"

count=$(grep -c -- "^ *testerImage: .*$marker\$" "$values" || true)
if [ "$count" != 1 ]; then
  echo "stamp: $values must hold exactly one 'testerImage: \"\" $marker' line (found $count)" >&2
  exit 1
fi
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT
sed -E "s|^( *testerImage: ).*($marker)\$|\\1\"$image\" \\2|" "$values" >"$tmp"
cat "$tmp" >"$values"

got=$(yq -r '.preview.verification.testerImage' "$values")
if [ "$got" != "$image" ]; then
  echo "stamp: preview.verification.testerImage is '$got' after stamping, not '$image'" >&2
  exit 1
fi
echo "preview.verification.testerImage=$image"
