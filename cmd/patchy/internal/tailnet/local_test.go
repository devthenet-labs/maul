// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package tailnet

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// fakeClient puts a tailscale on PATH that runs script.
func fakeClient(t *testing.T, script string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake client is a shell script")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tailscale"), []byte("#!/bin/sh\n"+script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

// TestLocal: the client is asked for its status, read-only, and its
// output comes back as it was; a failing client is an error.
func TestLocal(t *testing.T) {
	fakeClient(t, `[ "$1 $2" = "status --json" ] || exit 9
/bin/cat <<'JSON'
`+statusJSON+`
JSON
`)
	raw, err := Local(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	st, err := ParseStatus(raw)
	if err != nil || !Find(st, "patchy-previews", previewTags).Found {
		t.Errorf("the status read back: %v", err)
	}

	fakeClient(t, "echo 'secret-ish detail' >&2; exit 1\n")
	if _, err := Local(context.Background()); err == nil {
		t.Error("a failing client gave a status")
	}
}

func TestLocalNoClient(t *testing.T) {
	if runtime.GOOS == "darwin" {
		if _, err := os.Stat(macAppCLI); err == nil {
			t.Skip("this Mac has the Tailscale app")
		}
	}
	t.Setenv("PATH", t.TempDir())
	if _, err := Local(context.Background()); !errors.Is(err, ErrNoClient) {
		t.Errorf("Local without a client = %v", err)
	}
}
