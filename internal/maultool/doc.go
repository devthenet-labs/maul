// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package maultool is maul-tool, the agent evaluator's one shell CLI
// (DESIGN 2.1, 2.7; SPEC S1-09): the evaluator runs `maul-tool <verb> ...`
// through its harness's Bash tool, one command per call, and maul-tool
// sends it to the tools container of its tool pod over the toolwire
// contract (POST /v1/<verb>).
//
// Verbs: http <METHOD> <path> [json], browser <verb> [args], exec
// [--file f]... -- argv, logs <component>, events, check run <file> and
// check results; with S2, files ls|grep|read [--base] and run <dir> [--base]
// [-- args], and --base on exec and check run. confirm and finish are agent-runner's, never the agent's.
//
// Output, in a fixed order: the ledger line `[maul-tool ledger seq N]`;
// the time left of the agent's run (`[maul-tool time left] 12m30s`, when
// EnvDeadline is set); maul's own measurements (`[observed] ...`, one line
// each, any text a runner took from the page quoted onto that line, a
// failed text step's Received text on a line of its own marked as the
// page's); a `[screenshot] <path>` line for each image copied; then
// `[untrusted output]` and the inline output exactly as the tools
// container returned it, always last, so nothing the application chose
// can pass for one of maul's lines. A refusal prints the time left and
// `[maul-tool refused: <code>] <reason>` after the ledger line. Exit codes:
// ExitOK, ExitUsage (nothing sent), ExitRefused, ExitToolError.
//
// maul-tool runs as the agent, so it is held to the agent's own file
// confinement: it reads (--file, check run) and writes (screenshots, at
// shots/<seq>.png) only under the scratch directory, through os.Root, so
// neither a path nor a symlink reaches outside it. A screenshot is fetched
// by sha256 (toolwire.ArtifactPath) and kept only when its bytes match the
// digest and size the ledger entry records; the tools container never
// chooses a local path.
//
// It reads two environment variables, MAUL_TOOLS_URL (http or https, a bare
// origin) and MAUL_TOOLS_DEADLINE (the run's cutoff, RFC 3339; gate D-B8),
// and, when it exists, the per-evaluation key file KeyFile
// (/work/input/tools-key, which the model's own file tools cannot reach),
// sent as toolwire.KeyHeader with every call and never printed; it
// ignores proxy variables and follows no redirect.
// The package imports only the standard library and internal/toolwire (a
// test pins that).
package maultool
