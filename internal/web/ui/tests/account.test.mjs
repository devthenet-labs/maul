// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

import assert from "node:assert/strict";
import { test } from "node:test";
import { MAX_CREDENTIAL_BYTES, credentialProblem, guessKind, healthOf } from "../src/account.ts";
import { hrefForAccount, parseRoute } from "../src/router.ts";

test("the account route", () => {
  assert.deepEqual(parseRoute("#/account"), { view: "account" });
  assert.deepEqual(parseRoute(hrefForAccount()), { view: "account" });
});

test("guessKind reads the Anthropic prefixes only", () => {
  assert.equal(guessKind(" sk-ant-api03-x"), "api-key");
  assert.equal(guessKind("sk-ant-oat01-x\n"), "oauth-token");
  assert.equal(guessKind("other"), null);
});

test("credentialProblem holds a value to what the server stores", () => {
  assert.equal(credentialProblem("  sk-ant-api03-abc\n", "api-key"), null);
  assert.equal(credentialProblem("anything-else", "oauth-token"), null);
  assert.match(credentialProblem("", "api-key"), /Paste/);
  assert.match(credentialProblem("two words", "api-key"), /one token/);
  assert.match(credentialProblem("a\r\nb", "api-key"), /one token/);
  assert.match(credentialProblem("ключ", "api-key"), /one token/);
  assert.match(credentialProblem("sk-ant-oat01-x", "api-key"), /setup-token/);
  assert.match(credentialProblem("sk-ant-api03-x", "oauth-token"), /api key/);
  assert.equal(credentialProblem("a".repeat(MAX_CREDENTIAL_BYTES), "api-key"), null);
  assert.match(credentialProblem("a".repeat(MAX_CREDENTIAL_BYTES + 1), "api-key"), /at most/);
});

test("healthOf reads an empty health as Unknown", () => {
  const c = { provider: "anthropic", stored: true, kind: "api-key", fingerprint: "", health: "", lastUsedAt: "", lastRejectedAt: "" };
  assert.equal(healthOf(c), "Unknown");
  assert.equal(healthOf({ ...c, health: "Rejected" }), "Rejected");
});
