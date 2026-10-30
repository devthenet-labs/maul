// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

//go:build !e2edial

package previewtools

import "net"

// Seams is what only the e2e build (the e2edial tag, dial_e2e.go) can
// change. A released relay dials the addresses it was given, listens on
// Port, refuses local callers and writes its termination message to
// TerminationPath, whatever its environment says.
type Seams struct {
	// Dial replaces the network dialer; nil dials the address itself.
	Dial DialFunc
	// TerminationPath is the termination message file.
	TerminationPath string
	// ListenAddr is the address the relay listens on.
	ListenAddr string
	// AllowLocal admits loopback callers (the e2e suite's own).
	AllowLocal bool
	// ConfigDir is where the tool ConfigMap is mounted.
	ConfigDir string
}

// SeamsFrom returns the released relay's seams; getenv is unused.
func SeamsFrom(func(string) string) (Seams, error) {
	return Seams{TerminationPath: TerminationPath, ListenAddr: listenAddr, ConfigDir: DefaultConfigDir}, nil
}

// Browser is the released relay's browser: the image's (ImageBrowser), its
// preview front on FrontAddr.
func (Seams) Browser() (*BrowserOptions, ArtifactKeeper, net.Listener, error) {
	return openImageBrowser()
}
