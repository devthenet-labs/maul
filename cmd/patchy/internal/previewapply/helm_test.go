// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewapply

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeHelmBinary writes a helm stand-in that records its arguments, one
// per line, and answers `status` from the release named in its file.
func fakeHelmBinary(t *testing.T, status string) (string, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the helm stand-in is a shell script")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "args")
	script := `#!/bin/sh
printf '%s\n' "$@" >> "` + log + `"
echo --- >> "` + log + `"
case "$1" in
version) echo v3.19.0+gabcdef ;;
status) ` + status + ` ;;
upgrade) echo "Release has been upgraded."; [ "$2" != failing ] ;;
esac
`
	bin := filepath.Join(dir, "helm")
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil { //nolint:gosec // a test's own script
		t.Fatal(err)
	}
	return bin, log
}

func TestExecHelm(t *testing.T) {
	bin, log := fakeHelmBinary(t, `echo '{"name":"patchy","version":7,"info":{"status":"deployed"}}'`)
	var out bytes.Buffer
	h := ExecHelm{Binary: bin, Kubeconfig: "/k/config", Context: "desk", Out: &out}
	ctx := t.Context()
	if v, err := h.Version(ctx); err != nil || v != "v3.19.0+gabcdef" {
		t.Errorf("Version = %q, %v", v, err)
	}
	if rev, err := h.Revision(ctx, "patchy", "patchy"); err != nil || rev != 7 {
		t.Errorf("Revision = %d, %v", rev, err)
	}
	if err := h.Run(ctx, []string{"upgrade", "patchy", "chart", "-n", "patchy"}); err != nil {
		t.Errorf("Run: %v", err)
	}
	if !strings.Contains(out.String(), "upgraded") {
		t.Errorf("output not streamed: %q", out.String())
	}
	if err := h.Run(ctx, []string{"upgrade", "failing", "chart"}); err == nil ||
		!strings.Contains(err.Error(), "helm upgrade failing") {
		t.Errorf("Run(failing) = %v", err)
	}
	data, err := os.ReadFile(log) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	calls := strings.Split(strings.TrimSuffix(string(data), "---\n"), "---\n")
	want := []string{
		"version\n--short\n",
		"status\npatchy\n-n\npatchy\n-o\njson\n--kubeconfig\n/k/config\n--kube-context\ndesk\n",
		"upgrade\npatchy\nchart\n-n\npatchy\n--kubeconfig\n/k/config\n--kube-context\ndesk\n",
	}
	for i, w := range want {
		if i >= len(calls) || calls[i] != w {
			t.Errorf("call %d = %q, want %q", i, calls[min(i, len(calls)-1)], w)
		}
	}
}

// TestExecHelmNotInstalled: helm's "not found" is revision 0; any other
// failure is an error carrying helm's message.
func TestExecHelmNotInstalled(t *testing.T) {
	bin, _ := fakeHelmBinary(t, `echo 'Error: release: not found' >&2; exit 1`)
	if rev, err := (ExecHelm{Binary: bin}).Revision(t.Context(), "patchy", "patchy"); rev != 0 || err != nil {
		t.Errorf("Revision = %d, %v", rev, err)
	}
	bin, _ = fakeHelmBinary(t, `echo 'Error: Kubernetes cluster unreachable' >&2; exit 1`)
	if _, err := (ExecHelm{Binary: bin}).Revision(t.Context(), "patchy", "patchy"); err == nil ||
		!strings.Contains(err.Error(), "unreachable") {
		t.Errorf("Revision = %v", err)
	}
	if _, err := (ExecHelm{Binary: filepath.Join(t.TempDir(), "no-helm")}).Version(t.Context()); err == nil {
		t.Error("Version found a missing helm")
	}
}
