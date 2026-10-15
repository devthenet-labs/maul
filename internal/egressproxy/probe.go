// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
)

// ProbeResult is what a probe reads off its request's audit line.
type ProbeResult struct {
	Status         int    `json:"status"`
	UpstreamStatus int    `json:"upstream_status"`
	Outcome        string `json:"outcome"`
	Reason         string `json:"reason"`
	Credential     string `json:"credential"`
}

// OK reports whether the probed request was forwarded and answered 200.
func (r ProbeResult) OK() bool {
	return r.Outcome == OutcomeProxied && r.Status == http.StatusOK
}

// Probe serves one GET of path on srv's handler for profile, discarding the
// body, and reads the audit line the request left in audit, the buffer
// srv's audit logger writes JSON lines to. Lines without an outcome (a
// credential_rejected warning) are not request lines and are skipped; of
// the rest, the last is the probe's own.
func Probe(ctx context.Context, srv *Server, profile, path string, audit *bytes.Buffer) (ProbeResult, error) {
	h, err := srv.Handler(profile)
	if err != nil {
		return ProbeResult{}, err
	}
	u, err := url.ParseRequestURI(path)
	if err != nil {
		return ProbeResult{}, err
	}
	req := (&http.Request{Method: http.MethodGet, URL: u, RequestURI: path, Proto: "HTTP/1.1", ProtoMajor: 1,
		ProtoMinor: 1, Header: http.Header{}, Body: http.NoBody, Host: "probe", RemoteAddr: "127.0.0.1:0",
	}).WithContext(ctx)
	func() {
		// An aborted stream is the engine's answer too: its audit line
		// is written while the abort unwinds.
		defer func() {
			if r := recover(); r != nil && r != http.ErrAbortHandler { //nolint:errorlint // sentinel panic value
				panic(r)
			}
		}()
		h.ServeHTTP(&discardWriter{header: http.Header{}}, req)
	}()
	var last ProbeResult
	sc := bufio.NewScanner(bytes.NewReader(audit.Bytes()))
	found := false
	for sc.Scan() {
		var line ProbeResult
		if json.Unmarshal(sc.Bytes(), &line) == nil && line.Outcome != "" {
			last, found = line, true
		}
	}
	if !found {
		return ProbeResult{}, errors.New("the request left no audit line")
	}
	return last, nil
}

// discardWriter is a ResponseWriter that keeps nothing but headers.
type discardWriter struct {
	header http.Header
}

func (w *discardWriter) Header() http.Header         { return w.header }
func (w *discardWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w *discardWriter) WriteHeader(int)             {}
