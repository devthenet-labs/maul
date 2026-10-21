// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package broker

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/devthenet-labs/maul/internal/httpguard"
	"github.com/devthenet-labs/maul/internal/usagewire"
)

// UsageHandler serves the usage endpoint (usagewire), meant for its own
// listener, which the chart's NetworkPolicy opens to the run controllers
// only; nil while Config.Usage is off. It answers GET
// usagewire.PathPrefix+"<job>" with the ledger entries of that Job's pods,
// to a caller presenting a projected token for the usage audience that
// TokenReview resolves to one of Config.Usage.Callers. It reads the ledger
// and nothing else: the broker still writes nothing to Kubernetes.
func (s *Server) UsageHandler() http.Handler {
	if s.usageAuth == nil {
		return nil
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+usagewire.PathPrefix+"{job}", s.serveUsage)
	return mux
}

// serveUsage runs the same pre-authentication as the proxy (the source IP's
// bucket, then the token's shape), then the TokenReview, then answers.
func (s *Server) serveUsage(w http.ResponseWriter, r *http.Request) {
	status, caller := http.StatusOK, ""
	job := r.PathValue("job")
	defer func() {
		s.log.LogAttrs(r.Context(), slog.LevelInfo, "usage read",
			slog.String("caller", caller), slog.String("job", clip(job)), slog.Int("status", status))
	}()
	refuse := func(code int, msg string) {
		status = code
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
	}
	ip := httpguard.SourceIP(r)
	release, reason := s.ips.acquire(ip)
	if reason != "" {
		countPreauth(r.Context(), "source_rate")
		refuse(http.StatusTooManyRequests, "egress broker: "+reason)
		return
	}
	defer release()
	token, ok := bearerToken(r)
	if !ok {
		s.ips.penalize(ip)
		refuse(http.StatusUnauthorized, "missing bearer token")
		return
	}
	if err := tokenShape(token, s.usageAuth.audience, s.now()); err != nil {
		s.ips.penalize(ip)
		countPreauth(r.Context(), "token_shape")
		refuse(http.StatusUnauthorized, err.Error())
		return
	}
	id, err := s.usageAuth.authenticateToken(r, token)
	switch {
	case errors.Is(err, errReviewThrottled):
		w.Header().Set("Retry-After", strconv.Itoa(s.usageAuth.reviews.retryAfter()))
		refuse(http.StatusTooManyRequests, "egress broker: token review throttled; retry")
		return
	case errors.Is(err, errReviewUnavailable):
		refuse(http.StatusServiceUnavailable, "egress broker: token review unavailable")
		return
	case err != nil:
		s.ips.penalize(ip)
		refuse(http.StatusUnauthorized, err.Error())
		return
	}
	caller = id.user
	if !usagewire.ValidJobName(job) {
		refuse(http.StatusBadRequest, "not a job name")
		return
	}
	pods := s.ledger.jobUsage(job)
	if pods == nil {
		pods = []usagewire.Pod{}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(usagewire.Response{BootTime: s.boot.UTC(), Pods: pods})
}

// bearerToken reads "Authorization: Bearer <token>".
func bearerToken(r *http.Request) (string, bool) {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}
