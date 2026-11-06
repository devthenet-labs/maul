// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Command preview-tools is the relay in the tools container of the agent
// evaluator's tool pod. preview-controller runs it beside the browser and
// sandbox sidecars, with the tool ConfigMap mounted at
// /etc/maul-tools: the components' Service addresses, the saved checks to
// replay, the tool-call cap and the wall clock. In an agents-namespace tool
// pod (the config's mode workspace) intent-controller runs it instead, with
// the config and the :8090 key (/etc/maul-tools/tools-key) from its one
// Secret: it builds the head and base trees in their sandboxes (over
// /run/maul-exec/<side>/sock), replays every saved check at each side, and
// serves exec, files, run and checks only to callers holding the key. It waits for the
// components, replays every saved check alone under fresh run tokens, then
// serves the evaluator's maul-tool calls on :8090, refusing callers on
// loopback or the pod's own addresses, until agent-runner's finish or its
// own deadline. With a browser in the pod it first opens the Playwright
// CLI session against the browser container's keyed CDP shim (the key from
// the mounted /etc/maul-cdp/key, written only into a 0600 session config)
// and serves the preview front on 127.0.0.1:8091. Every call and run is a MAUL-TOOLS ledger line on stdout;
// the summary closes the ledger and is repeated as the termination
// message, which preview-controller checks the log against.
//
// It holds no Kubernetes client and no credential, and takes no flags.
// Exit 0 means the ledger was closed; 1 that it could not be; 2 that the
// config was refused. Engine in internal/previewtools.
//
// Two subcommands are an agents-namespace tool pod's trusted inits (SPEC
// S2-09), run before any PR code: `preview-tools prepare` fetches the head
// and base tarballs from the artifact server, verifies each against its
// digest, extracts them into the pristine trees volumes, installs the
// image's tool-exec into maul-bin and runs the strict sandbox probe over
// every target (exit 78 on open egress); `preview-tools seed --side` copies
// one side's pristine tree into its work volume as that side's sandbox UID.
// Exit 1 means a tree or the shim was refused, 2 that the arguments were.
// Engine in internal/previewtools/prepare.
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
	code := dispatch(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// dispatch runs the relay with no arguments, else the init its first
// argument names.
func dispatch(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case prepareCommand:
			return runPrepare(ctx, args[1:], defaultPrepareDeps(), stderr)
		case seedCommand:
			return runSeed(ctx, args[1:], defaultSeedDeps(), stderr)
		}
		slog.New(slog.NewTextHandler(stderr, nil)).Error("unknown command; the relay takes no arguments",
			"command", args[0])
		return exitMisconfigs
	}
	seams, err := previewtools.SeamsFrom(os.Getenv)
	if err != nil {
		slog.New(slog.NewTextHandler(stderr, nil)).Error("e2e seams refused", "err", err)
		return 2
	}
	return run(ctx, seams, stdout, stderr)
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
	opts := previewtools.RunOptions{
		Options: previewtools.Options{
			Config: cfg, Dir: s.ConfigDir, Out: stdout, Exec: ex, TerminationPath: s.TerminationPath,
			Client: previewtools.NewClient(previewtools.ClientOptions{Dial: s.Dial}),
		},
		Listener: ln,
	}
	if cfg.IsWorkspace() {
		key, err := previewtools.LoadKey(s.ConfigDir)
		if err != nil {
			log.Error("the per-evaluation key is refused", "dir", s.ConfigDir, "err", err)
			return 2
		}
		opts.Head, opts.Base = previewtools.NewSandboxes(s.ExecDir)
		opts.Trees, opts.Key = s.TreesDir, key
	}
	if cfg.Browser {
		front, err := withBrowser(&opts, s)
		if err != nil {
			// The ledger still closes: every browser call is "unavailable".
			log.Error("the browser relay is unavailable", "err", err)
		} else {
			defer func() { _ = front.Close() }()
		}
	}
	sum, err := previewtools.Run(ctx, opts)
	if err != nil {
		log.Error("the ledger did not close cleanly", "err", err)
		return 1
	}
	log.Info("ledger closed", "entries", sum.Count, "sha256", sum.SHA256)
	return 0
}

// withBrowser sets up the browser relay and its preview front's loopback
// listener: the image's, unless the e2e build scripts it (Seams.Browser).
func withBrowser(o *previewtools.RunOptions, s previewtools.Seams) (net.Listener, error) {
	br, store, front, err := s.Browser()
	if err != nil {
		return nil, err
	}
	o.Browser, o.Artifacts, o.Front = br, store, front
	return front, nil
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
