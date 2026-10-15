// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/bitwise-media-group/patchy/internal/cli"
	"github.com/bitwise-media-group/patchy/internal/egressproxy"
	"github.com/bitwise-media-group/patchy/internal/egressproxy/awsca"
	"github.com/bitwise-media-group/patchy/internal/egressproxy/credfile"
	"github.com/bitwise-media-group/patchy/internal/netprofile"
)

// The registry mounts' defaults (the chart's volume paths).
const (
	defaultCredentialsDir = "/etc/patchy/registry-credentials"
	defaultCADir          = "/etc/patchy/registry-ca"
)

// registriesEnv is the registries' environment variable as the chart
// stamps it, read when neither --registries nor PATCHY_REGISTRIES is set.
const registriesEnv = "PATCHY_EGRESS_REGISTRIES"

// getenv reads the process environment; tests replace it.
var getenv = os.Getenv

// registrySettings is the validated registry half of the configuration.
type registrySettings struct {
	set              netprofile.RegistrySet
	alwaysDeny       []netip.Prefix
	cas              map[string]*x509.CertPool
	maxArtifactBytes int64
	pollInterval     time.Duration
}

// configureRegistries validates the registry flags against the profiles:
// the registries JSON (with its hosts held to the deny table, the profiles'
// forge hosts included), every grant against its registry, the
// always-denied ranges (--always-deny-cidrs and the Kubernetes Service IP),
// each CA bundle a registry names, the artifact cap and the poll interval.
func configureRegistries(opts *cli.Options, profiles netprofile.Set) (registrySettings, error) {
	always, err := egressproxy.ParseDenyCIDRs(opts.StringList("always-deny-cidrs"))
	if err != nil {
		return registrySettings{}, fmt.Errorf("--always-deny-cidrs: %w", err)
	}
	// kubelet injects the API Service's address into every container,
	// whatever automountServiceAccountToken says.
	if a, err := netip.ParseAddr(getenv("KUBERNETES_SERVICE_HOST")); err == nil && a.Is4() {
		always = append(always, netip.PrefixFrom(a, 32))
	}
	raw := opts.String("registries")
	if raw == "" {
		raw = getenv(registriesEnv)
	}
	set, err := netprofile.ParseRegistries([]byte(raw),
		netprofile.RegistryContext{DenyHosts: profiles.Hosts(), AlwaysDeny: always})
	if err != nil {
		return registrySettings{}, fmt.Errorf("--registries: %w", err)
	}
	if err := netprofile.CheckGrants(profiles, set); err != nil {
		return registrySettings{}, fmt.Errorf("--network-profiles: %w", err)
	}
	maxArtifact := int64(opts.Int("max-artifact-bytes"))
	if maxArtifact < 1 || maxArtifact > netprofile.MaxArtifactBytesLimit {
		return registrySettings{}, fmt.Errorf("--max-artifact-bytes %d is outside 1-%d", maxArtifact,
			netprofile.MaxArtifactBytesLimit)
	}
	poll := opts.Duration("credential-poll-interval")
	if poll < credfile.MinInterval {
		return registrySettings{}, fmt.Errorf("--credential-poll-interval %s is under %s", poll, credfile.MinInterval)
	}
	cas := map[string]*x509.CertPool{}
	for _, r := range set.Registries() {
		if !r.CA {
			continue
		}
		pool, err := loadCA(filepath.Join(opts.String("registry-ca-dir"), r.Name+".pem"))
		if err != nil {
			return registrySettings{}, fmt.Errorf("registry %s: %w", r.Name, err)
		}
		cas[r.Name] = pool
	}
	return registrySettings{set: set, alwaysDeny: always, cas: cas, maxArtifactBytes: maxArtifact,
		pollInterval: poll}, nil
}

// loadCA reads a PEM bundle into a pool of its own: a registry with a CA
// trusts nothing else.
func loadCA(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path) //nolint:gosec // the operator's mounted CA directory
	if err != nil {
		return nil, fmt.Errorf("CA bundle: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("CA bundle %s holds no PEM certificate", path)
	}
	return pool, nil
}

// sources is the proxy's CredentialSource: each registry's credential comes
// from the file source (bearer, basic) or the CodeArtifact minter.
type sources struct {
	files  *credfile.Source
	minter *awsca.Minter
	by     map[string]egressproxy.CredentialSource
	stop   func() error
	once   sync.Once
}

var _ egressproxy.CredentialSource = (*sources)(nil)

// Get is the registry's source's credential, or unavailable for a registry
// with none (a CodeArtifact registry whose AWS configuration failed).
func (s *sources) Get(registry string) (egressproxy.Credential, error) {
	src, ok := s.by[registry]
	if !ok {
		return egressproxy.Credential{}, fmt.Errorf("%w: registry %s has no credential source",
			egressproxy.ErrCredentialUnavailable, registry)
	}
	return src.Get(registry)
}

// Invalidate asks the registry's source to re-read or re-mint.
func (s *sources) Invalidate(registry string) {
	if src, ok := s.by[registry]; ok {
		src.Invalidate(registry)
	}
}

// run polls the files and mints the tokens until ctx ends.
func (s *sources) run(ctx context.Context) {
	var wg sync.WaitGroup
	if s.files != nil {
		wg.Go(func() { _ = s.files.Run(ctx) })
	}
	if s.minter != nil {
		wg.Go(func() { _ = s.minter.Run(ctx) })
	}
	wg.Wait()
}

// close unregisters the expiry gauge.
func (s *sources) close() {
	s.once.Do(func() {
		if s.stop != nil {
			_ = s.stop()
		}
	})
}

// buildSources makes the credential sources the registries need, nil when
// none has a login. The file source reads every registry's files once
// now: a file that exists and cannot be read refuses the start, a missing
// one is a 503 on its route until it appears. A CodeArtifact registry
// whose AWS configuration cannot be loaded is a warning and a 503, never a
// refused start: the credential-free routes keep serving.
func buildSources(ctx context.Context, s settings, log *slog.Logger) (*sources, error) {
	var files []credfile.Registry
	var minted []awsca.Registry
	for _, r := range s.engine.Registries.Registries() {
		switch r.Auth.Type {
		case netprofile.AuthBearer, netprofile.AuthBasic:
			files = append(files, credfile.Registry{Name: r.Name, Type: r.Auth.Type})
		case netprofile.AuthCodeArtifact:
			m, err := awsca.FromNetprofile(r)
			if err != nil {
				return nil, err
			}
			minted = append(minted, m)
		}
	}
	if len(files) == 0 && len(minted) == 0 {
		return nil, nil
	}
	src := &sources{by: map[string]egressproxy.CredentialSource{}}
	if len(files) > 0 {
		f, err := credfile.New(s.credentialsDir, files, credfile.Options{
			Interval: s.pollInterval,
			Log:      log,
			OnRefresh: func(registry, result string) {
				egressproxy.CountCredentialRefresh(ctx, registry, egressproxy.CredentialStatic, result)
			},
		})
		if err != nil {
			return nil, fmt.Errorf("registry credentials: %w", err)
		}
		src.files = f
		for _, r := range files {
			src.by[r.Name] = f
		}
	}
	if len(minted) > 0 {
		if err := src.addMinter(ctx, s, minted, log); err != nil {
			log.LogAttrs(ctx, slog.LevelWarn, "credential_mint_failed", slog.String("credential", "codeartifact"),
				slog.String("error_code", awsca.ErrorCode(err)),
				slog.String("reason", "aws configuration: the codeartifact routes answer 503"))
		}
	}
	return src, nil
}

// addMinter builds the CodeArtifact minter on the two guarded SDK clients
// and registers its expiry gauge.
func (s *sources) addMinter(ctx context.Context, set settings, regs []awsca.Registry, log *slog.Logger) error {
	clients := awsca.NewClients(set.engine.Policy)
	m, err := awsca.New(regs, func(region string) (awsca.API, error) {
		cfg, err := clients.LoadConfig(ctx, region)
		if err != nil {
			return nil, err
		}
		return awsca.NewAPI(cfg), nil
	}, awsca.Options{
		Log: log,
		OnRefresh: func(registry, result string) {
			egressproxy.CountCredentialRefresh(ctx, registry, egressproxy.CredentialCodeArtifact, result)
		},
		AddressClass: clients.LastAddressClass,
	})
	if err != nil {
		return err
	}
	names := make([]string, 0, len(regs))
	for _, r := range regs {
		names = append(names, r.Name)
		s.by[r.Name] = m
	}
	s.minter = m
	stop, err := egressproxy.RegisterCredentialExpiry(names, m.ExpiresAt, time.Now)
	if err != nil {
		log.LogAttrs(ctx, slog.LevelWarn, "credential expiry gauge unavailable", slog.Any("error", err))
		return nil
	}
	s.stop = stop
	return nil
}

// logRegistries is the startup line per registry: name, ecosystem, host
// and credential kind, never a value or a Secret name.
func logRegistries(ctx context.Context, log *slog.Logger, set netprofile.RegistrySet) {
	for _, r := range set.Registries() {
		log.LogAttrs(ctx, slog.LevelInfo, "registry configured",
			slog.String("registry", r.Name),
			slog.String("ecosystem", r.Ecosystem),
			slog.String("host", r.Hostname()),
			slog.String("credential", string(credentialKind(r))),
			slog.Bool("internal", r.Internal != nil),
			slog.Bool("ca", r.CA))
	}
}

// credentialKind is the kind of credential a registry's auth type gives.
func credentialKind(r netprofile.Registry) egressproxy.CredentialKind {
	switch r.Auth.Type {
	case netprofile.AuthBearer, netprofile.AuthBasic:
		return egressproxy.CredentialStatic
	case netprofile.AuthCodeArtifact:
		return egressproxy.CredentialCodeArtifact
	}
	return egressproxy.CredentialNone
}

// errNoRegistry is a probe of a registry no profile grants.
var errNoRegistry = errors.New("no such registry")

// grantingProfile is the first profile (by name) that grants the registry.
func grantingProfile(set netprofile.Set, registry string) (netprofile.Profile, bool) {
	for _, p := range set.Profiles() {
		if _, ok := p.Grant(registry); ok {
			return p, true
		}
	}
	return netprofile.Profile{}, false
}
