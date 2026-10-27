// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtester

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/devthenet-labs/maul/internal/acceptance"
)

// RequestTimeout bounds one request, response body included.
const RequestTimeout = 10 * time.Second

// RetryDelay spaces a criterion's second try from its failed first, so a
// transient blip does not fail both tries in the same instant. The run's
// budget bounds it like any request.
const RetryDelay = 1500 * time.Millisecond

// maxResponseHeaderBytes bounds a response's headers.
const maxResponseHeaderBytes = 32 << 10

// DialFunc dials a network address, as net.Dialer.DialContext does.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// Options configure a Client. The zero value is the released tester's.
type Options struct {
	// Dial replaces the network dialer (tests and the e2e build map
	// Service names to local listeners). nil dials the address itself.
	Dial DialFunc
	// Timeout replaces RequestTimeout when positive.
	Timeout time.Duration
	// RetryDelay replaces RetryDelay when positive; negative sends the
	// second try at once (tests).
	RetryDelay time.Duration
	// ReadinessBackoff replaces the readiness wait's first backoff when
	// positive (tests).
	ReadinessBackoff time.Duration
}

// Client sends the tester's requests: plain HTTP to one Service address at
// a time, never through a proxy, never following a redirect, without a
// cookie jar, without asking for compression, a fresh connection each time,
// the body read to at most acceptance.MaxResponseBytes.
type Client struct {
	http       *http.Client
	timeout    time.Duration
	dial       DialFunc
	retryDelay time.Duration
	backoff    time.Duration
}

// NewClient builds a Client.
func NewClient(o Options) *Client {
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
		ForceAttemptHTTP2:      false,
		TLSNextProto:           map[string]func(string, *tls.Conn) http.RoundTripper{},
		MaxResponseHeaderBytes: maxResponseHeaderBytes,
		ResponseHeaderTimeout:  timeout,
	}
	return &Client{
		http: &http.Client{
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Timeout: timeout,
		},
		timeout:    timeout,
		dial:       dial,
		retryDelay: positiveOr(o.RetryDelay, RetryDelay),
		backoff:    positiveOr(o.ReadinessBackoff, readinessBackoffMin),
	}
}

// positiveOr is d when positive, zero when negative, else def.
func positiveOr(d, def time.Duration) time.Duration {
	switch {
	case d > 0:
		return d
	case d < 0:
		return 0
	}
	return def
}

// Doer sends every request of a criterion to host ("<service>:<port>"),
// whatever the request says: the host is the spec's, never the criterion's.
// A second try starts RetryDelay after the first failed.
func (c *Client) Doer(ctx context.Context, host string) acceptance.Doer {
	return func(call acceptance.Call) acceptance.Response {
		if call.Try > 1 && call.Step == 0 {
			sleep(ctx, c.retryDelay)
		}
		return c.do(ctx, host, call.Request)
	}
}

func (c *Client) do(ctx context.Context, host string, b acceptance.BuiltRequest) acceptance.Response {
	if err := ctx.Err(); err != nil {
		// The run's budget is spent (or the run stopped): never sent.
		return acceptance.Response{Transport: classify(err)}
	}
	req, err := c.newRequest(ctx, host, b)
	if err != nil {
		// Unreachable for a request BuildRequest built; recorded as never
		// sent rather than sent differently from what was observed.
		return acceptance.Response{Transport: acceptance.TransportCaptureMissing}
	}
	start := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		return acceptance.Response{Transport: classify(err), LatencyMs: since(start)}
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, acceptance.MaxResponseBytes+1))
	out := acceptance.Response{
		Status: int32(resp.StatusCode), Header: resp.Header, Body: body, LatencyMs: since(start),
	}
	switch {
	case err != nil:
		out.Body, out.Transport = nil, classify(err)
	case len(body) > acceptance.MaxResponseBytes:
		out.Transport = acceptance.TransportTooLarge
	}
	return out
}

// newRequest builds the request structurally: scheme http, the spec's
// host, and the built request's escaped path and query exactly. A request
// whose target would differ from what Observe records is refused.
func (c *Client) newRequest(ctx context.Context, host string, b acceptance.BuiltRequest) (*http.Request, error) {
	path, err := url.PathUnescape(b.Path)
	if err != nil {
		return nil, err
	}
	target := b.Target()
	u := &url.URL{
		Scheme: "http", Host: host, Path: path, RawPath: b.Path,
		RawQuery: b.RawQuery, ForceQuery: strings.HasSuffix(target, "?"),
	}
	if u.RequestURI() != target {
		return nil, errors.New("request target does not survive URL encoding")
	}
	var body io.Reader
	if b.Body != nil {
		body = bytes.NewReader(b.Body)
	}
	req, err := http.NewRequestWithContext(ctx, b.Method, u.String(), body)
	if err != nil {
		return nil, err
	}
	if req.URL.Host != host || req.URL.RequestURI() != target {
		return nil, errors.New("request does not name the target as built")
	}
	for name, v := range b.Headers {
		req.Header.Set(name, v)
	}
	return req, nil
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

func since(start time.Time) int32 {
	return int32(min(time.Since(start).Milliseconds(), math.MaxInt32))
}
