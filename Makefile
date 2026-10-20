# maul — triage and remediation pipeline for security findings, driven by
# GitHub issues as the state machine.
#
# Everything lives in mise tasks: the go-cli archetype (test/lint/release
# machinery + pinned tools) comes from the shared toolchain submodule at .mise/,
# selected in the root mise.toml, which also defines the repo-local tasks
# (multi-binary build). This Makefile is only the thin forwarding shim —
# `make <task>` == `mise run <task>`.
include .mise/mise.mk
