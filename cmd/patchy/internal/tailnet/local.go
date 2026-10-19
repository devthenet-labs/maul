// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package tailnet

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"time"
)

// localTimeout bounds one status read: the client answers from memory.
const localTimeout = 10 * time.Second

// macAppCLI is the CLI the macOS app bundles, for a Mac whose PATH has no
// tailscale.
const macAppCLI = "/Applications/Tailscale.app/Contents/MacOS/Tailscale"

// ErrNoClient reports a machine with no Tailscale CLI to ask.
var ErrNoClient = errors.New("no tailscale command on PATH")

// Local is `tailscale status --json` from the caller's own client:
// read-only, bounded in time and size. It returns the raw status for
// ParseStatus.
func Local(ctx context.Context) ([]byte, error) {
	bin, err := exec.LookPath("tailscale")
	if err != nil {
		if runtime.GOOS != "darwin" {
			return nil, ErrNoClient
		}
		if _, serr := os.Stat(macAppCLI); serr != nil {
			return nil, ErrNoClient
		}
		bin = macAppCLI
	}
	ctx, cancel := context.WithTimeout(ctx, localTimeout)
	defer cancel()
	out := &limitedBuffer{max: MaxStatus}
	cmd := exec.CommandContext(ctx, bin, "status", "--json")
	cmd.Stdout = out
	// A child that keeps stdout open past the deadline must not hold Run.
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		// stderr is never quoted: it is the client's, and says nothing a
		// re-run on a machine on the tailnet would not.
		return nil, fmt.Errorf("tailscale status --json: %w", err)
	}
	if out.over {
		return nil, fmt.Errorf("%w: more than %d bytes", ErrStatus, MaxStatus)
	}
	return out.Bytes(), nil
}

// limitedBuffer keeps at most max bytes and notes any more.
type limitedBuffer struct {
	bytes.Buffer
	max  int
	over bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.Len(); len(p) > room {
		b.over = true
		if room > 0 {
			b.Buffer.Write(p[:room])
		}
		return len(p), nil
	}
	return b.Buffer.Write(p)
}
