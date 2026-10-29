// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Command cdp-shim is the keyed CDP shim of a tool pod's browser container
// (DESIGN 4.2; SPEC S1-07): the preview-browser image's entrypoint, run as
// a native sidecar. It serves internal/cdpshim on --addr (default
// 127.0.0.1:9222, inside the pod's network namespace) to callers that
// present the key read once from --key-file and send no Origin, and starts
// one headless Chromium (--chromium) per accepted WebSocket over
// --remote-debugging-pipe, each with a fresh profile under --tmp: at most
// --max-live at once (2), killed on disconnect or after --idle without a
// message (10m).
//
// It holds no credential but the key, never logs it, and imports only the
// standard library and internal/cdpshim. Exit 0 on SIGTERM or SIGINT (every
// live browser killed), 1 when it cannot serve, 2 on a bad flag or key.
package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/devthenet-labs/maul/internal/cdpshim"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stderr)
	stop()
	os.Exit(code)
}

// run is the binary, its arguments and diagnostics stream injected. It
// serves until ctx ends.
func run(ctx context.Context, args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("cdp-shim", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", "127.0.0.1:9222", "the address to serve CDP on")
	keyFile := fs.String("key-file", "", "the mounted file holding the per-evaluation key (required)")
	chromium := fs.String("chromium", "/usr/bin/chromium", "the Chromium binary")
	tmp := fs.String("tmp", "", "the directory browser profiles are made in (the system temporary directory when empty)")
	maxLive := fs.Int("max-live", cdpshim.DefaultMaxLive, "live browsers at most")
	idle := fs.Duration("idle", cdpshim.DefaultIdle, "kill a browser whose connection carries nothing for this long")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	log := slog.New(slog.NewTextHandler(stderr, nil))
	if *keyFile == "" {
		log.Error("--key-file is required")
		return 2
	}
	key, err := cdpshim.ReadKey(*keyFile)
	if err != nil {
		log.Error("cannot read the key", "err", err)
		return 2
	}
	shim, err := cdpshim.New(cdpshim.Options{
		Key:     key,
		Launch:  cdpshim.Chromium{Path: *chromium, TempDir: *tmp, Stderr: stderr}.Launch,
		MaxLive: *maxLive,
		Idle:    *idle,
		Log:     log,
	})
	if err != nil {
		log.Error("cannot start", "err", err)
		return 2
	}
	defer shim.Close()
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Error("cannot listen", "addr", *addr, "err", err)
		return 1
	}
	hs := &http.Server{Handler: shim.Handler(), ReadHeaderTimeout: 10 * time.Second}
	served := make(chan error, 1)
	go func() { served <- hs.Serve(ln) }()
	log.Info("serving", "addr", ln.Addr().String(), "chromium", *chromium, "max_live", *maxLive, "idle", idle.String())
	select {
	case err := <-served:
		log.Error("stopped serving", "err", err)
		return 1
	case <-ctx.Done():
	}
	shim.Close()
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := hs.Shutdown(shutdown); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		log.Error("shutdown", "err", err)
	}
	return 0
}
