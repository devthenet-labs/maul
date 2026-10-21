// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package hostprobe

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"

	"github.com/devthenet-labs/maul/internal/previewauth"
)

const scopeName = "github.com/devthenet-labs/maul/internal/previewauth/adapters/hostprobe"

// DefaultTimeout bounds one host's probe.
const DefaultTimeout = 10 * time.Second

// bodyLimit bounds what a probe reads of an answer (the gateway's pages are
// well under it).
const bodyLimit = 4096

// maxConcurrent bounds the probes in flight.
const maxConcurrent = 4

var (
	unprotectedGauge = sync.OnceValue(func() metric.Int64Gauge {
		g, err := otel.Meter(scopeName).Int64Gauge("maul.preview_auth.unprotected_hosts",
			metric.WithDescription("Ready preview hosts that answered an unauthenticated request without "+
				"redirecting to sign in for their own Preview (and, in gateway mode, a host no Preview owns that "+
				"answered other than with the gateway's 404)"))
		if err != nil {
			otel.Handle(err)
		}
		return g
	})
	unreachableGauge = sync.OnceValue(func() metric.Int64Gauge {
		g, err := otel.Meter(scopeName).Int64Gauge("maul.preview_auth.probe.unreachable_hosts",
			metric.WithDescription("Ready preview hosts the relay's probe could not reach, or the preview "+
				"gateway refused by its inbound allowlist"))
		if err != nil {
			otel.Handle(err)
		}
		return g
	})
)

// Lister lists the Ready Previews to probe.
type Lister interface {
	Ready(ctx context.Context) ([]previewauth.View, error)
}

// Result is one pass's outcome.
type Result struct {
	Probed, Unprotected, Unreachable int
	// UnprotectedLabels lists the unprotected hosts' labels.
	UnprotectedLabels []string
}

// Prober probes preview hosts.
type Prober struct {
	Lister    Lister
	Callbacks previewauth.Callbacks
	Issuer    string
	Interval  time.Duration
	Timeout   time.Duration
	// Transport, when set, carries the probes (tests); otherwise a clone of
	// http.DefaultTransport.
	Transport http.RoundTripper
	Log       *slog.Logger
	// Gateway, when set (the preview gateway is on), is the gateway's host
	// grammar, for its redirect to its sign-in host's /start.
	Gateway *previewauth.Hosts
	// Mode is the path expected to answer (previewauth.ProbeModeFor). Empty
	// derives it from Gateway: ProbeEither with it, ProbeALB without. Under
	// ProbeGateway every pass also asks for previewauth.UnknownProbeLabel,
	// which must be the gateway's own 404.
	Mode previewauth.ProbeMode
}

// expect is what a live host must answer with.
func (p *Prober) expect() previewauth.ProbeExpect {
	e := previewauth.ProbeExpect{Mode: p.Mode, Issuer: p.Issuer, Callbacks: p.Callbacks}
	if p.Gateway != nil {
		e.Gateway = *p.Gateway
	}
	if e.Mode == "" {
		e.Mode = previewauth.ProbeModeFor(p.Gateway != nil, true)
	}
	return e
}

// NeedLeaderElection is false: every replica probes; nothing is written.
func (p *Prober) NeedLeaderElection() bool { return false }

// Start probes every Interval until ctx ends. A non-positive Interval
// disables probing.
func (p *Prober) Start(ctx context.Context) error {
	if p.Interval <= 0 {
		return nil
	}
	t := time.NewTicker(p.Interval)
	defer t.Stop()
	for {
		p.Once(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func (p *Prober) client() *http.Client {
	rt := p.Transport
	if rt == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.DisableKeepAlives = true
		rt = tr
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &http.Client{
		Transport: rt, Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Once runs one pass and records its gauges. A failed listing records
// nothing (the gauges keep their last values) and is logged.
func (p *Prober) Once(ctx context.Context) Result {
	views, err := p.Lister.Ready(ctx)
	if err != nil {
		p.log().LogAttrs(ctx, slog.LevelWarn, "preview host probe: list previews", slog.Any("error", err))
		return Result{}
	}
	c := p.client()
	expect := p.expect()
	if expect.Mode == previewauth.ProbeGateway {
		// The host no Preview owns, judged like any other: its label is
		// never a live one, so it cannot be mistaken for one below.
		views = append(views, previewauth.View{Label: previewauth.UnknownProbeLabel, Slot: -1})
	}
	var (
		mu  sync.Mutex
		res Result
		wg  sync.WaitGroup
		sem = make(chan struct{}, maxConcurrent)
	)
	for _, v := range views {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			verdict := p.probe(ctx, c, expect, v)
			mu.Lock()
			defer mu.Unlock()
			res.Probed++
			switch verdict {
			case errUnreachable:
				res.Unreachable++
			case nil:
			default:
				res.Unprotected++
				res.UnprotectedLabels = append(res.UnprotectedLabels, v.Label)
			}
		}()
	}
	wg.Wait()
	unprotectedGauge().Record(ctx, int64(res.Unprotected))
	unreachableGauge().Record(ctx, int64(res.Unreachable))
	return res
}

var errUnreachable = errors.New("unreachable")

func (p *Prober) probe(ctx context.Context, c *http.Client, expect previewauth.ProbeExpect,
	v previewauth.View) error {
	host := v.Label + "." + p.Callbacks.Suffix()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+"/", nil)
	if err != nil {
		return errUnreachable
	}
	req.Header.Set("Accept", "text/html")
	req.Header.Set("User-Agent", "maul-preview-auth-probe")
	resp, err := c.Do(req)
	if err != nil {
		p.log().LogAttrs(ctx, slog.LevelDebug, "preview host probe: unreachable",
			slog.String("label", v.Label), slog.Any("error", err))
		return errUnreachable
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, bodyLimit))
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, bodyLimit))
	_ = resp.Body.Close()
	if p.Gateway != nil && previewauth.IsAllowlistRefusal(resp.StatusCode, resp.Header, body) {
		// The gateway refuses this replica's egress address by its inbound
		// allowlist before it routes the host: the allowlist working, not an
		// unprotected host, and nothing about the host can be judged.
		p.log().LogAttrs(ctx, slog.LevelDebug, "preview host probe: refused by the gateway's inbound allowlist",
			slog.String("label", v.Label))
		return errUnreachable
	}
	if v.Label == previewauth.UnknownProbeLabel {
		err = previewauth.JudgeGatewayUnknown(resp.StatusCode, resp.Header, body)
	} else {
		err = expect.Live(resp.StatusCode, resp.Header.Get("Location"), v.Slot, v.Label)
	}
	if err != nil {
		p.log().LogAttrs(ctx, slog.LevelWarn, "preview host is not protected by the sign-in relay",
			slog.String("label", v.Label), slog.String("slot", strconv.Itoa(v.Slot)),
			slog.Int("status", resp.StatusCode))
		return err
	}
	return nil
}

func (p *Prober) log() *slog.Logger {
	if p.Log != nil {
		return p.Log
	}
	return slog.New(slog.DiscardHandler)
}
