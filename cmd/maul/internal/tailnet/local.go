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
// A variable so a test can hold a client past it.
var localTimeout = 10 * time.Second

// localWaitDelay is how long Run waits, past the deadline, for a child of
// the client that still holds stdout open.
const localWaitDelay = time.Second

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
	cmd.WaitDelay = localWaitDelay
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

// limitedBuffer keeps at most max bytes and notes any more. Its buffer is
// a field, not embedded: an embedded bytes.Buffer would promote ReadFrom,
// and io.Copy (which exec uses for Stdout) would then read the whole
// stream past Write and its limit.
type limitedBuffer struct {
	buf  bytes.Buffer
	max  int
	over bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); len(p) > room {
		b.over = true
		if room > 0 {
			b.buf.Write(p[:room])
		}
		return len(p), nil
	}
	return b.buf.Write(p)
}

// Bytes is what was kept.
func (b *limitedBuffer) Bytes() []byte { return b.buf.Bytes() }

// String is what was kept, as a string.
func (b *limitedBuffer) String() string { return b.buf.String() }
