// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package tailnet

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"
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

// TestLocalDeadlineWithChildHoldingStdout is the regression test for the
// unbounded status read: a client that outlives the deadline and leaves a
// child holding stdout open must not hold Local until that child exits.
func TestLocalDeadlineWithChildHoldingStdout(t *testing.T) {
	t.Cleanup(func(old time.Duration) func() { return func() { localTimeout = old } }(localTimeout))
	localTimeout = 200 * time.Millisecond
	fakeClient(t, "/bin/sleep 20 &\nexec /bin/sleep 20\n")
	start := time.Now()
	_, err := Local(context.Background())
	if err == nil {
		t.Error("a client past its deadline gave a status")
	}
	if took := time.Since(start); took > localTimeout+localWaitDelay+3*time.Second {
		t.Errorf("Local took %s with a child holding stdout, want about %s", took, localTimeout+localWaitDelay)
	}
}

// TestLocalTooLarge: a client printing more than MaxStatus is refused as
// ErrStatus, not truncated into a status.
func TestLocalTooLarge(t *testing.T) {
	fakeClient(t, "/usr/bin/head -c "+strconv.Itoa(MaxStatus+1)+" /dev/zero\n")
	if _, err := Local(context.Background()); !errors.Is(err, ErrStatus) {
		t.Errorf("Local with %d bytes = %v, want ErrStatus", MaxStatus+1, err)
	}
}
