// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/previewauth"
)

// The relay's HTTP client and readiness wait, from slice 1's tester
// (internal/previewtester client.go and readiness.go, which stay there
// until the tester is deleted in S4): plain HTTP to a component's Service
// address, never through a proxy, never following a redirect, no cookie
// jar, no compression, a fresh connection each time, a bounded body.

// RequestTimeout bounds one request, response body included.
const RequestTimeout = 10 * time.Second

// MaxResponseBytes bounds a response body the relay reads.
const MaxResponseBytes = acceptance.MaxResponseBytes

// maxResponseHeaderBytes bounds a response's headers.
const maxResponseHeaderBytes = 32 << 10

// DialFunc dials a network address, as net.Dialer.DialContext does.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// ClientOptions configure a Client. The zero value is the released
// relay's.
type ClientOptions struct {
	// Dial replaces the network dialer (tests and the e2e build map
	// Service addresses to local listeners). nil dials the address itself.
	Dial DialFunc
	// Timeout replaces RequestTimeout when positive.
	Timeout time.Duration
	// ReadinessBackoff replaces the readiness wait's first backoff when
	// positive (tests).
	ReadinessBackoff time.Duration
}

// Client sends the relay's requests to the components.
type Client struct {
	http    *http.Client
	dial    DialFunc
	backoff time.Duration
}

// NewClient builds a Client.
func NewClient(o ClientOptions) *Client {
	timeout := RequestTimeout
	if o.Timeout > 0 {
		timeout = o.Timeout
	}
	dial := o.Dial
	if dial == nil {
		dial = (&net.Dialer{Timeout: timeout}).DialContext
	}
	transport := &http.Transport{
		Proxy:                  nil, // never the environment's proxy
		DialContext:            dial,
		DisableKeepAlives:      true,
		DisableCompression:     true,
		TLSNextProto:           map[string]func(string, *tls.Conn) http.RoundTripper{},
		MaxResponseHeaderBytes: maxResponseHeaderBytes,
		ResponseHeaderTimeout:  timeout,
	}
	c := &Client{
		http: &http.Client{
			Transport:     transport,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Timeout:       timeout,
		},
		dial:    dial,
		backoff: readinessBackoffMin,
	}
	if o.ReadinessBackoff > 0 {
		c.backoff = max(o.ReadinessBackoff, time.Millisecond)
	}
	return c
}

// outbound is one request to a component: the address is the config's,
// the target (escaped path and query) the caller's.
type outbound struct {
	Addr    string
	Method  string
	Target  string
	Header  map[string]string
	Body    []byte
	Host    string
	Forward previewauth.ForwardedFor
}

// reply is what came back: the response, or how no response came.
type reply struct {
	Status    int
	Header    http.Header
	Body      []byte
	TooLarge  bool
	Transport acceptance.Transport
	LatencyMs int64
}

// errTarget is a target that does not survive URL encoding as given.
var errTarget = errors.New("request target is not a path the relay sends as given")

// send sends o. The request carries the gateway's header set
// (previewauth.CleanRequest), so the application sees the evaluator as it
// sees any viewer behind the gateway.
func (c *Client) send(ctx context.Context, o outbound) (reply, error) {
	u, err := url.ParseRequestURI(o.Target)
	if err != nil || u.Scheme != "" || u.Host != "" || u.RequestURI() != o.Target {
		return reply{}, errTarget
	}
	u.Scheme, u.Host = "http", o.Addr
	var body io.Reader
	if o.Body != nil {
		body = bytes.NewReader(o.Body)
	}
	req, err := http.NewRequestWithContext(ctx, o.Method, u.String(), body)
	if err != nil || req.URL.Host != o.Addr || req.URL.RequestURI() != o.Target {
		return reply{}, errTarget
	}
	for name, v := range o.Header {
		req.Header.Set(name, v)
	}
	previewauth.CleanRequest(req.Header, o.Forward)
	if o.Host != "" {
		req.Host = o.Host
	}
	start := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		return reply{Transport: classify(err), LatencyMs: time.Since(start).Milliseconds()}, nil
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	out := reply{Status: resp.StatusCode, Header: resp.Header, Body: b, Transport: acceptance.TransportNone,
		LatencyMs: time.Since(start).Milliseconds()}
	switch {
	case err != nil:
		out.Body, out.Transport = nil, classify(err)
	case len(b) > MaxResponseBytes:
		out.Body, out.TooLarge = b[:MaxResponseBytes], true
	}
	return out, nil
}

// classify names how a request failed without a response.
func classify(err error) acceptance.Transport {
	var dnsErr *net.DNSError
	var netErr net.Error
	var opErr *net.OpError
	var recErr tls.RecordHeaderError
	switch {
	case errors.As(err, &dnsErr):
		return acceptance.TransportDNS
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return acceptance.TransportTimeout
	case errors.Is(err, syscall.ECONNREFUSED), errors.As(err, &opErr) && opErr.Op == "dial":
		return acceptance.TransportRefused
	case errors.As(err, &recErr):
		return acceptance.TransportTLS
	default:
		return acceptance.TransportReset
	}
}

// The readiness wait (moved from the tester, whose comment says why): the
// tool pod is new on every evaluation and its slot policy may not be
// programmed for it yet, which a client sees as a refused connection. A
// component still unreachable after the window is the application's
// failure, never maul's Infra.
const (
	// ReadinessWindowMax bounds the readiness wait.
	ReadinessWindowMax = 60 * time.Second
	// readinessBudgetShare keeps the window at most a quarter of the
	// evaluation's wall clock.
	readinessBudgetShare = 4
	readinessBackoffMin  = 100 * time.Millisecond
	readinessBackoffMax  = time.Second
	readinessDialTimeout = 3 * time.Second
)

// ReadinessWindow is the readiness wait's window for an evaluation of the
// given wall clock: a quarter of it, at most ReadinessWindowMax.
func ReadinessWindow(budget time.Duration) time.Duration {
	return min(budget/readinessBudgetShare, ReadinessWindowMax)
}

// awaitTargets waits, concurrently and within window, for each target
// (component key to address) to accept a TCP connection, which it closes at
// once. It returns one wait per target, in key order.
func (c *Client) awaitTargets(ctx context.Context, targets map[string]string,
	window time.Duration) []acceptance.TargetWait {
	ctx, cancel := context.WithTimeout(ctx, window)
	defer cancel()
	keys := slices.Sorted(maps.Keys(targets))
	waits := make([]acceptance.TargetWait, len(keys))
	var wg sync.WaitGroup
	for i, key := range keys {
		wg.Go(func() { waits[i] = c.awaitTarget(ctx, key, targets[key]) })
	}
	wg.Wait()
	return waits
}

func (c *Client) awaitTarget(ctx context.Context, key, addr string) acceptance.TargetWait {
	w := acceptance.TargetWait{Target: key}
	start := time.Now()
	backoff := c.backoff
	for {
		w.Attempts++
		dctx, cancel := context.WithTimeout(ctx, readinessDialTimeout)
		conn, err := c.dial(dctx, "tcp", addr)
		cancel()
		if err == nil {
			_ = conn.Close()
			break
		}
		w.Record(classify(err))
		if w.Attempts >= acceptance.MaxWaitAttempts || !sleep(ctx, backoff) {
			break
		}
		backoff = min(backoff*2, readinessBackoffMax)
	}
	w.WaitMs = int32(min(time.Since(start).Milliseconds(), acceptance.MaxWaitMs))
	return w
}

// sleep waits d, reporting false when ctx ends first.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
