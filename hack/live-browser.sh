#!/usr/bin/env bash
# Copyright 2026 Bitwise Media Group Ltd.
# SPDX-License-Identifier: MIT

# The browser tool against the real thing (`mise run live-browser`, CI's
# browser-tools job): installs internal/previewtools/node exactly from its
# lockfile (npm ci, no install scripts), then runs, with a real headless
# Chromium (MAUL_LIVE_CHROMIUM, else chromium or google-chrome on PATH):
#
#   - internal/cdpshim TestLiveCDPShim: the keyed CDP shim;
#   - internal/previewtools TestPlaywrightCLIFormat: the pinned CLI's
#     snapshot, ref and result grammar against its golden (a renovate bump
#     that changes it fails here), TestFixtureTypechecks: maul's fixture,
#     the runner's config and a compiled check under tsc --strict, and
#     TestLiveBrowser: the browser trial's matrix through the relay with no
#     model (overlap fails clickable, invisible fails visible, correct passes
#     all, three replays each);
#   - with MAUL_LIVE_PAGE_FUNCTIONS=1, internal/checks/playwright
#     TestLivePageFunctions on the same pinned packages (it launches
#     Playwright's own Chromium, downloaded first).
#
#   hack/live-browser.sh                  # always run
#   hack/live-browser.sh --if-changed REF # run only if the browser tool
#                                         # changed since REF
set -euo pipefail

cd "$(dirname "$0")/.."

watched=(
  internal/previewtools
  internal/checks/playwright
  internal/cdpshim
  hack/testdata/battle
  hack/live-browser.sh
  Dockerfile.preview-tools
  Dockerfile.preview-browser
)

if [[ "${1:-}" == "--if-changed" ]]; then
  ref="${2:?--if-changed needs a git ref}"
  if git diff --quiet "$ref" HEAD -- "${watched[@]}"; then
    echo "live-browser: the browser tool did not change since $ref; skipping"
    exit 0
  fi
fi

node_dir=internal/previewtools/node
(cd "$node_dir" && npm ci --ignore-scripts --no-audit --no-fund)

if [[ -z "${MAUL_LIVE_CHROMIUM:-}" ]]; then
  for c in chromium google-chrome chromium-browser; do
    if p="$(command -v "$c")"; then
      export MAUL_LIVE_CHROMIUM="$p"
      break
    fi
  done
fi
if [[ -z "${MAUL_LIVE_CHROMIUM:-}" ]]; then
  echo "live-browser: no Chromium found; set MAUL_LIVE_CHROMIUM" >&2
  exit 1
fi
export MAUL_LIVE_BROWSER=1

go test -count=1 -v -run '^TestLiveCDPShim$' ./internal/cdpshim
go test -count=1 -v -timeout 20m \
  -run '^(TestPlaywrightCLIFormat|TestFixtureTypechecks|TestLiveBrowser)$' ./internal/previewtools

if [[ "${MAUL_LIVE_PAGE_FUNCTIONS:-}" == 1 ]]; then
  (cd "$node_dir" && node node_modules/playwright/cli.js install chromium)
  MAUL_LIVE_PLAYWRIGHT="$PWD/$node_dir" go test -count=1 -v -run '^TestLivePageFunctions$' \
    ./internal/checks/playwright
fi
