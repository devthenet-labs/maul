// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/devthenet-labs/maul/internal/toolwire"
)

// The captured logs and events (DESIGN 2.9): preview-controller writes
// each component's redacted log tail and the slot's pod events into the
// tool ConfigMap on each poll; the relay re-reads the mounted file on every
// call, so the agent sees the latest poll. The bytes are the application's
// (a log line) or carry its words (a probe's message), so they are served
// as untrusted.

// readCapture reads one captured file in dir, at most
// toolwire.MaxOutputBytes. A file not captured yet is ("", false).
func readCapture(dir, name string) (string, bool, error) {
	f, err := os.Open(filepath.Join(dir, name))
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, toolwire.MaxOutputBytes))
	if err != nil {
		return "", false, err
	}
	return string(b), true, nil
}

// logs answers `maul-tool logs <component>`.
func (r *Relay) logs(req *toolwire.LogsRequest) result {
	if _, ok := r.cfg.component(req.Component); !ok {
		return refusal(toolwire.RefusedInvalid, "no previewed component is named "+req.Component)
	}
	return r.captured(LogFilePrefix+req.Component, "no log captured for "+req.Component+" yet")
}

// events answers `maul-tool events`.
func (r *Relay) events() result {
	return r.captured(EventsFile, "no pod events captured yet")
}

func (r *Relay) captured(name, missing string) result {
	text, ok, err := readCapture(r.dir, name)
	switch {
	case err != nil:
		return result{reason: "the capture could not be read"}
	case !ok:
		return result{reason: missing}
	}
	return result{output: text, inline: text}
}
