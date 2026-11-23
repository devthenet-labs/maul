// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/devthenet-labs/maul/internal/toolwire"
)

// The browser relay's adapters in the image: the pinned Playwright CLI,
// run in the relay's one session (BrowserCLI), and the directory the
// relay keeps artifacts in by digest (ArtifactKeeper).

// PlaywrightCLI runs the image's Playwright CLI in the relay's session.
type PlaywrightCLI struct {
	// Node is the node binary; NodeDir the Node project; Dir the CLI's
	// working directory (its artifacts are written under it).
	Node, NodeDir, Dir string
	// Session names the session (SessionName).
	Session string
	// Timeout bounds one call; zero is a minute.
	Timeout time.Duration
}

// Run implements BrowserCLI.
func (p PlaywrightCLI) Run(ctx context.Context, args []string) (CLIResult, error) {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := os.MkdirAll(p.Dir, 0o700); err != nil {
		return CLIResult{}, err
	}
	bin := filepath.Join(p.NodeDir, "node_modules", "@playwright", "cli", "playwright-cli.js")
	argv := append([]string{bin, "-s=" + p.Session}, args...)
	cmd := exec.CommandContext(ctx, p.Node, argv...)
	cmd.Dir = p.Dir
	cmd.Env = []string{
		"PATH=" + filepath.Dir(p.Node) + ":/usr/bin:/bin",
		"HOME=" + p.Dir, "TMPDIR=" + p.Dir, "XDG_CACHE_HOME=" + filepath.Join(p.Dir, ".cache"),
		"NO_UPDATE_NOTIFIER=1", "PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1",
	}
	// A file, not a pipe: open starts the session's daemon, which keeps
	// the CLI's descriptors, so a pipe would not close when the CLI exits.
	out, err := os.CreateTemp(p.Dir, ".out-")
	if err != nil {
		return CLIResult{}, err
	}
	defer func() { _ = out.Close(); _ = os.Remove(out.Name()) }()
	cmd.Stdout, cmd.Stderr = out, out
	cmd.WaitDelay = 5 * time.Second
	err = cmd.Run()
	exit := 0
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee):
		exit = ee.ExitCode()
	case err != nil:
		return CLIResult{}, err
	}
	if _, err := out.Seek(0, io.SeekStart); err != nil {
		return CLIResult{}, err
	}
	b, err := io.ReadAll(io.LimitReader(out, toolwire.MaxOutputBytes))
	if err != nil {
		return CLIResult{}, err
	}
	return CLIResult{Output: string(b), Exit: exit}, nil
}

// DirArtifacts keeps artifacts as files named by their sha256 in a
// directory (an emptyDir in the pod).
type DirArtifacts struct {
	Dir string
}

// Put implements ArtifactKeeper.
func (d DirArtifacts) Put(data []byte) (string, error) {
	sum := sha256Hex(data)
	if err := os.MkdirAll(d.Dir, 0o700); err != nil {
		return "", err
	}
	return sum, os.WriteFile(filepath.Join(d.Dir, sum), data, 0o600)
}

// Read implements ArtifactStore.
func (d DirArtifacts) Read(digest string) ([]byte, error) {
	if toolwire.ArtifactPath(digest) == "" {
		return nil, os.ErrNotExist
	}
	f, err := os.Open(filepath.Join(d.Dir, digest))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, toolwire.MaxArtifactBytes+1))
}
