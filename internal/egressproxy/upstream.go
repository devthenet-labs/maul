// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/bitwise-media-group/patchy/internal/httpguard"
)

// copiedHeaders are the only response headers that leave the proxy.
var copiedHeaders = []string{"Content-Type", "Last-Modified"}

// outboundURL joins the fixed base and the validated escaped path. The path
// is the request's own, never rebuilt from the parsed module and version.
// net/url sends it in its canonical escaping, which is the escaping it
// arrived in: a path in any other escaping was refused (RawPath set), and
// the go command escapes as net/url does (an escaped capital's '!' travels
// as %21).
func outboundURL(base *url.URL, path string) *url.URL {
	return &url.URL{Scheme: base.Scheme, Host: base.Host, Path: strings.TrimSuffix(base.Path, "/") + "/" + path}
}

// forward sends req to its fixed upstream and streams the answer back:
// only 200, 404 and 410 pass through, only the allowlisted headers are
// copied, and the body is capped for its kind.
func (s *Server) forward(w *httpguard.CountingWriter, r *http.Request, req Request, e *auditEntry) {
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Limits.RequestTimeout)
	defer cancel()
	base := s.cfg.GoProxy
	if req.Route == RouteSumDB {
		base = s.cfg.SumDB
	}
	u := outboundURL(base, req.Upstream)
	out, err := http.NewRequestWithContext(ctx, r.Method, base.String(), http.NoBody)
	if err != nil {
		refuse(w, e, http.StatusBadGateway, OutcomeUpstreamError, "request", "egress-proxy: cannot build the request")
		return
	}
	out.URL, out.Host = u, u.Host
	out.Header = http.Header{
		"User-Agent":      {s.ua},
		"Accept-Encoding": {"identity"},
	}
	resp, err := s.client.Do(out)
	if err != nil {
		reason := "unreachable"
		switch {
		case errors.Is(err, ErrDeniedAddress):
			reason = ReasonDeniedAddress
		case errors.Is(err, context.DeadlineExceeded):
			reason = "timeout"
		}
		refuse(w, e, http.StatusBadGateway, OutcomeUpstreamError, reason,
			"egress-proxy: upstream unreachable ("+reason+")")
		return
	}
	defer func() { _ = resp.Body.Close() }()
	e.upstreamStatus = resp.StatusCode
	switch resp.StatusCode {
	case http.StatusOK, http.StatusNotFound, http.StatusGone:
	default:
		refuse(w, e, http.StatusBadGateway, OutcomeUpstreamError, "status",
			"egress-proxy: upstream answered "+strconv.Itoa(resp.StatusCode))
		return
	}
	limit := s.cfg.Limits.bodyCap(req)
	if resp.ContentLength > limit {
		refuse(w, e, http.StatusBadGateway, OutcomeTooLarge, "content_length",
			"egress-proxy: upstream response exceeds the "+strconv.FormatInt(limit, 10)+"-byte cap")
		return
	}
	h := w.Header()
	for _, k := range copiedHeaders {
		if v := resp.Header.Get(k); v != "" {
			h.Set(k, v)
		}
	}
	if resp.ContentLength >= 0 {
		h.Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	}
	// The context's deadline only reaches the upstream side: a client that
	// stops reading blocks the copy in a write to its connection, holding
	// this request's limit slots, until a write deadline ends it. The same
	// absolute deadline bounds both sides. An unsupported writer (a test
	// recorder) has no connection to block on.
	if dl, ok := ctx.Deadline(); ok {
		if err := http.NewResponseController(w).SetWriteDeadline(dl); err != nil &&
			!errors.Is(err, http.ErrNotSupported) {
			refuse(w, e, http.StatusInternalServerError, OutcomeUpstreamError, "write_deadline",
				"egress-proxy: cannot bound the response")
			return
		}
	}
	e.outcome = OutcomeProxied
	w.WriteHeader(resp.StatusCode)
	if r.Method == http.MethodHead {
		return
	}
	s.stream(w, resp.Body, limit, e)
}

// stream copies at most limit bytes of body. A body past the cap, or one
// that fails mid-stream, aborts the connection, so the client sees a
// truncated transfer and the go command's own hash check fails.
func (s *Server) stream(w io.Writer, body io.Reader, limit int64, e *auditEntry) {
	n, err := io.Copy(w, io.LimitReader(body, limit))
	if err != nil {
		e.outcome, e.reason = OutcomeUpstreamError, "stream"
		panic(http.ErrAbortHandler)
	}
	if n < limit {
		return
	}
	var one [1]byte
	if _, err := io.ReadFull(body, one[:]); err == nil {
		e.outcome, e.reason = OutcomeTooLarge, "stream"
		panic(http.ErrAbortHandler)
	}
}

// dialReason names why a registry request failed, for the audit line: the
// same reasons step 1's forward gives.
func dialReason(err error) string {
	switch {
	case errors.Is(err, ErrDeniedAddress):
		return ReasonDeniedAddress
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	return "unreachable"
}
