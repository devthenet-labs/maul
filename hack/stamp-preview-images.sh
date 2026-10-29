#!/usr/bin/env sh
# Copyright 2026 Bitwise Media Group Ltd.
# SPDX-License-Identifier: MIT
#
# Stamp one of maul's preview images, by digest, into a chart values file:
#
#   hack/stamp-preview-images.sh <values.yaml> <image> <repository> <sha256:digest>
#
# where <image> is one of
#
#   tester    preview.verification.testerImage         (# x-maul-preview-tester-image)
#   tools     preview.verification.tools.image         (# x-maul-preview-tools-image)
#   browser   preview.verification.tools.browserImage  (# x-maul-preview-browser-image)
#   toolbox   preview.verification.tools.toolboxImage  (# x-maul-preview-toolbox-image)
#
# The release's helm job (.github/workflows/helm.yaml) runs it once per image
# before packaging charts/maul. Each runs inside preview slots, whose
# admission policies admit it only by exact digest, so the chart carries it
# by digest, never by tag.
#
# It replaces, in place, the one values line marked for that image (marked as
# release-please marks its versions), so the file keeps its layout and
# comments, then reads the key back with yq (mikefarah v4). Anything amiss
# fails: an unknown image, a malformed repository or digest, no marked line
# or more than one, or the key not holding the image afterwards.
set -eu

if [ "$#" -ne 4 ]; then
  echo "usage: $0 <values.yaml> <tester|tools|browser|toolbox> <repository> <sha256:digest>" >&2
  exit 2
fi
values=$1
name=$2
repository=$3
digest=$4

case $name in
tester) key=testerImage path=.preview.verification.testerImage ;;
tools) key=image path=.preview.verification.tools.image ;;
browser) key=browserImage path=.preview.verification.tools.browserImage ;;
toolbox) key=toolboxImage path=.preview.verification.tools.toolboxImage ;;
*)
  echo "stamp: '$name' is not one of tester, tools, browser, toolbox" >&2
  exit 2
  ;;
esac
marker="# x-maul-preview-$name-image"

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

count=$(grep -c -- "^ *$key: .*$marker\$" "$values" || true)
if [ "$count" != 1 ]; then
  echo "stamp: $values must hold exactly one '$key: \"\" $marker' line (found $count)" >&2
  exit 1
fi
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT
sed -E "s|^( *$key: ).*($marker)\$|\\1\"$image\" \\2|" "$values" >"$tmp"
cat "$tmp" >"$values"

got=$(yq -r "$path" "$values")
if [ "$got" != "$image" ]; then
  echo "stamp: ${path#.} is '$got' after stamping, not '$image'" >&2
  exit 1
fi
echo "${path#.}=$image"
