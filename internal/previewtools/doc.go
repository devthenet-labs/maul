// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package previewtools is the engine of cmd/preview-tools, the relay in
// the tools container of the agent evaluator's tool pod (DESIGN 2.1, 2.2,
// 2.7, 2.9, 4.2; SPEC S1-06, Q1, Q7, Q18). It replaces slice 1's tester:
// its HTTP client and readiness wait come from internal/previewtester,
// which stays until S4.
//
// Config (config.go) is its one input, the tool ConfigMap preview-
// controller mounts at DefaultConfigDir: ConfigFile holds the components
// (key, path prefix on the preview host, "<ClusterIP>:<port>"; the pod
// resolves no name), the preview host and the pairwise viewer the gateway
// would send, the saved checks to replay, maxToolCalls, the wall clock, and
// whether this is a replay round; "log.<component>" and EventsFile hold the
// captured log tails and pod events (capture.go), re-read on every call.
//
// The Relay (server.go) serves POST /v1/<verb> on Port, one endpoint per
// toolwire verb, one call at a time, and writes every call, refused ones
// included, to the toolwire ledger on stdout:
//
//   - http: the path routed to the component serving it on the preview
//     host (v1alpha1.PreviewRoute), sent with the gateway's header set
//     (previewauth.CleanRequest, Host the preview host); the exchange is
//     untrusted, the status and latency observed;
//   - browser: one Playwright CLI call in the relay's session (browser.go):
//     the allowlisted verbs only, no flags, navigation confined to the
//     preview front, the post-action snapshot inline (capped), ANSI
//     stripped, favicon noise filtered, every artifact hashed and kept
//     (served to maul-tool at GET /v1/artifacts/<sha256>), and
//     generate-locator recorded for each ref acted on; "unavailable" in a
//     pod with no browser;
//   - exec: argv run in the sandbox through the exec shim (exec.go);
//   - logs, events: the captured files;
//   - check run: internal/checks validates the check, then a dry run;
//     check results: the replay and each dry-run check's latest run;
//   - confirm (agent-runner's): every named digest needs a green dry run
//     in this evaluation, else nothing runs; each check then runs again,
//     a relay entry each;
//   - finish (agent-runner's): the cited images as evidence lines, the
//     summary to stdout and the termination message, and the relay ends.
//
// The browser relay owns the CLI session: opened against the browser
// container's keyed CDP shim before the agent starts (the key only in a
// 0600 session config), its viewport set to 1280x800, reopened once if
// lost; each open a relay session entry. The preview front (NewFront, on
// loopback FrontAddr) gives the browser one origin, the components
// path-routed behind it as the gateway routes them. A browser check
// (browsercheck.go) is compiled by internal/checks/playwright, written
// into a fresh directory beside maul's fixture (node/maul-fixture.ts) and
// config, and run by the pinned Playwright test runner in its own process
// against a fresh Chromium; the JSON reporter's report is judged here.
//
// Every check run (checkrun.go) is the check alone, under a fresh run
// token drawn from crypto/rand, re-run once under another after a
// non-pass, so a fail then a pass is quarantined; every assertion is
// judged here, never in the sandbox. A response keeps the application's
// bytes (Untrusted) apart from maul's measurements (Observed) and maul's
// words (Reason, step details), which never quote the application.
//
// Past maxToolCalls an agent call is refused, and the refusal recorded;
// the ledger keeps reservedEntries of room for confirmation and finish.
// GuardListener refuses connections from loopback and from the pod's own
// addresses, so page JS and sandbox code cannot call the relay.
//
// Run (lifecycle.go) is the container's life: readiness wait, replay, a
// replay round's finish, else serve until the runner's finish, the wall
// clock or a stop; every way out closes the ledger.
//
// dial.go and, under the e2edial tag only, dial_e2e.go are the e2e
// suite's seams. The package imports only the standard library,
// api/v1alpha1 and internal/acceptance, checks, checks/playwright,
// previewauth, toolexec and toolwire (a test pins that): no Kubernetes client, no credential.
package previewtools
