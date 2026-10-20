// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package telemetry wires OpenTelemetry tracing, metrics, and structured
// logging into the maul binaries and owns their lifecycle.
//
// Telemetry is off by default. Init picks one of three modes, flag/config
// winning over environment: when a directory is configured (Config.Dir, wired
// by each binary's flags/config) it runs in file mode, writing one JSON file
// per signal (traces.json, metrics.json, logs.json) via the OTEL stdout
// exporters — the exact format otel-tui's --from-json-file ingests, so a user
// can save the directory, ship it to us, and we replay it. When no directory is
// set but OTEL_* environment variables are, it runs in env mode and defers to
// contrib/exporters/autoexport (OTLP to a collector, console, or none). With
// neither, it stays disabled and logging is the plain stderr handler maul has
// always used.
//
// The package installs the global Tracer/Meter/Logger providers, so the
// instrumented packages reach them through the otel.Tracer/otel.Meter globals
// and the default slog.Logger rather than importing this package — that
// one-way dependency keeps telemetry free of cycles with the code it observes.
// The slog logger fans every record out to both the stderr text handler (gated
// at the configured verbosity) and an otelslog bridge into the OTEL log
// pipeline (which accepts debug regardless), so the shipped file captures full
// diagnostics while stderr stays as quiet as it was before. Logging goes to
// stderr only; stdout stays reserved for agent-runner's event stream.
//
// Init returns a ShutdownFunc that flushes the providers in order
// (tracer, meter, logger) and then closes the files; main calls it inline after
// the command returns, since cobra skips PersistentPostRunE on error and os.Exit
// skips defers. Init never fails the caller: any setup error yields a working
// stderr-only logger plus a no-op shutdown alongside the error.
package telemetry
