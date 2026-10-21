// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package broker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/devthenet-labs/maul/internal/httpguard"
)

// Pre-authentication runs ahead of the TokenReview so that a pod varying the
// token header degrades itself rather than the API server or every other
// brokered Job: a syntactic token check that needs no network, a per-source-IP
// token bucket and in-flight cap, and a global TokenReview limiter with a
// short queue. None of it is a security decision — TokenReview still decides
// who the caller is — it only bounds how much work an unauthenticated caller
// can cause.

// maxTokenBytes bounds the caller token the broker will even decode; a
// projected ServiceAccount token is a little over a kilobyte.
const maxTokenBytes = 16 << 10

// reviewQueueWait is how long a request waits for a TokenReview slot before
// it is refused; it is the "small queue" in front of the API server.
const reviewQueueWait = 2 * time.Second

// tokenShape rejects, without a TokenReview, a token that cannot be a
// pod-bound projected ServiceAccount token for this broker: three base64url
// segments, an aud containing the broker audience, an exp in the future and
// a kubernetes.io.pod claim. The signature is not checked here — that is the
// API server's job — so a passing token is merely worth reviewing.
func tokenShape(token, audience string, now time.Time) error {
	if token == "" {
		return fmt.Errorf("missing %s header", TokenHeader)
	}
	if len(token) > maxTokenBytes {
		return errors.New("token too large")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return errors.New("token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return errors.New("token payload is not base64url")
	}
	var claims struct {
		Aud json.RawMessage `json:"aud"`
		Exp int64           `json:"exp"`
		K8s struct {
			Pod *struct {
				Name string `json:"name"`
			} `json:"pod"`
		} `json:"kubernetes.io"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return errors.New("token payload is not JSON")
	}
	if !audienceContains(claims.Aud, audience) {
		return fmt.Errorf("token is not bound to audience %s", audience)
	}
	if claims.Exp == 0 || !time.Unix(claims.Exp, 0).After(now) {
		return errors.New("token is expired")
	}
	if claims.K8s.Pod == nil || claims.K8s.Pod.Name == "" {
		return errors.New("token is not pod-bound")
	}
	return nil
}

// audienceContains reports whether a JWT aud claim (a string or an array of
// strings) names audience.
func audienceContains(raw json.RawMessage, audience string) bool {
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		return one == audience
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err == nil {
		for _, a := range many {
			if a == audience {
				return true
			}
		}
	}
	return false
}

// ipLimiter is the broker's per-source-IP token bucket and in-flight cap:
// httpguard's limiter with the cap at the burst and no queue, so a source
// over either is refused at once. A nil *ipLimiter is disabled: acquire
// always succeeds.
type ipLimiter struct {
	guard *httpguard.IPLimiter
	// now is the clock the guard reads, through a closure, so a test can
	// freeze the broker's limiter after construction.
	now func() time.Time
}

// newIPLimiter returns nil when perSecond is not positive. An unset burst
// is one second of the rate (at least one), so a rate alone never
// serializes a pod's parallel requests behind an in-flight cap of one.
func newIPLimiter(perSecond float64, burst int, now func() time.Time) *ipLimiter {
	l := &ipLimiter{now: now}
	l.guard = httpguard.NewIPLimiter(httpguard.IPLimiterConfig{
		Rate: perSecond, Burst: burst, InFlight: burst, Wait: 0,
		Now: func() time.Time { return l.now() },
	})
	if l.guard == nil {
		return nil
	}
	return l
}

// acquire takes one token from ip's bucket and one in-flight slot (the cap
// is the burst), returning the release for the slot or the refusal reason.
// It never waits, so no context is consulted.
func (l *ipLimiter) acquire(ip string) (func(), string) {
	if l == nil {
		return func() {}, ""
	}
	return l.guard.Acquire(context.Background(), ip)
}

// penalize charges ip an extra token: a failed authentication costs twice
// what a successful one does, so a token-guessing loop starves faster.
func (l *ipLimiter) penalize(ip string) {
	if l == nil {
		return
	}
	l.guard.Charge(ip)
}

// reviewLimiter bounds TokenReview calls broker-wide. A nil *reviewLimiter
// is disabled.
type reviewLimiter struct {
	lim *rate.Limiter
}

// newReviewLimiter returns nil when perSecond is not positive. The burst is
// one second's worth, so a cold start does not spend the whole budget at
// once.
func newReviewLimiter(perSecond float64) *reviewLimiter {
	if perSecond <= 0 {
		return nil
	}
	return &reviewLimiter{lim: rate.NewLimiter(rate.Limit(perSecond), max(1, int(perSecond)))}
}

// retryAfter is the Retry-After, in whole seconds, for a caller refused a
// review slot: one slot's interval, at least a second.
func (l *reviewLimiter) retryAfter() int {
	if l == nil || l.lim.Limit() <= 0 {
		return 1
	}
	return max(1, int(math.Ceil(1/float64(l.lim.Limit()))))
}

// admit waits up to reviewQueueWait for a review slot.
func (l *reviewLimiter) admit(ctx context.Context) error {
	if l == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, reviewQueueWait)
	defer cancel()
	return l.lim.Wait(ctx)
}
