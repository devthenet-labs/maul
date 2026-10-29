// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Command preview-tools is the relay in the tools container of the agent
// evaluator's tool pod. preview-controller runs it beside the browser and
// sandbox sidecars, with the tool ConfigMap mounted at
// /etc/maul-tools: the components' Service addresses, the saved checks to
// replay, the tool-call cap and the wall clock. It waits for the
// components, replays every saved check alone under fresh run tokens, then
// serves the evaluator's maul-tool calls on :8090, refusing callers on
// loopback or the pod's own addresses, until agent-runner's finish or its
// own deadline. Every call and run is a MAUL-TOOLS ledger line on stdout;
// the summary closes the ledger and is repeated as the termination
// message, which preview-controller checks the log against.
//
// It holds no Kubernetes client and no credential, and takes no flags.
// Exit 0 means the ledger was closed; 1 that it could not be; 2 that the
// config was refused. Engine in internal/previewtools.
package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/devthenet-labs/maul/internal/previewtools"
	"github.com/devthenet-labs/maul/internal/toolexec"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := 2
	seams, err := previewtools.SeamsFrom(os.Getenv)
	if err != nil {
		slog.New(slog.NewTextHandler(os.Stderr, nil)).Error("e2e seams refused", "err", err)
	} else {
		code = run(ctx, seams, os.Stdout, os.Stderr)
	}
	stop()
	os.Exit(code)
}

// run is the binary, its seams and streams injected. stdout carries only
// the ledger; diagnostics go to stderr.
func run(ctx context.Context, s previewtools.Seams, stdout, stderr io.Writer) int {
	log := slog.New(slog.NewTextHandler(stderr, nil))
	cfg, err := previewtools.LoadConfig(s.ConfigDir)
	if err != nil {
		log.Error("config refused", "dir", s.ConfigDir, "err", err)
		return 2
	}
	ln, err := listen(s)
	if err != nil {
		log.Error("listen", "addr", s.ListenAddr, "err", err)
		return 1
	}
	defer func() { _ = ln.Close() }()
	var ex previewtools.Execer
	if cfg.Sandbox {
		ex = toolexec.NewClient(toolexec.SocketPath)
	}
	sum, err := previewtools.Run(ctx, previewtools.RunOptions{
		Options: previewtools.Options{
			Config: cfg, Dir: s.ConfigDir, Out: stdout, Exec: ex, TerminationPath: s.TerminationPath,
			Client: previewtools.NewClient(previewtools.ClientOptions{Dial: s.Dial}),
		},
		Listener: ln,
	})
	if err != nil {
		log.Error("the ledger did not close cleanly", "err", err)
		return 1
	}
	log.Info("ledger closed", "entries", sum.Count, "sha256", sum.SHA256)
	return 0
}

// listen opens the relay's listener, guarded against local callers unless
// the e2e build admits them.
func listen(s previewtools.Seams) (net.Listener, error) {
	ln, err := net.Listen("tcp", s.ListenAddr)
	if err != nil {
		return nil, err
	}
	if s.AllowLocal {
		return ln, nil
	}
	own, err := previewtools.OwnAddresses()
	if err != nil {
		_ = ln.Close()
		return nil, err
	}
	return previewtools.GuardListener(ln, own), nil
}
