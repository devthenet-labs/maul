// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

import assert from "node:assert/strict";
import { test } from "node:test";
import {
  MAX_CREDENTIAL_BYTES,
  credentialProblem,
  guessKind,
  healthOf,
  healthText,
  kindLabel,
  placeholder,
  providerLabel,
} from "../src/account.ts";
import { hrefForAccount, parseRoute } from "../src/router.ts";

test("the account route", () => {
  assert.deepEqual(parseRoute("#/account"), { view: "account" });
  assert.deepEqual(parseRoute(hrefForAccount()), { view: "account" });
});

const ANTHROPIC = ["api-key", "oauth-token"];
const OPENAI = ["api-key"];

test("guessKind reads each provider's own prefixes only", () => {
  assert.equal(guessKind("anthropic", " sk-ant-api03-x"), "api-key");
  assert.equal(guessKind("anthropic", "sk-ant-oat01-x\n"), "oauth-token");
  assert.equal(guessKind("anthropic", "other"), null);
  assert.equal(guessKind("anthropic", "sk-proj-x"), null);
  assert.equal(guessKind("openai", " sk-proj-x\n"), "api-key");
  assert.equal(guessKind("openai", "sk-x"), "api-key");
  assert.equal(guessKind("openai", "sk-ant-api03-x"), null);
  assert.equal(guessKind("openai", "other"), null);
});

test("credentialProblem holds an Anthropic value to what the server stores", () => {
  const p = (v, k) => credentialProblem("anthropic", ANTHROPIC, v, k);
  assert.equal(p("  sk-ant-api03-abc\n", "api-key"), null);
  assert.equal(p("anything-else", "oauth-token"), null);
  assert.match(p("", "api-key"), /Paste/);
  assert.match(p("two words", "api-key"), /one token/);
  assert.match(p("a\r\nb", "api-key"), /one token/);
  assert.match(p("ключ", "api-key"), /one token/);
  assert.match(p("sk-ant-oat01-x", "api-key"), /setup-token/);
  assert.match(p("sk-ant-api03-x", "oauth-token"), /api key/);
  assert.match(p("sk-proj-x", "api-key"), /OpenAI credential; store it under OpenAI/);
  assert.equal(p("a".repeat(MAX_CREDENTIAL_BYTES), "api-key"), null);
  assert.match(p("a".repeat(MAX_CREDENTIAL_BYTES + 1), "api-key"), /at most/);
});

test("credentialProblem holds an OpenAI value to OpenAI's rules", () => {
  const p = (v, k) => credentialProblem("openai", OPENAI, v, k);
  assert.equal(p(" sk-proj-abc\n", "api-key"), null);
  assert.equal(p("anything-else", "api-key"), null);
  assert.match(p("sk-ant-api03-x", "api-key"), /Anthropic credential; store it under Anthropic/);
  assert.match(p("sk-ant-oat01-x", "api-key"), /Anthropic/);
  assert.match(p("sk-proj-abc", "oauth-token"), /takes no oauth-token/);
  assert.match(p("two words", "api-key"), /one token/);
});

test("each provider has its own labels, placeholders and health text", () => {
  assert.equal(providerLabel("anthropic"), "Anthropic");
  assert.equal(providerLabel("openai"), "OpenAI");
  assert.equal(providerLabel("other"), "other");
  assert.equal(kindLabel("openai", "api-key"), "OpenAI API key");
  assert.equal(kindLabel("anthropic", "oauth-token"), "Claude setup-token (subscription)");
  assert.equal(placeholder("openai", "api-key"), "sk-proj-…");
  assert.equal(placeholder("anthropic", "oauth-token"), "sk-ant-oat…");
  assert.match(healthText("openai", "Rejected"), /^OpenAI rejected it/);
  assert.match(healthText("anthropic", "Valid"), /^Anthropic accepted it/);
  assert.match(healthText("other", "Unknown"), /Not checked yet/);
});

test("healthOf reads an empty health as Unknown", () => {
  const c = { provider: "anthropic", kinds: ANTHROPIC, stored: true, kind: "api-key", fingerprint: "", health: "", lastUsedAt: "", lastRejectedAt: "" };
  assert.equal(healthOf(c), "Unknown");
  assert.equal(healthOf({ ...c, health: "Rejected" }), "Rejected");
});
