// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package httpguard

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCountingWriter(t *testing.T) {
	t.Run("implicit 200 and bytes", func(t *testing.T) {
		rec := httptest.NewRecorder()
		w := &CountingWriter{ResponseWriter: rec}
		if w.Status != 0 {
			t.Fatalf("status before any write = %d, want 0", w.Status)
		}
		_, _ = w.Write([]byte("hello "))
		_, _ = w.Write([]byte("world"))
		if w.Status != http.StatusOK || w.Bytes != 11 {
			t.Fatalf("status, bytes = %d, %d; want 200, 11", w.Status, w.Bytes)
		}
		if rec.Body.String() != "hello world" {
			t.Fatalf("body = %q", rec.Body.String())
		}
	})
	t.Run("first status wins", func(t *testing.T) {
		rec := httptest.NewRecorder()
		w := &CountingWriter{ResponseWriter: rec}
		w.WriteHeader(http.StatusTeapot)
		w.WriteHeader(http.StatusInternalServerError) // superfluous; recorded nowhere
		_, _ = w.Write([]byte("x"))
		if w.Status != http.StatusTeapot || w.Bytes != 1 {
			t.Fatalf("status, bytes = %d, %d; want 418, 1", w.Status, w.Bytes)
		}
		if rec.Code != http.StatusTeapot {
			t.Fatalf("recorded code = %d", rec.Code)
		}
	})
}

// flushRecorder records whether Flush reached it.
type flushRecorder struct {
	*httptest.ResponseRecorder
	flushed bool
}

func (f *flushRecorder) Flush() { f.flushed = true }

// TestCountingWriterUnwrapReachesFlusher: an http.ResponseController on the
// wrapper still flushes the real writer, which SSE streaming depends on.
func TestCountingWriterUnwrapReachesFlusher(t *testing.T) {
	inner := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	w := &CountingWriter{ResponseWriter: inner}
	if w.Unwrap() != http.ResponseWriter(inner) {
		t.Fatal("Unwrap did not return the wrapped writer")
	}
	if err := http.NewResponseController(w).Flush(); err != nil {
		t.Fatalf("Flush through the wrapper: %v", err)
	}
	if !inner.flushed {
		t.Fatal("Flush did not reach the wrapped writer")
	}
}
