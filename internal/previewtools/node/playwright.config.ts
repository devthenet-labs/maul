// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT
//
// The Playwright test runner's config for one compiled check (DESIGN 2.7,
// SPEC Q3): maul writes it beside the spec in a fresh directory and runs
// one check per process, one worker, no retries (the relay re-runs a
// failure itself under a fresh run token), the JSON reporter, nothing
// recorded. baseURL is the preview front.
import { defineConfig } from "@playwright/test";

export default defineConfig({
    testDir: ".",
    testMatch: "check.spec.ts",
    workers: 1,
    retries: 0,
    timeout: 30_000,
    forbidOnly: true,
    reporter: [["json", { outputFile: "results.json" }]],
    outputDir: "test-results",
    use: {
        baseURL: process.env.MAUL_BASE_URL,
        viewport: { width: 1280, height: 800 },
        actionTimeout: 5_000,
        trace: "off",
        screenshot: "off",
        video: "off",
    },
});
