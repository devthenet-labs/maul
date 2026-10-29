// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT
//
// maul's Playwright fixture (DESIGN 2.7, SP-1 (b)): every compiled check
// imports test and expect from here. The worker's browser is not launched:
// it is attached over CDP to the browser container's keyed shim, which
// starts a fresh Chromium for this connection alone (Q3). The key is read
// from the file MAUL_CDP_KEY_FILE names, never from the environment, so no
// value of it reaches a test's environment. maul writes this file beside
// every compiled check.
import { readFileSync } from "node:fs";
import { test as base, chromium, expect, type Browser } from "@playwright/test";

function required(name: string): string {
    const v = process.env[name];
    if (!v) throw new Error("maul: " + name + " is not set");
    return v;
}

function cdpKey(): string {
    return readFileSync(required("MAUL_CDP_KEY_FILE"), "utf8").trim();
}

export const test = base.extend<object, { browser: Browser }>({
    browser: [
        async ({}, use) => {
            const browser = await chromium.connectOverCDP(required("MAUL_CDP_URL"), {
                headers: { "X-Maul-Cdp-Key": cdpKey() },
                timeout: 10_000,
            });
            await use(browser);
            await browser.close();
        },
        { scope: "worker" },
    ],
});

export { expect };
