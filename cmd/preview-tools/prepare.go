// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"

	"github.com/devthenet-labs/maul/internal/previewtools/prepare"
	"github.com/devthenet-labs/maul/internal/sandboxprobe"
)

// prepareCommand is the agents-namespace tool pod's trusted init (SPEC
// S2-09): `preview-tools prepare --head-url --head-sha256 --base-url
// --base-sha256 --trees --bin`, as internal/jobs renders it.
const prepareCommand = "prepare"

// toolExecName is the exec shim's file name, in the image beside this
// binary and in the maul-bin volume, where the sandboxes run it from.
const toolExecName = "tool-exec"

// imageToolExec is where Dockerfile.preview-tools puts tool-exec.
const imageToolExec = "/usr/local/bin/" + toolExecName

// probeArgs select the strict probe over every target, the cluster
// resolver included, as the evaluator's prepare runs it.
var probeArgs = []string{
	"--mode=" + string(sandboxprobe.ModeStrict),
	"--targets=" + sandboxprobe.TargetInternet + "," + sandboxprobe.TargetKubernetes + "," +
		sandboxprobe.TargetMetadata + "," + sandboxprobe.TargetDNS,
}

// Exit statuses of prepare and seed besides 0 and the probe's own.
const (
	exitFailed     = 1
	exitMisconfigs = 2
)

// prepareFlags are prepare's arguments.
type prepareFlags struct {
	headURL, headSHA, baseURL, baseSHA string
	trees, bin                         string
}

// prepareDeps are what prepare reaches beyond its arguments.
type prepareDeps struct {
	fetcher  prepare.Fetcher
	tmpDir   string
	toolExec string
	probe    func(ctx context.Context) int
}

// defaultPrepareDeps are the image's: the artifact server over a client
// that follows no proxy or redirect, /tmp, the image's tool-exec and the
// strict probe in-process.
func defaultPrepareDeps() prepareDeps {
	return prepareDeps{
		fetcher: prepare.Fetcher{Client: prepare.NewClient()},
		tmpDir:  os.TempDir(), toolExec: imageToolExec, probe: strictProbe(os.Getenv),
	}
}

// strictProbe runs sandboxprobe as `agent-runner sandbox-probe` would with
// probeArgs: 0 when every target is blocked, sandboxprobe.ExitUnenforced
// when one answers.
func strictProbe(getenv func(string) string) func(context.Context) int {
	return func(ctx context.Context) int {
		return sandboxprobe.Main(ctx, probeArgs, getenv, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	}
}

// parsePrepare parses and checks prepare's arguments.
func parsePrepare(args []string) (prepareFlags, error) {
	var f prepareFlags
	fs := flag.NewFlagSet(prepareCommand, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&f.headURL, "head-url", "", "the head tarball on the artifact server")
	fs.StringVar(&f.headSHA, "head-sha256", "", "its sha256")
	fs.StringVar(&f.baseURL, "base-url", "", "the base tarball on the artifact server")
	fs.StringVar(&f.baseSHA, "base-sha256", "", "its sha256")
	fs.StringVar(&f.trees, "trees", "", "where each side's pristine tree goes, <trees>/<side>")
	fs.StringVar(&f.bin, "bin", "", "where tool-exec goes")
	if err := fs.Parse(args); err != nil {
		return f, err
	}
	if fs.NArg() > 0 {
		return f, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	var errs []error
	for name, u := range map[string]string{"head-url": f.headURL, "base-url": f.baseURL} {
		if p, err := url.Parse(u); err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" {
			errs = append(errs, fmt.Errorf("--%s %q is not an http(s) URL", name, u))
		}
	}
	for name, d := range map[string]string{"head-sha256": f.headSHA, "base-sha256": f.baseSHA} {
		if !prepare.ValidDigest(d) {
			errs = append(errs, fmt.Errorf("--%s %q is not 64 lower-case hex characters", name, d))
		}
	}
	if f.headURL == f.baseURL || f.headSHA == f.baseSHA {
		errs = append(errs, errors.New("head and base are one tarball"))
	}
	for name, p := range map[string]string{"trees": f.trees, "bin": f.bin} {
		if !filepath.IsAbs(p) {
			errs = append(errs, fmt.Errorf("--%s %q is not an absolute path", name, p))
		}
	}
	return f, errors.Join(errs...)
}

// runPrepare is `preview-tools prepare`: both trees fetched, verified and
// extracted, made readable by every side, tool-exec installed, then the
// strict probe, whose status is the init's (ExitUnenforced on open
// egress). Nothing after a failure runs: a refused tree is never followed
// by an installed shim or a probe.
func runPrepare(ctx context.Context, args []string, d prepareDeps, stderr io.Writer) int {
	log := slog.New(slog.NewTextHandler(stderr, nil))
	f, err := parsePrepare(args)
	if err != nil {
		log.Error("prepare: arguments refused", "err", err)
		return exitMisconfigs
	}
	sides := []struct{ side, url, sha string }{{"head", f.headURL, f.headSHA}, {"base", f.baseURL, f.baseSHA}}
	for _, s := range sides {
		dst := filepath.Join(f.trees, s.side)
		if err := d.fetcher.Tree(ctx, s.url, s.sha, d.tmpDir, dst); err != nil {
			log.Error("prepare: tree refused", "side", s.side, "url", s.url, "err", err)
			return exitFailed
		}
		if err := prepare.Readable(dst); err != nil {
			log.Error("prepare: tree not made readable", "side", s.side, "err", err)
			return exitFailed
		}
		log.Info("prepare: tree extracted", "side", s.side, "sha256", s.sha)
	}
	if err := prepare.Install(d.toolExec, f.bin); err != nil {
		log.Error("prepare: tool-exec not installed", "bin", f.bin, "err", err)
		return exitFailed
	}
	return d.probe(ctx)
}
