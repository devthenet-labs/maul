// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package httpguard

import "net/http"

// CountingWriter counts what one response wrote so the audit line can report
// status and bytes. Status is the first status written (200 once a body is
// written without one), 0 while nothing has been. It exposes the wrapped
// writer via Unwrap so an http.ResponseController still reaches the real
// Flusher.
type CountingWriter struct {
	http.ResponseWriter
	Status int
	Bytes  int64
}

// WriteHeader records the first status and passes every call through.
func (w *CountingWriter) WriteHeader(code int) {
	if w.Status == 0 {
		w.Status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

// Write counts the bytes the wrapped writer accepted.
func (w *CountingWriter) Write(p []byte) (int, error) {
	if w.Status == 0 {
		w.Status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.Bytes += int64(n)
	return n, err
}

// Unwrap returns the wrapped writer, for http.ResponseController.
func (w *CountingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
