// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package awsca

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/endpointcreds"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	"github.com/aws/aws-sdk-go-v2/service/codeartifact"

	"github.com/devthenet-labs/maul/internal/egressproxy"
)

// PodIdentityAddr is the EKS Pod Identity agent's node-local endpoint: the
// one address the container-credentials client may dial.
var PodIdentityAddr = netip.MustParseAddrPort("169.254.170.23:80")

// clientTimeout bounds one SDK HTTP exchange.
const clientTimeout = 30 * time.Second

// podIdentityPolicy admits exactly PodIdentityAddr.
type podIdentityPolicy struct{}

// Check implements egressproxy.DialPolicy.
func (podIdentityPolicy) Check(ap netip.AddrPort) error {
	if ap != PodIdentityAddr {
		return fmt.Errorf("%w: %s is not the Pod Identity agent %s", egressproxy.ErrDeniedAddress, ap,
			PodIdentityAddr)
	}
	return nil
}

// Address classes for credential_mint_failed.
const (
	ClassNone      = "none"
	ClassPublic    = "public"
	ClassPrivate   = "private"
	ClassLinkLocal = "link-local"
	ClassLoopback  = "loopback"
	ClassOther     = "other"
)

// AddressClass names the kind of address a: the hint that tells an
// operator a VPC endpoint with private DNS answered (private) rather than
// the public endpoint.
func AddressClass(a netip.Addr) string {
	a = a.Unmap()
	switch {
	case a.IsLoopback():
		return ClassLoopback
	case a.IsLinkLocalUnicast():
		return ClassLinkLocal
	case a.IsPrivate(), netip.MustParsePrefix("100.64.0.0/10").Contains(a):
		return ClassPrivate
	case a.IsGlobalUnicast():
		return ClassPublic
	}
	return ClassOther
}

// recordingPolicy is a DialPolicy that remembers the class of the last
// address it judged, then defers to the policy it wraps.
type recordingPolicy struct {
	inner egressproxy.DialPolicy
	last  atomic.Value // string
}

// Check implements egressproxy.DialPolicy.
func (p *recordingPolicy) Check(ap netip.AddrPort) error {
	p.last.Store(AddressClass(ap.Addr()))
	return p.inner.Check(ap)
}

// Clients are the two HTTP clients the AWS SDK is given, each on the
// proxy's guarded dialer with its own policy.
type Clients struct {
	// API reaches CodeArtifact and STS: public IPv4 on port 443 only (the
	// forwarding routes' AddrPolicy), never the Pod Identity agent.
	API *http.Client
	// PodIdentity reaches the container-credentials endpoint: exactly
	// PodIdentityAddr.
	PodIdentity *http.Client

	api *recordingPolicy
}

// NewClients builds the two clients; public is the proxy's AddrPolicy (its
// --deny-cidrs included).
func NewClients(public egressproxy.AddrPolicy) *Clients {
	rec := &recordingPolicy{inner: public}
	return &Clients{
		API:         egressproxy.NewGuardedClient(rec, clientTimeout),
		PodIdentity: egressproxy.NewGuardedClient(podIdentityPolicy{}, clientTimeout),
		api:         rec,
	}
}

// LastAddressClass is the class of the address the API client last
// judged, ClassNone before any.
func (c *Clients) LastAddressClass() string {
	if v, ok := c.api.last.Load().(string); ok {
		return v
	}
	return ClassNone
}

// LoadConfig is the SDK configuration for region with the default
// credential chain (IRSA's web identity through STS on the API client,
// Pod Identity's container credentials on the PodIdentity client) and the
// EC2 instance metadata client switched off: the proxy's identity is its
// own role, never the node's.
func (c *Clients) LoadConfig(ctx context.Context, region string) (aws.Config, error) {
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(region),
		config.WithHTTPClient(c.API),
		config.WithEndpointCredentialOptions(func(o *endpointcreds.Options) { o.HTTPClient = c.PodIdentity }),
		config.WithEC2IMDSClientEnableState(imds.ClientDisabled),
	)
	if err != nil {
		return aws.Config{}, fmt.Errorf("aws config for %s: %w", region, err)
	}
	return cfg, nil
}

// NewAPI is the CodeArtifact client over cfg.
func NewAPI(cfg aws.Config, optFns ...func(*codeartifact.Options)) API {
	return codeartifact.NewFromConfig(cfg, optFns...)
}
