// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

//go:build e2edial

package previewtools

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"strconv"
)

// The e2e suite's seams: envtest has no kubelet, kube-proxy or cluster DNS,
// so the suite's fake kubelet runs the relay built with the e2edial tag,
// maps each component's Service address to a loopback listener, names a
// termination message file and a listen address of its own, calls the
// relay from loopback, stages a workspace's sockets and trees in
// directories of its own, and (no Chromium either) puts the scripted browser
// (browser_e2e.go) in the browser container's place. Never in a released
// binary.

const (
	// DialMapEnv is a JSON object mapping "<address>:<port>" to the
	// loopback address dialled instead; any other address is refused.
	DialMapEnv = "MAUL_E2E_DIAL_MAP"
	// TerminationEnv replaces the termination message path when set.
	TerminationEnv = "MAUL_E2E_TERMINATION_PATH"
	// ListenEnv replaces the listen address when set (a loopback one).
	ListenEnv = "MAUL_E2E_LISTEN"
	// ConfigDirEnv replaces the tool ConfigMap's mount when set.
	ConfigDirEnv = "MAUL_E2E_TOOLS_CONFIG_DIR"
	// BrowserEnv, an absolute directory, replaces the image's browser with
	// the scripted one (browser_e2e.go) working there.
	BrowserEnv = "MAUL_E2E_BROWSER_DIR"
	// ExecDirEnv and TreesDirEnv, absolute directories, replace a
	// workspace's exec-socket and pristine-trees mounts (the e2e kubelet
	// stages them in directories of its own).
	ExecDirEnv  = "MAUL_E2E_EXEC_DIR"
	TreesDirEnv = "MAUL_E2E_TREES_DIR"
	// SandboxUIDEnv, a UID, replaces both sides' shim UIDs: the e2e kubelet
	// serves both shims in its own process.
	SandboxUIDEnv = "MAUL_E2E_SANDBOX_UID"
)

// Seams is what only the e2e build can change (see dial.go).
type Seams struct {
	Dial            DialFunc
	TerminationPath string
	ListenAddr      string
	AllowLocal      bool
	ConfigDir       string
	// ExecDir and TreesDir are a workspace's sockets and trees (dial.go).
	ExecDir, TreesDir string
	// HeadUID and BaseUID are the UIDs each side's shim is served by
	// (dial.go); SandboxUIDEnv replaces both.
	HeadUID, BaseUID int
	// BrowserDir, when set, is the scripted browser's working directory.
	BrowserDir string
}

// Browser is the scripted browser when BrowserDir is set, else the image's.
func (s Seams) Browser() (*BrowserOptions, ArtifactKeeper, net.Listener, error) {
	if s.BrowserDir == "" {
		return openImageBrowser()
	}
	return ScriptedBrowser(s.BrowserDir)
}

// SeamsFrom reads the e2e seams from the environment.
func SeamsFrom(getenv func(string) string) (Seams, error) {
	s := Seams{TerminationPath: TerminationPath, ListenAddr: listenAddr, ConfigDir: DefaultConfigDir,
		ExecDir: DefaultExecDir, TreesDir: DefaultTreesDir, HeadUID: HeadSandboxUID, BaseUID: BaseSandboxUID}
	if d := getenv(ConfigDirEnv); d != "" {
		s.ConfigDir = d
	}
	if d := getenv(BrowserEnv); d != "" {
		if !filepath.IsAbs(d) {
			return Seams{}, fmt.Errorf("%s: %q is not an absolute directory", BrowserEnv, d)
		}
		s.BrowserDir = d
	}
	for env, dst := range map[string]*string{ExecDirEnv: &s.ExecDir, TreesDirEnv: &s.TreesDir} {
		if d := getenv(env); d != "" {
			if !filepath.IsAbs(d) {
				return Seams{}, fmt.Errorf("%s: %q is not an absolute directory", env, d)
			}
			*dst = d
		}
	}
	if u := getenv(SandboxUIDEnv); u != "" {
		uid, err := strconv.Atoi(u)
		if err != nil || uid < 0 {
			return Seams{}, fmt.Errorf("%s: %q is not a UID", SandboxUIDEnv, u)
		}
		s.HeadUID, s.BaseUID = uid, uid
	}
	if p := getenv(TerminationEnv); p != "" {
		s.TerminationPath = p
	}
	if a := getenv(ListenEnv); a != "" {
		if !loopback(a) {
			return Seams{}, fmt.Errorf("%s: %q is not a loopback address", ListenEnv, a)
		}
		s.ListenAddr, s.AllowLocal = a, true
	}
	raw := getenv(DialMapEnv)
	if raw == "" {
		return s, nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return Seams{}, fmt.Errorf("%s: %w", DialMapEnv, err)
	}
	for from, to := range m {
		if !loopback(to) {
			return Seams{}, fmt.Errorf("%s: %s maps to %q, not a loopback address", DialMapEnv, from, to)
		}
	}
	var d net.Dialer
	s.Dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		to, ok := m[addr]
		if !ok {
			return nil, &net.DNSError{Err: "e2e dial map: no listener", Name: addr, IsNotFound: true}
		}
		return d.DialContext(ctx, network, to)
	}
	return s, nil
}

func loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	ip := net.ParseIP(host)
	return err == nil && ip != nil && ip.IsLoopback()
}
