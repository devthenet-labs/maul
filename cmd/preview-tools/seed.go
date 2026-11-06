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
	"os"
	"path/filepath"

	"github.com/devthenet-labs/maul/internal/previewtools/prepare"
)

// seedCommand copies one side's pristine tree into that side's work
// volume (SPEC S2-09): `preview-tools seed --side=head|base`, as
// internal/jobs renders the seed-head and seed-base inits.
const seedCommand = "seed"

// The seed's fixed paths: the side's pristine tree at seedTrees/<side>,
// read-only, and its work volume.
const (
	seedTrees = "/trees"
	seedWork  = "/work"
)

// sideUIDs are each side's sandbox UID (internal/jobs), the only UID its
// seed runs as, so the writable copy is that sandbox's alone.
var sideUIDs = map[string]int{"head": 65540, "base": 65541}

// seedDeps are the seed's paths and identity, fixed in the image.
type seedDeps struct {
	trees, work string
	getuid      func() int
}

func defaultSeedDeps() seedDeps { return seedDeps{trees: seedTrees, work: seedWork, getuid: os.Getuid} }

// parseSeed parses the seed's one argument, its side.
func parseSeed(args []string) (string, error) {
	fs := flag.NewFlagSet(seedCommand, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var side string
	n := 0
	fs.Func("side", "head or base", func(v string) error { side, n = v, n+1; return nil })
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if fs.NArg() > 0 {
		return "", fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if n != 1 {
		return "", errors.New("--side names exactly one side")
	}
	if _, ok := sideUIDs[side]; !ok {
		return "", fmt.Errorf("--side %q is not head or base", side)
	}
	return side, nil
}

// runSeed is `preview-tools seed`: refused unless it runs as its side's
// sandbox UID, then the work volume's layout and the tree's copy.
func runSeed(_ context.Context, args []string, d seedDeps, stderr io.Writer) int {
	log := slog.New(slog.NewTextHandler(stderr, nil))
	side, err := parseSeed(args)
	if err != nil {
		log.Error("seed: arguments refused", "err", err)
		return exitMisconfigs
	}
	if uid := d.getuid(); uid != sideUIDs[side] {
		log.Error("seed: refused; it runs only as its side's sandbox", "side", side, "uid", uid,
			"want", sideUIDs[side])
		return exitMisconfigs
	}
	if err := prepare.Seed(filepath.Join(d.trees, side), d.work); err != nil {
		log.Error("seed: tree not copied", "side", side, "err", err)
		return exitFailed
	}
	log.Info("seed: tree copied", "side", side)
	return 0
}
