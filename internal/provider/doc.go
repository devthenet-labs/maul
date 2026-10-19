// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package provider is the pure logic behind brokered runners: which
// gateway environment a model provider (anthropic, bedrock, vertex,
// foundry: ClaudeNames) needs in the agent pod, and how canonical model ids
// translate to the provider-specific ids the claude CLI's --model flag
// expects there.
//
// Canonical ids ("anthropic/claude-sonnet-5") stay canonical everywhere
// controller-side — CRDs, allowlists, pricing; translation happens in-pod at
// agentrun's cliModel, fed by the PATCHY_MODEL_MAP this package renders. The
// per-provider derived defaults (Bedrock's region-prefixed ids, Vertex's bare
// ids) mean operators only write a model map for Foundry, whose deployment
// names cannot be derived, or to override a default.
//
// Names is the egress broker's closed route table: ClaudeNames plus openai,
// the Responses route the codex harness runs on. A brokered codex runner's
// pod gets only CodexEnv (the route), and its stages the token and
// placeholder variables CodexGateway names.
package provider
