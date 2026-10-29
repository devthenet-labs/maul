// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package cdpshim

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
)

// MaxMessageBytes bounds one CDP message either way (SP-1: full-page
// screenshots).
const MaxMessageBytes = 64 << 20

// ErrNUL refuses a message holding a NUL: on the pipe it would end the
// message early and start a second one.
var ErrNUL = errors.New("cdpshim: message holds a NUL byte")

// Browser is one Chromium, spoken to over its debugging pipe.
type Browser interface {
	// Send writes one message.
	Send(msg []byte) error
	// Recv reads the next message; an error ends the browser's output.
	Recv() ([]byte, error)
	// Kill stops the browser and closes the pipe; it is idempotent and
	// unblocks Send and Recv.
	Kill()
	// Wait waits for the browser to exit and releases what it held. It is
	// called once, after Kill.
	Wait() error
}

// Launcher starts one browser.
type Launcher func() (Browser, error)

// Args is Chromium's argv after the binary: CDP over the pipe only,
// headless, the profile in userDataDir, and without Chromium's own sandbox
// (SP-3). Never a TCP debugging port, never --remote-allow-origins.
func Args(userDataDir string) []string {
	return []string{
		"--headless",
		"--remote-debugging-pipe",
		"--user-data-dir=" + userDataDir,
		"--no-sandbox",
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-dev-shm-usage",
		"--disable-background-networking",
		"--disable-breakpad",
		"about:blank",
	}
}

// Chromium launches the Chromium binary at Path, each with a fresh profile
// directory under TempDir (os.TempDir when empty). Its stderr goes to
// Stderr (discarded when nil); it inherits the shim's environment, which
// holds nothing secret.
type Chromium struct {
	Path    string
	TempDir string
	Stderr  io.Writer
}

// Launch starts one Chromium on a fresh pipe pair: fd 3 is what it reads,
// fd 4 what it writes. It runs in a process group of its own, which Kill
// takes down whole, and dies with the shim.
func (c Chromium) Launch() (Browser, error) {
	dir, err := os.MkdirTemp(c.TempDir, "cdp-shim-")
	if err != nil {
		return nil, fmt.Errorf("cdpshim: profile directory: %w", err)
	}
	fail := func(err error, files ...*os.File) (Browser, error) {
		for _, f := range files {
			_ = f.Close()
		}
		_ = os.RemoveAll(dir)
		return nil, err
	}
	chromeReads, shimWrites, err := os.Pipe()
	if err != nil {
		return fail(fmt.Errorf("cdpshim: pipe: %w", err))
	}
	shimReads, chromeWrites, err := os.Pipe()
	if err != nil {
		return fail(fmt.Errorf("cdpshim: pipe: %w", err), chromeReads, shimWrites)
	}
	cmd := exec.Command(c.Path, Args(dir)...)
	cmd.ExtraFiles = []*os.File{chromeReads, chromeWrites}
	cmd.Stderr = c.Stderr
	ownGroup(cmd)
	if err := cmd.Start(); err != nil {
		return fail(fmt.Errorf("cdpshim: start %s: %w", c.Path, err), chromeReads, chromeWrites, shimReads, shimWrites)
	}
	_ = chromeReads.Close()
	_ = chromeWrites.Close()
	wait := func() error {
		err := cmd.Wait()
		_ = os.RemoveAll(dir)
		return err
	}
	return newPipe(shimReads, shimWrites, func() { killGroup(cmd) }, wait), nil
}

// pipe frames messages on a browser's debugging pipe.
type pipe struct {
	in   *bufio.Scanner
	r    io.Closer
	mu   sync.Mutex // serialises writes
	w    io.WriteCloser
	kill func()
	wait func() error
	once sync.Once
}

// newPipe frames r (the browser's output) and w (its input); kill stops
// the browser and wait reaps it.
func newPipe(r io.ReadCloser, w io.WriteCloser, kill func(), wait func() error) *pipe {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), MaxMessageBytes+1)
	sc.Split(splitNUL)
	return &pipe{in: sc, r: r, w: w, kill: kill, wait: wait}
}

// splitNUL is a bufio.SplitFunc on NUL terminators; a trailing partial
// message at EOF is dropped, as Chromium never writes one.
func splitNUL(data []byte, atEOF bool) (int, []byte, error) {
	if i := bytes.IndexByte(data, 0); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF {
		return len(data), nil, nil
	}
	return 0, nil, nil
}

func (p *pipe) Send(msg []byte) error {
	if bytes.IndexByte(msg, 0) >= 0 {
		return ErrNUL
	}
	if len(msg) > MaxMessageBytes {
		return fmt.Errorf("cdpshim: message of %d bytes over %d", len(msg), MaxMessageBytes)
	}
	framed := make([]byte, len(msg)+1)
	copy(framed, msg)
	p.mu.Lock()
	defer p.mu.Unlock()
	_, err := p.w.Write(framed)
	return err
}

func (p *pipe) Recv() ([]byte, error) {
	if p.in.Scan() {
		return bytes.Clone(p.in.Bytes()), nil
	}
	if err := p.in.Err(); err != nil {
		return nil, err
	}
	return nil, io.EOF
}

func (p *pipe) Kill() {
	p.once.Do(func() {
		p.kill()
		_ = p.r.Close()
		_ = p.w.Close()
	})
}

func (p *pipe) Wait() error { return p.wait() }
