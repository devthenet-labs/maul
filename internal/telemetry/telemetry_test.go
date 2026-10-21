// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package telemetry

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// clearOTELEnv blanks every OTEL_* var so env detection cannot drift a test into
// env mode on a machine that happens to set them.
func clearOTELEnv(t *testing.T) {
	t.Helper()
	for _, k := range otelEnvVars {
		t.Setenv(k, "")
	}
}

func TestInitDisabled(t *testing.T) {
	clearOTELEnv(t)
	prov, shutdown, err := Init(context.Background(), Config{})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if prov.Mode != ModeDisabled {
		t.Errorf("mode = %v, want disabled", prov.Mode)
	}
	if prov.Logger == nil {
		t.Fatal("disabled Init returned a nil logger")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("disabled shutdown: %v", err)
	}
}

func TestInitFileModeCreatesFiles(t *testing.T) {
	clearOTELEnv(t)
	dir := filepath.Join(t.TempDir(), "tel")
	prov, shutdown, err := Init(context.Background(), Config{Dir: dir, ServiceName: "maul", ServiceVersion: "test"})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if prov.Mode != ModeFile {
		t.Errorf("mode = %v, want file", prov.Mode)
	}
	for _, name := range []string{"traces.json", "metrics.json", "logs.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("expected %s to exist: %v", name, err)
		}
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("file-mode shutdown: %v", err)
	}
}

func TestInitFileWinsOverEnv(t *testing.T) {
	clearOTELEnv(t)
	t.Setenv("OTEL_TRACES_EXPORTER", "console")
	dir := filepath.Join(t.TempDir(), "tel")
	prov, shutdown, err := Init(context.Background(), Config{Dir: dir})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer func() {
		if err := shutdown(context.Background()); err != nil {
			t.Errorf("file-mode shutdown: %v", err)
		}
	}()
	if prov.Mode != ModeFile {
		t.Errorf("mode = %v, want file (the flag wins over OTEL_* env)", prov.Mode)
	}
}

// captureHandler is a slog.Handler that records the records it receives, gated
// at its own level so tests can assert per-child level routing through the
// fanout. It is safe for concurrent use.
type captureHandler struct {
	level   slog.Level
	mu      *sync.Mutex
	records *[]slog.Record
	attrs   []slog.Attr
}

func newCaptureHandler(level slog.Level) *captureHandler {
	return &captureHandler{level: level, mu: &sync.Mutex{}, records: &[]slog.Record{}}
}

func (h *captureHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	*h.records = append(*h.records, r)
	return nil
}

func (h *captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := *h
	next.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)
	return &next
}

func (h *captureHandler) WithGroup(string) slog.Handler { return h }

func (h *captureHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(*h.records)
}
