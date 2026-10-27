// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes/fake"
)

// The kube adapter reads at most the limit asked for, whatever the stream
// holds: the fake clientset serves "fake logs" for every pod.
func TestKubeLogReaderLimits(t *testing.T) {
	r := KubeLogReader{Pods: fake.NewClientset().CoreV1()}
	got, err := r.ReadLog(context.Background(), "maul-preview-0", "pod", "tester", LogOptions{LimitBytes: 4})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "fake" {
		t.Errorf("read %q, want the first 4 bytes", got)
	}
	if _, err := r.ReadLog(context.Background(), "maul-preview-0", "pod", "tester", LogOptions{}); err == nil {
		t.Error("read without a limit")
	}
}

// The directory adapter (envtest and e2e have no log endpoint) reads
// <dir>/<namespace>/<pod>/<container>.log, never outside its directory, and
// reports a missing log as NotFound.
func TestDirLogReader(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "maul-preview-0", "preview-shop-1-vt-0123456789-abcde", "tester.log")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("line one\nline two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := DirLogReader{Dir: dir}
	ctx := context.Background()
	got, err := r.ReadLog(ctx, "maul-preview-0", "preview-shop-1-vt-0123456789-abcde", "tester",
		LogOptions{LimitBytes: 8})
	if err != nil || string(got) != "line one" {
		t.Errorf("read %q, %v", got, err)
	}
	for _, name := range []string{"..", "../x", "a/b", "", "UPPER"} {
		if _, err := r.ReadLog(ctx, "maul-preview-0", name, "tester", LogOptions{LimitBytes: 8}); err == nil ||
			kerrors.IsNotFound(err) {
			t.Errorf("pod name %q: err %v, want a refusal", name, err)
		}
	}
	_, err = r.ReadLog(ctx, "maul-preview-0", "gone", "tester", LogOptions{LimitBytes: 8})
	if !kerrors.IsNotFound(err) {
		t.Errorf("missing log: %v, want NotFound", err)
	}
}
