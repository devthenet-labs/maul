// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package broker

import (
	"log/slog"
	"net/http"
	"time"
)

// auditEntry is what one request's audit line carries.
type auditEntry struct {
	pod     string
	route   string
	model   string
	reason  string // the rejection reason; "" when proxied
	status  int
	bytes   int64
	elapsed time.Duration
	// tokens is what this request charged the pod; estimated marks a
	// charge that came (in part) from the request-size estimate because
	// the response's usage was never seen.
	tokens    int64
	estimated bool
	// cost is what this request charged the pod in micro-USD, priced
	// whatever the limit unit.
	cost int64
	// totals is the pod's running count after this request, so operators
	// can size the limits from observed runs.
	totals podTotals
	// costLimit is the pod's cost cap in micro-USD (the smaller of
	// --cost-per-pod and its budget annotation); 0 unless the cost limits
	// are enforced.
	costLimit int64
	// creds is whether per-user credentials are on; only then does the
	// line carry user and credential.
	creds bool
	// user is the run's attributed person ("github-id:<id>", "org", or why
	// there is none); credential the paying credential's fingerprint, or
	// "org".
	user, credential string
}

// audit emits the one slog line every request gets: caller pod, route,
// method, path, status, duration, bytes, the model named, the tokens and
// micro-USD charged and the pod's running totals, and under per-user
// credentials the attributed person and the paying credential's
// fingerprint. Never bodies, never headers, never a credential — model
// prompts and completions must not land in the broker's logs.
func audit(log *slog.Logger, r *http.Request, e auditEntry) {
	msg := "proxied"
	if e.reason != "" {
		msg = "rejected"
	}
	attrs := []slog.Attr{
		slog.String("pod", e.pod),
		slog.String("route", e.route),
		slog.String("method", r.Method),
		slog.String("path", clip(r.URL.Path)),
		slog.Int("status", e.status),
		slog.Duration("duration", e.elapsed),
		slog.Int64("bytes", e.bytes),
		slog.String("model", clip(e.model)),
		slog.String("reason", e.reason),
		slog.Int64("tokens", e.tokens),
		slog.Bool("estimated", e.estimated),
		slog.Int64("pod_requests", e.totals.requests),
		slog.Int64("pod_tokens", e.totals.tokens),
		slog.Int64("cost_micro_usd", e.cost),
		slog.Int64("pod_cost_micro_usd", e.totals.cost),
		slog.Int64("pod_cost_limit_micro_usd", e.costLimit),
	}
	if e.creds {
		attrs = append(attrs, slog.String("user", e.user), slog.String("credential", e.credential))
	}
	log.LogAttrs(r.Context(), slog.LevelInfo, msg, attrs...)
}
