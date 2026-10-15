// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy

import (
	"context"
	"log/slog"
	"time"
)

// The outcomes a request ends in: the audit line's outcome and the
// requests counter's attribute.
const (
	OutcomeProxied       = "proxied"
	OutcomeNotAllowed    = "not_allowed"
	OutcomeBadRequest    = "bad_request"
	OutcomeBadPath       = "bad_path"
	OutcomeMethod        = "method"
	OutcomeLimited       = "limited"
	OutcomeUpstreamError = "upstream_error"
	OutcomeTooLarge      = "too_large"
	OutcomeSuspended     = "suspended"
)

// The refusal reasons beside an outcome.
const (
	ReasonPrefix        = "prefix"
	ReasonVersion       = "version"
	ReasonDeniedAddress = "denied_address"
	ReasonNotFound      = "not_found"
	ReasonLocal         = "answered_locally"
)

// maxEcho bounds a caller-controlled string before it reaches an audit
// line.
const maxEcho = 256

// clip truncates s to maxEcho bytes, marking the cut.
func clip(s string) string {
	if len(s) <= maxEcho {
		return s
	}
	return s[:maxEcho] + "...(truncated)"
}

// auditEntry is what one request's audit line carries.
type auditEntry struct {
	profile        string
	port           int
	sourceIP       string
	pod            PodInfo
	route          string
	kind           string
	module         string
	version        string
	method         string
	status         int
	upstreamStatus int
	bytes          int64
	elapsed        time.Duration
	outcome        string
	reason         string
}

// audit emits the one line every request gets. Never a header and never a
// body: only the parsed request, the attribution and the result.
func audit(ctx context.Context, log *slog.Logger, e auditEntry) {
	msg := "proxied"
	if e.outcome != OutcomeProxied {
		msg = "refused"
	}
	log.LogAttrs(ctx, slog.LevelInfo, msg,
		slog.String("profile", e.profile),
		slog.Int("port", e.port),
		slog.String("source_ip", clip(e.sourceIP)),
		slog.String("pod", clip(e.pod.Name)),
		slog.String("finding", clip(e.pod.Finding)),
		slog.String("owner", clip(e.pod.Owner)),
		slog.String("run_kind", clip(e.pod.RunKind)),
		slog.String("attempt", clip(e.pod.Attempt)),
		slog.String("repo", clip(e.pod.Repo)),
		slog.String("route", e.route),
		slog.String("kind", e.kind),
		slog.String("module", clip(e.module)),
		slog.String("version", clip(e.version)),
		slog.String("method", clip(e.method)),
		slog.Int("status", e.status),
		slog.Int("upstream_status", e.upstreamStatus),
		slog.Int64("bytes", e.bytes),
		slog.Duration("duration", e.elapsed),
		slog.String("outcome", e.outcome),
		slog.String("reason", e.reason))
}
