// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Command tool-exec is the exec shim of a tool pod's sandbox container
// (DESIGN 4.2, SPEC S1-08): the toolbox image's entrypoint, run as a
// native sidecar. It serves internal/toolexec on a unix socket in the
// emptyDir it shares with the tools container (--socket, default
// /run/maul-exec/sock), writes each request's files under --scratch
// (default /work/scratch) and runs its argv there as the container's own
// UID, at most 60 s and 64 KiB of output each, one at a time.
//
// Programs get only the environment variables named in passEnv, read from
// the shim's own (the image's ENV), plus HOME at the scratch directory:
// whatever else the pod sets is withheld.
//
// With --workdir (SPEC S2-07) it is instead the workspace shim of an
// agents-namespace tool pod's sandbox, run from maul-bin on the
// repository's accepted image: a build runs in <workdir>/tree for the
// request's timeout (at most 20 minutes, its output's last 256 KiB kept),
// an exec in <workdir>/scratch/<run>; the programs' environment is only
// what each request passes (the search path and the build's network
// profile) plus the shim's own places, --home (default <workdir>/home) and
// --cache-dir (default <workdir>/cache), never the shim's own environment;
// and before it answers it kills every process of its UID a program left.
//
// It never serves as root, holds no credential and no Kubernetes client,
// and imports only the standard library and internal/toolexec (with its
// toolwire). Exit 0 on SIGTERM or SIGINT, 1 when it cannot serve, 2 on a
// bad flag.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/devthenet-labs/maul/internal/toolexec"
)

// passEnv names the variables a program inherits from the shim: the
// toolchains' own, as the toolbox image sets them.
var passEnv = []string{
	"PATH", "LANG", "TMPDIR", "XDG_CACHE_HOME", "SSL_CERT_FILE",
	"GOPATH", "GOCACHE", "GOMODCACHE", "GOTOOLCHAIN", "GOFLAGS", "GOPROXY", "GOSUMDB",
	"NODE_PATH", "NPM_CONFIG_CACHE", "PYTHONDONTWRITEBYTECODE", "PIP_NO_CACHE_DIR",
}

// getuid and procfs are the host's; a test replaces them.
var (
	getuid = os.Getuid
	procfs = toolexec.Procfs
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Getenv, os.Stderr)
	stop()
	os.Exit(code)
}

// run is the binary, its arguments, environment and diagnostics stream
// injected. It serves until ctx ends.
func run(ctx context.Context, args []string, getenv func(string) string, stderr io.Writer) int {
	fs := flag.NewFlagSet("tool-exec", flag.ContinueOnError)
	fs.SetOutput(stderr)
	socket := fs.String("socket", toolexec.SocketPath, "the unix socket to serve on")
	scratch := fs.String("scratch", "", "the directory files are written in and programs run in (default "+
		toolexec.ScratchDir+"; not with --workdir)")
	work := fs.String("workdir", "", "serve a workspace: the absolute directory holding tree, bin and scratch")
	home := fs.String("home", "", "a workspace's HOME, absolute (default <workdir>/home)")
	cache := fs.String("cache-dir", "", "a workspace's writable caches, absolute (default <workdir>/cache)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	log := slog.New(slog.NewTextHandler(stderr, nil))
	opts, err := options(*scratch, *work, *home, *cache, getenv)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	if getuid() == 0 {
		log.Error("cannot start: the shim never runs as root")
		return 1
	}
	srv, err := toolexec.NewServer(opts)
	if err != nil {
		log.Error("cannot start", "err", err)
		return 1
	}
	ln, err := toolexec.Listen(*socket)
	if err != nil {
		log.Error("cannot listen", "socket", *socket, "err", err)
		return 1
	}
	hs := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	served := make(chan error, 1)
	go func() { served <- hs.Serve(ln) }()
	log.Info("serving", "socket", *socket, "scratch", opts.Scratch, "workdir", opts.Work)
	select {
	case err := <-served:
		log.Error("stopped serving", "err", err)
		return 1
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := hs.Shutdown(shutdown); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		log.Error("shutdown", "err", err)
	}
	return 0
}

// options are the server's options from the flags: a slot shim's scratch
// directory and the environment it passes on, or a workspace's places.
func options(scratch, work, home, cache string, getenv func(string) string) (toolexec.Options, error) {
	if work == "" {
		if home != "" || cache != "" {
			return toolexec.Options{}, errors.New("--home and --cache-dir need --workdir")
		}
		if scratch == "" {
			scratch = toolexec.ScratchDir
		}
		var env []string
		for _, name := range passEnv {
			if v := getenv(name); v != "" {
				env = append(env, name+"="+v)
			}
		}
		return toolexec.Options{Scratch: scratch, Env: env}, nil
	}
	if scratch != "" {
		return toolexec.Options{}, errors.New("--scratch is <workdir>/scratch in a workspace")
	}
	if home == "" {
		home = filepath.Join(work, "home")
	}
	if cache == "" {
		cache = filepath.Join(work, "cache")
	}
	for _, p := range []string{work, home, cache} {
		if !filepath.IsAbs(p) {
			return toolexec.Options{}, fmt.Errorf("%q is not an absolute directory", p)
		}
	}
	return toolexec.Options{Work: work, Home: home, Cache: cache, Procfs: procfs}, nil
}
