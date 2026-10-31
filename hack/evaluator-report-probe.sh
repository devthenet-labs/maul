#!/usr/bin/env bash
# Copyright 2026 Bitwise Media Group Ltd.
# SPDX-License-Identifier: MIT

# Runs internal/agentrun's TestEvaluatorReportProbe: the REAL model, through
# the REAL claude CLI at the version Dockerfile.claude-agent-runner pins
# (hack/claude-cli.sh), is given the real evaluate prompt (and, in a second
# session, the out-of-turns repair prompt) for a tiny synthetic round with a
# stand-in maul-tool, and the report it writes is held to the evaluate
# stage's strict decoder. It fails, printing the decoder's errors and the
# report, when maul would refuse what the model wrote. The e2e suite's
# scripted claude always writes the format exactly; only this shows whether
# the prompt teaches it.
#
# It needs a model credential and costs real money: two short sessions on
# the evaluate model, about $0.20 in all, each CLI run capped at $0.40 and
# its cost printed. The credential is the CLI's own: ANTHROPIC_API_KEY, or CLAUDE_CODE_OAUTH_TOKEN from
# `claude setup-token`. Without one it says so and exits 0.
# MAUL_EVALUATOR_PROBE_MODEL overrides the model (default claude-sonnet-5,
# intent-controller's --intent-evaluate-model).
#
#   hack/evaluator-report-probe.sh                  # always run
#   hack/evaluator-report-probe.sh --if-changed REF # run only if the prompts,
#                                                   # the report schema or the
#                                                   # CLI pin changed since REF
set -euo pipefail

cd "$(dirname "$0")/.."

watched=(
  Dockerfile.claude-agent-runner
  internal/templates/prompt_evaluate.md.tmpl
  internal/templates/evaluate_prompts.go
  internal/templates/prompt_repair.md.tmpl
  internal/templates/repair.go
  internal/report/evaluation.go
  internal/report/frontmatter.go
  internal/agentrun/evaluate_tools.go
  internal/agentrun/evaluate_report_probe_test.go
  internal/harness/claude.go
  hack/evaluator-report-probe.sh
  hack/claude-cli.sh
)

if [[ "${1:-}" == "--if-changed" ]]; then
  ref="${2:?--if-changed needs a git ref}"
  if git diff --quiet "$ref" HEAD -- "${watched[@]}"; then
    echo "evaluator-report-probe: neither the evaluator's prompts, its report schema nor the claude CLI pin changed since $ref; skipping"
    exit 0
  fi
fi

if [[ -z "${ANTHROPIC_API_KEY:-}" && -z "${CLAUDE_CODE_OAUTH_TOKEN:-}" ]]; then
  msg="evaluator-report-probe: SKIPPED, no model credential (set ANTHROPIC_API_KEY or CLAUDE_CODE_OAUTH_TOKEN; in CI, the repository secret EVALUATOR_PROBE_ANTHROPIC_API_KEY or EVALUATOR_PROBE_CLAUDE_CODE_OAUTH_TOKEN)"
  if [[ -n "${GITHUB_ACTIONS:-}" ]]; then
    echo "::warning title=evaluator-report-probe skipped::$msg"
  fi
  echo "$msg"
  exit 0
fi

cli="$(./hack/claude-cli.sh)"
MAUL_EVALUATOR_PROBE_CLI="$cli" go test -count=1 -v -timeout 30m -run '^TestEvaluatorReportProbe$' ./internal/agentrun
