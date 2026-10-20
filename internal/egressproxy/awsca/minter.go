// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package awsca

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/codeartifact"
	"github.com/aws/smithy-go"

	"github.com/devthenet-labs/maul/internal/egressproxy"
	"github.com/devthenet-labs/maul/internal/netprofile"
)

// Scheduling constants.
const (
	// RefreshMargin: refresh at least this long before expiry, when that
	// is earlier than the half-life.
	RefreshMargin = 15 * time.Minute
	// JitterFraction spreads refreshes by ±10%.
	JitterFraction = 0.1
	// BackoffMin and BackoffMax bound the retry delay after a failure.
	BackoffMin = 30 * time.Second
	BackoffMax = 5 * time.Minute
	// MinRemint is the shortest gap between two Invalidate-driven mints of
	// one registry.
	MinRemint = time.Minute
	// mintTimeout bounds one GetAuthorizationToken call.
	mintTimeout = time.Minute
)

// The refresh results OnRefresh reports (the refreshes metric's result).
const (
	ResultMinted     = "minted"
	ResultMintFailed = "mint_failed"
)

// API is the one CodeArtifact operation the minter calls; the SDK client
// satisfies it, and tests fake it.
type API interface {
	GetAuthorizationToken(ctx context.Context, in *codeartifact.GetAuthorizationTokenInput,
		optFns ...func(*codeartifact.Options)) (*codeartifact.GetAuthorizationTokenOutput, error)
}

// Registry is one codeartifact registry.
type Registry struct {
	Name string
	// Ecosystem is npm or pypi: it decides the login's form.
	Ecosystem string
	// Domain and DomainOwner name the CodeArtifact domain; Region is its
	// region (the API client's).
	Domain, DomainOwner, Region string
	// Duration is the lifetime asked for; 0 is the default (12h).
	Duration time.Duration
}

// FromNetprofile is the minter's view of a validated codeartifact
// registry.
func FromNetprofile(r netprofile.Registry) (Registry, error) {
	ca := r.Auth.CodeArtifact
	if r.Auth.Type != netprofile.AuthCodeArtifact || ca == nil {
		return Registry{}, fmt.Errorf("registry %s is not a codeartifact registry", r.Name)
	}
	return Registry{Name: r.Name, Ecosystem: r.Ecosystem, Domain: ca.Domain, DomainOwner: ca.DomainOwner,
		Region: ca.Region, Duration: time.Duration(ca.Duration()) * time.Second}, nil
}

// Options tune a Minter. Every field has a production default.
type Options struct {
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// After is the timer; nil is time.After.
	After func(time.Duration) <-chan time.Time
	// Jitter returns a value in [-1, 1); nil is uniform random.
	Jitter func() float64
	// Log receives the lifecycle lines; nil discards them.
	Log *slog.Logger
	// OnRefresh is called once per mint outcome; nil is no-op.
	OnRefresh func(registry, result string)
	// AddressClass is the class of the endpoint address last dialled, for
	// credential_mint_failed; nil reports ClassNone.
	AddressClass func() string
}

// entry is one registry's minting state.
type entry struct {
	reg  Registry
	api  API
	wake chan struct{}

	mu       sync.Mutex
	cred     egressproxy.Credential
	minted   bool
	lastWake time.Time
}

// Minter mints and serves CodeArtifact tokens; it is a
// egressproxy.CredentialSource.
type Minter struct {
	opts    Options
	entries map[string]*entry
}

var _ egressproxy.CredentialSource = (*Minter)(nil)

// New builds a Minter for the registries; apiFor returns the CodeArtifact
// client for a region (one per region is enough). Nothing is minted until
// Run.
func New(regs []Registry, apiFor func(region string) (API, error), opts Options) (*Minter, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.After == nil {
		opts.After = time.After
	}
	if opts.Jitter == nil {
		opts.Jitter = func() float64 { return 2*rand.Float64() - 1 }
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	if opts.OnRefresh == nil {
		opts.OnRefresh = func(string, string) {}
	}
	if opts.AddressClass == nil {
		opts.AddressClass = func() string { return ClassNone }
	}
	m := &Minter{opts: opts, entries: map[string]*entry{}}
	apis := map[string]API{}
	for _, r := range regs {
		if err := netprofile.ValidName(r.Name); err != nil {
			return nil, fmt.Errorf("codeartifact registry: %w", err)
		}
		if r.Ecosystem != netprofile.EcosystemNPM && r.Ecosystem != netprofile.EcosystemPyPI {
			return nil, fmt.Errorf("registry %s: codeartifact serves npm and pypi, not %q", r.Name, r.Ecosystem)
		}
		if r.Domain == "" || r.DomainOwner == "" || r.Region == "" {
			return nil, fmt.Errorf("registry %s: domain, domainOwner and region are required", r.Name)
		}
		if _, dup := m.entries[r.Name]; dup {
			return nil, fmt.Errorf("registry %s is listed twice", r.Name)
		}
		if r.Duration == 0 {
			r.Duration = netprofile.DefaultCodeArtifactDuration * time.Second
		}
		api, ok := apis[r.Region]
		if !ok {
			var err error
			if api, err = apiFor(r.Region); err != nil {
				return nil, fmt.Errorf("registry %s: %w", r.Name, err)
			}
			apis[r.Region] = api
		}
		m.entries[r.Name] = &entry{reg: r, api: api, wake: make(chan struct{}, 1)}
	}
	return m, nil
}

// Get returns the registry's current token, or ErrCredentialUnavailable
// before the first mint and once the token has expired.
func (m *Minter) Get(registry string) (egressproxy.Credential, error) {
	e, ok := m.entries[registry]
	if !ok {
		return egressproxy.Credential{}, fmt.Errorf("%w: registry %s is not a codeartifact registry",
			egressproxy.ErrCredentialUnavailable, registry)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.minted {
		return egressproxy.Credential{}, fmt.Errorf("%w: registry %s has not minted a token yet",
			egressproxy.ErrCredentialUnavailable, registry)
	}
	if !e.cred.Usable(m.opts.Now()) {
		return egressproxy.Credential{}, fmt.Errorf("%w: registry %s's token expired at %s",
			egressproxy.ErrCredentialUnavailable, registry, e.cred.ExpiresAt().UTC().Format(time.RFC3339))
	}
	return e.cred, nil
}

// ExpiresAt is when the registry's current token expires (the expiry
// gauge); false before the first mint.
func (m *Minter) ExpiresAt(registry string) (time.Time, bool) {
	e, ok := m.entries[registry]
	if !ok {
		return time.Time{}, false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cred.ExpiresAt(), e.minted
}

// Invalidate asks for an early mint after the registry answered 401, at
// most once per MinRemint per registry.
func (m *Minter) Invalidate(registry string) {
	e, ok := m.entries[registry]
	if !ok {
		return
	}
	now := m.opts.Now()
	e.mu.Lock()
	if !e.lastWake.IsZero() && now.Sub(e.lastWake) < MinRemint {
		e.mu.Unlock()
		return
	}
	e.lastWake = now
	e.mu.Unlock()
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// Run mints every registry in the background until ctx ends: the first
// mint at once, then on RefreshDelay's schedule, backing off on failure.
func (m *Minter) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	for _, e := range m.entries {
		wg.Go(func() { m.loop(ctx, e) })
	}
	wg.Wait()
	return nil
}

// loop is one registry's mint schedule.
func (m *Minter) loop(ctx context.Context, e *entry) {
	failures := 0
	for {
		var delay time.Duration
		issued := m.opts.Now()
		cred, err := m.mint(ctx, e)
		switch {
		case ctx.Err() != nil:
			return
		case err == nil:
			failures = 0
			e.mu.Lock()
			e.cred, e.minted = cred, true
			e.mu.Unlock()
			delay = RefreshDelay(cred.ExpiresAt().Sub(issued), m.opts.Jitter())
			m.opts.Log.Info("credential_minted", "registry", e.reg.Name, "credential", "codeartifact",
				"expires_at", cred.ExpiresAt().UTC().Format(time.RFC3339), "refresh_in", delay.Round(time.Second))
			m.opts.OnRefresh(e.reg.Name, ResultMinted)
		default:
			failures++
			delay = Backoff(failures)
			m.opts.Log.Warn("credential_mint_failed", "registry", e.reg.Name, "credential", "codeartifact",
				"code", ErrorCode(err), "endpoint_class", m.opts.AddressClass(), "retry_in", delay)
			m.opts.OnRefresh(e.reg.Name, ResultMintFailed)
		}
		select {
		case <-ctx.Done():
			return
		case <-m.opts.After(delay):
		case <-e.wake:
		}
	}
}

// mint calls GetAuthorizationToken once.
func (m *Minter) mint(ctx context.Context, e *entry) (egressproxy.Credential, error) {
	ctx, cancel := context.WithTimeout(ctx, mintTimeout)
	defer cancel()
	out, err := e.api.GetAuthorizationToken(ctx, &codeartifact.GetAuthorizationTokenInput{
		Domain:          aws.String(e.reg.Domain),
		DomainOwner:     aws.String(e.reg.DomainOwner),
		DurationSeconds: aws.Int64(int64(e.reg.Duration / time.Second)),
	})
	if err != nil {
		return egressproxy.Credential{}, err
	}
	if out == nil || out.AuthorizationToken == nil || *out.AuthorizationToken == "" {
		return egressproxy.Credential{}, errEmptyToken
	}
	expires := m.opts.Now().Add(e.reg.Duration)
	if out.Expiration != nil {
		expires = *out.Expiration
	}
	cred, err := egressproxy.CodeArtifactCredential(e.reg.Ecosystem, *out.AuthorizationToken, expires)
	if err != nil {
		return egressproxy.Credential{}, fmt.Errorf("%w: %w", errInvalidToken, err)
	}
	return cred, nil
}

// errEmptyToken and errInvalidToken are answers without a usable token.
var (
	errEmptyToken   = errors.New("codeartifact answered without a token")
	errInvalidToken = errors.New("codeartifact answered a token that cannot be sent")
)

// RefreshDelay is how long after minting a token of the given lifetime to
// refresh it: half the lifetime, or RefreshMargin before expiry if that is
// earlier, but never before a quarter of the lifetime (so a 15-minute token
// is not refreshed the moment it arrives), then spread by jitter (in
// [-1, 1)) times JitterFraction. A non-positive lifetime retries after
// BackoffMin.
func RefreshDelay(lifetime time.Duration, jitter float64) time.Duration {
	if lifetime <= 0 {
		return BackoffMin
	}
	d := min(lifetime/2, lifetime-RefreshMargin)
	d = max(d, lifetime/4)
	return time.Duration(float64(d) * (1 + JitterFraction*jitter))
}

// Backoff is the retry delay after the n-th consecutive failure (n >= 1):
// BackoffMin doubling to BackoffMax.
func Backoff(n int) time.Duration {
	d := BackoffMin
	for i := 1; i < n && d < BackoffMax; i++ {
		d *= 2
	}
	return min(d, BackoffMax)
}

// ErrorCode is what credential_mint_failed logs of an error: the AWS
// error code, or a fixed word for a refused dial, a timeout, a missing
// token and anything else. Never the message, which can carry request
// detail.
func ErrorCode(err error) string {
	var api smithy.APIError
	switch {
	case errors.As(err, &api):
		return api.ErrorCode()
	case errors.Is(err, egressproxy.ErrDeniedAddress):
		return "DeniedAddress"
	case errors.Is(err, context.DeadlineExceeded):
		return "Timeout"
	case errors.Is(err, errEmptyToken):
		return "EmptyToken"
	case errors.Is(err, errInvalidToken):
		return "InvalidToken"
	}
	return "RequestFailure"
}
