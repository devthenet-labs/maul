// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/bitwise-media-group/patchy/internal/netprofile"
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

	// The registry routes' outcomes (step 2). Only private_module can end
	// a /go line, and only on a profile holding a go grant.
	OutcomeNotGranted            = "not_granted"
	OutcomeUpstreamAuth          = "upstream_auth"
	OutcomeRegistryRefused       = "registry_refused"
	OutcomeCredentialUnavailable = "credential_unavailable"
	OutcomeCredentialEcho        = "credential_echo"
	OutcomeRedirectRefused       = "redirect_refused"
	OutcomeDeniedHost            = "denied_host"
	OutcomePrivateModule         = "private_module"
	OutcomeChecksumNotPublic     = "checksum_not_public"
	OutcomePrerelease            = "prerelease"
)

// The refusal reasons beside an outcome.
const (
	ReasonPrefix        = "prefix"
	ReasonVersion       = "version"
	ReasonDeniedAddress = "denied_address"
	ReasonNotFound      = "not_found"
	ReasonLocal         = "answered_locally"
	ReasonPrivateModule = "private_module"
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
	// reg is set on /r/ routes only: its attributes follow reason, so a
	// /go line keeps step 1's attribute list exactly.
	reg *registryAudit
}

// registryAudit is what a /r/ route's audit line adds.
type registryAudit struct {
	ecosystem  string
	registry   string
	pkg        string
	credential CredentialKind
	// links are counted on pip pages only.
	rewritten, dropped int
	redirects          int
	redirectHost       string
}

// audit emits the one line every request gets. Never a header and never a
// body: only the parsed request, the attribution and the result.
func audit(ctx context.Context, log *slog.Logger, e auditEntry) {
	msg := "proxied"
	if e.outcome != OutcomeProxied {
		msg = "refused"
	}
	attrs := []slog.Attr{
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
		slog.String("reason", e.reason),
	}
	if r := e.reg; r != nil {
		cred := r.credential
		if cred == "" {
			cred = CredentialNone
		}
		attrs = append(attrs, slog.String("ecosystem", r.ecosystem), slog.String("registry", r.registry))
		if r.ecosystem != netprofile.EcosystemGo {
			attrs = append(attrs, slog.String("package", clip(r.pkg)))
		}
		attrs = append(attrs, slog.String("credential", string(cred)))
		if r.ecosystem == netprofile.EcosystemPyPI {
			attrs = append(attrs, slog.Int("rewritten_links", r.rewritten), slog.Int("dropped_links", r.dropped))
		}
		attrs = append(attrs, slog.Int("redirects", r.redirects), slog.String("redirect_host", clip(r.redirectHost)))
	}
	log.LogAttrs(ctx, slog.LevelInfo, msg, attrs...)
}

// The tunnel's outcomes (step 3) beside the shared not_allowed,
// bad_request, limited and suspended.
const (
	OutcomeTunnelled     = "tunnelled"
	OutcomeIPLiteral     = "ip_literal"
	OutcomeBadHost       = "bad_host"
	OutcomeHardDenied    = "hard_denied"
	OutcomePort          = "port"
	OutcomeDeniedAddress = "denied_address"
	OutcomeDialError     = "dial_error"
	OutcomeBadHello      = "bad_hello"
	OutcomeSNIMissing    = "sni_missing"
	OutcomeSNIMismatch   = "sni_mismatch"
	OutcomeECH           = "ech"
	OutcomeUploadCap     = "upload_cap"
	OutcomeDownloadCap   = "download_cap"
	OutcomeIdleTimeout   = "idle_timeout"
	OutcomeMaxDuration   = "max_duration"
	OutcomeShutdown      = "shutdown"
)

// tunnelEntry is what one tunnel's audit lines carry.
type tunnelEntry struct {
	profile    string
	port       int
	sourceIP   string
	pod        PodInfo
	host       string // the CONNECT host: validated, or clipped and quoted
	sni        string
	alpn       []string
	upstreamIP string
	bytesUp    int64
	bytesDown  int64
	elapsed    time.Duration
	status     int
	outcome    string
	reason     string
	// forwarded is set once the hello went upstream: the close line then
	// reads "tunnel", whatever ended it.
	forwarded bool
}

// identity is the attribute list every tunnel line starts with.
func (e tunnelEntry) identity() []slog.Attr {
	return []slog.Attr{
		slog.String("profile", e.profile),
		slog.Int("port", e.port),
		slog.String("source_ip", clip(e.sourceIP)),
		slog.String("pod", clip(e.pod.Name)),
		slog.String("finding", clip(e.pod.Finding)),
		slog.String("owner", clip(e.pod.Owner)),
		slog.String("run_kind", clip(e.pod.RunKind)),
		slog.String("attempt", clip(e.pod.Attempt)),
		slog.String("repo", clip(e.pod.Repo)),
		slog.String("route", "tunnel"),
		slog.String("host", e.host),
	}
}

// auditTunnelOpened is the info line written with the 200, before any
// hello byte: it survives a crash that loses the close line.
func auditTunnelOpened(ctx context.Context, log *slog.Logger, e tunnelEntry) {
	log.LogAttrs(ctx, slog.LevelInfo, "tunnel opened", append(e.identity(),
		slog.String("upstream_ip", e.upstreamIP))...)
}

// auditTunnelProgress is an open tunnel's periodic line, with its running
// byte counts.
func auditTunnelProgress(ctx context.Context, log *slog.Logger, e tunnelEntry) {
	log.LogAttrs(ctx, slog.LevelInfo, "tunnel progress", append(e.identity(),
		slog.Int64("bytes_up", e.bytesUp),
		slog.Int64("bytes_down", e.bytesDown),
		slog.Duration("duration", e.elapsed))...)
}

// auditTunnel is the one close line every CONNECT gets: "tunnel" for a
// tunnel that carried the client's hello upstream (whatever ended it),
// "refused" for everything refused before that. Never a header and never a
// payload byte.
func auditTunnel(ctx context.Context, log *slog.Logger, e tunnelEntry) {
	msg := "refused"
	if e.forwarded {
		msg = "tunnel"
	}
	attrs := append(e.identity(),
		slog.String("sni", clip(e.sni)),
		slog.String("alpn", clip(strings.Join(e.alpn, ","))),
		slog.String("upstream_ip", e.upstreamIP),
		slog.Int64("bytes_up", e.bytesUp),
		slog.Int64("bytes_down", e.bytesDown),
		slog.Duration("duration", e.elapsed),
		slog.Int("status", e.status),
		slog.String("outcome", e.outcome),
		slog.String("reason", e.reason),
	)
	log.LogAttrs(ctx, slog.LevelInfo, msg, attrs...)
}
