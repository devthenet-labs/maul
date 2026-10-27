// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Command preview-tester observes a Preview's approved http acceptance
// criteria from inside its slot. preview-controller runs it as a one-shot
// Job beside the Preview's components, with the criteria, the run tokens
// and each component's Service address in the literal environment variable
// MAUL_VERIFY_SPEC. It sends each step's request to that Service by name,
// writes one MAUL-VERIFY v1 line per criterion to stdout and a summary
// after them, and repeats the summary as its termination message.
// preview-controller reads both back; intent-controller judges.
//
// It holds no credential and no Kubernetes client, takes no flags and
// imports only the standard library and internal/acceptance (a test pins
// that). Exit 0 means the summary was written, whatever the criteria
// showed; 1 that the run stopped before its summary; 2 that the spec was
// refused. Engine in internal/previewtester.
package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/devthenet-labs/maul/internal/previewtester"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Getenv, terminationPath(os.Getenv), os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run is the binary, its environment, termination message file and
// streams injected. stdout carries only the wire format; diagnostics go to
// stderr.
func run(ctx context.Context, getenv func(string) string, termPath string, stdout, stderr io.Writer) int {
	log := slog.New(slog.NewTextHandler(stderr, nil))
	spec, err := previewtester.ParseSpec([]byte(getenv(previewtester.EnvSpec)))
	if err != nil {
		log.Error("spec refused", "variable", previewtester.EnvSpec, "err", err)
		return 2
	}
	dial, err := testDial(getenv)
	if err != nil {
		log.Error("test dial map refused", "err", err)
		return 2
	}
	client := previewtester.NewClient(previewtester.Options{Dial: dial})
	sum, err := previewtester.Run(ctx, spec, client, stdout)
	if err != nil {
		log.Error("run stopped before its summary", "err", err)
		return 1
	}
	if err := previewtester.WriteTermination(termPath, sum); err != nil {
		log.Error("termination message not written", "err", err)
		return 1
	}
	log.Info("observed", "criteria", sum.Count)
	return 0
}
